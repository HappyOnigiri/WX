package archive

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/HappyOnigiri/WX/internal/discovery"
	"github.com/HappyOnigiri/WX/internal/domain"
	"github.com/HappyOnigiri/WX/internal/gitx"
	"github.com/HappyOnigiri/WX/internal/state"
	"github.com/HappyOnigiri/WX/internal/workspace"
)

type Manager struct {
	Git       *gitx.Runner
	Preparer  *workspace.Preparer
	Ownership state.OwnershipValidator
}

// SnapshotWithPersistence は、正確な ref 名と object ID の永続化後に recovery ref を公開する。
// 永続化失敗時は ref を公開せず、公開失敗時は永続行を残すので、reconcile は未完了 archive と無関係な ref を区別できる。
func (m *Manager) SnapshotWithPersistence(ctx context.Context, repo discovery.Repository, worktree, sessionID string, expiry time.Time, persist func(state.Snapshot) error) (state.Snapshot, error) {
	var snapshot state.Snapshot
	if err := m.Git.WithCommonDirLock(string(repo.CommonDir), func() error {
		var err error
		snapshot, err = m.snapshotObjects(ctx, repo, worktree, sessionID, expiry)
		return err
	}); err != nil {
		return state.Snapshot{}, err
	}
	if persist != nil {
		if err := persist(snapshot); err != nil {
			return state.Snapshot{}, err
		}
	}
	if err := m.Git.WithCommonDirLock(string(repo.CommonDir), func() error {
		return m.publishSnapshotRefs(ctx, repo, snapshot)
	}); err != nil {
		return state.Snapshot{}, err
	}
	return snapshot, nil
}

func (m *Manager) snapshotObjects(ctx context.Context, repo discovery.Repository, worktree, sessionID string, expiry time.Time) (state.Snapshot, error) {
	if m.Preparer == nil {
		return state.Snapshot{}, errors.New("snapshot requires a workspace preparer")
	}
	worktreeIdentity, err := m.Preparer.WorktreeIdentity(worktree)
	if err != nil {
		return state.Snapshot{}, fmt.Errorf("%w: capture worktree identity before snapshot: %w", state.ErrOwnership, err)
	}
	worktreeValue := func(env []string, args ...string) (string, error) {
		result, runErr := m.Preparer.RunGitInWorktree(ctx, worktree, worktreeIdentity, env, nil, args...)
		if runErr != nil {
			return "", runErr
		}
		return strings.TrimSpace(result.Stdout), nil
	}
	worktreeRun := func(env []string, input []byte, args ...string) (gitx.Result, error) {
		return m.Preparer.RunGitInWorktree(ctx, worktree, worktreeIdentity, env, input, args...)
	}
	head, err := worktreeValue(nil, "rev-parse", "HEAD")
	if err != nil {
		return state.Snapshot{}, err
	}
	headRef := fmt.Sprintf("refs/wx/recovery/%s/%s/head", sessionID, repo.ID)
	worktreeRef := fmt.Sprintf("refs/wx/recovery/%s/%s/worktree", sessionID, repo.ID)
	indexRef := fmt.Sprintf("refs/wx/recovery/%s/%s/index", sessionID, repo.ID)
	id := domain.StableID("snapshot", sessionID, string(repo.ID))
	created := time.Now().UTC()
	// clean worktree は HEAD の tree と commit で完全に表せるため、新しい object を作らず base OID と ref メタデータだけを記録する。
	// dirty 経路より多くを clean と判定すると未 snapshot の作業を失うため、次の flag は必須である。
	// ユーザー設定の status.showUntrackedFiles と submodule.<name>.ignore/diff.ignoreSubmodules は、一時 index の `add -A` が記録する内容を隠し得る。
	// commentlint:allow-long -- 未 snapshot の作業を失わないための判定条件を説明する
	statusOutput, err := worktreeValue(nil, "status", "--porcelain=v1", "--untracked-files=all", "--ignore-submodules=none")
	if err != nil {
		return state.Snapshot{}, fmt.Errorf("check worktree cleanliness: %w", err)
	}
	clean := strings.TrimSpace(statusOutput) == ""
	if clean {
		flagged, flagErr := indexHidesWorktreeChanges(worktreeValue)
		if flagErr != nil {
			return state.Snapshot{}, flagErr
		}
		clean = !flagged
	}
	if clean {
		headTree, err := worktreeValue(nil, "rev-parse", "HEAD^{tree}")
		if err != nil {
			return state.Snapshot{}, fmt.Errorf("resolve clean HEAD tree: %w", err)
		}
		return state.Snapshot{ID: id, SessionID: sessionID, RepositoryID: string(repo.ID), HeadOID: head, HeadRef: headRef, IndexTreeOID: headTree, IndexRef: indexRef, WorktreeOID: head, WorktreeRef: worktreeRef, Status: "ARCHIVED", CreatedAt: state.FormatTime(created), ExpiresAt: state.FormatTime(expiry)}, nil
	}
	indexTree, err := worktreeValue(nil, "write-tree")
	if err != nil {
		return state.Snapshot{}, fmt.Errorf("write index tree: %w", err)
	}
	tmpFile, err := os.CreateTemp("", ".wx-index-*")
	if err != nil {
		return state.Snapshot{}, fmt.Errorf("create temporary snapshot index: %w", err)
	}
	tmp := tmpFile.Name()
	if err := tmpFile.Close(); err != nil {
		_ = os.Remove(tmp)
		return state.Snapshot{}, fmt.Errorf("close temporary snapshot index: %w", err)
	}
	defer func() { _ = os.Remove(tmp) }()
	env := []string{"GIT_INDEX_FILE=" + tmp}
	if _, err := worktreeRun(env, nil, "read-tree", head); err != nil {
		return state.Snapshot{}, err
	}
	if _, err := worktreeRun(env, nil, "add", "-A", "--", "."); err != nil {
		return state.Snapshot{}, err
	}
	worktreeTree, err := worktreeValue(env, "write-tree")
	if err != nil {
		return state.Snapshot{}, err
	}
	commitEnv := append([]string(nil), env...)
	commitEnv = append(commitEnv,
		"GIT_AUTHOR_NAME=wx", "GIT_AUTHOR_EMAIL=wx@localhost",
		"GIT_COMMITTER_NAME=wx", "GIT_COMMITTER_EMAIL=wx@localhost",
		"GIT_AUTHOR_DATE=2000-01-01T00:00:00Z", "GIT_COMMITTER_DATE=2000-01-01T00:00:00Z",
	)
	commitRes, err := worktreeRun(commitEnv, []byte("wx recovery snapshot\n"), "commit-tree", worktreeTree, "-p", head)
	if err != nil {
		return state.Snapshot{}, err
	}
	worktreeCommit := strings.TrimSpace(commitRes.Stdout)
	return state.Snapshot{ID: id, SessionID: sessionID, RepositoryID: string(repo.ID), HeadOID: head, HeadRef: headRef, IndexTreeOID: indexTree, IndexRef: indexRef, WorktreeOID: worktreeCommit, WorktreeRef: worktreeRef, Status: "ARCHIVED", CreatedAt: state.FormatTime(created), ExpiresAt: state.FormatTime(expiry)}, nil
}

// indexHidesWorktreeChanges は、git status が隠し得る assume-unchanged と skip-worktree の index 項目を調べる。
// dirty snapshot はそれらを持たない一時 index を HEAD から再構築するため、`add -A` は現在の内容を記録する。
// これらがあれば clean の短絡経路を使わず、追加の `git ls-files -v` は status が clean の場合だけ実行する。
func indexHidesWorktreeChanges(worktreeValue func(env []string, args ...string) (string, error)) (bool, error) {
	listing, err := worktreeValue(nil, "ls-files", "-v")
	if err != nil {
		return false, fmt.Errorf("inspect index stat flags: %w", err)
	}
	for _, line := range strings.Split(listing, "\n") {
		if line == "" {
			continue
		}
		if tag := line[0]; tag == 'S' || (tag >= 'a' && tag <= 'z') {
			return true, nil
		}
	}
	return false, nil
}

// recoveryRefTargets は snapshot が公開する ref と object の対応を返し、index tree ref も含める。
// ref がなければ index tree は dangling object となり、保持期限前でも `git gc` に回収されて Resume が staged/unstaged 内容を復元できなくなる。
func recoveryRefTargets(snapshot state.Snapshot) map[string]string {
	targets := map[string]string{snapshot.HeadRef: snapshot.HeadOID, snapshot.WorktreeRef: snapshot.WorktreeOID}
	if snapshot.IndexRef != "" {
		targets[snapshot.IndexRef] = snapshot.IndexTreeOID
	}
	return targets
}

// publishSnapshotRefs は snapshot の recovery ref を publish する。
// 各 ref の一致は ensureRecoveryRef が既存値の照合か update-ref の成否で保証するため、publish 後の再検証は行わない。
func (m *Manager) publishSnapshotRefs(ctx context.Context, repo discovery.Repository, snapshot state.Snapshot) error {
	targets := recoveryRefTargets(snapshot)
	for ref, want := range targets {
		if err := m.ensureRecoveryRef(ctx, repo, ref, want); err != nil {
			return err
		}
	}
	return nil
}

func (m *Manager) ensureRecoveryRef(ctx context.Context, repo discovery.Repository, ref, oid string) error {
	existing, err := m.gitValue(ctx, string(repo.MainPath), nil, "show-ref", "--verify", "--hash", ref)
	if err == nil {
		if existing != oid {
			return fmt.Errorf("recovery ref %s already points to unexpected object", ref)
		}
		return nil
	}
	if _, err := m.Git.Run(ctx, string(repo.MainPath), "update-ref", "--create-reflog", ref, oid); err != nil {
		return err
	}
	return nil
}

func (m *Manager) DeleteSnapshotRefs(ctx context.Context, repo discovery.Repository, snapshot state.Snapshot) error {
	return m.Git.WithCommonDirLock(string(repo.CommonDir), func() error {
		for ref, want := range recoveryRefTargets(snapshot) {
			if _, err := m.Git.Run(ctx, string(repo.MainPath), "check-ref-format", ref); err != nil {
				return fmt.Errorf("invalid recovery ref %q: %w", ref, err)
			}
			got, err := m.gitValue(ctx, string(repo.MainPath), nil, "show-ref", "--verify", "--hash", ref)
			if err != nil {
				var gitErr *gitx.Error
				if errors.As(err, &gitErr) && (gitErr.Result.ExitCode == 1 || gitErr.Result.ExitCode == 128) {
					continue // A prior interrupted GC may already have removed this ref.
				}
				return err
			}
			if got != want {
				return fmt.Errorf("refuse to delete recovery ref %s with unexpected OID", ref)
			}
			if _, err := m.Git.Run(ctx, string(repo.MainPath), "update-ref", "-d", ref, want); err != nil {
				return err
			}
		}
		return nil
	})
}

func (m *Manager) gitValue(ctx context.Context, dir string, env []string, args ...string) (string, error) {
	res, err := m.Git.RunEnv(ctx, dir, env, args...)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(res.Stdout), nil
}
