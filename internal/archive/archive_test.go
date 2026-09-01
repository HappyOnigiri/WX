package archive

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/HappyOnigiri/WX/internal/discovery"
	"github.com/HappyOnigiri/WX/internal/domain"
	"github.com/HappyOnigiri/WX/internal/gitx"
)

func TestRemoveWorktreeRejectsSymlinkInRecordedPath(t *testing.T) {
	temp := t.TempDir()
	repository := filepath.Join(temp, "repository")
	root := filepath.Join(temp, "wx")
	mustMkdir(t, repository)
	mustMkdir(t, root)
	gitCommand(t, repository, "init", "-b", "main")
	gitCommand(t, repository, "config", "user.name", "test")
	gitCommand(t, repository, "config", "user.email", "test@example.com")
	if err := os.WriteFile(filepath.Join(repository, "tracked"), []byte("content\n"), 0600); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, repository, "add", ".")
	gitCommand(t, repository, "commit", "-m", "initial")
	head := gitCommand(t, repository, "rev-parse", "HEAD")

	first := filepath.Join(root, "slot-a", "repo")
	second := filepath.Join(root, "slot-b", "repo")
	mustMkdir(t, filepath.Dir(first))
	mustMkdir(t, filepath.Dir(second))
	gitCommand(t, repository, "worktree", "add", "--detach", first, head)
	gitCommand(t, repository, "worktree", "add", "--detach", second, head)
	gitCommand(t, repository, "worktree", "remove", "--force", first)
	if err := os.Remove(filepath.Dir(first)); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Dir(second), filepath.Dir(first)); err != nil {
		t.Fatal(err)
	}

	repo := discovery.Repository{
		ID:        domain.RepositoryID("repository"),
		MainPath:  domain.CanonicalPath(repository),
		CommonDir: domain.CanonicalPath(filepath.Join(repository, ".git")),
	}
	manager := &Manager{Git: &gitx.Runner{Timeout: 5 * time.Second}}
	err := manager.RemoveWorktree(context.Background(), repo, root, first, head)
	if err == nil || !strings.Contains(err.Error(), "symlink component") {
		t.Fatalf("RemoveWorktree error = %v, want symlink rejection", err)
	}
	if _, err := os.Stat(filepath.Join(second, ".git")); err != nil {
		t.Fatalf("unrelated registered worktree was changed: %v", err)
	}
}

func mustMkdir(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0700); err != nil {
		t.Fatal(err)
	}
}

func gitCommand(t *testing.T, dir string, args ...string) string {
	t.Helper()
	command := exec.Command("git", args...)
	command.Dir = dir
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, output)
	}
	return strings.TrimSpace(string(output))
}
