package state

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// installDeferredCommitFault は transaction 内では成功し、commit 時だけ外部キー違反になる行を作る。
// state の CAS が成功した後でも commit error を返し、成功扱いへ進まないことを検査する。
func installDeferredCommitFault(t *testing.T, store *Store, trigger string) {
	t.Helper()
	ctx := context.Background()
	for _, statement := range []string{
		`CREATE TABLE mutation_fault_parent(id TEXT PRIMARY KEY)`,
		`CREATE TABLE mutation_fault_child(id TEXT REFERENCES mutation_fault_parent(id) DEFERRABLE INITIALLY DEFERRED)`,
		trigger,
	} {
		if _, err := store.db.ExecContext(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
}

func TestMutationBackupSyncErrorsAreReturned(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	root, _, err := openBackupRootForTest(t, dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := syncBackupFile(root, "missing.tmp"); err == nil || !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing backup file error=%v, want os.ErrNotExist", err)
	}
	if err := root.Close(); err != nil {
		t.Fatal(err)
	}
	if err := syncBackupFile(root, "missing.tmp"); err == nil {
		t.Fatal("syncing a file through a closed root succeeded")
	}
	if err := syncBackupDirectory(root); err == nil {
		t.Fatal("syncing a directory through a closed root succeeded")
	}
}

func TestMutationWorkspaceForgetBlockersPropagatesBeginAndLookupErrors(t *testing.T) {
	t.Parallel()
	store := openTestStore(t)
	if _, err := store.WorkspaceForgetBlockers(context.Background(), "/missing"); err == nil {
		t.Fatal("forget blockers accepted an unknown workspace")
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := store.WorkspaceForgetBlockers(context.Background(), "/workspace"); err == nil {
		t.Fatal("forget blockers succeeded on a closed store")
	}
}

func TestMutationCountMetadataCandidatesSumsEverySource(t *testing.T) {
	t.Parallel()
	store := openTestStore(t)
	seedWorkspace(t, store)
	ctx := context.Background()
	old := FormatTime(time.Now().Add(-time.Hour))
	if _, err := store.db.ExecContext(ctx, `DELETE FROM events`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `INSERT INTO jobs(id,kind,state,attempt,finished_at) VALUES('old-job','PREPARE','SUCCEEDED',1,?)`, old); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `INSERT INTO events(time,level,kind,message) VALUES(?,'info','old-event','old')`, old); err != nil {
		t.Fatal(err)
	}
	session := Session{ID: "old-session", WorkspaceID: "workspace", SlotID: "old-session", State: "ACTIVE", AgentKind: "codex", TokenHash: HashToken("old-session")}
	if _, err := store.CreateSlotSession(ctx, Slot{ID: session.SlotID, WorkspaceID: session.WorkspaceID, Generation: 1, RootID: testRootID, RelPath: filepath.Join("workspace", session.SlotID), State: "LEASED"}, nil, session, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE sessions SET state='EXPIRED',expires_at=?,agent_session_id='old-agent' WHERE id=?`, old, session.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `INSERT INTO rpc_idempotency(idempotency_key,method,params,completed_at,expires_at,state) VALUES('old-rpc','test','{}',?,?, 'COMPLETED')`, old, old); err != nil {
		t.Fatal(err)
	}
	threshold := FormatTime(time.Now().Add(time.Hour))
	count, err := store.CountMetadataCandidates(ctx, threshold, threshold, threshold)
	if err != nil || count != 4 {
		t.Fatalf("metadata candidate count=%d err=%v, want four sources", count, err)
	}
}

func TestMutationFinishJobWithDetailPublishesElapsedAndFailureDetails(t *testing.T) {
	t.Parallel()
	store := openTestStore(t)
	ctx := context.Background()
	job, err := store.CreateJob(ctx, "PREPARE", "", "slot", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ClaimJob(ctx, job.ID, "owner"); err != nil {
		t.Fatal(err)
	}
	started := FormatTime(time.Now().Add(-time.Second))
	if _, err := store.db.ExecContext(ctx, `UPDATE jobs SET started_at=? WHERE id=?`, started, job.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.FinishJobWithDetail(ctx, job.ID, "owner", errors.New("prepare failed"), "PREPARE_FAILED", "/logs/prepare.log"); err != nil {
		t.Fatal(err)
	}
	var message string
	if err := store.db.QueryRowContext(ctx, `SELECT message FROM events WHERE slot_id='slot' ORDER BY id DESC LIMIT 1`).Scan(&message); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"state=FAILED", "failure_code=PREPARE_FAILED", "detail_path=/logs/prepare.log"} {
		if !strings.Contains(message, want) {
			t.Fatalf("job event=%q, missing %q", message, want)
		}
	}
	if strings.Contains(message, "elapsed=0s") {
		t.Fatalf("job event did not calculate elapsed time: %q", message)
	}
}

func TestMutationConfirmSlotCreationReturnsCommitFailure(t *testing.T) {
	t.Parallel()
	store := openTestStore(t)
	seedWorkspace(t, store)
	ctx := context.Background()
	slot := Slot{ID: "confirm-commit-fault", WorkspaceID: "workspace", Generation: 1, RootID: testRootID, RelPath: "workspace/confirm-commit-fault", OwnerSessionID: "confirm-owner"}
	if err := store.ReserveSlot(ctx, slot); err != nil {
		t.Fatal(err)
	}
	installDeferredCommitFault(t, store, `CREATE TRIGGER mutation_confirm_commit_fault AFTER UPDATE OF state ON slots WHEN NEW.id='confirm-commit-fault' BEGIN INSERT INTO mutation_fault_child(id) VALUES('missing'); END`)
	if err := store.ConfirmSlotCreation(ctx, slot.ID, "identity"); err == nil {
		t.Fatal("slot creation confirmation hid its commit failure")
	}
	stored, err := store.Slot(ctx, slot.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.State != "ALLOCATING" || stored.DirIdentity != "" {
		t.Fatalf("failed confirmation changed reservation=%+v", stored)
	}
}

func TestMutationQuarantineReservedSlotReturnsCommitFailure(t *testing.T) {
	t.Parallel()
	store := openTestStore(t)
	seedWorkspace(t, store)
	ctx := context.Background()
	slot := Slot{ID: "quarantine-commit-fault", WorkspaceID: "workspace", Generation: 1, RootID: testRootID, RelPath: "workspace/quarantine-commit-fault"}
	if err := store.ReserveSlot(ctx, slot); err != nil {
		t.Fatal(err)
	}
	installDeferredCommitFault(t, store, `CREATE TRIGGER mutation_quarantine_commit_fault AFTER UPDATE OF state ON slots WHEN NEW.id='quarantine-commit-fault' BEGIN INSERT INTO mutation_fault_child(id) VALUES('missing'); END`)
	if err := store.QuarantineReservedSlot(ctx, slot.ID, "ALLOCATION_FAILED"); err == nil {
		t.Fatal("slot quarantine hid its commit failure")
	}
	stored, err := store.Slot(ctx, slot.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.State != "ALLOCATING" {
		t.Fatalf("failed quarantine changed reservation state=%s", stored.State)
	}
}

func TestMutationRegisterReservedSlotStoresWorkspaceMembershipMetadata(t *testing.T) {
	t.Parallel()
	store := openTestStore(t)
	seedWorkspace(t, store)
	ctx := context.Background()
	session := Session{ID: "registered-session", WorkspaceID: "workspace", SlotID: "registered-slot", State: "STARTING", AgentKind: "codex", TokenHash: HashToken("registered-session")}
	slot := Slot{ID: session.SlotID, WorkspaceID: session.WorkspaceID, Generation: 1, RootID: testRootID, RelPath: "workspace/registered-slot", OwnerSessionID: session.ID}
	if err := store.ReserveSlot(ctx, slot); err != nil {
		t.Fatal(err)
	}
	if err := store.ConfirmSlotCreation(ctx, slot.ID, "registered-identity"); err != nil {
		t.Fatal(err)
	}
	job, err := store.RegisterReservedSlotSession(ctx, slot.ID, []SlotRepository{{RepositoryID: "repository", DirName: "repository", State: "PREPARING", RequestedRef: "main", BaseOID: "base", Fingerprint: "fingerprint"}}, session, "PREPARING", "PREPARE")
	if err != nil || job.Kind != "PREPARE" {
		t.Fatalf("reserved registration job=%+v err=%v", job, err)
	}
	var memberships int
	if err := store.db.QueryRowContext(ctx, `SELECT count(*) FROM session_repositories WHERE session_id=?`, session.ID).Scan(&memberships); err != nil {
		t.Fatal(err)
	}
	if memberships != 1 {
		t.Fatalf("session repository memberships=%d, want one", memberships)
	}
	var lastLeasedAt string
	if err := store.db.QueryRowContext(ctx, `SELECT COALESCE(last_leased_at,'') FROM repositories WHERE id='repository'`).Scan(&lastLeasedAt); err != nil {
		t.Fatal(err)
	}
	if lastLeasedAt == "" {
		t.Fatal("reserved registration did not record repository lease time")
	}
}

func TestMutationReplacePlacementsAllowsRestoringSlots(t *testing.T) {
	t.Parallel()
	store := openTestStore(t)
	seedWorkspace(t, store)
	ctx := context.Background()
	session := Session{ID: "restoring-placement", WorkspaceID: "workspace", SlotID: "restoring-placement", State: "RESTORING", AgentKind: "codex", TokenHash: HashToken("restoring-placement")}
	if _, err := store.CreateSlotSession(ctx, Slot{ID: session.SlotID, WorkspaceID: session.WorkspaceID, Generation: 1, RootID: testRootID, RelPath: "workspace/restoring-placement", State: "RESTORING"}, nil, session, ""); err != nil {
		t.Fatal(err)
	}
	placement := Placement{RepositoryID: "repository", RelativePath: "config", Kind: "copy", SourcePath: "/source/config", ContentSHA256: "hash"}
	if err := store.ReplacePlacements(ctx, session.SlotID, []Placement{placement}); err != nil {
		t.Fatal(err)
	}
	placements, err := store.Placements(ctx, session.SlotID)
	if err != nil || len(placements) != 1 || placements[0] != placement {
		t.Fatalf("restoring placements=%+v err=%v", placements, err)
	}
	stored, err := store.Slot(ctx, session.SlotID)
	if err != nil || !stored.PlacementHistoryComplete {
		t.Fatalf("restoring slot placement history=%+v err=%v", stored, err)
	}
}

func TestMutationBeginStandbyUpdatePropagatesWriteFailure(t *testing.T) {
	t.Parallel()
	store := openTestStore(t)
	seedWorkspace(t, store)
	ctx := context.Background()
	if _, err := store.CreateStandby(ctx, Slot{ID: "begin-update-fault", WorkspaceID: "workspace", Generation: 1, RootID: testRootID, RelPath: "workspace/begin-update-fault", State: "PREPARING"}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateJob(ctx, "UPDATE", "workspace", "begin-update-fault", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `CREATE TRIGGER mutation_begin_update_fault BEFORE UPDATE OF update_started_at ON slots WHEN OLD.id='begin-update-fault' BEGIN SELECT RAISE(ABORT,'begin update fault'); END`); err != nil {
		t.Fatal(err)
	}
	if err := store.BeginStandbyUpdate(ctx, "begin-update-fault"); err == nil {
		t.Fatal("standby update start hid its write failure")
	}
}

func TestMutationCompleteStandbyUpdatePropagatesRepositoryWriteFailure(t *testing.T) {
	t.Parallel()
	store := openTestStore(t)
	seedWorkspace(t, store)
	ctx := context.Background()
	if _, err := store.CreateStandby(ctx, Slot{ID: "complete-update-fault", WorkspaceID: "workspace", Generation: 1, RootID: testRootID, RelPath: "workspace/complete-update-fault", State: "PREPARING"}, []SlotRepository{{RepositoryID: "repository", DirName: "repository", State: "UPDATE_RUNNING", UpdateBaseOID: "base", UpdateFingerprint: "fingerprint"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `CREATE TRIGGER mutation_complete_update_fault BEFORE UPDATE OF state ON slot_repositories WHEN OLD.slot_id='complete-update-fault' BEGIN SELECT RAISE(ABORT,'complete update fault'); END`); err != nil {
		t.Fatal(err)
	}
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := completeStandbyUpdateRepositories(ctx, tx, "complete-update-fault"); err == nil {
		t.Fatal("standby repository completion hid its write failure")
	}
	_ = tx.Rollback()
}

func TestMutationStageStandbyUpdateStoresEveryPlacement(t *testing.T) {
	t.Parallel()
	store := openTestStore(t)
	seedWorkspace(t, store)
	ctx := context.Background()
	repository := SlotRepository{RepositoryID: "repository", DirName: "repository", State: "READY", RequestedRef: "main", BaseOID: "old", Fingerprint: "old-fingerprint", CompatibilityFingerprint: "compatible"}
	prepareJob, err := store.CreateStandby(ctx, Slot{ID: "multi-placement", WorkspaceID: "workspace", Generation: 1, RootID: testRootID, RelPath: "workspace/multi-placement", State: "PREPARING"}, []SlotRepository{repository})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.ReplacePlacements(ctx, "multi-placement", []Placement{{RepositoryID: "repository", RelativePath: "old", Kind: "copy", SourcePath: "/source/old", ContentSHA256: "old-hash"}}); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkReady(ctx, "multi-placement"); err != nil {
		t.Fatal(err)
	}
	claimed, err := store.ClaimJob(ctx, prepareJob.ID, "prepare")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.FinishJob(ctx, claimed.ID, "prepare", nil); err != nil {
		t.Fatal(err)
	}
	target := SlotRepository{RepositoryID: "repository", RequestedRef: "feature", BaseOID: "new", Fingerprint: "new-fingerprint", CompatibilityFingerprint: "compatible", UpdateBaseOID: "old", UpdateFingerprint: "old-fingerprint"}
	placements := []Placement{
		{RepositoryID: "repository", RelativePath: "one", Kind: "copy", SourcePath: "/source/one", ContentSHA256: "one-hash"},
		{RepositoryID: "repository", RelativePath: "two", Kind: "copy", SourcePath: "/source/two", ContentSHA256: "two-hash"},
	}
	if _, err := store.ReserveIdleStandbyUpdate(ctx, "multi-placement", "workspace", []SlotRepository{target}, placements, "copy"); err != nil {
		t.Fatal(err)
	}
	staged, err := store.UpdatePlacements(ctx, "multi-placement")
	if err != nil || len(staged) != len(placements) {
		t.Fatalf("staged placements=%+v err=%v", staged, err)
	}
}

func TestMutationRecordStandbySuccessPropagatesEachFailureBoundary(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	t.Run("begin", func(t *testing.T) {
		store := openTestStore(t)
		if err := store.Close(); err != nil {
			t.Fatal(err)
		}
		if _, _, err := store.RecordStandbySuccess(ctx, "missing"); err == nil {
			t.Fatal("standby success accepted a closed store")
		}
	})
	t.Run("record", func(t *testing.T) {
		store := openTestStore(t)
		if _, err := store.db.ExecContext(ctx, `DROP TABLE sessions`); err != nil {
			t.Fatal(err)
		}
		if _, _, err := store.RecordStandbySuccess(ctx, "missing"); err == nil {
			t.Fatal("standby success hid its recording query failure")
		}
	})
	t.Run("commit", func(t *testing.T) {
		store := openTestStore(t)
		seedWorkspace(t, store)
		session := Session{ID: "success-commit-fault", WorkspaceID: "workspace", SlotID: "success-commit-fault", State: "ACTIVE", AgentKind: "codex", TokenHash: HashToken("success-commit-fault")}
		if _, err := store.CreateSlotSession(ctx, Slot{ID: session.SlotID, WorkspaceID: session.WorkspaceID, Generation: 1, RootID: testRootID, RelPath: "workspace/success-commit-fault", State: "LEASED"}, nil, session, ""); err != nil {
			t.Fatal(err)
		}
		installDeferredCommitFault(t, store, `CREATE TRIGGER mutation_success_commit_fault AFTER INSERT ON standby_replenish_successes BEGIN INSERT INTO mutation_fault_child(id) VALUES('missing'); END`)
		if _, _, err := store.RecordStandbySuccess(ctx, session.ID); err == nil {
			t.Fatal("standby success hid its commit failure")
		}
		var successes int
		if err := store.db.QueryRowContext(ctx, `SELECT count(*) FROM standby_replenish_successes`).Scan(&successes); err != nil {
			t.Fatal(err)
		}
		if successes != 0 {
			t.Fatalf("failed standby success left %d durable records", successes)
		}
	})
}

func TestMutationRecordStandbySuccessAcceptsStartingSession(t *testing.T) {
	t.Parallel()
	store := openTestStore(t)
	seedWorkspace(t, store)
	ctx := context.Background()
	session := Session{ID: "starting-success", WorkspaceID: "workspace", SlotID: "starting-success", State: "STARTING", AgentKind: "codex", TokenHash: HashToken("starting-success")}
	if _, err := store.CreateSlotSession(ctx, Slot{ID: session.SlotID, WorkspaceID: session.WorkspaceID, Generation: 1, RootID: testRootID, RelPath: "workspace/starting-success", State: "LEASED"}, nil, session, ""); err != nil {
		t.Fatal(err)
	}
	if _, created, err := store.RecordStandbySuccess(ctx, session.ID); err != nil || !created {
		t.Fatalf("starting standby success created=%v err=%v", created, err)
	}
}

func TestMutationReleaseReturnsCommitFailureWhilePreparing(t *testing.T) {
	t.Parallel()
	store := openTestStore(t)
	seedWorkspace(t, store)
	ctx := context.Background()
	session := Session{ID: "release-commit-fault", WorkspaceID: "workspace", SlotID: "release-commit-fault", State: "ACTIVE", AgentKind: "codex", TokenHash: HashToken("release-commit-fault")}
	if _, err := store.CreateSlotSession(ctx, Slot{ID: session.SlotID, WorkspaceID: session.WorkspaceID, Generation: 1, RootID: testRootID, RelPath: "workspace/release-commit-fault", State: "PREPARING"}, nil, session, ""); err != nil {
		t.Fatal(err)
	}
	installDeferredCommitFault(t, store, fmt.Sprintf(`CREATE TRIGGER mutation_release_commit_fault AFTER UPDATE OF state ON sessions WHEN NEW.id='%s' BEGIN INSERT INTO mutation_fault_child(id) VALUES('missing'); END`, session.ID))
	if _, _, err := store.Release(ctx, session.ID, session.WorkspaceID, session.SlotID); err == nil {
		t.Fatal("release hid its commit failure")
	}
	stored, err := store.SessionByID(ctx, session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.State != "ACTIVE" {
		t.Fatalf("failed release changed session state=%s", stored.State)
	}
}
