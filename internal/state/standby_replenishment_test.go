package state

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

func createFinishedFailedStandby(t *testing.T, store *Store, id string) {
	t.Helper()
	ctx := context.Background()
	job, err := store.CreateStandby(ctx, Slot{ID: id, WorkspaceID: "workspace", Generation: 1, RootID: testRootID, RelPath: filepath.Join("workspace", id), State: "PREPARING"}, nil)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := store.SetSlotState(ctx, id, []string{"PREPARING"}, "FAILED", "PREPARE_FAILED"); err != nil {
		t.Fatalf("fail slot: %v", err)
	}
	claimed, err := store.ClaimJob(ctx, job.ID, "test")
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if err := store.FinishJob(ctx, claimed.ID, "test", errors.New("prepare failed")); err != nil {
		t.Fatalf("finish: %v", err)
	}
}

func TestStandbyReplenishmentConsumesSuccessAndExcludesOnlyExistingFailures(t *testing.T) {
	store := openTestStore(t)
	seedWorkspace(t, store)
	ctx := context.Background()
	createFinishedFailedStandby(t, store, "failed-before")
	_, err := store.CreateStandby(ctx, Slot{ID: "failed-pending", WorkspaceID: "workspace", Generation: 1, RootID: testRootID, RelPath: "workspace/failed-pending", State: "PREPARING"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetSlotState(ctx, "failed-pending", []string{"PREPARING"}, "FAILED", "PREPARE_FAILED"); err != nil {
		t.Fatal(err)
	}

	session := Session{ID: "normal", WorkspaceID: "workspace", SlotID: "normal", State: "STARTING", AgentKind: "codex", TokenHash: HashToken("normal")}
	if _, err := store.CreateSlotSession(ctx, Slot{ID: session.SlotID, WorkspaceID: "workspace", Generation: 1, RootID: testRootID, RelPath: "workspace/normal", State: "PREPARING"}, nil, session, "PREPARE"); err != nil {
		t.Fatal(err)
	}
	_, _, replenishJob, replenished, err := store.FinishPreparationWithReplenishment(ctx, session.SlotID)
	if err != nil {
		t.Fatal(err)
	}
	if !replenished || replenishJob.Kind != "ENSURE_STANDBY" {
		t.Fatalf("replenishment job=%+v replenished=%v", replenishJob, replenished)
	}
	if got := store.StandbyCount(ctx, "workspace"); got != 1 {
		t.Fatalf("count after success=%d, want pending failure only", got)
	}
	var successes, exclusions int
	if err := store.db.QueryRowContext(ctx, `SELECT count(*) FROM standby_replenish_successes`).Scan(&successes); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRowContext(ctx, `SELECT count(*) FROM standby_replenish_exclusions`).Scan(&exclusions); err != nil {
		t.Fatal(err)
	}
	if successes != 1 || exclusions != 1 {
		t.Fatalf("successes=%d exclusions=%d, want one each", successes, exclusions)
	}

	createFinishedFailedStandby(t, store, "failed-after")
	if _, created, err := store.RecordStandbySuccess(ctx, session.ID); err != nil || created {
		t.Fatalf("duplicate success created=%v err=%v", created, err)
	}
	if got := store.StandbyCount(ctx, "workspace"); got != 2 {
		t.Fatalf("duplicate success changed later failure count=%d, want 2", got)
	}
}

func TestStandbyReplenishmentExcludesRestorationSuccess(t *testing.T) {
	store := openTestStore(t)
	seedWorkspace(t, store)
	ctx := context.Background()
	createFinishedFailedStandby(t, store, "failed")
	parent := Session{ID: "restore-parent", WorkspaceID: "workspace", SlotID: "restore-parent", State: "ARCHIVED", AgentKind: "codex", TokenHash: HashToken("restore-parent")}
	if _, err := store.CreateSlotSession(ctx, Slot{ID: parent.SlotID, WorkspaceID: "workspace", Generation: 1, RootID: testRootID, RelPath: "workspace/restore-parent", State: "ARCHIVED"}, nil, parent, ""); err != nil {
		t.Fatal(err)
	}
	restored := Session{ID: "restored", WorkspaceID: "workspace", SlotID: "restored", ParentSessionID: parent.ID, State: "RESTORING", AgentKind: "codex", TokenHash: HashToken("restored")}
	if _, err := store.CreateSlotSession(ctx, Slot{ID: restored.SlotID, WorkspaceID: "workspace", Generation: 1, RootID: testRootID, RelPath: "workspace/restored", State: "RESTORING"}, nil, restored, "RESTORE"); err != nil {
		t.Fatal(err)
	}
	_, _, replenishJob, replenished, err := store.FinishPreparationWithReplenishment(ctx, restored.SlotID)
	if err != nil {
		t.Fatal(err)
	}
	if replenished || replenishJob.ID != "" {
		t.Fatalf("restore unexpectedly replenished: job=%+v replenished=%v", replenishJob, replenished)
	}
	if got := store.StandbyCount(ctx, "workspace"); got != 1 {
		t.Fatalf("restoration changed standby count=%d", got)
	}
	if _, err := store.db.ExecContext(ctx, `DELETE FROM jobs WHERE session_id=?`, restored.ID); err != nil {
		t.Fatal(err)
	}
	if _, created, err := store.RecordStandbySuccess(ctx, restored.ID); err != nil || created {
		t.Fatalf("explicit restoration success created=%v err=%v", created, err)
	}
}

func TestLeaseReadyWithReplenishmentAndRecoveryAreIdempotent(t *testing.T) {
	store := openTestStore(t)
	seedWorkspace(t, store)
	ctx := context.Background()
	createFinishedFailedStandby(t, store, "failed-ready")
	if _, err := store.CreateStandby(ctx, Slot{ID: "ready", WorkspaceID: "workspace", Generation: 1, RootID: testRootID, RelPath: "workspace/ready", State: "READY"}, nil); err != nil {
		t.Fatal(err)
	}
	readySession := Session{ID: "ready-session", WorkspaceID: "workspace", SlotID: "ready", State: "ACTIVE", AgentKind: "codex", TokenHash: HashToken("ready")}
	job, created, err := store.LeaseReadyWithReplenishment(ctx, "ready", readySession)
	if err != nil || !created || job.Kind != "ENSURE_STANDBY" {
		t.Fatalf("ready lease job=%+v created=%v err=%v", job, created, err)
	}
	if _, created, err := store.RecordStandbySuccess(ctx, readySession.ID); err != nil || created {
		t.Fatalf("duplicate ready success created=%v err=%v", created, err)
	}

	createFinishedFailedStandby(t, store, "failed-recover")
	recoverSession := Session{ID: "recover-session", WorkspaceID: "workspace", SlotID: "recover-session", State: "ACTIVE", AgentKind: "codex", TokenHash: HashToken("recover")}
	if _, err := store.CreateSlotSession(ctx, Slot{ID: recoverSession.SlotID, WorkspaceID: "workspace", Generation: 1, RootID: testRootID, RelPath: "workspace/recover-session", State: "LEASED"}, nil, recoverSession, ""); err != nil {
		t.Fatal(err)
	}
	jobs, err := store.RecoverStandbyReplenishments(ctx)
	if err != nil || len(jobs) != 1 || jobs[0].Kind != "ENSURE_STANDBY" {
		t.Fatalf("recovery jobs=%+v err=%v", jobs, err)
	}
	jobs, err = store.RecoverStandbyReplenishments(ctx)
	if err != nil || len(jobs) != 0 {
		t.Fatalf("duplicate recovery jobs=%+v err=%v", jobs, err)
	}
}

func TestLeaseReadyWithReplenishmentRejectsColdRepositories(t *testing.T) {
	store := openTestStore(t)
	seedWorkspace(t, store)
	ctx := context.Background()
	if _, err := store.CreateStandby(ctx, Slot{ID: "cold-ready", WorkspaceID: "workspace", Generation: 1, RootID: testRootID, RelPath: "workspace/cold-ready", State: "READY"}, []SlotRepository{{RepositoryID: "repository", DirName: "repository", State: "COLD"}}); err != nil {
		t.Fatal(err)
	}
	session := Session{ID: "cold-ready-session", WorkspaceID: "workspace", SlotID: "cold-ready", State: "ACTIVE", AgentKind: "codex", TokenHash: HashToken("cold-ready")}
	if _, _, err := store.LeaseReadyWithReplenishment(ctx, "cold-ready", session); err == nil {
		t.Fatal("cold repository was accepted on the warm lease path")
	}
	if slot, err := store.Slot(ctx, "cold-ready"); err != nil || slot.State != "READY" || slot.OwnerSessionID != "" {
		t.Fatalf("cold slot after rejected lease=%+v err=%v", slot, err)
	}
}

func TestCreateStandbyIfNeededRevalidatesCapacityAndGeneration(t *testing.T) {
	store := openTestStore(t)
	seedWorkspace(t, store)
	ctx := context.Background()
	slot := func(id string, generation int) Slot {
		return Slot{ID: id, WorkspaceID: "workspace", Generation: generation, RootID: testRootID, RelPath: filepath.Join("workspace", id), State: "PREPARING"}
	}
	job, created, err := store.CreateStandbyIfNeeded(ctx, slot("first", 1), nil, 1)
	if err != nil || !created || job.Kind != "PREPARE" {
		t.Fatalf("first creation job=%+v created=%v err=%v", job, created, err)
	}
	if _, created, err := store.CreateStandbyIfNeeded(ctx, slot("second", 1), nil, 1); err != nil || created {
		t.Fatalf("capacity revalidation created=%v err=%v", created, err)
	}
	if _, created, err := store.CreateStandbyIfNeeded(ctx, slot("stale", 2), nil, 2); err != nil || created {
		t.Fatalf("generation mismatch created=%v err=%v", created, err)
	}
	if got := store.StandbyCount(ctx, "workspace"); got != 1 {
		t.Fatalf("standby count=%d, want one", got)
	}
}

func TestRegisterReservedStandbyRejectsChangedWorkspaceGeneration(t *testing.T) {
	store := openTestStore(t)
	seedWorkspace(t, store)
	ctx := context.Background()
	slot := Slot{ID: "reserved", WorkspaceID: "workspace", Generation: 1, RootID: testRootID, RelPath: "workspace/reserved"}
	reserved, err := store.ReserveStandbyIfNeeded(ctx, slot, 1)
	if err != nil || !reserved {
		t.Fatalf("reserve standby=%v err=%v", reserved, err)
	}
	if err := store.ConfirmSlotCreation(ctx, slot.ID, "slot-identity"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE workspaces SET generation=2 WHERE id='workspace'`); err != nil {
		t.Fatal(err)
	}
	_, err = store.RegisterReservedStandby(ctx, slot.ID, []SlotRepository{{
		RepositoryID: "repository", DirName: "repository", State: "READY",
	}})
	if err == nil {
		t.Fatal("stale standby registration succeeded")
	}
	registered, err := store.Slot(ctx, slot.ID)
	if err != nil {
		t.Fatal(err)
	}
	if registered.State != "REGISTERING" {
		t.Fatalf("stale standby state=%s, want REGISTERING", registered.State)
	}
	repos, err := store.SlotRepositories(ctx, slot.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(repos) != 0 {
		t.Fatalf("stale standby repositories=%+v, want none", repos)
	}
}

// 隔離 slot は READY へ戻らないため待機枠に数えず、成功イベントを待たずに補充できる。
func TestStandbyCountExcludesQuarantinedSlotsAndAllowsReplenishment(t *testing.T) {
	store := openTestStore(t)
	seedWorkspace(t, store)
	ctx := context.Background()
	if _, err := store.CreateStandby(ctx, Slot{ID: "quarantined", WorkspaceID: "workspace", Generation: 1, RootID: testRootID, RelPath: "workspace/quarantined", State: "PREPARING"}, nil); err != nil {
		t.Fatal(err)
	}
	if err := store.SetSlotState(ctx, "quarantined", []string{"PREPARING"}, "QUARANTINED", "JOB_RETRY_EXHAUSTED"); err != nil {
		t.Fatal(err)
	}
	if got := store.StandbyCount(ctx, "workspace"); got != 0 {
		t.Fatalf("standby count with a quarantined slot=%d, want zero", got)
	}
	job, created, err := store.CreateStandbyIfNeeded(ctx, Slot{ID: "replacement", WorkspaceID: "workspace", Generation: 1, RootID: testRootID, RelPath: "workspace/replacement", State: "PREPARING"}, nil, 1)
	if err != nil || !created || job.Kind != "PREPARE" {
		t.Fatalf("replenishment job=%+v created=%v err=%v", job, created, err)
	}
	if got := store.StandbyCount(ctx, "workspace"); got != 1 {
		t.Fatalf("standby count after replenishment=%d, want the replacement only", got)
	}
}

// 補充停止は replenish_suspensions が唯一の権威で、隔離 slot の数は診断にも解除にも関与しない。
func TestStandbyReplenishmentSuspensionDiagnosticsAndRetry(t *testing.T) {
	store := openTestStore(t)
	seedWorkspace(t, store)
	ctx := context.Background()
	for _, id := range []string{"quarantine-a", "quarantine-b"} {
		if _, err := store.CreateStandby(ctx, Slot{ID: id, WorkspaceID: "workspace", Generation: 1, RootID: testRootID, RelPath: filepath.Join("workspace", id), State: "PREPARING"}, nil); err != nil {
			t.Fatal(err)
		}
		if err := store.SetSlotState(ctx, id, []string{"PREPARING"}, "QUARANTINED", "JOB_RETRY_EXHAUSTED"); err != nil {
			t.Fatal(err)
		}
	}
	// 隔離だけでは停止しない。停止は準備失敗の記録が入った時点で成立する。
	if blocked, err := store.StandbyReplenishmentDiagnostics(ctx); err != nil || len(blocked) != 0 {
		t.Fatalf("diagnostics without a suspension=%+v err=%v", blocked, err)
	}
	if err := store.SuspendReplenish(ctx, "workspace", SuspendReplenishReasonStandbyFailure, "job-1"); err != nil {
		t.Fatal(err)
	}
	// 停止中の再記録は最初の理由を残す。
	if err := store.SuspendReplenish(ctx, "workspace", SuspendReplenishReasonClean, "run-1"); err != nil {
		t.Fatal(err)
	}
	blocked, err := store.StandbyReplenishmentDiagnostics(ctx)
	if err != nil || len(blocked) != 1 || blocked[0].Generation != 1 {
		t.Fatalf("blocked diagnostics=%+v err=%v", blocked, err)
	}
	if blocked[0].Reason != SuspendReplenishReasonStandbyFailure || blocked[0].Detail != "job-1" || blocked[0].SuspendedAt == "" {
		t.Fatalf("suspension reason=%+v, want the first failure", blocked[0])
	}
	retry, err := store.RetryStandbyReplenishment(ctx, "workspace")
	if err != nil || retry.Generation != 1 || !retry.Suspended || retry.Job.ID == "" || retry.Job.State != "PENDING" {
		t.Fatalf("retry=%+v err=%v", retry, err)
	}
	if suspended, err := store.ReplenishSuspended(ctx, "workspace"); err != nil || suspended {
		t.Fatalf("suspended after retry=%v err=%v", suspended, err)
	}
	for _, id := range []string{"quarantine-a", "quarantine-b"} {
		slot, err := store.Slot(ctx, id)
		if err != nil || slot.State != "QUARANTINED" {
			t.Fatalf("quarantine slot %s changed: %+v err=%v", id, slot, err)
		}
	}
	// 2 回目は停止が無く、既存の ENSURE_STANDBY を重複させない。
	second, err := store.RetryStandbyReplenishment(ctx, "workspace")
	if err != nil || second.Job.ID != retry.Job.ID || second.Suspended {
		t.Fatalf("second retry=%+v err=%v", second, err)
	}
}

func TestRetryStandbyReplenishmentRefusesAnActiveClean(t *testing.T) {
	store := openTestStore(t)
	seedWorkspace(t, store)
	ctx := context.Background()
	if err := store.SuspendReplenish(ctx, "workspace", SuspendReplenishReasonStandbyFailure, "job-1"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.BeginCleanRun(ctx, "clean", "normal", nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := store.RetryStandbyReplenishment(ctx, "workspace"); !errors.Is(err, ErrCleanInProgress) {
		t.Fatalf("retry during clean err=%v", err)
	}
	if suspended, err := store.ReplenishSuspended(ctx, "workspace"); err != nil || !suspended {
		t.Fatalf("suspension after a refused retry=%v err=%v", suspended, err)
	}
}

func TestFailedStandbySuspensionRequiresCurrentUnownedPreparation(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name       string
		state      string
		generation int
		owned      bool
		want       bool
	}{
		{name: "preparing", state: "PREPARING", generation: 1, want: true},
		{name: "failed", state: "FAILED", generation: 1, want: true},
		{name: "quarantined", state: "QUARANTINED", generation: 1, want: true},
		{name: "stale", state: "STALE", generation: 1},
		{name: "ready", state: "READY", generation: 1},
		{name: "obsolete preparing", state: "PREPARING", generation: 2},
		{name: "obsolete failed", state: "FAILED", generation: 2},
		{name: "obsolete quarantined", state: "QUARANTINED", generation: 2},
		{name: "session preparation", state: "PREPARING", generation: 1, owned: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := openTestStore(t)
			seedWorkspace(t, store)
			ctx := context.Background()
			slot := Slot{ID: "standby", WorkspaceID: "workspace", Generation: 1, RootID: testRootID, RelPath: "workspace/standby", State: test.state}
			var job Job
			var err error
			if test.owned {
				session := Session{ID: "session", WorkspaceID: "workspace", SlotID: slot.ID, State: "STARTING", AgentKind: "codex", TokenHash: HashToken("token")}
				job, err = store.CreateSlotSession(ctx, slot, nil, session, "PREPARE")
			} else {
				job, err = store.CreateStandby(ctx, slot, nil)
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err := store.db.ExecContext(ctx, `UPDATE workspaces SET generation=? WHERE id='workspace'`, test.generation); err != nil {
				t.Fatal(err)
			}
			if suspended, err := store.SuspendFailedStandbyReplenishment(ctx, job.ID); err != nil || suspended != test.want {
				t.Fatalf("new suspension=%v err=%v, want %v", suspended, err, test.want)
			}
			if suspended, err := store.ReplenishSuspended(ctx, "workspace"); err != nil || suspended != test.want {
				t.Fatalf("stored suspension=%v err=%v, want %v", suspended, err, test.want)
			}
			if suspended, err := store.SuspendFailedStandbyReplenishment(ctx, job.ID); err != nil || suspended {
				t.Fatalf("duplicate suspension=%v err=%v", suspended, err)
			}
		})
	}
}

func TestFailedStandbySuspensionPreservesTheFirstReasonAndReportsWriteFailure(t *testing.T) {
	t.Parallel()
	store := openTestStore(t)
	seedWorkspace(t, store)
	ctx := context.Background()
	job, err := store.CreateStandby(ctx, Slot{ID: "standby", WorkspaceID: "workspace", Generation: 1, RootID: testRootID, RelPath: "workspace/standby", State: "FAILED"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SuspendReplenish(ctx, "workspace", SuspendReplenishReasonClean, "clean-run"); err != nil {
		t.Fatal(err)
	}
	if suspended, err := store.SuspendFailedStandbyReplenishment(ctx, job.ID); err != nil || suspended {
		t.Fatalf("already stopped: suspension=%v err=%v", suspended, err)
	}
	blocked, err := store.StandbyReplenishmentDiagnostics(ctx)
	if err != nil || len(blocked) != 1 || blocked[0].Reason != SuspendReplenishReasonClean || blocked[0].Detail != "clean-run" {
		t.Fatalf("first reason changed: diagnostics=%+v err=%v", blocked, err)
	}
	if err := store.ResumeReplenish(ctx, "workspace"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `CREATE TRIGGER fail_suspend BEFORE INSERT ON replenish_suspensions BEGIN SELECT RAISE(ABORT,'fault'); END`); err != nil {
		t.Fatal(err)
	}
	if suspended, err := store.SuspendFailedStandbyReplenishment(ctx, job.ID); err == nil || suspended {
		t.Fatalf("write failure: suspension=%v err=%v", suspended, err)
	}
	if suspended, err := store.ReplenishSuspended(ctx, "workspace"); err != nil || suspended {
		t.Fatalf("write failure left a suspension: suspended=%v err=%v", suspended, err)
	}
}

func TestStandbyReplenishmentRollsBackOnEnsureJobFailure(t *testing.T) {
	store := openTestStore(t)
	seedWorkspace(t, store)
	ctx := context.Background()
	createFinishedFailedStandby(t, store, "failed")
	if _, err := store.db.ExecContext(ctx, `CREATE TRIGGER fail_replenish_job BEFORE INSERT ON jobs WHEN NEW.kind='ENSURE_STANDBY' BEGIN SELECT RAISE(ABORT,'fault'); END`); err != nil {
		t.Fatal(err)
	}
	session := Session{ID: "normal-fault", WorkspaceID: "workspace", SlotID: "normal-fault", State: "STARTING", AgentKind: "codex", TokenHash: HashToken("normal-fault")}
	if _, err := store.CreateSlotSession(ctx, Slot{ID: session.SlotID, WorkspaceID: "workspace", Generation: 1, RootID: testRootID, RelPath: "workspace/normal-fault", State: "PREPARING"}, nil, session, "PREPARE"); err != nil {
		t.Fatal(err)
	}
	if _, _, _, _, err := store.FinishPreparationWithReplenishment(ctx, session.SlotID); err == nil {
		t.Fatal("finish preparation succeeded despite replenishment job insertion fault")
	}
	if slot, err := store.Slot(ctx, session.SlotID); err != nil || slot.State != "PREPARING" {
		t.Fatalf("rolled-back normal slot=%+v err=%v", slot, err)
	}
	if stored, err := store.SessionByID(ctx, session.ID); err != nil || stored.State != "STARTING" {
		t.Fatalf("rolled-back normal session=%+v err=%v", stored, err)
	}
	if got := store.StandbyCount(ctx, "workspace"); got != 1 {
		t.Fatalf("rolled-back standby count=%d, want one", got)
	}
	var successes, exclusions int
	if err := store.db.QueryRowContext(ctx, `SELECT count(*) FROM standby_replenish_successes`).Scan(&successes); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRowContext(ctx, `SELECT count(*) FROM standby_replenish_exclusions`).Scan(&exclusions); err != nil {
		t.Fatal(err)
	}
	if successes != 0 || exclusions != 0 {
		t.Fatalf("rolled-back records successes=%d exclusions=%d", successes, exclusions)
	}
}

// createReadyStandby は job を終えた READY の待機 slot を作り、削除の予約が競合しない状態にする。
func createReadyStandby(t *testing.T, store *Store, id string) {
	t.Helper()
	ctx := context.Background()
	job, err := store.CreateStandby(ctx, Slot{ID: id, WorkspaceID: "workspace", Generation: 1, RootID: testRootID, RelPath: filepath.Join("workspace", id), State: "READY"}, nil)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	claimed, err := store.ClaimJob(ctx, job.ID, "test")
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if err := store.FinishJob(ctx, claimed.ID, "test", nil); err != nil {
		t.Fatalf("finish: %v", err)
	}
}

func TestRemovingSlotFreesStandbyRoomAndRemovalSchedulesReplenishment(t *testing.T) {
	store := openTestStore(t)
	seedWorkspace(t, store)
	ctx := context.Background()
	createReadyStandby(t, store, "returned")
	if got := store.StandbyCount(ctx, "workspace"); got != 1 {
		t.Fatalf("READY standby count=%d, want 1", got)
	}
	if _, changed, err := store.ScheduleRemoval(ctx, "returned", ""); err != nil || !changed {
		t.Fatalf("schedule removal changed=%v err=%v", changed, err)
	}
	if got := store.StandbyCount(ctx, "workspace"); got != 0 {
		t.Fatalf("REMOVING standby count=%d, want the slot to leave the warm count", got)
	}
	job, err := store.FinishRemoval(ctx, "returned")
	if err != nil {
		t.Fatal(err)
	}
	if job.Kind != "ENSURE_STANDBY" || job.WorkspaceID != "workspace" || job.State != "PENDING" {
		t.Fatalf("replenishment job=%+v, want a pending ENSURE_STANDBY for the workspace", job)
	}

	createReadyStandby(t, store, "second")
	if _, changed, err := store.ScheduleRemoval(ctx, "second", ""); err != nil || !changed {
		t.Fatalf("second schedule removal changed=%v err=%v", changed, err)
	}
	duplicate, err := store.FinishRemoval(ctx, "second")
	if err != nil {
		t.Fatal(err)
	}
	if duplicate.ID != "" {
		t.Fatalf("duplicate replenishment job=%+v, want none while the first is pending", duplicate)
	}
	var pending int
	if err := store.db.QueryRowContext(ctx, `SELECT count(*) FROM jobs WHERE kind='ENSURE_STANDBY'`).Scan(&pending); err != nil {
		t.Fatal(err)
	}
	if pending != 1 {
		t.Fatalf("ENSURE_STANDBY jobs=%d, want 1", pending)
	}
}

func TestRemovalDuringCleanDoesNotScheduleReplenishment(t *testing.T) {
	store := openTestStore(t)
	seedWorkspace(t, store)
	ctx := context.Background()
	createReadyStandby(t, store, "cleaned")
	if _, changed, err := store.ScheduleRemoval(ctx, "cleaned", ""); err != nil || !changed {
		t.Fatalf("schedule removal changed=%v err=%v", changed, err)
	}
	if _, err := store.db.ExecContext(ctx, `INSERT INTO clean_runs(id,mode,state,created_at,updated_at) VALUES('run','standby',?,?,?)`, CleanRunRunning, now(), now()); err != nil {
		t.Fatal(err)
	}
	job, err := store.FinishRemoval(ctx, "cleaned")
	if err != nil {
		t.Fatal(err)
	}
	if job.ID != "" {
		t.Fatalf("replenishment job=%+v, want none while clean is running", job)
	}
}
