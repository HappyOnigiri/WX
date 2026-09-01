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
