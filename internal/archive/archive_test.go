package archive

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/HappyOnigiri/WX/internal/config"
	"github.com/HappyOnigiri/WX/internal/discovery"
	"github.com/HappyOnigiri/WX/internal/domain"
	"github.com/HappyOnigiri/WX/internal/gitx"
	"github.com/HappyOnigiri/WX/internal/state"
	"github.com/HappyOnigiri/WX/internal/workspace"
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

func TestRemovalReconcilesMissingRegistrationAndRejectsWrongRepository(t *testing.T) {
	root := t.TempDir()
	first := filepath.Join(root, "first")
	second := filepath.Join(root, "second")
	for _, repository := range []string{first, second} {
		mustMkdir(t, repository)
		gitCommand(t, repository, "init", "-b", "main")
		gitCommand(t, repository, "config", "user.name", "test")
		gitCommand(t, repository, "config", "user.email", "test@example.com")
		if err := os.WriteFile(filepath.Join(repository, "tracked"), []byte("base\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		gitCommand(t, repository, "add", ".")
		gitCommand(t, repository, "commit", "-m", "initial")
	}
	wxRoot := filepath.Join(root, "wx")
	target := filepath.Join(wxRoot, "slot", "root")
	mustMkdir(t, filepath.Dir(target))
	head := gitCommand(t, first, "rev-parse", "HEAD")
	gitCommand(t, first, "worktree", "add", "--detach", target, head)
	firstRepo := discovery.Repository{ID: "first", MainPath: domain.CanonicalPath(first), CommonDir: domain.CanonicalPath(gitCommand(t, first, "rev-parse", "--path-format=absolute", "--git-common-dir"))}
	secondRepo := discovery.Repository{ID: "second", MainPath: domain.CanonicalPath(second), CommonDir: domain.CanonicalPath(gitCommand(t, second, "rev-parse", "--path-format=absolute", "--git-common-dir"))}
	manager := &Manager{Git: &gitx.Runner{Timeout: 5 * time.Second}}
	if err := manager.RemoveWorktree(context.Background(), secondRepo, wxRoot, target, ""); err == nil || !strings.Contains(err.Error(), "common directory") {
		t.Fatalf("wrong repository removal error=%v", err)
	}
	if err := os.RemoveAll(target); err != nil {
		t.Fatal(err)
	}
	if err := manager.RemoveWorktree(context.Background(), firstRepo, wxRoot, target, head); err != nil {
		t.Fatalf("missing registered worktree reconciliation: %v", err)
	}
	if output := gitCommand(t, first, "worktree", "list", "--porcelain"); strings.Contains(output, target) {
		t.Fatal("missing worktree registration remains")
	}
}

func TestSnapshotRejectsConflictingExistingRecoveryRef(t *testing.T) {
	repository := t.TempDir()
	gitCommand(t, repository, "init", "-b", "main")
	gitCommand(t, repository, "config", "user.name", "test")
	gitCommand(t, repository, "config", "user.email", "test@example.com")
	if err := os.WriteFile(filepath.Join(repository, "tracked"), []byte("first\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, repository, "add", ".")
	gitCommand(t, repository, "commit", "-m", "first")
	repo := discovery.Repository{ID: "repository", MainPath: domain.CanonicalPath(repository), CommonDir: domain.CanonicalPath(gitCommand(t, repository, "rev-parse", "--path-format=absolute", "--git-common-dir"))}
	manager := &Manager{Git: &gitx.Runner{Timeout: 5 * time.Second}}
	snapshot, err := manager.Snapshot(context.Background(), repo, repository, "session", time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repository, "tracked"), []byte("second\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, repository, "add", ".")
	gitCommand(t, repository, "commit", "-m", "second")
	newHead := gitCommand(t, repository, "rev-parse", "HEAD")
	gitCommand(t, repository, "update-ref", snapshot.HeadRef, newHead)
	if _, err := manager.Snapshot(context.Background(), repo, repository, "session", time.Now().Add(time.Hour)); err == nil || !strings.Contains(err.Error(), "unexpected object") {
		t.Fatalf("conflicting recovery ref error=%v", err)
	}
}

func TestRestorePropagatesPreparationAndIndexFailures(t *testing.T) {
	root := t.TempDir()
	repository := filepath.Join(root, "repository")
	mustMkdir(t, repository)
	gitCommand(t, repository, "init", "-b", "main")
	gitCommand(t, repository, "config", "user.name", "test")
	gitCommand(t, repository, "config", "user.email", "test@example.com")
	if err := os.WriteFile(filepath.Join(repository, "tracked"), []byte("base\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, repository, "add", ".")
	gitCommand(t, repository, "commit", "-m", "initial")
	repo := discovery.Repository{ID: "repository", MainPath: domain.CanonicalPath(repository), CommonDir: domain.CanonicalPath(gitCommand(t, repository, "rev-parse", "--path-format=absolute", "--git-common-dir"))}
	runner := &gitx.Runner{Timeout: 5 * time.Second}
	cfg := config.Defaults()
	cfg.Storage.WorktreeRoot = filepath.Join(root, "worktrees")
	manager := &Manager{Git: runner, Preparer: &workspace.Preparer{Git: runner, Config: cfg}}
	snapshot, err := manager.Snapshot(context.Background(), repo, repository, "session", time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Restore(context.Background(), repo, filepath.Join(root, "outside"), "outside", snapshot); err == nil || !strings.Contains(err.Error(), "outside") {
		t.Fatalf("outside restore error=%v", err)
	}
	badIndex := snapshot
	badIndex.IndexTreeOID = "not-an-object"
	target := filepath.Join(cfg.Storage.WorktreeRoot, "bad-index", "root")
	if err := manager.Restore(context.Background(), repo, target, "bad-index", badIndex); err == nil {
		t.Fatal("restore with invalid index tree succeeded")
	}
	_, _ = runner.Run(context.Background(), repository, "worktree", "unlock", target)
	_, _ = runner.Run(context.Background(), repository, "worktree", "remove", "--force", target)
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
