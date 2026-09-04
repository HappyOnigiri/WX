package workspace

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/HappyOnigiri/WX/internal/config"
	"github.com/HappyOnigiri/WX/internal/discovery"
	"github.com/HappyOnigiri/WX/internal/domain"
	"github.com/HappyOnigiri/WX/internal/fdexec"
	"github.com/HappyOnigiri/WX/internal/gitx"
	"github.com/HappyOnigiri/WX/internal/state"
)

type Preparer struct {
	Git       *gitx.Runner
	Config    config.Config
	Ownership state.OwnershipValidator
	SlotPath  string
	// OwnedRoot is held by the daemon for its configured worktree root. When it
	// is set, all target creation and descriptor-bound Git operations use this
	// inode namespace rather than reopening a mutable pathname.
	OwnedRoot *os.Root
	RootPath  string
	// RootID and SlotRelPath are how SQLite records this slot's location:
	// the durable root generation and the slot's path relative to it. They
	// are what ownership validation compares, in place of the absolute
	// SlotPath, so a renamed or reconfigured root cannot make two different
	// directories look like the same slot.
	RootID      string
	SlotRelPath string
}

// markerIdentity builds the slot-scoped marker identity for one repository.
func (p *Preparer) markerIdentity(repo discovery.Repository, slotID string) MarkerIdentity {
	return MarkerIdentity{SlotID: slotID, RootID: p.RootID, RepositoryID: string(repo.ID)}
}

// WorktreeDirName is the exported form of worktreeDirName, used by
// internal/archive to describe a removal target the same way prepare does.
func (p *Preparer) WorktreeDirName(target string) (string, error) {
	return p.worktreeDirName(target)
}

// worktreeDirName returns the repository's directory name inside the slot,
// which is the single path component between SlotPath and target. It is read
// back from the caller's target rather than recomputed from configuration:
// the name recorded when the slot was created is the authority, and a later
// configuration change must not be able to redirect an existing slot.
func (p *Preparer) worktreeDirName(target string) (string, error) {
	if p.SlotPath == "" {
		return "", fmt.Errorf("%w: slot path is unavailable", state.ErrOwnership)
	}
	relative, err := filepath.Rel(filepath.Clean(p.SlotPath), filepath.Clean(target))
	if err != nil {
		return "", fmt.Errorf("%w: worktree is not inside its slot: %w", state.ErrOwnership, err)
	}
	if relative == "." || !filepath.IsLocal(relative) || strings.ContainsRune(relative, filepath.Separator) {
		return "", fmt.Errorf("%w: worktree %s is not a direct child of slot %s", state.ErrOwnership, target, p.SlotPath)
	}
	return relative, nil
}

func (p *Preparer) Prepare(ctx context.Context, repo discovery.Repository, target, oid, slotID string) error {
	return p.prepare(ctx, repo, target, oid, slotID, preparePhaseCreate)
}

// PrepareForRestore creates the clean base of a restore while leaving the
// worktree locked as RESTORING. Snapshot contents and the saved index must be
// installed before any resume-phase prepare command runs.
func (p *Preparer) PrepareForRestore(ctx context.Context, repo discovery.Repository, target, oid, slotID string) error {
	return p.prepare(ctx, repo, target, oid, slotID, preparePhaseRestore)
}

type preparePhase string

const (
	preparePhaseCreate  preparePhase = "create"
	preparePhaseRestore preparePhase = "restore"
)

func (p *Preparer) prepare(ctx context.Context, repo discovery.Repository, target, oid, slotID string, phase preparePhase) error {
	root, target, err := p.prepareTarget(target)
	if err != nil {
		return err
	}
	return p.Git.WithCommonDirLock(string(repo.CommonDir), func() error {
		return p.prepareLocked(ctx, repo, target, oid, slotID, phase, root)
	})
}

func (p *Preparer) prepareTarget(target string) (string, string, error) {
	root, err := config.ExpandHome(p.Config.Storage.WorktreeRoot)
	if err != nil {
		return "", "", err
	}
	target = filepath.Clean(target)
	if !domain.IsWithin(root, target) {
		return "", "", fmt.Errorf("target %s is outside wx worktree root", target)
	}
	if err := p.requirePinnedRoot(root); err != nil {
		return "", "", err
	}
	ownedRoot, relativeTarget, closeOwnedRoot, err := p.openOwnedRoot(root, target)
	if err != nil {
		return "", "", fmt.Errorf("open wx worktree root: %w", err)
	}
	defer closeOwnedRoot()
	if err := ownedRoot.MkdirAll(filepath.Dir(relativeTarget), 0o700); err != nil {
		return "", "", fmt.Errorf("create worktree parent safely: %w", err)
	}
	return root, target, nil
}

func (p *Preparer) prepareLocked(ctx context.Context, repo discovery.Repository, target, oid, slotID string, phase preparePhase, root string) error {
	locked, err := p.prepareLockedTarget(ctx, repo, target, oid, slotID, phase, root)
	if err != nil {
		return err
	}
	defer locked.close()
	lockedRoot := locked.root
	lockedRelativeTarget := locked.relative
	existingWorktree := locked.existing
	targetIdentity := locked.identity

	cleanup := !existingWorktree
	ownedAfterLock := false
	defer func() {
		if cleanup && ownedAfterLock {
			// A failed preparation is removable only while ownership can still
			// be proved. If a command or concurrent filesystem change invalidated
			// that proof, leave the locked target and marker for quarantine/reconcile.
			if err := p.validatePreparedTarget(context.Background(), repo, target, oid, slotID, phase, lockedRoot, lockedRelativeTarget, targetIdentity, "validate worktree before cleanup"); err != nil {
				return
			}
			if _, err := p.runWorktreeAdminOwned(context.Background(), repo, lockedRoot, lockedRelativeTarget, target, targetIdentity, "unlock"); err != nil {
				return
			}
			if _, err := p.runWorktreeAdminOwned(context.Background(), repo, lockedRoot, lockedRelativeTarget, target, targetIdentity, "remove", "--force"); err != nil {
				return
			}
			_ = removeOwnershipMarkerAt(lockedRoot, root, target, string(repo.ID))
		}
	}()
	if existingWorktree {
		if _, err := p.runWorktreeAdminOwned(ctx, repo, lockedRoot, lockedRelativeTarget, target, targetIdentity, "unlock"); err != nil {
			return fmt.Errorf("unlock existing wx worktree: %w", err)
		}
	}
	lockState := "PREPARING"
	if phase == preparePhaseRestore {
		lockState = "RESTORING"
	}
	if _, err := p.runWorktreeAdminOwned(ctx, repo, lockedRoot, lockedRelativeTarget, target, targetIdentity, "lock", "--reason", "wx:"+slotID+":"+lockState); err != nil {
		return err
	}
	// This validation is deliberately after the new lock is acquired. It
	// proves that the marker, physical path, Git registration, OID, and
	// lock reason still describe the same slot before any file operation.
	if err := p.validatePreparedTarget(ctx, repo, target, oid, slotID, phase, lockedRoot, lockedRelativeTarget, targetIdentity, "wx worktree ownership changed after lock"); err != nil {
		return fmt.Errorf("wx worktree ownership changed after lock: %w", err)
	}
	ownedAfterLock = true
	// Revalidate the durable owner immediately before each operation that
	// writes into or reuses the worktree. A common-directory lock protects
	// Git metadata, while this read-only state proof protects the slot/path
	// association and state machine independently.
	if err := p.validatePreparedTarget(ctx, repo, target, oid, slotID, phase, lockedRoot, lockedRelativeTarget, targetIdentity, "wx worktree ownership changed before includes"); err != nil {
		return fmt.Errorf("wx worktree ownership changed before includes: %w", err)
	}
	if err := p.copyIncludesAt(repo, lockedRoot, lockedRelativeTarget); err != nil {
		return err
	}
	if err := p.validatePreparedTarget(ctx, repo, target, oid, slotID, phase, lockedRoot, lockedRelativeTarget, targetIdentity, "wx worktree ownership changed before links"); err != nil {
		return fmt.Errorf("wx worktree ownership changed before links: %w", err)
	}
	if err := p.createLinksAt(ctx, repo, lockedRoot, lockedRelativeTarget); err != nil {
		return err
	}
	if phase == preparePhaseCreate {
		if err := p.validatePreparedTarget(ctx, repo, target, oid, slotID, phase, lockedRoot, lockedRelativeTarget, targetIdentity, "wx worktree ownership changed before prepare command"); err != nil {
			return fmt.Errorf("wx worktree ownership changed before prepare command: %w", err)
		}
		if err := p.runPrepareWithIdentity(ctx, repo, target, ""); err != nil {
			return err
		}
		if err := p.verifyPreparedTargetIdentity(lockedRoot, lockedRelativeTarget, targetIdentity); err != nil {
			return fmt.Errorf("wx worktree ownership changed during prepare command: %w", err)
		}
	}
	if phase == preparePhaseCreate {
		if err := p.verifyPreparedTargetIdentity(lockedRoot, lockedRelativeTarget, targetIdentity); err != nil {
			return fmt.Errorf("wx worktree ownership changed before tracked status: %w", err)
		}
		if err := p.validateTrackedClean(ctx, target); err != nil {
			return err
		}
		if err := p.verifyPreparedTargetIdentity(lockedRoot, lockedRelativeTarget, targetIdentity); err != nil {
			return fmt.Errorf("wx worktree ownership changed during tracked status: %w", err)
		}
	}
	targetRoot, currentIdentity, err := domain.OpenDirectoryAt(lockedRoot, lockedRelativeTarget)
	if err != nil {
		return fmt.Errorf("open prepared worktree: %w", err)
	}
	defer func() { _ = targetRoot.Close() }()
	if targetIdentity != "" && currentIdentity != targetIdentity {
		return fmt.Errorf("%w: prepared worktree identity changed (expected %s, got %s)", state.ErrOwnership, targetIdentity, currentIdentity)
	}
	head, err := p.runGitInDirectory(ctx, targetRoot, "rev-parse", "HEAD")
	if err != nil {
		return err
	}
	if strings.TrimSpace(head.Stdout) != oid {
		return fmt.Errorf("prepared HEAD differs from requested OID")
	}
	if _, err := p.runGitInDirectory(ctx, targetRoot, "symbolic-ref", "-q", "HEAD"); err == nil {
		return errors.New("prepared worktree is not detached")
	}
	if phase == preparePhaseRestore {
		// Keep the RESTORING lock until archive.Manager has restored the
		// snapshot tree/index and run the resume-phase command.
		cleanup = false
		return nil
	}
	if _, err = p.runWorktreeAdminOwned(ctx, repo, lockedRoot, lockedRelativeTarget, target, targetIdentity, "unlock"); err != nil {
		return err
	}
	_, err = p.runWorktreeAdminOwned(ctx, repo, lockedRoot, lockedRelativeTarget, target, targetIdentity, "lock", "--reason", "wx:"+slotID+":READY")
	if err != nil {
		return err
	}
	if err := p.validatePreparedTarget(ctx, repo, target, oid, slotID, phase, lockedRoot, lockedRelativeTarget, targetIdentity, "wx worktree ownership changed before READY"); err != nil {
		return fmt.Errorf("wx worktree ownership changed before READY: %w", err)
	}
	cleanup = false
	return nil
}

type lockedTarget struct {
	root     *os.Root
	relative string
	identity string
	existing bool
	close    func()
}

func (p *Preparer) prepareLockedTarget(ctx context.Context, repo discovery.Repository, target, oid, slotID string, phase preparePhase, root string) (*lockedTarget, error) {
	prepareSlotStates, prepareRepositoryStates := preparationOwnershipStates(phase)
	// Re-open the descriptor after taking the common-directory lock and
	// repeat the physical/ownership checks. A pre-lock check alone would
	// allow a path replacement between validation and the Git operation.
	lockedRoot, lockedRelativeTarget, closeLockedRoot, err := p.openOwnedRoot(root, target)
	if err != nil {
		return nil, fmt.Errorf("revalidate wx worktree root: %w", err)
	}
	keepOpen := false
	defer func() {
		if !keepOpen {
			closeLockedRoot()
		}
	}()
	if err := lockedRoot.MkdirAll(filepath.Dir(lockedRelativeTarget), 0o700); err != nil {
		return nil, fmt.Errorf("create worktree parent safely: %w", err)
	}
	if _, err := lockedRoot.Lstat(lockedRelativeTarget); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	existingWorktree, err := p.existingTargetState(ctx, repo, target, oid, slotID, phase, root, lockedRoot, lockedRelativeTarget)
	if err != nil {
		return nil, err
	}
	targetIdentity := ""
	if existingWorktree {
		identityDirectory, identity, identityErr := domain.OpenDirectoryAt(lockedRoot, lockedRelativeTarget)
		if identityDirectory != nil {
			_ = identityDirectory.Close()
		}
		if identityErr != nil {
			return nil, fmt.Errorf("%w: capture existing worktree identity: %w", state.ErrOwnership, identityErr)
		}
		targetIdentity = identity
	}
	if err := p.validateStateOwnership(ctx, repo, target, slotID, prepareSlotStates, prepareRepositoryStates); err != nil {
		return nil, fmt.Errorf("wx worktree ownership changed before marker: %w", err)
	}
	if err := EnsureOwnershipMarkerAt(lockedRoot, root, target, p.markerIdentity(repo, slotID), string(repo.CommonDir)); err != nil {
		return nil, fmt.Errorf("prepare wx ownership marker: %w", err)
	}
	if !existingWorktree {
		parentDirectory, _, parentErr := domain.OpenDirectoryAt(lockedRoot, filepath.Dir(lockedRelativeTarget))
		if parentErr != nil {
			return nil, fmt.Errorf("%w: worktree parent ownership changed: %w", state.ErrOwnership, parentErr)
		}
		if closeErr := parentDirectory.Close(); closeErr != nil {
			return nil, fmt.Errorf("%w: close worktree parent descriptor: %w", state.ErrOwnership, closeErr)
		}
		if err := p.validateStateOwnership(ctx, repo, target, slotID, prepareSlotStates, prepareRepositoryStates); err != nil {
			return nil, fmt.Errorf("wx worktree ownership changed before creation: %w", err)
		}
		targetIdentity, err = p.addWorktreeWithIdentity(ctx, repo, lockedRoot, target, lockedRelativeTarget, oid)
		if err != nil {
			return nil, err
		}
		if err := p.verifyPreparedTargetIdentity(lockedRoot, lockedRelativeTarget, targetIdentity); err != nil {
			return nil, fmt.Errorf("prepared worktree escaped ownership root: %w", err)
		}
	}
	keepOpen = true
	return &lockedTarget{root: lockedRoot, relative: lockedRelativeTarget, identity: targetIdentity, existing: existingWorktree, close: closeLockedRoot}, nil
}

// requirePinnedRoot fails closed unless the daemon-held root descriptor is
// present and pinned to root. Production always constructs a Preparer this
// way; there is no fallback to a mutable pathname.
func (p *Preparer) requirePinnedRoot(root string) error {
	if p.OwnedRoot == nil || filepath.Clean(p.RootPath) != filepath.Clean(root) {
		return errors.New("wx worktree root descriptor is unavailable")
	}
	return nil
}

func (p *Preparer) verifyPreparedTargetIdentity(lockedRoot *os.Root, relative, expected string) error {
	if expected == "" {
		return nil
	}
	currentDirectory, currentIdentity, identityErr := domain.OpenDirectoryAt(lockedRoot, relative)
	if currentDirectory != nil {
		_ = currentDirectory.Close()
	}
	if identityErr != nil {
		return fmt.Errorf("%w: worktree target identity is unavailable: %w", state.ErrOwnership, identityErr)
	}
	if currentIdentity != expected {
		return fmt.Errorf("%w: worktree target identity changed (expected %s, got %s)", state.ErrOwnership, expected, currentIdentity)
	}
	return nil
}

func (p *Preparer) validatePreparedTarget(ctx context.Context, repo discovery.Repository, target, oid, slotID string, phase preparePhase, lockedRoot *os.Root, relative, expected, stage string) error {
	if identityErr := p.verifyPreparedTargetIdentity(lockedRoot, relative, expected); identityErr != nil {
		return fmt.Errorf("%s: %w", stage, identityErr)
	}
	if validationErr := p.validateExistingWorktreeOwnedForPhase(ctx, repo, target, oid, slotID, phase); validationErr != nil {
		return validationErr
	}
	if identityErr := p.verifyPreparedTargetIdentity(lockedRoot, relative, expected); identityErr != nil {
		return fmt.Errorf("%s after validation: %w", stage, identityErr)
	}
	return nil
}

func (p *Preparer) openOwnedRoot(root, target string) (*os.Root, string, func(), error) {
	if err := p.requirePinnedRoot(root); err != nil {
		return nil, "", func() {}, err
	}
	relative, err := filepath.Rel(filepath.Clean(root), filepath.Clean(target))
	if err != nil {
		return nil, "", func() {}, err
	}
	return p.OwnedRoot, relative, func() {}, nil
}

// addWorktree reserves the target namespace by creating/opening its final
// directory through the pinned parent descriptor, then runs Git with that
// target descriptor as its current directory and "." as the target. A
// replacement of any lexical root/ancestor/target after this point cannot
// redirect Git's checkout or its worktree registration outside the owned inode.
func (p *Preparer) addWorktree(ctx context.Context, repo discovery.Repository, owner *os.Root, target, relativeTarget, oid string) error {
	_, err := p.addWorktreeWithIdentity(ctx, repo, owner, target, relativeTarget, oid)
	return err
}

func (p *Preparer) addWorktreeWithIdentity(ctx context.Context, repo discovery.Repository, owner *os.Root, target, relativeTarget, oid string) (string, error) {
	parentRelative := filepath.Dir(relativeTarget)
	parent, _, err := domain.OpenDirectoryAt(owner, parentRelative)
	if err != nil {
		return "", fmt.Errorf("open worktree target namespace: %w", err)
	}
	defer func() { _ = parent.Close() }()
	if info, statErr := parent.Stat(); statErr != nil || !info.IsDir() {
		if statErr != nil {
			return "", fmt.Errorf("validate worktree target namespace: %w", statErr)
		}
		return "", errors.New("worktree target namespace is not a directory")
	}
	name := filepath.Base(relativeTarget)
	if name == "." || name == string(filepath.Separator) || name == "" {
		return "", errors.New("worktree target has no relative leaf")
	}
	// Reserve the final leaf with mkdirat. Using owner.Mkdir(relativeTarget)
	// here would reopen the parent pathname after the descriptor barrier.
	if err := unix.Mkdirat(int(parent.Fd()), name, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return "", fmt.Errorf("reserve worktree target namespace: %w", err)
	}
	targetDirectory, targetIdentity, err := domain.OpenDirectoryAt(owner, relativeTarget)
	if err != nil {
		return "", fmt.Errorf("open reserved worktree target: %w", err)
	}
	defer func() { _ = targetDirectory.Close() }()
	// --git-dir identifies the source repository without changing cwd.  -C
	// would make Git resolve the target relative to repo.MainPath, defeating
	// the descriptor-bound target namespace established above.
	_, err = p.Git.RunAt(ctx, targetDirectory, nil, nil, "--git-dir", string(repo.CommonDir), "worktree", "add", "--detach", ".", oid)
	if err == nil {
		return targetIdentity, nil
	}
	// Git may have updated its common-directory registration before reporting
	// an error (or the child may have been interrupted). If the descriptor
	// namespace or registration cannot be proved clean, preserve the target for
	// the daemon's ownership quarantine instead of turning an ambiguous add into
	// a generic FAILED slot.
	entries, readErr := targetDirectory.Readdirnames(-1)
	if readErr != nil {
		return "", fmt.Errorf("%w: inspect git worktree add target: %w", state.ErrOwnership, readErr)
	}
	if len(entries) > 0 {
		return "", fmt.Errorf("%w: git worktree add outcome is uncertain: %w", state.ErrOwnership, err)
	}
	_, _, found, inspectErr := RegisteredWorktreeLockStatusAt(ctx, p.Git, string(repo.MainPath), owner, p.RootPath, relativeTarget, targetIdentity)
	if inspectErr != nil || found {
		if inspectErr == nil {
			inspectErr = errors.New("Git registration remains after failed worktree add")
		}
		return "", fmt.Errorf("%w: git worktree add registration is uncertain: %w", state.ErrOwnership, inspectErr)
	}
	var gitErr *gitx.Error
	if errors.As(err, &gitErr) && gitErr.Result.ExitCode < 0 {
		return "", fmt.Errorf("%w: git worktree add process outcome is uncertain: %w", state.ErrOwnership, err)
	}
	return "", err
}

func (p *Preparer) runWorktreeAdmin(ctx context.Context, repo discovery.Repository, owner *os.Root, relativeTarget, target string, args ...string) (gitx.Result, error) {
	return p.runWorktreeAdminOwned(ctx, repo, owner, relativeTarget, target, "", args...)
}

// RemoveWorktreeAt performs the destructive worktree removal from the
// descriptor-bound target inode. The caller must have completed its ownership
// checks while holding the Git common-directory lock. A failure after those
// checks is ownership-uncertain because the target may have been renamed or
// replaced while Git was being started, so it is returned as ErrOwnership.
func (p *Preparer) RemoveWorktreeAt(ctx context.Context, repo discovery.Repository, root, target, expectedIdentity string) error {
	if err := p.requirePinnedRoot(root); err != nil {
		return fmt.Errorf("%w: descriptor-bound worktree removal requires the pinned root", state.ErrOwnership)
	}
	relativeTarget, err := domain.RelativeWithin(filepath.Clean(root), filepath.Clean(target))
	if err != nil {
		return fmt.Errorf("%w: worktree target is outside the pinned root: %w", state.ErrOwnership, err)
	}
	if _, err := p.runWorktreeAdminOwned(ctx, repo, p.OwnedRoot, relativeTarget, target, expectedIdentity, "remove", "--force"); err != nil {
		return fmt.Errorf("%w: descriptor-bound worktree removal is uncertain: %w", state.ErrOwnership, err)
	}
	return nil
}

func (p *Preparer) runWorktreeAdminOwned(ctx context.Context, repo discovery.Repository, owner *os.Root, relativeTarget, target, expectedIdentity string, args ...string) (gitx.Result, error) {
	if p.OwnedRoot == nil {
		return gitx.Result{}, errors.New("wx worktree root descriptor is unavailable")
	}
	targetDirectory, identity, err := domain.OpenDirectoryAt(owner, relativeTarget)
	if err != nil {
		return gitx.Result{}, fmt.Errorf("open worktree target namespace: %w", err)
	}
	defer func() { _ = targetDirectory.Close() }()
	if expectedIdentity != "" && identity != expectedIdentity {
		return gitx.Result{}, fmt.Errorf("%w: worktree target identity changed (expected %s, got %s)", state.ErrOwnership, expectedIdentity, identity)
	}
	// Keep cwd at the descriptor-bound target itself. Supplying --git-dir selects
	// the source repository while "." identifies the reserved worktree inode.
	command := append([]string{"--git-dir", string(repo.CommonDir), "worktree"}, args...)
	command = append(command, ".")
	result, runErr := p.Git.RunAt(ctx, targetDirectory, nil, nil, command...)
	if runErr != nil && expectedIdentity != "" {
		currentDirectory, currentIdentity, identityErr := domain.OpenDirectoryAt(owner, relativeTarget)
		if currentDirectory != nil {
			_ = currentDirectory.Close()
		}
		if identityErr != nil {
			return result, fmt.Errorf("%w: worktree target identity became unavailable during Git operation: %w", state.ErrOwnership, identityErr)
		}
		if currentIdentity != expectedIdentity {
			return result, fmt.Errorf("%w: worktree target identity changed during Git operation (expected %s, got %s)", state.ErrOwnership, expectedIdentity, currentIdentity)
		}
	}
	return result, runErr
}

// PrepareResumeWithIdentity is the descriptor-bound resume phase. The
// identity is supplied by archive.Manager after the clean base is prepared and
// remains attached through the resume command and its final ownership proof.
func (p *Preparer) PrepareResumeWithIdentity(ctx context.Context, repo discovery.Repository, target, oid, slotID, expectedIdentity string) error {
	if err := p.VerifyWorktreeIdentity(target, expectedIdentity); err != nil {
		return fmt.Errorf("validate restoring worktree identity before resume prepare: %w", err)
	}
	if err := p.validateExistingWorktreeOwnedForPhase(ctx, repo, target, oid, slotID, preparePhaseRestore); err != nil {
		return fmt.Errorf("validate restoring worktree before resume prepare: %w", err)
	}
	if err := p.runPrepareWithIdentity(ctx, repo, target, expectedIdentity); err != nil {
		return err
	}
	if err := p.VerifyWorktreeIdentity(target, expectedIdentity); err != nil {
		return fmt.Errorf("wx worktree ownership changed after resume prepare: %w", err)
	}
	if err := p.validateExistingWorktreeOwnedForPhase(ctx, repo, target, oid, slotID, preparePhaseRestore); err != nil {
		return fmt.Errorf("wx worktree ownership changed during resume prepare: %w", err)
	}
	return nil
}

// FinishRestoreWithIdentity transitions a restored worktree to READY while
// keeping the physical target identity attached to both Git admin commands.
func (p *Preparer) FinishRestoreWithIdentity(ctx context.Context, repo discovery.Repository, target, oid, slotID, expectedIdentity string) error {
	if err := p.validateExistingWorktreeOwnedForPhase(ctx, repo, target, oid, slotID, preparePhaseRestore); err != nil {
		return err
	}
	if expectedIdentity == "" {
		if identity, identityErr := p.WorktreeIdentity(target); identityErr == nil {
			expectedIdentity = identity
		}
	}
	if err := p.VerifyWorktreeIdentity(target, expectedIdentity); err != nil {
		return err
	}
	root, err := config.ExpandHome(p.Config.Storage.WorktreeRoot)
	if err != nil {
		return err
	}
	owner, relativeTarget, closeOwner, err := p.openOwnedRoot(root, filepath.Clean(target))
	if err != nil {
		return fmt.Errorf("open restored worktree namespace: %w", err)
	}
	defer closeOwner()
	if _, err := p.runWorktreeAdminOwned(ctx, repo, owner, relativeTarget, target, expectedIdentity, "unlock"); err != nil {
		return fmt.Errorf("unlock restored worktree: %w", err)
	}
	if _, err := p.runWorktreeAdminOwned(ctx, repo, owner, relativeTarget, target, expectedIdentity, "lock", "--reason", "wx:"+slotID+":READY"); err != nil {
		return err
	}
	if err := p.VerifyWorktreeIdentity(target, expectedIdentity); err != nil {
		return fmt.Errorf("validate restored READY worktree identity: %w", err)
	}
	if err := p.validateExistingWorktreeOwnedForPhase(ctx, repo, target, oid, slotID, preparePhaseRestore); err != nil {
		return fmt.Errorf("validate restored READY worktree: %w", err)
	}
	return nil
}

func (p *Preparer) existingTargetState(ctx context.Context, repo discovery.Repository, target, oid, slotID string, phase preparePhase, root string, ownedRoot *os.Root, relativeTarget string) (bool, error) {
	info, err := ownedRoot.Lstat(relativeTarget)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return false, errors.New("target is not a physical directory")
	}
	entries, err := readOwnedDirectory(ownedRoot, relativeTarget)
	if err != nil {
		return false, err
	}
	if len(entries) == 0 {
		// An empty shell may be left by an interrupted allocation. A marker,
		// when present, still has to match; a missing marker is created below.
		markerRelative, markerErr := ownershipMarkerRelative(root, target, string(repo.ID))
		if markerErr != nil {
			return false, markerErr
		}
		if _, markerErr := ownedRoot.Lstat(markerRelative); markerErr != nil && !errors.Is(markerErr, os.ErrNotExist) {
			return false, markerErr
		}
		return false, nil
	}
	if err := p.validateExistingWorktreeOwnedForPhase(ctx, repo, target, oid, slotID, phase); err != nil {
		return false, fmt.Errorf("non-empty target is not the expected worktree: %w", err)
	}
	return true, nil
}

func readOwnedDirectory(root *os.Root, relative string) ([]string, error) {
	directory, _, err := domain.OpenDirectoryAt(root, relative)
	if err != nil {
		return nil, err
	}
	defer func() { _ = directory.Close() }()
	return directory.Readdirnames(-1)
}

func (p *Preparer) validateExistingWorktree(ctx context.Context, repo discovery.Repository, target, oid string) error {
	return p.validateExistingWorktreeOwnedForPhase(ctx, repo, target, oid, "", preparePhaseCreate)
}

func (p *Preparer) validateExistingWorktreeOwnedForPhase(ctx context.Context, repo discovery.Repository, target, oid, slotID string, phase preparePhase) error {
	slotStates, repositoryStates := preparationOwnershipStates(phase)
	err := p.validateExistingWorktreeOwnedForStates(ctx, repo, target, oid, slotID, slotStates, repositoryStates)
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, state.ErrOwnership) {
		return err
	}
	return fmt.Errorf("%w: %w", state.ErrOwnership, err)
}

// ValidateSlotWorktreeOwnership verifies a worktree that was already marked
// READY at the repository level before the enclosing prepare/restore job
// committed its slot state. A daemon crash can leave that durable intermediate
// state behind; replay must prove the exact slot/path/registration before
// promoting the slot instead of treating the READY row as sufficient.
func (p *Preparer) ValidateSlotWorktreeOwnership(ctx context.Context, repo discovery.Repository, target, oid, slotID string) error {
	return p.validateSlotWorktreeOwnershipForPhase(ctx, repo, target, oid, slotID, preparePhaseCreate)
}

// ValidateRestoringSlotWorktreeOwnership is the replay check for a repository
// that was durably marked READY during restore. It keeps the enclosing slot in
// RESTORING instead of widening the proof to unrelated lifecycle states.
func (p *Preparer) ValidateRestoringSlotWorktreeOwnership(ctx context.Context, repo discovery.Repository, target, oid, slotID string) error {
	return p.validateSlotWorktreeOwnershipForPhase(ctx, repo, target, oid, slotID, preparePhaseRestore)
}

func (p *Preparer) validateSlotWorktreeOwnershipForPhase(ctx context.Context, repo discovery.Repository, target, oid, slotID string, phase preparePhase) error {
	if slotID == "" {
		return fmt.Errorf("%w: slot ID is required for replay validation", state.ErrOwnership)
	}
	slotStates, repositoryStates := preparationOwnershipStates(phase)
	// The repository row is already READY at this crash-replay boundary, while
	// the enclosing slot is still in its phase state. Keep that one durable
	// intermediate state eligible without accepting unrelated lifecycle states.
	repositoryStates = append(repositoryStates, "READY")
	if err := p.validateExistingWorktreeOwnedForStates(ctx, repo, target, oid, slotID, slotStates, repositoryStates); err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return err
		}
		return fmt.Errorf("%w: validate replayed slot worktree: %w", state.ErrOwnership, err)
	}
	return nil
}

func (p *Preparer) validateExistingWorktreeOwnedForStates(ctx context.Context, repo discovery.Repository, target, oid, slotID string, slotStates, repositoryStates []string) error {
	root, err := config.ExpandHome(p.Config.Storage.WorktreeRoot)
	if err != nil {
		return err
	}
	target = filepath.Clean(target)
	if !domain.IsWithin(root, target) {
		return errors.New("worktree target is outside wx ownership root")
	}
	owner, relativeTarget, closeOwner, err := p.openOwnedRoot(root, target)
	if err != nil {
		return fmt.Errorf("open worktree ownership root: %w", err)
	}
	defer closeOwner()
	if err := ValidateOwnershipMarkerAt(owner, root, target, p.markerIdentity(repo, slotID), string(repo.CommonDir)); err != nil {
		return err
	}
	ownedTarget, targetIdentity, err := domain.OpenDirectoryAt(owner, relativeTarget)
	if err != nil {
		return fmt.Errorf("worktree target is not physical: %w", err)
	}
	targetRoot := ownedTarget
	defer func() { _ = targetRoot.Close() }()
	gitMarker, err := owner.Lstat(filepath.Join(relativeTarget, ".git"))
	if err != nil || gitMarker.Mode()&os.ModeSymlink != 0 {
		return errors.New("missing or unsafe .git marker")
	}
	common, err := p.runGitInDirectory(ctx, targetRoot, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return err
	}
	expectedCommon, err := filepath.EvalSymlinks(string(repo.CommonDir))
	if err != nil {
		return err
	}
	actualCommon, err := filepath.EvalSymlinks(strings.TrimSpace(common.Stdout))
	if err != nil || actualCommon != expectedCommon {
		return errors.New("common Git directory does not match")
	}
	head, err := p.runGitInDirectory(ctx, targetRoot, "rev-parse", "HEAD")
	detached := false
	if err == nil {
		_, detachedErr := p.runGitInDirectory(ctx, targetRoot, "symbolic-ref", "-q", "HEAD")
		detached = detachedErr != nil
	}
	if err != nil || strings.TrimSpace(head.Stdout) != oid || !detached {
		return errors.New("HEAD is not the expected detached commit")
	}
	if err := ValidateRegisteredWorktreeAt(ctx, p.Git, string(repo.MainPath), owner, root, relativeTarget, targetIdentity, slotID, slotID != ""); err != nil {
		return err
	}
	if slotID == "" {
		return nil
	}
	proof, err := p.stateOwnershipProof(ctx, repo, target, slotID, slotStates, repositoryStates)
	if err != nil {
		return err
	}
	// A recorded identity has to be the directory actually open. Preparation
	// and restore record it only once the worktree is complete, so an empty
	// record means a run that was interrupted before that point and is
	// allowed to converge on retry. A record that differs means this is not
	// the directory wx prepared, however well the marker and the Git
	// metadata reproduce it.
	if proof.DirIdentity != "" && proof.DirIdentity != targetIdentity {
		return fmt.Errorf("%w: worktree directory identity does not match the SQLite record", state.ErrOwnership)
	}
	return nil
}

func (p *Preparer) runGitInDirectory(ctx context.Context, directory *os.File, args ...string) (gitx.Result, error) {
	return p.Git.RunAt(ctx, directory, nil, nil, args...)
}

// WorktreeIdentity returns the device/inode identity of target through the
// configured ownership root. Callers that perform more than one operation on
// a restored or leased worktree carry this identity across the sequence so a
// replacement target cannot be mistaken for the original slot.
func (p *Preparer) WorktreeIdentity(target string) (string, error) {
	root, err := config.ExpandHome(p.Config.Storage.WorktreeRoot)
	if err != nil {
		return "", err
	}
	target = filepath.Clean(target)
	if !domain.IsWithin(root, target) {
		return "", fmt.Errorf("worktree target is outside wx ownership root")
	}
	owner, relative, closeOwner, err := p.openOwnedRoot(root, target)
	if err != nil {
		return "", err
	}
	defer closeOwner()
	directory, identity, err := domain.OpenDirectoryAt(owner, relative)
	if err != nil {
		return "", err
	}
	if err := directory.Close(); err != nil {
		return "", err
	}
	return identity, nil
}

// VerifyWorktreeIdentity rejects a target that no longer names the physical
// directory captured by the caller. An empty expected identity keeps the
// compatibility path used by older in-process callers.
func (p *Preparer) VerifyWorktreeIdentity(target, expectedIdentity string) error {
	if expectedIdentity == "" {
		return nil
	}
	actual, err := p.WorktreeIdentity(target)
	if err != nil {
		return fmt.Errorf("%w: worktree target identity is unavailable: %w", state.ErrOwnership, err)
	}
	if actual != expectedIdentity {
		return fmt.Errorf("%w: worktree target identity changed (expected %s, got %s)", state.ErrOwnership, expectedIdentity, actual)
	}
	return nil
}

// RunGitInWorktree runs a Git command in a target whose identity was captured
// by WorktreeIdentity. Production preparers use the pinned root and descriptor
// cwd, so a lexical root/target replacement cannot redirect the command. The
// identity is checked both before and after the child runs; a changed target is
// ownership-uncertain even when Git itself reports success or failure.
func (p *Preparer) RunGitInWorktree(ctx context.Context, target, expectedIdentity string, env []string, input []byte, args ...string) (gitx.Result, error) {
	root, err := config.ExpandHome(p.Config.Storage.WorktreeRoot)
	if err != nil {
		return gitx.Result{}, err
	}
	owner, relative, closeOwner, err := p.openOwnedRoot(root, filepath.Clean(target))
	if err != nil {
		return gitx.Result{}, fmt.Errorf("open worktree command root: %w", err)
	}
	defer closeOwner()
	directory, identity, err := domain.OpenDirectoryAt(owner, relative)
	if err != nil {
		return gitx.Result{}, fmt.Errorf("open worktree command directory: %w", err)
	}
	defer func() { _ = directory.Close() }()
	if expectedIdentity != "" && identity != expectedIdentity {
		return gitx.Result{}, fmt.Errorf("%w: worktree target identity changed before Git (expected %s, got %s)", state.ErrOwnership, expectedIdentity, identity)
	}
	result, runErr := p.Git.RunAt(ctx, directory, env, input, args...)
	if expectedIdentity != "" {
		if identityErr := p.VerifyWorktreeIdentity(target, expectedIdentity); identityErr != nil {
			return result, fmt.Errorf("worktree target identity changed during Git: %w", identityErr)
		}
	}
	return result, runErr
}

// ValidateReady verifies the physical and Git-administrative invariants that
// make a stored READY worktree safe to lease.
func (p *Preparer) ValidateReady(ctx context.Context, repo discovery.Repository, target, oid string) error {
	if err := p.ValidateOwnership(ctx, repo, target, oid); err != nil {
		return err
	}
	root, err := config.ExpandHome(p.Config.Storage.WorktreeRoot)
	if err != nil {
		return err
	}
	common, err := filepath.EvalSymlinks(string(repo.CommonDir))
	if err != nil {
		return err
	}
	slotID, err := p.validateRemovalOwnership(repo, root, target, common)
	if err != nil {
		return err
	}
	if err := p.validateStateOwnership(ctx, repo, target, slotID, []string{"READY"}, []string{"READY"}); err != nil {
		return err
	}
	return p.validateTrackedClean(ctx, target)
}

// ValidateOwnership verifies the physical and Git-administrative ownership
// invariants without requiring a clean index or working tree. Restored leased
// sessions intentionally contain the archived user's tracked changes.
func (p *Preparer) ValidateOwnership(ctx context.Context, repo discovery.Repository, target, oid string) error {
	if err := p.validateExistingWorktree(ctx, repo, target, oid); err != nil {
		return err
	}
	root, err := config.ExpandHome(p.Config.Storage.WorktreeRoot)
	if err != nil {
		return err
	}
	common, err := filepath.EvalSymlinks(string(repo.CommonDir))
	if err != nil {
		return err
	}
	slotID, err := p.validateRemovalOwnership(repo, root, target, common)
	if err != nil {
		return err
	}
	owner, relativeTarget, closeOwner, openErr := p.openOwnedRoot(root, target)
	if openErr != nil {
		return openErr
	}
	defer closeOwner()
	directory, targetIdentity, openErr := domain.OpenDirectoryAt(owner, relativeTarget)
	if openErr != nil {
		return openErr
	}
	if closeErr := directory.Close(); closeErr != nil {
		return closeErr
	}
	if err := ValidateRegisteredWorktreeAt(ctx, p.Git, string(repo.MainPath), owner, root, relativeTarget, targetIdentity, slotID, true); err != nil {
		return err
	}
	return p.validateStateOwnershipWithIdentity(ctx, repo, target, slotID, targetIdentity, allOwnershipSlotStates, allOwnershipRepositoryStates)
}

func (p *Preparer) validateRemovalOwnership(repo discovery.Repository, root, target, common string) (string, error) {
	owner, _, closeOwner, err := p.openOwnedRoot(root, target)
	if err != nil {
		return "", err
	}
	defer closeOwner()
	return ValidateRemovalOwnershipAt(owner, root, target, p.markerIdentity(repo, ""), common)
}

// ValidateRestoringOwnership is the slot-bound ownership check used while a
// restore lock is held. Keeping the slot ID in this check prevents a restore
// handoff from accepting a different wx lock reason at the same path.
func (p *Preparer) ValidateRestoringOwnership(ctx context.Context, repo discovery.Repository, target, oid, slotID string) error {
	return p.validateExistingWorktreeOwnedForPhase(ctx, repo, target, oid, slotID, preparePhaseRestore)
}

var (
	allOwnershipSlotStates       = []string{"PREPARING", "RESTORING", "READY", "LEASED", "DRAINING", "SNAPSHOTTING", "SNAPSHOTTED", "ARCHIVED", "REMOVING", "RETIRING"}
	allOwnershipRepositoryStates = []string{"PREPARING", "PREPARE_RUNNING", "RESTORING", "RESTORE_RUNNING", "READY", "LEASED", "RETIRING"}
)

func preparationOwnershipStates(phase preparePhase) ([]string, []string) {
	if phase == preparePhaseRestore {
		return []string{"RESTORING"}, []string{"RESTORING", "RESTORE_RUNNING"}
	}
	return []string{"PREPARING"}, []string{"PREPARING", "PREPARE_RUNNING"}
}

func (p *Preparer) validateStateOwnership(ctx context.Context, repo discovery.Repository, target, slotID string, slotStates, repositoryStates []string) error {
	_, err := p.stateOwnershipProof(ctx, repo, target, slotID, slotStates, repositoryStates)
	return err
}

// stateOwnershipProof is validateStateOwnership with the proof returned, for
// the caller that needs the recorded identity to compare it itself.
func (p *Preparer) stateOwnershipProof(ctx context.Context, repo discovery.Repository, target, slotID string, slotStates, repositoryStates []string) (state.WorktreeOwnership, error) {
	if slotID == "" {
		return state.WorktreeOwnership{}, nil
	}
	if p.Ownership == nil {
		return state.WorktreeOwnership{}, fmt.Errorf("%w: state-backed worktree ownership validator is required", state.ErrOwnership)
	}
	dirName, err := p.worktreeDirName(target)
	if err != nil {
		return state.WorktreeOwnership{}, err
	}
	return p.Ownership.ValidateWorktreeOwnership(ctx, state.WorktreeOwnershipRequest{
		SlotID:       slotID,
		RepositoryID: string(repo.ID),
		WorkspaceID:  "",
		RootID:       p.RootID,
		SlotRelPath:  p.SlotRelPath,
		DirName:      dirName,
		// DirIdentity is intentionally left empty here. This helper runs
		// both before the worktree directory exists (the pre-creation
		// checks inside prepare) and after, so it cannot promise an
		// identity; the callers that hold a descriptor pass one through
		// validateStateOwnershipWithIdentity instead, and the caller that
		// only has a record to compare reads it from the returned proof.
		CommonDir:               string(repo.CommonDir),
		AllowedSlotStates:       slotStates,
		AllowedRepositoryStates: repositoryStates,
	})
}

// validateStateOwnershipWithIdentity is the fail-closed form used once the
// caller holds the worktree directory open. Presenting the identity makes a
// missing SQLite record a failure rather than a silent pass.
func (p *Preparer) validateStateOwnershipWithIdentity(ctx context.Context, repo discovery.Repository, target, slotID, dirIdentity string, slotStates, repositoryStates []string) error {
	if slotID == "" {
		return nil
	}
	if p.Ownership == nil {
		return fmt.Errorf("%w: state-backed worktree ownership validator is required", state.ErrOwnership)
	}
	if dirIdentity == "" {
		return fmt.Errorf("%w: worktree directory identity is unavailable", state.ErrOwnership)
	}
	dirName, err := p.worktreeDirName(target)
	if err != nil {
		return err
	}
	_, err = p.Ownership.ValidateWorktreeOwnership(ctx, state.WorktreeOwnershipRequest{
		SlotID:                  slotID,
		RepositoryID:            string(repo.ID),
		RootID:                  p.RootID,
		SlotRelPath:             p.SlotRelPath,
		DirName:                 dirName,
		DirIdentity:             dirIdentity,
		CommonDir:               string(repo.CommonDir),
		AllowedSlotStates:       slotStates,
		AllowedRepositoryStates: repositoryStates,
	})
	return err
}

func (p *Preparer) validateTrackedClean(ctx context.Context, target string) error {
	root, err := config.ExpandHome(p.Config.Storage.WorktreeRoot)
	if err != nil {
		return err
	}
	owner, relative, closeOwner, err := p.openOwnedRoot(root, target)
	if err != nil {
		return err
	}
	defer closeOwner()
	directory, _, err := domain.OpenDirectoryAt(owner, relative)
	if err != nil {
		return err
	}
	defer func() { _ = directory.Close() }()
	status, err := p.Git.RunAt(ctx, directory, nil, nil, "status", "--porcelain=v1", "--untracked-files=no")
	if err != nil {
		return err
	}
	if strings.TrimSpace(status.Stdout) != "" {
		return errors.New("prepared worktree has tracked changes")
	}
	return nil
}

func (p *Preparer) copyIncludes(repo discovery.Repository, target string) error {
	root, err := config.ExpandHome(p.Config.Storage.WorktreeRoot)
	if err != nil {
		return err
	}
	owner, relativeTarget, closeOwner, err := p.openOwnedRoot(root, filepath.Clean(target))
	if err != nil {
		return err
	}
	defer closeOwner()
	return p.copyIncludesAt(repo, owner, relativeTarget)
}

// defaultIncludeNames are the agent rule and tool configuration files that are
// kept out of version control by convention. Git does not carry them into a
// worktree, so an agent started there loses the local rules their author wrote
// for that repository, and the documented workaround for it is to import a
// file from the home directory instead. The workspace-root materializer
// already copies the same class of file without a manifest
// (MaterializeRootAt); this keeps the per-repository side symmetric.
//
// Only regular files are copied, and only while Git does not track them. A
// tracked path is checked out by the worktree itself, and a directory or
// symlink under one of these names belongs to a sharing scheme its author
// chose deliberately: both stay the job of an explicit .worktreeinclude entry.
var defaultIncludeNames = []string{
	// Claude Code
	"CLAUDE.local.md",
	".claudeignore",
	// Codex and the other AGENTS.md readers
	"AGENTS.local.md",
	"AGENTS.override.md",
	// Gemini CLI
	"GEMINI.local.md",
	".geminiignore",
	".aiexclude",
	// Cursor
	".cursorrules",
	".cursorignore",
	// Windsurf
	".windsurfrules",
	".codeiumignore",
	// Cline, Roo Code, Kilo Code
	".clinerules",
	".roorules",
	".kilocoderules",
	// Aider
	".aider.conf.yml",
	// MCP servers, shared by several agents
	".mcp.json",
}

// defaultIncludeCandidates returns the default names that exist in the main
// worktree as regular physical files. A name listed in .worktreelink is left
// out: an explicit link rule owns that path, and copying it first would turn
// createLinksAt into a target collision. Absence is not an error here, since
// the list is applied to every repository and only the names an author keeps
// locally should reach the worktree.
func defaultIncludeCandidates(mainPath string, c config.Config) ([]string, error) {
	if !c.DefaultAgentRulesEnabled(mainPath) {
		return nil, nil
	}
	linkPatterns, err := readPhysicalPatterns(mainPath, ".worktreelink")
	if err != nil {
		return nil, err
	}
	linked := map[string]bool{}
	for _, pattern := range linkPatterns {
		linked[filepath.Clean(pattern)] = true
	}
	root, err := OpenPhysicalRoot(mainPath)
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()
	out := make([]string, 0, len(defaultIncludeNames))
	for _, name := range defaultIncludeNames {
		clean, err := safeRelative(name)
		if err != nil {
			return nil, err
		}
		if linked[clean] {
			continue
		}
		info, err := domain.PhysicalPathInfo(root, clean)
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		out = append(out, clean)
	}
	return out, nil
}

// defaultIncludes narrows the candidates to the names Git does not track. One
// ls-files call answers for the whole list: the names are checked on every
// cold start, and a tracked one is skipped rather than reported, so that a
// repository that commits a file under one of these names still prepares.
func (p *Preparer) defaultIncludes(mainPath string) ([]string, error) {
	candidates, err := defaultIncludeCandidates(mainPath, p.Config)
	if err != nil {
		return nil, err
	}
	if len(candidates) == 0 {
		return nil, nil
	}
	listed, err := p.Git.Run(context.Background(), mainPath, append([]string{"ls-files", "-z", "--"}, candidates...)...)
	if err != nil {
		return nil, err
	}
	tracked := map[string]bool{}
	for _, entry := range strings.Split(listed.Stdout, "\x00") {
		if entry == "" {
			continue
		}
		tracked[filepath.Clean(entry)] = true
	}
	out := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		if tracked[candidate] {
			continue
		}
		out = append(out, candidate)
	}
	return out, nil
}

// copyIncludesAt is the descriptor-bound include materializer. destinationRoot
// is opened from the already pinned owner namespace and every write is
// relative to it; the lexical target pathname is never used for a destination
// syscall.
func (p *Preparer) copyIncludesAt(repo discovery.Repository, owner *os.Root, relativeTarget string) error {
	patterns, err := readPhysicalPatterns(string(repo.MainPath), ".worktreeinclude")
	if err != nil {
		return err
	}
	defaults, err := p.defaultIncludes(string(repo.MainPath))
	if err != nil {
		return err
	}
	destinationRoot, err := domain.OpenRootAt(owner, relativeTarget)
	if err != nil {
		return fmt.Errorf("open include destination: %w", err)
	}
	defer func() { _ = destinationRoot.Close() }()
	// Defaults run first so that an explicit .worktreeinclude entry for the
	// same path is the one that decides the final content.
	for _, rel := range defaults {
		sourceRoot, sourceErr := OpenPhysicalRoot(string(repo.MainPath))
		if sourceErr != nil {
			return sourceErr
		}
		copyErr := copyPathFromOwnedRoot(sourceRoot, rel, destinationRoot, rel)
		_ = sourceRoot.Close()
		if copyErr != nil {
			return fmt.Errorf("copy default include %s: %w", rel, copyErr)
		}
	}
	for _, pattern := range patterns {
		clean := filepath.Clean(pattern)
		if filepath.IsAbs(pattern) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
			return fmt.Errorf("unsafe .worktreeinclude pattern %q", pattern)
		}
		matches, err := safeGlob(string(repo.MainPath), pattern)
		if err != nil {
			return err
		}
		for _, src := range matches {
			rel, err := filepath.Rel(string(repo.MainPath), src)
			if err != nil {
				return err
			}
			rel, err = safeRelative(rel)
			if err != nil {
				return fmt.Errorf("unsafe .worktreeinclude match %q: %w", src, err)
			}
			tracked, err := p.Git.Run(context.Background(), string(repo.MainPath), "ls-files", "--error-unmatch", "--", rel)
			if err == nil && strings.TrimSpace(tracked.Stdout) != "" {
				return fmt.Errorf(".worktreeinclude would overwrite tracked path %s", rel)
			}
			sourceRoot, sourceErr := OpenPhysicalRoot(string(repo.MainPath))
			if sourceErr != nil {
				return sourceErr
			}
			copyErr := copyPathFromOwnedRoot(sourceRoot, rel, destinationRoot, rel)
			_ = sourceRoot.Close()
			if copyErr != nil {
				return copyErr
			}
		}
	}
	return nil
}

func (p *Preparer) createLinks(ctx context.Context, repo discovery.Repository, target string) error {
	root, err := config.ExpandHome(p.Config.Storage.WorktreeRoot)
	if err != nil {
		return err
	}
	owner, relativeTarget, closeOwner, err := p.openOwnedRoot(root, filepath.Clean(target))
	if err != nil {
		return err
	}
	defer closeOwner()
	return p.createLinksAt(ctx, repo, owner, relativeTarget)
}

// createLinksAt mirrors createLinks while keeping the destination Root open
// across all checks and symlink creation. This closes the root replacement
// window between validating the worktree and writing an ignored link.
func (p *Preparer) createLinksAt(ctx context.Context, repo discovery.Repository, owner *os.Root, relativeTarget string) error {
	patterns, err := readPhysicalPatterns(string(repo.MainPath), ".worktreelink")
	if err != nil {
		return err
	}
	if err := validateRuleConflicts(nil, patterns); err != nil {
		return err
	}
	destinationRoot, err := domain.OpenRootAt(owner, relativeTarget)
	if err != nil {
		return fmt.Errorf("open link destination: %w", err)
	}
	defer func() { _ = destinationRoot.Close() }()
	for _, rel := range patterns {
		clean := filepath.Clean(rel)
		if filepath.IsAbs(rel) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
			return fmt.Errorf("unsafe .worktreelink path %q", rel)
		}
		if _, err := p.Git.Run(ctx, string(repo.MainPath), "check-ignore", "-q", "--", clean); err != nil {
			return fmt.Errorf(".worktreelink path %q is not ignored", rel)
		}
		source := filepath.Join(string(repo.MainPath), clean)
		if err := domain.ValidatePhysicalPath(source, false); err != nil {
			return fmt.Errorf(".worktreelink source %s is not physical: %w", rel, err)
		}
		destinationRelative, err := safeRelative(clean)
		if err != nil {
			return err
		}
		if err := ensureRootDirectory(destinationRoot, filepath.Dir(destinationRelative)); err != nil {
			return err
		}
		if info, err := destinationRoot.Lstat(destinationRelative); err == nil {
			if info.Mode()&os.ModeSymlink != 0 {
				existing, readErr := destinationRoot.Readlink(destinationRelative)
				if readErr == nil && existing == source {
					continue
				}
			}
			return fmt.Errorf(".worktreelink target collision %s", rel)
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err := destinationRoot.Symlink(source, destinationRelative); err != nil {
			return err
		}
	}
	return nil
}

func (p *Preparer) runPrepareWithIdentity(ctx context.Context, repo discovery.Repository, target, expectedIdentity string) error {
	override, ok := p.Config.Repositories[string(repo.MainPath)]
	if !ok || len(override.Prepare.Command) == 0 {
		return nil
	}
	timeout := override.Prepare.Timeout.Duration
	if timeout <= 0 {
		timeout = p.Config.Readiness.Timeout.Duration
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	root, err := config.ExpandHome(p.Config.Storage.WorktreeRoot)
	if err != nil {
		return fmt.Errorf("open prepare command directory: %w", err)
	}
	owner, relative, closeOwner, err := p.openOwnedRoot(root, target)
	if err != nil {
		return fmt.Errorf("open prepare command directory: %w", err)
	}
	defer closeOwner()
	directory, identity, err := domain.OpenDirectoryAt(owner, relative)
	if err != nil {
		return fmt.Errorf("open prepare command directory: %w", err)
	}
	defer func() { _ = directory.Close() }()
	if expectedIdentity != "" && identity != expectedIdentity {
		return fmt.Errorf("%w: worktree target identity changed before prepare command (expected %s, got %s)", state.ErrOwnership, expectedIdentity, identity)
	}
	cmd, err := fdexec.Start(cctx, p.Git.FDHelper, directory, os.Environ(), override.Prepare.Command...)
	if err != nil {
		return err
	}
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	runErr := cmd.Run()
	if expectedIdentity != "" {
		if identityErr := p.VerifyWorktreeIdentity(target, expectedIdentity); identityErr != nil {
			return fmt.Errorf("worktree target identity changed during prepare command: %w", identityErr)
		}
	}
	return runErr
}

// Fingerprint hashes everything that makes a prepared worktree reusable.
// dirName is the repository's directory name inside the slot, hashed so that
// changing storage.repo_dir_source or a repositories.<path>.dir_name
// override makes existing READY slots stop matching and be rebuilt at the
// new location instead of being leased at the old one.
//
// schema=3 marks the layout change that introduced dirName; a fingerprint
// written by an earlier wx therefore never compares equal.
func Fingerprint(generation int, oid string, repo discovery.Repository, dirName string, c config.Config) (string, error) {
	if err := domain.ValidatePhysicalPath(string(repo.MainPath), false); err != nil {
		return "", err
	}
	h := sha256.New()
	_, _ = fmt.Fprintf(h, "schema=3\ngeneration=%d\noid=%s\ndir=%s\n", generation, oid, dirName)
	for _, name := range []string{".worktreeinclude", ".worktreelink"} {
		data, err := readPhysicalManifest(string(repo.MainPath), name)
		if err != nil {
			return "", err
		}
		_, _ = fmt.Fprintf(h, "manifest=%s:%x\n", name, sha256.Sum256(data))
	}
	patterns, err := readPhysicalPatterns(string(repo.MainPath), ".worktreeinclude")
	if err != nil {
		return "", err
	}
	seenIncludes := map[string]bool{}
	// The default includes are hashed without the tracked check copyIncludesAt
	// applies, because Fingerprint has no Git runner. The asymmetry only costs
	// a cold start: a tracked file under one of these names is hashed here but
	// left to the checkout, so editing it in the main worktree rebuilds slots
	// that would have been reusable. Leaving the untracked ones out instead
	// would hand out slots carrying stale local rules.
	// The setting itself is intentionally not hashed: when no default file
	// exists, toggling it does not change the materialized worktree.
	defaults, err := defaultIncludeCandidates(string(repo.MainPath), c)
	if err != nil {
		return "", err
	}
	for _, rel := range defaults {
		seenIncludes[rel] = true
		if err := fingerprintPath(h, string(repo.MainPath), filepath.Join(string(repo.MainPath), rel)); err != nil {
			return "", err
		}
	}
	for _, pattern := range patterns {
		clean := filepath.Clean(pattern)
		if filepath.IsAbs(pattern) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
			return "", fmt.Errorf("unsafe .worktreeinclude pattern %q", pattern)
		}
		matches, err := safeGlob(string(repo.MainPath), pattern)
		if err != nil {
			return "", err
		}
		for _, match := range matches {
			rel, err := filepath.Rel(string(repo.MainPath), match)
			if err != nil {
				return "", err
			}
			rel, err = safeRelative(rel)
			if err != nil {
				return "", err
			}
			if seenIncludes[rel] {
				continue
			}
			seenIncludes[rel] = true
			if err := fingerprintPath(h, string(repo.MainPath), match); err != nil {
				return "", err
			}
		}
	}
	workspaceRoot, err := repositoryWorkspaceRoot(repo)
	if err != nil {
		return "", err
	}
	rules := c.Workspaces[workspaceRoot]
	_, _ = fmt.Fprintf(h, "workspace-root=%s\ncopy-rules=%q\nlink-rules=%q\n", workspaceRoot, rules.Copy, rules.Link)
	copyNames := append([]string{"AGENTS.md", "AGENTS.local.md", "CLAUDE.md", "CLAUDE.local.md"}, rules.Copy...)
	seenCopies := map[string]bool{}
	for _, name := range copyNames {
		clean, err := safeRelative(name)
		if err != nil {
			return "", err
		}
		if seenCopies[clean] {
			continue
		}
		seenCopies[clean] = true
		path := filepath.Join(workspaceRoot, clean)
		if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			return "", err
		}
		if err := fingerprintPath(h, workspaceRoot, path); err != nil {
			return "", err
		}
	}
	for _, name := range rules.Link {
		clean, err := safeRelative(name)
		if err != nil {
			return "", err
		}
		path := filepath.Join(workspaceRoot, clean)
		if err := domain.ValidatePhysicalPath(path, false); err != nil {
			return "", err
		}
		info, err := os.Lstat(path)
		if err != nil {
			return "", err
		}
		_, _ = fmt.Fprintf(h, "workspace-link=%s:%s\n", clean, info.Mode())
	}
	if o, ok := c.Repositories[string(repo.MainPath)]; ok {
		_, _ = fmt.Fprint(h, o.Prepare.Command, o.Prepare.Version)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func repositoryWorkspaceRoot(repo discovery.Repository) (string, error) {
	if repo.RelativePath == "" || filepath.Clean(repo.RelativePath) == "." {
		return string(repo.MainPath), nil
	}
	rel, err := safeRelative(repo.RelativePath)
	if err != nil {
		return "", err
	}
	root := string(repo.MainPath)
	for range strings.Split(rel, string(filepath.Separator)) {
		root = filepath.Dir(root)
	}
	return root, nil
}

func fingerprintPath(h hash.Hash, root, path string) error {
	absoluteRoot, err := filepath.Abs(filepath.Clean(root))
	if err != nil {
		return err
	}
	absolutePath, err := filepath.Abs(filepath.Clean(path))
	if err != nil {
		return err
	}
	rel, err := filepath.Rel(absoluteRoot, absolutePath)
	if err != nil {
		return err
	}
	rel, err = safeRelative(rel)
	if err != nil {
		return err
	}
	rootHandle, err := OpenPhysicalRoot(absoluteRoot)
	if err != nil {
		return err
	}
	defer func() { _ = rootHandle.Close() }()
	return fingerprintRootPath(h, rootHandle, rel, rel)
}

func fingerprintRootPath(h hash.Hash, root *os.Root, relative, display string) error {
	info, err := domain.PhysicalPathInfo(root, relative)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return errors.New("include symlinks are not followed")
	}
	_, _ = fmt.Fprintf(h, "path=%s mode=%s size=%d\n", display, info.Mode(), info.Size())
	if info.IsDir() {
		directory, err := root.OpenFile(relative, os.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
		if err != nil {
			return err
		}
		openedInfo, statErr := directory.Stat()
		if statErr != nil || !openedInfo.IsDir() || !os.SameFile(info, openedInfo) {
			_ = directory.Close()
			return fmt.Errorf("fingerprint directory %s changed while opening", display)
		}
		entries, readErr := directory.Readdirnames(-1)
		closeErr := directory.Close()
		if readErr != nil {
			return readErr
		}
		if closeErr != nil {
			return closeErr
		}
		sort.Strings(entries)
		for _, name := range entries {
			child := filepath.Join(relative, name)
			childDisplay := filepath.Join(display, name)
			if err := fingerprintRootPath(h, root, child, childDisplay); err != nil {
				return err
			}
		}
		return nil
	}
	file, err := root.OpenFile(relative, os.O_RDONLY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	openedInfo, err := file.Stat()
	if err != nil || !openedInfo.Mode().IsRegular() || !os.SameFile(info, openedInfo) {
		_ = file.Close()
		return fmt.Errorf("fingerprint file %s changed while opening", display)
	}
	_, copyErr := io.Copy(h, file)
	closeErr := file.Close()
	if copyErr != nil {
		return copyErr
	}
	return closeErr
}

func MaterializeRoot(source, target string, rules config.Workspace) error {
	var err error
	source, err = filepath.Abs(filepath.Clean(source))
	if err != nil {
		return err
	}
	target, err = filepath.Abs(filepath.Clean(target))
	if err != nil {
		return err
	}
	destinationRoot, err := domain.EnsurePhysicalDirectoryRoot(target, 0o700)
	if err != nil {
		return fmt.Errorf("workspace target is not physical: %w", err)
	}
	defer func() { _ = destinationRoot.Close() }()
	return MaterializeRootAt(source, destinationRoot, rules)
}

// MaterializeRootAt materializes workspace-level copy/link rules into an
// already pinned destination namespace. It is used by the daemon for
// multi-repository slots after their slot root has been opened from the
// manager-held wx root descriptor.
func MaterializeRootAt(source string, destinationRoot *os.Root, rules config.Workspace) error {
	if destinationRoot == nil {
		return errors.New("workspace destination root is nil")
	}
	var err error
	source, err = filepath.Abs(filepath.Clean(source))
	if err != nil {
		return err
	}
	if err := domain.ValidatePhysicalPath(source, false); err != nil {
		return fmt.Errorf("workspace source is not physical: %w", err)
	}
	sourceRoot, err := OpenPhysicalRoot(source)
	if err != nil {
		return err
	}
	defer func() { _ = sourceRoot.Close() }()
	copyNames := append([]string{"AGENTS.md", "AGENTS.local.md", "CLAUDE.md", "CLAUDE.local.md"}, rules.Copy...)
	if err := validateRuleConflicts(copyNames, rules.Link); err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, rel := range copyNames {
		clean, err := safeRelative(rel)
		if err != nil {
			return err
		}
		if seen[clean] {
			continue
		}
		seen[clean] = true
		if _, err := domain.PhysicalPathInfo(sourceRoot, clean); errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			return err
		}
		if err := copyPathFromOwnedRoot(sourceRoot, clean, destinationRoot, clean); err != nil {
			return fmt.Errorf("copy workspace root path %s: %w", clean, err)
		}
	}
	for _, rel := range rules.Link {
		clean, err := safeRelative(rel)
		if err != nil {
			return err
		}
		src := filepath.Join(source, clean)
		if _, err := domain.PhysicalPathInfo(sourceRoot, clean); err != nil {
			return fmt.Errorf("link workspace root path %s: %w", clean, err)
		}
		if err := domain.ValidatePhysicalPath(src, false); err != nil {
			return fmt.Errorf("workspace link source %s is not physical: %w", clean, err)
		}
		if err := ensureRootDirectory(destinationRoot, filepath.Dir(clean)); err != nil {
			return err
		}
		if info, err := destinationRoot.Lstat(clean); err == nil {
			if info.Mode()&os.ModeSymlink != 0 {
				existing, readErr := destinationRoot.Readlink(clean)
				if readErr == nil && existing == src {
					continue
				}
			}
			return fmt.Errorf("workspace root link collision %s", clean)
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err := destinationRoot.Symlink(src, clean); err != nil {
			return err
		}
	}
	return nil
}

func safeRelative(path string) (string, error) {
	clean := filepath.Clean(path)
	if path == "" || filepath.IsAbs(path) || clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("unsafe workspace root path %q", path)
	}
	return clean, nil
}
