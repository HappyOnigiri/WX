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
	"github.com/HappyOnigiri/WX/internal/state"
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
	if err := os.WriteFile(filepath.Join(repository, "tracked"), []byte("content\n"), 0o600); err != nil {
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

func TestSnapshotRefsAreIdempotentAndDeletionChecksOwnership(t *testing.T) {
	temp := t.TempDir()
	repository := filepath.Join(temp, "repository")
	mustMkdir(t, repository)
	gitCommand(t, repository, "init", "-b", "main")
	gitCommand(t, repository, "config", "user.name", "test")
	gitCommand(t, repository, "config", "user.email", "test@example.com")
	if err := os.WriteFile(filepath.Join(repository, "tracked"), []byte("content\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, repository, "add", ".")
	gitCommand(t, repository, "commit", "-m", "initial")
	repo := discovery.Repository{ID: "repository", MainPath: domain.CanonicalPath(repository), CommonDir: domain.CanonicalPath(filepath.Join(repository, ".git"))}
	manager := &Manager{Git: &gitx.Runner{Timeout: 5 * time.Second}}
	expires := time.Now().Add(time.Hour)
	snapshot, err := manager.Snapshot(context.Background(), repo, repository, "session", expires)
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := manager.Snapshot(context.Background(), repo, repository, "session", expires)
	if err != nil || replayed.WorktreeOID != snapshot.WorktreeOID {
		t.Fatalf("replayed snapshot=%+v err=%v", replayed, err)
	}
	conflict := snapshot
	conflict.HeadOID = strings.Repeat("0", 40)
	if err := manager.DeleteSnapshotRefs(context.Background(), repo, conflict); err == nil {
		t.Fatal("conflicting recovery ref deletion succeeded")
	}
	if err := manager.DeleteSnapshotRefs(context.Background(), repo, snapshot); err != nil {
		t.Fatal(err)
	}
	if err := manager.DeleteSnapshotRefs(context.Background(), repo, snapshot); err != nil {
		t.Fatalf("idempotent ref deletion: %v", err)
	}
	expired := state.Snapshot{ExpiresAt: time.Now().Add(-time.Second).Format(time.RFC3339Nano)}
	if err := manager.Restore(context.Background(), repo, filepath.Join(temp, "restore"), "slot", expired); err == nil {
		t.Fatal("expired restore succeeded")
	}
	unavailable := snapshot
	unavailable.ExpiresAt = expires.Format(time.RFC3339Nano)
	if err := manager.Restore(context.Background(), repo, filepath.Join(temp, "restore"), "slot", unavailable); err == nil {
		t.Fatal("restore with deleted refs succeeded")
	}
}

func TestArchiveRejectsUnownedAndMismatchedWorktrees(t *testing.T) {
	temp := t.TempDir()
	repository := filepath.Join(temp, "repository")
	root := filepath.Join(temp, "wx")
	mustMkdir(t, repository)
	mustMkdir(t, root)
	gitCommand(t, repository, "init", "-b", "main")
	gitCommand(t, repository, "config", "user.name", "test")
	gitCommand(t, repository, "config", "user.email", "test@example.com")
	if err := os.WriteFile(filepath.Join(repository, "tracked"), []byte("content\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, repository, "add", ".")
	gitCommand(t, repository, "commit", "-m", "initial")
	head := gitCommand(t, repository, "rev-parse", "HEAD")
	common := gitCommand(t, repository, "rev-parse", "--path-format=absolute", "--git-common-dir")
	repo := discovery.Repository{ID: "repository", MainPath: domain.CanonicalPath(repository), CommonDir: domain.CanonicalPath(common)}
	manager := &Manager{Git: &gitx.Runner{Timeout: 5 * time.Second}}
	ctx := context.Background()

	if err := manager.RemoveWorktree(ctx, repo, root, repository, head); err == nil || !strings.Contains(err.Error(), "outside") {
		t.Fatalf("outside removal error=%v", err)
	}
	missing := filepath.Join(root, "missing")
	if err := manager.RemoveWorktree(ctx, repo, root, missing, head); err != nil {
		t.Fatalf("idempotent missing removal: %v", err)
	}
	registered := filepath.Join(root, "slot", "repo")
	mustMkdir(t, filepath.Dir(registered))
	gitCommand(t, repository, "worktree", "add", "--detach", registered, head)
	if err := manager.RemoveWorktree(ctx, repo, root, registered, strings.Repeat("0", 40)); err == nil || !strings.Contains(err.Error(), "HEAD") {
		t.Fatalf("mismatched HEAD removal error=%v", err)
	}
	if _, err := manager.Snapshot(ctx, repo, filepath.Join(temp, "not-a-worktree"), "bad", time.Now().Add(time.Hour)); err == nil {
		t.Fatal("snapshot of missing worktree succeeded")
	}

	invalidRef := state.Snapshot{HeadRef: "not a ref", HeadOID: head, WorktreeRef: "also bad", WorktreeOID: head}
	if err := manager.DeleteSnapshotRefs(ctx, repo, invalidRef); err == nil || !strings.Contains(err.Error(), "invalid recovery ref") {
		t.Fatalf("invalid ref deletion error=%v", err)
	}
	if err := manager.RemoveWorktree(ctx, repo, root, registered, head); err != nil {
		t.Fatal(err)
	}
}

func mustMkdir(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o700); err != nil {
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
