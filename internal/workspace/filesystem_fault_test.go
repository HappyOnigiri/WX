package workspace

import (
	"os"
	"path/filepath"
	"testing"
)

// TestEnsureRootDirectoryRejectsWriteProtectedParentは、既存競合とは異なるMkdir失敗分岐を確認する。
// 欠落成分のLstatは親の検索権限だけで成功するが、親が書き込みを拒否するためMkdirが恒久的なエラーになる。
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

// TestCopyPathFromOwnedRootRejectsUnsafeSourcePathは、copyPathFromOwnedRootのsource側safeRelative拒否を確認する。
// この分岐は、別テストで確認済みのdestination側検査に隠れている。
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

// TestSafeGlobPropagatesNestedPatternErrorsは、walkSafeGlob自身の再帰呼び出しで生じたエラーが親へ伝播することを確認する。
// safeGlobは先に全segmentを検証するため、不正なsegmentではwalkに到達しない。実在ディレクトリに続く不正segmentを直接walkSafeGlobへ渡して再帰を発生させる。
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
