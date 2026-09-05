package workspace

import (
	"context"
	"crypto/sha256"
	"errors"
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
	"github.com/HappyOnigiri/WX/internal/state"
)

type allowOwnershipValidator struct{}

func (allowOwnershipValidator) ValidateWorktreeOwnership(context.Context, state.WorktreeOwnershipRequest) (state.WorktreeOwnership, error) {
	return state.WorktreeOwnership{}, nil
}

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

func TestWorkspaceRootDefaultSymlinkRuleIsSkipped(t *testing.T) {
	source, target := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(source, "CLAUDE.md"), []byte("rules\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	repository := discovery.Repository{MainPath: domain.CanonicalPath(source)}
	before, err := Fingerprint(1, "oid", repository, config.Defaults())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("CLAUDE.md", filepath.Join(source, "AGENTS.md")); err != nil {
		t.Fatal(err)
	}
	after, err := Fingerprint(1, "oid", repository, config.Defaults())
	if err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Fatalf("default symlink changed fingerprint before=%s after=%s", before, after)
	}
	if err := MaterializeRoot(source, target, config.Workspace{}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(target, "AGENTS.md")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("default symlink materialized: %v", err)
	}
	if err := MaterializeRoot(source, t.TempDir(), config.Workspace{Copy: []string{"AGENTS.md"}}); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("explicit symlink copy error=%v", err)
	}
}

func TestMaterializeRootAtUsesPinnedDestination(t *testing.T) {
	source, target := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(source, "copied.txt"), []byte("pinned copy\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(source, "shared"), 0o700); err != nil {
		t.Fatal(err)
	}
	owner, err := os.OpenRoot(target)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = owner.Close() }()
	if err := MaterializeRootAt(source, owner, config.Workspace{Copy: []string{"copied.txt"}, Link: []string{"shared"}}); err != nil {
		t.Fatalf("pinned materialization: %v", err)
	}
	data, err := owner.ReadFile("copied.txt")
	if err != nil || string(data) != "pinned copy\n" {
		t.Fatalf("pinned copy=%q err=%v", data, err)
	}
	link, err := owner.Readlink("shared")
	if err != nil || link != filepath.Join(source, "shared") {
		t.Fatalf("pinned link=%q err=%v", link, err)
	}
	if err := MaterializeRootAt(source, nil, config.Workspace{}); err == nil {
		t.Fatal("nil destination root was accepted")
	}
}

func TestPinnedIncludeAndLinkMaterializationStayWithinRoot(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "worktrees")
	repository := filepath.Join(base, "repository")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(repository, 0o700); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, repository, "init", "-b", "main")
	if err := os.WriteFile(filepath.Join(repository, ".worktreeinclude"), []byte("local.env\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repository, "local.env"), []byte("local\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repository, ".gitignore"), []byte("shared\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(repository, "shared"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repository, ".worktreelink"), []byte("shared\n"), 0o600); err != nil {
		t.Fatal(err)
	}
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
	if err := preparer.copyIncludes(repo, target); err != nil {
		t.Fatalf("pinned include: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(target, "local.env"))
	if err != nil || string(data) != "local\n" {
		t.Fatalf("included file=%q err=%v", data, err)
	}
	if err := preparer.createLinks(context.Background(), repo, target); err != nil {
		t.Fatalf("pinned link: %v", err)
	}
	link, err := os.Readlink(filepath.Join(target, "shared"))
	if err != nil || link != filepath.Join(repository, "shared") {
		t.Fatalf("pinned link=%q err=%v", link, err)
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
	if err := MaterializeRoot(source, target, config.Workspace{Link: []string{"../outside"}}); err == nil {
		t.Fatal("unsafe link succeeded")
	}
}

func TestRuleConflictsAreRejectedBeforeMaterialization(t *testing.T) {
	source := t.TempDir()
	if err := os.MkdirAll(filepath.Join(source, "shared", "child"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "copy"), []byte("copy\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name  string
		rules config.Workspace
	}{
		{name: "copy ancestor of link", rules: config.Workspace{Copy: []string{"shared"}, Link: []string{"shared/child"}}},
		{name: "link ancestor of copy", rules: config.Workspace{Copy: []string{"shared/child"}, Link: []string{"shared"}}},
		{name: "link ancestor of link", rules: config.Workspace{Link: []string{"shared", "shared/child"}}},
		{name: "exact copy and link overlap", rules: config.Workspace{Copy: []string{"copy"}, Link: []string{"copy"}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			target := t.TempDir()
			if err := MaterializeRoot(source, target, test.rules); err == nil {
				t.Fatal("conflicting rules succeeded")
			}
			entries, err := os.ReadDir(target)
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 0 {
				t.Fatalf("conflicting rules partially materialized %v", entries)
			}
		})
	}
}

func TestSafeGlobRejectsMalformedPatternWithoutMatches(t *testing.T) {
	root := t.TempDir()
	if _, err := safeGlob(root, "["); err == nil {
		t.Fatal("malformed glob unexpectedly succeeded")
	}
}

func TestPhysicalManifestRejectsSymlinkRoot(t *testing.T) {
	physical := t.TempDir()
	if err := os.WriteFile(filepath.Join(physical, ".worktreeinclude"), []byte("secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(filepath.Dir(physical), "manifest-alias")
	if err := os.Symlink(physical, alias); err != nil {
		t.Fatal(err)
	}
	if _, err := readPhysicalPatterns(alias, ".worktreeinclude"); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("manifest through symlink root succeeded: %v", err)
	}
}

func TestPrepareCommandSuccessFailureAndTimeout(t *testing.T) {
	root := t.TempDir()
	repository := filepath.Join(root, "repository")
	if err := os.Mkdir(repository, 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "target")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	repo := discovery.Repository{MainPath: domain.CanonicalPath(repository)}
	cfg := config.Defaults()
	cfg.Storage.WorktreeRoot = root
	cfg.Repositories = map[string]config.Repository{repository: {Prepare: config.Prepare{Command: []string{"/bin/sh", "-c", "printf ready > marker"}, Timeout: config.Duration{Duration: time.Second}}}}
	owner, _, err := domain.OpenOwnedRoot(root, root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = owner.Close() }()
	preparer := Preparer{Git: &gitx.Runner{Timeout: time.Second}, Config: cfg, OwnedRoot: owner, RootPath: root}
	if err := preparer.runPrepareWithIdentity(context.Background(), repo, target, ""); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(filepath.Join(target, "marker")); err != nil || string(data) != "ready" {
		t.Fatalf("marker=%q err=%v", data, err)
	}
	cfg.Repositories[repository] = config.Repository{Prepare: config.Prepare{Command: []string{"/bin/sh", "-c", "exit 7"}, Timeout: config.Duration{Duration: time.Second}}}
	preparer.Config = cfg
	if err := preparer.runPrepareWithIdentity(context.Background(), repo, target, ""); err == nil {
		t.Fatal("failed prepare command succeeded")
	}
	cfg.Repositories[repository] = config.Repository{Prepare: config.Prepare{Command: []string{"/bin/sh", "-c", "sleep 2"}, Timeout: config.Duration{Duration: 20 * time.Millisecond}}}
	preparer.Config = cfg
	if err := preparer.runPrepareWithIdentity(context.Background(), repo, target, ""); err == nil {
		t.Fatal("timed out prepare command succeeded")
	}
}

func TestPrepareRejectsPathsOutsideRootSymlinksAndForeignContents(t *testing.T) {
	root := t.TempDir()
	cfg := config.Defaults()
	cfg.Storage.WorktreeRoot = root
	owner, _, err := domain.OpenOwnedRoot(root, root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = owner.Close() }()
	preparer := Preparer{Git: &gitx.Runner{Timeout: time.Second}, Config: cfg, OwnedRoot: owner, RootPath: root}
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
	descendant := filepath.Join(root, "descendant")
	outside := t.TempDir()
	if err := os.Symlink(outside, descendant); err != nil {
		t.Fatal(err)
	}
	if err := preparer.Prepare(context.Background(), repo, filepath.Join(descendant, "root"), "oid", "slot"); err == nil {
		t.Fatal("descendant symlink escaping the worktree root succeeded")
	}
	if entries, err := os.ReadDir(outside); err != nil || len(entries) != 0 {
		t.Fatalf("outside directory changed: entries=%v err=%v", entries, err)
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

// NEW-2: Git の add syscall は記述子で予約した対象名前空間を使う必要がある。
// 親を開いた後 Git 開始前にルートを置き換えても、ファイルと登録は逃げない。
func TestAddWorktreeUsesReservedNamespaceAcrossRootReplacement(t *testing.T) {
	base := t.TempDir()
	repository := filepath.Join(base, "repository")
	root := filepath.Join(base, "worktrees")
	outside := filepath.Join(base, "outside")
	if err := os.MkdirAll(repository, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(outside, 0o700); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, repository, "init", "-b", "main")
	gitCommand(t, repository, "config", "user.name", "test")
	gitCommand(t, repository, "config", "user.email", "test@example.com")
	if err := os.WriteFile(filepath.Join(repository, "tracked"), []byte("base\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, repository, "add", "tracked")
	gitCommand(t, repository, "commit", "-m", "initial")
	head := gitOutput(t, repository, "rev-parse", "HEAD")
	common := gitOutput(t, repository, "rev-parse", "--path-format=absolute", "--git-common-dir")
	repo := discovery.Repository{MainPath: domain.CanonicalPath(repository), CommonDir: domain.CanonicalPath(common)}
	owner, _, err := domain.OpenOwnedRoot(root, root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = owner.Close() }()
	if err := owner.MkdirAll("slot", 0o700); err != nil {
		t.Fatal(err)
	}
	runner := &gitx.Runner{Timeout: 5 * time.Second}
	p := Preparer{Git: runner, Config: func() config.Config { cfg := config.Defaults(); cfg.Storage.WorktreeRoot = root; return cfg }(), OwnedRoot: owner, RootPath: root}
	target := filepath.Join(root, "slot", "root")
	runner.SetBeforeRunAtHook(func(args []string) {
		if len(args) < 3 || args[0] != "--git-dir" {
			return
		}
		old := root + "-old"
		if err := os.Rename(root, old); err != nil {
			t.Fatalf("replace worktree root: %v", err)
		}
		if err := os.Symlink(outside, root); err != nil {
			t.Fatalf("install replacement root: %v", err)
		}
	})
	if err := p.addWorktree(context.Background(), repo, owner, target, filepath.Join("slot", "root"), head); err != nil {
		t.Fatalf("descriptor-bound worktree add failed: %v", err)
	}
	oldTarget := filepath.Join(root+"-old", "slot", "root")
	if info, err := os.Stat(oldTarget); err != nil || !info.IsDir() {
		t.Fatalf("worktree was not created in reserved namespace: info=%v err=%v", info, err)
	}
	if _, err := os.Lstat(filepath.Join(outside, "slot", "root")); !os.IsNotExist(err) {
		t.Fatalf("Git created worktree outside wx root: %v", err)
	}
	registered := gitOutput(t, repository, "worktree", "list", "--porcelain")
	if strings.Contains(registered, outside) || !strings.Contains(registered, oldTarget) {
		t.Fatalf("Git registration escaped reserved namespace: %q", registered)
	}
}

func TestPrepareClassifiesReplacedRootAsOwnershipUncertain(t *testing.T) {
	base := t.TempDir()
	repository := filepath.Join(base, "repository")
	root := filepath.Join(base, "worktrees")
	outside := filepath.Join(base, "outside")
	if err := os.Mkdir(repository, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(outside, 0o700); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, repository, "init", "-b", "main")
	gitCommand(t, repository, "config", "user.name", "test")
	gitCommand(t, repository, "config", "user.email", "test@example.com")
	if err := os.WriteFile(filepath.Join(repository, "tracked"), []byte("base\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, repository, "add", "tracked")
	gitCommand(t, repository, "commit", "-m", "initial")
	head := gitOutput(t, repository, "rev-parse", "HEAD")
	common := gitOutput(t, repository, "rev-parse", "--path-format=absolute", "--git-common-dir")
	repo := discovery.Repository{ID: "repo", MainPath: domain.CanonicalPath(repository), CommonDir: domain.CanonicalPath(common)}
	owner, _, err := domain.OpenOwnedRoot(root, root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = owner.Close() }()
	cfg := config.Defaults()
	cfg.Storage.WorktreeRoot = root
	slotPath := filepath.Join(root, testSlotRelPath)
	preparer := Preparer{Git: &gitx.Runner{Timeout: 5 * time.Second}, Config: cfg, Ownership: allowOwnershipValidator{}, OwnedRoot: owner, RootPath: root, SlotPath: slotPath, RootID: testRootID, SlotRelPath: testSlotRelPath}
	target := filepath.Join(slotPath, testRepositoryID)
	oldRoot := root + "-old"
	if err := os.Rename(root, oldRoot); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, root); err != nil {
		t.Fatal(err)
	}

	err = preparer.Prepare(context.Background(), repo, target, head, "slot")
	if !errors.Is(err, state.ErrOwnership) {
		t.Fatalf("replaced root returned non-ownership error: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(outside, testWorkspaceID)); !os.IsNotExist(err) {
		t.Fatalf("replaced root escaped into outside directory: %v", err)
	}
}

// 記述子に束縛された Git add 完了後に対象 leaf を別の物理 worktree へ置換できる。
// 置換先をこの prepare job が予約した worktree として扱ってはならない。
func TestPrepareRejectsTargetReplacementAfterGitAdd(t *testing.T) {
	base := t.TempDir()
	repository := filepath.Join(base, "repository")
	root := filepath.Join(base, "worktrees")
	if err := os.MkdirAll(repository, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, repository, "init", "-b", "main")
	gitCommand(t, repository, "config", "user.name", "test")
	gitCommand(t, repository, "config", "user.email", "test@example.com")
	if err := os.WriteFile(filepath.Join(repository, "tracked"), []byte("base\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, repository, "add", "tracked")
	gitCommand(t, repository, "commit", "-m", "initial")
	head := gitOutput(t, repository, "rev-parse", "HEAD")
	common := gitOutput(t, repository, "rev-parse", "--path-format=absolute", "--git-common-dir")
	repo := discovery.Repository{ID: "repo", MainPath: domain.CanonicalPath(repository), CommonDir: domain.CanonicalPath(common)}
	owner, _, err := domain.OpenOwnedRoot(root, root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = owner.Close() }()
	replacement := filepath.Join(root, "replacement")
	gitCommand(t, repository, "worktree", "add", "--detach", replacement, head)
	runner := &gitx.Runner{Timeout: 5 * time.Second}
	cfg := config.Defaults()
	cfg.Storage.WorktreeRoot = root
	slotPath := filepath.Join(root, testSlotRelPath)
	preparer := Preparer{Git: runner, Config: cfg, Ownership: allowOwnershipValidator{}, OwnedRoot: owner, RootPath: root, SlotPath: slotPath, RootID: testRootID, SlotRelPath: testSlotRelPath}
	target := filepath.Join(slotPath, testRepositoryID)
	targetOld := target + "-old"
	replaced := false
	runner.SetBeforeRunAtHook(func(args []string) {
		if replaced || !strings.Contains(strings.Join(args, " "), "worktree lock") {
			return
		}
		if err := os.Rename(target, targetOld); err != nil {
			t.Fatalf("move reserved target: %v", err)
		}
		if err := os.Rename(replacement, target); err != nil {
			t.Fatalf("install replacement target: %v", err)
		}
		replaced = true
	})
	err = preparer.Prepare(context.Background(), repo, target, head, "slot")
	if !replaced {
		t.Fatal("post-add target replacement barrier was not reached")
	}
	if !errors.Is(err, state.ErrOwnership) {
		t.Fatalf("replaced target returned non-ownership error: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(target, ".git")); statErr != nil {
		t.Fatalf("replacement worktree was removed or changed: %v", statErr)
	}
	if _, statErr := os.Stat(filepath.Join(targetOld, ".git")); statErr != nil {
		t.Fatalf("reserved worktree was removed after ownership failure: %v", statErr)
	}
}

// Finding 1: 一致する Git worktree でも wx の所有権証明なしには再利用できない。
func TestPrepareRefusesForeignRegisteredWorktreeWithoutWxOwnershipProof(t *testing.T) {
	root := t.TempDir()
	repository := filepath.Join(root, "repository")
	worktreeRoot := filepath.Join(root, "worktrees")
	if err := os.MkdirAll(repository, 0o700); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, repository, "init", "-b", "main")
	gitCommand(t, repository, "config", "user.name", "test")
	gitCommand(t, repository, "config", "user.email", "test@example.com")
	if err := os.WriteFile(filepath.Join(repository, "tracked"), []byte("base\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, repository, "add", ".")
	gitCommand(t, repository, "commit", "-m", "initial")
	head := gitOutput(t, repository, "rev-parse", "HEAD")
	common := gitOutput(t, repository, "rev-parse", "--path-format=absolute", "--git-common-dir")
	slotPath := filepath.Join(worktreeRoot, testSlotRelPath)
	target := filepath.Join(slotPath, testRepositoryID)
	if err := os.MkdirAll(slotPath, 0o700); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, repository, "worktree", "add", "--detach", target, head)
	repo := discovery.Repository{ID: "repo", MainPath: domain.CanonicalPath(repository), CommonDir: domain.CanonicalPath(common)}
	cfg := config.Defaults()
	cfg.Storage.WorktreeRoot = worktreeRoot
	owner, _, err := domain.OpenOwnedRoot(worktreeRoot, worktreeRoot)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = owner.Close() }()
	preparer := Preparer{Git: &gitx.Runner{Timeout: 5 * time.Second}, Config: cfg, Ownership: allowOwnershipValidator{}, OwnedRoot: owner, RootPath: worktreeRoot, SlotPath: slotPath, RootID: testRootID, SlotRelPath: testSlotRelPath}
	if err := preparer.Prepare(context.Background(), repo, target, head, "slot"); err == nil || !strings.Contains(err.Error(), "ownership marker") {
		t.Fatalf("foreign worktree prepare error=%v", err)
	}
	if _, err := os.Stat(filepath.Join(target, ".git")); err != nil {
		t.Fatalf("foreign worktree was removed or changed: %v", err)
	}
}

func TestIncludeAndLinkPoliciesRejectUnsafeInputs(t *testing.T) {
	base := t.TempDir()
	repository := filepath.Join(base, "repository")
	if err := os.Mkdir(repository, 0o700); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, repository, "init", "-b", "main")
	gitCommand(t, repository, "config", "user.name", "test")
	gitCommand(t, repository, "config", "user.email", "test@example.com")
	if err := os.WriteFile(filepath.Join(repository, ".worktreeinclude"), []byte("../outside\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(base, "worktrees")
	target := filepath.Join(root, "target")
	if err := os.MkdirAll(target, 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := config.Defaults()
	cfg.Storage.WorktreeRoot = root
	owner, _, err := domain.OpenOwnedRoot(root, root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = owner.Close() }()
	preparer := Preparer{Git: &gitx.Runner{Timeout: time.Second}, Config: cfg, OwnedRoot: owner, RootPath: root}
	repo := discovery.Repository{MainPath: domain.CanonicalPath(repository)}
	if err := preparer.copyIncludes(repo, target); err == nil || !strings.Contains(err.Error(), "unsafe") {
		t.Fatalf("unsafe include error=%v", err)
	}
	if err := os.WriteFile(filepath.Join(repository, ".worktreeinclude"), []byte("..\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := preparer.copyIncludes(repo, target); err == nil || !strings.Contains(err.Error(), "unsafe") {
		t.Fatalf("parent include error=%v", err)
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

// Finding 4: source と destination の symlink 祖先は、それぞれのルートから逃げてはならない。
func TestMaterializationRejectsSymlinkAncestors(t *testing.T) {
	root := t.TempDir()
	repository := filepath.Join(root, "repository")
	outside := filepath.Join(root, "outside")
	target := filepath.Join(root, "target")
	if err := os.MkdirAll(repository, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(outside, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "secret"), []byte("must not escape\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(repository, "linked")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repository, ".worktreeinclude"), []byte("linked/*\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(target, 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := config.Defaults()
	cfg.Storage.WorktreeRoot = root
	owner, _, err := domain.OpenOwnedRoot(root, root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = owner.Close() }()
	preparer := Preparer{Git: &gitx.Runner{Timeout: time.Second}, Config: cfg, OwnedRoot: owner, RootPath: root}
	repo := discovery.Repository{MainPath: domain.CanonicalPath(repository)}
	if err := preparer.copyIncludes(repo, target); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("include through symlink ancestor succeeded: %v", err)
	}
	if _, err := Fingerprint(1, "oid", repo, config.Defaults()); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("fingerprint through symlink ancestor succeeded: %v", err)
	}

	gitCommand(t, repository, "init", "-b", "main")
	gitCommand(t, repository, "config", "user.name", "test")
	gitCommand(t, repository, "config", "user.email", "test@example.com")
	if err := os.WriteFile(filepath.Join(repository, ".gitignore"), []byte("source\ndest/child\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repository, ".worktreeinclude"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(outside, "source"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "source"), filepath.Join(repository, "source")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repository, ".worktreelink"), []byte("source\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := preparer.createLinks(context.Background(), discovery.Repository{MainPath: domain.CanonicalPath(repository)}, target); err == nil || !strings.Contains(err.Error(), "physical") {
		t.Fatalf("link source through symlink ancestor succeeded: %v", err)
	}

	linkSource := filepath.Join(repository, "real-source")
	if err := os.Mkdir(linkSource, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repository, ".worktreelink"), []byte("dest/child\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(repository, "dest"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repository, "dest", "child"), []byte("child\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(target, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(target, "dest")); err != nil {
		t.Fatal(err)
	}
	if err := preparer.createLinks(context.Background(), discovery.Repository{MainPath: domain.CanonicalPath(repository)}, target); err == nil || !strings.Contains(err.Error(), "destination") {
		t.Fatalf("link destination through symlink ancestor succeeded: %v", err)
	}
	if _, err := os.Stat(filepath.Join(outside, "child")); !os.IsNotExist(err) {
		t.Fatalf("outside destination was modified: %v", err)
	}

	materializedTarget := filepath.Join(root, "materialized")
	if err := os.Mkdir(materializedTarget, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(materializedTarget, "nested")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(repository, "nested"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repository, "nested", "value"), []byte("value\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := MaterializeRoot(repository, materializedTarget, config.Workspace{Copy: []string{"nested/value"}}); err == nil || !strings.Contains(err.Error(), "destination") {
		t.Fatalf("copy destination through symlink ancestor succeeded: %v", err)
	}
}

// tracked path を指す .worktreeinclude entry は無視され、worktree の内容は checkout が決める。
// main worktree 側だけ内容を変えることで、コピーが起きていないことを見分ける。
func TestWorktreeIncludeIgnoresTrackedPaths(t *testing.T) {
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

func TestPrepareFailureCleansPartialWorktreeAndCoversPolicyEdges(t *testing.T) {
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
	if err := os.WriteFile(filepath.Join(repository, ".worktreeinclude"), []byte("tracked\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, repository, "add", ".")
	gitCommand(t, repository, "commit", "-m", "initial")
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
	if err := os.WriteFile(filepath.Join(repository, ".worktreeinclude"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repository, ".worktreelink"), []byte("tracked\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	linkTarget := filepath.Join(slotPath, "link-failure")
	if err := preparer.Prepare(context.Background(), repo, linkTarget, head, "link-failure"); err == nil || !strings.Contains(err.Error(), "not ignored") {
		t.Fatalf("prepare link policy error=%v", err)
	}
	if _, err := os.Stat(filepath.Join(linkTarget, ".git")); !os.IsNotExist(err) {
		t.Fatalf("link-policy partial worktree remains: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repository, ".worktreelink"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	failingCfg := cfg
	failingCfg.Repositories = map[string]config.Repository{repository: {Prepare: config.Prepare{Command: []string{"/usr/bin/false"}, Timeout: config.Duration{Duration: time.Second}}}}
	preparer.Config = failingCfg
	commandTarget := filepath.Join(slotPath, "command-failure")
	if err := preparer.Prepare(context.Background(), repo, commandTarget, head, "command-failure"); err == nil {
		t.Fatal("failed prepare command completed a worktree")
	}
	if _, err := os.Stat(filepath.Join(commandTarget, ".git")); !os.IsNotExist(err) {
		t.Fatalf("command-failure partial worktree remains: %v", err)
	}
	dirtyCfg := cfg
	dirtyCfg.Repositories = map[string]config.Repository{repository: {Prepare: config.Prepare{Command: []string{"/bin/sh", "-c", "printf changed > tracked"}, Timeout: config.Duration{Duration: time.Second}}}}
	preparer.Config = dirtyCfg
	dirtyTarget := filepath.Join(slotPath, "dirty-command")
	if err := preparer.Prepare(context.Background(), repo, dirtyTarget, head, "dirty-command"); err == nil || !strings.Contains(err.Error(), "tracked changes") {
		t.Fatalf("dirty prepare command error=%v", err)
	}
	if _, err := os.Stat(filepath.Join(dirtyTarget, ".git")); !os.IsNotExist(err) {
		t.Fatalf("dirty-command partial worktree remains: %v", err)
	}

	cfg.Repositories = map[string]config.Repository{repository: {Prepare: config.Prepare{Command: []string{"/usr/bin/true"}, Version: "v1"}}}
	cfg.Readiness.Timeout.Duration = time.Second
	preparer.Config = cfg
	runPrepareTarget := filepath.Join(slotPath, "run-prepare")
	if err := os.MkdirAll(runPrepareTarget, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := preparer.runPrepareWithIdentity(context.Background(), repo, runPrepareTarget, ""); err != nil {
		t.Fatal(err)
	}
	if fingerprint, err := Fingerprint(1, head, repo, cfg); err != nil || fingerprint == "" {
		t.Fatalf("fingerprint=%q err=%v", fingerprint, err)
	}
	badRoot := preparer
	badRoot.Config.Storage.WorktreeRoot = "$UNSUPPORTED/worktrees"
	if err := badRoot.Prepare(context.Background(), repo, target, head, "bad-root"); err == nil {
		t.Fatal("unsupported worktree root expansion succeeded")
	}
	if err := preparer.ValidateReady(context.Background(), repo, filepath.Join(root, "missing-ready"), head); err == nil {
		t.Fatal("missing READY worktree validated")
	}
}

func TestPatternFilesThatAreDirectoriesAreRejected(t *testing.T) {
	base := t.TempDir()
	repository := filepath.Join(base, "repository")
	if err := os.Mkdir(repository, 0o700); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(base, "worktrees")
	target := filepath.Join(root, "target")
	if err := os.MkdirAll(target, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(repository, ".worktreeinclude"), 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := config.Defaults()
	cfg.Storage.WorktreeRoot = root
	owner, _, err := domain.OpenOwnedRoot(root, root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = owner.Close() }()
	preparer := Preparer{Git: &gitx.Runner{Timeout: time.Second}, Config: cfg, OwnedRoot: owner, RootPath: root}
	repo := discovery.Repository{MainPath: domain.CanonicalPath(repository)}
	if err := preparer.copyIncludes(repo, target); err == nil {
		t.Fatal("directory .worktreeinclude was accepted")
	}
	if err := os.Mkdir(filepath.Join(repository, ".worktreelink"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := preparer.createLinks(context.Background(), repo, target); err == nil {
		t.Fatal("directory .worktreelink was accepted")
	}
	if err := os.Remove(filepath.Join(repository, ".worktreeinclude")); err != nil {
		t.Fatal(err)
	}
	physicalManifest := filepath.Join(repository, "physical-include")
	if err := os.WriteFile(physicalManifest, []byte("value\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(physicalManifest, filepath.Join(repository, ".worktreeinclude")); err != nil {
		t.Fatal(err)
	}
	if err := preparer.copyIncludes(repo, target); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("symlink .worktreeinclude was accepted: %v", err)
	}
}

func TestFingerprintTracksMaterializedCopyInputs(t *testing.T) {
	root := t.TempDir()
	repository := filepath.Join(root, "repository")
	if err := os.Mkdir(repository, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repository, ".worktreeinclude"), []byte("local.env\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	local := filepath.Join(repository, "local.env")
	if err := os.WriteFile(local, []byte("first\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	repo := discovery.Repository{MainPath: domain.CanonicalPath(repository), RelativePath: "repository"}
	cfg := config.Defaults()
	first, err := Fingerprint(1, "oid", repo, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(local, []byte("second\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	second, err := Fingerprint(1, "oid", repo, cfg)
	if err != nil || second == first {
		t.Fatalf("include content fingerprint first=%s second=%s err=%v", first, second, err)
	}
	if err := os.WriteFile(filepath.Join(root, "AGENTS.md"), []byte("instructions\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	third, err := Fingerprint(1, "oid", repo, cfg)
	if err != nil || third == second {
		t.Fatalf("workspace copy fingerprint second=%s third=%s err=%v", second, third, err)
	}
	if err := os.WriteFile(filepath.Join(root, "custom.txt"), []byte("custom\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg.Workspaces[root] = config.Workspace{Copy: []string{"custom.txt"}}
	fourth, err := Fingerprint(1, "oid", repo, cfg)
	if err != nil || fourth == third {
		t.Fatalf("workspace rule fingerprint third=%s fourth=%s err=%v", third, fourth, err)
	}
}

func TestFingerprintCoversRecursiveDuplicateAndWorkspaceLinkInputs(t *testing.T) {
	root := t.TempDir()
	repository := filepath.Join(root, "nested", "repository")
	if err := os.MkdirAll(filepath.Join(repository, "included", "child"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repository, "included", "child", "value"), []byte("one\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repository, ".worktreeinclude"), []byte("included\nincluded\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "custom.txt"), []byte("custom\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "shared"), 0o700); err != nil {
		t.Fatal(err)
	}
	repo := discovery.Repository{MainPath: domain.CanonicalPath(repository), RelativePath: filepath.Join("nested", "repository")}
	cfg := config.Defaults()
	cfg.Workspaces[root] = config.Workspace{
		Copy: []string{"custom.txt", "custom.txt"},
		Link: []string{"shared"},
	}
	cfg.Repositories[repository] = config.Repository{Prepare: config.Prepare{Command: []string{"true"}, Version: "v2"}}
	first, err := Fingerprint(2, "oid", repo, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repository, "included", "child", "value"), []byte("two\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	second, err := Fingerprint(2, "oid", repo, cfg)
	if err != nil || first == second {
		t.Fatalf("recursive fingerprint first=%s second=%s err=%v", first, second, err)
	}

	if err := os.RemoveAll(filepath.Join(repository, "included")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(root, filepath.Join(repository, "included")); err != nil {
		t.Fatal(err)
	}
	if _, err := Fingerprint(2, "oid", repo, cfg); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("include symlink error=%v", err)
	}
	if err := os.WriteFile(filepath.Join(repository, ".worktreeinclude"), []byte("../outside\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Fingerprint(2, "oid", repo, cfg); err == nil || !strings.Contains(err.Error(), "unsafe") {
		t.Fatalf("unsafe fingerprint include error=%v", err)
	}

	cleanRepo := discovery.Repository{MainPath: domain.CanonicalPath(repository), RelativePath: "../outside"}
	if err := os.WriteFile(filepath.Join(repository, ".worktreeinclude"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Fingerprint(2, "oid", cleanRepo, cfg); err == nil {
		t.Fatal("unsafe repository relative path was fingerprinted")
	}

	unsafeCopy := cfg
	unsafeCopy.Workspaces[root] = config.Workspace{Copy: []string{"../outside"}}
	if _, err := Fingerprint(2, "oid", repo, unsafeCopy); err == nil {
		t.Fatal("unsafe workspace copy path was fingerprinted")
	}
	unsafeLink := cfg
	unsafeLink.Workspaces[root] = config.Workspace{Link: []string{"../outside"}}
	if _, err := Fingerprint(2, "oid", repo, unsafeLink); err == nil {
		t.Fatal("unsafe workspace link path was fingerprinted")
	}
	missingLink := cfg
	missingLink.Workspaces[root] = config.Workspace{Link: []string{"missing"}}
	if _, err := Fingerprint(2, "oid", repo, missingLink); err == nil {
		t.Fatal("missing workspace link path was fingerprinted")
	}
}

func TestWorkspaceHelpersSurfaceFilesystemAndGitErrors(t *testing.T) {
	root := t.TempDir()
	repository := filepath.Join(root, "repository")
	target := filepath.Join(root, "target")
	if err := os.MkdirAll(repository, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(target, 0o700); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, repository, "init", "-b", "main")
	gitCommand(t, repository, "config", "user.name", "test")
	gitCommand(t, repository, "config", "user.email", "test@example.com")
	if err := os.WriteFile(filepath.Join(repository, "tracked"), []byte("base\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, repository, "add", ".")
	gitCommand(t, repository, "commit", "-m", "initial")
	repo := discovery.Repository{
		ID:        testRepositoryID,
		MainPath:  domain.CanonicalPath(repository),
		CommonDir: domain.CanonicalPath(filepath.Join(repository, ".git")),
	}
	cfg := config.Defaults()
	cfg.Storage.WorktreeRoot = root
	owner, _, err := domain.OpenOwnedRoot(root, root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = owner.Close() }()
	preparer := Preparer{Git: &gitx.Runner{Timeout: time.Second}, Config: cfg, OwnedRoot: owner, RootPath: root, RootID: testRootID}

	if err := preparer.validateTrackedClean(context.Background(), filepath.Join(root, "missing")); err == nil {
		t.Fatal("tracked-clean validation of a missing worktree succeeded")
	}
	if err := os.WriteFile(filepath.Join(repository, ".worktreeinclude"), []byte("[\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := preparer.copyIncludes(repo, target); err == nil {
		t.Fatal("invalid include glob succeeded")
	}
	include := filepath.Join(repository, "included")
	if err := os.Symlink(filepath.Join(repository, "tracked"), include); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repository, ".worktreeinclude"), []byte("included\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := preparer.copyIncludes(repo, target); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("include copy error=%v", err)
	}

	if err := os.WriteFile(filepath.Join(repository, ".gitignore"), []byte("blocked/child\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repository, ".worktreelink"), []byte("blocked/child\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "blocked"), []byte("file"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := preparer.createLinks(context.Background(), repo, target); err == nil {
		t.Fatal("link beneath regular file succeeded")
	}

	missingCommon := repo
	missingCommon.CommonDir = domain.CanonicalPath(filepath.Join(root, "missing-common"))
	if err := preparer.validateExistingWorktree(context.Background(), missingCommon, repository, gitOutput(t, repository, "rev-parse", "HEAD")); err == nil {
		t.Fatal("worktree with missing expected common directory validated")
	}
	other := filepath.Join(root, "other")
	if err := os.Mkdir(other, 0o700); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, other, "init", "-b", "main")
	mismatchedCommon := repo
	mismatchedCommon.CommonDir = domain.CanonicalPath(filepath.Join(other, ".git"))
	if err := preparer.validateExistingWorktree(context.Background(), mismatchedCommon, repository, gitOutput(t, repository, "rev-parse", "HEAD")); err == nil {
		t.Fatal("worktree with mismatched common directory validated")
	}

	gitMarkerTarget := filepath.Join(root, "git-marker-target")
	if err := os.Mkdir(gitMarkerTarget, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(repository, ".git"), filepath.Join(gitMarkerTarget, ".git")); err != nil {
		t.Fatal(err)
	}
	if err := preparer.validateExistingWorktree(context.Background(), repo, gitMarkerTarget, "oid"); err == nil {
		t.Fatal("symlink .git marker validated")
	}
	invalidGitTarget := filepath.Join(root, "invalid-git-target")
	if err := os.Mkdir(invalidGitTarget, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(invalidGitTarget, ".git"), []byte("not a git marker"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := preparer.validateExistingWorktree(context.Background(), repo, invalidGitTarget, "oid"); err == nil {
		t.Fatal("invalid .git marker validated")
	}

	h := sha256.New()
	if err := fingerprintPath(h, root, filepath.Join(root, "missing-fingerprint")); err == nil {
		t.Fatal("missing path was fingerprinted")
	}
	if err := fingerprintPath(h, root, root); err == nil {
		t.Fatal("fingerprint root itself was accepted as a relative input")
	}
	if err := fingerprintPath(h, repository, root); err == nil {
		t.Fatal("fingerprint path outside root was accepted")
	}

	gitCommand(t, repository, "checkout", "--detach")
	missingMain := repo
	missingMain.MainPath = domain.CanonicalPath(filepath.Join(root, "missing-main"))
	if err := preparer.validateExistingWorktree(context.Background(), missingMain, repository, gitOutput(t, repository, "rev-parse", "HEAD")); err == nil {
		t.Fatal("worktree validation with missing main repository succeeded")
	}
	unregistered := repo
	unregistered.MainPath = domain.CanonicalPath(other)
	if err := EnsureOwnershipMarkerAt(owner, root, repository, markerFor("slot"), string(repo.CommonDir)); err != nil {
		t.Fatal(err)
	}
	if err := preparer.validateExistingWorktree(context.Background(), unregistered, repository, gitOutput(t, repository, "rev-parse", "HEAD")); err == nil || !strings.Contains(err.Error(), "not registered") {
		t.Fatalf("unregistered worktree error=%v", err)
	}
}

func TestWorkspaceHelpersRejectUnreadableInputsAndUnwritableTargets(t *testing.T) {
	root := t.TempDir()
	h := sha256.New()
	owner, err := OpenPhysicalRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = owner.Close() }()

	unreadableFile := filepath.Join(root, "unreadable-file")
	if err := os.WriteFile(unreadableFile, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(unreadableFile, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(unreadableFile, 0o600) })
	if err := fingerprintPath(h, root, unreadableFile); err == nil {
		t.Fatal("unreadable file was fingerprinted")
	}
	if err := copyPathFromOwnedRoot(owner, "unreadable-file", owner, "copy"); err == nil {
		t.Fatal("unreadable file was copied")
	}

	unreadableDirectory := filepath.Join(root, "unreadable-directory")
	if err := os.Mkdir(unreadableDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(unreadableDirectory, "child"), []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(unreadableDirectory, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(unreadableDirectory, 0o700) })
	if err := fingerprintPath(h, root, unreadableDirectory); err == nil {
		t.Fatal("unreadable directory was fingerprinted")
	}
	if err := copyPathFromOwnedRoot(owner, "unreadable-directory", owner, "directory-copy"); err == nil {
		t.Fatal("unreadable directory was copied")
	}

	source := filepath.Join(root, "source")
	if err := os.WriteFile(source, []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	readOnly := filepath.Join(root, "read-only")
	if err := os.Mkdir(readOnly, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(readOnly, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(readOnly, 0o700) })
	if err := copyPathFromOwnedRoot(owner, "source", owner, filepath.Join("read-only", "target")); err == nil {
		t.Fatal("file copied into unwritable directory")
	}

	manifestRoot := filepath.Join(root, "manifest")
	if err := os.Mkdir(manifestRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	manifest := filepath.Join(manifestRoot, ".worktreeinclude")
	if err := os.WriteFile(manifest, []byte("value\n"), 0); err != nil {
		t.Fatal(err)
	}
	repo := discovery.Repository{MainPath: domain.CanonicalPath(manifestRoot)}
	preparer := Preparer{Git: &gitx.Runner{Timeout: time.Second}, Config: config.Defaults()}
	if _, err := Fingerprint(1, "oid", repo, config.Defaults()); err == nil {
		t.Fatal("unreadable fingerprint manifest succeeded")
	}
	if err := preparer.copyIncludes(repo, root); err == nil {
		t.Fatal("unreadable include manifest succeeded")
	}
	if err := os.Remove(manifest); err != nil {
		t.Fatal(err)
	}
	linkManifest := filepath.Join(manifestRoot, ".worktreelink")
	if err := os.WriteFile(linkManifest, []byte("value\n"), 0); err != nil {
		t.Fatal(err)
	}
	if err := preparer.createLinks(context.Background(), repo, root); err == nil {
		t.Fatal("unreadable link manifest succeeded")
	}

	linkSource := filepath.Join(root, "link-source")
	if err := os.Mkdir(linkSource, 0o700); err != nil {
		t.Fatal(err)
	}
	linkTarget := filepath.Join(root, "link-target")
	if err := os.Mkdir(linkTarget, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(linkTarget, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(linkTarget, 0o700) })
	if err := MaterializeRoot(root, linkTarget, config.Workspace{Link: []string{"link-source"}}); err == nil {
		t.Fatal("workspace link created in unwritable target")
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
		t.Fatal(err)
	}
	if err := preparer.ValidateReady(context.Background(), repo, target, head); err != nil {
		t.Fatal(err)
	}
	if err := preparer.Prepare(context.Background(), repo, target, head, "slot"); err != nil {
		t.Fatalf("idempotent prepare: %v", err)
	}
	if err := os.WriteFile(filepath.Join(target, "tracked"), []byte("changed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := preparer.ValidateReady(context.Background(), repo, target, head); err == nil || !strings.Contains(err.Error(), "tracked changes") {
		t.Fatalf("dirty READY validation error=%v", err)
	}
	gitCommand(t, target, "checkout", "--", "tracked")
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

func TestDefaultIncludesCarryUntrackedRuleFilesWithoutAManifest(t *testing.T) {
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

func TestFingerprintTracksDefaultIncludeContent(t *testing.T) {
	base := t.TempDir()
	repository := filepath.Join(base, "repository")
	if err := os.Mkdir(repository, 0o700); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, repository, "init", "-b", "main")
	cfg := config.Defaults()
	repo := discovery.Repository{MainPath: domain.CanonicalPath(repository)}
	bare, err := Fingerprint(1, "oid", repo, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repository, "GEMINI.local.md"), []byte("first\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	disabledCfg := cfg
	disabledCfg.Includes.DefaultAgentRules = false
	disabled, err := Fingerprint(1, "oid", repo, disabledCfg)
	if err != nil {
		t.Fatal(err)
	}
	if disabled != bare {
		t.Fatal("disabling default includes changed the fingerprint without materialized content")
	}
	added, err := Fingerprint(1, "oid", repo, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if added == bare {
		t.Fatal("an added default include left the fingerprint unchanged")
	}
	if err := os.WriteFile(filepath.Join(repository, "GEMINI.local.md"), []byte("second\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	edited, err := Fingerprint(1, "oid", repo, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if edited == added {
		t.Fatal("an edited default include left the fingerprint unchanged")
	}
	// 明示的な link ルールが所有するパスはコピーせず、その内容で slot も
	// 再構築してはならない。
	if err := os.WriteFile(filepath.Join(repository, ".worktreelink"), []byte("GEMINI.local.md\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	linked, err := Fingerprint(1, "oid", repo, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repository, "GEMINI.local.md"), []byte("third\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	relinked, err := Fingerprint(1, "oid", repo, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if relinked != linked {
		t.Fatal("a linked default include still contributed its content")
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
