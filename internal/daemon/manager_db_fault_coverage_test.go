package daemon

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/HappyOnigiri/WX/internal/archive"
	"github.com/HappyOnigiri/WX/internal/config"
	"github.com/HappyOnigiri/WX/internal/discovery"
	"github.com/HappyOnigiri/WX/internal/domain"
	"github.com/HappyOnigiri/WX/internal/state"
)

// TestScheduleColdRepositoryRemovalsSurvivesQuarantineStorageFailure
// verifies that quarantineCleanupFailure logs (rather than panics or
// silently loses) a storage failure while quarantining a slot whose cold
// repository path could not be verified, keeping the calling GC sweep alive
// for the rest of the candidates in the batch.
func TestScheduleColdRepositoryRemovalsSurvivesQuarantineStorageFailure(t *testing.T) {
	ctx, manager, store, workspaceRecord, resolved, databasePath := managerCoverageFixture(t)
	slotID := domain.StableID("cold-schedule", "quarantine-fault")
	outsidePath := filepath.Join(t.TempDir(), "outside-worktree-fault")
	if _, err := store.CreateStandby(ctx,
		state.Slot{ID: slotID, WorkspaceID: string(workspaceRecord.ID), Generation: 1, Path: filepath.Join(manager.Config().Storage.WorktreeRoot, "cold-schedule", slotID, "root"), State: "RETIRING"},
		[]state.SlotRepository{{RepositoryID: string(resolved[0].Repository.ID), WorktreePath: outsidePath, State: "RETIRING", BaseOID: resolved[0].OID}}); err != nil {
		t.Fatal(err)
	}

	raw, err := sql.Open("sqlite", databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	if _, err := raw.ExecContext(ctx, `CREATE TRIGGER fail_quarantine_cleanup BEFORE UPDATE ON slots WHEN NEW.state='QUARANTINED' BEGIN SELECT RAISE(ABORT,'injected quarantine failure'); END`); err != nil {
		t.Fatal(err)
	}

	candidates := []state.ColdRepositoryCandidate{{SlotID: slotID, WorkspaceID: string(workspaceRecord.ID), RepositoryID: string(resolved[0].Repository.ID), WorktreePath: outsidePath}}
	if count := manager.scheduleColdRepositoryRemovals(ctx, candidates, map[string]bool{}); count != 0 {
		t.Fatalf("unverifiable cold repository was scheduled despite injected failure: count=%d", count)
	}
	if slot, err := store.Slot(ctx, slotID); err != nil || slot.State != "RETIRING" {
		t.Fatalf("slot state changed despite injected quarantine failure: slot=%+v err=%v", slot, err)
	}
}

// TestReconcileArtifactsSurvivesQuarantineStorageFailure verifies that a
// storage failure while quarantining a missing owned artifact is logged and
// does not abort the wider reconciliation pass (a startup routine that must
// keep making progress across every artifact even when one durable update
// fails).
func TestReconcileArtifactsSurvivesQuarantineStorageFailure(t *testing.T) {
	ctx, manager, store, _, _, databasePath := managerCoverageFixture(t)
	missingID := "missing-artifact"
	if _, err := store.CreateSlotSession(ctx,
		state.Slot{ID: missingID, Generation: 1, Path: filepath.Join(manager.Config().Storage.WorktreeRoot, "missing-artifact"), State: "LEASED"},
		nil,
		state.Session{ID: missingID, SlotID: missingID, State: "ACTIVE", AgentKind: "codex", TokenHash: state.HashToken(missingID)}, ""); err != nil {
		t.Fatal(err)
	}

	raw, err := sql.Open("sqlite", databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	if _, err := raw.ExecContext(ctx, `CREATE TRIGGER fail_quarantine_update BEFORE UPDATE ON slots WHEN NEW.state='QUARANTINED' BEGIN SELECT RAISE(ABORT,'injected quarantine failure'); END`); err != nil {
		t.Fatal(err)
	}

	// Must not panic and must leave the slot in its prior (non-quarantined)
	// state; the failure is logged, not silently promoted to a crash.
	manager.reconcileArtifacts(ctx)
	if slot, err := store.Slot(ctx, missingID); err != nil || slot.State != "LEASED" {
		t.Fatalf("slot state changed despite injected quarantine failure: slot=%+v err=%v", slot, err)
	}
}

// TestRemoveSlotWorktreesWrapsRepositorySnapshotStorageFailure verifies that
// a storage failure reading repository snapshots before a worktree removal
// is reported as removal metadata failure (state.ErrOwnership-compatible via
// removalMetadataFailure) rather than an opaque low-level SQL error, keeping
// the failure classification callers rely on to decide whether to
// quarantine.
func TestRemoveSlotWorktreesWrapsRepositorySnapshotStorageFailure(t *testing.T) {
	root := t.TempDir()
	databasePath := filepath.Join(root, "state.db")
	store, err := state.Open(databasePath)
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
		{ID: "repository", MainPath: discoveryPath(filepath.Join(root, "repository")), CommonDir: discoveryPath(filepath.Join(root, "repository", ".git")), DefaultBranch: "main"},
	}}
	if err := store.UpsertWorkspace(ctx, w); err != nil {
		t.Fatal(err)
	}
	slotPath := filepath.Join(cfg.Storage.WorktreeRoot, "slot")
	sessionID := "session"
	if _, err := store.CreateSlotSession(ctx,
		state.Slot{ID: "slot", WorkspaceID: "workspace", Generation: 1, Path: slotPath, State: "REMOVING"},
		nil,
		state.Session{ID: sessionID, WorkspaceID: "workspace", SlotID: "slot", State: "ARCHIVED", AgentKind: "codex", TokenHash: state.HashToken(sessionID)}, ""); err != nil {
		t.Fatal(err)
	}

	raw, err := sql.Open("sqlite", databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	if _, err := raw.Exec(`DROP TABLE snapshots`); err != nil {
		t.Fatal(err)
	}

	if err := manager.removeSlotWorktrees(ctx, archive.Manager{}, cfg.Storage.WorktreeRoot, "slot", sessionID, slotPath); err == nil {
		t.Fatal("worktree removal succeeded despite an unreadable snapshot table")
	}
}

// TestResumeRestoreJobWrapsSlotRepositoryStorageFailure verifies that a
// storage failure reading the restoring slot's own repository rows (after
// the parent recovery snapshot has already been validated as usable) is
// propagated as an error instead of proceeding to rebuild repository state
// from a read that silently returned nothing.
func TestResumeRestoreJobWrapsSlotRepositoryStorageFailure(t *testing.T) {
	root := t.TempDir()
	databasePath := filepath.Join(root, "state.db")
	store, err := state.Open(databasePath)
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
		{ID: "repository", MainPath: discoveryPath(filepath.Join(root, "repository")), CommonDir: discoveryPath(filepath.Join(root, "repository", ".git")), RelativePath: "repository", DefaultBranch: "main"},
	}}
	if err := store.UpsertWorkspace(ctx, w); err != nil {
		t.Fatal(err)
	}
	parentRepos := []state.SlotRepository{
		{RepositoryID: "repository", WorktreePath: filepath.Join(cfg.Storage.WorktreeRoot, "parent", "repository"), State: "ARCHIVED"},
	}
	parentID := "parent-db-fault"
	if _, err := store.CreateSlotSession(ctx,
		state.Slot{ID: parentID, WorkspaceID: "workspace", Generation: 1, Path: filepath.Join(cfg.Storage.WorktreeRoot, "parent"), State: "ARCHIVED"},
		parentRepos,
		state.Session{ID: parentID, WorkspaceID: "workspace", SlotID: parentID, State: "ARCHIVED", AgentKind: "codex", TokenHash: state.HashToken(parentID)}, ""); err != nil {
		t.Fatal(err)
	}
	expiry := time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano)
	if err := store.SaveSnapshot(ctx, state.Snapshot{ID: "snap-db-fault", SessionID: parentID, RepositoryID: "repository", HeadOID: "head", HeadRef: "refs/wx/recovery/head", IndexTreeOID: "index", WorktreeOID: "worktree", WorktreeRef: "refs/wx/recovery/worktree", Status: "ARCHIVED", CreatedAt: time.Now().UTC().Format(time.RFC3339Nano), ExpiresAt: expiry}); err != nil {
		t.Fatal(err)
	}
	childID := "child-db-fault"
	if _, err := store.CreateSlotSession(ctx,
		state.Slot{ID: childID, WorkspaceID: "workspace", Generation: 1, Path: filepath.Join(cfg.Storage.WorktreeRoot, "child"), State: "RESTORING"},
		nil,
		state.Session{ID: childID, WorkspaceID: "workspace", SlotID: childID, ParentSessionID: parentID, State: "RESTORING", AgentKind: "codex", TokenHash: state.HashToken(childID)}, ""); err != nil {
		t.Fatal(err)
	}

	raw, err := sql.Open("sqlite", databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	if _, err := raw.Exec(`DROP TABLE slot_repositories`); err != nil {
		t.Fatal(err)
	}

	if err := manager.resumeRestoreJob(ctx, childID); err == nil {
		t.Fatal("resume restore succeeded despite an unreadable slot_repositories table")
	}
}
