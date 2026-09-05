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
