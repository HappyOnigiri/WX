package daemon

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/HappyOnigiri/WX/internal/state"
)

func TestRegisteredRemovalIgnoresMarkersIdentityAndGitPointer(t *testing.T) {
	t.Parallel()
	ctx, manager, store, workspaceRecord, resolved, _ := managerCoverageFixture(t)
	slot := testSlot(t, manager, string(workspaceRecord.ID), "registered", 1, "REMOVING")
	repo := resolved[0].Repository
	target := filepath.Join(slot.Path, "repo")
	if err := os.MkdirAll(target, 0o700); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "keep"), []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, ".git"), []byte("gitdir: "+outside), 0o600); err != nil {
		t.Fatal(err)
	}
	slot.DirIdentity = "obsolete"
	if _, err := store.CreateStandby(ctx, slot, []state.SlotRepository{{RepositoryID: string(repo.ID), DirName: "repo", State: "RESTORE_RUNNING", BaseOID: "obsolete"}}); err != nil {
		t.Fatal(err)
	}
	common := string(repo.CommonDir)
	for _, name := range []string{"registered", "unrelated"} {
		admin := filepath.Join(common, "worktrees", name)
		if err := os.MkdirAll(admin, 0o700); err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(target, ".git")
		if name == "unrelated" {
			link = filepath.Join(outside, ".git")
		}
		if err := os.WriteFile(filepath.Join(admin, "gitdir"), []byte(link), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(admin, "locked"), []byte("user lock"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := manager.removeRegisteredSlot(ctx, slot); err != nil {
		t.Fatal(err)
	}
	if err := manager.removeRegisteredSlot(ctx, slot); err != nil {
		t.Fatalf("retry after deletion: %v", err)
	}
	for _, removed := range []string{slot.Path, filepath.Join(common, "worktrees", "registered")} {
		if _, err := os.Lstat(removed); !os.IsNotExist(err) {
			t.Fatalf("registered artifact remains: %s: %v", removed, err)
		}
	}
	for _, kept := range []string{filepath.Join(outside, "keep"), filepath.Join(common, "worktrees", "unrelated")} {
		if _, err := os.Stat(kept); err != nil {
			t.Fatalf("unrelated artifact changed: %s: %v", kept, err)
		}
	}
}

func TestRegisteredRemovalRejectsUnregisteredAndParentSymlink(t *testing.T) {
	t.Parallel()
	ctx, manager, store, workspaceRecord, _, _ := managerCoverageFixture(t)
	slot := testSlot(t, manager, string(workspaceRecord.ID), "unknown", 1, "REMOVING")
	if err := manager.removeRegisteredSlot(ctx, slot); err == nil {
		t.Fatal("unregistered target accepted")
	}
	if _, err := store.CreateStandby(ctx, slot, nil); err != nil {
		t.Fatal(err)
	}
	parent := filepath.Dir(slot.Path)
	moved := parent + "-moved"
	if err := os.Rename(parent, moved); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(moved, parent); err != nil {
		t.Fatal(err)
	}
	if err := manager.removeRegisteredSlot(ctx, slot); err == nil {
		t.Fatal("parent symlink followed")
	}
	if _, err := os.Stat(filepath.Join(moved, "unknown")); err != nil {
		t.Fatalf("symlink target deleted: %v", err)
	}
}

func TestRegisteredSnapshotRemovalIgnoresContentsAndPreservesSymlinkTarget(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	owner, err := os.OpenRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = owner.Close() }()
	if err := owner.MkdirAll("_recovery/workspace-snapshots", 0o700); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "keep")
	if err := os.WriteFile(outside, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	relative := "_recovery/workspace-snapshots/broken.tar"
	if err := os.Symlink(outside, filepath.Join(root, relative)); err != nil {
		t.Fatal(err)
	}
	if err := removeRegisteredSnapshot(owner, relative); err != nil {
		t.Fatal(err)
	}
	if err := removeRegisteredSnapshot(owner, relative); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(outside); err != nil || string(data) != "keep" {
		t.Fatalf("outside changed: %q %v", data, err)
	}
}
