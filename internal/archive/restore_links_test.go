package archive

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/HappyOnigiri/WX/internal/config"
)

func TestRestoreUsesDestinationIgnoreRulesAndRetainsTreeValidation(t *testing.T) {
	repository, repo, manager, worktreeRoot := archiveFixture(t)
	hooksPath := filepath.Join(worktreeRoot, "hooks")
	if err := os.Mkdir(hooksPath, 0o700); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, repository, "config", "core.hooksPath", hooksPath)
	if err := os.WriteFile(filepath.Join(repository, ".gitignore"), []byte("/.tools/\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, repository, "add", ".gitignore")
	gitCommand(t, repository, "commit", "-m", "directory-only tools ignore")
	oldHead := gitCommand(t, repository, "rev-parse", "HEAD")
	if err := os.WriteFile(filepath.Join(repository, ".gitignore"), []byte("/.tools\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, repository, "add", ".gitignore")
	gitCommand(t, repository, "commit", "-m", "symlink tools ignore")
	if err := os.Mkdir(filepath.Join(repository, ".tools"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repository, ".worktreelink"), []byte(".tools\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(worktreeRoot, "source")
	gitCommand(t, repository, "worktree", "add", "--detach", source, oldHead)
	snapshot, err := manager.SnapshotWithPersistence(context.Background(), repo, source, "old-ignore", time.Now().Add(time.Hour), nil)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	target := filepath.Join(worktreeRoot, "slot", "root")
	pointAtSlot(t, manager, worktreeRoot, target)
	if err := manager.Restore(context.Background(), repo, target, "slot", snapshot); err != nil {
		t.Fatalf("restore with directory-only ignore: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(target, ".tools")); !os.IsNotExist(err) {
		t.Fatalf("unignored symlink changed restored tree: %v", err)
	}

	manager.Preparer.Config.Repositories = map[string]config.Repository{
		string(repo.MainPath): {Prepare: config.Prepare{Command: []string{"/bin/sh", "-c", "printf '%s\\n' unexpected > unexpected"}, Timeout: config.Duration{Duration: time.Second}}},
	}
	mismatchTarget := filepath.Join(worktreeRoot, "slot-mismatch", "root")
	pointAtSlot(t, manager, worktreeRoot, mismatchTarget)
	if err := manager.Restore(context.Background(), repo, mismatchTarget, "slot-mismatch", snapshot); err == nil || !strings.Contains(err.Error(), "restored working tree does not match snapshot") {
		t.Fatalf("ordinary restored tree difference error=%v", err)
	}
}
