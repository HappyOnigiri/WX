package daemon

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/HappyOnigiri/WX/internal/config"
	"github.com/HappyOnigiri/WX/internal/discovery"
	"github.com/HappyOnigiri/WX/internal/domain"
	"github.com/HappyOnigiri/WX/internal/state"
)

// TestResumeRestoreJobQuarantinesWhenParentSnapshotJobFailed verifies that a
// restoring child whose parent session is still being archived, but whose
// parent slot has already failed or been quarantined, is itself quarantined
// with a descriptive error instead of being left to poll a snapshot that
// will never complete.
func TestResumeRestoreJobQuarantinesWhenParentSnapshotJobFailed(t *testing.T) {
	ctx, manager, store, workspaceRecord, _, _ := managerCoverageFixture(t)
	parentID := domain.StableID("resume-restore", "parent-failed")
	if _, err := store.CreateSlotSession(ctx,
		testSlot(t, manager, string(workspaceRecord.ID), parentID, 1, "FAILED"),
		nil,
		state.Session{ID: parentID, WorkspaceID: string(workspaceRecord.ID), SlotID: parentID, State: "RELEASING", AgentKind: "codex", TokenHash: state.HashToken(parentID)}, ""); err != nil {
		t.Fatal(err)
	}
	childID := domain.StableID("resume-restore", "child-of-failed-parent")
	if _, err := store.CreateSlotSession(ctx,
		testSlot(t, manager, string(workspaceRecord.ID), childID, 1, "RESTORING"),
		nil,
		state.Session{ID: childID, WorkspaceID: string(workspaceRecord.ID), SlotID: childID, ParentSessionID: parentID, State: "RESTORING", AgentKind: "codex", TokenHash: state.HashToken(childID)}, ""); err != nil {
		t.Fatal(err)
	}
	if err := manager.resumeRestoreJob(ctx, childID); err == nil || !strings.Contains(err.Error(), "parent snapshot job failed") {
		t.Fatalf("resume restore with failed parent job error=%v", err)
	}
	if slot, err := store.Slot(ctx, childID); err != nil || slot.State != "QUARANTINED" {
		t.Fatalf("resume restore with failed parent slot=%+v err=%v", slot, err)
	}
}

// TestResumeRestoreJobQuarantinesOnIncompleteRepositorySnapshotSet drives the
// zero-stored-repositories replay path of resumeRestoreJob: the parent
// session's recovery snapshot is usable but does not cover every repository
// in the workspace, which must quarantine the restoring slot with a
// descriptive error rather than silently resuming a partial workspace.
func TestResumeRestoreJobQuarantinesOnIncompleteRepositorySnapshotSet(t *testing.T) {
	root := t.TempDir()
	store, err := state.Open(filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cfg := config.Defaults()
	cfg.Storage.WorktreeRoot = filepath.Join(root, "worktrees")
	m := testManager(t, cfg, store)
	defer m.Close()
	ctx := context.Background()

	// Kind "repository" sidesteps the multi_repository workspace-root
	// snapshot check inside recoveryUsable, which is exercised separately;
	// this test only needs an incomplete per-repository snapshot set.
	w := discovery.Workspace{ID: "workspace", Root: discoveryPath(root), Kind: "repository", Repositories: []discovery.Repository{
		{ID: "repository-1", MainPath: discoveryPath(filepath.Join(root, "repository-1")), CommonDir: discoveryPath(filepath.Join(root, "repository-1", ".git")), RelativePath: "repository-1", DefaultBranch: "main"},
		{ID: "repository-2", MainPath: discoveryPath(filepath.Join(root, "repository-2")), CommonDir: discoveryPath(filepath.Join(root, "repository-2", ".git")), RelativePath: "repository-2", DefaultBranch: "main"},
	}}
	w = registerTestWorkspace(t, store, w)
	workspaceID := string(w.ID)

	parentID := "parent-incomplete"
	parentRepos := []state.SlotRepository{
		{RepositoryID: "repository-1", DirName: "repository-1", State: "ARCHIVED"},
		{RepositoryID: "repository-2", DirName: "repository-2", State: "ARCHIVED"},
	}
	if _, err := store.CreateSlotSession(ctx,
		slotAtPath(t, m, workspaceID, parentID, filepath.Join(cfg.Storage.WorktreeRoot, parentID), 1, "ARCHIVED"),
		parentRepos,
		state.Session{ID: parentID, WorkspaceID: workspaceID, SlotID: parentID, State: "ARCHIVED", AgentKind: "codex", TokenHash: state.HashToken(parentID)}, ""); err != nil {
		t.Fatal(err)
	}
	expiry := time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano)
	if err := store.SaveSnapshot(ctx, state.Snapshot{ID: "snapshot-1", SessionID: parentID, RepositoryID: "repository-1", HeadOID: "head", HeadRef: "refs/wx/recovery/head", IndexTreeOID: "index", WorktreeOID: "worktree", WorktreeRef: "refs/wx/recovery/worktree", Status: "ARCHIVED", CreatedAt: time.Now().UTC().Format(time.RFC3339Nano), ExpiresAt: expiry}); err != nil {
		t.Fatal(err)
	}

	childID := "child-incomplete"
	if _, err := store.CreateSlotSession(ctx,
		slotAtPath(t, m, workspaceID, childID, filepath.Join(cfg.Storage.WorktreeRoot, childID), 1, "RESTORING"),
		nil,
		state.Session{ID: childID, WorkspaceID: workspaceID, SlotID: childID, ParentSessionID: parentID, State: "RESTORING", AgentKind: "codex", TokenHash: state.HashToken(childID)}, ""); err != nil {
		t.Fatal(err)
	}

	if err := m.resumeRestoreJob(ctx, childID); err == nil || !strings.Contains(err.Error(), "snapshot missing repository") {
		t.Fatalf("resume restore with incomplete repository snapshot set error=%v", err)
	}
	if slot, err := store.Slot(ctx, childID); err != nil || slot.State != "QUARANTINED" {
		t.Fatalf("resume restore incomplete snapshot slot=%+v err=%v", slot, err)
	}
}

// TestDoctorReportsGitAndSQLiteFailures verifies Doctor surfaces a broken
// git executable and a closed state database as failing checks with the
// underlying error text, instead of masking them as "ok".
func TestDoctorReportsGitAndSQLiteFailures(t *testing.T) {
	ctx, manager, _, _, _, _ := managerCoverageFixture(t)
	t.Setenv("PATH", t.TempDir())
	checks := manager.Doctor(ctx)["checks"].(map[string]any)
	if got, ok := checks["git"].(string); !ok || got == "ok" || got == "" {
		t.Fatalf("git failure not reported: checks=%v", checks)
	}
}
