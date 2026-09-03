package workspace

import (
	"os"
	"path/filepath"
	"testing"
)

// TestEnsureRootDirectoryRejectsWriteProtectedParent covers the Mkdir failure
// branch that is distinct from the already-exists race: a directory component
// is missing (Lstat sees os.ErrNotExist, which only requires search
// permission on the parent) but the parent itself forbids writes, so Mkdir
// fails for a real, non-transient reason.
func TestEnsureRootDirectoryRejectsWriteProtectedParent(t *testing.T) {
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
	if err := ensureRootDirectory(owner, filepath.Join("readonly", "new")); err == nil {
		t.Fatal("directory creation inside a read-only parent was accepted")
	}
}

// TestCopyPathFromOwnedRootRejectsUnsafeSourcePath covers the source-side
// safeRelative rejection in copyPathFromOwnedRoot, which is otherwise
// shadowed by the destination-side check already covered elsewhere.
func TestCopyPathFromOwnedRootRejectsUnsafeSourcePath(t *testing.T) {
	source := t.TempDir()
	destination := t.TempDir()
	if err := os.WriteFile(filepath.Join(source, "regular"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	sourceRoot, err := OpenPhysicalRoot(source)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sourceRoot.Close() }()
	destinationRoot, err := OpenPhysicalRoot(destination)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = destinationRoot.Close() }()
	if err := copyPathFromOwnedRoot(sourceRoot, "..", destinationRoot, "regular"); err == nil {
		t.Fatal("unsafe source path was accepted")
	}
}

// TestSafeGlobPropagatesNestedPatternErrors covers walkSafeGlob's propagation
// of an error raised by its own recursive call. safeGlob itself validates
// every pattern segment upfront and never reaches the walk for a malformed
// segment, so walkSafeGlob is called directly here with a first segment that
// matches a real directory, forcing the recursive call whose error must then
// be propagated by the parent frame.
func TestSafeGlobPropagatesNestedPatternErrors(t *testing.T) {
	root := t.TempDir()
	nested := filepath.Join(root, "valid")
	if err := os.Mkdir(nested, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nested, "entry"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	owner, err := os.OpenRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = owner.Close() }()
	var matches []string
	if err := walkSafeGlob(owner, ".", []string{"valid", "["}, 0, &matches); err == nil {
		t.Fatal("malformed nested glob pattern was accepted")
	}
}
