package state

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

func TestReleaseDuringPreparationDefersSnapshotUntilPreparationFinishes(t *testing.T) {
	store := openTestStore(t)
	seedWorkspace(t, store)
	ctx := context.Background()
	session := Session{ID: "session", WorkspaceID: "workspace", SlotID: "slot", State: "STARTING", AgentKind: "codex", TokenHash: HashToken("token")}
	if _, err := store.CreateSlotSession(ctx, Slot{ID: "slot", WorkspaceID: "workspace", Generation: 1, RootID: testRootID, RelPath: "workspace/slot", State: "PREPARING"}, nil, session, "PREPARE"); err != nil {
		t.Fatal(err)
	}
	if job, scheduled, err := store.Release(ctx, session.ID, session.WorkspaceID, session.SlotID); err != nil || scheduled || job.ID != "" {
		t.Fatalf("release during preparation: scheduled=%v job=%+v err=%v", scheduled, job, err)
	}
	storedSession, err := store.SessionByID(ctx, session.ID)
	if err != nil || storedSession.State != "RELEASING" {
		t.Fatalf("session=%+v err=%v", storedSession, err)
	}
	slot, err := store.Slot(ctx, session.SlotID)
	if err != nil || slot.State != "PREPARING" {
		t.Fatalf("slot=%+v err=%v", slot, err)
	}
	var snapshots int
	if err := store.db.QueryRow(`SELECT count(*) FROM jobs WHERE session_id=? AND kind='SNAPSHOT'`, session.ID).Scan(&snapshots); err != nil {
		t.Fatal(err)
	}
	if snapshots != 0 {
		t.Fatalf("snapshot jobs before preparation=%d", snapshots)
	}

	job, scheduled, err := store.FinishPreparationWithRelease(ctx, session.SlotID)
	if err != nil || !scheduled || job.Kind != "SNAPSHOT" || job.SessionID != session.ID || job.WorkspaceID != session.WorkspaceID {
		t.Fatalf("finish preparation: scheduled=%v job=%+v err=%v", scheduled, job, err)
	}
	slot, err = store.Slot(ctx, session.SlotID)
	if err != nil || slot.State != "DRAINING" {
		t.Fatalf("finished slot=%+v err=%v", slot, err)
	}
	if err := store.db.QueryRow(`SELECT count(*) FROM jobs WHERE session_id=? AND kind='SNAPSHOT'`, session.ID).Scan(&snapshots); err != nil {
		t.Fatal(err)
	}
	if snapshots != 1 {
		t.Fatalf("snapshot jobs after preparation=%d", snapshots)
	}
	if _, changed, err := store.Release(ctx, session.ID, session.WorkspaceID, session.SlotID); err != nil || changed {
		t.Fatalf("duplicate release: changed=%v err=%v", changed, err)
	}
}

func TestFinishPreparationLeasesSlotWhoseSessionBoundBeforePreparationFinished(t *testing.T) {
	store := openTestStore(t)
	seedWorkspace(t, store)
	ctx := context.Background()
	session := Session{ID: "session", WorkspaceID: "workspace", SlotID: "slot", State: "STARTING", AgentKind: "codex", TokenHash: HashToken("token")}
	if _, err := store.CreateSlotSession(ctx, Slot{ID: "slot", WorkspaceID: "workspace", Generation: 1, RootID: testRootID, RelPath: "workspace/slot", State: "PREPARING"}, nil, session, "PREPARE"); err != nil {
		t.Fatal(err)
	}
	if err := store.BindAgentSession(ctx, session.ID, "agent"); err != nil {
		t.Fatal(err)
	}
	bound, err := store.SessionByID(ctx, session.ID)
	if err != nil || bound.State != "ACTIVE" {
		t.Fatalf("bound session=%+v err=%v", bound, err)
	}
	job, scheduled, err := store.FinishPreparationWithRelease(ctx, session.SlotID)
	if err != nil || scheduled || job.ID != "" {
		t.Fatalf("finish preparation: scheduled=%v job=%+v err=%v", scheduled, job, err)
	}
	slot, err := store.Slot(ctx, session.SlotID)
	if err != nil || slot.State != "LEASED" {
		t.Fatalf("slot=%+v err=%v", slot, err)
	}
	finished, err := store.SessionByID(ctx, session.ID)
	if err != nil || finished.State != "ACTIVE" || finished.AgentSessionID != "agent" {
		t.Fatalf("session=%+v err=%v", finished, err)
	}
}

func TestReadySlotsRejectsCorruptGenerationType(t *testing.T) {
	store := openTestStore(t)
	seedWorkspace(t, store)
	job, err := store.CreateStandby(context.Background(), Slot{ID: "ready-corrupt", WorkspaceID: "workspace", Generation: 1, RootID: testRootID, RelPath: "workspace/ready-corrupt", State: "READY"}, nil)
	if err != nil || job.ID == "" {
		t.Fatalf("create standby job=%+v err=%v", job, err)
	}
	if _, err := store.db.Exec(`UPDATE slots SET generation='not-an-integer' WHERE id='ready-corrupt'`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ReadySlots(context.Background(), "workspace"); err == nil {
		t.Fatal("READY slot with corrupt generation type was decoded")
	}
}

func TestFinishPreparationSchedulesSnapshotAfterRelease(t *testing.T) {
	store := openTestStore(t)
	seedWorkspace(t, store)
	ctx := context.Background()
	session := Session{ID: "releasing", WorkspaceID: "workspace", SlotID: "releasing", State: "RELEASING", AgentKind: "codex", TokenHash: HashToken("token")}
	if _, err := store.CreateSlotSession(ctx, Slot{ID: "releasing", WorkspaceID: "workspace", Generation: 1, RootID: testRootID, RelPath: "workspace/releasing", State: "PREPARING"}, nil, session, ""); err != nil {
		t.Fatal(err)
	}
	job, scheduled, err := store.FinishPreparationWithRelease(ctx, session.SlotID)
	if err != nil || !scheduled || job.Kind != "SNAPSHOT" || job.WorkspaceID != "workspace" {
		t.Fatalf("finish preparation job=%+v scheduled=%v err=%v", job, scheduled, err)
	}
	slot, err := store.Slot(ctx, session.SlotID)
	if err != nil || slot.State != "DRAINING" {
		t.Fatalf("slot after release preparation=%+v err=%v", slot, err)
	}
}

func TestPreparationRetryAndOwnerStateBranches(t *testing.T) {
	ctx := context.Background()
	t.Run("retry failed preparation", func(t *testing.T) {
		store := openTestStore(t)
		seedWorkspace(t, store)
		root := t.TempDir()
		if _, err := store.CreateStandby(ctx, Slot{ID: "retry", WorkspaceID: "workspace", Generation: 1, RootID: testRootID, RelPath: "workspace/retry", State: "FAILED"}, []SlotRepository{{RepositoryID: "repository", WorktreePath: filepath.Join(root, "retry", "repository"), State: "PREPARE_RUNNING"}}); err != nil {
			t.Fatal(err)
		}
		if err := store.ResetPreparationForRetry(ctx, "retry"); err != nil {
			t.Fatal(err)
		}
		slot, err := store.Slot(ctx, "retry")
		if err != nil || slot.State != "PREPARING" {
			t.Fatalf("retried slot=%+v err=%v", slot, err)
		}
		repository, err := store.SlotRepository(ctx, "retry", "repository")
		if err != nil || repository.State != "PREPARING" {
			t.Fatalf("retried repository=%+v err=%v", repository, err)
		}
	})

	t.Run("finish unowned preparation", func(t *testing.T) {
		store := openTestStore(t)
		seedWorkspace(t, store)
		if _, err := store.CreateStandby(ctx, Slot{ID: "unowned", WorkspaceID: "workspace", Generation: 1, RootID: testRootID, RelPath: "workspace/unowned", State: "PREPARING"}, nil); err != nil {
			t.Fatal(err)
		}
		job, scheduled, err := store.FinishPreparationWithRelease(ctx, "unowned")
		if err != nil || scheduled || job.ID != "" {
			t.Fatalf("unowned finish job=%+v scheduled=%v err=%v", job, scheduled, err)
		}
		slot, err := store.Slot(ctx, "unowned")
		if err != nil || slot.State != "READY" {
			t.Fatalf("unowned slot=%+v err=%v", slot, err)
		}
	})

	t.Run("finish starting owner", func(t *testing.T) {
		store := openTestStore(t)
		seedWorkspace(t, store)
		session := Session{ID: "starting", WorkspaceID: "workspace", SlotID: "starting", State: "STARTING", AgentKind: "codex", TokenHash: HashToken("token")}
		if _, err := store.CreateSlotSession(ctx, Slot{ID: "starting", WorkspaceID: "workspace", Generation: 1, RootID: testRootID, RelPath: "workspace/starting", State: "PREPARING"}, nil, session, "PREPARE"); err != nil {
			t.Fatal(err)
		}
		job, scheduled, err := store.FinishPreparationWithRelease(ctx, "starting")
		if err != nil || scheduled || job.ID != "" {
			t.Fatalf("starting finish job=%+v scheduled=%v err=%v", job, scheduled, err)
		}
		slot, err := store.Slot(ctx, "starting")
		if err != nil || slot.State != "LEASED" {
			t.Fatalf("starting slot=%+v err=%v", slot, err)
		}
		stored, err := store.SessionByID(ctx, session.ID)
		if err != nil || stored.State != "ACTIVE" {
			t.Fatalf("starting session=%+v err=%v", stored, err)
		}
	})

	t.Run("finish restoring without pending mapping", func(t *testing.T) {
		store := openTestStore(t)
		seedWorkspace(t, store)
		session := Session{ID: "restoring", WorkspaceID: "workspace", SlotID: "restoring", State: "RESTORING", AgentKind: "codex", TokenHash: HashToken("token")}
		if _, err := store.CreateSlotSession(ctx, Slot{ID: "restoring", WorkspaceID: "workspace", Generation: 1, RootID: testRootID, RelPath: "workspace/restoring", State: "RESTORING"}, nil, session, "RESTORE"); err != nil {
			t.Fatal(err)
		}
		job, scheduled, err := store.FinishPreparationWithRelease(ctx, "restoring")
		if err != nil || scheduled || job.ID != "" {
			t.Fatalf("restoring finish job=%+v scheduled=%v err=%v", job, scheduled, err)
		}
		slot, err := store.Slot(ctx, "restoring")
		if err != nil || slot.State != "LEASED" {
			t.Fatalf("restoring slot=%+v err=%v", slot, err)
		}
		stored, err := store.SessionByID(ctx, session.ID)
		if err != nil || stored.State != "ACTIVE" {
			t.Fatalf("restoring session=%+v err=%v", stored, err)
		}
	})

	t.Run("finish owner in unexpected state", func(t *testing.T) {
		store := openTestStore(t)
		seedWorkspace(t, store)
		session := Session{ID: "unexpected", WorkspaceID: "workspace", SlotID: "unexpected", State: "STARTING", AgentKind: "codex", TokenHash: HashToken("token")}
		if _, err := store.CreateSlotSession(ctx, Slot{ID: "unexpected", WorkspaceID: "workspace", Generation: 1, RootID: testRootID, RelPath: "workspace/unexpected", State: "PREPARING"}, nil, session, "PREPARE"); err != nil {
			t.Fatal(err)
		}
		if err := store.MarkSessionState(ctx, session.ID, []string{"STARTING"}, "SNAPSHOTTING"); err != nil {
			t.Fatal(err)
		}
		if _, _, err := store.FinishPreparationWithRelease(ctx, "unexpected"); err == nil {
			t.Fatal("finish preparation with a SNAPSHOTTING owner session must fail")
		}
		slot, err := store.Slot(ctx, "unexpected")
		if err != nil || slot.State != "PREPARING" {
			t.Fatalf("unexpected slot=%+v err=%v", slot, err)
		}
	})
}

func TestReadAndRestoreStateBoundaries(t *testing.T) {
	store := openTestStore(t)
	seedWorkspace(t, store)
	ctx := context.Background()
	if _, found, err := store.ReadySlot(ctx, "missing"); err != nil || found {
		t.Fatalf("missing ready slot=%+v found=%v err=%v", Slot{}, found, err)
	}
	parent := Session{ID: "parent-copy", WorkspaceID: "workspace", SlotID: "parent-copy", State: "ARCHIVED", AgentKind: "codex", TokenHash: HashToken("parent")}
	parentRepo := SlotRepository{RepositoryID: "repository", DirName: "repository", State: "READY", RequestedRef: "main", BaseOID: "head", Fingerprint: "fingerprint"}
	if _, err := store.CreateSlotSession(ctx, Slot{ID: parent.SlotID, WorkspaceID: "workspace", Generation: 1, RootID: testRootID, RelPath: filepath.Join("workspace", parent.SlotID), State: "SNAPSHOTTED"}, []SlotRepository{parentRepo}, parent, ""); err != nil {
		t.Fatal(err)
	}
	child := Session{ID: "child-copy", WorkspaceID: "workspace", SlotID: "child-copy", ParentSessionID: parent.ID, State: "STARTING", AgentKind: "codex", TokenHash: HashToken("child")}
	if _, err := store.CreateSlotSession(ctx, Slot{ID: child.SlotID, WorkspaceID: "workspace", Generation: 1, RootID: testRootID, RelPath: filepath.Join("workspace", child.SlotID), State: "PREPARING"}, nil, child, "RESTORE"); err != nil {
		t.Fatal(err)
	}
	if repositories, err := store.SlotRepositories(ctx, child.SlotID); err != nil || len(repositories) != 0 {
		// Restore は所属情報を session テーブルへ複製するが、slot のメタデータは
		// prepare 段階でリポジトリを実体化するまで空のままである。
		t.Fatalf("child slot repositories=%v err=%v", repositories, err)
	}
	if err := store.BindAgentSession(ctx, child.ID, "agent-copy"); err != nil {
		t.Fatalf("bind child agent: %v", err)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE sessions SET state='RESTORING',parent_session_id=NULL,pending_agent_session_id='pending' WHERE id='child-copy'`); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.FinishPreparationWithRelease(ctx, child.SlotID); err == nil || !strings.Contains(err.Error(), "no parent") {
		t.Fatalf("pending mapping without parent error=%v", err)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE sessions SET parent_session_id='parent-copy' WHERE id='child-copy'`); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.FinishPreparationWithRelease(ctx, child.SlotID); err != nil {
		t.Fatalf("pending mapping with missing parent error=%v", err)
	}
}

func TestReadySlotCountMatchesTheReadySlotCandidateSet(t *testing.T) {
	store := openTestStore(t)
	seedWorkspace(t, store)
	ctx := context.Background()
	standby := func(id, state string, generation int) {
		t.Helper()
		if _, err := store.CreateStandby(ctx, Slot{ID: id, WorkspaceID: "workspace", Generation: generation, RootID: testRootID, RelPath: filepath.Join("workspace", id), State: state}, nil); err != nil {
			t.Fatal(err)
		}
	}
	standby("ready-old", "READY", 1)
	standby("ready-new", "READY", 1)
	standby("preparing", "PREPARING", 1)
	standby("obsolete", "READY", 0)
	if count, err := store.ReadySlotCount(ctx, "workspace"); err != nil || count != 2 {
		t.Fatalf("ready slot count=%d err=%v", count, err)
	}
	if err := store.SetSlotState(ctx, "ready-old", []string{"READY"}, "LEASED", ""); err != nil {
		t.Fatal(err)
	}
	if count, err := store.ReadySlotCount(ctx, "workspace"); err != nil || count != 1 {
		t.Fatalf("ready slot count after leasing=%d err=%v", count, err)
	}
	if count, err := store.ReadySlotCount(ctx, "missing"); err != nil || count != 0 {
		t.Fatalf("unknown workspace ready slot count=%d err=%v", count, err)
	}
}

func TestFinishPreparationWithReleasePropagatesSnapshotJobFault(t *testing.T) {
	store := openTestStore(t)
	seedWorkspace(t, store)
	ctx := context.Background()
	session := Session{ID: "releasing-fault", WorkspaceID: "workspace", SlotID: "releasing-fault", State: "RELEASING", AgentKind: "codex", TokenHash: HashToken("releasing-fault")}
	if _, err := store.CreateSlotSession(ctx, Slot{ID: "releasing-fault", WorkspaceID: "workspace", Generation: 1, RootID: testRootID, RelPath: "workspace/releasing-fault", State: "PREPARING"}, nil, session, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`CREATE TRIGGER fail_finish_preparation_snapshot BEFORE INSERT ON jobs WHEN NEW.kind='SNAPSHOT' AND NEW.slot_id='releasing-fault' BEGIN SELECT RAISE(ABORT,'fault'); END`); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.FinishPreparationWithRelease(ctx, session.SlotID); err == nil {
		t.Fatal("finish preparation succeeded despite a snapshot job insertion fault")
	}
	if slot, err := store.Slot(ctx, session.SlotID); err != nil || slot.State != "PREPARING" {
		t.Fatalf("rolled-back finish preparation slot=%+v err=%v", slot, err)
	}
}
