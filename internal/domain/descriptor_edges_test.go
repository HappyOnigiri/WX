package domain

import (
	"os"
	"path/filepath"
	"testing"
)

// TestOpenOwnedDirectoryPropagatesOwnershipRootFailure は、directory descriptor を開く前に OpenOwnedRoot が path を拒否する経路を検証する。
// OpenOwnedDirectory は別の open を試さず、その error をそのまま返す必要がある。
func TestOpenOwnedDirectoryPropagatesOwnershipRootFailure(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if _, _, err := OpenOwnedDirectory(root, outside); err == nil {
		t.Fatal("OpenOwnedDirectory accepted a path outside the ownership root")
	}
}

// TestOpenOwnedDirectoryPropagatesDirectoryOpenFailure は OpenOwnedDirectory の二つ目の失敗経路を検証する。
// ownership root は解決できるが、要求した leaf が physical directory ではない。
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

// TestOpenDirectoryAtAndOpenRootAtRejectDirectRegularFileLeaf は、`..` traversal をせず到達した regular file leaf を両関数が拒否する経路を検証する。
// 既存の escape test は os.Root sandbox 内で先に失敗するため、この経路を通らない。
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

// TestPhysicalPathInfoRejectsSymlinkComponent は PhysicalPathInfo 自身の symlink 拒否を直接検証する。
// 通常は os.Root の保護が先に働くため、`..` escape なしで root 内 symlink に到達するこの多層防御の経路は通らない。
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

// TestOpenFilesystemRootAtFilesystemRootYieldsDotRelative は ownership root 自体が filesystem root の openFilesystemRoot 経路を検証する。
// TrimPrefix が空 relative path を残すため、これを `.` に正規化する必要がある。
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
