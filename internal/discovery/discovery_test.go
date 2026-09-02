package discovery

import (
	"context"
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

func TestReadPatternsIgnoresWhitespaceAndComments(t *testing.T) {
	path := filepath.Join(t.TempDir(), "patterns")
	if err := os.WriteFile(path, []byte("\n# comment\n first \n\tsecond\t\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	patterns, err := ReadPatterns(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(patterns, ",") != "first,second" {
		t.Fatalf("patterns=%v", patterns)
	}
	if patterns, err := ReadPatterns(filepath.Join(t.TempDir(), "missing")); err != nil || patterns != nil {
		t.Fatalf("missing patterns=%v err=%v", patterns, err)
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

	loop := filepath.Join(root, "pattern-loop")
	if err := os.Symlink(loop, loop); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadPatterns(loop); err == nil {
		t.Fatal("pattern symlink loop was treated as a missing file")
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
