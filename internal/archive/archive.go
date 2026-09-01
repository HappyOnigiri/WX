package archive

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/HappyOnigiri/WX/internal/discovery"
	"github.com/HappyOnigiri/WX/internal/domain"
	"github.com/HappyOnigiri/WX/internal/gitx"
	"github.com/HappyOnigiri/WX/internal/state"
	"github.com/HappyOnigiri/WX/internal/workspace"
)

type Manager struct {
	Git      *gitx.Runner
	Preparer *workspace.Preparer
}

func (m *Manager) Snapshot(ctx context.Context, repo discovery.Repository, worktree, sessionID string, expiry time.Time) (state.Snapshot, error) {
	var snapshot state.Snapshot
	err := m.Git.WithCommonDirLock(string(repo.CommonDir), func() error {
		head, err := m.gitValue(ctx, worktree, nil, "rev-parse", "HEAD")
		if err != nil {
			return err
		}
		indexTree, err := m.gitValue(ctx, worktree, nil, "write-tree")
		if err != nil {
			return fmt.Errorf("write index tree: %w", err)
		}
		tmp := filepath.Join(filepath.Dir(worktree), ".wx-index-"+sessionID)
		_ = os.Remove(tmp)
		defer os.Remove(tmp)
		env := []string{"GIT_INDEX_FILE=" + tmp}
		if _, err := m.Git.RunEnv(ctx, worktree, env, "read-tree", head); err != nil {
			return err
		}
		if _, err := m.Git.RunEnv(ctx, worktree, env, "add", "-A", "--", "."); err != nil {
			return err
		}
		worktreeTree, err := m.gitValue(ctx, worktree, env, "write-tree")
		if err != nil {
			return err
		}
		commitEnv := append(env,
			"GIT_AUTHOR_NAME=wx", "GIT_AUTHOR_EMAIL=wx@localhost",
			"GIT_COMMITTER_NAME=wx", "GIT_COMMITTER_EMAIL=wx@localhost",
			"GIT_AUTHOR_DATE=2000-01-01T00:00:00Z", "GIT_COMMITTER_DATE=2000-01-01T00:00:00Z",
		)
		commitRes, err := m.Git.RunEnvInput(ctx, worktree, commitEnv, []byte("wx recovery snapshot\n"), "commit-tree", worktreeTree, "-p", head)
		if err != nil {
			return err
		}
		worktreeCommit := strings.TrimSpace(commitRes.Stdout)
		headRef := fmt.Sprintf("refs/wx/recovery/%s/%s/head", sessionID, repo.ID)
		worktreeRef := fmt.Sprintf("refs/wx/recovery/%s/%s/worktree", sessionID, repo.ID)
		if err := m.ensureRecoveryRef(ctx, repo, headRef, head); err != nil {
			return err
		}
		if err := m.ensureRecoveryRef(ctx, repo, worktreeRef, worktreeCommit); err != nil {
			return err
		}
		for ref, want := range map[string]string{headRef: head, worktreeRef: worktreeCommit} {
			got, err := m.gitValue(ctx, string(repo.MainPath), nil, "rev-parse", "--verify", ref)
			if err != nil || got != want {
				return fmt.Errorf("verify recovery ref %s", ref)
			}
		}
		id := domain.StableID("snapshot", sessionID, string(repo.ID))
		created := time.Now().UTC()
		snapshot = state.Snapshot{ID: id, SessionID: sessionID, RepositoryID: string(repo.ID), HeadOID: head, HeadRef: headRef, IndexTreeOID: indexTree, WorktreeOID: worktreeCommit, WorktreeRef: worktreeRef, Status: "ARCHIVED", CreatedAt: created.Format(time.RFC3339Nano), ExpiresAt: expiry.UTC().Format(time.RFC3339Nano)}
		return nil
	})
	return snapshot, err
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

func (m *Manager) Restore(ctx context.Context, repo discovery.Repository, target, slotID string, s state.Snapshot) error {
	if expiry, err := time.Parse(time.RFC3339Nano, s.ExpiresAt); err != nil || !expiry.After(time.Now()) {
		return errors.New("recovery snapshot has expired")
	}
	for ref, want := range map[string]string{s.HeadRef: s.HeadOID, s.WorktreeRef: s.WorktreeOID} {
		got, err := m.gitValue(ctx, string(repo.MainPath), nil, "rev-parse", "--verify", ref)
		if err != nil || got != want {
			return fmt.Errorf("recovery ref %s does not match snapshot metadata", ref)
		}
	}
	// Preparer obtains the repository lock while creating and locking the worktree.
	if err := m.Preparer.Prepare(ctx, repo, target, s.HeadOID, slotID); err != nil {
		return err
	}
	return m.Git.WithCommonDirLock(string(repo.CommonDir), func() error {
		if _, err := m.Git.Run(ctx, target, "read-tree", "--reset", "-u", s.WorktreeOID+"^{tree}"); err != nil {
			return err
		}
		if _, err := m.Git.Run(ctx, target, "read-tree", s.IndexTreeOID); err != nil {
			return err
		}
		head, err := m.gitValue(ctx, target, nil, "rev-parse", "HEAD")
		if err != nil {
			return err
		}
		if head != s.HeadOID {
			return errors.New("restored HEAD does not match snapshot")
		}
		return nil
	})
}

func (m *Manager) RemoveWorktree(ctx context.Context, repo discovery.Repository, root, path, expectedHead string) error {
	return m.Git.WithCommonDirLock(string(repo.CommonDir), func() error {
		canonicalRoot, err := filepath.EvalSymlinks(root)
		if err != nil {
			return err
		}
		canonicalPath, err := filepath.EvalSymlinks(path)
		if err != nil {
			return err
		}
		if !domain.IsWithin(canonicalRoot, canonicalPath) {
			return errors.New("worktree path is outside canonical wx root")
		}
		for current := canonicalPath; current != canonicalRoot; current = filepath.Dir(current) {
			info, err := os.Lstat(current)
			if err != nil {
				return err
			}
			if info.Mode()&os.ModeSymlink != 0 {
				return fmt.Errorf("symlink component in removal path %s", current)
			}
		}
		common, err := m.gitValue(ctx, path, nil, "rev-parse", "--path-format=absolute", "--git-common-dir")
		if err != nil {
			return err
		}
		expected, err := filepath.EvalSymlinks(string(repo.CommonDir))
		if err != nil {
			return err
		}
		actual, err := filepath.EvalSymlinks(common)
		if err != nil {
			return err
		}
		if actual != expected {
			return errors.New("worktree common directory does not match repository ownership")
		}
		if expectedHead != "" {
			head, err := m.gitValue(ctx, path, nil, "rev-parse", "HEAD")
			if err != nil || head != expectedHead {
				return errors.New("worktree HEAD does not match SQLite ownership metadata")
			}
		}
		listed, err := m.Git.Run(ctx, string(repo.MainPath), "worktree", "list", "--porcelain", "-z")
		if err != nil {
			return err
		}
		registered := false
		for _, field := range strings.Split(listed.Stdout, "\x00") {
			if strings.TrimPrefix(field, "worktree ") == canonicalPath && strings.HasPrefix(field, "worktree ") {
				registered = true
				break
			}
		}
		if !registered {
			return errors.New("worktree is not registered at expected path")
		}
		if _, err := m.Git.Run(ctx, string(repo.MainPath), "worktree", "unlock", path); err != nil {
			return err
		}
		_, err = m.Git.Run(ctx, string(repo.MainPath), "worktree", "remove", "--force", path)
		return err
	})
}

func (m *Manager) DeleteSnapshotRefs(ctx context.Context, repo discovery.Repository, snapshot state.Snapshot) error {
	return m.Git.WithCommonDirLock(string(repo.CommonDir), func() error {
		for ref, want := range map[string]string{snapshot.HeadRef: snapshot.HeadOID, snapshot.WorktreeRef: snapshot.WorktreeOID} {
			got, err := m.gitValue(ctx, string(repo.MainPath), nil, "show-ref", "--verify", "--hash", ref)
			if err != nil {
				var gitErr *gitx.Error
				if errors.As(err, &gitErr) && gitErr.Result.ExitCode == 1 {
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
