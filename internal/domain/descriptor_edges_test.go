package domain

import (
	"os"
	"path/filepath"
	"testing"
)

// TestOpenOwnedDirectoryPropagatesOwnershipRootFailure exercises the branch
// where OpenOwnedRoot itself rejects the requested path before any directory
// descriptor is opened: OpenOwnedDirectory must surface that error unchanged
// rather than attempting to open anything.
func TestOpenOwnedDirectoryPropagatesOwnershipRootFailure(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if _, _, err := OpenOwnedDirectory(root, outside); err == nil {
		t.Fatal("OpenOwnedDirectory accepted a path outside the ownership root")
	}
}

// TestOpenOwnedDirectoryPropagatesDirectoryOpenFailure exercises the second
// failure branch of OpenOwnedDirectory: the ownership root itself resolves
// fine, but the requested leaf is not a physical directory.
func TestOpenOwnedDirectoryPropagatesDirectoryOpenFailure(t *testing.T) {
	root := t.TempDir()
	regular := filepath.Join(root, "regular")
	if err := os.WriteFile(regular, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := OpenOwnedDirectory(root, regular); err == nil {
		t.Fatal("OpenOwnedDirectory accepted a regular file as a directory leaf")
	}
}

// TestOpenDirectoryAtAndOpenRootAtRejectDirectRegularFileLeaf targets the
// "owned path is not a physical directory" branch in both OpenDirectoryAt and
// OpenRootAt for a leaf reached without any ".." traversal, which the
// existing escape tests do not exercise (those fail earlier, inside the
// os.Root sandbox itself).
func TestOpenDirectoryAtAndOpenRootAtRejectDirectRegularFileLeaf(t *testing.T) {
	root := t.TempDir()
	regular := filepath.Join(root, "leaf")
	if err := os.WriteFile(regular, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	owner, relative, err := OpenOwnedRoot(root, root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = owner.Close() }()
	if relative != "." {
		t.Fatalf("relative=%q", relative)
	}
	if _, _, err := OpenDirectoryAt(owner, "leaf"); err == nil {
		t.Fatal("OpenDirectoryAt accepted a regular file leaf")
	}
	if _, err := OpenRootAt(owner, "leaf"); err == nil {
		t.Fatal("OpenRootAt accepted a regular file leaf")
	}
}

// TestPhysicalPathInfoRejectsSymlinkComponent exercises PhysicalPathInfo's
// own symlink rejection directly: every path callers resolve through it
// (OpenDirectoryAt, OpenRootAt, OpenOwnedRoot) normally hits os.Root's own
// symlink protection first, which never lets this defense-in-depth check run
// on an in-root symlink reached without any escaping "..".
func TestPhysicalPathInfoRejectsSymlinkComponent(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	owner, err := os.OpenRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = owner.Close() }()
	if _, err := PhysicalPathInfo(owner, "link"); err == nil {
		t.Fatal("PhysicalPathInfo accepted a symlink component")
	}
}

// TestOpenFilesystemRootAtFilesystemRootYieldsDotRelative exercises the
// branch in openFilesystemRoot where the ownership root itself is the
// filesystem root: TrimPrefix leaves an empty relative path, which must
// normalize to ".".
func TestOpenOwnedRootAtFilesystemRootYieldsDotRelative(t *testing.T) {
	owned, relative, err := OpenOwnedRoot(string(filepath.Separator), string(filepath.Separator))
	if err != nil {
		t.Fatalf("OpenOwnedRoot(filesystem root): %v", err)
	}
	defer func() { _ = owned.Close() }()
	if relative != "." {
		t.Fatalf("relative=%q, want \".\"", relative)
	}
	if _, err := owned.Lstat("."); err != nil {
		t.Fatalf("filesystem root descriptor is unusable: %v", err)
	}
}
