package workspace

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/HappyOnigiri/WX/internal/domain"
	"github.com/HappyOnigiri/WX/internal/gitx"
	"github.com/HappyOnigiri/WX/internal/state"
)

// TestEnsureOwnershipMarkerAtDirectFaultInjection exercises the descriptor-bound
// marker writer's own defensive checks directly. EnsureOwnershipMarker(At) both
// reject a nil/invalid owner before ever calling this helper, so its guard is
// otherwise dead from the exported entry points; a permission fault on the
// marker's parent directory is the only non-racy way to reach its remaining
// filesystem-error branches (MkdirAll, Lstat, and OpenFile failing for a reason
// other than the marker already existing).
func TestEnsureOwnershipMarkerAtDirectFaultInjection(t *testing.T) {
	marker := ownershipMarker{Version: 1, SlotID: "slot", Target: "target", CommonDir: "common"}

	if err := ensureOwnershipMarkerAt(nil, "marker", marker); err == nil {
		t.Fatal("nil ownership root was accepted")
	}

	t.Run("parent through regular file", func(t *testing.T) {
		root := t.TempDir()
		if err := os.WriteFile(filepath.Join(root, "blocker"), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		owner, err := os.OpenRoot(root)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = owner.Close() }()
		if err := ensureOwnershipMarkerAt(owner, filepath.Join("blocker", "marker"), marker); err == nil {
			t.Fatal("marker parent traversing a regular file was accepted")
		}
	})

	t.Run("lookup blocked by unsearchable ancestor", func(t *testing.T) {
		root := t.TempDir()
		blocked := filepath.Join(root, "blocked")
		if err := os.Mkdir(blocked, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(blocked, 0); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(blocked, 0o755) })
		owner, err := os.OpenRoot(root)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = owner.Close() }()
		if err := ensureOwnershipMarkerAt(owner, filepath.Join("blocked", "marker"), marker); err == nil {
			t.Fatal("marker lookup below an unsearchable directory was accepted")
		}
	})

	t.Run("creation blocked by unwritable parent", func(t *testing.T) {
		root := t.TempDir()
		readOnly := filepath.Join(root, "readonly")
		if err := os.Mkdir(readOnly, 0o500); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(readOnly, 0o755) })
		owner, err := os.OpenRoot(root)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = owner.Close() }()
		if err := ensureOwnershipMarkerAt(owner, filepath.Join("readonly", "marker"), marker); err == nil {
			t.Fatal("marker creation inside a read-only directory was accepted")
		}
	})
}

// TestValidateOwnershipMarkerRejectsNonDirectoryTargetWithRequiredLeaf covers
// newOwnershipMarker's own directory check for the allowMissingTarget=false
// path used by validation (as opposed to creation): domain.ValidatePhysicalPath
// only rejects symlink components, so a regular file target passes it and must
// be caught by newOwnershipMarker's later os.Lstat/IsDir check instead.
func TestValidateOwnershipMarkerRejectsNonDirectoryTargetWithRequiredLeaf(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target-file")
	if err := os.WriteFile(target, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	common := t.TempDir()
	owner, _, err := domain.OpenOwnedRoot(root, root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = owner.Close() }()
	if err := ValidateOwnershipMarkerAt(owner, root, target, "slot", common); !errors.Is(err, state.ErrOwnership) {
		t.Fatalf("regular file target validation error=%v", err)
	}
}

// TestRegisteredWorktreeLockStatusSkipsSymlinkAliasedRegistrations proves the
// documented boundary: a Git registration whose recorded path is reached
// through a symlink ancestor is rejected by the physical-path check before its
// canonical spelling is ever compared, so it can never be treated as a match
// even though resolving the query target would land on the same real
// directory. Git itself resolves symlinks while creating a worktree, so the
// admin file that backs "git worktree list" is rewritten afterward to store
// the symlink-aliased spelling that a corrupted or attacker-controlled
// admin area could otherwise present.
func TestRegisteredWorktreeLockStatusSkipsSymlinkAliasedRegistrations(t *testing.T) {
	repository := t.TempDir()
	gitCommand(t, repository, "init", "-b", "main")
	gitCommand(t, repository, "config", "user.name", "test")
	gitCommand(t, repository, "config", "user.email", "test@example.com")
	if err := os.WriteFile(filepath.Join(repository, "tracked"), []byte("base\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, repository, "add", ".")
	gitCommand(t, repository, "commit", "-m", "initial")

	actualParent := filepath.Join(repository, "actual-parent")
	if err := os.Mkdir(actualParent, 0o700); err != nil {
		t.Fatal(err)
	}
	aliasParent := filepath.Join(repository, "alias-parent")
	if err := os.Symlink(actualParent, aliasParent); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, repository, "worktree", "add", "--detach", filepath.Join(actualParent, "wt"), gitOutput(t, repository, "rev-parse", "HEAD"))

	gitdirFiles, err := filepath.Glob(filepath.Join(repository, ".git", "worktrees", "*", "gitdir"))
	if err != nil || len(gitdirFiles) != 1 {
		t.Fatalf("locate worktree gitdir admin file: files=%v err=%v", gitdirFiles, err)
	}
	aliasedTarget := filepath.Join(aliasParent, "wt")
	if err := os.WriteFile(gitdirFiles[0], []byte(filepath.Join(aliasedTarget, ".git")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	runner := &gitx.Runner{Timeout: 5 * time.Second}
	ctx := context.Background()
	if _, found, err := RegisteredWorktreeLockReason(ctx, runner, repository, aliasedTarget); err != nil || found {
		t.Fatalf("symlink-aliased registration was treated as a match: found=%v err=%v", found, err)
	}
}

// TestRegisteredWorktreeLockReasonPropagatesUnresolvableTargetSymlinkLoop
// covers the query-side canonicalization failure: a symlink loop at the
// queried target cannot be resolved, and that error must propagate instead of
// silently reporting "not found".
func TestRegisteredWorktreeLockReasonPropagatesUnresolvableTargetSymlinkLoop(t *testing.T) {
	repository := t.TempDir()
	gitCommand(t, repository, "init", "-b", "main")
	gitCommand(t, repository, "config", "user.name", "test")
	gitCommand(t, repository, "config", "user.email", "test@example.com")
	if err := os.WriteFile(filepath.Join(repository, "tracked"), []byte("base\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, repository, "add", ".")
	gitCommand(t, repository, "commit", "-m", "initial")

	loopA := filepath.Join(repository, "loop-a")
	loopB := filepath.Join(repository, "loop-b")
	if err := os.Symlink(loopB, loopA); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(loopA, loopB); err != nil {
		t.Fatal(err)
	}

	runner := &gitx.Runner{Timeout: 5 * time.Second}
	ctx := context.Background()
	if _, _, err := RegisteredWorktreeLockReason(ctx, runner, repository, loopA); err == nil {
		t.Fatal("symlink loop at the queried target was treated as resolvable")
	}
}

// TestRegisteredWorktreeLockStatusAtSkipsRemovedWorktreeRegistration covers
// the descriptor-open failure branch: Git can still list a worktree whose
// physical directory was removed by an interrupted deletion. That stale
// registration must be skipped rather than crash or falsely match.
func TestRegisteredWorktreeLockStatusAtSkipsRemovedWorktreeRegistration(t *testing.T) {
	repository := t.TempDir()
	gitCommand(t, repository, "init", "-b", "main")
	gitCommand(t, repository, "config", "user.name", "test")
	gitCommand(t, repository, "config", "user.email", "test@example.com")
	if err := os.WriteFile(filepath.Join(repository, "tracked"), []byte("base\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, repository, "add", ".")
	gitCommand(t, repository, "commit", "-m", "initial")

	root := t.TempDir()
	removedTarget := filepath.Join(root, "removed")
	keptTarget := filepath.Join(root, "kept")
	head := gitOutput(t, repository, "rev-parse", "HEAD")
	gitCommand(t, repository, "worktree", "add", "--detach", removedTarget, head)
	gitCommand(t, repository, "worktree", "add", "--detach", keptTarget, head)

	if err := os.RemoveAll(removedTarget); err != nil {
		t.Fatal(err)
	}

	owner, _, err := domain.OpenOwnedRoot(root, root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = owner.Close() }()

	runner := &gitx.Runner{Timeout: 5 * time.Second}
	ctx := context.Background()
	if _, _, found, err := RegisteredWorktreeLockStatusAt(ctx, runner, repository, owner, root, "removed", "any-identity"); err != nil || found {
		t.Fatalf("removed worktree registration was treated as a match: found=%v err=%v", found, err)
	}
}

// TestRemoveOwnershipMarkerAtPropagatesPermissionFailures covers the Lstat
// error branch that is distinct from "already removed": the marker's
// directory becomes unsearchable, so the lookup fails for a real reason
// instead of os.ErrNotExist.
func TestRemoveOwnershipMarkerAtPropagatesPermissionFailures(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "slots", "slot", "root")
	if err := os.MkdirAll(target, 0o700); err != nil {
		t.Fatal(err)
	}
	common := t.TempDir()
	owner, _, err := domain.OpenOwnedRoot(root, root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = owner.Close() }()
	if err := EnsureOwnershipMarkerAt(owner, root, target, "slot", common); err != nil {
		t.Fatal(err)
	}
	slotDirectory := filepath.Join(root, "slots", "slot")
	if err := os.Chmod(slotDirectory, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(slotDirectory, 0o700) })
	if err := removeOwnershipMarkerAt(owner, root, target); err == nil {
		t.Fatal("descriptor-bound marker removal below an unsearchable directory was accepted")
	}
}

// TestNewOwnershipMarkerAtPropagatesDirectoryOpenFailureForExistingTarget
// covers the branch where the target's own Lstat succeeds (only its parent's
// search permission is required) but opening it as a directory fails because
// the target itself denies search access.
func TestNewOwnershipMarkerAtPropagatesDirectoryOpenFailureForExistingTarget(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "slots", "slot", "root")
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
	common := t.TempDir()
	if _, err := newOwnershipMarkerAt(owner, root, target, "slot", common, true); err == nil {
		t.Fatal("unsearchable existing marker target was accepted")
	}
}

// TestValidateRemovalOwnershipRejectsMalformedMarkerContents proves that
// ValidateRemovalOwnership (and its descriptor-bound counterpart) reach and
// propagate readOwnershipMarker's own content validation, not only the
// target/common-directory identity checks exercised elsewhere.
func TestValidateRemovalOwnershipRejectsMalformedMarkerContents(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "slots", "slot", "root")
	if err := os.MkdirAll(target, 0o700); err != nil {
		t.Fatal(err)
	}
	common := t.TempDir()
	if err := EnsureOwnershipMarker(root, target, "slot", common); err != nil {
		t.Fatal(err)
	}
	markerPath := filepath.Join(filepath.Dir(target), ownershipMarkerNameForTarget(target))
	if err := os.WriteFile(markerPath, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ValidateRemovalOwnership(root, target, common); !errors.Is(err, state.ErrOwnership) {
		t.Fatalf("malformed marker removal proof error=%v", err)
	}

	descriptorTarget := filepath.Join(root, "slots", "slot2", "root")
	if err := os.MkdirAll(descriptorTarget, 0o700); err != nil {
		t.Fatal(err)
	}
	owner, _, err := domain.OpenOwnedRoot(root, root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = owner.Close() }()
	if err := EnsureOwnershipMarkerAt(owner, root, descriptorTarget, "slot2", common); err != nil {
		t.Fatal(err)
	}
	descriptorMarkerRelative, err := ownershipMarkerRelative(root, descriptorTarget)
	if err != nil {
		t.Fatal(err)
	}
	file, err := owner.OpenFile(descriptorMarkerRelative, os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write([]byte("{not json")); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := ValidateRemovalOwnershipAt(owner, root, descriptorTarget, common); !errors.Is(err, state.ErrOwnership) {
		t.Fatalf("malformed descriptor-bound marker removal proof error=%v", err)
	}
}

// TestOpenPhysicalRootPropagatesOwnedRootFailureOnUnsearchableParent covers
// OpenPhysicalRoot's own domain.OpenOwnedRoot error branch: the physical-path
// and ancestor-symlink checks only require search permission on each
// ancestor's parent, but actually opening the immediate parent as a Root
// additionally requires search permission on the parent itself.
func TestOpenPhysicalRootPropagatesOwnedRootFailureOnUnsearchableParent(t *testing.T) {
	root := t.TempDir()
	parent := filepath.Join(root, "parent")
	target := filepath.Join(parent, "target")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(parent, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(parent, 0o755) })
	if _, err := OpenPhysicalRoot(target); err == nil {
		t.Fatal("physical root behind an unsearchable parent was opened")
	}
}

// TestOpenPhysicalRootPropagatesReopenFailureOnUnsearchableTarget covers
// OpenPhysicalRoot's own OpenRoot error branch: the preceding physical-path
// and Lstat checks only require search permission on the target's parent, but
// reopening the target itself as a Root additionally requires search
// permission on the target.
func TestOpenPhysicalRootPropagatesReopenFailureOnUnsearchableTarget(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "blocked")
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(target, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(target, 0o755) })
	if _, err := OpenPhysicalRoot(target); err == nil {
		t.Fatal("unsearchable physical root was opened")
	}
}
