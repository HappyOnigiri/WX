package state

import (
	"context"
	"reflect"
	"testing"
	"time"
)

func TestWorkspaceSlotPathsIncludesRetiredRootsAndAllSlotStates(t *testing.T) {
	store := openTestStore(t)
	seedWorkspace(t, store)
	seedRoot(t, store, "root-retired", "/retired-wx", "retired-identity", false)
	seedSessionScopeSlot(t, store, "slot-ready", "workspace", testRootID, "workspace/ready", "READY")
	seedSessionScopeSlot(t, store, "slot-quarantined", "workspace", "root-retired", "workspace/quarantined", "QUARANTINED")

	got, err := store.WorkspaceSlotPaths(context.Background(), "workspace")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"/retired-wx/workspace/quarantined", "/wx/workspace/ready"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("workspace slot paths=%v, want %v", got, want)
	}

	missing, err := store.WorkspaceSlotPaths(context.Background(), "missing-workspace")
	if err != nil {
		t.Fatal(err)
	}
	if len(missing) != 0 {
		t.Fatalf("missing workspace slot paths=%v, want empty", missing)
	}
}

func TestWorkspaceSessionScopesReturnsIdentityAndState(t *testing.T) {
	store := openTestStore(t)
	seedWorkspace(t, store)
	seedWorkspaceRows(t, store, "other-workspace", "/other", "single_repository", "other-repository", "/other/repository", "/other/repository/.git", "")

	base := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	seedSessionScopeSlot(t, store, "slot-native", "workspace", testRootID, "workspace/native", "LEASED")
	seedSessionScopeSession(t, store, "native", "workspace", "slot-native", "ACTIVE", "claude", "native-id", "pending-id", FormatTime(base.Add(3*time.Minute)), "")
	seedSessionScopeSlot(t, store, "slot-pending", "workspace", testRootID, "workspace/pending", "DRAINING")
	seedSessionScopeSession(t, store, "pending", "workspace", "slot-pending", "RESTORING", "codex", "", "pending-id", FormatTime(base.Add(2*time.Minute)), "")
	seedSessionScopeSlot(t, store, "slot-old", "workspace", testRootID, "workspace/old", "SNAPSHOTTED")
	seedSessionScopeSession(t, store, "old", "workspace", "slot-old", "RELEASING", "claude", "old-id", "", FormatTime(base.Add(time.Minute)), "")
	seedSessionScopeSlot(t, store, "slot-other", "other-workspace", testRootID, "other/session", "LEASED")
	seedSessionScopeSession(t, store, "other", "other-workspace", "slot-other", "ACTIVE", "codex", "other-id", "", FormatTime(base.Add(4*time.Minute)), "")

	got, err := store.WorkspaceSessionScopes(context.Background(), "workspace")
	if err != nil {
		t.Fatal(err)
	}
	want := []SessionScope{
		{ID: "native", Agent: "claude", AgentSessionID: "native-id", State: "ACTIVE"},
		{ID: "pending", Agent: "codex", AgentSessionID: "pending-id", State: "RESTORING"},
		{ID: "old", Agent: "claude", AgentSessionID: "old-id", State: "RELEASING"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("workspace session scopes=%+v, want %+v", got, want)
	}
}

func TestPreviousWorktreeUsesParentSlotAndRepositoryShape(t *testing.T) {
	store := openTestStore(t)
	seedWorkspace(t, store)
	seedSessionScopeRepository(t, store, "repository-b")

	seedSessionScopeSlot(t, store, "slot-no-parent", "workspace", testRootID, "workspace/no-parent", "LEASED")
	seedSessionScopeSession(t, store, "session-no-parent", "workspace", "slot-no-parent", "ACTIVE", "codex", "native-no-parent", "", now(), "")
	if got, err := store.PreviousWorktree(context.Background(), "session-no-parent"); err != nil || got != "" {
		t.Fatalf("session without parent previous worktree=%q err=%v", got, err)
	}

	seedSessionScopeSlot(t, store, "slot-parent-single", "workspace", testRootID, "workspace/parent-single", "SNAPSHOTTED")
	seedSessionScopeSlotRepository(t, store, "slot-parent-single", "repository", "repo-dir")
	seedSessionScopeSession(t, store, "session-parent-single", "workspace", "slot-parent-single", "ARCHIVED", "claude", "native-parent-single", "", now(), "")
	seedSessionScopeSlot(t, store, "slot-child-single", "workspace", testRootID, "workspace/child-single", "LEASED")
	seedSessionScopeSession(t, store, "session-child-single", "workspace", "slot-child-single", "ACTIVE", "claude", "native-child-single", "", now(), "session-parent-single")
	if got, err := store.PreviousWorktree(context.Background(), "session-child-single"); err != nil || got != "/wx/workspace/parent-single/repo-dir" {
		t.Fatalf("single-repository previous worktree=%q err=%v", got, err)
	}

	seedSessionScopeSlot(t, store, "slot-parent-multi", "workspace", testRootID, "workspace/parent-multi", "SNAPSHOTTED")
	seedSessionScopeSlotRepository(t, store, "slot-parent-multi", "repository", "repo-a")
	seedSessionScopeSlotRepository(t, store, "slot-parent-multi", "repository-b", "repo-b")
	seedSessionScopeSession(t, store, "session-parent-multi", "workspace", "slot-parent-multi", "ARCHIVED", "codex", "native-parent-multi", "", now(), "")
	seedSessionScopeSlot(t, store, "slot-child-multi", "workspace", testRootID, "workspace/child-multi", "LEASED")
	seedSessionScopeSession(t, store, "session-child-multi", "workspace", "slot-child-multi", "ACTIVE", "codex", "native-child-multi", "", now(), "session-parent-multi")
	if got, err := store.PreviousWorktree(context.Background(), "session-child-multi"); err != nil || got != "/wx/workspace/parent-multi" {
		t.Fatalf("multi-repository previous worktree=%q err=%v", got, err)
	}
}

func seedSessionScopeSlot(t *testing.T, store *Store, id, workspaceID, rootID, relPath, state string) {
	t.Helper()
	if _, err := store.db.ExecContext(context.Background(), `INSERT INTO slots(id,workspace_id,generation,root_id,rel_path,state,created_at,updated_at) VALUES(?,?,1,?,?,?, ?,?)`, id, workspaceID, rootID, relPath, state, now(), now()); err != nil {
		t.Fatal(err)
	}
}

func seedSessionScopeSession(t *testing.T, store *Store, id, workspaceID, slotID, state, agent, agentSessionID, pendingAgentSessionID, createdAt, parentID string) {
	t.Helper()
	if _, err := store.db.ExecContext(context.Background(), `INSERT INTO sessions(id,workspace_id,slot_id,parent_session_id,state,agent_kind,agent_session_id,session_token_hash,created_at,archived_at,expires_at,pending_agent_session_id) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`, id, workspaceID, slotID, nullString(parentID), state, agent, nullString(agentSessionID), HashToken(id), createdAt, nullString(""), nullString(""), nullString(pendingAgentSessionID)); err != nil {
		t.Fatal(err)
	}
}

func seedSessionScopeRepository(t *testing.T, store *Store, id string) {
	t.Helper()
	if _, err := store.db.ExecContext(context.Background(), `INSERT INTO repositories(id,main_worktree_path,common_git_dir,default_branch,remote_name,first_seen_at,last_seen_at) VALUES(?,?,?,'main','',?,?)`, id, "/"+id, "/"+id+"/.git", now(), now()); err != nil {
		t.Fatal(err)
	}
}

func seedSessionScopeSlotRepository(t *testing.T, store *Store, slotID, repositoryID, dirName string) {
	t.Helper()
	if _, err := store.db.ExecContext(context.Background(), `INSERT INTO slot_repositories(slot_id,repository_id,dir_name,state,requested_ref,base_oid,prepare_fingerprint) VALUES(?,?,?,?,?,?,?)`, slotID, repositoryID, dirName, "READY", "main", "head", "fingerprint"); err != nil {
		t.Fatal(err)
	}
}
