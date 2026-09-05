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

func TestScheduleColdRepositoryRemovalsSurvivesQuarantineStorageFailure(t *testing.T) {
	t.Parallel()
	ctx, manager, store, workspaceRecord, resolved, databasePath := managerCoverageFixture(t)
	slotID := domain.StableID("cold-schedule", "quarantine-fault")
	outsidePath := filepath.Join(t.TempDir(), "outside-worktree-fault")
	if _, err := store.CreateStandby(ctx,
		slotAtPath(t, manager, string(workspaceRecord.ID), slotID, filepath.Join(manager.Config().Storage.WorktreeRoot, "cold-schedule", slotID, "root"), 1, "RETIRING"),
		[]state.SlotRepository{{RepositoryID: string(resolved[0].Repository.ID), DirName: "repository", State: "RETIRING", BaseOID: resolved[0].OID}}); err != nil {
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
	if result := manager.scheduleColdRepositoryRemovals(ctx, candidates, map[string]bool{}); result.Scheduled != 0 {
		t.Fatalf("unverifiable cold repository was scheduled despite injected failure: result=%+v", result)
	}
	if slot, err := store.Slot(ctx, slotID); err != nil || slot.State != "RETIRING" {
		t.Fatalf("slot state changed despite injected quarantine failure: slot=%+v err=%v", slot, err)
	}
}

func TestReconcileArtifactsSurvivesQuarantineStorageFailure(t *testing.T) {
	t.Parallel()
	ctx, manager, store, _, _, databasePath := managerCoverageFixture(t)
	missingID := "missing-artifact"
	if _, err := store.CreateSlotSession(ctx,
		slotAtPath(t, manager, "", missingID, filepath.Join(manager.Config().Storage.WorktreeRoot, "missing-artifact"), 1, "LEASED"),
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

	manager.reconcileArtifacts(ctx)
	if slot, err := store.Slot(ctx, missingID); err != nil || slot.State != "LEASED" {
		t.Fatalf("slot state changed despite injected quarantine failure: slot=%+v err=%v", slot, err)
	}
}

func TestRemoveSlotWorktreesWrapsRepositorySnapshotStorageFailure(t *testing.T) {
	t.Parallel()
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
	registered, _, err := store.UpsertWorkspaceGeneration(ctx, w)
	if err != nil {
		t.Fatal(err)
	}
	workspaceID := string(registered.ID)
	slot := testSlot(t, manager, workspaceID, "slot", 1, "REMOVING")
	sessionID := "session"
	if _, err := store.CreateSlotSession(ctx, slot, nil,
		state.Session{ID: sessionID, WorkspaceID: workspaceID, SlotID: "slot", State: "ARCHIVED", AgentKind: "codex", TokenHash: state.HashToken(sessionID)}, ""); err != nil {
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

	if err := manager.removeSlotWorktrees(ctx, archive.Manager{}, cfg.Storage.WorktreeRoot, slot, sessionID); err == nil {
		t.Fatal("worktree removal succeeded despite an unreadable snapshot table")
	}
}

func TestResumeRestoreJobWrapsSlotRepositoryStorageFailure(t *testing.T) {
	t.Parallel()
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

	w := discovery.Workspace{Root: discoveryPath(root), Kind: "repository", Repositories: []discovery.Repository{
		{ID: "repository", MainPath: discoveryPath(filepath.Join(root, "repository")), CommonDir: discoveryPath(filepath.Join(root, "repository", ".git")), RelativePath: "repository", DefaultBranch: "main"},
	}}
	w = registerTestWorkspace(t, store, w)
	parentRepos := []state.SlotRepository{
		{RepositoryID: "repository", DirName: "repository", State: "ARCHIVED"},
	}
	parentID := "parent-db-fault"
	if _, err := store.CreateSlotSession(ctx,
		slotAtPath(t, manager, string(w.ID), parentID, filepath.Join(cfg.Storage.WorktreeRoot, "parent"), 1, "ARCHIVED"),
		parentRepos,
		state.Session{ID: parentID, WorkspaceID: string(w.ID), SlotID: parentID, State: "ARCHIVED", AgentKind: "codex", TokenHash: state.HashToken(parentID)}, ""); err != nil {
		t.Fatal(err)
	}
	expiry := time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano)
	if err := store.SaveSnapshot(ctx, state.Snapshot{ID: "snap-db-fault", SessionID: parentID, RepositoryID: "repository", HeadOID: "head", HeadRef: "refs/wx/recovery/head", IndexTreeOID: "index", WorktreeOID: "worktree", WorktreeRef: "refs/wx/recovery/worktree", Status: "ARCHIVED", CreatedAt: time.Now().UTC().Format(time.RFC3339Nano), ExpiresAt: expiry}); err != nil {
		t.Fatal(err)
	}
	childID := "child-db-fault"
	if _, err := store.CreateSlotSession(ctx,
		slotAtPath(t, manager, string(w.ID), childID, filepath.Join(cfg.Storage.WorktreeRoot, "child"), 1, "RESTORING"),
		nil,
		state.Session{ID: childID, WorkspaceID: string(w.ID), SlotID: childID, ParentSessionID: parentID, State: "RESTORING", AgentKind: "codex", TokenHash: state.HashToken(childID)}, ""); err != nil {
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
