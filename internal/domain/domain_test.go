package domain

import (
	"os"
	"path/filepath"
	"testing"
)

func TestIDsAndContainment(t *testing.T) {
	id, err := NewID()
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateID(id); err != nil {
		t.Fatal(err)
	}
	if err := ValidateID("not-an-id"); err == nil {
		t.Fatal("malformed ID was accepted")
	}
	if _, err := Canonicalize(""); err == nil {
		t.Fatal("empty path was canonicalized")
	}
	if _, err := Canonicalize(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("missing path was canonicalized")
	}
	if !IsWithin("/tmp/wx", "/tmp/wx/a/root") {
		t.Fatal("expected descendant")
	}
	for _, p := range []string{"/tmp/wx", "/tmp/wx-other", "/tmp/else"} {
		if IsWithin("/tmp/wx", p) {
			t.Errorf("%s must not be within root", p)
		}
	}
}

func TestOpenOwnedRootRejectsEscapingDescendantSymlink(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "slot")); err != nil {
		t.Fatal(err)
	}
	owned, relative, err := OpenOwnedRoot(root, filepath.Join(root, "slot", "root"))
	if err != nil {
		t.Fatal(err)
	}
	defer owned.Close()
	if _, err := owned.Lstat(relative); err == nil {
		t.Fatal("descriptor-relative lookup followed symlink outside ownership root")
	}
}

func TestValidatePhysicalPathRejectsParentSymlink(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	link := filepath.Join(root, "link")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	if err := ValidatePhysicalPath(filepath.Join(link, "child"), true); err == nil {
		t.Fatal("parent symlink was accepted for a missing leaf")
	}
	if err := ValidatePhysicalPath(filepath.Join(root, "missing"), true); err != nil {
		t.Fatalf("safe missing leaf was rejected: %v", err)
	}
}

func TestEnsurePhysicalDirectoryCreatesMissingSuffixWithoutFollowingSymlinks(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "one", "two", "three")
	if err := EnsurePhysicalDirectory(target, 0o700); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Lstat(target); err != nil || !info.IsDir() {
		t.Fatalf("target=%v err=%v", info, err)
	}
	outside := t.TempDir()
	link := filepath.Join(root, "link")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	if err := EnsurePhysicalDirectory(filepath.Join(link, "escape"), 0o700); err == nil {
		t.Fatal("directory creation followed a symlink outside the physical root")
	}
	if _, err := os.Lstat(filepath.Join(outside, "escape")); !os.IsNotExist(err) {
		t.Fatalf("outside directory was modified: %v", err)
	}
}

func TestSlotTransitions(t *testing.T) {
	if !CanTransitionSlot(SlotReady, SlotLeased) {
		t.Fatal("READY -> LEASED rejected")
	}
	if CanTransitionSlot(SlotArchived, SlotReady) {
		t.Fatal("ARCHIVED -> READY accepted")
	}
	if !CanTransitionSlot(SlotReady, SlotFailed) || !CanTransitionSlot(SlotLeased, SlotQuarantined) {
		t.Fatal("fail-safe slot transitions were rejected")
	}
	if CanTransitionSlot(SlotArchived, SlotFailed) {
		t.Fatal("archived slot transitioned back to a failure state")
	}
}
