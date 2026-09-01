package pool

import (
	"context"
	"github.com/HappyOnigiri/WX/internal/discovery"
	"github.com/HappyOnigiri/WX/internal/domain"
	"github.com/HappyOnigiri/WX/internal/gitx"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolveBranchesFallbackAndOverride(t *testing.T) {
	root := t.TempDir()
	a := initRepo(t, filepath.Join(root, "a"))
	b := initRepo(t, filepath.Join(root, "b"))
	git(t, a, "branch", "feature")
	w := discovery.Workspace{Repositories: []discovery.Repository{{ID: "a", MainPath: domain.CanonicalPath(a), RelativePath: "a", DefaultBranch: "main"}, {ID: "b", MainPath: domain.CanonicalPath(b), RelativePath: "b", DefaultBranch: "main"}}}
	r, err := ResolveBranches(context.Background(), &gitx.Runner{}, w, []string{"feature"})
	if err != nil {
		t.Fatal(err)
	}
	if r[0].RequestedRef != "feature" || r[1].OID != gitOut(t, b, "rev-parse", "main") {
		t.Fatalf("resolved=%+v", r)
	}
	r, err = ResolveBranches(context.Background(), &gitx.Runner{}, w, []string{"feature", "b=main"})
	if err != nil {
		t.Fatal(err)
	}
	if r[1].RequestedRef != "main" {
		t.Fatalf("override=%+v", r[1])
	}
}
func initRepo(t *testing.T, path string) string {
	t.Helper()
	if err := os.MkdirAll(path, 0700); err != nil {
		t.Fatal(err)
	}
	git(t, path, "init", "-b", "main")
	git(t, path, "config", "user.name", "test")
	git(t, path, "config", "user.email", "test@example.com")
	if err := os.WriteFile(filepath.Join(path, "tracked.txt"), []byte("base\n"), 0600); err != nil {
		t.Fatal(err)
	}
	git(t, path, "add", ".")
	git(t, path, "commit", "-m", "initial")
	return path
}
func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
}
func gitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(out))
}
