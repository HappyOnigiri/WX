package state

import (
	"context"
	"strings"
	"testing"
)

func newPendingResumeFixture(t *testing.T, parentMapped bool) (*Store, context.Context, Session, Session) {
	t.Helper()
	store := openTestStore(t)
	ctx := context.Background()
	seedWorkspace(t, store)

	parent := Session{
		ID:          "pending-parent",
		WorkspaceID: "workspace",
		SlotID:      "pending-parent",
		State:       "ARCHIVED",
		AgentKind:   "codex",
		TokenHash:   HashToken("pending-parent"),
	}
	if _, err := store.CreateSlotSession(ctx, Slot{
		ID:          parent.SlotID,
		WorkspaceID: parent.WorkspaceID,
		Generation:  1,
		RootID:      testRootID,
		RelPath:     "workspace/pending-parent",
		State:       "SNAPSHOTTED",
	}, nil, parent, ""); err != nil {
		t.Fatal(err)
	}
	if parentMapped {
		if err := store.BindAgentSession(ctx, parent.ID, "native-pending"); err != nil {
			t.Fatal(err)
		}
	}

	child := Session{
		ID:                    "pending-child",
		WorkspaceID:           parent.WorkspaceID,
		SlotID:                "pending-child",
		ParentSessionID:       parent.ID,
		State:                 "RESTORING",
		AgentKind:             parent.AgentKind,
		PendingAgentSessionID: "native-pending",
		TokenHash:             HashToken("pending-child"),
	}
	if _, err := store.CreateSlotSession(ctx, Slot{
		ID:          child.SlotID,
		WorkspaceID: child.WorkspaceID,
		Generation:  1,
		RootID:      testRootID,
		RelPath:     "workspace/pending-child",
		State:       "RESTORING",
	}, nil, child, "RESTORE"); err != nil {
		t.Fatal(err)
	}
	return store, ctx, parent, child
}

func TestPendingRestoreBindSameIDKeepsParentMapping(t *testing.T) {
	store, ctx, parent, child := newPendingResumeFixture(t, true)
	if err := store.BindAgentSession(ctx, child.ID, "native-pending"); err != nil {
		t.Fatalf("bind pending restore: %v", err)
	}

	gotChild, err := store.SessionByID(ctx, child.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotChild.AgentSessionID != "" || gotChild.PendingAgentSessionID != "native-pending" || gotChild.State != "RESTORING" {
		t.Fatalf("same-ID bind changed child mapping: %+v", gotChild)
	}
	gotParent, err := store.FindByAgentSession(ctx, parent.AgentKind, "native-pending")
	if err != nil || gotParent.ID != parent.ID {
		t.Fatalf("same-ID bind moved parent mapping: parent=%+v err=%v", gotParent, err)
	}
}

func TestPendingRestoreBindForkIDClearsPendingAndBindsFork(t *testing.T) {
	store, ctx, parent, child := newPendingResumeFixture(t, true)
	if err := store.BindAgentSession(ctx, child.ID, "fork-agent"); err != nil {
		t.Fatalf("bind forked restore: %v", err)
	}

	gotChild, err := store.SessionByID(ctx, child.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotChild.AgentSessionID != "fork-agent" || gotChild.PendingAgentSessionID != "" || gotChild.State != "RESTORING" {
		t.Fatalf("fork bind did not replace pending mapping: %+v", gotChild)
	}
	gotParent, err := store.FindByAgentSession(ctx, parent.AgentKind, "native-pending")
	if err != nil || gotParent.ID != parent.ID {
		t.Fatalf("fork bind changed pending parent mapping: parent=%+v err=%v", gotParent, err)
	}
}

func TestCreateSlotSessionRejectsConcurrentRestoreFork(t *testing.T) {
	store, ctx, parent, _ := newPendingResumeFixture(t, false)
	second := Session{
		ID:              "pending-child-two",
		WorkspaceID:     parent.WorkspaceID,
		SlotID:          "pending-child-two",
		ParentSessionID: parent.ID,
		State:           "RESTORING",
		AgentKind:       parent.AgentKind,
		TokenHash:       HashToken("pending-child-two"),
	}
	_, err := store.CreateSlotSession(ctx, Slot{
		ID:          second.SlotID,
		WorkspaceID: second.WorkspaceID,
		Generation:  1,
		RootID:      testRootID,
		RelPath:     "workspace/pending-child-two",
		State:       "RESTORING",
	}, nil, second, "RESTORE")
	if err == nil || !strings.Contains(err.Error(), "already being restored") {
		t.Fatalf("second restore error=%v", err)
	}
}

func TestPendingRestoreWithoutParentMappingActivatesChild(t *testing.T) {
	store, ctx, parent, child := newPendingResumeFixture(t, false)
	if _, _, err := store.FinishPreparationWithRelease(ctx, child.SlotID); err != nil {
		t.Fatalf("finish restore without parent mapping: %v", err)
	}

	gotParent, err := store.SessionByID(ctx, parent.ID)
	if err != nil {
		t.Fatal(err)
	}
	gotChild, err := store.SessionByID(ctx, child.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotParent.AgentSessionID != "" {
		t.Fatalf("unmapped parent gained an agent mapping: %+v", gotParent)
	}
	if gotChild.AgentSessionID != "native-pending" || gotChild.PendingAgentSessionID != "" || gotChild.State != "ACTIVE" {
		t.Fatalf("pending child was not activated: %+v", gotChild)
	}
}

func TestPendingRestoreMappingConflictRecordsWarning(t *testing.T) {
	store, ctx, parent, child := newPendingResumeFixture(t, true)
	if _, err := store.db.ExecContext(ctx, `CREATE TRIGGER ignore_pending_child_mapping BEFORE UPDATE ON sessions WHEN OLD.id='pending-child' AND NEW.pending_agent_session_id IS NULL BEGIN SELECT RAISE(IGNORE); END`); err != nil {
		t.Fatal(err)
	}

	if _, _, err := store.FinishPreparationWithRelease(ctx, child.SlotID); err != nil {
		t.Fatalf("finish conflicting restore: %v", err)
	}
	var eventKind, eventMessage string
	if err := store.db.QueryRowContext(ctx, `SELECT kind,message FROM events WHERE session_id=? AND kind='resume_mapping_conflict'`, child.ID).Scan(&eventKind, &eventMessage); err != nil {
		t.Fatalf("conflict warning event: %v", err)
	}
	if eventKind != "resume_mapping_conflict" || !strings.Contains(eventMessage, "native-pending") {
		t.Fatalf("conflict warning=%q %q", eventKind, eventMessage)
	}

	gotChild, err := store.SessionByID(ctx, child.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotChild.State != "ACTIVE" || gotChild.AgentSessionID != "" || gotChild.PendingAgentSessionID != "native-pending" {
		t.Fatalf("conflicting child state=%+v", gotChild)
	}
	gotParent, err := store.SessionByID(ctx, parent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotParent.AgentSessionID != "" {
		t.Fatalf("conflicting restore removed no parent mapping: %+v", gotParent)
	}
}
