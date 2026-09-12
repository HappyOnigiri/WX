package workspace

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/HappyOnigiri/WX/internal/config"
	"github.com/HappyOnigiri/WX/internal/discovery"
	"github.com/HappyOnigiri/WX/internal/domain"
	"github.com/HappyOnigiri/WX/internal/gitx"
)

// tracked path を指す .worktreeinclude entry は無視され、worktree の内容は checkout が決める。
// main worktree 側だけ内容を変えることで、コピーが起きていないことを見分ける。
func TestWorktreeIncludeIgnoresTrackedPaths(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	repository := filepath.Join(root, "repository")
	worktreeRoot := filepath.Join(root, "worktrees")
	if err := os.Mkdir(repository, 0o700); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, repository, "init", "-b", "main")
	gitCommand(t, repository, "config", "user.name", "test")
	gitCommand(t, repository, "config", "user.email", "test@example.com")
	if err := os.WriteFile(filepath.Join(repository, "tracked"), []byte("base\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repository, ".worktreeinclude"), []byte("tracked\nuntracked\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, repository, "add", ".")
	gitCommand(t, repository, "commit", "-m", "initial")
	if err := os.WriteFile(filepath.Join(repository, "tracked"), []byte("modified\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repository, "untracked"), []byte("local\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	head := gitOutput(t, repository, "rev-parse", "HEAD")
	common := gitOutput(t, repository, "rev-parse", "--path-format=absolute", "--git-common-dir")
	repo := discovery.Repository{ID: "repo", MainPath: domain.CanonicalPath(repository), CommonDir: domain.CanonicalPath(common)}
	cfg := config.Defaults()
	cfg.Storage.WorktreeRoot = worktreeRoot
	if err := os.MkdirAll(worktreeRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	owner, _, err := domain.OpenOwnedRoot(worktreeRoot, worktreeRoot)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = owner.Close() }()
	slotPath := filepath.Join(worktreeRoot, testSlotRelPath)
	preparer := Preparer{Git: &gitx.Runner{Timeout: 5 * time.Second}, Config: cfg, Ownership: allowOwnershipValidator{}, OwnedRoot: owner, RootPath: worktreeRoot, SlotPath: slotPath, RootID: testRootID, SlotRelPath: testSlotRelPath}
	target := filepath.Join(slotPath, testRepositoryID)
	if err := preparer.Prepare(context.Background(), repo, target, head, "slot"); err != nil {
		t.Fatalf("tracked include was not ignored: %v", err)
	}
	if content, err := os.ReadFile(filepath.Join(target, "tracked")); err != nil || string(content) != "base\n" {
		t.Fatalf("tracked include overwrote the checkout: content=%q err=%v", content, err)
	}
	// 同じ manifest の untracked entry は従来どおりコピーされ、tracked entry の無視が manifest 全体を止めていないことを示す。
	if content, err := os.ReadFile(filepath.Join(target, "untracked")); err != nil || string(content) != "local\n" {
		t.Fatalf("untracked include was not copied: content=%q err=%v", content, err)
	}
}

// directory include は tracked file の存在だけで省略せず、未追跡 file だけを materialize する。
func TestWorktreeIncludeCopiesUntrackedFilesInsideTrackedDirectory(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	repository := filepath.Join(root, "repository")
	target := filepath.Join(root, "target")
	if err := os.MkdirAll(filepath.Join(repository, "settings"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(target, "settings"), 0o700); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, repository, "init", "-b", "main")
	if err := os.WriteFile(filepath.Join(repository, "settings", "tracked"), []byte("source\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repository, "settings", "local"), []byte("local\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repository, ".worktreeinclude"), []byte("settings\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, repository, "add", "settings/tracked")
	if err := os.WriteFile(filepath.Join(target, "settings", "tracked"), []byte("checkout\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	owner, err := os.OpenRoot(target)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = owner.Close() }()
	repo := discovery.Repository{MainPath: domain.CanonicalPath(repository)}
	preparer := Preparer{Git: &gitx.Runner{Timeout: 5 * time.Second}, Config: config.Defaults()}
	if err := preparer.copyIncludesAt(repo, owner, "."); err != nil {
		t.Fatalf("directory include copy: %v", err)
	}
	if content, err := os.ReadFile(filepath.Join(target, "settings", "tracked")); err != nil || string(content) != "checkout\n" {
		t.Fatalf("tracked include was overwritten: content=%q err=%v", content, err)
	}
	if content, err := os.ReadFile(filepath.Join(target, "settings", "local")); err != nil || string(content) != "local\n" {
		t.Fatalf("untracked include was not copied: content=%q err=%v", content, err)
	}
}

func TestWorktreeIncludeReturnsTrackedCheckGitErrors(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	repository := filepath.Join(root, "repository")
	target := filepath.Join(root, "target")
	if err := os.MkdirAll(filepath.Join(repository, "settings"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repository, "settings", "local"), []byte("local\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repository, ".worktreeinclude"), []byte("settings\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	owner, err := os.OpenRoot(target)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = owner.Close() }()
	repo := discovery.Repository{MainPath: domain.CanonicalPath(repository)}
	preparer := Preparer{Git: &gitx.Runner{Timeout: 5 * time.Second}, Config: config.Defaults()}
	if err := preparer.copyIncludesAt(repo, owner, "."); err == nil || !strings.Contains(err.Error(), "list tracked includes") {
		t.Fatalf("tracked check Git failure was not returned: %v", err)
	}
	if _, err := os.Stat(filepath.Join(target, "settings", "local")); !os.IsNotExist(err) {
		t.Fatalf("include was copied despite tracked check failure: %v", err)
	}
}

func TestDefaultIncludesCarryUntrackedRuleFilesWithoutAManifest(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	root := filepath.Join(base, "worktrees")
	repository := filepath.Join(base, "repository")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(repository, 0o700); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, repository, "init", "-b", "main")
	gitCommand(t, repository, "config", "user.name", "test")
	gitCommand(t, repository, "config", "user.email", "test@example.com")
	// 未追跡のルールファイル。デフォルトが用意されているケース。
	if err := os.WriteFile(filepath.Join(repository, "CLAUDE.local.md"), []byte("local rules\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repository, ".mcp.json"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// デフォルト名の追跡済みファイルは checkout に任せ、エラーにしない。
	if err := os.WriteFile(filepath.Join(repository, ".cursorrules"), []byte("shared\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// ディレクトリと symlink は明示的な manifest 項目の責務として残す。
	if err := os.Mkdir(filepath.Join(repository, ".clinerules"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repository, ".clinerules", "style.md"), []byte("style\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, "shared-override.md"), []byte("shared override\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(base, "shared-override.md"), filepath.Join(repository, "AGENTS.override.md")); err != nil {
		t.Fatal(err)
	}
	// 明示的な link ルールがパスを所有するため、デフォルトで先にコピーして
	// createLinks を衝突させてはならない。
	if err := os.WriteFile(filepath.Join(repository, ".geminiignore"), []byte("vendor\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repository, ".worktreelink"), []byte(".geminiignore\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repository, ".gitignore"), []byte("CLAUDE.local.md\n.mcp.json\n.clinerules\nAGENTS.override.md\n.geminiignore\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, repository, "add", ".gitignore", ".cursorrules", ".worktreelink")
	gitCommand(t, repository, "commit", "-m", "initial")
	target := filepath.Join(root, "slot", "root")
	if err := os.MkdirAll(target, 0o700); err != nil {
		t.Fatal(err)
	}
	owner, _, err := domain.OpenOwnedRoot(root, root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = owner.Close() }()
	cfg := config.Defaults()
	cfg.Storage.WorktreeRoot = root
	preparer := Preparer{Git: &gitx.Runner{Timeout: 5 * time.Second}, Config: cfg, OwnedRoot: owner, RootPath: root}
	repo := discovery.Repository{MainPath: domain.CanonicalPath(repository)}
	disabledCfg := cfg
	disabledCfg.Includes.DefaultAgentRules = false
	disabledPreparer := preparer
	disabledPreparer.Config = disabledCfg
	disabledTarget := filepath.Join(root, "slot-disabled", "root")
	if err := os.MkdirAll(disabledTarget, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := disabledPreparer.copyIncludes(repo, disabledTarget); err != nil {
		t.Fatalf("disabled default include copy: %v", err)
	}
	for _, name := range []string{"CLAUDE.local.md", ".mcp.json"} {
		if _, err := os.Lstat(filepath.Join(disabledTarget, name)); !os.IsNotExist(err) {
			t.Fatalf("%s was materialized while defaults were disabled: %v", name, err)
		}
	}
	enabled := true
	disabledCfg.Repositories[string(repo.MainPath)] = config.Repository{Includes: config.RepositoryIncludes{DefaultAgentRules: &enabled}}
	overriddenPreparer := preparer
	overriddenPreparer.Config = disabledCfg
	overriddenTarget := filepath.Join(root, "slot-overridden", "root")
	if err := os.MkdirAll(overriddenTarget, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := overriddenPreparer.copyIncludes(repo, overriddenTarget); err != nil {
		t.Fatalf("repository-enabled default include copy: %v", err)
	}
	for _, name := range []string{"CLAUDE.local.md", ".mcp.json"} {
		if _, err := os.Lstat(filepath.Join(overriddenTarget, name)); err != nil {
			t.Fatalf("%s was not materialized by the repository override: %v", name, err)
		}
	}
	disabled := false
	enabledCfg := cfg
	enabledCfg.Repositories[string(repo.MainPath)] = config.Repository{Includes: config.RepositoryIncludes{DefaultAgentRules: &disabled}}
	repositoryDisabledPreparer := preparer
	repositoryDisabledPreparer.Config = enabledCfg
	repositoryDisabledTarget := filepath.Join(root, "slot-repository-disabled", "root")
	if err := os.MkdirAll(repositoryDisabledTarget, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := repositoryDisabledPreparer.copyIncludes(repo, repositoryDisabledTarget); err != nil {
		t.Fatalf("repository-disabled default include copy: %v", err)
	}
	for _, name := range []string{"CLAUDE.local.md", ".mcp.json"} {
		if _, err := os.Lstat(filepath.Join(repositoryDisabledTarget, name)); !os.IsNotExist(err) {
			t.Fatalf("%s was materialized despite the repository override: %v", name, err)
		}
	}
	delete(cfg.Repositories, string(repo.MainPath))
	if err := preparer.copyIncludes(repo, target); err != nil {
		t.Fatalf("default include copy: %v", err)
	}
	for name, want := range map[string]string{"CLAUDE.local.md": "local rules\n", ".mcp.json": "{}\n"} {
		data, err := os.ReadFile(filepath.Join(target, name))
		if err != nil || string(data) != want {
			t.Fatalf("default include %s=%q err=%v", name, data, err)
		}
	}
	for _, name := range []string{".cursorrules", ".clinerules", "AGENTS.override.md", ".geminiignore"} {
		if _, err := os.Lstat(filepath.Join(target, name)); !os.IsNotExist(err) {
			t.Fatalf("%s was materialized by the defaults: %v", name, err)
		}
	}
	if err := preparer.createLinks(context.Background(), repo, target); err != nil {
		t.Fatalf("link after defaults: %v", err)
	}
	link, err := os.Readlink(filepath.Join(target, ".geminiignore"))
	if err != nil || link != filepath.Join(repository, ".geminiignore") {
		t.Fatalf("linked default=%q err=%v", link, err)
	}
}
