package workspace

import (
	"context"
	"crypto/sha256"
	"errors"
	"log/slog"
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
	if err := MaterializeRoot(nil, source, target, config.Workspace{Link: []string{"shared"}}); err == nil {
		t.Fatal("link collision succeeded")
	}
	if err := MaterializeRoot(nil, source, target, config.Workspace{Copy: []string{"../outside"}}); err == nil {
		t.Fatal("unsafe copy succeeded")
	}
	if err := MaterializeRoot(nil, source, target, config.Workspace{Link: []string{"../outside"}}); err == nil {
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
			if err := MaterializeRoot(nil, source, target, test.rules); err == nil {
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

// TestSkippedSourcesAreRecorded は、skip した source が後から追える形で warn ログに残ることを確認する。
func TestSkippedSourcesAreRecorded(t *testing.T) {
	base := t.TempDir()
	repository := filepath.Join(base, "repository")
	if err := os.Mkdir(repository, 0o700); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, repository, "init", "-b", "main")
	gitCommand(t, repository, "config", "user.name", "test")
	gitCommand(t, repository, "config", "user.email", "test@example.com")
	if err := os.WriteFile(filepath.Join(repository, "real"), []byte("real\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("real", filepath.Join(repository, "linked-source")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repository, "unignored"), []byte("unignored\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repository, ".worktreelink"), []byte("linked-source\nunignored\n"), 0o600); err != nil {
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
	var logged strings.Builder
	logger := slog.New(slog.NewTextHandler(&logged, &slog.HandlerOptions{Level: slog.LevelWarn}))
	preparer := Preparer{Git: &gitx.Runner{Timeout: time.Second}, Config: cfg, OwnedRoot: owner, RootPath: root, Log: logger}
	repo := discovery.Repository{MainPath: domain.CanonicalPath(repository)}
	if err := preparer.createLinks(context.Background(), repo, target); err != nil {
		t.Fatalf("createLinks: %v", err)
	}
	for _, want := range []string{"source is a symlink", "linked-source", "is not ignored", "unignored"} {
		if !strings.Contains(logged.String(), want) {
			t.Fatalf("skip log %q missing from %q", want, logged.String())
		}
	}

	logged.Reset()
	workspaceRoot := filepath.Join(base, "workspace")
	if err := os.Mkdir(workspaceRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspaceRoot, "real"), []byte("real\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"copied", "linked"} {
		if err := os.Symlink("real", filepath.Join(workspaceRoot, name)); err != nil {
			t.Fatal(err)
		}
	}
	materialized := filepath.Join(base, "materialized")
	rules := config.Workspace{Copy: []string{"copied"}, Link: []string{"linked"}}
	if err := MaterializeRoot(logger, workspaceRoot, materialized, rules); err != nil {
		t.Fatalf("MaterializeRoot: %v", err)
	}
	for _, name := range []string{"copied", "linked"} {
		if _, err := os.Lstat(filepath.Join(materialized, name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("symlink workspace source %s materialized: %v", name, err)
		}
		if !strings.Contains(logged.String(), name) {
			t.Fatalf("skip log for %s missing from %q", name, logged.String())
		}
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
	if err := preparer.createLinks(context.Background(), repo, target); err != nil {
		t.Fatalf("missing unignored link should be skipped: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repository, "not-ignored"), []byte("now present\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// 未 ignore の link は worktree に追跡差分を作らないよう、その 1 件だけ skip する。
	if err := preparer.createLinks(context.Background(), repo, target); err != nil {
		t.Fatalf("present unignored link error=%v", err)
	}
	if _, err := os.Lstat(filepath.Join(target, "not-ignored")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unignored link materialized: %v", err)
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
	// include の tracked 判定は repository 全体の ls-files を一度引くため、走査前に Git repository が要る。
	gitCommand(t, repository, "init", "-b", "main")
	gitCommand(t, repository, "config", "user.name", "test")
	gitCommand(t, repository, "config", "user.email", "test@example.com")
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
	// symlink の link source は辿らず skip する。repository の外を指す実体を worktree に持ち込まないためである。
	if err := preparer.createLinks(context.Background(), discovery.Repository{MainPath: domain.CanonicalPath(repository)}, target); err != nil {
		t.Fatalf("symlink link source error=%v", err)
	}
	if _, err := os.Lstat(filepath.Join(target, "source")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("symlink link source materialized: %v", err)
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
	if err := MaterializeRoot(nil, repository, materializedTarget, config.Workspace{Copy: []string{"nested/value"}}); err == nil || !strings.Contains(err.Error(), "destination") {
		t.Fatalf("copy destination through symlink ancestor succeeded: %v", err)
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
	// 未 ignore の link は skip されるため、prepare 自体は成功して worktree が残る。
	// 成功した prepare は所有権 marker を slot directory に残すので、後続の失敗ケースとは別の slot を使う。
	linkSlotRelPath := filepath.Join(testWorkspaceID, "slot-link-skipped")
	linkPreparer := preparer
	linkPreparer.SlotPath = filepath.Join(worktreeRoot, linkSlotRelPath)
	linkPreparer.SlotRelPath = linkSlotRelPath
	linkTarget := filepath.Join(linkPreparer.SlotPath, testRepositoryID)
	if err := linkPreparer.Prepare(context.Background(), repo, linkTarget, head, "link-skipped"); err != nil {
		t.Fatalf("prepare link policy error=%v", err)
	}
	if _, err := os.Stat(filepath.Join(linkTarget, ".git")); err != nil {
		t.Fatalf("link-skipped worktree missing: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(linkTarget, "tracked")); err != nil {
		t.Fatalf("link-skipped worktree lost its tracked file: %v", err)
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
	// symlink の include は辿らず skip し、include 処理全体は成功させる。
	if err := preparer.copyIncludes(repo, target); err != nil {
		t.Fatalf("include copy error=%v", err)
	}
	if _, err := os.Lstat(filepath.Join(target, "included")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("include symlink materialized: %v", err)
	}

	if err := os.WriteFile(filepath.Join(repository, ".gitignore"), []byte("blocked/child\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repository, ".worktreelink"), []byte("blocked/child\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(repository, "blocked"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repository, "blocked", "child"), []byte("source"), 0o600); err != nil {
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
	if err := MaterializeRoot(nil, root, linkTarget, config.Workspace{Link: []string{"link-source"}}); err == nil {
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
	if err := MaterializeRoot(nil, brokenSource, materialized, config.Workspace{Link: []string{"missing"}}); err == nil {
		t.Fatal("missing root link source succeeded")
	}
	if err := os.WriteFile(filepath.Join(brokenSource, "AGENTS.local.md"), []byte("rules"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(brokenSource, "AGENTS.local.md"), filepath.Join(materialized, "AGENTS.local.md")); err != nil {
		t.Fatal(err)
	}
	if err := MaterializeRoot(nil, brokenSource, materialized, config.Workspace{}); err == nil {
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
