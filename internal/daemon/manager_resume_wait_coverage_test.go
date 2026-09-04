package daemon

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/HappyOnigiri/WX/internal/archive"
	"github.com/HappyOnigiri/WX/internal/config"
	"github.com/HappyOnigiri/WX/internal/discovery"
	"github.com/HappyOnigiri/WX/internal/domain"
	"github.com/HappyOnigiri/WX/internal/state"
)

// TestRemoveSlotWorktreesRejectsRepositoryOutsideRoot verifies that a
// repository directory name that escapes the wx root is rejected as an
// ownership failure instead of being handed to
// archiveManager.RemoveWorktree, which could otherwise be pointed at an
// arbitrary filesystem path.
//
// slot_repositories.dir_name is plain text, so a corrupted or forged row can
// still spell a traversal even though every name wx writes is a single
// component. The derived worktree path is what removal checks.
func TestRemoveSlotWorktreesRejectsRepositoryOutsideRoot(t *testing.T) {
	ctx, manager, store, workspaceRecord, resolved, _ := managerCoverageFixture(t)
	root := manager.Config().Storage.WorktreeRoot
	slotID := domain.StableID("remove-slot", "outside-repo")
	slot := testSlot(t, manager, string(workspaceRecord.ID), slotID, 1, "REMOVING")
	// The slot is two levels below the root, so three levels of traversal
	// leave the root entirely.
	escaping := filepath.Join("..", "..", "..", "outside-repo")
	if _, err := store.CreateStandby(ctx, slot,
		[]state.SlotRepository{{RepositoryID: string(resolved[0].Repository.ID), DirName: escaping, State: "REMOVING", BaseOID: resolved[0].OID}}); err != nil {
		t.Fatal(err)
	}
	stored, err := store.SlotRepository(ctx, slotID, string(resolved[0].Repository.ID))
	if err != nil {
		t.Fatal(err)
	}
	if domain.IsWithin(root, stored.WorktreePath) {
		t.Fatalf("derived worktree path %s did not escape root %s; the case under test no longer applies", stored.WorktreePath, root)
	}
	if err := manager.removeSlotWorktrees(ctx, archive.Manager{}, root, slot, ""); !errors.Is(err, state.ErrOwnership) {
		t.Fatalf("repository outside root error=%v", err)
	}
}

// TestRemoveSlotWorktreesRejectsReplacedSlotDirectory proves that the last
// proof before the destructive RemoveAll presents the identity of the
// directory on disk. A slot directory that was deleted and recreated under
// the same name keeps its root generation and root-relative path, so only the
// inode comparison can tell it apart from the one SQLite recorded.
func TestRemoveSlotWorktreesRejectsReplacedSlotDirectory(t *testing.T) {
	ctx, manager, store, workspaceRecord, _, _ := managerCoverageFixture(t)
	root := manager.Config().Storage.WorktreeRoot
	slotID := domain.StableID("remove-slot", "replaced-directory")
	slot := testSlot(t, manager, string(workspaceRecord.ID), slotID, 1, "REMOVING")
	if _, err := store.CreateStandby(ctx, slot, nil); err != nil {
		t.Fatal(err)
	}
	// Removing and recreating the directory under the same name is exactly
	// the substitution the identity is there to catch.
	if err := os.Remove(slot.Path); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(slot.Path, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := manager.removeSlotWorktrees(ctx, archive.Manager{}, root, slot, ""); !errors.Is(err, state.ErrOwnership) {
		t.Fatalf("replaced slot directory error=%v, want an ownership failure", err)
	}
	// Fail closed means the replacement is left on disk rather than deleted
	// as though it were wx's.
	if _, err := os.Lstat(slot.Path); err != nil {
		t.Fatalf("replaced slot directory was removed despite the failed proof: %v", err)
	}
}

// TestWaitForSnapshotReturnsImmediatelyWhenArchivedRecoveryIsUsable proves
// waitForSnapshot's success path: once a session has reached ARCHIVED with a
// non-expired repository snapshot, it returns without waiting out the
// readiness timeout.
func TestWaitForSnapshotReturnsImmediatelyWhenArchivedRecoveryIsUsable(t *testing.T) {
	ctx, manager, store, workspaceRecord, resolved, _ := managerCoverageFixture(t, "repository")
	repository := resolved[0].Repository
	sessionID := domain.StableID("wait-snapshot", "archived-usable")
	slotPath := filepath.Join(manager.Config().Storage.WorktreeRoot, "wait-snapshot", sessionID, "root")
	if err := os.MkdirAll(slotPath, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateSlotSession(ctx,
		slotAtPath(t, manager, string(workspaceRecord.ID), sessionID, slotPath, 1, "SNAPSHOTTED"),
		[]state.SlotRepository{{RepositoryID: string(repository.ID), DirName: testDirName(repository, manager.Config()), State: "ARCHIVED", BaseOID: resolved[0].OID}},
		state.Session{ID: sessionID, WorkspaceID: string(workspaceRecord.ID), SlotID: sessionID, State: "ARCHIVED", AgentKind: "codex", TokenHash: state.HashToken(sessionID)}, ""); err != nil {
		t.Fatal(err)
	}
	expiry := time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano)
	if err := store.SaveSnapshot(ctx, state.Snapshot{ID: "snap", SessionID: sessionID, RepositoryID: string(repository.ID), HeadOID: resolved[0].OID, HeadRef: "refs/wx/recovery/head", IndexTreeOID: "index", WorktreeOID: "worktree", WorktreeRef: "refs/wx/recovery/worktree", Status: "ARCHIVED", CreatedAt: time.Now().UTC().Format(time.RFC3339Nano), ExpiresAt: expiry}); err != nil {
		t.Fatal(err)
	}
	session, snaps, err := manager.waitForSnapshot(ctx, sessionID)
	if err != nil {
		t.Fatalf("archived usable recovery wait: %v", err)
	}
	if session.State != "ARCHIVED" || len(snaps) != 1 {
		t.Fatalf("archived usable recovery result session=%+v snaps=%v", session, snaps)
	}
}

// TestResumeWaitsForInFlightSnapshotBeforeEvaluatingRecovery verifies that
// Resume defers to waitForSnapshot when the session it is asked to resume is
// still mid-archive (SNAPSHOTTING), rather than immediately evaluating a
// recovery snapshot set that has not been written yet.
func TestResumeWaitsForInFlightSnapshotBeforeEvaluatingRecovery(t *testing.T) {
	root := t.TempDir()
	store, err := state.Open(filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cfg := config.Defaults()
	cfg.Storage.WorktreeRoot = filepath.Join(root, "worktrees")
	cfg.Readiness.Timeout.Duration = 20 * time.Millisecond
	manager := testManager(t, cfg, store)
	defer manager.Close()
	ctx := context.Background()
	sessionID := "snapshotting"
	if _, err := store.CreateSlotSession(ctx,
		slotAtPath(t, manager, "", sessionID, filepath.Join(cfg.Storage.WorktreeRoot, sessionID), 0, "SNAPSHOTTING"),
		nil,
		state.Session{ID: sessionID, SlotID: sessionID, State: "SNAPSHOTTING", AgentKind: "codex", TokenHash: state.HashToken(sessionID)}, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Resume(ctx, sessionID, "codex", os.Getpid(), false); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("resume of an in-flight snapshot did not wait: %v", err)
	}
}

// TestResumeReportsIncompleteRecoverySnapshotAcrossRepositories verifies
// that Resume refuses to build a lease from a recovery snapshot set that
// does not cover every repository in the archived workspace, instead of
// silently omitting the missing repository from the resumed session.
func TestResumeReportsIncompleteRecoverySnapshotAcrossRepositories(t *testing.T) {
	root := t.TempDir()
	store, err := state.Open(filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cfg := config.Defaults()
	cfg.Storage.WorktreeRoot = filepath.Join(root, "worktrees")
	manager := testManager(t, cfg, store)
	defer manager.Close()
	ctx := context.Background()

	w := discovery.Workspace{ID: "workspace", Root: discoveryPath(root), Kind: "repository", Repositories: []discovery.Repository{
		{ID: "repository-1", MainPath: discoveryPath(filepath.Join(root, "repository-1")), CommonDir: discoveryPath(filepath.Join(root, "repository-1", ".git")), RelativePath: "repository-1", DefaultBranch: "main"},
		{ID: "repository-2", MainPath: discoveryPath(filepath.Join(root, "repository-2")), CommonDir: discoveryPath(filepath.Join(root, "repository-2", ".git")), RelativePath: "repository-2", DefaultBranch: "main"},
	}}
	w = registerTestWorkspace(t, store, w)
	sessionRepos := []state.SlotRepository{
		{RepositoryID: "repository-1", DirName: "repository-1", State: "ARCHIVED"},
		{RepositoryID: "repository-2", DirName: "repository-2", State: "ARCHIVED"},
	}
	sessionID := "old-session"
	if _, err := store.CreateSlotSession(ctx,
		slotAtPath(t, manager, string(w.ID), sessionID, filepath.Join(cfg.Storage.WorktreeRoot, "old"), 1, "SNAPSHOTTED"),
		sessionRepos,
		state.Session{ID: sessionID, WorkspaceID: string(w.ID), SlotID: sessionID, State: "ARCHIVED", AgentKind: "codex", TokenHash: state.HashToken(sessionID)}, ""); err != nil {
		t.Fatal(err)
	}
	expiry := time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano)
	if err := store.SaveSnapshot(ctx, state.Snapshot{ID: "snap-1", SessionID: sessionID, RepositoryID: "repository-1", HeadOID: "head", HeadRef: "refs/wx/recovery/head", IndexTreeOID: "index", WorktreeOID: "worktree", WorktreeRef: "refs/wx/recovery/worktree", Status: "ARCHIVED", CreatedAt: time.Now().UTC().Format(time.RFC3339Nano), ExpiresAt: expiry}); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Resume(ctx, sessionID, "codex", os.Getpid(), true); err == nil || !strings.Contains(err.Error(), "incomplete recovery snapshot") {
		t.Fatalf("resume with incomplete recovery snapshot error=%v", err)
	}
}
