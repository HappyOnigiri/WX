package archive

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
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

type allowOwnershipValidator struct{}

func (allowOwnershipValidator) ValidateWorktreeOwnership(context.Context, state.WorktreeOwnershipRequest) (state.WorktreeOwnership, error) {
	return state.WorktreeOwnership{}, nil
}

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

// archive snapshot は、prepare と同じ pin 済み descriptor から lease 中の worktree を読む必要がある。
// 最初の Git 読取り直前に lexical root を置換しても、snapshot が置換先 path を使ってはならない。
func TestSnapshotUsesPinnedDescriptorAcrossRootReplacement(t *testing.T) {
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
	if err := os.WriteFile(filepath.Join(repository, "tracked"), []byte("base\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, repository, "add", ".")
	gitCommand(t, repository, "commit", "-m", "initial")
	head := gitCommand(t, repository, "rev-parse", "HEAD")
	target := filepath.Join(root, "slot", "root")
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, repository, "worktree", "add", "--detach", target, head)
	if err := os.WriteFile(filepath.Join(target, "tracked"), []byte("leased\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	common := gitCommand(t, repository, "rev-parse", "--path-format=absolute", "--git-common-dir")
	repo := discovery.Repository{ID: "repository", MainPath: domain.CanonicalPath(repository), CommonDir: domain.CanonicalPath(common)}
	owner, _, err := domain.OpenOwnedRoot(root, root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = owner.Close() }()
	runner := &gitx.Runner{Timeout: 5 * time.Second}
	cfg := config.Defaults()
	cfg.Storage.WorktreeRoot = root
	preparer := &workspace.Preparer{Git: runner, Config: cfg, OwnedRoot: owner, RootPath: root}
	manager := &Manager{Git: runner, Preparer: preparer}
	replaced := false
	runner.SetBeforeRunAtHook(func(args []string) {
		if replaced {
			return
		}
		if err := os.Rename(root, root+"-old"); err != nil {
			t.Fatalf("replace snapshot root: %v", err)
		}
		if err := os.Symlink(outside, root); err != nil {
			t.Fatalf("install snapshot root replacement: %v", err)
		}
		replaced = true
	})
	snapshot, err := manager.SnapshotWithPersistence(context.Background(), repo, target, "snapshot", time.Now().Add(time.Hour), nil)
	if err != nil {
		t.Fatalf("descriptor-bound snapshot failed: %v", err)
	}
	if !replaced {
		t.Fatal("snapshot descriptor barrier was not reached")
	}
	oldTarget := filepath.Join(root+"-old", "slot", "root")
	if data, readErr := os.ReadFile(filepath.Join(oldTarget, "tracked")); readErr != nil || string(data) != "leased\n" {
		t.Fatalf("pinned snapshot did not read original worktree: data=%q err=%v", data, readErr)
	}
	if _, statErr := os.Lstat(filepath.Join(outside, "slot")); !os.IsNotExist(statErr) {
		t.Fatalf("snapshot escaped into replacement root: %v", statErr)
	}
	for ref, want := range map[string]string{snapshot.HeadRef: snapshot.HeadOID, snapshot.WorktreeRef: snapshot.WorktreeOID} {
		if got := gitCommand(t, repository, "rev-parse", "--verify", ref); got != want {
			t.Fatalf("snapshot ref %s=%s want %s", ref, got, want)
		}
	}
}

func TestSnapshotRefsAreIdempotentAndDeletionChecksOwnership(t *testing.T) {
	repository, repo, manager, worktreeRoot := archiveFixture(t)
	if err := os.WriteFile(filepath.Join(repository, "tracked"), []byte("content\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, repository, "add", ".")
	gitCommand(t, repository, "commit", "-m", "content")
	temp := filepath.Dir(worktreeRoot)
	expires := time.Now().Add(time.Hour)
	snapshot, err := manager.SnapshotWithPersistence(context.Background(), repo, repository, "session", expires, nil)
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := manager.SnapshotWithPersistence(context.Background(), repo, repository, "session", expires, nil)
	if err != nil || replayed.WorktreeOID != snapshot.WorktreeOID {
		t.Fatalf("replayed snapshot=%+v err=%v", replayed, err)
	}
	conflict := snapshot
	conflict.HeadOID = strings.Repeat("0", 40)
	if err := manager.DeleteSnapshotRefs(context.Background(), repo, conflict); err == nil {
		t.Fatal("conflicting recovery ref deletion succeeded")
	}
	if err := manager.DeleteSnapshotRefs(context.Background(), repo, snapshot); err != nil {
		t.Fatal(err)
	}
	if err := manager.DeleteSnapshotRefs(context.Background(), repo, snapshot); err != nil {
		t.Fatalf("idempotent ref deletion: %v", err)
	}
	expired := state.Snapshot{ExpiresAt: time.Now().Add(-time.Second).Format(time.RFC3339Nano)}
	if err := manager.Restore(context.Background(), repo, filepath.Join(temp, "restore"), "slot", expired); err == nil {
		t.Fatal("expired restore succeeded")
	}
	unavailable := snapshot
	unavailable.ExpiresAt = expires.Format(time.RFC3339Nano)
	if err := manager.Restore(context.Background(), repo, filepath.Join(temp, "restore"), "slot", unavailable); err == nil {
		t.Fatal("restore with deleted refs succeeded")
	}
}

// TestSnapshotOfCleanWorktreeReusesHeadInsteadOfCreatingContentObjects は clean session の archive 最小化を検証する。
// staged、unstaged、untracked の変更がなければ新しい tree や recovery commit、一時 index を作らず、HEAD の tree/commit を直接記録して復元する。
func TestSnapshotOfCleanWorktreeReusesHeadInsteadOfCreatingContentObjects(t *testing.T) {
	repository, repo, manager, worktreeRoot := archiveFixture(t)
	head := gitCommand(t, repository, "rev-parse", "HEAD")
	headTree := gitCommand(t, repository, "rev-parse", "HEAD^{tree}")
	snapshot, err := manager.SnapshotWithPersistence(context.Background(), repo, repository, "clean-session", time.Now().Add(time.Hour), nil)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.HeadOID != head {
		t.Fatalf("head oid=%s, want %s", snapshot.HeadOID, head)
	}
	if snapshot.WorktreeOID != head {
		t.Fatalf("clean snapshot fabricated a new recovery commit instead of reusing HEAD: worktree=%s head=%s", snapshot.WorktreeOID, head)
	}
	if snapshot.IndexTreeOID != headTree {
		t.Fatalf("clean snapshot index tree=%s, want HEAD tree %s", snapshot.IndexTreeOID, headTree)
	}
	target := filepath.Join(worktreeRoot, "restore", "root")
	pointAtSlot(t, manager, worktreeRoot, target)
	if err := manager.Restore(context.Background(), repo, target, "restore-slot", snapshot); err != nil {
		t.Fatalf("restore from clean snapshot: %v", err)
	}
	if status := gitCommand(t, target, "status", "--porcelain"); status != "" {
		t.Fatalf("restored clean worktree is not clean: %q", status)
	}
	if restoredHead := gitCommand(t, target, "rev-parse", "HEAD"); restoredHead != head {
		t.Fatalf("restored head=%s, want %s", restoredHead, head)
	}
}

// TestSnapshotDoesNotTakeCleanShortcutWhenGitStatusIsBlinded は、未 snapshot の作業を失わないための clean short-circuit を検証する。
// `git status` は設定で内容を隠せる一方、dirty 経路の一時 index には assume-unchanged/skip-worktree bit がないため `add -A` は内容を記録する。
// short-circuit が隠れた状態を信頼すると、recovery snapshot を作った後に slot worktree を物理削除して作業を失う。
// commentlint:allow-long -- `git status` と一時 index の観測差が安全条件であるため
func TestSnapshotDoesNotTakeCleanShortcutWhenGitStatusIsBlinded(t *testing.T) {
	for _, test := range []struct {
		name    string
		blind   func(t *testing.T, repository string)
		path    string
		content string
	}{
		{
			name: "untracked hidden by status.showUntrackedFiles",
			blind: func(t *testing.T, repository string) {
				gitCommand(t, repository, "config", "status.showUntrackedFiles", "no")
				if err := os.WriteFile(filepath.Join(repository, "untracked"), []byte("unsaved\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
			path:    "untracked",
			content: "unsaved\n",
		},
		{
			name: "modification hidden by assume-unchanged",
			blind: func(t *testing.T, repository string) {
				if err := os.WriteFile(filepath.Join(repository, "tracked"), []byte("unsaved\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				gitCommand(t, repository, "update-index", "--assume-unchanged", "tracked")
			},
			path:    "tracked",
			content: "unsaved\n",
		},
		{
			name: "modification hidden by skip-worktree",
			blind: func(t *testing.T, repository string) {
				if err := os.WriteFile(filepath.Join(repository, "tracked"), []byte("unsaved\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				gitCommand(t, repository, "update-index", "--skip-worktree", "tracked")
			},
			path:    "tracked",
			content: "unsaved\n",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			repository, repo, manager, worktreeRoot := archiveFixture(t)
			test.blind(t, repository)
			if status := gitCommand(t, repository, "status", "--porcelain=v1"); status != "" {
				t.Fatalf("fixture does not actually blind git status: %q", status)
			}
			head := gitCommand(t, repository, "rev-parse", "HEAD")
			snapshot, err := manager.SnapshotWithPersistence(context.Background(), repo, repository, "blinded", time.Now().Add(time.Hour), nil)
			if err != nil {
				t.Fatal(err)
			}
			if snapshot.WorktreeOID == head {
				t.Fatal("clean shortcut was taken while git status was hiding worktree content")
			}
			target := filepath.Join(worktreeRoot, "restore", "root")
			pointAtSlot(t, manager, worktreeRoot, target)
			if err := manager.Restore(context.Background(), repo, target, "restore-slot", snapshot); err != nil {
				t.Fatalf("restore from snapshot: %v", err)
			}
			restored, err := os.ReadFile(filepath.Join(target, test.path))
			if err != nil {
				t.Fatalf("hidden content was not restored: %v", err)
			}
			if string(restored) != test.content {
				t.Fatalf("restored %s=%q, want %q", test.path, restored, test.content)
			}
		})
	}
}

// TestSnapshotFailsClosedWhenCleanlinessCannotBeDetermined は、clean 判定 probe の失敗時に short-circuit せず snapshot を中止することを検証する。
func TestSnapshotFailsClosedWhenCleanlinessCannotBeDetermined(t *testing.T) {
	for _, test := range []struct {
		name    string
		pattern string
		message string
	}{
		{name: "status", pattern: " status --porcelain=v1", message: "check worktree cleanliness"},
		{name: "index stat flags", pattern: " ls-files -v", message: "inspect index stat flags"},
	} {
		t.Run(test.name, func(t *testing.T) {
			repository, repo, manager, _ := archiveFixture(t)
			installGitFault(t, test.pattern, 1)
			_, err := manager.SnapshotWithPersistence(context.Background(), repo, repository, "fault", time.Now().Add(time.Hour), nil)
			if err == nil || !strings.Contains(err.Error(), test.message) {
				t.Fatalf("snapshot did not fail closed on an unusable cleanliness probe: %v", err)
			}
		})
	}
}

func TestSnapshotWithPersistenceCommitsMetadataBeforePublishingRefs(t *testing.T) {
	repository, repo, manager, _ := archiveFixture(t)
	ctx := context.Background()
	persisted := false
	snapshot, err := manager.SnapshotWithPersistence(ctx, repo, repository, "durable-boundary", time.Now().Add(time.Hour), func(snapshot state.Snapshot) error {
		persisted = true
		listed, listErr := manager.Git.Run(ctx, repository, "for-each-ref", "--format=%(refname)", "refs/wx/recovery")
		if listErr != nil {
			return listErr
		}
		if strings.TrimSpace(listed.Stdout) != "" {
			return errors.New("recovery refs were visible before metadata persistence")
		}
		if snapshot.HeadRef == "" || snapshot.WorktreeRef == "" || snapshot.HeadOID == "" || snapshot.WorktreeOID == "" {
			return errors.New("persistence callback received incomplete snapshot")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !persisted {
		t.Fatal("snapshot persistence callback was not called")
	}
	for ref, want := range map[string]string{snapshot.HeadRef: snapshot.HeadOID, snapshot.WorktreeRef: snapshot.WorktreeOID} {
		if got := gitCommand(t, repository, "rev-parse", "--verify", ref); got != want {
			t.Fatalf("published recovery ref %s=%s, want %s", ref, got, want)
		}
	}
}

func TestSnapshotWithPersistenceDoesNotPublishRefsWhenMetadataPersistenceFails(t *testing.T) {
	repository, repo, manager, _ := archiveFixture(t)
	wantErr := errors.New("metadata transaction rolled back")
	_, err := manager.SnapshotWithPersistence(context.Background(), repo, repository, "persistence-failure", time.Now().Add(time.Hour), func(state.Snapshot) error {
		return wantErr
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("persistence error=%v, want %v", err, wantErr)
	}
	if refs := gitCommand(t, repository, "for-each-ref", "--format=%(refname)", "refs/wx/recovery"); refs != "" {
		t.Fatalf("recovery refs published after persistence failure: %s", refs)
	}
}

func TestArchiveRejectsUnownedAndMismatchedWorktrees(t *testing.T) {
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
	common := gitCommand(t, repository, "rev-parse", "--path-format=absolute", "--git-common-dir")
	repo := discovery.Repository{ID: "repository", MainPath: domain.CanonicalPath(repository), CommonDir: domain.CanonicalPath(common)}
	manager := removalManager(t, root, allowOwnershipValidator{})
	ctx := context.Background()

	if err := manager.RemoveWorktree(ctx, repo, root, repository, head); err == nil || !strings.Contains(err.Error(), "outside") {
		t.Fatalf("outside removal error=%v", err)
	}
	missing := filepath.Join(root, "missing")
	if err := manager.RemoveWorktree(ctx, repo, root, missing, head); err != nil {
		t.Fatalf("idempotent missing removal: %v", err)
	}
	registered := filepath.Join(root, "slot", "repo")
	mustMkdir(t, filepath.Dir(registered))
	gitCommand(t, repository, "worktree", "add", "--detach", registered, head)
	unowned := filepath.Join(root, "foreign", "root")
	mustMkdir(t, filepath.Dir(unowned))
	gitCommand(t, repository, "worktree", "add", "--detach", unowned, head)
	pointAtSlot(t, manager, root, unowned)
	if err := manager.RemoveWorktree(ctx, repo, root, unowned, head); err == nil || !strings.Contains(err.Error(), "ownership marker") {
		t.Fatalf("unowned registered worktree removal error=%v", err)
	}
	if _, err := os.Stat(filepath.Join(unowned, ".git")); err != nil {
		t.Fatalf("unowned registered worktree was removed: %v", err)
	}
	markOwnedWorktree(t, root, registered, "slot", repo)
	pointAtSlot(t, manager, root, registered)
	if err := manager.RemoveWorktree(ctx, repo, root, registered, strings.Repeat("0", 40)); err == nil || !strings.Contains(err.Error(), "HEAD") {
		t.Fatalf("mismatched HEAD removal error=%v", err)
	}
	if _, err := manager.SnapshotWithPersistence(ctx, repo, filepath.Join(temp, "not-a-worktree"), "bad", time.Now().Add(time.Hour), nil); err == nil {
		t.Fatal("snapshot of missing worktree succeeded")
	}

	invalidRef := state.Snapshot{HeadRef: "not a ref", HeadOID: head, WorktreeRef: "also bad", WorktreeOID: head}
	if err := manager.DeleteSnapshotRefs(ctx, repo, invalidRef); err == nil || !strings.Contains(err.Error(), "invalid recovery ref") {
		t.Fatalf("invalid ref deletion error=%v", err)
	}
	if err := manager.RemoveWorktree(ctx, repo, root, registered, head); err != nil {
		t.Fatal(err)
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

func TestSnapshotRejectsConflictingExistingRecoveryRef(t *testing.T) {
	repository, repo, manager, _ := archiveFixture(t)
	snapshot, err := manager.SnapshotWithPersistence(context.Background(), repo, repository, "session", time.Now().Add(time.Hour), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repository, "tracked"), []byte("second\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, repository, "add", ".")
	gitCommand(t, repository, "commit", "-m", "second")
	newHead := gitCommand(t, repository, "rev-parse", "HEAD")
	gitCommand(t, repository, "update-ref", snapshot.HeadRef, newHead)
	if _, err := manager.SnapshotWithPersistence(context.Background(), repo, repository, "session", time.Now().Add(time.Hour), nil); err == nil || !strings.Contains(err.Error(), "unexpected object") {
		t.Fatalf("conflicting recovery ref error=%v", err)
	}
}

func TestEnsureRecoveryRefPropagatesCreationFailure(t *testing.T) {
	repository, repo, manager, _ := archiveFixture(t)
	head := gitCommand(t, repository, "rev-parse", "HEAD")
	installGitFault(t, " update-ref --create-reflog ", 1)
	ref := "refs/wx/recovery/fault/repository/head"
	if err := manager.ensureRecoveryRef(context.Background(), repo, ref, head); err == nil {
		t.Fatal("recovery ref creation succeeded despite injected Git failure")
	}
}

func TestRestorePropagatesPreparationAndIndexFailures(t *testing.T) {
	repository, repo, manager, worktreeRoot := archiveFixture(t)
	root := filepath.Dir(worktreeRoot)
	runner := manager.Git
	snapshot, err := manager.SnapshotWithPersistence(context.Background(), repo, repository, "session", time.Now().Add(time.Hour), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Restore(context.Background(), repo, filepath.Join(root, "outside"), "outside", snapshot); err == nil || !strings.Contains(err.Error(), "outside") {
		t.Fatalf("outside restore error=%v", err)
	}
	badIndex := snapshot
	badIndex.IndexTreeOID = "not-an-object"
	target := filepath.Join(worktreeRoot, "bad-index", "root")
	pointAtSlot(t, manager, worktreeRoot, target)
	if err := manager.Restore(context.Background(), repo, target, "bad-index", badIndex); err == nil {
		t.Fatal("restore with invalid index tree succeeded")
	}
	for ref, want := range map[string]string{snapshot.HeadRef: snapshot.HeadOID, snapshot.WorktreeRef: snapshot.WorktreeOID} {
		if got := gitCommand(t, repository, "rev-parse", "--verify", ref); got != want {
			t.Fatalf("restore failure changed snapshot ref %s: got %s want %s", ref, got, want)
		}
	}
	_, _ = runner.Run(context.Background(), repository, "worktree", "unlock", target)
	_, _ = runner.Run(context.Background(), repository, "worktree", "remove", "--force", target)
}

// Finding 5: resume prepare は復元済みの snapshot tree と index を観測する。
func TestRestoreRunsPrepareCommandAfterSnapshotTreeAndIndex(t *testing.T) {
	root := t.TempDir()
	repository := filepath.Join(root, "repository")
	worktreeRoot := filepath.Join(root, "worktrees")
	source := filepath.Join(worktreeRoot, "source")
	mustMkdir(t, repository)
	gitCommand(t, repository, "init", "-b", "main")
	gitCommand(t, repository, "config", "user.name", "test")
	gitCommand(t, repository, "config", "user.email", "test@example.com")
	if err := os.WriteFile(filepath.Join(repository, "state.txt"), []byte("base\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, repository, "add", "state.txt")
	gitCommand(t, repository, "commit", "-m", "initial")
	head := gitCommand(t, repository, "rev-parse", "HEAD")
	gitCommand(t, repository, "worktree", "add", "--detach", source, head)
	if err := os.WriteFile(filepath.Join(source, "state.txt"), []byte("snapshot\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	common := gitCommand(t, repository, "rev-parse", "--path-format=absolute", "--git-common-dir")
	repo := discovery.Repository{ID: "repository", MainPath: domain.CanonicalPath(repository), CommonDir: domain.CanonicalPath(common)}
	marker := filepath.Join(root, "resume-prepare-ran")
	t.Setenv("WX_RESTORE_PREPARE_MARKER", marker)
	cfg := config.Defaults()
	cfg.Storage.WorktreeRoot = worktreeRoot
	cfg.Repositories = map[string]config.Repository{
		repository: {Prepare: config.Prepare{Command: []string{"/bin/sh", "-c", `test "$(cat state.txt)" = "snapshot" && printf ran > "$WX_RESTORE_PREPARE_MARKER"`}, Timeout: config.Duration{Duration: time.Second}}},
	}
	runner := &gitx.Runner{Timeout: 5 * time.Second}
	mustMkdir(t, worktreeRoot)
	owner, _, err := domain.OpenOwnedRoot(worktreeRoot, worktreeRoot)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = owner.Close() }()
	preparer := &workspace.Preparer{Git: runner, Config: cfg, Ownership: allowOwnershipValidator{}, OwnedRoot: owner, RootPath: worktreeRoot}
	manager := &Manager{Git: runner, Preparer: preparer, Ownership: allowOwnershipValidator{}}
	snapshot, err := manager.SnapshotWithPersistence(context.Background(), repo, source, "source-session", time.Now().Add(time.Hour), nil)
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(worktreeRoot, "slot", "root")
	pointAtSlot(t, manager, worktreeRoot, target)
	if err := manager.Restore(context.Background(), repo, target, "slot", snapshot); err != nil {
		t.Fatalf("restore failed: %v", err)
	}
	if data, err := os.ReadFile(filepath.Join(target, "state.txt")); err != nil || string(data) != "snapshot\n" {
		t.Fatalf("restored state=%q err=%v", data, err)
	}
	if data, err := os.ReadFile(marker); err != nil || string(data) != "ran" {
		t.Fatalf("resume prepare marker=%q err=%v", data, err)
	}
	if staged := gitCommand(t, target, "diff", "--cached", "--name-only"); staged != "" {
		t.Fatalf("unexpected staged paths after restore: %s", staged)
	}
	if diff := gitCommand(t, target, "diff", "--name-only"); diff != "state.txt" {
		t.Fatalf("unexpected unstaged paths after restore: %s", diff)
	}
}

func TestSnapshotPropagatesGitStageFailures(t *testing.T) {
	for _, test := range []struct {
		name       string
		pattern    func(head string) string
		occurrence int
	}{
		{name: "index tree", pattern: func(string) string { return " write-tree " }, occurrence: 1},
		{name: "temporary index read", pattern: func(head string) string { return " read-tree " + head + " " }, occurrence: 1},
		{name: "temporary index add", pattern: func(string) string { return " add -A -- . " }, occurrence: 1},
		{name: "worktree tree", pattern: func(string) string { return " write-tree " }, occurrence: 2},
		{name: "worktree commit", pattern: func(string) string { return " commit-tree " }, occurrence: 1},
		{name: "recovery ref creation", pattern: func(string) string { return " update-ref --create-reflog " }, occurrence: 1},
		{name: "recovery ref verification", pattern: func(string) string { return " rev-parse --verify refs/wx/recovery/" }, occurrence: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			repository, repo, manager, _ := archiveFixture(t)
			head := gitCommand(t, repository, "rev-parse", "HEAD")
			// clean worktree は snapshotObjects の write-tree/read-tree/add/commit-tree を省くため、各注入 fault が実行経路に当たるよう dirty にする。
			if err := os.WriteFile(filepath.Join(repository, "tracked"), []byte("dirty\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			installGitFault(t, test.pattern(head), test.occurrence)
			if _, err := manager.SnapshotWithPersistence(context.Background(), repo, repository, "fault", time.Now().Add(time.Hour), nil); err == nil {
				t.Fatal("snapshot succeeded despite injected Git failure")
			}
		})
	}
}

func TestRestorePropagatesGitVerificationFailures(t *testing.T) {
	for _, test := range []struct {
		name       string
		pattern    func(state.Snapshot) string
		occurrence int
	}{
		{name: "worktree reset", pattern: func(state.Snapshot) string { return " read-tree --reset -u " }, occurrence: 1},
		{name: "index restore", pattern: func(snapshot state.Snapshot) string { return " read-tree " + snapshot.IndexTreeOID + " " }, occurrence: 1},
		{name: "index verification", pattern: func(state.Snapshot) string { return " write-tree " }, occurrence: 1},
		{name: "verification index read", pattern: func(snapshot state.Snapshot) string { return " read-tree " + snapshot.HeadOID + " " }, occurrence: 1},
		{name: "verification index add", pattern: func(state.Snapshot) string { return " add -A -- . " }, occurrence: 1},
		{name: "worktree verification tree", pattern: func(state.Snapshot) string { return " write-tree " }, occurrence: 2},
		{name: "expected tree lookup", pattern: func(snapshot state.Snapshot) string { return " rev-parse " + snapshot.WorktreeOID + "^{tree} " }, occurrence: 1},
		{name: "status verification", pattern: func(state.Snapshot) string { return " status --porcelain=v2 " }, occurrence: 1},
		{name: "restored head lookup", pattern: func(state.Snapshot) string { return " rev-parse HEAD " }, occurrence: 2},
		{name: "ready ownership validation", pattern: func(state.Snapshot) string { return " worktree list --porcelain -z " }, occurrence: 2},
	} {
		t.Run(test.name, func(t *testing.T) {
			repository, repo, manager, worktreeRoot := archiveFixture(t)
			snapshot, err := manager.SnapshotWithPersistence(context.Background(), repo, repository, "source", time.Now().Add(time.Hour), nil)
			if err != nil {
				t.Fatal(err)
			}
			installGitFault(t, test.pattern(snapshot), test.occurrence)
			target := filepath.Join(worktreeRoot, "fault", "root")
			pointAtSlot(t, manager, worktreeRoot, target)
			if err := manager.Restore(context.Background(), repo, target, "fault", snapshot); err == nil {
				t.Fatal("restore succeeded despite injected Git failure")
			}
		})
	}
}

func TestDeleteSnapshotRefsPropagatesGitFailures(t *testing.T) {
	for _, pattern := range []string{" show-ref --verify --hash ", " update-ref -d "} {
		t.Run(strings.TrimSpace(pattern), func(t *testing.T) {
			repository, repo, manager, _ := archiveFixture(t)
			snapshot, err := manager.SnapshotWithPersistence(context.Background(), repo, repository, "source", time.Now().Add(time.Hour), nil)
			if err != nil {
				t.Fatal(err)
			}
			installGitFault(t, pattern, 1)
			if err := manager.DeleteSnapshotRefs(context.Background(), repo, snapshot); err == nil {
				t.Fatal("snapshot ref deletion succeeded despite injected Git failure")
			}
		})
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

func archiveFixture(t *testing.T) (string, discovery.Repository, *Manager, string) {
	t.Helper()
	root := t.TempDir()
	// repository を worktreeRoot の中に置き、この fixture の Snapshot/Restore が repository 自身を archive 対象 worktree として扱えるようにする。
	// これで Preparer の pin 済み root を通る descriptor-bound 経路を通り、設定済み wx root 配下の worktree だけで Snapshot を呼ぶ本番と一致する。
	worktreeRoot := filepath.Join(root, "worktrees")
	mustMkdir(t, worktreeRoot)
	repository := filepath.Join(worktreeRoot, "repository")
	mustMkdir(t, repository)
	gitCommand(t, repository, "init", "-b", "main")
	gitCommand(t, repository, "config", "user.name", "test")
	gitCommand(t, repository, "config", "user.email", "test@example.com")
	if err := os.WriteFile(filepath.Join(repository, "tracked"), []byte("base\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, repository, "add", ".")
	gitCommand(t, repository, "commit", "-m", "initial")
	common := gitCommand(t, repository, "rev-parse", "--path-format=absolute", "--git-common-dir")
	repo := discovery.Repository{ID: "repository", MainPath: domain.CanonicalPath(repository), CommonDir: domain.CanonicalPath(common)}
	cfg := config.Defaults()
	cfg.Storage.WorktreeRoot = worktreeRoot
	runner := &gitx.Runner{Timeout: 5 * time.Second}
	owner, _, err := domain.OpenOwnedRoot(worktreeRoot, worktreeRoot)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = owner.Close() })
	preparer := &workspace.Preparer{Git: runner, Config: cfg, Ownership: allowOwnershipValidator{}, OwnedRoot: owner, RootPath: worktreeRoot}
	manager := &Manager{Git: runner, Preparer: preparer, Ownership: allowOwnershipValidator{}}
	return repository, repo, manager, worktreeRoot
}

func installGitFault(t *testing.T, pattern string, occurrence int) {
	t.Helper()
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	marker := filepath.Join(bin, "matches")
	wrapper := filepath.Join(bin, "git")
	script := `#!/bin/sh
if case " $* " in *"$WX_FAULT_PATTERN"*) true;; *) false;; esac; then
  count=0
  if [ -f "$WX_FAULT_MARKER" ]; then read -r count < "$WX_FAULT_MARKER"; fi
  count=$((count + 1))
  printf '%s\n' "$count" > "$WX_FAULT_MARKER"
  if [ "$count" -eq "$WX_FAULT_OCCURRENCE" ]; then
    printf 'injected git failure\n' >&2
    exit 2
  fi
fi
exec "$WX_REAL_GIT" "$@"
`
	if err := os.WriteFile(wrapper, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WX_REAL_GIT", realGit)
	t.Setenv("WX_FAULT_PATTERN", pattern)
	t.Setenv("WX_FAULT_MARKER", marker)
	t.Setenv("WX_FAULT_OCCURRENCE", strconv.Itoa(occurrence))
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// removalManager は root 配下の worktree の所有権を実際に証明できる manager を作る。
// 削除には、root に pin した OwnedRoot を使う descriptor-bound guard と、SQLite の root generation/root-relative slot path の両方が必要である。
// pointAtSlot が target ごとの slot を指定する。
func removalManager(t *testing.T, root string, ownership state.OwnershipValidator) *Manager {
	t.Helper()
	owner, _, err := domain.OpenOwnedRoot(root, root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = owner.Close() })
	runner := &gitx.Runner{Timeout: 5 * time.Second}
	cfg := config.Defaults()
	cfg.Storage.WorktreeRoot = root
	preparer := &workspace.Preparer{Git: runner, Config: cfg, Ownership: ownership, OwnedRoot: owner, RootPath: root, RootID: testRootID}
	return &Manager{Git: runner, Preparer: preparer, Ownership: ownership}
}

func mustMkdir(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o700); err != nil {
		t.Fatal(err)
	}
}

// markerIdentityFor は wx が slot 内の repository に書く marker の identity を作る。
// marker は `.wx-owner-<repository_id>` であり、親 directory と root generation は target path から導出せず fixture の値を使う。
func markerIdentityFor(repo discovery.Repository, slotID string) workspace.MarkerIdentity {
	return workspace.MarkerIdentity{SlotID: slotID, RootID: testRootID, RepositoryID: string(repo.ID)}
}

func markOwnedWorktree(t *testing.T, root, target, slotID string, repo discovery.Repository) {
	t.Helper()
	owner, _, err := domain.OpenOwnedRoot(root, root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = owner.Close() }()
	if err := workspace.EnsureOwnershipMarkerAt(owner, root, target, markerIdentityFor(repo, slotID), string(repo.CommonDir)); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, filepath.Dir(string(repo.CommonDir)), "worktree", "lock", "--reason", "wx:"+slotID+":READY", target)
}

// pointAtSlot は fixture の Preparer に worktree の所属 slot を設定する。
// 所有権証明は絶対 path を比較しないため、削除/検証で slot の root 相対位置と repository directory 名を指定できる必要がある。
func pointAtSlot(t *testing.T, manager *Manager, root, target string) {
	t.Helper()
	slotPath := filepath.Dir(target)
	relative, err := filepath.Rel(root, slotPath)
	if err != nil {
		t.Fatal(err)
	}
	manager.Preparer.SlotPath = slotPath
	manager.Preparer.SlotRelPath = relative
	manager.Preparer.RootID = testRootID
}

func gitCommand(t *testing.T, dir string, args ...string) string {
	t.Helper()
	command := exec.Command("git", args...)
	command.Dir = dir
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, output)
	}
	return strings.TrimSpace(string(output))
}
