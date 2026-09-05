package state

import (
	"context"
	"errors"
	"testing"
	"time"
)

// seedCleanSlot は clean の対象選定 test 用に、slot と占有 session を直接登録する。
func seedCleanSlot(t *testing.T, store *Store, slotID, slotState, sessionID, sessionState string) {
	t.Helper()
	ctx := context.Background()
	owner := any(nil)
	if sessionID != "" {
		owner = sessionID
	}
	if _, err := store.db.ExecContext(ctx, `INSERT INTO slots(id,workspace_id,generation,root_id,rel_path,dir_identity,state,owner_session_id,created_at,updated_at) VALUES(?,'workspace',1,?,?,'1:1',?,?,?,?)`,
		slotID, testRootID, "workspace/"+slotID, slotState, owner, now(), now()); err != nil {
		t.Fatal(err)
	}
	if sessionID == "" {
		return
	}
	if _, err := store.db.ExecContext(ctx, `INSERT INTO sessions(id,workspace_id,slot_id,state,agent_kind,session_token_hash,created_at) VALUES(?,'workspace',?,?,'codex',?,?)`,
		sessionID, slotID, sessionState, HashToken("token"), now()); err != nil {
		t.Fatal(err)
	}
}

func TestCleanCandidatesReportsEveryUnarchivedSlot(t *testing.T) {
	store := openTestStore(t)
	seedWorkspace(t, store)
	seedCleanSlot(t, store, "ready", "READY", "", "")
	seedCleanSlot(t, store, "leased", "LEASED", "session", "ACTIVE")
	seedCleanSlot(t, store, "gone", "ARCHIVED", "", "")
	candidates, err := store.CleanCandidates(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 2 {
		t.Fatalf("candidates=%+v", candidates)
	}
	if candidates[0].SlotID != "leased" || candidates[0].SessionState != "ACTIVE" || candidates[0].Path != testRootPath+"/workspace/leased" {
		t.Fatalf("leased candidate=%+v", candidates[0])
	}
	if candidates[1].SlotID != "ready" || candidates[1].SessionState != "" {
		t.Fatalf("ready candidate=%+v", candidates[1])
	}
}

func TestBeginCleanRunJoinsSameModeAndRejectsOther(t *testing.T) {
	store := openTestStore(t)
	seedWorkspace(t, store)
	ctx := context.Background()
	targets := []CleanTarget{{SlotID: "ready", WorkspaceID: "workspace", Path: "/wx/workspace/ready", State: "PENDING"}}
	runID, joined, err := store.BeginCleanRun(ctx, "run1", "normal", targets, []string{"workspace"})
	if err != nil || joined || runID != "run1" {
		t.Fatalf("first run: id=%s joined=%v err=%v", runID, joined, err)
	}
	sameID, joined, err := store.BeginCleanRun(ctx, "run2", "normal", targets, nil)
	if err != nil || !joined || sameID != "run1" {
		t.Fatalf("rejoin: id=%s joined=%v err=%v", sameID, joined, err)
	}
	if _, _, err := store.BeginCleanRun(ctx, "run3", "all", targets, nil); !errors.Is(err, ErrCleanModeConflict) {
		t.Fatalf("mode conflict err=%v", err)
	}
	suspended, err := store.ReplenishSuspended(ctx, "workspace")
	if err != nil || !suspended {
		t.Fatalf("suspension=%v err=%v", suspended, err)
	}
	if err := store.ResumeReplenish(ctx, "workspace"); err != nil {
		t.Fatal(err)
	}
	if suspended, err := store.ReplenishSuspended(ctx, "workspace"); err != nil || suspended {
		t.Fatalf("resumed suspension=%v err=%v", suspended, err)
	}
}

func TestCleanRunBlocksNewLeasesUntilItFinishes(t *testing.T) {
	store := openTestStore(t)
	seedWorkspace(t, store)
	ctx := context.Background()
	if _, _, err := store.BeginCleanRun(ctx, "run", "normal", nil, nil); err != nil {
		t.Fatal(err)
	}
	session := Session{ID: "new", WorkspaceID: "workspace", SlotID: "new", State: "STARTING", AgentKind: "codex", TokenHash: HashToken("token")}
	_, err := store.CreateSlotSession(ctx, Slot{ID: "new", WorkspaceID: "workspace", Generation: 1, RootID: testRootID, RelPath: "workspace/new", State: "PREPARING"}, nil, session, "PREPARE")
	if !errors.Is(err, ErrCleanInProgress) {
		t.Fatalf("lease during clean err=%v", err)
	}
	if _, err := store.CreateStandby(ctx, Slot{ID: "standby", WorkspaceID: "workspace", Generation: 1, RootID: testRootID, RelPath: "workspace/standby", State: "PREPARING"}, nil); !errors.Is(err, ErrCleanInProgress) {
		t.Fatalf("standby during clean err=%v", err)
	}
	finished, err := store.FinishCleanRun(ctx, "run")
	if err != nil || !finished {
		t.Fatalf("finish empty run: finished=%v err=%v", finished, err)
	}
	if _, err := store.CreateSlotSession(ctx, Slot{ID: "new", WorkspaceID: "workspace", Generation: 1, RootID: testRootID, RelPath: "workspace/new", State: "PREPARING"}, nil, session, "PREPARE"); err != nil {
		t.Fatalf("lease after clean err=%v", err)
	}
}

func TestCleanTargetTransitionsAndRunCompletion(t *testing.T) {
	store := openTestStore(t)
	seedWorkspace(t, store)
	ctx := context.Background()
	targets := []CleanTarget{
		{SlotID: "ready", WorkspaceID: "workspace", Path: "/wx/workspace/ready", State: "PENDING"},
		{SlotID: "held", WorkspaceID: "workspace", Path: "/wx/workspace/held", State: "SKIPPED", Reason: "session in use"},
	}
	if _, _, err := store.BeginCleanRun(ctx, "run", "normal", targets, nil); err != nil {
		t.Fatal(err)
	}
	run, stored, err := store.CleanRunByID(ctx, "run")
	if err != nil || run.Mode != "normal" || len(stored) != 2 {
		t.Fatalf("run=%+v targets=%+v err=%v", run, stored, err)
	}
	if finished, err := store.FinishCleanRun(ctx, "run"); err != nil || finished {
		t.Fatalf("run with pending target closed: finished=%v err=%v", finished, err)
	}
	if err := store.SetCleanTargetState(ctx, "run", "ready", []string{"PENDING"}, "REMOVING", ""); err != nil {
		t.Fatal(err)
	}
	if err := store.SetCleanTargetState(ctx, "run", "ready", []string{"PENDING"}, "DONE", ""); err == nil {
		t.Fatal("transition from a state the target already left was accepted")
	}
	if err := store.SetCleanTargetState(ctx, "run", "ready", []string{"REMOVING"}, "DONE", ""); err != nil {
		t.Fatal(err)
	}
	finished, err := store.FinishCleanRun(ctx, "run")
	if err != nil || !finished {
		t.Fatalf("finish: finished=%v err=%v", finished, err)
	}
	if _, found, err := store.ActiveCleanRun(ctx); err != nil || found {
		t.Fatalf("finished run still active: found=%v err=%v", found, err)
	}
	if runs, err := store.RunningCleanRuns(ctx); err != nil || len(runs) != 0 {
		t.Fatalf("running runs=%+v err=%v", runs, err)
	}
}

func TestRunningCleanRunsSurviveForRestartResume(t *testing.T) {
	store := openTestStore(t)
	seedWorkspace(t, store)
	ctx := context.Background()
	targets := []CleanTarget{{SlotID: "ready", WorkspaceID: "workspace", Path: "/wx/workspace/ready", State: "PENDING"}}
	if _, _, err := store.BeginCleanRun(ctx, "run", "all", targets, nil); err != nil {
		t.Fatal(err)
	}
	runs, err := store.RunningCleanRuns(ctx)
	if err != nil || len(runs) != 1 || runs[0].ID != "run" || runs[0].Mode != "all" {
		t.Fatalf("running runs=%+v err=%v", runs, err)
	}
	active, found, err := store.ActiveCleanRun(ctx)
	if err != nil || !found || active.ID != "run" {
		t.Fatalf("active run=%+v found=%v err=%v", active, found, err)
	}
}

func TestSessionTerminationRequestIsSingleAndDeadlineBound(t *testing.T) {
	store := openTestStore(t)
	seedWorkspace(t, store)
	seedCleanSlot(t, store, "leased", "LEASED", "session", "ACTIVE")
	ctx := context.Background()
	targets := []CleanTarget{{SlotID: "leased", WorkspaceID: "workspace", SessionID: "session", Path: "/wx/workspace/leased", State: "PENDING"}}
	if _, _, err := store.BeginCleanRun(ctx, "run", "all", targets, nil); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(30 * time.Second)
	if err := store.RequestSessionTermination(ctx, "run", "leased", "session", "request", deadline); err != nil {
		t.Fatal(err)
	}
	// 同じ mode の再実行が終了要求を重複させないことを確かめる。
	if err := store.RequestSessionTermination(ctx, "run", "leased", "session", "second", deadline); err == nil {
		t.Fatal("second termination request moved the target again")
	}
	var requests int
	if err := store.db.QueryRow(`SELECT count(*) FROM session_termination_requests WHERE session_id='session'`).Scan(&requests); err != nil {
		t.Fatal(err)
	}
	if requests != 1 {
		t.Fatalf("termination requests=%d", requests)
	}
	request, found, err := store.PendingTermination(ctx, "session")
	if err != nil || !found || request.RequestID != "request" {
		t.Fatalf("pending request=%+v found=%v err=%v", request, found, err)
	}
	if err := store.FinishTermination(ctx, "session", "wrong", "CONFIRMED"); err == nil {
		t.Fatal("a mismatched request id closed the termination request")
	}
	if err := store.FinishTermination(ctx, "session", "request", "CONFIRMED"); err != nil {
		t.Fatal(err)
	}
	if _, found, err := store.PendingTermination(ctx, "session"); err != nil || found {
		t.Fatalf("confirmed request still pending: found=%v err=%v", found, err)
	}
	stored, err := store.CleanTargets(ctx, "run")
	if err != nil || len(stored) != 1 || stored[0].State != "TERMINATING" || stored[0].TerminateDeadline == "" {
		t.Fatalf("targets=%+v err=%v", stored, err)
	}
}
