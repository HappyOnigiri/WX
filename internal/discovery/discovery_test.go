package discovery

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/HappyOnigiri/WX/internal/config"
	"github.com/HappyOnigiri/WX/internal/domain"
	"github.com/HappyOnigiri/WX/internal/gitx"
)

func TestResolveRepositoryUsesMainWorktreeAndConfiguredBranch(t *testing.T) {
	repository := filepath.Join(t.TempDir(), "repository")
	initDiscoveryRepository(t, repository)
	nested := filepath.Join(repository, "nested")
	if err := os.Mkdir(nested, 0o700); err != nil {
		t.Fatal(err)
	}
	mainPath, err := domain.Canonicalize(repository)
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Defaults()
	cfg.Repositories[string(mainPath)] = config.Repository{DefaultBranch: "trunk"}
	discoverer := Discoverer{Git: &gitx.Runner{Timeout: 5 * time.Second}, Config: cfg}
	workspace, err := discoverer.Resolve(context.Background(), nested)
	if err != nil {
		t.Fatal(err)
	}
	if workspace.Kind != "repository" || workspace.Root != mainPath || len(workspace.Repositories) != 1 {
		t.Fatalf("workspace=%+v", workspace)
	}
	repositoryState := workspace.Repositories[0]
	if repositoryState.MainPath != mainPath || repositoryState.RelativePath != "." || repositoryState.DefaultBranch != "trunk" || repositoryState.CommonDir == "" {
		t.Fatalf("repository=%+v", repositoryState)
	}
}

func TestResolveMultiRepositoryHonorsExclusionsDepthAndWorktreeRoot(t *testing.T) {
	root := t.TempDir()
	included := filepath.Join(root, "group", "included")
	excluded := filepath.Join(root, "excluded")
	tooDeep := filepath.Join(root, "a", "b", "too-deep")
	worktreeRoot := filepath.Join(root, "managed-worktrees")
	for _, path := range []string{included, excluded, tooDeep, worktreeRoot} {
		initDiscoveryRepository(t, path)
	}
	if err := os.Symlink(included, filepath.Join(root, "repository-link")); err != nil {
		t.Fatal(err)
	}
	cfg := config.Defaults()
	cfg.Storage.WorktreeRoot = worktreeRoot
	cfg.Discovery.Exclude = []string{"excluded"}
	cfg.Discovery.MaxDepth = 2
	cfg.Discovery.MaxEntries = 100
	cfg.Discovery.Timeout.Duration = time.Second
	discoverer := Discoverer{Git: &gitx.Runner{Timeout: 5 * time.Second}, Config: cfg}
	workspace, err := discoverer.Resolve(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if workspace.Kind != "multi_repository" || len(workspace.Repositories) != 1 {
		t.Fatalf("workspace=%+v", workspace)
	}
	if got := workspace.Repositories[0].RelativePath; got != filepath.Join("group", "included") {
		t.Fatalf("relative path=%q", got)
	}
}

func TestResolveMultiRepositoryFailsClosedOnLimitsAndMissingRepositories(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "one", "two"), 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := config.Defaults()
	cfg.Storage.WorktreeRoot = filepath.Join(t.TempDir(), "worktrees")
	cfg.Discovery.MaxEntries = 1
	cfg.Discovery.Timeout.Duration = time.Second
	discoverer := Discoverer{Git: &gitx.Runner{Timeout: time.Second}, Config: cfg}
	if _, err := discoverer.Resolve(context.Background(), root); err == nil || !strings.Contains(err.Error(), "max_entries") {
		t.Fatalf("entry-limit error=%v", err)
	}

	cfg.Discovery.MaxEntries = 100
	discoverer.Config = cfg
	if _, err := discoverer.Resolve(context.Background(), root); err == nil || !strings.Contains(err.Error(), "no Git repositories") {
		t.Fatalf("empty discovery error=%v", err)
	}
	if _, err := discoverer.Resolve(context.Background(), filepath.Join(root, "missing")); err == nil {
		t.Fatal("missing discovery root succeeded")
	}
}

func TestInspectRepositoryFailsClosedAtGitMetadataBoundaries(t *testing.T) {
	root := t.TempDir()
	main := filepath.Join(root, "main")
	common := filepath.Join(main, ".git")
	if err := os.MkdirAll(common, 0o700); err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	fakeGit := filepath.Join(bin, "git")
	script := `#!/bin/sh
case "$WX_DISCOVERY_MODE" in
  worktree-fail) exit 1 ;;
  empty) exit 0 ;;
esac
if [ "$1 $2" = "worktree list" ]; then
  printf 'worktree %s\000' "$WX_DISCOVERY_MAIN"
  exit 0
fi
if [ "$WX_DISCOVERY_MODE" = "common-fail" ]; then
  exit 1
fi
printf '%s\n' "$WX_DISCOVERY_COMMON"
`
	if err := os.WriteFile(fakeGit, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	t.Setenv("WX_DISCOVERY_MAIN", main)
	t.Setenv("WX_DISCOVERY_COMMON", common)
	discoverer := Discoverer{Git: &gitx.Runner{Timeout: time.Second}, Config: config.Defaults()}

	t.Setenv("WX_DISCOVERY_MODE", "worktree-fail")
	if _, err := discoverer.repositoryWorkspace(context.Background(), main); err == nil {
		t.Fatal("repository workspace ignored worktree-list failure")
	}
	for _, test := range []struct {
		name, mode, mainPath, commonPath string
	}{
		{name: "worktree command", mode: "worktree-fail", mainPath: main, commonPath: common},
		{name: "missing main record", mode: "empty", mainPath: main, commonPath: common},
		{name: "missing main path", mainPath: filepath.Join(root, "missing-main"), commonPath: common},
		{name: "common command", mode: "common-fail", mainPath: main, commonPath: common},
		{name: "missing common path", mainPath: main, commonPath: filepath.Join(root, "missing-common")},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("WX_DISCOVERY_MODE", test.mode)
			t.Setenv("WX_DISCOVERY_MAIN", test.mainPath)
			t.Setenv("WX_DISCOVERY_COMMON", test.commonPath)
			if _, err := discoverer.inspectRepo(context.Background(), main, "."); err == nil {
				t.Fatal("invalid Git metadata was accepted")
			}
		})
	}
}

func TestMultiRepositoryDiscoveryPropagatesTraversalContextAndRepositoryErrors(t *testing.T) {
	cfg := config.Defaults()
	cfg.Discovery.Timeout.Duration = time.Second
	discoverer := Discoverer{Git: &gitx.Runner{Timeout: time.Second}, Config: cfg}
	root := t.TempDir()

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := discoverer.multiWorkspace(canceled, root); err == nil {
		t.Fatal("canceled discovery succeeded")
	}
	if _, err := discoverer.multiWorkspace(context.Background(), filepath.Join(root, "missing")); err == nil {
		t.Fatal("missing discovery root succeeded")
	}

	repository := filepath.Join(root, "repository")
	if err := os.MkdirAll(filepath.Join(repository, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte("#!/bin/sh\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	if _, err := discoverer.multiWorkspace(context.Background(), root); err == nil {
		t.Fatal("repository inspection failure was ignored")
	}
}

func initDiscoveryRepository(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"init", "-b", "main"},
		{"config", "user.name", "wx test"},
		{"config", "user.email", "wx@example.invalid"},
	} {
		runDiscoveryGit(t, path, args...)
	}
	if err := os.WriteFile(filepath.Join(path, "tracked"), []byte("base\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runDiscoveryGit(t, path, "add", "tracked")
	runDiscoveryGit(t, path, "commit", "-m", "initial")
}

func runDiscoveryGit(t *testing.T, directory string, args ...string) {
	t.Helper()
	command := exec.Command("git", args...)
	command.Dir = directory
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, output)
	}
}

func TestResolveMultiRepositoryCollapsesLinkedWorktreesOntoTheMainWorktree(t *testing.T) {
	root := t.TempDir()
	main := filepath.Join(root, "server")
	initDiscoveryRepository(t, main)
	// 同じ repository の二つの linked worktree を workspace root 内で sibling として置く。
	// 三つの entry は Git common directory を共有するため、inspectRepo は同じ repository ID を導く。
	for _, name := range []string{"server-feature", "server-hotfix"} {
		runDiscoveryGit(t, main, "worktree", "add", "--detach", filepath.Join(root, name))
	}
	other := filepath.Join(root, "client")
	initDiscoveryRepository(t, other)

	cfg := config.Defaults()
	cfg.Storage.WorktreeRoot = filepath.Join(t.TempDir(), "worktrees")
	cfg.Discovery.Timeout.Duration = 30 * time.Second
	discoverer := Discoverer{Git: &gitx.Runner{Timeout: 30 * time.Second}, Config: cfg}
	workspace, err := discoverer.Resolve(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if workspace.Kind != "multi_repository" {
		t.Fatalf("kind=%q", workspace.Kind)
	}
	if len(workspace.Repositories) != 2 {
		var located []string
		for _, repo := range workspace.Repositories {
			located = append(located, repo.RelativePath)
		}
		t.Fatalf("repositories=%v; duplicates of one common directory were not collapsed", located)
	}
	byID := map[domain.RepositoryID]Repository{}
	for _, repo := range workspace.Repositories {
		if existing, duplicate := byID[repo.ID]; duplicate {
			t.Fatalf("repository %s enumerated twice (%q and %q)", repo.ID, existing.RelativePath, repo.RelativePath)
		}
		byID[repo.ID] = repo
	}
	// 残る entry は main worktree でなければならない。
	// state.validateWorkspaceRepositoryAssociation は workspace_root + relative_path が repositories.main_worktree_path と一致することを証明する。
	for _, repo := range workspace.Repositories {
		located := filepath.Join(string(workspace.Root), repo.RelativePath)
		canonical, canonicalErr := domain.Canonicalize(located)
		if canonicalErr != nil {
			t.Fatal(canonicalErr)
		}
		if canonical != repo.MainPath {
			t.Fatalf("repository %s kept relative path %q which resolves to %s, not its main worktree %s", repo.ID, repo.RelativePath, canonical, repo.MainPath)
		}
	}
}

func TestResolveMultiRepositoryRefusesRepositoryVisibleOnlyThroughALinkedWorktree(t *testing.T) {
	outside := t.TempDir()
	main := filepath.Join(outside, "server")
	initDiscoveryRepository(t, main)
	root := t.TempDir()
	runDiscoveryGit(t, main, "worktree", "add", "--detach", filepath.Join(root, "server-feature"))

	cfg := config.Defaults()
	cfg.Storage.WorktreeRoot = filepath.Join(t.TempDir(), "worktrees")
	cfg.Discovery.Timeout.Duration = 30 * time.Second
	discoverer := Discoverer{Git: &gitx.Runner{Timeout: 30 * time.Second}, Config: cfg}
	_, err := discoverer.Resolve(context.Background(), root)
	if err == nil || !strings.Contains(err.Error(), "linked worktree") {
		t.Fatalf("error=%v; a repository whose main worktree is outside the workspace must fail closed", err)
	}
}

func TestInspectRepositoryReadsOriginRemoteName(t *testing.T) {
	repository := filepath.Join(t.TempDir(), "checkout")
	initDiscoveryRepository(t, repository)
	discoverer := Discoverer{Git: &gitx.Runner{Timeout: 30 * time.Second}, Config: config.Defaults()}

	without, err := discoverer.inspectRepo(context.Background(), repository, ".")
	if err != nil {
		t.Fatal(err)
	}
	if without.RemoteName != "" {
		t.Fatalf("remote name without an origin=%q", without.RemoteName)
	}

	runDiscoveryGit(t, repository, "remote", "add", "origin", "https://github.com/HappyOnigiri/WX.git")
	with, err := discoverer.inspectRepo(context.Background(), repository, ".")
	if err != nil {
		t.Fatal(err)
	}
	if with.RemoteName != "WX" {
		t.Fatalf("remote name=%q", with.RemoteName)
	}
}

func TestRemoteBaseNameReducesRemoteURLForms(t *testing.T) {
	for _, test := range []struct{ url, want string }{
		{url: "https://github.com/HappyOnigiri/WX.git", want: "WX"},
		{url: "https://github.com/HappyOnigiri/WX", want: "WX"},
		{url: "  https://github.com/HappyOnigiri/WX/  ", want: "WX"},
		{url: "git@github.com:HappyOnigiri/WX.git", want: "WX"},
		{url: "ssh://git@example.invalid/deep/path/name.git", want: "name"},
		{url: "/srv/git/bare-repo.git", want: "bare-repo"},
		{url: `C:\repos\windows-style.git`, want: "windows-style"},
		{url: "", want: ""},
		{url: "   ", want: ""},
		{url: "https://example.invalid/.git", want: ""},
		{url: "/", want: ""},
	} {
		if got := RemoteBaseName(test.url); got != test.want {
			t.Errorf("RemoteBaseName(%q)=%q want %q", test.url, got, test.want)
		}
	}
}

func TestWorkspaceIDsAreFreshShortIdentifiers(t *testing.T) {
	repository := filepath.Join(t.TempDir(), "repository")
	initDiscoveryRepository(t, repository)
	cfg := config.Defaults()
	cfg.Storage.WorktreeRoot = filepath.Join(t.TempDir(), "worktrees")
	cfg.Discovery.Timeout.Duration = 30 * time.Second
	discoverer := Discoverer{Git: &gitx.Runner{Timeout: 30 * time.Second}, Config: cfg}

	first, err := discoverer.Resolve(context.Background(), repository)
	if err != nil {
		t.Fatal(err)
	}
	second, err := discoverer.Resolve(context.Background(), repository)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []domain.WorkspaceID{first.ID, second.ID} {
		if !domain.ValidShortID(string(id)) {
			t.Fatalf("workspace id=%q is not a short identifier", id)
		}
	}
	// identity は discovery ではなく internal/state が解決する。
	// 同じ repository を二度探索しても、候補 ID は意図的に異なる。
	if first.ID == second.ID {
		t.Fatalf("two discovery passes proposed the same id %q; the proposal must be random", first.ID)
	}
}

func TestPolicyRootSharesRepositoryAndLinkedWorktreeSelection(t *testing.T) {
	root := t.TempDir()
	main := filepath.Join(root, "main")
	initDiscoveryRepository(t, main)
	sub := filepath.Join(main, "sub")
	if err := os.Mkdir(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	linked := filepath.Join(root, "linked")
	runDiscoveryGit(t, main, "worktree", "add", "--detach", linked)
	alias := filepath.Join(root, "alias")
	if err := os.Symlink(sub, alias); err != nil {
		t.Fatal(err)
	}
	d := Discoverer{Git: &gitx.Runner{Timeout: 5 * time.Second}, Config: config.Defaults()}
	for _, cwd := range []string{main, sub, linked, alias} {
		got, err := d.PolicyRoot(context.Background(), cwd)
		if err != nil || got != main {
			t.Fatalf("cwd=%s root=%s err=%v", cwd, got, err)
		}
	}
	if got, err := d.PolicyRoot(context.Background(), root); err != nil || got != root {
		t.Fatalf("bundle=%s err=%v", got, err)
	}
	if _, err := d.PolicyRoot(context.Background(), filepath.Join(root, "missing")); err == nil {
		t.Fatal("missing path accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := d.PolicyRoot(ctx, root); err == nil {
		t.Fatal("cancellation ignored")
	}
}

func TestPolicyRootRejectsGitExecutionFailureInsteadOfUsingCWD(t *testing.T) {
	root, bin := t.TempDir(), t.TempDir()
	fakeGit := filepath.Join(bin, "git")
	if err := os.WriteFile(fakeGit, []byte("#!/bin/sh\nprintf '%s\\n' 'fatal: cannot read configuration' >&2\nexit 128\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	d := Discoverer{Git: &gitx.Runner{}, Config: config.Defaults()}
	got, err := d.PolicyRoot(context.Background(), root)
	if err == nil {
		t.Fatalf("root=%q; Git execution failure was treated as a non-repository", got)
	}
	if got != "" || errors.Is(err, gitx.ErrNotRepository) || !strings.Contains(err.Error(), root) {
		t.Fatalf("root=%q err=%v", got, err)
	}
}

func TestResolveRejectsGitExecutionFailureBeforeMultiRepositoryFallback(t *testing.T) {
	root, bin := t.TempDir(), t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "nested"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "nested", ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	fakeGit := filepath.Join(bin, "git")
	if err := os.WriteFile(fakeGit, []byte("#!/bin/sh\nprintf '%s\\n' 'fatal: cannot read configuration' >&2\nexit 128\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	d := Discoverer{Git: &gitx.Runner{}, Config: config.Defaults()}
	if _, err := d.Resolve(context.Background(), root); err == nil || errors.Is(err, gitx.ErrNotRepository) {
		t.Fatalf("Git execution failure was accepted as a multi-repository workspace: %v", err)
	}
}
