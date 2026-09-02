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
	"os/exec"
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
	OwnedRoot        *os.Root
	RootPath         string
	RequireOwnedRoot bool
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
	prepareSlotStates, prepareRepositoryStates := preparationOwnershipStates(phase)
	root, err := config.ExpandHome(p.Config.Storage.WorktreeRoot)
	if err != nil {
		return err
	}
	target = filepath.Clean(target)
	if !domain.IsWithin(root, target) {
		return fmt.Errorf("target %s is outside wx worktree root", target)
	}
	if p.RequireOwnedRoot && p.OwnedRoot == nil && filepath.Clean(p.RootPath) == filepath.Clean(root) {
		return errors.New("wx worktree root descriptor is unavailable")
	}
	if !p.usesPinnedRoot(root) {
		if err := ensurePhysicalRoot(root); err != nil {
			return err
		}
	}
	ownedRoot, relativeTarget, closeOwnedRoot, err := p.openOwnedRoot(root, target)
	if err != nil {
		return fmt.Errorf("open wx worktree root: %w", err)
	}
	defer closeOwnedRoot()
	if err := ownedRoot.MkdirAll(filepath.Dir(relativeTarget), 0o700); err != nil {
		return fmt.Errorf("create worktree parent safely: %w", err)
	}
	return p.Git.WithCommonDirLock(string(repo.CommonDir), func() error {
		// Re-open the descriptor after taking the common-directory lock and
		// repeat the physical/ownership checks. A pre-lock check alone would
		// allow a path replacement between validation and the Git operation.
		lockedRoot, lockedRelativeTarget, closeLockedRoot, err := p.openOwnedRoot(root, target)
		if err != nil {
			return fmt.Errorf("revalidate wx worktree root: %w", err)
		}
		defer closeLockedRoot()
		if err := lockedRoot.MkdirAll(filepath.Dir(lockedRelativeTarget), 0o700); err != nil {
			return fmt.Errorf("create worktree parent safely: %w", err)
		}
		if _, err := lockedRoot.Lstat(lockedRelativeTarget); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		existingWorktree, err := p.existingTargetState(ctx, repo, target, oid, slotID, phase, root, lockedRoot, lockedRelativeTarget)
		if err != nil {
			return err
		}
		if err := p.validateStateOwnership(ctx, repo, target, slotID, prepareSlotStates, prepareRepositoryStates); err != nil {
			return fmt.Errorf("wx worktree ownership changed before marker: %w", err)
		}
		var markerErr error
		if p.usesPinnedRoot(root) {
			markerErr = EnsureOwnershipMarkerAt(lockedRoot, root, target, slotID, string(repo.CommonDir))
		} else {
			markerErr = EnsureOwnershipMarker(root, target, slotID, string(repo.CommonDir))
		}
		if err := markerErr; err != nil {
			return fmt.Errorf("prepare wx ownership marker: %w", err)
		}
		if !existingWorktree {
			if err := domain.ValidatePhysicalPath(filepath.Dir(target), false); err != nil {
				return fmt.Errorf("worktree parent contains a symlink: %w", err)
			}
			if _, err := lockedRoot.Lstat(filepath.Dir(lockedRelativeTarget)); err != nil {
				return fmt.Errorf("worktree parent ownership changed: %w", err)
			}
			if err := p.validateStateOwnership(ctx, repo, target, slotID, prepareSlotStates, prepareRepositoryStates); err != nil {
				return fmt.Errorf("wx worktree ownership changed before creation: %w", err)
			}
			if err := p.addWorktree(ctx, repo, lockedRoot, target, lockedRelativeTarget, oid); err != nil {
				return err
			}
			if _, err := lockedRoot.Lstat(lockedRelativeTarget); err != nil {
				return fmt.Errorf("%w: prepared worktree escaped ownership root: %v", state.ErrOwnership, err)
			}
		}
		cleanup := !existingWorktree
		ownedAfterLock := false
		defer func() {
			if cleanup && ownedAfterLock {
				// A failed preparation is removable only while ownership can still
				// be proved. If a command or concurrent filesystem change invalidated
				// that proof, leave the locked target and marker for quarantine/reconcile.
				if err := p.validateExistingWorktreeOwnedForPhase(context.Background(), repo, target, oid, slotID, phase); err != nil {
					return
				}
				if _, err := p.runWorktreeAdmin(context.Background(), repo, lockedRoot, lockedRelativeTarget, target, "unlock"); err != nil {
					return
				}
				if _, err := p.runWorktreeAdmin(context.Background(), repo, lockedRoot, lockedRelativeTarget, target, "remove", "--force"); err != nil {
					return
				}
				if p.usesPinnedRoot(root) {
					_ = removeOwnershipMarkerAt(lockedRoot, root, target)
				} else {
					_ = removeOwnershipMarker(root, target)
				}
			}
		}()
		if existingWorktree {
			if _, err := p.runWorktreeAdmin(ctx, repo, lockedRoot, lockedRelativeTarget, target, "unlock"); err != nil {
				return fmt.Errorf("unlock existing wx worktree: %w", err)
			}
		}
		lockState := "PREPARING"
		if phase == preparePhaseRestore {
			lockState = "RESTORING"
		}
		if _, err := p.runWorktreeAdmin(ctx, repo, lockedRoot, lockedRelativeTarget, target, "lock", "--reason", "wx:"+slotID+":"+lockState); err != nil {
			return err
		}
		// This validation is deliberately after the new lock is acquired. It
		// proves that the marker, physical path, Git registration, OID, and
		// lock reason still describe the same slot before any file operation.
		if err := p.validateExistingWorktreeOwnedForPhase(ctx, repo, target, oid, slotID, phase); err != nil {
			return fmt.Errorf("wx worktree ownership changed after lock: %w", err)
		}
		ownedAfterLock = true
		// Revalidate the durable owner immediately before each operation that
		// writes into or reuses the worktree. A common-directory lock protects
		// Git metadata, while this read-only state proof protects the slot/path
		// association and state machine independently.
		if err := p.validateExistingWorktreeOwnedForPhase(ctx, repo, target, oid, slotID, phase); err != nil {
			return fmt.Errorf("wx worktree ownership changed before includes: %w", err)
		}
		if err := p.copyIncludesAt(repo, target, lockedRoot, lockedRelativeTarget); err != nil {
			return err
		}
		if err := p.validateExistingWorktreeOwnedForPhase(ctx, repo, target, oid, slotID, phase); err != nil {
			return fmt.Errorf("wx worktree ownership changed before links: %w", err)
		}
		if err := p.createLinksAt(ctx, repo, target, lockedRoot, lockedRelativeTarget); err != nil {
			return err
		}
		if phase == preparePhaseCreate {
			if err := p.validateExistingWorktreeOwnedForPhase(ctx, repo, target, oid, slotID, phase); err != nil {
				return fmt.Errorf("wx worktree ownership changed before prepare command: %w", err)
			}
			if err := p.runPrepare(ctx, repo, target); err != nil {
				return err
			}
		}
		if phase == preparePhaseCreate {
			if err := p.validateTrackedClean(ctx, target); err != nil {
				return err
			}
		}
		targetRoot, _, err := domain.OpenDirectoryAt(lockedRoot, lockedRelativeTarget)
		if err != nil {
			return fmt.Errorf("open prepared worktree: %w", err)
		}
		defer func() { _ = targetRoot.Close() }()
		head, err := p.runGitInDirectory(ctx, target, targetRoot, "rev-parse", "HEAD")
		if err != nil {
			return err
		}
		if strings.TrimSpace(head.Stdout) != oid {
			return fmt.Errorf("prepared HEAD differs from requested OID")
		}
		if _, err := p.runGitInDirectory(ctx, target, targetRoot, "symbolic-ref", "-q", "HEAD"); err == nil {
			return errors.New("prepared worktree is not detached")
		}
		if phase == preparePhaseRestore {
			// Keep the RESTORING lock until archive.Manager has restored the
			// snapshot tree/index and run the resume-phase command.
			cleanup = false
			return nil
		}
		if _, err = p.runWorktreeAdmin(ctx, repo, lockedRoot, lockedRelativeTarget, target, "unlock"); err != nil {
			return err
		}
		_, err = p.runWorktreeAdmin(ctx, repo, lockedRoot, lockedRelativeTarget, target, "lock", "--reason", "wx:"+slotID+":READY")
		if err != nil {
			return err
		}
		if err := p.validateExistingWorktreeOwnedForPhase(ctx, repo, target, oid, slotID, phase); err != nil {
			return fmt.Errorf("wx worktree ownership changed before READY: %w", err)
		}
		cleanup = false
		return nil
	})
}

func (p *Preparer) usesPinnedRoot(root string) bool {
	return (p.OwnedRoot != nil || p.RequireOwnedRoot) && filepath.Clean(p.RootPath) == filepath.Clean(root)
}

func (p *Preparer) openOwnedRoot(root, target string) (*os.Root, string, func(), error) {
	if p.usesPinnedRoot(root) {
		if p.OwnedRoot == nil {
			return nil, "", func() {}, errors.New("wx worktree root descriptor is unavailable")
		}
		relative, err := filepath.Rel(filepath.Clean(root), filepath.Clean(target))
		if err != nil {
			return nil, "", func() {}, err
		}
		return p.OwnedRoot, relative, func() {}, nil
	}
	owned, relative, err := domain.OpenOwnedRoot(root, target)
	if err != nil {
		return nil, "", func() {}, err
	}
	return owned, relative, func() { _ = owned.Close() }, nil
}

// addWorktree reserves the target namespace by creating/opening its final
// directory through the pinned parent descriptor, then runs Git with that
// target descriptor as its current directory and "." as the target. A
// replacement of any lexical root/ancestor/target after this point cannot
// redirect Git's checkout or its worktree registration outside the owned inode.
func (p *Preparer) addWorktree(ctx context.Context, repo discovery.Repository, owner *os.Root, target, relativeTarget, oid string) error {
	if !p.usesPinnedRoot(p.RootPath) {
		_, err := p.Git.Run(ctx, string(repo.MainPath), "worktree", "add", "--detach", target, oid)
		return err
	}
	parentRelative := filepath.Dir(relativeTarget)
	parent, _, err := domain.OpenDirectoryAt(owner, parentRelative)
	if err != nil {
		return fmt.Errorf("open worktree target namespace: %w", err)
	}
	defer func() { _ = parent.Close() }()
	if info, statErr := parent.Stat(); statErr != nil || !info.IsDir() {
		if statErr != nil {
			return fmt.Errorf("validate worktree target namespace: %w", statErr)
		}
		return errors.New("worktree target namespace is not a directory")
	}
	name := filepath.Base(relativeTarget)
	if name == "." || name == string(filepath.Separator) || name == "" {
		return errors.New("worktree target has no relative leaf")
	}
	// Reserve the final leaf with mkdirat. Using owner.Mkdir(relativeTarget)
	// here would reopen the parent pathname after the descriptor barrier.
	if err := unix.Mkdirat(int(parent.Fd()), name, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return fmt.Errorf("reserve worktree target namespace: %w", err)
	}
	targetDirectory, targetIdentity, err := domain.OpenDirectoryAt(owner, relativeTarget)
	if err != nil {
		return fmt.Errorf("open reserved worktree target: %w", err)
	}
	defer func() { _ = targetDirectory.Close() }()
	// --git-dir identifies the source repository without changing cwd.  -C
	// would make Git resolve the target relative to repo.MainPath, defeating
	// the descriptor-bound target namespace established above.
	_, err = p.Git.RunAt(ctx, targetDirectory, nil, nil, "--git-dir", string(repo.CommonDir), "worktree", "add", "--detach", ".", oid)
	if err == nil {
		return nil
	}
	// Git may have updated its common-directory registration before reporting
	// an error (or the child may have been interrupted). If the descriptor
	// namespace or registration cannot be proved clean, preserve the target for
	// the daemon's ownership quarantine instead of turning an ambiguous add into
	// a generic FAILED slot.
	entries, readErr := targetDirectory.Readdirnames(-1)
	if readErr != nil {
		return fmt.Errorf("%w: inspect git worktree add target: %v", state.ErrOwnership, readErr)
	}
	if len(entries) > 0 {
		return fmt.Errorf("%w: git worktree add outcome is uncertain: %v", state.ErrOwnership, err)
	}
	_, _, found, inspectErr := RegisteredWorktreeLockStatusAt(ctx, p.Git, string(repo.MainPath), owner, p.RootPath, relativeTarget, targetIdentity)
	if inspectErr != nil || found {
		if inspectErr == nil {
			inspectErr = errors.New("Git registration remains after failed worktree add")
		}
		return fmt.Errorf("%w: git worktree add registration is uncertain: %v", state.ErrOwnership, inspectErr)
	}
	var gitErr *gitx.Error
	if errors.As(err, &gitErr) && gitErr.Result.ExitCode < 0 {
		return fmt.Errorf("%w: git worktree add process outcome is uncertain: %v", state.ErrOwnership, err)
	}
	return err
}

func (p *Preparer) runWorktreeAdmin(ctx context.Context, repo discovery.Repository, owner *os.Root, relativeTarget, target string, args ...string) (gitx.Result, error) {
	if !p.usesPinnedRoot(p.RootPath) {
		command := append([]string{"worktree"}, args...)
		command = append(command, target)
		return p.Git.Run(ctx, string(repo.MainPath), command...)
	}
	targetDirectory, _, err := domain.OpenDirectoryAt(owner, relativeTarget)
	if err != nil {
		return gitx.Result{}, fmt.Errorf("open worktree target namespace: %w", err)
	}
	defer func() { _ = targetDirectory.Close() }()
	// Keep cwd at the descriptor-bound target itself. Supplying --git-dir selects
	// the source repository while "." identifies the reserved worktree inode.
	command := append([]string{"--git-dir", string(repo.CommonDir), "worktree"}, args...)
	command = append(command, ".")
	return p.Git.RunAt(ctx, targetDirectory, nil, nil, command...)
}

// PrepareResume runs only the resume phase of a repository prepare. The
// caller invokes it after restoring the snapshot tree and index, so commands
// can inspect the recovered files and their staged state.
func (p *Preparer) PrepareResume(ctx context.Context, repo discovery.Repository, target, oid, slotID string) error {
	if err := p.validateExistingWorktreeOwnedForPhase(ctx, repo, target, oid, slotID, preparePhaseRestore); err != nil {
		return fmt.Errorf("validate restoring worktree before resume prepare: %w", err)
	}
	if err := p.runPrepare(ctx, repo, target); err != nil {
		return err
	}
	if err := p.validateExistingWorktreeOwnedForPhase(ctx, repo, target, oid, slotID, preparePhaseRestore); err != nil {
		return fmt.Errorf("wx worktree ownership changed during resume prepare: %w", err)
	}
	return nil
}

// FinishRestore transitions a successfully restored repository from its
// RESTORING lock to the READY lock. The final ownership check occurs while the
// restore lock is still held and again after the READY lock is acquired.
func (p *Preparer) FinishRestore(ctx context.Context, repo discovery.Repository, target, oid, slotID string) error {
	if err := p.validateExistingWorktreeOwnedForPhase(ctx, repo, target, oid, slotID, preparePhaseRestore); err != nil {
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
	if _, err := p.runWorktreeAdmin(ctx, repo, owner, relativeTarget, target, "unlock"); err != nil {
		return fmt.Errorf("unlock restored worktree: %w", err)
	}
	if _, err := p.runWorktreeAdmin(ctx, repo, owner, relativeTarget, target, "lock", "--reason", "wx:"+slotID+":READY"); err != nil {
		return err
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
		markerRelative, markerErr := ownershipMarkerRelative(root, target)
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

func ensurePhysicalRoot(path string) error {
	ownedRoot, err := domain.EnsurePhysicalDirectoryRoot(path, 0o700)
	if err != nil {
		return fmt.Errorf("open wx worktree root safely: %w", err)
	}
	defer func() { _ = ownedRoot.Close() }()
	if err := ownedRoot.Chmod(".", 0o700); err != nil {
		return err
	}
	return nil
}

func (p *Preparer) validateExistingWorktree(ctx context.Context, repo discovery.Repository, target, oid string) error {
	return p.validateExistingWorktreeOwned(ctx, repo, target, oid, "")
}

func (p *Preparer) validateExistingWorktreeOwned(ctx context.Context, repo discovery.Repository, target, oid, slotID string) error {
	return p.validateExistingWorktreeOwnedForPhase(ctx, repo, target, oid, slotID, preparePhaseCreate)
}

func (p *Preparer) validateExistingWorktreeOwnedForPhase(ctx context.Context, repo discovery.Repository, target, oid, slotID string, phase preparePhase) error {
	slotStates, repositoryStates := preparationOwnershipStates(phase)
	err := p.validateExistingWorktreeOwnedForStates(ctx, repo, target, oid, slotID, slotStates, repositoryStates)
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, state.ErrOwnership) {
		return err
	}
	return fmt.Errorf("%w: %v", state.ErrOwnership, err)
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
		return fmt.Errorf("%w: validate replayed slot worktree: %v", state.ErrOwnership, err)
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
	if p.usesPinnedRoot(root) {
		if err := ValidateOwnershipMarkerAt(owner, root, target, slotID, string(repo.CommonDir)); err != nil {
			return err
		}
	} else if err := ValidateOwnershipMarker(root, target, slotID, string(repo.CommonDir)); err != nil {
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
	common, err := p.runGitInDirectory(ctx, target, targetRoot, "rev-parse", "--path-format=absolute", "--git-common-dir")
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
	head, err := p.runGitInDirectory(ctx, target, targetRoot, "rev-parse", "HEAD")
	detached := false
	if err == nil {
		_, detachedErr := p.runGitInDirectory(ctx, target, targetRoot, "symbolic-ref", "-q", "HEAD")
		detached = detachedErr != nil
	}
	if err != nil || strings.TrimSpace(head.Stdout) != oid || !detached {
		return errors.New("HEAD is not the expected detached commit")
	}
	if p.usesPinnedRoot(root) {
		if err := ValidateRegisteredWorktreeAt(ctx, p.Git, string(repo.MainPath), owner, root, relativeTarget, targetIdentity, slotID, slotID != ""); err != nil {
			return err
		}
	} else if err := ValidateRegisteredWorktree(ctx, p.Git, string(repo.MainPath), target, slotID, slotID != ""); err != nil {
		return err
	}
	if slotID == "" {
		return nil
	}
	if err := p.validateStateOwnership(ctx, repo, target, slotID, slotStates, repositoryStates); err != nil {
		return err
	}
	return nil
}

func (p *Preparer) runGitInDirectory(ctx context.Context, target string, directory *os.File, args ...string) (gitx.Result, error) {
	if p.usesPinnedRoot(p.RootPath) {
		return p.Git.RunAt(ctx, directory, nil, nil, args...)
	}
	return p.Git.Run(ctx, target, args...)
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
	slotID, err := p.validateRemovalOwnership(root, target, common)
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
	slotID, err := p.validateRemovalOwnership(root, target, common)
	if err != nil {
		return err
	}
	if p.usesPinnedRoot(root) {
		owner, relativeTarget, closeOwner, openErr := p.openOwnedRoot(root, target)
		if openErr != nil {
			return openErr
		}
		defer closeOwner()
		_, targetIdentity, openErr := domain.OpenDirectoryAt(owner, relativeTarget)
		if openErr != nil {
			return openErr
		}
		if err := ValidateRegisteredWorktreeAt(ctx, p.Git, string(repo.MainPath), owner, root, relativeTarget, targetIdentity, slotID, true); err != nil {
			return err
		}
	} else if err := ValidateRegisteredWorktree(ctx, p.Git, string(repo.MainPath), target, slotID, true); err != nil {
		return err
	}
	return p.validateStateOwnership(ctx, repo, target, slotID, allOwnershipSlotStates, allOwnershipRepositoryStates)
}

func (p *Preparer) validateRemovalOwnership(root, target, common string) (string, error) {
	if p.usesPinnedRoot(root) {
		owner, _, closeOwner, err := p.openOwnedRoot(root, target)
		if err != nil {
			return "", err
		}
		defer closeOwner()
		return ValidateRemovalOwnershipAt(owner, root, target, common)
	}
	return ValidateRemovalOwnership(root, target, common)
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
	if slotID == "" {
		return nil
	}
	if p.Ownership == nil {
		return fmt.Errorf("%w: state-backed worktree ownership validator is required", state.ErrOwnership)
	}
	_, err := p.Ownership.ValidateWorktreeOwnership(ctx, state.WorktreeOwnershipRequest{
		SlotID:                  slotID,
		RepositoryID:            string(repo.ID),
		WorkspaceID:             "",
		SlotPath:                p.SlotPath,
		WorktreePath:            target,
		CommonDir:               string(repo.CommonDir),
		AllowedSlotStates:       slotStates,
		AllowedRepositoryStates: repositoryStates,
	})
	return err
}

func (p *Preparer) validateTrackedClean(ctx context.Context, target string) error {
	var status gitx.Result
	var err error
	if root, rootErr := config.ExpandHome(p.Config.Storage.WorktreeRoot); rootErr == nil && p.usesPinnedRoot(root) {
		owner, relative, closeOwner, openErr := p.openOwnedRoot(root, target)
		if openErr != nil {
			return openErr
		}
		defer closeOwner()
		directory, _, openErr := domain.OpenDirectoryAt(owner, relative)
		if openErr != nil {
			return openErr
		}
		defer func() { _ = directory.Close() }()
		status, err = p.Git.RunAt(ctx, directory, nil, nil, "status", "--porcelain=v1", "--untracked-files=no")
	} else {
		status, err = p.Git.Run(ctx, target, "status", "--porcelain=v1", "--untracked-files=no")
	}
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
	if err == nil && p.usesPinnedRoot(root) {
		owner, relativeTarget, closeOwner, openErr := p.openOwnedRoot(root, filepath.Clean(target))
		if openErr != nil {
			return openErr
		}
		defer closeOwner()
		return p.copyIncludesAt(repo, target, owner, relativeTarget)
	}
	return p.copyIncludesAt(repo, target, nil, "")
}

// copyIncludesAt is the descriptor-bound include materializer. When owner is
// supplied, destinationRoot is opened from that already pinned namespace and
// every write is relative to it; the lexical target pathname is never used for
// a destination syscall.
func (p *Preparer) copyIncludesAt(repo discovery.Repository, target string, owner *os.Root, relativeTarget string) error {
	patterns, err := readPhysicalPatterns(string(repo.MainPath), ".worktreeinclude")
	if err != nil {
		return err
	}
	var destinationRoot *os.Root
	if owner != nil {
		destinationRoot, err = domain.OpenRootAt(owner, relativeTarget)
		if err != nil {
			return fmt.Errorf("open include destination: %w", err)
		}
		defer func() { _ = destinationRoot.Close() }()
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
			var copyErr error
			if destinationRoot != nil {
				sourceRoot, sourceErr := OpenPhysicalRoot(string(repo.MainPath))
				if sourceErr != nil {
					return sourceErr
				}
				copyErr = copyPathFromOwnedRoot(sourceRoot, rel, destinationRoot, rel)
				_ = sourceRoot.Close()
			} else {
				copyErr = copyPathFromRoots(string(repo.MainPath), rel, target, rel)
			}
			if copyErr != nil {
				return copyErr
			}
		}
	}
	return nil
}

func (p *Preparer) createLinks(ctx context.Context, repo discovery.Repository, target string) error {
	root, err := config.ExpandHome(p.Config.Storage.WorktreeRoot)
	if err == nil && p.usesPinnedRoot(root) {
		owner, relativeTarget, closeOwner, openErr := p.openOwnedRoot(root, filepath.Clean(target))
		if openErr != nil {
			return openErr
		}
		defer closeOwner()
		return p.createLinksAt(ctx, repo, target, owner, relativeTarget)
	}
	return p.createLinksAt(ctx, repo, target, nil, "")
}

// createLinksAt mirrors createLinks while keeping the destination Root open
// across all checks and symlink creation. This closes the root replacement
// window between validating the worktree and writing an ignored link.
func (p *Preparer) createLinksAt(ctx context.Context, repo discovery.Repository, target string, owner *os.Root, relativeTarget string) error {
	patterns, err := readPhysicalPatterns(string(repo.MainPath), ".worktreelink")
	if err != nil {
		return err
	}
	if err := validateRuleConflicts(nil, patterns); err != nil {
		return err
	}
	var pinnedDestination *os.Root
	if owner != nil {
		pinnedDestination, err = domain.OpenRootAt(owner, relativeTarget)
		if err != nil {
			return fmt.Errorf("open link destination: %w", err)
		}
		defer func() { _ = pinnedDestination.Close() }()
	}
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
		destinationRoot := pinnedDestination
		closeDestination := func() {}
		if destinationRoot == nil {
			if err := domain.ValidatePhysicalPath(target, false); err != nil {
				return fmt.Errorf(".worktreelink target is not physical: %w", err)
			}
			destinationRoot, err = OpenPhysicalRoot(target)
			if err != nil {
				return err
			}
			closeDestination = func() { _ = destinationRoot.Close() }
		}
		destinationRelative, err := safeRelative(clean)
		if err != nil {
			closeDestination()
			return err
		}
		if err := ensureRootDirectory(destinationRoot, filepath.Dir(destinationRelative)); err != nil {
			closeDestination()
			return err
		}
		if info, err := destinationRoot.Lstat(destinationRelative); err == nil {
			if info.Mode()&os.ModeSymlink != 0 {
				existing, readErr := destinationRoot.Readlink(destinationRelative)
				if readErr == nil && existing == source {
					closeDestination()
					continue
				}
			}
			closeDestination()
			return fmt.Errorf(".worktreelink target collision %s", rel)
		} else if !errors.Is(err, os.ErrNotExist) {
			closeDestination()
			return err
		}
		if err := destinationRoot.Symlink(source, destinationRelative); err != nil {
			closeDestination()
			return err
		}
		closeDestination()
	}
	return nil
}

func (p *Preparer) runPrepare(ctx context.Context, repo discovery.Repository, target string) error {
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
	if root, rootErr := config.ExpandHome(p.Config.Storage.WorktreeRoot); rootErr == nil && p.usesPinnedRoot(root) {
		owner, relative, closeOwner, err := p.openOwnedRoot(root, target)
		if err != nil {
			return fmt.Errorf("open prepare command directory: %w", err)
		}
		defer closeOwner()
		directory, _, err := domain.OpenDirectoryAt(owner, relative)
		if err != nil {
			return fmt.Errorf("open prepare command directory: %w", err)
		}
		defer func() { _ = directory.Close() }()
		cmd, err := fdexec.Start(cctx, p.Git.FDHelper, directory, os.Environ(), override.Prepare.Command...)
		if err != nil {
			return err
		}
		cmd.Stdout = io.Discard
		cmd.Stderr = io.Discard
		return cmd.Run()
	}
	cmd := exec.CommandContext(cctx, override.Prepare.Command[0], override.Prepare.Command[1:]...)
	cmd.Dir = target
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	return cmd.Run()
}

func Fingerprint(generation int, oid string, repo discovery.Repository, c config.Config) (string, error) {
	if err := domain.ValidatePhysicalPath(string(repo.MainPath), false); err != nil {
		return "", err
	}
	h := sha256.New()
	_, _ = fmt.Fprintf(h, "schema=2\ngeneration=%d\noid=%s\n", generation, oid)
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
	info, err := rootPhysicalInfo(root, relative)
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
		if _, err := rootPhysicalInfo(sourceRoot, clean); errors.Is(err, os.ErrNotExist) {
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
		if _, err := rootPhysicalInfo(sourceRoot, clean); err != nil {
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

func copyPath(src, dst string) error {
	if err := domain.ValidatePhysicalPath(src, false); err != nil {
		return err
	}
	if err := domain.EnsurePhysicalDirectory(filepath.Dir(dst), 0o700); err != nil {
		return err
	}
	return copyPathFromRoots(filepath.Dir(src), filepath.Base(src), filepath.Dir(dst), filepath.Base(dst))
}
