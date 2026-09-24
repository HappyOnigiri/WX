package discovery

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/HappyOnigiri/WorktreeX/internal/config"
	"github.com/HappyOnigiri/WorktreeX/internal/gitx"
)

// TestResolveFromLinkedWorktreeUsesMainWorktreeConfigScope は、linked worktree を cwd にしても
// main worktree に設定した既定 branch が当たることを固定する。設定 scope を cwd 側の checkout に
// すると override が外れ、その checkout の現在 branch が既定 branch になる。
func TestResolveFromLinkedWorktreeUsesMainWorktreeConfigScope(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := t.TempDir()
	mainPath := filepath.Join(root, "main")
	linkedPath := filepath.Join(root, "linked")
	if err := os.MkdirAll(mainPath, 0o700); err != nil {
		t.Fatal(err)
	}
	runner := &gitx.Runner{Timeout: 10 * time.Second}
	run := func(dir string, args ...string) {
		t.Helper()
		if res, err := runner.Run(ctx, dir, args...); err != nil {
			t.Fatalf("git %v: %v stderr=%s", args, err, res.Stderr)
		}
	}
	run(root, "init", "-b", "main", mainPath)
	run(mainPath, "config", "user.email", "test@example.test")
	run(mainPath, "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(mainPath, "README"), []byte("main\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	run(mainPath, "add", "README")
	run(mainPath, "commit", "-m", "initial")
	run(mainPath, "branch", "release")
	run(mainPath, "worktree", "add", "--detach", linkedPath, "main")

	cfg := config.DefaultsV2()
	cfg.Workspaces[mainPath] = config.Workspace{Repositories: map[string]config.Repository{".": {DefaultBranch: "release"}}}
	d := Discoverer{Git: runner, Config: cfg}
	for _, cwd := range []string{mainPath, linkedPath} {
		w, err := d.Resolve(ctx, cwd)
		if err != nil {
			t.Fatalf("resolve %s: %v", cwd, err)
		}
		if len(w.Repositories) != 1 {
			t.Fatalf("resolve %s: repositories=%+v", cwd, w.Repositories)
		}
		if got := w.Repositories[0].DefaultBranch; got != "release" {
			t.Fatalf("resolve %s: default branch=%q, want release", cwd, got)
		}
	}
}
