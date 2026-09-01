package daemon

import (
	"context"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/HappyOnigiri/WX/internal/config"
	"github.com/HappyOnigiri/WX/internal/state"
)

func TestLeaseArchiveAndRestorePreservesGitState(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	initGitRepo(t, repo)
	if err := os.WriteFile(filepath.Join(repo, ".gitignore"), []byte("shared/\nlocal.cfg\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".worktreeinclude"), []byte("local.cfg\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".worktreelink"), []byte("shared\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(repo, "shared"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "shared", "data"), []byte("shared\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "local.cfg"), []byte("local\n"), 0600); err != nil {
		t.Fatal(err)
	}
	gitRun(t, repo, "add", ".gitignore", ".worktreeinclude", ".worktreelink")
	gitRun(t, repo, "commit", "-m", "worktree metadata")
	if err := os.WriteFile(filepath.Join(repo, "tracked.txt"), []byte("dirty main\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg := config.Defaults()
	cfg.Storage.WorktreeRoot = filepath.Join(root, "worktrees")
	cfg.Pool.WarmPerWorkspace = 0
	cfg.Retention.EndedWorktree.Duration = 0
	cfg.Readiness.Timeout.Duration = 10 * time.Second
	store, err := state.Open(filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	m := New(cfg, store, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx := context.Background()
	lease, err := m.ResolveAndLease(ctx, repo, nil, "codex", os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	waitCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := m.WaitReady(waitCtx, lease.SessionID, lease.Token); err != nil {
		t.Fatal(err)
	}
	if got := gitOutput(t, lease.Path, "rev-parse", "--abbrev-ref", "HEAD"); got != "HEAD" {
		t.Fatalf("worktree branch=%q", got)
	}
	data, err := os.ReadFile(filepath.Join(lease.Path, "tracked.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "base\n" {
		t.Fatalf("main dirty content leaked: %q", data)
	}
	if info, err := os.Lstat(filepath.Join(lease.Path, "shared")); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("shared is not a symlink: %v %v", info, err)
	}
	if data, err := os.ReadFile(filepath.Join(lease.Path, "local.cfg")); err != nil || string(data) != "local\n" {
		t.Fatalf("include copy=%q err=%v", data, err)
	}
	if err := m.BindAgentSession(ctx, lease.SessionID, lease.Token, "agent-session-1"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(lease.Path, "tracked.txt"), []byte("staged\n"), 0600); err != nil {
		t.Fatal(err)
	}
	gitRun(t, lease.Path, "add", "tracked.txt")
	if err := os.WriteFile(filepath.Join(lease.Path, "tracked.txt"), []byte("working\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(lease.Path, "untracked.txt"), []byte("untracked\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := m.Release(ctx, lease.SessionID, lease.Token, "test"); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, 10*time.Second, func() bool { snaps, _ := store.Snapshots(ctx, lease.SessionID); return len(snaps) == 1 })
	resumed, err := m.Resume(ctx, lease.SessionID, "codex", os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	if resumed.Path == lease.Path {
		t.Fatal("resume reused old physical path")
	}
	if err := m.WaitReady(waitCtx, resumed.SessionID, resumed.Token); err != nil {
		t.Fatal(err)
	}
	status := gitOutput(t, resumed.Path, "status", "--porcelain")
	if !strings.Contains(status, "MM tracked.txt") || !strings.Contains(status, "?? untracked.txt") {
		t.Fatalf("restored status:\n%s", status)
	}
	data, err = os.ReadFile(filepath.Join(resumed.Path, "tracked.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "working\n" {
		t.Fatalf("working content=%q", data)
	}
	data, err = os.ReadFile(filepath.Join(repo, "tracked.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "dirty main\n" {
		t.Fatalf("source main changed: %q", data)
	}
}

func TestMultiRepositoryBundleAndRootRules(t *testing.T) {
	root := t.TempDir()
	initGitRepo(t, filepath.Join(root, "service"))
	initGitRepo(t, filepath.Join(root, "web"))
	if err := os.WriteFile(filepath.Join(root, "AGENTS.md"), []byte("root rules\n"), 0600); err != nil {
		t.Fatal(err)
	}
	shared := filepath.Join(root, "audit")
	if err := os.Mkdir(shared, 0700); err != nil {
		t.Fatal(err)
	}
	cfg := config.Defaults()
	cfg.Storage.WorktreeRoot = filepath.Join(root, "worktrees")
	cfg.Pool.WarmPerWorkspace = 0
	cfg.Workspaces = map[string]config.Workspace{root: {Link: []string{"audit"}}}
	store, err := state.Open(filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	m := New(cfg, store, slog.New(slog.NewTextHandler(io.Discard, nil)))
	lease, err := m.ResolveAndLease(context.Background(), root, nil, "codex", 1)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := m.WaitReady(ctx, lease.SessionID, lease.Token); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"service", "web"} {
		path := filepath.Join(lease.Path, name)
		if got := gitOutput(t, path, "rev-parse", "--abbrev-ref", "HEAD"); got != "HEAD" {
			t.Fatalf("%s branch=%s", name, got)
		}
	}
	data, err := os.ReadFile(filepath.Join(lease.Path, "AGENTS.md"))
	if err != nil || string(data) != "root rules\n" {
		t.Fatalf("root rules=%q err=%v", data, err)
	}
	info, err := os.Lstat(filepath.Join(lease.Path, "audit"))
	if err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("audit link=%v err=%v", info, err)
	}
}

func initGitRepo(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0700); err != nil {
		t.Fatal(err)
	}
	gitRun(t, path, "init", "-b", "main")
	gitRun(t, path, "config", "user.name", "test")
	gitRun(t, path, "config", "user.email", "test@example.com")
	if err := os.WriteFile(filepath.Join(path, "tracked.txt"), []byte("base\n"), 0600); err != nil {
		t.Fatal(err)
	}
	gitRun(t, path, "add", ".")
	gitRun(t, path, "commit", "-m", "initial")
}
func gitRun(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}
func gitOutput(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}
func waitUntil(t *testing.T, timeout time.Duration, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("condition was not met before timeout")
}
