package state

import (
	"context"
	"errors"
	"path/filepath"
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
	t.Parallel()
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
	t.Parallel()
	store := openTestStore(t)
	seedWorkspace(t, store)
	ctx := context.Background()
	targets := []CleanTarget{{SlotID: "ready", WorkspaceID: "workspace", Path: "/wx/workspace/ready", State: "PENDING"}}
	runID, joined, err := store.BeginCleanRun(ctx, CleanRun{ID: "run1", Mode: "normal"}, targets, []string{"workspace"})
	if err != nil || joined || runID != "run1" {
		t.Fatalf("first run: id=%s joined=%v err=%v", runID, joined, err)
	}
	sameID, joined, err := store.BeginCleanRun(ctx, CleanRun{ID: "run2", Mode: "normal"}, targets, nil)
	if err != nil || !joined || sameID != "run1" {
		t.Fatalf("rejoin: id=%s joined=%v err=%v", sameID, joined, err)
	}
	if _, _, err := store.BeginCleanRun(ctx, CleanRun{ID: "run3", Mode: "all"}, targets, nil); !errors.Is(err, ErrCleanModeConflict) {
		t.Fatalf("mode conflict err=%v", err)
	}
	// 対象範囲が違えば同じ mode でも合流させない。範囲を絞った run が全 workspace の削除を引き受けてしまうためである。
	if _, _, err := store.BeginCleanRun(ctx, CleanRun{ID: "run4", Mode: "normal", WorkspaceID: "workspace"}, targets, nil); !errors.Is(err, ErrCleanModeConflict) {
		t.Fatalf("scope conflict err=%v", err)
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
	t.Parallel()
	store := openTestStore(t)
	seedWorkspace(t, store)
	ctx := context.Background()
	if _, _, err := store.BeginCleanRun(ctx, CleanRun{ID: "run", Mode: "normal"}, nil, nil); err != nil {
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
	t.Parallel()
	store := openTestStore(t)
	seedWorkspace(t, store)
	ctx := context.Background()
	targets := []CleanTarget{
		{SlotID: "ready", WorkspaceID: "workspace", Path: "/wx/workspace/ready", State: "PENDING"},
		{SlotID: "held", WorkspaceID: "workspace", Path: "/wx/workspace/held", State: "SKIPPED", Reason: "session in use"},
	}
	if _, _, err := store.BeginCleanRun(ctx, CleanRun{ID: "run", Mode: "normal"}, targets, nil); err != nil {
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
	t.Parallel()
	store := openTestStore(t)
	seedWorkspace(t, store)
	ctx := context.Background()
	targets := []CleanTarget{{SlotID: "ready", WorkspaceID: "workspace", Path: "/wx/workspace/ready", State: "PENDING"}}
	if _, _, err := store.BeginCleanRun(ctx, CleanRun{ID: "run", Mode: "all"}, targets, nil); err != nil {
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
	t.Parallel()
	store := openTestStore(t)
	seedWorkspace(t, store)
	seedCleanSlot(t, store, "leased", "LEASED", "session", "ACTIVE")
	ctx := context.Background()
	targets := []CleanTarget{{SlotID: "leased", WorkspaceID: "workspace", SessionID: "session", Path: "/wx/workspace/leased", State: "PENDING"}}
	if _, _, err := store.BeginCleanRun(ctx, CleanRun{ID: "run", Mode: "all"}, targets, nil); err != nil {
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

// 期限切れで閉じた要求が残っていても、次の clear --all が session へ届く新しい要求を出せることを確かめる。
func TestSessionTerminationRequestIsReissuedAfterTimeout(t *testing.T) {
	t.Parallel()
	store := openTestStore(t)
	seedWorkspace(t, store)
	seedCleanSlot(t, store, "leased", "LEASED", "session", "ACTIVE")
	ctx := t.Context()
	targets := []CleanTarget{{SlotID: "leased", WorkspaceID: "workspace", SessionID: "session", Path: "/wx/workspace/leased", State: "PENDING"}}
	if _, _, err := store.BeginCleanRun(ctx, CleanRun{ID: "run-a", Mode: "all"}, targets, nil); err != nil {
		t.Fatal(err)
	}
	staleDeadline := time.Now().Add(-30 * time.Second)
	if err := store.RequestSessionTermination(ctx, "run-a", "leased", "session", "request-a", staleDeadline); err != nil {
		t.Fatal(err)
	}
	if err := store.FinishTermination(ctx, "session", "request-a", "TIMED_OUT"); err != nil {
		t.Fatal(err)
	}
	if err := store.SetCleanTargetState(ctx, "run-a", "leased", []string{"TERMINATING"}, "FAILED", "timed out"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.FinishCleanRun(ctx, "run-a"); err != nil {
		t.Fatal(err)
	}

	if _, _, err := store.BeginCleanRun(ctx, CleanRun{ID: "run-b", Mode: "all"}, targets, nil); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(30 * time.Second)
	if err := store.RequestSessionTermination(ctx, "run-b", "leased", "session", "request-b", deadline); err != nil {
		t.Fatal(err)
	}
	request, found, err := store.PendingTermination(ctx, "session")
	if err != nil || !found || request.RequestID != "request-b" {
		t.Fatalf("pending request=%+v found=%v err=%v", request, found, err)
	}
	if request.Deadline != FormatTime(deadline) {
		t.Fatalf("deadline=%q want %q; the reissued request must not inherit the expired deadline", request.Deadline, FormatTime(deadline))
	}
	stored, err := store.CleanTargets(ctx, "run-b")
	if err != nil || len(stored) != 1 || stored[0].State != "TERMINATING" || stored[0].TerminateDeadline != FormatTime(deadline) {
		t.Fatalf("targets=%+v err=%v", stored, err)
	}
	var requests int
	if err := store.db.QueryRow(`SELECT count(*) FROM session_termination_requests WHERE session_id='session'`).Scan(&requests); err != nil {
		t.Fatal(err)
	}
	if requests != 1 {
		t.Fatalf("termination requests=%d", requests)
	}
}

// 未応答の要求がある間は再実行が要求を置き換えず、target も進まないことを確かめる。
func TestSessionTerminationRequestIsNotReissuedWhilePending(t *testing.T) {
	t.Parallel()
	store := openTestStore(t)
	seedWorkspace(t, store)
	seedCleanSlot(t, store, "leased", "LEASED", "session", "ACTIVE")
	ctx := t.Context()
	targets := []CleanTarget{{SlotID: "leased", WorkspaceID: "workspace", SessionID: "session", Path: "/wx/workspace/leased", State: "PENDING"}}
	if _, _, err := store.BeginCleanRun(ctx, CleanRun{ID: "run-a", Mode: "all"}, targets, nil); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(30 * time.Second)
	if err := store.RequestSessionTermination(ctx, "run-a", "leased", "session", "request-a", deadline); err != nil {
		t.Fatal(err)
	}
	// 要求を閉じないまま run が終わる（daemon の中断など）と、次の run は未応答の要求をそのまま引き継ぐ。
	if err := store.SetCleanTargetState(ctx, "run-a", "leased", []string{"TERMINATING"}, "FAILED", "interrupted"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.FinishCleanRun(ctx, "run-a"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.BeginCleanRun(ctx, CleanRun{ID: "run-b", Mode: "all"}, targets, nil); err != nil {
		t.Fatal(err)
	}
	later := time.Now().Add(10 * time.Minute)
	if err := store.RequestSessionTermination(ctx, "run-b", "leased", "session", "request-b", later); err != nil {
		t.Fatal(err)
	}
	request, found, err := store.PendingTermination(ctx, "session")
	if err != nil || !found || request.RequestID != "request-a" || request.Deadline != FormatTime(deadline) {
		t.Fatalf("pending request=%+v found=%v err=%v", request, found, err)
	}
	stored, err := store.CleanTargets(ctx, "run-b")
	if err != nil || len(stored) != 1 || stored[0].TerminateDeadline != FormatTime(deadline) {
		t.Fatalf("targets=%+v err=%v", stored, err)
	}
}

func TestDiscardRemovalPreservesActiveAndRunningWork(t *testing.T) {
	t.Parallel()
	store := openTestStore(t)
	seedWorkspace(t, store)
	ctx := t.Context()
	for _, sessionState := range []string{"ACTIVE", "DRAINING"} {
		slotID := "discard-" + sessionState
		session := Session{ID: slotID, SlotID: slotID, WorkspaceID: "workspace", State: sessionState, AgentKind: "codex", TokenHash: HashToken(slotID)}
		slot := Slot{ID: slotID, WorkspaceID: "workspace", Generation: 1, RootID: testRootID, RelPath: "workspace/" + slotID, State: "DRAINING"}
		if _, err := store.CreateSlotSession(ctx, slot, nil, session, ""); err != nil {
			t.Fatal(err)
		}
		job, changed, err := store.ScheduleDiscardRemoval(ctx, slotID)
		if err != nil {
			t.Fatal(err)
		}
		if sessionState == "ACTIVE" {
			if changed {
				t.Fatal("active session was discarded")
			}
		} else {
			if !changed || job.Kind != "REMOVE" || job.SessionID != "" {
				t.Fatalf("discard reservation=%+v changed=%v", job, changed)
			}
			if _, again, err := store.ScheduleDiscardRemoval(ctx, slotID); err != nil || again {
				t.Fatalf("duplicate discard changed=%v err=%v", again, err)
			}
		}
	}
}

// run の scope と補充再開の指示は daemon 再起動後も同じ内容で読み戻せる必要がある。
// driver は run を読み直して作り直すため、ここが欠けると再起動で指示が消える。
func TestCleanRunKeepsScopeAndReplenishAcrossReopen(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "state.db")
	store, err := openTestStoreAtPath(t, path)
	if err != nil {
		t.Fatal(err)
	}
	seedRoot(t, store, testRootID, testRootPath, "root-identity", true)
	seedWorkspace(t, store)
	ctx := context.Background()
	if _, _, err := store.BeginCleanRun(ctx, CleanRun{ID: "run", Mode: "standby", WorkspaceID: "workspace", Replenish: true}, nil, []string{"workspace"}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	running, err := reopened.RunningCleanRuns(ctx)
	if err != nil || len(running) != 1 || running[0].WorkspaceID != "workspace" || !running[0].Replenish {
		t.Fatalf("running runs=%+v err=%v", running, err)
	}
	run, _, err := reopened.CleanRunByID(ctx, "run")
	if err != nil || run.WorkspaceID != "workspace" || !run.Replenish || run.ReplenishState != CleanReplenishPending {
		t.Fatalf("run=%+v err=%v", run, err)
	}
	// 閉じるまでは補充再開の掃き出し対象にしない。
	if pending, err := reopened.PendingCleanReplenishRuns(ctx); err != nil || len(pending) != 0 {
		t.Fatalf("pending before the run closed=%+v err=%v", pending, err)
	}
	if finished, err := reopened.FinishCleanRun(ctx, "run"); err != nil || !finished {
		t.Fatalf("finish run: finished=%v err=%v", finished, err)
	}
	if pending, err := reopened.PendingCleanReplenishRuns(ctx); err != nil || len(pending) != 1 {
		t.Fatalf("pending after the run closed=%+v err=%v", pending, err)
	}
}

// 合流では補充再開の指示を上げるだけにする。指示なしの再実行が先行の約束を取り消すのは驚きになる。
func TestBeginCleanRunRaisesReplenishOnlyUpward(t *testing.T) {
	t.Parallel()
	store := openTestStore(t)
	seedWorkspace(t, store)
	ctx := context.Background()
	if _, _, err := store.BeginCleanRun(ctx, CleanRun{ID: "run", Mode: "standby"}, nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, joined, err := store.BeginCleanRun(ctx, CleanRun{ID: "again", Mode: "standby", Replenish: true}, nil, nil); err != nil || !joined {
		t.Fatalf("rejoin with replenish: joined=%v err=%v", joined, err)
	}
	run, _, err := store.CleanRunByID(ctx, "run")
	if err != nil || !run.Replenish {
		t.Fatalf("run after the raising rejoin=%+v err=%v", run, err)
	}
	if _, joined, err := store.BeginCleanRun(ctx, CleanRun{ID: "third", Mode: "standby"}, nil, nil); err != nil || !joined {
		t.Fatalf("rejoin without replenish: joined=%v err=%v", joined, err)
	}
	if run, _, err := store.CleanRunByID(ctx, "run"); err != nil || !run.Replenish {
		t.Fatalf("a rejoin without --replenish cleared the instruction: %+v err=%v", run, err)
	}
}

// 補充再開の引き受けは 1 度だけ成立する。driver は再起動や合流で何度でも同じ run を起こす。
func TestClaimCleanReplenishSucceedsOnlyOnce(t *testing.T) {
	t.Parallel()
	store := openTestStore(t)
	seedWorkspace(t, store)
	ctx := context.Background()
	if _, _, err := store.BeginCleanRun(ctx, CleanRun{ID: "run", Mode: "standby", Replenish: true}, nil, nil); err != nil {
		t.Fatal(err)
	}
	// RUNNING の間は引き受けない。停止解除は clean の実行中には通らない。
	if claimed, err := store.ClaimCleanReplenish(ctx, "run", false); err != nil || claimed {
		t.Fatalf("claim while running=%v err=%v", claimed, err)
	}
	if finished, err := store.FinishCleanRun(ctx, "run"); err != nil || !finished {
		t.Fatalf("finish run: finished=%v err=%v", finished, err)
	}
	if claimed, err := store.ClaimCleanReplenish(ctx, "run", false); err != nil || !claimed {
		t.Fatalf("first claim=%v err=%v", claimed, err)
	}
	if claimed, err := store.ClaimCleanReplenish(ctx, "run", false); err != nil || claimed {
		t.Fatalf("second claim=%v err=%v", claimed, err)
	}
	// 中断で RUNNING に残った引き受けは、起動時の掃き出しだけが取り直せる。
	if claimed, err := store.ClaimCleanReplenish(ctx, "run", true); err != nil || !claimed {
		t.Fatalf("resume claim=%v err=%v", claimed, err)
	}
	if err := store.FinishCleanReplenish(ctx, "run", `{"workspaces":[],"failures":[]}`); err != nil {
		t.Fatal(err)
	}
	if err := store.FinishCleanReplenish(ctx, "run", "{}"); err == nil {
		t.Fatal("finishing without a claim was accepted")
	}
	if claimed, err := store.ClaimCleanReplenish(ctx, "run", true); err != nil || claimed {
		t.Fatalf("claim after DONE=%v err=%v", claimed, err)
	}
	run, _, err := store.CleanRunByID(ctx, "run")
	if err != nil || run.ReplenishState != CleanReplenishDone || run.ReplenishResult == "" {
		t.Fatalf("run after the replenishment closed=%+v err=%v", run, err)
	}
}

// 再開の対象は、この run が clean として記録した停止行だけに限る。
func TestCleanSuspendedWorkspacesMatchesTheRunThatStopped(t *testing.T) {
	t.Parallel()
	store := openTestStore(t)
	seedWorkspace(t, store)
	seedWorkspaceRows(t, store, "other", "/other", "repository", "other-repository", "/other", "/other/.git", "")
	ctx := context.Background()
	if _, _, err := store.BeginCleanRun(ctx, CleanRun{ID: "run", Mode: "standby"}, nil, []string{"workspace"}); err != nil {
		t.Fatal(err)
	}
	if err := store.SuspendReplenish(ctx, "other", SuspendReplenishReasonStandbyFailure, "run"); err != nil {
		t.Fatal(err)
	}
	got, err := store.CleanSuspendedWorkspaces(ctx, "run")
	if err != nil || len(got) != 1 || got[0] != "workspace" {
		t.Fatalf("suspended workspaces=%v err=%v", got, err)
	}
	if got, err := store.CleanSuspendedWorkspaces(ctx, "another-run"); err != nil || len(got) != 0 {
		t.Fatalf("suspended workspaces of another run=%v err=%v", got, err)
	}
}
