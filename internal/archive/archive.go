package archive

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/HappyOnigiri/WorktreeX/internal/discovery"
	"github.com/HappyOnigiri/WorktreeX/internal/domain"
	"github.com/HappyOnigiri/WorktreeX/internal/gitx"
	"github.com/HappyOnigiri/WorktreeX/internal/state"
	"github.com/HappyOnigiri/WorktreeX/internal/workspace"
)

type Manager struct {
	Git       *gitx.Runner
	Preparer  *workspace.Preparer
	Ownership state.OwnershipValidator
}

// lockSlot は Preparer が示す slot への書き込みを、prepare を含む他の経路と直列化する。
// 最上位の operation で一度だけ取得し、内側の Preparer へは返った ctx を渡して再取得させない。
// recovery ref だけを扱う Preparer 無しの manager は slot を持たないため排他しない。
func (m *Manager) lockSlot(ctx context.Context) (context.Context, func(), error) {
	if m.Preparer == nil {
		return ctx, func() {}, nil
	}
	return m.Preparer.LockSlot(ctx)
}

// SnapshotWithPersistence は、正確な ref 名と object ID の永続化後に recovery ref を公開する。submodule の capsule も同じ順序に従う。
// 永続化失敗時は ref を公開せず、公開失敗時は永続行を残すので、reconcile は未完了 archive と無関係な ref を区別できる。
// 第 2 戻り値は snapshot に入らなかった submodule の作業で、呼び出し元はこれを持つ slot を自動回収から外す。
func (m *Manager) SnapshotWithPersistence(ctx context.Context, repo discovery.Repository, worktree, sessionID string, expiry time.Time, persist func(state.Snapshot, []SubmoduleCapsule) error) (state.Snapshot, []UnsavedSubmodule, error) {
	ctx, releaseSlot, err := m.lockSlot(ctx)
	if err != nil {
		return state.Snapshot{}, nil, err
	}
	defer releaseSlot()
	var snapshot state.Snapshot
	var unsaved []UnsavedSubmodule
	var capsules []SubmoduleCapsule
	if err := m.Git.WithCommonDirLock(ctx, string(repo.CommonDir), func(ctx context.Context) error {
		var err error
		snapshot, unsaved, capsules, err = m.snapshotObjects(ctx, repo, worktree, sessionID, expiry)
		return err
	}); err != nil {
		return state.Snapshot{}, nil, err
	}
	if persist != nil {
		if err := persist(snapshot, capsules); err != nil {
			return state.Snapshot{}, nil, err
		}
	}
	if err := m.Git.WithCommonDirLock(ctx, string(repo.CommonDir), func(ctx context.Context) error {
		if err := m.publishSnapshotRefs(ctx, repo, snapshot); err != nil {
			return err
		}
		return m.publishSubmoduleCapsules(ctx, repo, capsules)
	}); err != nil {
		return state.Snapshot{}, nil, err
	}
	return snapshot, unsaved, nil
}

// addWorktreeArgs は、一時 index へ worktree の現状を取り込む add の引数を返す。
// --sparse が無いと sparse 範囲外に実体があるだけで add が逸脱として失敗し、そこでの作業を保存できない。
// skip-worktree 付きの path は --sparse でも HEAD の内容のまま残るため、flag 付き path を外す方針と競合しない。
func addWorktreeArgs() []string {
	return []string{"add", "-A", "--sparse"}
}

func (m *Manager) snapshotObjects(ctx context.Context, repo discovery.Repository, worktree, sessionID string, expiry time.Time) (state.Snapshot, []UnsavedSubmodule, []SubmoduleCapsule, error) {
	if m.Preparer == nil {
		return state.Snapshot{}, nil, nil, errors.New("snapshot requires a workspace preparer")
	}
	worktreeIdentity, err := m.Preparer.WorktreeIdentity(worktree)
	if err != nil {
		return state.Snapshot{}, nil, nil, fmt.Errorf("%w: capture worktree identity before snapshot: %w", state.ErrOwnership, err)
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
		return state.Snapshot{}, nil, nil, err
	}
	headRef := fmt.Sprintf("refs/wx/recovery/%s/%s/head", sessionID, repo.ID)
	worktreeRef := fmt.Sprintf("refs/wx/recovery/%s/%s/worktree", sessionID, repo.ID)
	indexRef := fmt.Sprintf("refs/wx/recovery/%s/%s/index", sessionID, repo.ID)
	gitStateRef := fmt.Sprintf("refs/wx/recovery/%s/%s/gitstate", sessionID, repo.ID)
	conflictRef := fmt.Sprintf("refs/wx/recovery/%s/%s/conflict", sessionID, repo.ID)
	id := domain.StableID("snapshot", sessionID, string(repo.ID))
	created := time.Now().UTC()
	// 停止中の操作の制御ファイルは worktree 専用 gitdir にあり tree にも index にも現れないため、clean 判定より前に別途採取する。
	// `rebase -i` の edit 停止は working tree が clean なので、下の短絡経路にも同じ値を載せる必要がある。
	gitState, err := captureGitState(worktreeValue, worktreeRun, head)
	if err != nil {
		return state.Snapshot{}, nil, nil, fmt.Errorf("capture in-progress operation state: %w", err)
	}
	if gitState == "" {
		gitStateRef = ""
	}
	// clean worktree は HEAD の tree と commit で完全に表せるため、新しい object を作らず base OID と ref メタデータだけを記録する。
	// dirty 経路より多くを clean と判定すると未 snapshot の作業を失うため、次の flag は必須である。
	// ユーザー設定の status.showUntrackedFiles と submodule.<name>.ignore/diff.ignoreSubmodules は、一時 index の `add -A` が記録する内容を隠し得る。
	// skip-worktree/assume-unchanged が付いた path は snapshot の対象外（HEAD の内容として扱う）なので、ここでは clean 判定に影響しない。
	// 両 flag とも index と HEAD の差は隠さないため、status が clean なら flag 付き path に staged 内容が隠れていることもない。
	// porcelain=v2 は submodule の状態を行ごとの 3 列目に載せる。clean 判定と未保全 submodule の検出を同じ 1 回の出力から行い、
	// 2 回起動して観測時点がずれた結果、判定と保存の内容が食い違うことを防ぐ。
	// core.quotePath=false は非 ASCII の path をそのまま読むためで、制御文字を含む path は Git が従来どおり quote する。
	// commentlint:allow-long -- 未 snapshot の作業を失わないための判定条件と、status を 1 回だけ起動する理由を説明する
	statusOutput, err := worktreeValue(nil, "-c", "core.quotePath=false", "status", "--porcelain=v2", "--untracked-files=all", "--ignore-submodules=none")
	if err != nil {
		return state.Snapshot{}, nil, nil, fmt.Errorf("check worktree cleanliness: %w", err)
	}
	// 親が gitlink を commit した後は status が clean になるため、検出と子の保存は clean・dirty の両分岐で走らせる。
	reasons, modules := m.submoduleWork(ctx, repo, worktree, worktreeIdentity, statusOutput)
	capsules := m.captureSubmodules(repo, worktreeValue, worktreeRun, sessionID, modules, reasons)
	unsaved := sortedUnsavedSubmodules(reasons)
	if strings.TrimSpace(statusOutput) == "" {
		headTree, err := worktreeValue(nil, "rev-parse", "HEAD^{tree}")
		if err != nil {
			return state.Snapshot{}, nil, nil, fmt.Errorf("resolve clean HEAD tree: %w", err)
		}
		return state.Snapshot{ID: id, SessionID: sessionID, RepositoryID: string(repo.ID), HeadOID: head, HeadRef: headRef, IndexTreeOID: headTree, IndexRef: indexRef, WorktreeOID: head, WorktreeRef: worktreeRef, GitStateOID: gitState, GitStateRef: gitStateRef, ConflictRef: "", ConflictOID: "", Status: "ARCHIVED", CreatedAt: state.FormatTime(created), ExpiresAt: state.FormatTime(expiry)}, unsaved, capsules, nil
	}
	indexTree, conflict, err := captureConflictState(worktreeValue, worktreeRun, head)
	if err != nil {
		return state.Snapshot{}, nil, nil, err
	}
	if conflict == "" {
		indexTree, err = worktreeValue(nil, "write-tree")
		if err != nil {
			return state.Snapshot{}, nil, nil, fmt.Errorf("write index tree: %w", err)
		}
		conflictRef = ""
	}
	flags, err := readIndexFlags(worktreeValue, nil)
	if err != nil {
		return state.Snapshot{}, nil, nil, err
	}
	tmp, cleanup, err := temporaryIndex("snapshot", ".wx-index-*")
	if err != nil {
		return state.Snapshot{}, nil, nil, err
	}
	defer cleanup()
	env := []string{"GIT_INDEX_FILE=" + tmp}
	if _, err := worktreeRun(env, nil, "read-tree", head); err != nil {
		return state.Snapshot{}, nil, nil, err
	}
	// HEAD に無い index entry を先に持ち込む。ignore 規則に一致する force-added path は、
	// これが無いと一時 index から未追跡の ignored file に見え、add -A が作業内容ごと飛ばす。
	if err := seedForceAddedEntries(worktreeRun, worktreeValue, env); err != nil {
		return state.Snapshot{}, nil, nil, err
	}
	// 一時 index にも元 index と同じ flag を立ててから add するので、flag 付き path は HEAD の内容のまま記録される。
	// add に pathspec を渡さないのは、pathspec が flag 付き path だけに一致すると git が sparse-checkout の逸脱として exit 1 にするためである。
	if err := applyIndexFlags(worktreeRun, worktreeValue, env, flags); err != nil {
		return state.Snapshot{}, nil, nil, err
	}
	if _, err := worktreeRun(env, nil, addWorktreeArgs()...); err != nil {
		return state.Snapshot{}, nil, nil, err
	}
	worktreeTree, err := worktreeValue(env, "write-tree")
	if err != nil {
		return state.Snapshot{}, nil, nil, err
	}
	if candidates, candidateErr := snapshotLFSObjects(worktreeRun, env, head, worktreeTree); candidateErr != nil {
		m.logLFSOptimizationWarning("collect changed LFS objects", candidateErr)
	} else if len(candidates) > 0 {
		if _, compactErr := m.Preparer.CompactLFSObjects(ctx, repo, worktree, candidates); compactErr != nil {
			m.logLFSOptimizationWarning("compact changed LFS objects", compactErr)
		}
	}
	commitRes, err := worktreeRun(recoveryCommitEnv(env), []byte("wx recovery snapshot\n"), "commit-tree", worktreeTree, "-p", head)
	if err != nil {
		return state.Snapshot{}, nil, nil, err
	}
	worktreeCommit := strings.TrimSpace(commitRes.Stdout)
	return state.Snapshot{ID: id, SessionID: sessionID, RepositoryID: string(repo.ID), HeadOID: head, HeadRef: headRef, IndexTreeOID: indexTree, IndexRef: indexRef, WorktreeOID: worktreeCommit, WorktreeRef: worktreeRef, GitStateOID: gitState, GitStateRef: gitStateRef, ConflictOID: conflict, ConflictRef: conflictRef, Status: "ARCHIVED", CreatedAt: state.FormatTime(created), ExpiresAt: state.FormatTime(expiry)}, unsaved, capsules, nil
}

// recoveryRefTargets は snapshot が公開する ref と object の対応を返し、index tree ref も含める。
// ref がなければ index tree は dangling object となり、保持期限前でも `git gc` に回収されて Resume が staged/unstaged 内容を復元できなくなる。
func recoveryRefTargets(snapshot state.Snapshot) map[string]string {
	targets := map[string]string{snapshot.HeadRef: snapshot.HeadOID, snapshot.WorktreeRef: snapshot.WorktreeOID}
	if snapshot.IndexRef != "" {
		targets[snapshot.IndexRef] = snapshot.IndexTreeOID
	}
	// 停止中の操作を保持する commit は復元後の HEAD から到達できないため、ref が無ければ GC が制御ファイルごと回収する。
	if snapshot.GitStateRef != "" {
		targets[snapshot.GitStateRef] = snapshot.GitStateOID
	}
	if snapshot.ConflictRef != "" {
		targets[snapshot.ConflictRef] = snapshot.ConflictOID
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

// DeleteSnapshotRefs は snapshot が公開した ref を消す。submodules の capsule ref は source の
// ローカル module にあるため、同じ「期待 OID と一致するときだけ消す」規則で一緒に扱う。
func (m *Manager) DeleteSnapshotRefs(ctx context.Context, repo discovery.Repository, snapshot state.Snapshot, submodules []state.SubmoduleSnapshot) error {
	return m.Git.WithCommonDirLock(ctx, string(repo.CommonDir), func(ctx context.Context) error {
		if err := m.deleteSubmoduleCapsuleRefs(ctx, repo, submodules); err != nil {
			return err
		}
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

// temporaryIndex は GIT_INDEX_FILE に渡す空の一時 index を作り、path と後始末を返す。
// 作成直後の 0 byte file を git は空 index として読むため、呼び出し側は用途に応じて read-tree するか、そのまま積み上げる。
func temporaryIndex(label, pattern string) (string, func(), error) {
	file, err := os.CreateTemp("", pattern)
	if err != nil {
		return "", nil, fmt.Errorf("create temporary %s index: %w", label, err)
	}
	path := file.Name()
	if err := file.Close(); err != nil {
		_ = os.Remove(path)
		return "", nil, fmt.Errorf("close temporary %s index: %w", label, err)
	}
	return path, func() { _ = os.Remove(path) }, nil
}

// recoveryCommitEnv は recovery commit の author/committer を固定する。
// SNAPSHOT job の再実行が同じ commit OID を出さないと、SaveSnapshot の ON CONFLICT が不一致として失敗する。
func recoveryCommitEnv(env []string) []string {
	return append(append([]string(nil), env...),
		"GIT_AUTHOR_NAME=wx", "GIT_AUTHOR_EMAIL=wx@localhost",
		"GIT_COMMITTER_NAME=wx", "GIT_COMMITTER_EMAIL=wx@localhost",
		"GIT_AUTHOR_DATE=2000-01-01T00:00:00Z", "GIT_COMMITTER_DATE=2000-01-01T00:00:00Z",
	)
}

func (m *Manager) gitValue(ctx context.Context, dir string, env []string, args ...string) (string, error) {
	res, err := m.Git.RunEnv(ctx, dir, env, args...)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(res.Stdout), nil
}
