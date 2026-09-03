package pool

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/HappyOnigiri/WX/internal/discovery"
	"github.com/HappyOnigiri/WX/internal/domain"
	"github.com/HappyOnigiri/WX/internal/gitx"
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

func TestResolveBranchesRejectsAmbiguousAndMissingSpecifications(t *testing.T) {
	root := t.TempDir()
	a := initRepo(t, filepath.Join(root, "services", "api"))
	b := initRepo(t, filepath.Join(root, "legacy", "api"))
	w := discovery.Workspace{Repositories: []discovery.Repository{
		{ID: "a", MainPath: domain.CanonicalPath(a), RelativePath: "services/api", DefaultBranch: "main"},
		{ID: "b", MainPath: domain.CanonicalPath(b), RelativePath: "legacy/api", DefaultBranch: "main"},
	}}
	runner := &gitx.Runner{}
	cases := [][]string{{"=main"}, {"missing=main"}, {"api=main"}, {"services/api=main", "services/api=other"}, {"main", "other"}, {"services/api=missing"}}
	for _, specs := range cases {
		if _, err := ResolveBranches(context.Background(), runner, w, specs); err == nil {
			t.Errorf("ResolveBranches(%v) succeeded", specs)
		}
	}
	missingDefaults := w
	missingDefaults.Repositories = append([]discovery.Repository(nil), w.Repositories...)
	missingDefaults.Repositories[0].DefaultBranch = "missing"
	if _, err := ResolveBranches(context.Background(), runner, missingDefaults, nil); err == nil {
		t.Fatal("missing default branches succeeded")
	}
	broken := w
	broken.Repositories[0].MainPath = domain.CanonicalPath(filepath.Join(root, "missing"))
	if _, err := ResolveBranches(context.Background(), runner, broken, nil); err == nil {
		t.Fatal("Git execution failure succeeded")
	}
}

func TestResolveBranchesPropagatesGlobalResolutionFailure(t *testing.T) {
	root := t.TempDir()
	w := discovery.Workspace{Repositories: []discovery.Repository{{ID: "repository", MainPath: domain.CanonicalPath(filepath.Join(root, "missing")), RelativePath: "repository", DefaultBranch: "main"}}}
	if _, err := ResolveBranches(context.Background(), &gitx.Runner{}, w, []string{"feature"}); err == nil {
		t.Fatal("global branch resolution ignored Git failure")
	}
}

// TestResolveBranchesFailsClosedUnderACanceledContext uses a repository that
// resolves successfully on a live context, so the failures below can only come
// from the canceled context. Pointing the fixture at a nonexistent repository
// instead — as TestResolveBranchesPropagatesGlobalResolutionFailure does
// deliberately — would make both assertions hold whether or not cancellation
// had any effect at all.
func TestResolveBranchesFailsClosedUnderACanceledContext(t *testing.T) {
	root := t.TempDir()
	main := initRepo(t, filepath.Join(root, "repository"))
	repo := discovery.Repository{ID: "repository", MainPath: domain.CanonicalPath(main), RelativePath: "repository", DefaultBranch: "main"}
	w := discovery.Workspace{Repositories: []discovery.Repository{repo}}
	runner := &gitx.Runner{}
	if _, err := ResolveBranches(context.Background(), runner, w, []string{"feature"}); err != nil {
		t.Fatalf("global branch resolution on a live context: %v", err)
	}
	if _, err := ResolveBranches(context.Background(), runner, w, nil); err != nil {
		t.Fatalf("default branch resolution on a live context: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := ResolveBranches(ctx, runner, w, []string{"feature"}); err == nil {
		t.Fatal("canceled global branch resolution succeeded")
	}
	if _, err := ResolveBranches(ctx, runner, w, nil); err == nil {
		t.Fatal("canceled default branch resolution succeeded")
	}
}

func initRepo(t *testing.T, path string) string {
	t.Helper()
	if err := os.MkdirAll(path, 0o700); err != nil {
		t.Fatal(err)
	}
	git(t, path, "init", "-b", "main")
	git(t, path, "config", "user.name", "test")
	git(t, path, "config", "user.email", "test@example.com")
	if err := os.WriteFile(filepath.Join(path, "tracked.txt"), []byte("base\n"), 0o600); err != nil {
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
