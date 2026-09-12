package daemon

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/HappyOnigiri/WX/internal/state"
)

var testDatabaseTemplate []byte

// TestMain は migration 済みで業務データを含まない DB を一度だけ作り、各テストへ複製する。
func TestMain(m *testing.M) {
	root, err := os.MkdirTemp("", "wx-daemon-tests-")
	if err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	store, err := state.Open(filepath.Join(root, "state.db"))
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

// openTestStoreAtPath は指定 path へ専用 DB を展開して開く。root row は呼び出し側が必要に応じて用意する。
func openTestStoreAtPath(t testing.TB, path string) (*state.Store, error) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, err
	}
	_, writeErr := file.Write(testDatabaseTemplate)
	closeErr := file.Close()
	if writeErr != nil {
		return nil, writeErr
	}
	if closeErr != nil {
		return nil, closeErr
	}
	store, err := state.Open(path)
	return store, err
}
