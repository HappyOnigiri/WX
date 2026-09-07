package state

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

func TestCreateSlotSessionCommitsJobAtomically(t *testing.T) {
	store := openTestStore(t)
	seedWorkspace(t, store)
	ctx := context.Background()
	session := Session{ID: "session", WorkspaceID: "workspace", SlotID: "slot", State: "STARTING", AgentKind: "codex", TokenHash: HashToken("token")}
	job, err := store.CreateSlotSession(ctx, Slot{ID: "slot", WorkspaceID: "workspace", Generation: 1, RootID: testRootID, RelPath: "workspace/slot", State: "PREPARING"}, []SlotRepository{{RepositoryID: "repository", WorktreePath: filepath.Join(t.TempDir(), "worktree"), State: "PREPARING", RequestedRef: "main", BaseOID: "abc", Fingerprint: "fingerprint"}}, session, "PREPARE")
	if err != nil {
		t.Fatal(err)
	}
	var slotState, sessionState, jobState string
	if err := store.db.QueryRow(`SELECT sl.state,se.state,j.state FROM slots sl JOIN sessions se ON se.slot_id=sl.id JOIN jobs j ON j.slot_id=sl.id WHERE sl.id='slot'`).Scan(&slotState, &sessionState, &jobState); err != nil {
		t.Fatal(err)
	}
	if slotState != "PREPARING" || sessionState != "STARTING" || jobState != "PENDING" || job.Kind != "PREPARE" {
		t.Fatalf("atomic state = slot:%s session:%s job:%s kind:%s", slotState, sessionState, jobState, job.Kind)
	}
}

func TestCreateSlotSessionPropagatesLastLeasedAtFault(t *testing.T) {
	store := openTestStore(t)
	seedWorkspace(t, store)
	ctx := context.Background()
	if _, err := store.db.Exec(`CREATE TRIGGER fail_last_leased_at BEFORE UPDATE OF last_leased_at ON repositories WHEN OLD.id='repository' BEGIN SELECT RAISE(ABORT,'fault'); END`); err != nil {
		t.Fatal(err)
	}
	session := Session{ID: "leased-at-fault", WorkspaceID: "workspace", SlotID: "leased-at-fault", State: "STARTING", AgentKind: "codex", TokenHash: HashToken("leased-at-fault")}
	if _, err := store.CreateSlotSession(ctx, Slot{ID: "leased-at-fault", WorkspaceID: "workspace", Generation: 1, RootID: testRootID, RelPath: "workspace/leased-at-fault", State: "PREPARING"}, nil, session, ""); err == nil {
		t.Fatal("slot session creation succeeded despite a last_leased_at update fault")
	}
	if _, err := store.Slot(ctx, "leased-at-fault"); err == nil {
		t.Fatal("rolled-back slot session left a durable slot row")
	}
}

// 予約のCAS失敗には、その時点の行の値を添えて返す。
// 失敗メッセージだけで、別経路（reconcileの隔離など）に遷移させられたのかを判別できるようにするためである。
func TestReservationCASFailureReportsCurrentRow(t *testing.T) {
	store := openTestStore(t)
	seedWorkspace(t, store)
	ctx := context.Background()
	slot := Slot{ID: "reserved", WorkspaceID: "workspace", Generation: 1, RootID: testRootID, RelPath: "workspace/reserved", OwnerSessionID: "session"}
	if err := store.ReserveSlot(ctx, slot); err != nil {
		t.Fatal(err)
	}
	if err := store.QuarantineReservedSlot(ctx, slot.ID, "ALLOCATION_INTERRUPTED"); err != nil {
		t.Fatal(err)
	}
	err := store.ConfirmSlotCreation(ctx, slot.ID, "identity")
	if err == nil {
		t.Fatal("creation confirmation succeeded after the reservation was quarantined")
	}
	for _, want := range []string{"state=QUARANTINED", "failure_code=ALLOCATION_INTERRUPTED"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("compare-and-swap failure %q does not report %s", err, want)
		}
	}
}
