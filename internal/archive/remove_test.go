package archive

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/HappyOnigiri/WX/internal/config"
	"github.com/HappyOnigiri/WX/internal/discovery"
	"github.com/HappyOnigiri/WX/internal/domain"
	"github.com/HappyOnigiri/WX/internal/gitx"
	"github.com/HappyOnigiri/WX/internal/state"
	"github.com/HappyOnigiri/WX/internal/workspace"
)

func TestRemoveWorktreeRejectsSymlinkInRecordedPath(t *testing.T) {
	temp := t.TempDir()
	repository := filepath.Join(temp, "repository")
	root := filepath.Join(temp, "wx")
	mustMkdir(t, repository)
	mustMkdir(t, root)
	gitCommand(t, repository, "init", "-b", "main")
	gitCommand(t, repository, "config", "user.name", "test")
	gitCommand(t, repository, "config", "user.email", "test@example.com")
	if err := os.WriteFile(filepath.Join(repository, "tracked"), []byte("content\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, repository, "add", ".")
	gitCommand(t, repository, "commit", "-m", "initial")
	head := gitCommand(t, repository, "rev-parse", "HEAD")

	first := filepath.Join(root, "slot-a", "repo")
	second := filepath.Join(root, "slot-b", "repo")
	mustMkdir(t, filepath.Dir(first))
	mustMkdir(t, filepath.Dir(second))
	gitCommand(t, repository, "worktree", "add", "--detach", first, head)
	gitCommand(t, repository, "worktree", "add", "--detach", second, head)
	gitCommand(t, repository, "worktree", "remove", "--force", first)
	if err := os.Remove(filepath.Dir(first)); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Dir(second), filepath.Dir(first)); err != nil {
		t.Fatal(err)
	}

	repo := discovery.Repository{
		ID:        domain.RepositoryID("repository"),
		MainPath:  domain.CanonicalPath(repository),
		CommonDir: domain.CanonicalPath(filepath.Join(repository, ".git")),
	}
	manager := removalManager(t, root, allowOwnershipValidator{})
	pointAtSlot(t, manager, root, first)
	err := manager.RemoveWorktree(context.Background(), repo, root, first, head)
	if err == nil || !strings.Contains(err.Error(), "symlink component") {
		t.Fatalf("RemoveWorktree error = %v, want symlink rejection", err)
	}
	if _, err := os.Stat(filepath.Join(second, ".git")); err != nil {
		t.Fatalf("unrelated registered worktree was changed: %v", err)
	}
}

func TestRemoveWorktreeUsesPinnedDescriptorAcrossRootReplacement(t *testing.T) {
	temp := t.TempDir()
	repository := filepath.Join(temp, "repository")
	root := filepath.Join(temp, "wx")
	outside := filepath.Join(temp, "outside")
	mustMkdir(t, repository)
	mustMkdir(t, root)
	mustMkdir(t, outside)
	gitCommand(t, repository, "init", "-b", "main")
	gitCommand(t, repository, "config", "user.name", "test")
	gitCommand(t, repository, "config", "user.email", "test@example.com")
	if err := os.WriteFile(filepath.Join(repository, "tracked"), []byte("content\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, repository, "add", ".")
	gitCommand(t, repository, "commit", "-m", "initial")
	head := gitCommand(t, repository, "rev-parse", "HEAD")
	common := gitCommand(t, repository, "rev-parse", "--path-format=absolute", "--git-common-dir")
	repo := discovery.Repository{ID: "repository", MainPath: domain.CanonicalPath(repository), CommonDir: domain.CanonicalPath(common)}
	target := filepath.Join(root, "slot", "root")
	foreign := filepath.Join(outside, "foreign")
	mustMkdir(t, filepath.Dir(target))
	gitCommand(t, repository, "worktree", "add", "--detach", target, head)
	gitCommand(t, repository, "worktree", "add", "--detach", foreign, head)
	owner, _, err := domain.OpenOwnedRoot(root, root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = owner.Close() }()
	if err := workspace.EnsureOwnershipMarkerAt(owner, root, target, markerIdentityFor(repo, "slot"), common); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, repository, "worktree", "lock", "--reason", "wx:slot:READY", target)
	runner := &gitx.Runner{Timeout: 5 * time.Second}
	cfg := config.Defaults()
	cfg.Storage.WorktreeRoot = root
	preparer := &workspace.Preparer{Git: runner, Config: cfg, OwnedRoot: owner, RootPath: root, RootID: testRootID, SlotPath: filepath.Dir(target), SlotRelPath: "slot"}
	manager := &Manager{Git: runner, Preparer: preparer, Ownership: allowOwnershipValidator{}}
	replaced := false
	runner.SetBeforeRunAtHook(func(args []string) {
		if replaced || !strings.Contains(strings.Join(args, " "), "worktree remove") {
			return
		}
		oldRoot := root + "-old"
		if err := os.Rename(root, oldRoot); err != nil {
			t.Fatalf("replace configured root: %v", err)
		}
		if err := os.Symlink(outside, root); err != nil {
			t.Fatalf("install replacement root: %v", err)
		}
		replaced = true
	})
	err = manager.RemoveWorktree(context.Background(), repo, root, target, head)
	if !replaced {
		t.Fatal("descriptor-bound removal barrier was not reached")
	}
	if !errors.Is(err, state.ErrOwnership) {
		t.Fatalf("root replacement returned non-ownership error: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(root+"-old", "slot", "root", ".git")); statErr != nil {
		t.Fatalf("original worktree was removed after root replacement: %v", statErr)
	}
	if _, statErr := os.Stat(filepath.Join(outside, "foreign", ".git")); statErr != nil {
		t.Fatalf("foreign worktree was removed after root replacement: %v", statErr)
	}
}

func TestRemovalReconcilesMissingRegistrationAndRejectsWrongRepository(t *testing.T) {
	root := t.TempDir()
	first := filepath.Join(root, "first")
	second := filepath.Join(root, "second")
	for _, repository := range []string{first, second} {
		mustMkdir(t, repository)
		gitCommand(t, repository, "init", "-b", "main")
		gitCommand(t, repository, "config", "user.name", "test")
		gitCommand(t, repository, "config", "user.email", "test@example.com")
		if err := os.WriteFile(filepath.Join(repository, "tracked"), []byte("base\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		gitCommand(t, repository, "add", ".")
		gitCommand(t, repository, "commit", "-m", "initial")
	}
	wxRoot := filepath.Join(root, "wx")
	target := filepath.Join(wxRoot, "slot", "root")
	mustMkdir(t, filepath.Dir(target))
	head := gitCommand(t, first, "rev-parse", "HEAD")
	gitCommand(t, first, "worktree", "add", "--detach", target, head)
	firstRepo := discovery.Repository{ID: "first", MainPath: domain.CanonicalPath(first), CommonDir: domain.CanonicalPath(gitCommand(t, first, "rev-parse", "--path-format=absolute", "--git-common-dir"))}
	secondRepo := discovery.Repository{ID: "second", MainPath: domain.CanonicalPath(second), CommonDir: domain.CanonicalPath(gitCommand(t, second, "rev-parse", "--path-format=absolute", "--git-common-dir"))}
	markOwnedWorktree(t, wxRoot, target, "slot", firstRepo)
	manager := removalManager(t, wxRoot, allowOwnershipValidator{})
	pointAtSlot(t, manager, wxRoot, target)
	// marker は所属 repository 名で作るため、別 repository を指定すれば marker が見つからず拒否される。
	// これは本番で common directory 比較が行うフェイルクローズと同じ拒否を、より早く検証する。
	if err := manager.RemoveWorktree(context.Background(), secondRepo, wxRoot, target, ""); !errors.Is(err, state.ErrOwnership) || !strings.Contains(err.Error(), "ownership marker") {
		t.Fatalf("wrong repository removal error=%v", err)
	}
	if err := os.RemoveAll(target); err != nil {
		t.Fatal(err)
	}
	if err := manager.RemoveWorktree(context.Background(), firstRepo, wxRoot, target, head); err != nil {
		t.Fatalf("missing registered worktree reconciliation: %v", err)
	}
	if output := gitCommand(t, first, "worktree", "list", "--porcelain"); strings.Contains(output, target) {
		t.Fatal("missing worktree registration remains")
	}
}

func TestRemoveWorktreePropagatesGitFailures(t *testing.T) {
	for _, pattern := range []string{
		" worktree list --porcelain -z ",
		" rev-parse --path-format=absolute --git-common-dir ",
		" worktree remove --force ",
	} {
		t.Run(strings.TrimSpace(pattern), func(t *testing.T) {
			repository, repo, manager, worktreeRoot := archiveFixture(t)
			head := gitCommand(t, repository, "rev-parse", "HEAD")
			target := filepath.Join(worktreeRoot, "slot", "root")
			mustMkdir(t, filepath.Dir(target))
			gitCommand(t, repository, "worktree", "add", "--detach", target, head)
			markOwnedWorktree(t, worktreeRoot, target, "slot", repo)
			installGitFault(t, pattern, 1)
			if err := manager.RemoveWorktree(context.Background(), repo, worktreeRoot, target, head); err == nil {
				t.Fatal("worktree removal succeeded despite injected Git failure")
			}
		})
	}
}

func TestRemoveWorktreePropagatesRevalidationGitFailures(t *testing.T) {
	for _, test := range []struct {
		name       string
		pattern    string
		occurrence int
	}{
		{name: "unlock", pattern: " worktree unlock ", occurrence: 1},
		{name: "post-unlock lock status", pattern: " worktree list --porcelain -z ", occurrence: 2},
		{name: "post-unlock common dir", pattern: " rev-parse --path-format=absolute --git-common-dir ", occurrence: 2},
		{name: "post-unlock head", pattern: " rev-parse HEAD ", occurrence: 2},
	} {
		t.Run(test.name, func(t *testing.T) {
			repository, repo, manager, worktreeRoot := archiveFixture(t)
			head := gitCommand(t, repository, "rev-parse", "HEAD")
			target := filepath.Join(worktreeRoot, "slot", "root")
			mustMkdir(t, filepath.Dir(target))
			gitCommand(t, repository, "worktree", "add", "--detach", target, head)
			markOwnedWorktree(t, worktreeRoot, target, "slot", repo)
			installGitFault(t, test.pattern, test.occurrence)
			if err := manager.RemoveWorktree(context.Background(), repo, worktreeRoot, target, head); err == nil {
				t.Fatal("worktree removal succeeded despite an injected revalidation Git failure")
			}
			if _, err := os.Lstat(target); err != nil {
				t.Fatalf("worktree was removed despite a revalidation failure: %v", err)
			}
		})
	}
}

func TestRemoveWorktreeMissingRegistrationPropagatesGitFailures(t *testing.T) {
	newMissingRegisteredFixture := func(t *testing.T) (discovery.Repository, *Manager, string, string, string) {
		t.Helper()
		repository, repo, manager, worktreeRoot := archiveFixture(t)
		head := gitCommand(t, repository, "rev-parse", "HEAD")
		target := filepath.Join(worktreeRoot, "slot", "root")
		mustMkdir(t, filepath.Dir(target))
		gitCommand(t, repository, "worktree", "add", "--detach", target, head)
		markOwnedWorktree(t, worktreeRoot, target, "slot", repo)
		if err := os.RemoveAll(target); err != nil {
			t.Fatal(err)
		}
		return repo, manager, worktreeRoot, target, head
	}

	for _, test := range []struct {
		name       string
		pattern    string
		occurrence int
	}{
		{name: "unlock", pattern: " worktree unlock ", occurrence: 1},
		{name: "post-unlock lock status", pattern: " worktree list --porcelain -z ", occurrence: 2},
		{name: "final remove", pattern: " worktree remove --force ", occurrence: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			repo, manager, worktreeRoot, target, head := newMissingRegisteredFixture(t)
			installGitFault(t, test.pattern, test.occurrence)
			if err := manager.RemoveWorktree(context.Background(), repo, worktreeRoot, target, head); err == nil {
				t.Fatal("missing-worktree reconciliation succeeded despite an injected Git failure")
			}
		})
	}
}

func TestRemoveWorktreePropagatesFilesystemOwnershipFailures(t *testing.T) {
	t.Run("non-directory path component", func(t *testing.T) {
		_, repo, manager, worktreeRoot := archiveFixture(t)
		mustMkdir(t, worktreeRoot)
		blocker := filepath.Join(worktreeRoot, "file")
		if err := os.WriteFile(blocker, []byte("not a directory"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := manager.RemoveWorktree(context.Background(), repo, worktreeRoot, filepath.Join(blocker, "child"), ""); err == nil {
			t.Fatal("removal through a non-directory path component succeeded")
		}
	})

	t.Run("missing expected common directory", func(t *testing.T) {
		repository, repo, manager, worktreeRoot := archiveFixture(t)
		head := gitCommand(t, repository, "rev-parse", "HEAD")
		target := filepath.Join(worktreeRoot, "slot", "root")
		mustMkdir(t, filepath.Dir(target))
		gitCommand(t, repository, "worktree", "add", "--detach", target, head)
		repo.CommonDir = domain.CanonicalPath(filepath.Join(t.TempDir(), "missing-common-dir"))
		if err := manager.RemoveWorktree(context.Background(), repo, worktreeRoot, target, head); err == nil {
			t.Fatal("removal with missing ownership directory succeeded")
		}
	})
}
