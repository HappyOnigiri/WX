package workspace

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
)

func TestMaterializeRootCopiesLinksAndIsIdempotent(t *testing.T) {
	source, target := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(source, "AGENTS.md"), []byte("rules\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(source, "docs", "nested"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "docs", "nested", "note"), []byte("note\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(source, "shared"), 0o700); err != nil {
		t.Fatal(err)
	}
	rules := config.Workspace{Copy: []string{"docs", "docs"}, Link: []string{"shared"}}
	if err := MaterializeRoot(source, target, rules); err != nil {
		t.Fatal(err)
	}
	if err := MaterializeRoot(source, target, rules); err != nil {
		t.Fatalf("idempotent materialization: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(target, "docs", "nested", "note"))
	if err != nil || string(data) != "note\n" {
		t.Fatalf("copied data=%q err=%v", data, err)
	}
	link, err := os.Readlink(filepath.Join(target, "shared"))
	if err != nil || link != filepath.Join(source, "shared") {
		t.Fatalf("link=%q err=%v", link, err)
	}
}

func TestWorkspacePathValidationAndCollisionsFailClosed(t *testing.T) {
	for _, path := range []string{"", "../outside", "/absolute"} {
		if _, err := safeRelative(path); err == nil {
			t.Fatalf("safeRelative(%q) succeeded", path)
		}
	}
	source, target := t.TempDir(), t.TempDir()
	if err := os.Mkdir(filepath.Join(source, "shared"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "shared"), []byte("collision"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := MaterializeRoot(source, target, config.Workspace{Link: []string{"shared"}}); err == nil {
		t.Fatal("link collision succeeded")
	}
	if err := MaterializeRoot(source, target, config.Workspace{Copy: []string{"../outside"}}); err == nil {
		t.Fatal("unsafe copy succeeded")
	}
	linkSource := filepath.Join(source, "link")
	if err := os.Symlink(filepath.Join(source, "shared"), linkSource); err != nil {
		t.Fatal(err)
	}
	if err := copyPath(linkSource, filepath.Join(target, "copy")); err == nil {
		t.Fatal("copyPath followed a symlink")
	}
	regular := filepath.Join(source, "regular")
	if err := os.WriteFile(regular, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(target, "directory"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := copyPath(regular, filepath.Join(target, "directory")); err == nil {
		t.Fatal("copyPath overwrote a directory")
	}
}

func TestPrepareCommandSuccessFailureAndTimeout(t *testing.T) {
	repository, target := t.TempDir(), t.TempDir()
	repo := discovery.Repository{MainPath: domain.CanonicalPath(repository)}
	cfg := config.Defaults()
	cfg.Repositories = map[string]config.Repository{repository: {Prepare: config.Prepare{Command: []string{"/bin/sh", "-c", "printf ready > marker"}, Timeout: config.Duration{Duration: time.Second}}}}
	preparer := Preparer{Config: cfg}
	if err := preparer.runPrepare(context.Background(), repo, target); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(filepath.Join(target, "marker")); err != nil || string(data) != "ready" {
		t.Fatalf("marker=%q err=%v", data, err)
	}
	cfg.Repositories[repository] = config.Repository{Prepare: config.Prepare{Command: []string{"/bin/sh", "-c", "exit 7"}, Timeout: config.Duration{Duration: time.Second}}}
	preparer.Config = cfg
	if err := preparer.runPrepare(context.Background(), repo, target); err == nil {
		t.Fatal("failed prepare command succeeded")
	}
	cfg.Repositories[repository] = config.Repository{Prepare: config.Prepare{Command: []string{"/bin/sh", "-c", "sleep 2"}, Timeout: config.Duration{Duration: 20 * time.Millisecond}}}
	preparer.Config = cfg
	if err := preparer.runPrepare(context.Background(), repo, target); err == nil {
		t.Fatal("timed out prepare command succeeded")
	}
}

func TestPrepareRejectsPathsOutsideRootSymlinksAndForeignContents(t *testing.T) {
	root := t.TempDir()
	cfg := config.Defaults()
	cfg.Storage.WorktreeRoot = root
	preparer := Preparer{Git: &gitx.Runner{Timeout: time.Second}, Config: cfg}
	repo := discovery.Repository{ID: "repo", MainPath: domain.CanonicalPath(t.TempDir()), CommonDir: domain.CanonicalPath(filepath.Join(t.TempDir(), ".git"))}
	if err := preparer.Prepare(context.Background(), repo, filepath.Join(t.TempDir(), "outside"), "oid", "slot"); err == nil {
		t.Fatal("outside target succeeded")
	}
	target := filepath.Join(root, "target")
	if err := os.Symlink(t.TempDir(), target); err != nil {
		t.Fatal(err)
	}
	if err := preparer.Prepare(context.Background(), repo, target, "oid", "slot"); err == nil {
		t.Fatal("symlink target succeeded")
	}
	if err := os.Remove(target); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "foreign"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := preparer.Prepare(context.Background(), repo, target, "oid", "slot"); err == nil || !strings.Contains(err.Error(), "not the expected worktree") {
		t.Fatalf("foreign target error=%v", err)
	}
}

func TestIncludeAndLinkPoliciesRejectUnsafeInputs(t *testing.T) {
	repository, target := t.TempDir(), t.TempDir()
	gitCommand(t, repository, "init", "-b", "main")
	gitCommand(t, repository, "config", "user.name", "test")
	gitCommand(t, repository, "config", "user.email", "test@example.com")
	if err := os.WriteFile(filepath.Join(repository, ".worktreeinclude"), []byte("../outside\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	preparer := Preparer{Git: &gitx.Runner{Timeout: time.Second}, Config: config.Defaults()}
	repo := discovery.Repository{MainPath: domain.CanonicalPath(repository)}
	if err := preparer.copyIncludes(repo, target); err == nil || !strings.Contains(err.Error(), "unsafe") {
		t.Fatalf("unsafe include error=%v", err)
	}
	if err := os.WriteFile(filepath.Join(repository, ".worktreelink"), []byte("not-ignored\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := preparer.createLinks(context.Background(), repo, target); err == nil || !strings.Contains(err.Error(), "not ignored") {
		t.Fatalf("unignored link error=%v", err)
	}
	if err := os.WriteFile(filepath.Join(repository, ".worktreelink"), []byte("../outside\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := preparer.createLinks(context.Background(), repo, target); err == nil || !strings.Contains(err.Error(), "unsafe") {
		t.Fatalf("unsafe link error=%v", err)
	}
}

func TestReadyValidationAndMaterializationEdgeCases(t *testing.T) {
	root := t.TempDir()
	repository := filepath.Join(root, "repository")
	worktreeRoot := filepath.Join(root, "worktrees")
	if err := os.MkdirAll(repository, 0o700); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, repository, "init", "-b", "main")
	gitCommand(t, repository, "config", "user.name", "test")
	gitCommand(t, repository, "config", "user.email", "test@example.com")
	if err := os.WriteFile(filepath.Join(repository, ".gitignore"), []byte("shared/\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repository, "tracked"), []byte("base\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, repository, "add", ".")
	gitCommand(t, repository, "commit", "-m", "initial")
	head := gitOutput(t, repository, "rev-parse", "HEAD")
	common := gitOutput(t, repository, "rev-parse", "--path-format=absolute", "--git-common-dir")
	repo := discovery.Repository{ID: "repo", MainPath: domain.CanonicalPath(repository), CommonDir: domain.CanonicalPath(common)}
	cfg := config.Defaults()
	cfg.Storage.WorktreeRoot = worktreeRoot
	preparer := Preparer{Git: &gitx.Runner{Timeout: 5 * time.Second}, Config: cfg}
	target := filepath.Join(worktreeRoot, "slot", "root")
	if err := preparer.Prepare(context.Background(), repo, target, head, "slot"); err != nil {
		t.Fatal(err)
	}
	if err := preparer.ValidateReady(context.Background(), repo, target, head); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, repository, "worktree", "unlock", target)
	if err := preparer.ValidateReady(context.Background(), repo, target, head); err == nil || !strings.Contains(err.Error(), "lock") {
		t.Fatalf("unlocked READY validation error=%v", err)
	}
	gitCommand(t, repository, "worktree", "lock", target)
	if err := preparer.validateExistingWorktree(context.Background(), repo, target, strings.Repeat("0", 40)); err == nil {
		t.Fatal("unexpected HEAD passed existing worktree validation")
	}

	if err := os.Mkdir(filepath.Join(repository, "shared"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repository, ".worktreelink"), []byte("shared\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := preparer.createLinks(context.Background(), repo, target); err != nil {
		t.Fatal(err)
	}
	if err := preparer.createLinks(context.Background(), repo, target); err != nil {
		t.Fatalf("idempotent link: %v", err)
	}
	if err := os.Remove(filepath.Join(target, "shared")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "shared"), []byte("collision"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := preparer.createLinks(context.Background(), repo, target); err == nil || !strings.Contains(err.Error(), "collision") {
		t.Fatalf("link collision error=%v", err)
	}

	brokenSource, materialized := t.TempDir(), t.TempDir()
	if err := MaterializeRoot(brokenSource, materialized, config.Workspace{Link: []string{"missing"}}); err == nil {
		t.Fatal("missing root link source succeeded")
	}
	if err := os.WriteFile(filepath.Join(brokenSource, "AGENTS.local.md"), []byte("rules"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(brokenSource, "AGENTS.local.md"), filepath.Join(materialized, "AGENTS.local.md")); err != nil {
		t.Fatal(err)
	}
	if err := MaterializeRoot(brokenSource, materialized, config.Workspace{}); err == nil {
		t.Fatal("root copy overwrote destination symlink")
	}
	if _, err := Fingerprint(1, head, discovery.Repository{MainPath: domain.CanonicalPath(brokenSource)}, cfg); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(brokenSource, ".worktreeinclude"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := Fingerprint(1, head, discovery.Repository{MainPath: domain.CanonicalPath(brokenSource)}, cfg); err == nil {
		t.Fatal("unreadable fingerprint input succeeded")
	}
}

func gitOutput(t *testing.T, directory string, args ...string) string {
	t.Helper()
	command := exec.Command("git", args...)
	command.Dir = directory
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, output)
	}
	return strings.TrimSpace(string(output))
}

func gitCommand(t *testing.T, directory string, args ...string) {
	t.Helper()
	command := exec.Command("git", args...)
	command.Dir = directory
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, output)
	}
}
