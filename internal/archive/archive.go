package archive

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/HappyOnigiri/WX/internal/config"
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

// SnapshotWithPersistence publishes recovery refs only after persist has
// durably recorded the exact ref names and object IDs. This ordering closes
// the reconcile window in which a normal archive looked like an unknown ref.
// If persistence fails, no recovery ref has been made visible. If ref
// publication fails afterwards, the durable row remains and reconciliation
// can distinguish the incomplete archive from an unrelated ref.
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

func (m *Manager) Snapshot(ctx context.Context, repo discovery.Repository, worktree, sessionID string, expiry time.Time) (state.Snapshot, error) {
	var snapshot state.Snapshot
	err := m.Git.WithCommonDirLock(string(repo.CommonDir), func() error {
		var err error
		snapshot, err = m.snapshotObjects(ctx, repo, worktree, sessionID, expiry)
		if err != nil {
			return err
		}
		return m.publishSnapshotRefs(ctx, repo, snapshot)
	})
	if err != nil {
		return state.Snapshot{}, err
	}
	return snapshot, nil
}

func (m *Manager) snapshotObjects(ctx context.Context, repo discovery.Repository, worktree, sessionID string, expiry time.Time) (state.Snapshot, error) {
	worktreeDescriptorBound := false
	if m.Preparer != nil {
		if root, rootErr := config.ExpandHome(m.Preparer.Config.Storage.WorktreeRoot); rootErr == nil {
			worktreeDescriptorBound = domain.IsWithin(root, worktree)
		}
	}
	worktreeIdentity := ""
	if worktreeDescriptorBound {
		var identityErr error
		worktreeIdentity, identityErr = m.Preparer.WorktreeIdentity(worktree)
		if identityErr != nil {
			return state.Snapshot{}, fmt.Errorf("%w: capture worktree identity before snapshot: %w", state.ErrOwnership, identityErr)
		}
	}
	worktreeValue := func(env []string, args ...string) (string, error) {
		if !worktreeDescriptorBound {
			return m.gitValue(ctx, worktree, env, args...)
		}
		result, runErr := m.Preparer.RunGitInWorktree(ctx, worktree, worktreeIdentity, env, nil, args...)
		if runErr != nil {
			return "", runErr
		}
		return strings.TrimSpace(result.Stdout), nil
	}
	worktreeRun := func(env []string, input []byte, args ...string) (gitx.Result, error) {
		if !worktreeDescriptorBound {
			return m.Git.RunEnvInput(ctx, worktree, env, input, args...)
		}
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
	// A clean worktree (nothing staged, unstaged, or untracked relative to
	// HEAD) needs no new content objects: HEAD's own tree and commit already
	// represent it exactly. Skip write-tree/read-tree/add/commit-tree
	// entirely and record only the base OID and ref metadata, per the
	// clean-session archive minimization the design calls for.
	statusOutput, err := worktreeValue(nil, "status", "--porcelain=v1")
	if err != nil {
		return state.Snapshot{}, fmt.Errorf("check worktree cleanliness: %w", err)
	}
	if strings.TrimSpace(statusOutput) == "" {
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

// recoveryRefTargets maps every ref this snapshot publishes to the object it
// must point at, including the index tree ref. The index tree is otherwise a
// dangling object: without a ref keeping it reachable, `git gc`'s normal
// prune grace period can collect it before `retention.recovery_snapshot`
// elapses, leaving Resume unable to restore staged/unstaged content even
// though the head and worktree refs are still intact.
func recoveryRefTargets(snapshot state.Snapshot) map[string]string {
	targets := map[string]string{snapshot.HeadRef: snapshot.HeadOID, snapshot.WorktreeRef: snapshot.WorktreeOID}
	if snapshot.IndexRef != "" {
		targets[snapshot.IndexRef] = snapshot.IndexTreeOID
	}
	return targets
}

func (m *Manager) publishSnapshotRefs(ctx context.Context, repo discovery.Repository, snapshot state.Snapshot) error {
	targets := recoveryRefTargets(snapshot)
	for ref, want := range targets {
		if err := m.ensureRecoveryRef(ctx, repo, ref, want); err != nil {
			return err
		}
	}
	for ref, want := range targets {
		got, err := m.gitValue(ctx, string(repo.MainPath), nil, "rev-parse", "--verify", ref)
		if err != nil || got != want {
			return fmt.Errorf("verify recovery ref %s", ref)
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

func (m *Manager) Restore(ctx context.Context, repo discovery.Repository, target, slotID string, s state.Snapshot) error {
	if expiry, err := time.Parse(time.RFC3339Nano, s.ExpiresAt); err != nil || !expiry.After(time.Now()) {
		return errors.New("recovery snapshot has expired")
	}
	for ref, want := range recoveryRefTargets(s) {
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
	targetIdentity, err := m.Preparer.WorktreeIdentity(target)
	if err != nil {
		return fmt.Errorf("%w: capture restored worktree identity: %w", state.ErrOwnership, err)
	}
	return m.Git.WithCommonDirLock(string(repo.CommonDir), func() error {
		targetValue := func(env []string, args ...string) (string, error) {
			result, runErr := m.Preparer.RunGitInWorktree(ctx, target, targetIdentity, env, nil, args...)
			if runErr != nil {
				return "", runErr
			}
			return strings.TrimSpace(result.Stdout), nil
		}
		targetRun := func(env []string, input []byte, args ...string) (gitx.Result, error) {
			return m.Preparer.RunGitInWorktree(ctx, target, targetIdentity, env, input, args...)
		}
		for ref, want := range recoveryRefTargets(s) {
			got, err := m.gitValue(ctx, string(repo.MainPath), nil, "rev-parse", "--verify", ref)
			if err != nil || got != want {
				return fmt.Errorf("recovery ref %s changed during restore", ref)
			}
		}
		if err := m.Preparer.ValidateRestoringOwnership(ctx, repo, target, s.HeadOID, slotID); err != nil {
			return fmt.Errorf("validate restore worktree before snapshot: %w", err)
		}
		if _, err := targetRun(nil, nil, "read-tree", "--reset", "-u", s.WorktreeOID+"^{tree}"); err != nil {
			return err
		}
		if _, err := targetRun(nil, nil, "read-tree", s.IndexTreeOID); err != nil {
			return err
		}
		if err := m.Preparer.PrepareResumeWithIdentity(ctx, repo, target, s.HeadOID, slotID, targetIdentity); err != nil {
			return fmt.Errorf("resume prepare: %w", err)
		}
		head, err := targetValue(nil, "rev-parse", "HEAD")
		if err != nil {
			return err
		}
		if head != s.HeadOID {
			return errors.New("restored HEAD does not match snapshot")
		}
		if _, detachedErr := targetRun(nil, nil, "symbolic-ref", "-q", "HEAD"); detachedErr == nil {
			return errors.New("restored worktree is not detached")
		} else if errors.Is(detachedErr, state.ErrOwnership) {
			return detachedErr
		}
		indexTree, err := targetValue(nil, "write-tree")
		if err != nil || indexTree != s.IndexTreeOID {
			return errors.New("restored index does not match snapshot")
		}
		tmpFile, err := os.CreateTemp("", ".wx-verify-index-*")
		if err != nil {
			return fmt.Errorf("create temporary restore index: %w", err)
		}
		tmp := tmpFile.Name()
		if err := tmpFile.Close(); err != nil {
			_ = os.Remove(tmp)
			return fmt.Errorf("close temporary restore index: %w", err)
		}
		defer func() { _ = os.Remove(tmp) }()
		env := []string{"GIT_INDEX_FILE=" + tmp}
		if _, err := targetRun(env, nil, "read-tree", s.HeadOID); err != nil {
			return err
		}
		if _, err := targetRun(env, nil, "add", "-A", "--", "."); err != nil {
			return err
		}
		actualWorktreeTree, err := targetValue(env, "write-tree")
		if err != nil {
			return err
		}
		expectedWorktreeTree, err := m.gitValue(ctx, string(repo.MainPath), nil, "rev-parse", s.WorktreeOID+"^{tree}")
		if err != nil || actualWorktreeTree != expectedWorktreeTree {
			return errors.New("restored working tree does not match snapshot")
		}
		if _, err := targetRun(nil, nil, "status", "--porcelain=v2", "--untracked-files=all"); err != nil {
			return fmt.Errorf("validate restored status: %w", err)
		}
		if err := m.Preparer.VerifyWorktreeIdentity(target, targetIdentity); err != nil {
			return fmt.Errorf("validate restored worktree identity: %w", err)
		}
		if err := m.Preparer.ValidateRestoringOwnership(ctx, repo, target, s.HeadOID, slotID); err != nil {
			return fmt.Errorf("validate restored worktree ownership: %w", err)
		}
		if err := m.Preparer.FinishRestoreWithIdentity(ctx, repo, target, s.HeadOID, slotID, targetIdentity); err != nil {
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
			return removalOwnershipFailure(errors.New("worktree path is outside wx root"))
		}
		if m.Preparer != nil && m.Preparer.RequireOwnedRoot {
			if m.Preparer.OwnedRoot == nil || filepath.Clean(m.Preparer.RootPath) != absoluteRoot {
				return removalOwnershipFailure(errors.New("descriptor-bound worktree removal is unavailable"))
			}
		}
		relative, err := filepath.Rel(absoluteRoot, absolutePath)
		if err != nil {
			return removalOwnershipFailure(err)
		}
		if err := validateRemovalPathComponents(absoluteRoot, relative); err != nil {
			return removalOwnershipFailure(err)
		}
		lockReason, registered, err := workspace.RegisteredWorktreeLockReason(ctx, m.Git, string(repo.MainPath), absolutePath)
		if err != nil {
			return err
		}
		_, statErr := os.Lstat(absolutePath)
		if errors.Is(statErr, os.ErrNotExist) {
			if !registered {
				return nil // A prior attempt completed physical and Git metadata removal.
			}
			common, err := filepath.EvalSymlinks(string(repo.CommonDir))
			if err != nil {
				return removalOwnershipFailure(err)
			}
			slotID, err := workspace.ValidateRemovalOwnership(absoluteRoot, absolutePath, common)
			if err != nil {
				return fmt.Errorf("validate missing worktree ownership: %w", err)
			}
			if err := validateWxLockReason(lockReason, slotID); err != nil {
				return removalOwnershipFailure(err)
			}
			if err := m.validateStateOwnership(ctx, repo, absolutePath, slotID, []string{"REMOVING", "RETIRING"}, []string{"READY", "RETIRING"}); err != nil {
				return fmt.Errorf("validate missing worktree SQLite ownership: %w", err)
			}
			if err := validateRemovalPathComponents(absoluteRoot, relative); err != nil {
				return removalOwnershipFailure(err)
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
				return removalOwnershipFailure(errors.New("worktree lock changed before removal"))
			}
			if _, err := workspace.ValidateRemovalOwnership(absoluteRoot, absolutePath, common); err != nil {
				return fmt.Errorf("revalidate missing worktree ownership: %w", err)
			}
			if err := m.validateStateOwnership(ctx, repo, absolutePath, slotID, []string{"REMOVING", "RETIRING"}, []string{"READY", "RETIRING"}); err != nil {
				return fmt.Errorf("revalidate missing worktree SQLite ownership: %w", err)
			}
			if err := validateRemovalPathComponents(absoluteRoot, relative); err != nil {
				return removalOwnershipFailure(err)
			}
			_, removeErr := m.Git.Run(ctx, string(repo.MainPath), "worktree", "remove", "--force", absolutePath)
			return removeErr
		}
		if statErr != nil {
			return removalOwnershipFailure(statErr)
		}
		return m.removeExistingWorktree(ctx, repo, absoluteRoot, absolutePath, relative, expectedHead, lockReason, registered)
	})
}

func (m *Manager) removeExistingWorktree(ctx context.Context, repo discovery.Repository, absoluteRoot, absolutePath, relative, expectedHead, lockReason string, registered bool) error {
	targetIdentity := ""
	if m.Preparer != nil && m.Preparer.OwnedRoot != nil && filepath.Clean(m.Preparer.RootPath) == absoluteRoot {
		directory, identity, identityErr := domain.OpenDirectoryAt(m.Preparer.OwnedRoot, relative)
		if identityErr != nil {
			return fmt.Errorf("%w: capture worktree identity before removal: %w", state.ErrOwnership, identityErr)
		}
		targetIdentity = identity
		if closeErr := directory.Close(); closeErr != nil {
			return fmt.Errorf("%w: close worktree identity descriptor: %w", state.ErrOwnership, closeErr)
		}
	}
	common, err := filepath.EvalSymlinks(string(repo.CommonDir))
	if err != nil {
		return removalOwnershipFailure(err)
	}
	slotID, err := workspace.ValidateRemovalOwnership(absoluteRoot, absolutePath, common)
	if err != nil {
		return fmt.Errorf("validate worktree ownership: %w", err)
	}
	if err := validateWxLockReason(lockReason, slotID); err != nil {
		return removalOwnershipFailure(err)
	}
	if err := m.validateStateOwnership(ctx, repo, absolutePath, slotID, []string{"REMOVING", "RETIRING"}, []string{"READY", "RETIRING"}); err != nil {
		return fmt.Errorf("validate worktree SQLite ownership: %w", err)
	}
	canonicalRoot, err := filepath.EvalSymlinks(absoluteRoot)
	if err != nil {
		return removalOwnershipFailure(err)
	}
	canonicalPath, err := filepath.EvalSymlinks(absolutePath)
	if err != nil {
		return removalOwnershipFailure(err)
	}
	if !domain.IsWithin(canonicalRoot, canonicalPath) {
		return removalOwnershipFailure(errors.New("worktree path is outside canonical wx root"))
	}
	commonOutput, err := m.gitValue(ctx, absolutePath, nil, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return err
	}
	actual, err := filepath.EvalSymlinks(commonOutput)
	if err != nil {
		return removalOwnershipFailure(err)
	}
	if actual != common {
		return removalOwnershipFailure(errors.New("worktree common directory does not match repository ownership"))
	}
	if expectedHead != "" {
		head, err := m.gitValue(ctx, absolutePath, nil, "rev-parse", "HEAD")
		if err != nil || head != expectedHead {
			return removalOwnershipFailure(errors.New("worktree HEAD does not match SQLite ownership metadata"))
		}
	}
	if !registered {
		return removalOwnershipFailure(errors.New("worktree is not registered at expected path"))
	}
	if err := validateRemovalPathComponents(absoluteRoot, relative); err != nil {
		return removalOwnershipFailure(err)
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
		return removalOwnershipFailure(errors.New("worktree lock changed before removal"))
	}
	if _, err := workspace.ValidateRemovalOwnership(absoluteRoot, absolutePath, common); err != nil {
		return fmt.Errorf("revalidate worktree ownership: %w", err)
	}
	if err := m.validateStateOwnership(ctx, repo, absolutePath, slotID, []string{"REMOVING", "RETIRING"}, []string{"READY", "RETIRING"}); err != nil {
		return fmt.Errorf("revalidate worktree SQLite ownership: %w", err)
	}
	if err := validateRemovalPathComponents(absoluteRoot, relative); err != nil {
		return removalOwnershipFailure(err)
	}
	commonOutput, err = m.gitValue(ctx, absolutePath, nil, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return err
	}
	actual, err = filepath.EvalSymlinks(commonOutput)
	if err != nil || actual != common {
		return removalOwnershipFailure(errors.New("worktree common directory changed before removal"))
	}
	if expectedHead != "" {
		head, err := m.gitValue(ctx, absolutePath, nil, "rev-parse", "HEAD")
		if err != nil || head != expectedHead {
			return removalOwnershipFailure(errors.New("worktree HEAD changed before removal"))
		}
	}
	// This is the last durable ownership check before the destructive Git
	// operation. The common-directory lock remains held throughout the
	// physical/Git checks and this state read, so a forged marker/lock can
	// never authorize deletion without the matching SQLite rows.
	if err := m.validateStateOwnership(ctx, repo, absolutePath, slotID, []string{"REMOVING", "RETIRING"}, []string{"READY", "RETIRING"}); err != nil {
		return fmt.Errorf("worktree SQLite ownership changed before removal: %w", err)
	}
	if targetIdentity != "" {
		return m.Preparer.RemoveWorktreeAt(ctx, repo, absoluteRoot, absolutePath, targetIdentity)
	}
	_, err = m.Git.Run(ctx, string(repo.MainPath), "worktree", "remove", "--force", absolutePath)
	return err
}

func removalOwnershipFailure(err error) error {
	if err == nil || errors.Is(err, state.ErrOwnership) {
		return err
	}
	return fmt.Errorf("%w: %w", state.ErrOwnership, err)
}

func (m *Manager) validateStateOwnership(ctx context.Context, repo discovery.Repository, target, slotID string, slotStates, repositoryStates []string) error {
	validator := m.Ownership
	if validator == nil && m.Preparer != nil {
		validator = m.Preparer.Ownership
	}
	if validator == nil {
		return fmt.Errorf("%w: state-backed worktree ownership validator is required", state.ErrOwnership)
	}
	_, err := validator.ValidateWorktreeOwnership(ctx, state.WorktreeOwnershipRequest{
		SlotID:                  slotID,
		RepositoryID:            string(repo.ID),
		WorktreePath:            target,
		CommonDir:               string(repo.CommonDir),
		AllowedSlotStates:       slotStates,
		AllowedRepositoryStates: repositoryStates,
	})
	return err
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
