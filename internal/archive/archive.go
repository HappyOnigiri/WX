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
		defer func() { _ = os.Remove(tmp) }()
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
		commitEnv := append([]string(nil), env...)
		commitEnv = append(commitEnv,
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
		snapshot = state.Snapshot{ID: id, SessionID: sessionID, RepositoryID: string(repo.ID), HeadOID: head, HeadRef: headRef, IndexTreeOID: indexTree, WorktreeOID: worktreeCommit, WorktreeRef: worktreeRef, Status: "ARCHIVED", CreatedAt: state.FormatTime(created), ExpiresAt: state.FormatTime(expiry)}
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
	// Create and lock the clean base first. Resume-phase preparation is deferred
	// until the snapshot tree and saved index have been restored below.
	if m.Preparer == nil {
		return errors.New("restore requires a workspace preparer")
	}
	if err := m.Preparer.PrepareForRestore(ctx, repo, target, s.HeadOID, slotID); err != nil {
		return err
	}
	return m.Git.WithCommonDirLock(string(repo.CommonDir), func() error {
		for ref, want := range map[string]string{s.HeadRef: s.HeadOID, s.WorktreeRef: s.WorktreeOID} {
			got, err := m.gitValue(ctx, string(repo.MainPath), nil, "rev-parse", "--verify", ref)
			if err != nil || got != want {
				return fmt.Errorf("recovery ref %s changed during restore", ref)
			}
		}
		if err := m.Preparer.ValidateRestoringOwnership(ctx, repo, target, s.HeadOID, slotID); err != nil {
			return fmt.Errorf("validate restore worktree before snapshot: %w", err)
		}
		if _, err := m.Git.Run(ctx, target, "read-tree", "--reset", "-u", s.WorktreeOID+"^{tree}"); err != nil {
			return err
		}
		if _, err := m.Git.Run(ctx, target, "read-tree", s.IndexTreeOID); err != nil {
			return err
		}
		if err := m.Preparer.PrepareResume(ctx, repo, target, s.HeadOID, slotID); err != nil {
			return fmt.Errorf("resume prepare: %w", err)
		}
		head, err := m.gitValue(ctx, target, nil, "rev-parse", "HEAD")
		if err != nil {
			return err
		}
		if head != s.HeadOID {
			return errors.New("restored HEAD does not match snapshot")
		}
		if !gitx.IsDetached(ctx, m.Git, target) {
			return errors.New("restored worktree is not detached")
		}
		indexTree, err := m.gitValue(ctx, target, nil, "write-tree")
		if err != nil || indexTree != s.IndexTreeOID {
			return errors.New("restored index does not match snapshot")
		}
		tmp := filepath.Join(filepath.Dir(target), ".wx-verify-index-"+slotID)
		_ = os.Remove(tmp)
		defer func() { _ = os.Remove(tmp) }()
		env := []string{"GIT_INDEX_FILE=" + tmp}
		if _, err := m.Git.RunEnv(ctx, target, env, "read-tree", s.HeadOID); err != nil {
			return err
		}
		if _, err := m.Git.RunEnv(ctx, target, env, "add", "-A", "--", "."); err != nil {
			return err
		}
		actualWorktreeTree, err := m.gitValue(ctx, target, env, "write-tree")
		if err != nil {
			return err
		}
		expectedWorktreeTree, err := m.gitValue(ctx, string(repo.MainPath), nil, "rev-parse", s.WorktreeOID+"^{tree}")
		if err != nil || actualWorktreeTree != expectedWorktreeTree {
			return errors.New("restored working tree does not match snapshot")
		}
		if _, err := m.Git.Run(ctx, target, "status", "--porcelain=v2", "--untracked-files=all"); err != nil {
			return fmt.Errorf("validate restored status: %w", err)
		}
		if err := m.Preparer.ValidateOwnership(ctx, repo, target, s.HeadOID); err != nil {
			return fmt.Errorf("validate restored worktree ownership: %w", err)
		}
		if err := m.Preparer.FinishRestore(ctx, repo, target, s.HeadOID, slotID); err != nil {
			return err
		}
		return nil
	})
}

func (m *Manager) RemoveWorktree(ctx context.Context, repo discovery.Repository, root, path, expectedHead string) error {
	return m.Git.WithCommonDirLock(string(repo.CommonDir), func() error {
		// Check the path exactly as recorded in SQLite before resolving anything.
		// Resolving first would hide a symlink that redirects deletion to another
		// registered worktree.
		absoluteRoot, err := filepath.Abs(root)
		if err != nil {
			return err
		}
		absolutePath, err := filepath.Abs(path)
		if err != nil {
			return err
		}
		absoluteRoot, absolutePath = filepath.Clean(absoluteRoot), filepath.Clean(absolutePath)
		if !domain.IsWithin(absoluteRoot, absolutePath) {
			return errors.New("worktree path is outside wx root")
		}
		relative, err := filepath.Rel(absoluteRoot, absolutePath)
		if err != nil {
			return err
		}
		if err := validateRemovalPathComponents(absoluteRoot, relative); err != nil {
			return err
		}
		lockReason, registered, err := workspace.RegisteredWorktreeLockReason(ctx, m.Git, string(repo.MainPath), absolutePath)
		if err != nil {
			return err
		}
		if _, statErr := os.Lstat(absolutePath); errors.Is(statErr, os.ErrNotExist) {
			if !registered {
				return nil // A prior attempt completed physical and Git metadata removal.
			}
			common, err := filepath.EvalSymlinks(string(repo.CommonDir))
			if err != nil {
				return err
			}
			slotID, err := workspace.ValidateRemovalOwnership(absoluteRoot, absolutePath, common)
			if err != nil {
				return fmt.Errorf("validate missing worktree ownership: %w", err)
			}
			if err := validateWxLockReason(lockReason, slotID); err != nil {
				return err
			}
			if err := validateRemovalPathComponents(absoluteRoot, relative); err != nil {
				return err
			}
			if _, err := m.Git.Run(ctx, string(repo.MainPath), "worktree", "unlock", absolutePath); err != nil {
				return err
			}
			postReason, postLocked, found, err := workspace.RegisteredWorktreeLockStatus(ctx, m.Git, string(repo.MainPath), absolutePath)
			if err != nil {
				return err
			}
			if !found {
				return nil
			}
			if postLocked || postReason != "" {
				return errors.New("worktree lock changed before removal")
			}
			if _, err := workspace.ValidateRemovalOwnership(absoluteRoot, absolutePath, common); err != nil {
				return fmt.Errorf("revalidate missing worktree ownership: %w", err)
			}
			if err := validateRemovalPathComponents(absoluteRoot, relative); err != nil {
				return err
			}
			_, removeErr := m.Git.Run(ctx, string(repo.MainPath), "worktree", "remove", "--force", absolutePath)
			return removeErr
		} else if statErr != nil {
			return statErr
		}
		common, err := filepath.EvalSymlinks(string(repo.CommonDir))
		if err != nil {
			return err
		}
		slotID, err := workspace.ValidateRemovalOwnership(absoluteRoot, absolutePath, common)
		if err != nil {
			return fmt.Errorf("validate worktree ownership: %w", err)
		}
		if err := validateWxLockReason(lockReason, slotID); err != nil {
			return err
		}
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
		commonOutput, err := m.gitValue(ctx, path, nil, "rev-parse", "--path-format=absolute", "--git-common-dir")
		if err != nil {
			return err
		}
		actual, err := filepath.EvalSymlinks(commonOutput)
		if err != nil {
			return err
		}
		if actual != common {
			return errors.New("worktree common directory does not match repository ownership")
		}
		if expectedHead != "" {
			head, err := m.gitValue(ctx, path, nil, "rev-parse", "HEAD")
			if err != nil || head != expectedHead {
				return errors.New("worktree HEAD does not match SQLite ownership metadata")
			}
		}
		if !registered {
			return errors.New("worktree is not registered at expected path")
		}
		if err := validateRemovalPathComponents(absoluteRoot, relative); err != nil {
			return err
		}
		if _, err := m.Git.Run(ctx, string(repo.MainPath), "worktree", "unlock", path); err != nil {
			return err
		}
		postReason, postLocked, found, err := workspace.RegisteredWorktreeLockStatus(ctx, m.Git, string(repo.MainPath), absolutePath)
		if err != nil {
			return err
		}
		if !found {
			return nil
		}
		if postLocked || postReason != "" {
			return errors.New("worktree lock changed before removal")
		}
		if _, err := workspace.ValidateRemovalOwnership(absoluteRoot, absolutePath, common); err != nil {
			return fmt.Errorf("revalidate worktree ownership: %w", err)
		}
		if err := validateRemovalPathComponents(absoluteRoot, relative); err != nil {
			return err
		}
		commonOutput, err = m.gitValue(ctx, path, nil, "rev-parse", "--path-format=absolute", "--git-common-dir")
		if err != nil {
			return err
		}
		actual, err = filepath.EvalSymlinks(commonOutput)
		if err != nil || actual != common {
			return errors.New("worktree common directory changed before removal")
		}
		if expectedHead != "" {
			head, err := m.gitValue(ctx, path, nil, "rev-parse", "HEAD")
			if err != nil || head != expectedHead {
				return errors.New("worktree HEAD changed before removal")
			}
		}
		_, err = m.Git.Run(ctx, string(repo.MainPath), "worktree", "remove", "--force", path)
		return err
	})
}

func validateWxLockReason(reason, slotID string) error {
	if reason != "wx:"+slotID+":READY" && reason != "wx:"+slotID+":PREPARING" && reason != "wx:"+slotID+":RESTORING" {
		return fmt.Errorf("worktree lock reason does not belong to wx slot %s", slotID)
	}
	return nil
}

func validateRemovalPathComponents(root, relative string) error {
	current := root
	components := strings.Split(relative, string(filepath.Separator))
	for index, component := range components {
		if component == "." {
			continue
		}
		current = filepath.Join(current, component)
		info, err := os.Lstat(current)
		if errors.Is(err, os.ErrNotExist) && index == len(components)-1 {
			return nil
		}
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("symlink component in removal path %s", current)
		}
	}
	return nil
}

func (m *Manager) DeleteSnapshotRefs(ctx context.Context, repo discovery.Repository, snapshot state.Snapshot) error {
	return m.Git.WithCommonDirLock(string(repo.CommonDir), func() error {
		for ref, want := range map[string]string{snapshot.HeadRef: snapshot.HeadOID, snapshot.WorktreeRef: snapshot.WorktreeOID} {
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
