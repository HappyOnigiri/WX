package workspace

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/HappyOnigiri/WX/internal/config"
	"github.com/HappyOnigiri/WX/internal/discovery"
	"github.com/HappyOnigiri/WX/internal/domain"
	"github.com/HappyOnigiri/WX/internal/state"
)

// TestPrepareRejectsOwnershipChangesAtEachRevalidationCheckpoint drives an
// ownership-changed failure through every one of prepareLocked's repeated
// revalidation checkpoints (before includes, before links, before/during the
// prepare command, before/during the tracked-status check, and before READY)
// by varying which call to the ownership validator fails. A successful CREATE
// phase invokes the validator exactly 7 times, so failAt=1..7 sweeps every
// checkpoint that calls it.
func TestPrepareRejectsOwnershipChangesAtEachRevalidationCheckpoint(t *testing.T) {
	for failAt := 1; failAt <= 7; failAt++ {
		t.Run(fmt.Sprintf("failAt=%d", failAt), func(t *testing.T) {
			_, repo, preparer, head, target := prepareEdgesFixture(t)
			root := preparer.Config.Storage.WorktreeRoot
			if err := os.MkdirAll(root, 0o700); err != nil {
				t.Fatal(err)
			}
			preparer.Ownership = &edgeCountingOwnershipValidator{failAt: failAt}
			if err := preparer.Prepare(context.Background(), repo, target, head, "slot"); err == nil {
				t.Fatalf("prepare succeeded despite the ownership validator failing at call %d", failAt)
			}
		})
	}
}

// readyStateRejectingOwnershipValidator allows every ownership proof except
// one that asks specifically for the READY state alone, letting a test tell
// apart ValidateOwnership's broad lifecycle proof from ValidateReady's own,
// stricter READY-only proof.
type readyStateRejectingOwnershipValidator struct{}

func (readyStateRejectingOwnershipValidator) ValidateWorktreeOwnership(_ context.Context, req state.WorktreeOwnershipRequest) (state.WorktreeOwnership, error) {
	if len(req.AllowedSlotStates) == 1 && req.AllowedSlotStates[0] == "READY" {
		return state.WorktreeOwnership{}, errors.New("ready-specific state proof rejected")
	}
	return state.WorktreeOwnership{}, nil
}

// TestValidateReadyEnforcesItsOwnReadyStateProof proves that ValidateReady
// performs its own narrower state-ownership check rather than only relying on
// the broader lifecycle proof already performed by ValidateOwnership.
func TestValidateReadyEnforcesItsOwnReadyStateProof(t *testing.T) {
	ctx := context.Background()
	_, repo, preparer, head, target := prepareEdgesFixture(t)
	root := preparer.Config.Storage.WorktreeRoot
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := preparer.Prepare(ctx, repo, target, head, "slot"); err != nil {
		t.Fatal(err)
	}
	preparer.Ownership = readyStateRejectingOwnershipValidator{}
	if err := preparer.ValidateReady(ctx, repo, target, head); err == nil || err.Error() != "ready-specific state proof rejected" {
		t.Fatalf("ValidateReady accepted despite its own state proof failing: %v", err)
	}
}

// TestRunGitInWorktreeUnpinnedFastPathAndDescriptorFaults covers the
// unpinned/no-identity fast path (which skips descriptor handling entirely)
// and the descriptor-bound faults that only apply once an identity or a
// pinned root is involved.
func TestRunGitInWorktreeUnpinnedFastPathAndDescriptorFaults(t *testing.T) {
	ctx := context.Background()
	_, repo, preparer, head, target := prepareEdgesFixture(t)
	root := preparer.Config.Storage.WorktreeRoot
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := preparer.Prepare(ctx, repo, target, head, "slot"); err != nil {
		t.Fatal(err)
	}

	if _, err := preparer.RunGitInWorktree(ctx, target, "", nil, nil, "rev-parse", "HEAD"); err != nil {
		t.Fatalf("unpinned no-identity fast path: %v", err)
	}

	preparer.RootPath = root
	preparer.OwnedRoot = nil
	if _, err := preparer.RunGitInWorktree(ctx, target, "identity", nil, nil, "rev-parse", "HEAD"); err == nil {
		t.Fatal("pinned command with a missing root descriptor succeeded")
	}

	owner, _, err := domain.OpenOwnedRoot(root, root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = owner.Close() }()
	preparer.OwnedRoot = owner
	if _, err := preparer.RunGitInWorktree(ctx, target, "not-the-real-identity", nil, nil, "rev-parse", "HEAD"); !errors.Is(err, state.ErrOwnership) {
		t.Fatalf("mismatched pre-command identity error=%v", err)
	}
}

// TestWorktreeIdentityPropagatesDescriptorAndOpenFailures covers
// WorktreeIdentity's own descriptor-open failure (the configured root itself
// becomes unsearchable) and its subsequent directory-open failure (the
// target itself becomes unsearchable, which only requires search permission
// on the target rather than on any of its ancestors).
func TestWorktreeIdentityPropagatesDescriptorAndOpenFailures(t *testing.T) {
	ctx := context.Background()
	_, repo, preparer, head, target := prepareEdgesFixture(t)
	root := preparer.Config.Storage.WorktreeRoot
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := preparer.Prepare(ctx, repo, target, head, "slot"); err != nil {
		t.Fatal(err)
	}

	if err := os.Chmod(root, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := preparer.WorktreeIdentity(target); err == nil {
		_ = os.Chmod(root, 0o700)
		t.Fatal("worktree identity behind an unsearchable configured root succeeded")
	}
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}

	if err := os.Chmod(target, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(target, 0o700) })
	if _, err := preparer.WorktreeIdentity(target); err == nil {
		t.Fatal("worktree identity for an unsearchable target succeeded")
	}
}

// TestCopyIncludesAndCreateLinksPropagateDescriptorFaults covers the
// pinned-mode descriptor faults shared by copyIncludes/createLinks (a missing
// root descriptor) and by copyIncludesAt/createLinksAt (an unsearchable
// destination target), which are otherwise only reachable through the
// unpinned path in existing tests.
func TestCopyIncludesAndCreateLinksPropagateDescriptorFaults(t *testing.T) {
	ctx := context.Background()
	_, repo, preparer, head, target := prepareEdgesFixture(t)
	root := preparer.Config.Storage.WorktreeRoot
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := preparer.Prepare(ctx, repo, target, head, "slot"); err != nil {
		t.Fatal(err)
	}

	preparer.RootPath = root
	preparer.OwnedRoot = nil
	if err := preparer.copyIncludes(repo, target); err == nil {
		t.Fatal("copyIncludes with a missing pinned root descriptor succeeded")
	}
	if err := preparer.createLinks(ctx, repo, target); err == nil {
		t.Fatal("createLinks with a missing pinned root descriptor succeeded")
	}

	owner, _, err := domain.OpenOwnedRoot(root, root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = owner.Close() }()
	preparer.OwnedRoot = owner

	if err := os.Chmod(target, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(target, 0o700) })
	if err := preparer.copyIncludes(repo, target); err == nil {
		t.Fatal("pinned copyIncludes opened an unsearchable destination")
	}
	if err := preparer.createLinks(ctx, repo, target); err == nil {
		t.Fatal("pinned createLinks opened an unsearchable destination")
	}
}

// TestPinnedPrepareFailureFullyCleansUpAndRemovesOwnershipMarker exercises the
// pinned-mode branch of prepareLocked's deferred cleanup, which is otherwise
// only covered in unpinned mode elsewhere: a failing prepare command must
// still unlock and remove the reserved worktree and remove its
// descriptor-bound ownership marker.
func TestPinnedPrepareFailureFullyCleansUpAndRemovesOwnershipMarker(t *testing.T) {
	ctx := context.Background()
	_, repo, preparer, head, target := prepareEdgesFixture(t)
	root := preparer.Config.Storage.WorktreeRoot
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	owner, _, err := domain.OpenOwnedRoot(root, root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = owner.Close() }()
	preparer.OwnedRoot = owner
	preparer.RootPath = root

	cfg := preparer.Config
	cfg.Repositories = map[string]config.Repository{string(repo.MainPath): {Prepare: config.Prepare{Command: []string{"/bin/sh", "-c", "exit 1"}, Timeout: config.Duration{Duration: 5 * time.Second}}}}
	preparer.Config = cfg
	if err := preparer.Prepare(ctx, repo, target, head, "slot"); err == nil {
		t.Fatal("prepare succeeded despite a failing prepare command")
	}
	if _, _, found, err := RegisteredWorktreeLockStatusAt(ctx, preparer.Git, string(repo.MainPath), owner, root, filepath.Join("slots", "slot", "root"), "irrelevant"); err != nil || found {
		t.Fatalf("pinned cleanup left a Git registration: found=%v err=%v", found, err)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatalf("pinned cleanup left the target directory: %v", err)
	}
}

// TestMaterializeRootAtRejectsSymlinkAncestorInCopyRule covers the copy-rule
// existence check's non-ErrNotExist error branch: a copy rule reaching
// through a symlinked ancestor directory must be rejected instead of treated
// as simply "missing".
func TestMaterializeRootAtRejectsSymlinkAncestorInCopyRule(t *testing.T) {
	source := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "value"), []byte("value"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(source, "linked")); err != nil {
		t.Fatal(err)
	}
	destination := t.TempDir()
	destinationRoot, err := OpenPhysicalRoot(destination)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = destinationRoot.Close() }()
	rules := config.Workspace{Copy: []string{filepath.Join("linked", "value")}}
	if err := MaterializeRootAt(source, destinationRoot, rules); err == nil {
		t.Fatal("workspace copy rule through a symlink ancestor was accepted")
	}
}

// TestRemoveWorktreeAtRequiresAPinnedRootDescriptor covers the guard that
// rejects descriptor-bound removal outside pinned mode, which the other
// RemoveWorktreeAt tests never exercise because they always run pinned.
func TestRemoveWorktreeAtRequiresAPinnedRootDescriptor(t *testing.T) {
	_, repo, preparer, _, target := prepareEdgesFixture(t)
	root := preparer.Config.Storage.WorktreeRoot
	if err := preparer.RemoveWorktreeAt(context.Background(), repo, root, target, "identity"); !errors.Is(err, state.ErrOwnership) {
		t.Fatalf("unpinned descriptor-bound removal error=%v", err)
	}
}

// TestValidateExistingWorktreeOwnedForStatesCoversPhysicalAndGitDivergence
// exercises the ownership-marker-independent checks that
// validateExistingWorktreeOwnedForStates performs on the worktree itself: a
// removed target, a missing/unsafe .git marker, and a .git file that exists
// but no longer works as a Git pointer.
func TestValidateExistingWorktreeOwnedForStatesCoversPhysicalAndGitDivergence(t *testing.T) {
	ctx := context.Background()

	t.Run("target removed after marker survives", func(t *testing.T) {
		_, repo, preparer, head, target := prepareEdgesFixture(t)
		root := preparer.Config.Storage.WorktreeRoot
		if err := os.MkdirAll(root, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := preparer.Prepare(ctx, repo, target, head, "slot"); err != nil {
			t.Fatal(err)
		}
		if err := os.RemoveAll(target); err != nil {
			t.Fatal(err)
		}
		if err := preparer.ValidateOwnership(ctx, repo, target, head); err == nil {
			t.Fatal("ownership validation for a removed physical target succeeded")
		}
	})

	t.Run("git marker replaced by a symlink", func(t *testing.T) {
		_, repo, preparer, head, target := prepareEdgesFixture(t)
		root := preparer.Config.Storage.WorktreeRoot
		if err := os.MkdirAll(root, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := preparer.Prepare(ctx, repo, target, head, "slot"); err != nil {
			t.Fatal(err)
		}
		gitFile := filepath.Join(target, ".git")
		if err := os.Remove(gitFile); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(filepath.Join(t.TempDir(), "outside"), gitFile); err != nil {
			t.Fatal(err)
		}
		if err := preparer.ValidateOwnership(ctx, repo, target, head); err == nil || !strings.Contains(err.Error(), "missing or unsafe .git marker") {
			t.Fatalf("symlinked .git marker error=%v", err)
		}
	})

	t.Run("git marker present but unusable", func(t *testing.T) {
		_, repo, preparer, head, target := prepareEdgesFixture(t)
		root := preparer.Config.Storage.WorktreeRoot
		if err := os.MkdirAll(root, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := preparer.Prepare(ctx, repo, target, head, "slot"); err != nil {
			t.Fatal(err)
		}
		gitFile := filepath.Join(target, ".git")
		if err := os.WriteFile(gitFile, []byte("gitdir: /nonexistent/common/dir\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := preparer.ValidateOwnership(ctx, repo, target, head); err == nil || strings.Contains(err.Error(), "missing or unsafe .git marker") {
			t.Fatalf("unusable .git marker error=%v, want a Git command failure instead", err)
		}
	})

	t.Run("target becomes unsearchable after the marker validates", func(t *testing.T) {
		_, repo, preparer, head, target := prepareEdgesFixture(t)
		root := preparer.Config.Storage.WorktreeRoot
		if err := os.MkdirAll(root, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := preparer.Prepare(ctx, repo, target, head, "slot"); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(target, 0); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(target, 0o700) })
		if err := preparer.ValidateOwnership(ctx, repo, target, head); err == nil {
			t.Fatal("ownership validation opened an unsearchable target")
		}
	})

	t.Run("git common directory diverges from the recorded repository", func(t *testing.T) {
		_, repo, preparer, head, target := prepareEdgesFixture(t)
		root := preparer.Config.Storage.WorktreeRoot
		if err := os.MkdirAll(root, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := preparer.Prepare(ctx, repo, target, head, "slot"); err != nil {
			t.Fatal(err)
		}
		// Point the worktree's .git file at a second, unrelated repository so
		// Git commands keep working but report a different common directory
		// than the one recorded for this slot.
		otherRepository := t.TempDir()
		gitCommand(t, otherRepository, "init", "-b", "main")
		gitFile := filepath.Join(target, ".git")
		if err := os.WriteFile(gitFile, []byte("gitdir: "+filepath.Join(otherRepository, ".git")+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := preparer.ValidateOwnership(ctx, repo, target, head); err == nil || !strings.Contains(err.Error(), "common Git directory does not match") {
			t.Fatalf("diverged common directory error=%v", err)
		}
	})
}

// TestPrepareLockedTargetPropagatesRevalidationDescriptorFailure covers
// prepareLockedTarget's own descriptor re-open, which repeats the physical
// check after the common-directory lock is taken specifically to close a
// path-replacement race; an unsearchable configured root must fail this
// re-open even though nothing has called prepareTarget's earlier check yet.
func TestPrepareLockedTargetPropagatesRevalidationDescriptorFailure(t *testing.T) {
	_, repo, preparer, head, target := prepareEdgesFixture(t)
	root := preparer.Config.Storage.WorktreeRoot
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(root, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(root, 0o700) })
	if err := preparer.prepareLocked(context.Background(), repo, target, head, "slot", preparePhaseCreate, root); err == nil {
		t.Fatal("prepareLockedTarget opened an unsearchable configured root")
	}
}

// TestPrepareLockedTargetPropagatesParentCreationFailure covers the
// MkdirAll failure branch that is distinct from "already exists": the
// target's grandparent directory forbids writes, so creating the target's
// own parent directory fails for a real reason.
func TestPrepareLockedTargetPropagatesParentCreationFailure(t *testing.T) {
	_, repo, preparer, head, target := prepareEdgesFixture(t)
	root := preparer.Config.Storage.WorktreeRoot
	workspaceDirectory := filepath.Join(root, testWorkspaceID)
	if err := os.MkdirAll(workspaceDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(workspaceDirectory, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(workspaceDirectory, 0o700) })
	if err := preparer.prepareLocked(context.Background(), repo, target, head, "slot", preparePhaseCreate, root); err == nil {
		t.Fatal("prepareLockedTarget created a worktree parent below a read-only directory")
	}
}

// TestPrepareLockedTargetPropagatesMarkerWriteFailure covers prepareLocked's
// own markerErr branch: the target's parent directory already exists (so
// MkdirAll is a no-op) but forbids writes, so writing the ownership marker
// beside the not-yet-created target fails.
func TestPrepareLockedTargetPropagatesMarkerWriteFailure(t *testing.T) {
	_, repo, preparer, head, target := prepareEdgesFixture(t)
	root := preparer.Config.Storage.WorktreeRoot
	slotDirectory := filepath.Dir(target)
	if err := os.MkdirAll(slotDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(slotDirectory, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(slotDirectory, 0o700) })
	err := preparer.prepareLocked(context.Background(), repo, target, head, "slot", preparePhaseCreate, root)
	if err == nil || !strings.Contains(err.Error(), "marker") {
		t.Fatalf("prepareLockedTarget wrote an ownership marker below a read-only directory: %v", err)
	}
}

// TestRunPrepareWithIdentityForcesDescriptorPathWhenIdentityExpected covers
// the branch where a non-empty expected identity forces the descriptor-bound
// command path even though the preparer is not otherwise pinned; the
// configured root does not exist yet, so opening it must fail instead of
// silently falling back to the plain exec.Command path that ignores identity.
func TestRunPrepareWithIdentityForcesDescriptorPathWhenIdentityExpected(t *testing.T) {
	_, repo, preparer, _, target := prepareEdgesFixture(t)
	cfg := preparer.Config
	cfg.Repositories = map[string]config.Repository{string(repo.MainPath): {Prepare: config.Prepare{Command: []string{"/bin/true"}, Timeout: config.Duration{Duration: time.Second}}}}
	preparer.Config = cfg
	if err := preparer.runPrepareWithIdentity(context.Background(), repo, target, "some-identity"); err == nil {
		t.Fatal("descriptor-bound prepare command with a missing configured root succeeded")
	}
}

// TestRunPrepareWithIdentityPropagatesTargetOpenFailure covers the
// descriptor-bound path's own target-open failure: the configured root
// exists (so openOwnedRoot succeeds) but the target itself does not, so
// opening it as a directory must fail.
func TestRunPrepareWithIdentityPropagatesTargetOpenFailure(t *testing.T) {
	_, repo, preparer, _, target := prepareEdgesFixture(t)
	root := preparer.Config.Storage.WorktreeRoot
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := preparer.Config
	cfg.Repositories = map[string]config.Repository{string(repo.MainPath): {Prepare: config.Prepare{Command: []string{"/bin/true"}, Timeout: config.Duration{Duration: time.Second}}}}
	preparer.Config = cfg
	if err := preparer.runPrepareWithIdentity(context.Background(), repo, target, "some-identity"); err == nil {
		t.Fatal("descriptor-bound prepare command opened a missing target")
	}
}

// TestRunPrepareWithIdentityDetectsTargetReplacementDuringCommand covers the
// post-command identity check: the prepare command itself replaces the
// target directory with a new one at the same path before exiting, which
// must be detected as an ownership-uncertain identity change rather than
// treated as if the original directory were untouched.
func TestRunPrepareWithIdentityDetectsTargetReplacementDuringCommand(t *testing.T) {
	_, repo, preparer, head, target := prepareEdgesFixture(t)
	root := preparer.Config.Storage.WorktreeRoot
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := preparer.Prepare(context.Background(), repo, target, head, "slot"); err != nil {
		t.Fatal(err)
	}
	identity, err := preparer.WorktreeIdentity(target)
	if err != nil {
		t.Fatal(err)
	}
	// The command replaces its own working directory with a fresh, empty one
	// of the same name before it exits, so by the time cmd.Run() returns the
	// target names a different physical inode.
	script := "parent=$(dirname \"$PWD\"); name=$(basename \"$PWD\"); cd \"$parent\" && rm -rf \"$name\" && mkdir \"$name\""
	cfg := preparer.Config
	cfg.Repositories = map[string]config.Repository{string(repo.MainPath): {Prepare: config.Prepare{Command: []string{"/bin/sh", "-c", script}, Timeout: config.Duration{Duration: 5 * time.Second}}}}
	preparer.Config = cfg
	if err := preparer.runPrepareWithIdentity(context.Background(), repo, target, identity); !errors.Is(err, state.ErrOwnership) {
		t.Fatalf("target replacement during the prepare command was not detected: %v", err)
	}
}

// TestPrepareResumeWithIdentityRejectsAMismatchedIdentityBeforeResume covers
// PrepareResumeWithIdentity's own leading identity proof, which is otherwise
// only exercised with an empty (compatibility, always-passing) identity by
// PrepareResume elsewhere.
func TestPrepareResumeWithIdentityRejectsAMismatchedIdentityBeforeResume(t *testing.T) {
	ctx := context.Background()
	_, repo, preparer, head, target := prepareEdgesFixture(t)
	root := preparer.Config.Storage.WorktreeRoot
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := preparer.PrepareForRestore(ctx, repo, target, head, "slot"); err != nil {
		t.Fatal(err)
	}
	if err := preparer.PrepareResumeWithIdentity(ctx, repo, target, head, "slot", "not-the-real-identity"); !errors.Is(err, state.ErrOwnership) {
		t.Fatalf("mismatched pre-resume identity error=%v", err)
	}
}

// TestPrepareResumeWithIdentityDetectsTargetReplacementDuringResumeCommand
// covers the post-command identity re-check in the resume phase, mirroring
// the equivalent CREATE-phase coverage above but through
// PrepareResumeWithIdentity's own call sequence.
func TestPrepareResumeWithIdentityDetectsTargetReplacementDuringResumeCommand(t *testing.T) {
	ctx := context.Background()
	_, repo, preparer, head, target := prepareEdgesFixture(t)
	root := preparer.Config.Storage.WorktreeRoot
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := preparer.PrepareForRestore(ctx, repo, target, head, "slot"); err != nil {
		t.Fatal(err)
	}
	identity, err := preparer.WorktreeIdentity(target)
	if err != nil {
		t.Fatal(err)
	}
	script := "parent=$(dirname \"$PWD\"); name=$(basename \"$PWD\"); cd \"$parent\" && rm -rf \"$name\" && mkdir \"$name\""
	cfg := preparer.Config
	cfg.Repositories = map[string]config.Repository{string(repo.MainPath): {Prepare: config.Prepare{Command: []string{"/bin/sh", "-c", script}, Timeout: config.Duration{Duration: 5 * time.Second}}}}
	preparer.Config = cfg
	if err := preparer.PrepareResumeWithIdentity(ctx, repo, target, head, "slot", identity); !errors.Is(err, state.ErrOwnership) {
		t.Fatalf("target replacement during the resume command was not detected: %v", err)
	}
}

// TestFinishRestoreWithIdentityRejectsAMismatchedIdentityBeforeUnlock covers
// FinishRestoreWithIdentity's own leading identity proof, mirroring the
// resume-phase equivalent above.
func TestFinishRestoreWithIdentityRejectsAMismatchedIdentityBeforeUnlock(t *testing.T) {
	ctx := context.Background()
	_, repo, preparer, head, target := prepareEdgesFixture(t)
	root := preparer.Config.Storage.WorktreeRoot
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := preparer.PrepareForRestore(ctx, repo, target, head, "slot"); err != nil {
		t.Fatal(err)
	}
	if err := preparer.FinishRestoreWithIdentity(ctx, repo, target, head, "slot", "not-the-real-identity"); !errors.Is(err, state.ErrOwnership) {
		t.Fatalf("mismatched pre-finish identity error=%v", err)
	}
}

// TestFinishRestoreWithIdentityPropagatesUnlockAndLockFailures covers the
// admin Git-command failure branches distinct from the "missing repository"
// failure exercised elsewhere: a real repository whose specific unlock/lock
// call is made to fail.
func TestFinishRestoreWithIdentityPropagatesUnlockAndLockFailures(t *testing.T) {
	for _, test := range []struct {
		name    string
		pattern string
	}{
		{name: "unlock fails", pattern: "worktree unlock"},
		{name: "lock fails", pattern: "worktree lock"},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			_, repo, preparer, head, target := prepareEdgesFixture(t)
			root := preparer.Config.Storage.WorktreeRoot
			if err := os.MkdirAll(root, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := preparer.PrepareForRestore(ctx, repo, target, head, "slot"); err != nil {
				t.Fatal(err)
			}
			// installGitFault only intercepts calls made through the fault
			// wrapper it installs into PATH from this point on, so
			// PrepareForRestore's own (already completed) Git calls above do
			// not count toward the occurrence: FinishRestoreWithIdentity's
			// own unlock/lock call is the first one seen here.
			installGitFault(t, test.pattern, 1)
			if err := preparer.FinishRestoreWithIdentity(ctx, repo, target, head, "slot", ""); err == nil {
				t.Fatalf("finish restore succeeded despite a failing %q", test.pattern)
			}
		})
	}
}

// TestExistingTargetStatePropagatesLstatAndDirectoryOpenFailures calls
// existingTargetState directly to cover its own filesystem-error branches,
// which the normal Prepare flow cannot reach independently because
// prepareLockedTarget already performs an equivalent Lstat immediately
// beforehand.
func TestExistingTargetStatePropagatesLstatAndDirectoryOpenFailures(t *testing.T) {
	ctx := context.Background()
	_, repo, preparer, head, _ := prepareEdgesFixture(t)
	root := preparer.Config.Storage.WorktreeRoot

	t.Run("target lookup blocked by an unsearchable parent", func(t *testing.T) {
		slotDirectory := filepath.Join(root, testWorkspaceID, "blockd")
		target := filepath.Join(slotDirectory, testRepositoryID)
		if err := os.MkdirAll(slotDirectory, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(slotDirectory, 0); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(slotDirectory, 0o700) })
		owner, _, err := domain.OpenOwnedRoot(root, root)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = owner.Close() }()
		relative, err := filepath.Rel(root, target)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := preparer.existingTargetState(ctx, repo, target, head, "slot", preparePhaseCreate, root, owner, relative); err == nil {
			t.Fatal("existing target state lookup below an unsearchable directory succeeded")
		}
	})

	t.Run("target directory itself is unsearchable", func(t *testing.T) {
		slotDirectory := filepath.Join(root, testWorkspaceID, "blocko")
		target := filepath.Join(slotDirectory, testRepositoryID)
		if err := os.MkdirAll(target, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(target, 0); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(target, 0o700) })
		owner, _, err := domain.OpenOwnedRoot(root, root)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = owner.Close() }()
		relative, err := filepath.Rel(root, target)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := preparer.existingTargetState(ctx, repo, target, head, "slot", preparePhaseCreate, root, owner, relative); err == nil {
			t.Fatal("existing target state opened an unsearchable directory")
		}
	})
}

// TestAddWorktreeWithIdentityPropagatesLeafReservationFailure calls
// addWorktreeWithIdentity directly (bypassing ownership marker creation,
// which would otherwise fail first against the same read-only parent) to
// cover its own mkdirat failure branch distinct from the leaf already
// existing.
func TestAddWorktreeWithIdentityPropagatesLeafReservationFailure(t *testing.T) {
	_, repo, preparer, head, target := prepareEdgesFixture(t)
	root := preparer.Config.Storage.WorktreeRoot
	slotDirectory := filepath.Dir(target)
	if err := os.MkdirAll(slotDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	owner, _, err := domain.OpenOwnedRoot(root, root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = owner.Close() }()
	preparer.OwnedRoot = owner
	preparer.RootPath = root
	relativeTarget, err := filepath.Rel(root, target)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(slotDirectory, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(slotDirectory, 0o700) })
	if _, err := preparer.addWorktreeWithIdentity(context.Background(), repo, owner, target, relativeTarget, head); err == nil {
		t.Fatal("worktree leaf reservation below a read-only parent succeeded")
	}
}

// TestPrepareOnANewWorktreePropagatesFinalUnlockAndReadyLockFailures covers
// the CREATE phase's closing PREPARING-to-READY transition, which is
// otherwise only exercised for its opening PREPARING lock elsewhere.
func TestPrepareOnANewWorktreePropagatesFinalUnlockAndReadyLockFailures(t *testing.T) {
	t.Run("final unlock fails", func(t *testing.T) {
		_, repo, preparer, head, target := prepareEdgesFixture(t)
		root := preparer.Config.Storage.WorktreeRoot
		if err := os.MkdirAll(root, 0o700); err != nil {
			t.Fatal(err)
		}
		// A fresh worktree never unlocks an existing one, so the first
		// "worktree unlock" is the closing PREPARING-to-READY transition.
		installGitFault(t, "worktree unlock", 1)
		if err := preparer.Prepare(context.Background(), repo, target, head, "slot"); err == nil {
			t.Fatal("prepare succeeded despite a failing final unlock")
		}
	})
	t.Run("ready lock fails", func(t *testing.T) {
		_, repo, preparer, head, target := prepareEdgesFixture(t)
		root := preparer.Config.Storage.WorktreeRoot
		if err := os.MkdirAll(root, 0o700); err != nil {
			t.Fatal(err)
		}
		// The first "worktree lock" is the opening PREPARING lock; the
		// second is the closing READY lock.
		installGitFault(t, "worktree lock", 2)
		if err := preparer.Prepare(context.Background(), repo, target, head, "slot"); err == nil {
			t.Fatal("prepare succeeded despite a failing READY lock")
		}
	})
}

// TestRunWorktreeAdminOwnedRejectsAMismatchedIdentityBeforeTheGitCommand
// covers runWorktreeAdminOwned's own pre-command identity proof directly,
// which the higher-level Prepare/RemoveWorktreeAt flows never exercise with
// a deliberately wrong identity.
func TestRunWorktreeAdminOwnedRejectsAMismatchedIdentityBeforeTheGitCommand(t *testing.T) {
	ctx := context.Background()
	_, repo, preparer, head, target := prepareEdgesFixture(t)
	root := preparer.Config.Storage.WorktreeRoot
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	owner, _, err := domain.OpenOwnedRoot(root, root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = owner.Close() }()
	preparer.OwnedRoot = owner
	preparer.RootPath = root
	if err := preparer.Prepare(ctx, repo, target, head, "slot"); err != nil {
		t.Fatal(err)
	}
	relative, err := filepath.Rel(root, target)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := preparer.runWorktreeAdminOwned(ctx, repo, owner, relative, target, "not-the-real-identity", "unlock"); !errors.Is(err, state.ErrOwnership) {
		t.Fatalf("mismatched pre-command admin identity error=%v", err)
	}
}

// TestVerifyPreparedTargetIdentityDetectsMismatchAndUnavailability calls
// verifyPreparedTargetIdentity directly to cover both of its failure
// branches: a target that no longer exists, and one whose identity no
// longer matches what was expected.
func TestVerifyPreparedTargetIdentityDetectsMismatchAndUnavailability(t *testing.T) {
	ctx := context.Background()
	_, repo, preparer, head, target := prepareEdgesFixture(t)
	root := preparer.Config.Storage.WorktreeRoot
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	owner, _, err := domain.OpenOwnedRoot(root, root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = owner.Close() }()
	preparer.OwnedRoot = owner
	preparer.RootPath = root
	if err := preparer.Prepare(ctx, repo, target, head, "slot"); err != nil {
		t.Fatal(err)
	}
	relative, err := filepath.Rel(root, target)
	if err != nil {
		t.Fatal(err)
	}
	if err := preparer.verifyPreparedTargetIdentity(owner, relative, "not-the-real-identity"); !errors.Is(err, state.ErrOwnership) {
		t.Fatalf("mismatched identity error=%v", err)
	}
	if err := os.RemoveAll(target); err != nil {
		t.Fatal(err)
	}
	if err := preparer.verifyPreparedTargetIdentity(owner, relative, "any-identity"); !errors.Is(err, state.ErrOwnership) {
		t.Fatalf("unavailable identity error=%v", err)
	}
}

// TestAddWorktreeWithIdentityRejectsAReservedLeafThatIsNotADirectory covers
// the branch where mkdirat's reservation tolerates an already-existing leaf
// (os.ErrExist) but that leaf turns out not to be the physical directory
// addWorktreeWithIdentity needs to open next.
func TestAddWorktreeWithIdentityRejectsAReservedLeafThatIsNotADirectory(t *testing.T) {
	_, repo, preparer, head, target := prepareEdgesFixture(t)
	root := preparer.Config.Storage.WorktreeRoot
	slotDirectory := filepath.Dir(target)
	if err := os.MkdirAll(slotDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	owner, _, err := domain.OpenOwnedRoot(root, root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = owner.Close() }()
	preparer.OwnedRoot = owner
	preparer.RootPath = root
	relativeTarget, err := filepath.Rel(root, target)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := preparer.addWorktreeWithIdentity(context.Background(), repo, owner, target, relativeTarget, head); err == nil {
		t.Fatal("worktree add reserved a regular file as its target leaf")
	}
}

// TestFingerprintRejectsAMissingRepositoryMainPath covers Fingerprint's own
// leading physical-path check with a repository that does not exist at all,
// as opposed to the symlink-ancestor and unreadable-input cases exercised
// elsewhere.
func TestFingerprintRejectsAMissingRepositoryMainPath(t *testing.T) {
	missing := domain.CanonicalPath(filepath.Join(t.TempDir(), "missing-repository"))
	if _, err := Fingerprint(1, "oid", discovery.Repository{MainPath: missing}, config.Defaults()); err == nil {
		t.Fatal("fingerprint of a missing repository main path succeeded")
	}
}
