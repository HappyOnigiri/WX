package pool

import (
	"context"
	"errors"
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

func TestResolveBranchesRejectsGlobalMissingBranchForSingleRepository(t *testing.T) {
	root := t.TempDir()
	repo := initRepo(t, filepath.Join(root, "repository"))
	w := discovery.Workspace{Repositories: []discovery.Repository{{ID: "repository", MainPath: domain.CanonicalPath(repo), RelativePath: "repository", DefaultBranch: "main"}}}

	_, err := ResolveBranches(context.Background(), &gitx.Runner{}, w, []string{"missing"})
	if err == nil {
		t.Fatal("missing global branch for a single repository succeeded")
	}
	for _, want := range []string{"branch \"missing\" does not exist", "repository", "refusing to use default branch \"main\""} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not contain %q", err, want)
		}
	}
}

func TestResolveBranchesRejectsGlobalMissingBranchForAllRepositories(t *testing.T) {
	root := t.TempDir()
	a := initRepo(t, filepath.Join(root, "a"))
	b := initRepo(t, filepath.Join(root, "b"))
	w := discovery.Workspace{Repositories: []discovery.Repository{
		{ID: "a", MainPath: domain.CanonicalPath(a), RelativePath: "a", DefaultBranch: "main"},
		{ID: "b", MainPath: domain.CanonicalPath(b), RelativePath: "b", DefaultBranch: "main"},
	}}

	_, err := ResolveBranches(context.Background(), &gitx.Runner{}, w, []string{"missing"})
	if err == nil {
		t.Fatal("missing global branch for all repositories succeeded")
	}
	for _, want := range []string{"branch \"missing\" does not exist in any repository", "a", "b", "refusing to use default branches"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not contain %q", err, want)
		}
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
	var unresolved *UnresolvedDefaultBranchError
	emptyDefault := w
	emptyDefault.Repositories = append([]discovery.Repository(nil), w.Repositories...)
	emptyDefault.Repositories[0].DefaultBranch = ""
	if _, err := ResolveBranches(context.Background(), runner, emptyDefault, nil); !errors.As(err, &unresolved) {
		t.Fatalf("empty default branch error=%v, want UnresolvedDefaultBranchError", err)
	} else if unresolved.RepositoryRelativePath != "services/api" || strings.Contains(err.Error(), `default branch ""`) {
		t.Fatalf("unresolved default branch error=%q, want repository path and no empty branch name", err)
	}
}

func TestResolveBranchesPropagatesGlobalResolutionFailure(t *testing.T) {
	root := t.TempDir()
	w := discovery.Workspace{Repositories: []discovery.Repository{{ID: "repository", MainPath: domain.CanonicalPath(filepath.Join(root, "missing")), RelativePath: "repository", DefaultBranch: "main"}}}
	if _, err := ResolveBranches(context.Background(), &gitx.Runner{}, w, []string{"feature"}); err == nil {
		t.Fatal("global branch resolution ignored Git failure")
	}
}

func TestResolveBranchesWithFetchAdoptsFastForwardRemoteWithoutChangingSource(t *testing.T) {
	root := t.TempDir()
	origin := filepath.Join(root, "origin.git")
	git(t, root, "init", "--bare", origin)
	repoPath := initRepo(t, filepath.Join(root, "repo"))
	git(t, repoPath, "remote", "add", "origin", origin)
	git(t, repoPath, "push", "-u", "origin", "main")
	localOID := gitOut(t, repoPath, "rev-parse", "HEAD")
	if err := os.WriteFile(filepath.Join(repoPath, "tracked.txt"), []byte("remote\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	git(t, repoPath, "commit", "-am", "remote")
	remoteOID := gitOut(t, repoPath, "rev-parse", "HEAD")
	git(t, repoPath, "push", "origin", "main")
	git(t, repoPath, "reset", "--hard", localOID)
	git(t, repoPath, "update-ref", "refs/remotes/origin/main", localOID)

	repo := discovery.Repository{
		ID:            "repo",
		MainPath:      domain.CanonicalPath(repoPath),
		CommonDir:     domain.CanonicalPath(filepath.Join(repoPath, ".git")),
		RelativePath:  ".",
		DefaultBranch: "main",
	}
	warnings := []FetchWarning{}
	resolved, err := ResolveBranchesWithFetch(context.Background(), &gitx.Runner{}, discovery.Workspace{Repositories: []discovery.Repository{repo}}, nil, func(w FetchWarning) {
		warnings = append(warnings, w)
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(warnings) != 0 {
		t.Fatalf("warnings=%v, want none", warnings)
	}
	if len(resolved) != 1 || resolved[0].OID != remoteOID {
		t.Fatalf("resolved=%+v, want remote OID %s", resolved, remoteOID)
	}
	if got := gitOut(t, repoPath, "rev-parse", "HEAD"); got != localOID {
		t.Fatalf("source HEAD=%s, want unchanged %s", got, localOID)
	}
	if got := gitOut(t, repoPath, "rev-parse", "refs/heads/main"); got != localOID {
		t.Fatalf("source branch=%s, want unchanged %s", got, localOID)
	}
	if got := gitOut(t, repoPath, "status", "--porcelain", "--untracked-files=no"); got != "" {
		t.Fatalf("source index/worktree status=%q, want clean", got)
	}
	if got, err := os.ReadFile(filepath.Join(repoPath, "tracked.txt")); err != nil || string(got) != "base\n" {
		t.Fatalf("source tracked file=%q err=%v, want unchanged content", got, err)
	}
	if got := gitOut(t, repoPath, "rev-parse", "refs/remotes/origin/main"); got != remoteOID {
		t.Fatalf("origin/main=%s, want %s", got, remoteOID)
	}
	if _, err := os.Stat(filepath.Join(repoPath, ".git", "FETCH_HEAD")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("FETCH_HEAD err=%v, want it untouched", err)
	}
	git(t, repoPath, "update-ref", "refs/remotes/origin/main", localOID)
	if _, err := ResolveBranchesWithFetch(context.Background(), &gitx.Runner{}, discovery.Workspace{Repositories: []discovery.Repository{repo}}, []string{"main"}, nil); err != nil {
		t.Fatal(err)
	}
	if got := gitOut(t, repoPath, "rev-parse", "refs/remotes/origin/main"); got != localOID {
		t.Fatalf("explicit branch updated origin/main=%s, want unchanged %s", got, localOID)
	}
}

func TestResolveBranchesWithFetchFallsBackPerRepository(t *testing.T) {
	root := t.TempDir()
	a := initRepo(t, filepath.Join(root, "a"))
	b := initRepo(t, filepath.Join(root, "b"))
	repos := []discovery.Repository{
		{ID: "a", MainPath: domain.CanonicalPath(a), CommonDir: domain.CanonicalPath(filepath.Join(a, ".git")), RelativePath: "a", DefaultBranch: "main"},
		{ID: "b", MainPath: domain.CanonicalPath(b), CommonDir: domain.CanonicalPath(filepath.Join(b, ".git")), RelativePath: "b", DefaultBranch: "main"},
	}
	var warnings []FetchWarning
	resolved, err := ResolveBranchesWithFetch(context.Background(), &gitx.Runner{}, discovery.Workspace{Repositories: repos}, nil, func(w FetchWarning) {
		warnings = append(warnings, w)
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(resolved) != len(repos) {
		t.Fatalf("resolved=%+v", resolved)
	}
	if len(warnings) != len(repos) {
		t.Fatalf("warnings=%v, want one warning per repository", warnings)
	}
	for i, item := range resolved {
		if item.OID != gitOut(t, string(repos[i].MainPath), "rev-parse", "main") {
			t.Fatalf("resolved[%d]=%+v, want local OID", i, item)
		}
	}
}

func TestResolveBranchesWithFetchKeepsDivergedLocalBranch(t *testing.T) {
	root := t.TempDir()
	origin := filepath.Join(root, "origin.git")
	git(t, root, "init", "--bare", origin)
	repoPath := initRepo(t, filepath.Join(root, "repo"))
	git(t, repoPath, "remote", "add", "origin", origin)
	git(t, repoPath, "push", "-u", "origin", "main")
	git(t, repoPath, "commit", "--allow-empty", "-m", "local")
	localOID := gitOut(t, repoPath, "rev-parse", "HEAD")
	remotePath := filepath.Join(root, "remote")
	git(t, root, "clone", origin, remotePath)
	git(t, remotePath, "config", "user.name", "test")
	git(t, remotePath, "config", "user.email", "test@example.com")
	git(t, remotePath, "commit", "--allow-empty", "-m", "remote")
	remoteOID := gitOut(t, remotePath, "rev-parse", "HEAD")
	git(t, remotePath, "push", "origin", "main")

	repo := discovery.Repository{ID: "repo", MainPath: domain.CanonicalPath(repoPath), CommonDir: domain.CanonicalPath(filepath.Join(repoPath, ".git")), RelativePath: ".", DefaultBranch: "main"}
	var warnings []FetchWarning
	resolved, err := ResolveBranchesWithFetch(context.Background(), &gitx.Runner{}, discovery.Workspace{Repositories: []discovery.Repository{repo}}, nil, func(w FetchWarning) {
		warnings = append(warnings, w)
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(warnings) != 0 {
		t.Fatalf("warnings=%v, want none for a normal divergence", warnings)
	}
	if len(resolved) != 1 || resolved[0].OID != localOID {
		t.Fatalf("resolved=%+v, want local %s instead of remote %s", resolved, localOID, remoteOID)
	}
}

// TestResolveBranchesFailsClosedUnderACanceledContext は通常 context なら解決できる repository を使う。
// 不存在 repository を使わず、中断した context だけが失敗原因になることを確認する。
func TestResolveBranchesFailsClosedUnderACanceledContext(t *testing.T) {
	root := t.TempDir()
	main := initRepo(t, filepath.Join(root, "repository"))
	repo := discovery.Repository{ID: "repository", MainPath: domain.CanonicalPath(main), RelativePath: "repository", DefaultBranch: "main"}
	w := discovery.Workspace{Repositories: []discovery.Repository{repo}}
	runner := &gitx.Runner{}
	if _, err := ResolveBranches(context.Background(), runner, w, []string{"main"}); err != nil {
		t.Fatalf("global branch resolution on a live context: %v", err)
	}
	if _, err := ResolveBranches(context.Background(), runner, w, nil); err != nil {
		t.Fatalf("default branch resolution on a live context: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := ResolveBranches(ctx, runner, w, []string{"main"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled global branch resolution error=%v, want context.Canceled", err)
	}
	if _, err := ResolveBranches(ctx, runner, w, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled default branch resolution error=%v, want context.Canceled", err)
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
