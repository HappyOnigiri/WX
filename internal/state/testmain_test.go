package state

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

var testDatabaseTemplate []byte

// TestMain は migration 済みで業務データを含まない DB を一度だけ作り、各テストへ複製する。
func TestMain(m *testing.M) {
	root, err := os.MkdirTemp("", "wx-state-tests-")
	if err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	store, err := Open(filepath.Join(root, "state.db"))
	if err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		_ = os.RemoveAll(root)
		os.Exit(1)
	}
	backupPath, err := store.Backup(context.Background(), 1, time.Hour)
	closeErr := store.Close()
	if err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		_ = os.RemoveAll(root)
		os.Exit(1)
	}
	if closeErr != nil {
		_, _ = fmt.Fprintln(os.Stderr, closeErr)
		_ = os.RemoveAll(root)
		os.Exit(1)
	}
	testDatabaseTemplate, err = os.ReadFile(backupPath)
	if err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		_ = os.RemoveAll(root)
		os.Exit(1)
	}
	code := m.Run()
	_ = os.RemoveAll(root)
	os.Exit(code)
}

// writeTestDatabase は専用 path に DB の複製を排他的に展開する。
func writeTestDatabase(t testing.TB, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	_, writeErr := file.Write(testDatabaseTemplate)
	closeErr := file.Close()
	if writeErr != nil {
		t.Fatal(writeErr)
	}
	if closeErr != nil {
		t.Fatal(closeErr)
	}
}

// openTestStoreAtPath はテンプレートを指定 path へ展開して開く。業務データの seed は呼び出し側が行う。
func openTestStoreAtPath(t testing.TB, path string) (*Store, error) {
	t.Helper()
	writeTestDatabase(t, path)
	return Open(path)
}
