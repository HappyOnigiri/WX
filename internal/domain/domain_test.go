package domain

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestIDsAndContainment(t *testing.T) {
	if _, err := NewID(); err != nil {
		t.Fatal(err)
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

func TestOpenOwnedRootRejectsInvalidOwnershipRoots(t *testing.T) {
	root := t.TempDir()
	if owned, _, err := OpenOwnedRoot(root, t.TempDir()); err == nil {
		_ = owned.Close()
		t.Fatal("outside path was accepted")
	}
	if owned, _, err := OpenOwnedRoot(filepath.Join(root, "missing"), filepath.Join(root, "missing", "slot")); err == nil {
		_ = owned.Close()
		t.Fatal("missing ownership root was accepted")
	}
	regular := filepath.Join(root, "file")
	if err := os.WriteFile(regular, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if owned, _, err := OpenOwnedRoot(regular, filepath.Join(regular, "slot")); err == nil {
		_ = owned.Close()
		t.Fatal("regular file ownership root was accepted")
	}
}

func TestOpenOwnedRootPinsRootAcrossReplacement(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "owned")
	target := filepath.Join(root, "slot", "root")
	if err := os.MkdirAll(target, 0o700); err != nil {
		t.Fatal(err)
	}
	owned, relative, err := OpenOwnedRoot(root, target)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = owned.Close() }()

	replaced := filepath.Join(parent, "owned-old")
	if err := os.Rename(root, replaced); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "slot", "root"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := owned.Lstat(relative); err != nil {
		t.Fatalf("pinned ownership root lost after path replacement: %v", err)
	}
	if _, err := os.Stat(filepath.Join(replaced, "slot", "root")); err != nil {
		t.Fatalf("original ownership root is unavailable: %v", err)
	}
}

// NEW-1: allocation must continue in the descriptor-pinned root when the
// configured pathname is replaced after the root check.
func TestOpenOwnedRootAllocationSurvivesRootReplacement(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "owned")
	outside := filepath.Join(parent, "outside")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(outside, 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "workspaces", "slot", "root")
	owned, relative, err := OpenOwnedRoot(root, target)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = owned.Close() }()
	old := filepath.Join(parent, "owned-old")
	if err := os.Rename(root, old); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, root); err != nil {
		t.Fatal(err)
	}
	if err := owned.MkdirAll(relative, 0o700); err != nil {
		t.Fatalf("descriptor-relative allocation failed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(old, "workspaces", "slot", "root")); err != nil {
		t.Fatalf("pinned root was not allocated: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(outside, "workspaces")); !os.IsNotExist(err) {
		t.Fatalf("replacement target was modified outside ownership root: %v", err)
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
	if err := ValidatePhysicalPath(filepath.Join(root, "missing", "child"), true); !os.IsNotExist(err) {
		t.Fatalf("missing intermediate component was not rejected: %v", err)
	}
	if err := ValidatePhysicalPath(string(filepath.Separator), false); err != nil {
		t.Fatalf("filesystem root was rejected: %v", err)
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
	regular := filepath.Join(root, "regular")
	if err := os.WriteFile(regular, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := EnsurePhysicalDirectory(regular, 0o700); err == nil {
		t.Fatal("regular file was accepted as a physical directory")
	}
	if err := EnsurePhysicalDirectory(string(filepath.Separator), 0o700); err != nil {
		t.Fatalf("filesystem root was rejected: %v", err)
	}
}

func TestEnsurePhysicalDirectoryRootPinsFinalDirectoryAcrossReplacement(t *testing.T) {
	parent := t.TempDir()
	path := filepath.Join(parent, "root", "nested")
	outside := filepath.Join(parent, "outside")
	if err := os.Mkdir(outside, 0o700); err != nil {
		t.Fatal(err)
	}
	owned, err := EnsurePhysicalDirectoryRoot(path, 0o700)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = owned.Close() }()

	oldPath := filepath.Join(parent, "root-old", "nested")
	if err := os.Rename(filepath.Dir(path), filepath.Dir(oldPath)); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Dir(path)); err != nil {
		t.Fatal(err)
	}
	file, err := owned.OpenFile("marker", os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(oldPath, "marker")); err != nil {
		t.Fatalf("pinned directory was not used after replacement: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(outside, "marker")); !os.IsNotExist(err) {
		t.Fatalf("replacement directory was modified: %v", err)
	}
}

func TestNewShortIDIsFixedWidthLowercaseBase36(t *testing.T) {
	seen := map[string]bool{}
	for range 512 {
		id, err := NewShortID()
		if err != nil {
			t.Fatal(err)
		}
		if len(id) != ShortIDLength {
			t.Fatalf("id=%q length=%d want %d", id, len(id), ShortIDLength)
		}
		if !ValidShortID(id) {
			t.Fatalf("id=%q is outside the lowercase base36 alphabet", id)
		}
		if id != strings.ToLower(id) {
			t.Fatalf("id=%q contains uppercase; APFS is case-insensitive by default", id)
		}
		seen[id] = true
	}
	// 512 draws from ~2.18e9 values collide with probability well under 1e-4,
	// so a large duplicate count means the generator is not random.
	if len(seen) < 500 {
		t.Fatalf("only %d distinct ids out of 512 draws", len(seen))
	}
}

func TestNewShortIDIsUsableAsAPathAndRefComponent(t *testing.T) {
	id, err := NewShortID()
	if err != nil {
		t.Fatal(err)
	}
	if strings.ContainsAny(id, `/\:.`) {
		t.Fatalf("id=%q contains a character that breaks a path component, a Git ref component, or a wx lock reason", id)
	}
	if !ValidWxLockReason("wx:"+id+":READY", id) {
		t.Fatalf("id=%q cannot be embedded in a wx lock reason", id)
	}
}

func TestValidShortIDRejectsEverythingNewShortIDCannotProduce(t *testing.T) {
	for _, value := range []string{"", "abc", "abcdefg", "ABCDEF", "abcde-", "abcd/f", "abcde.", "  abcd"} {
		if ValidShortID(value) {
			t.Errorf("ValidShortID(%q) accepted", value)
		}
	}
	for _, value := range []string{"000000", "zzzzzz", "a1b2c3"} {
		if !ValidShortID(value) {
			t.Errorf("ValidShortID(%q) rejected", value)
		}
	}
}
