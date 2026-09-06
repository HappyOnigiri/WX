package state

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestBackupRetentionSkipsNonDatabaseEntriesAndPrunesOldGenerations(t *testing.T) {
	store := openTestStore(t)
	seedWorkspace(t, store)
	ctx := context.Background()
	first, err := store.Backup(ctx, 10, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	backupDir := filepath.Dir(first)
	old := filepath.Join(backupDir, "00000000T000000.000000000Z.db")
	if err := os.WriteFile(old, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	oldTime := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(old, oldTime, oldTime); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(backupDir, "notes.txt"), []byte("ignore"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(backupDir, "ignored-dir"), 0o700); err != nil {
		t.Fatal(err)
	}
	second, err := store.Backup(ctx, 1, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if second == first {
		t.Fatal("consecutive backups reused their path")
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatalf("old backup survived retention: %v", err)
	}
	databaseBackups, err := filepath.Glob(filepath.Join(backupDir, "*.db"))
	if err != nil || len(databaseBackups) != 1 || databaseBackups[0] != second {
		t.Fatalf("retained backups=%v err=%v want %q", databaseBackups, err, second)
	}
}
