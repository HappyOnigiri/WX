package daemon

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/HappyOnigiri/WX/internal/config"
	"github.com/HappyOnigiri/WX/internal/discovery"
	"github.com/HappyOnigiri/WX/internal/domain"
	"github.com/HappyOnigiri/WX/internal/state"
)

func TestWorkspaceRecoveryExclusionsUseSlotDirectoryNames(t *testing.T) {
	t.Parallel()
	cfg := config.Defaults()
	cfg.Workspaces["/src/bundle"] = config.Workspace{Link: []string{"shared"}}
	w := discoveryWorkspaceForExclusions()
	repos := []state.SlotRepository{{RepositoryID: "repo-1", DirName: "server"}}
	got := workspaceRecoveryExclusions(w, repos, cfg)
	want := map[string]bool{"server": true, ".wx-owner-repo-1": true, "shared": true}
	if len(got) != len(want) {
		t.Fatalf("exclusions=%v want keys %v", got, want)
	}
	for _, value := range got {
		if !want[value] {
			t.Fatalf("exclusions=%v contains unexpected %q", got, value)
		}
	}
	if containsString(got, w.Repositories[0].RelativePath) {
		t.Fatalf("exclusions=%v still use the source-relative repository path", got)
	}
	if got := workspaceRecoveryExclusions(w, []state.SlotRepository{{RepositoryID: "repo-1"}}, config.Defaults()); len(got) != 0 {
		t.Fatalf("nameless repository exclusions=%v", got)
	}
}

func discoveryWorkspaceForExclusions() discovery.Workspace {
	return discovery.Workspace{
		ID: "wsp001", Root: "/src/bundle", Kind: "multi_repository",
		Repositories: []discovery.Repository{{ID: "repo-1", RelativePath: filepath.Join("group", "server")}},
	}
}

func TestResumeRestoreJobQuarantinesWhenParentSnapshotJobFailed(t *testing.T) {
	t.Parallel()
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

func TestResumeRestoreJobQuarantinesOnIncompleteRepositorySnapshotSet(t *testing.T) {
	t.Parallel()
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
