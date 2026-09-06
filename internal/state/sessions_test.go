package state

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestReleaseCreatesExactlyOneSnapshotJob(t *testing.T) {
	store := openTestStore(t)
	seedWorkspace(t, store)
	ctx := context.Background()
	session := Session{ID: "session", WorkspaceID: "workspace", SlotID: "slot", State: "ACTIVE", AgentKind: "codex", TokenHash: HashToken("token")}
	if _, err := store.CreateSlotSession(ctx, Slot{ID: "slot", WorkspaceID: "workspace", Generation: 1, RootID: testRootID, RelPath: "workspace/slot", State: "LEASED"}, nil, session, ""); err != nil {
		t.Fatal(err)
	}
	job, changed, err := store.Release(ctx, session.ID, session.WorkspaceID, session.SlotID)
	if err != nil || !changed || job.Kind != "SNAPSHOT" {
		t.Fatalf("first release: changed=%v job=%+v err=%v", changed, job, err)
	}
	if _, changed, err := store.Release(ctx, session.ID, session.WorkspaceID, session.SlotID); err != nil || changed {
		t.Fatalf("duplicate release: changed=%v err=%v", changed, err)
	}
	var jobs int
	if err := store.db.QueryRow(`SELECT count(*) FROM jobs WHERE session_id='session' AND kind='SNAPSHOT'`).Scan(&jobs); err != nil {
		t.Fatal(err)
	}
	if jobs != 1 {
		t.Fatalf("snapshot jobs=%d, want 1", jobs)
	}
}

func TestReleaseCleansUpUnboundAndRestoringSessions(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	for _, sessionState := range []string{"UNBOUND", "RESTORING"} {
		t.Run(sessionState, func(t *testing.T) {
			id := strings.ToLower(sessionState)
			session := Session{ID: id, SlotID: id, State: sessionState, AgentKind: "codex", TokenHash: HashToken("token")}
			if _, err := store.CreateSlotSession(ctx, Slot{ID: id, State: sessionState, RootID: testRootID, RelPath: filepath.Join("_unbound", id)}, nil, session, ""); err != nil {
				t.Fatal(err)
			}
			job, changed, err := store.Release(ctx, id, "", id)
			if err != nil || !changed || job.Kind != "REMOVE" || job.SessionID != "" {
				t.Fatalf("release job=%+v changed=%v err=%v", job, changed, err)
			}
			storedSession, err := store.SessionByID(ctx, id)
			if err != nil || storedSession.State != "EXPIRED" {
				t.Fatalf("session=%+v err=%v", storedSession, err)
			}
			slot, err := store.Slot(ctx, id)
			if err != nil || slot.State != "REMOVING" || slot.OwnerSessionID != "" {
				t.Fatalf("slot=%+v err=%v", slot, err)
			}
			if _, changed, err := store.Release(ctx, id, "", id); err != nil || changed {
				t.Fatalf("duplicate release changed=%v err=%v", changed, err)
			}
		})
	}
}

// READY は UNBOUND slot が実際に遷移できる状態（PREPARING・RESTORING・QUARANTINED）ではなく、
// session だけ先に EXPIRED へ進めた後の CAS ガードを踏ませるための人工的な状態である。
func TestUnboundReleaseRollsBackWhenSlotStateChanged(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	session := Session{ID: "unbound", SlotID: "unbound", State: "UNBOUND", AgentKind: "codex", TokenHash: HashToken("token")}
	if _, err := store.CreateSlotSession(ctx, Slot{ID: "unbound", State: "UNBOUND", RootID: testRootID, RelPath: "_unbound/unbound"}, nil, session, ""); err != nil {
		t.Fatal(err)
	}
	if err := store.SetSlotState(ctx, "unbound", []string{"UNBOUND"}, "READY", ""); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Release(ctx, "unbound", "", "unbound"); err == nil {
		t.Fatal("release succeeded after slot ownership state changed")
	}
	storedSession, err := store.SessionByID(ctx, "unbound")
	if err != nil || storedSession.State != "UNBOUND" {
		t.Fatalf("release transaction did not roll back: session=%+v err=%v", storedSession, err)
	}
}

// 隔離 slot の owner は DRAINING へ進めないため、返却は session を終端させて owner を外す一度きりの操作になる。
// これが成立しないと orphan reconcile が同じ session の返却を無限に再試行する。
func TestReleaseExpiresSessionOwningQuarantinedSlot(t *testing.T) {
	ctx := context.Background()
	for _, sessionState := range []string{"STARTING", "ACTIVE", "UNBOUND", "RESTORING"} {
		t.Run(sessionState, func(t *testing.T) {
			store := openTestStore(t)
			seedWorkspace(t, store)
			id := "quarantined-" + strings.ToLower(sessionState)
			session := Session{ID: id, WorkspaceID: "workspace", SlotID: id, State: sessionState, AgentKind: "codex", TokenHash: HashToken(id)}
			if _, err := store.CreateSlotSession(ctx, Slot{ID: id, WorkspaceID: "workspace", Generation: 1, RootID: testRootID, RelPath: filepath.Join("workspace", id), State: "PREPARING"}, nil, session, ""); err != nil {
				t.Fatal(err)
			}
			if err := store.SetSlotState(ctx, id, []string{"PREPARING"}, "QUARANTINED", "JOB_RETRY_EXHAUSTED"); err != nil {
				t.Fatal(err)
			}
			job, changed, quarantineExpired, err := store.ReleaseWithOutcome(ctx, id, "workspace", id)
			if err != nil || changed || job.ID != "" || !quarantineExpired {
				t.Fatalf("release job=%+v changed=%v quarantineExpired=%v err=%v", job, changed, quarantineExpired, err)
			}
			storedSession, err := store.SessionByID(ctx, id)
			if err != nil || storedSession.State != "EXPIRED" {
				t.Fatalf("session=%+v err=%v", storedSession, err)
			}
			slot, err := store.Slot(ctx, id)
			if err != nil || slot.State != "QUARANTINED" || slot.OwnerSessionID != "" || slot.FailureCode != "JOB_RETRY_EXHAUSTED" {
				t.Fatalf("slot=%+v err=%v", slot, err)
			}
			// 終端後は候補に残らず、同じ返却を繰り返してもエラーにならない。
			// 二度目は終端する session が無いため、隔離による終端としても報告しない。
			if _, changed, quarantineExpired, err := store.ReleaseWithOutcome(ctx, id, "workspace", id); err != nil || changed || quarantineExpired {
				t.Fatalf("repeated release changed=%v quarantineExpired=%v err=%v", changed, quarantineExpired, err)
			}
			candidates, err := store.OrphanCandidates(ctx, FormatTime(time.Now().Add(time.Hour)))
			if err != nil {
				t.Fatal(err)
			}
			for _, candidate := range candidates {
				if candidate.ID == id {
					t.Fatalf("expired session remained an orphan candidate: %+v", candidate)
				}
			}
		})
	}
}

func TestAgentProcessRegistrationAndDependencyDeferral(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	if _, err := store.CreateSlotSession(ctx, Slot{ID: "agent", State: "LEASED", RootID: testRootID, RelPath: "_unbound/agent"}, nil, Session{ID: "agent", SlotID: "agent", State: "ACTIVE", AgentKind: "codex", TokenHash: HashToken("token")}, ""); err != nil {
		t.Fatal(err)
	}
	if err := store.RegisterAgentProcess(ctx, "agent", "token", 0); err == nil {
		t.Fatal("agent process registration accepted a non-positive PID")
	}
	if err := store.RegisterAgentProcess(ctx, "agent", "wrong", os.Getpid()); err == nil {
		t.Fatal("agent process registration accepted an invalid token")
	}
	if err := store.RegisterAgentProcess(ctx, "agent", "token", os.Getpid()); err != nil {
		t.Fatal(err)
	}
	session, err := store.SessionByID(ctx, "agent")
	if err != nil || session.AgentPID != os.Getpid() {
		t.Fatalf("registered agent process=%d err=%v", session.AgentPID, err)
	}
	if _, err := store.db.ExecContext(ctx, `CREATE TRIGGER fail_agent_pid BEFORE UPDATE OF agent_pid ON sessions BEGIN SELECT RAISE(ABORT,'fault'); END`); err != nil {
		t.Fatal(err)
	}
	if err := store.RegisterAgentProcess(ctx, "agent", "token", os.Getpid()); err == nil {
		t.Fatal("agent process registration ignored a storage failure")
	}
	if _, err := store.db.ExecContext(ctx, `DROP TRIGGER fail_agent_pid`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE sessions SET state='ARCHIVED' WHERE id='agent'`); err != nil {
		t.Fatal(err)
	}
	if err := store.RegisterAgentProcess(ctx, "agent", "token", os.Getpid()); err == nil {
		t.Fatal("agent process registration accepted an archived session")
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE sessions SET state='ACTIVE' WHERE id='agent'`); err != nil {
		t.Fatal(err)
	}

	job, err := store.CreateJob(ctx, "RESTORE", "", "agent", "agent")
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := store.ClaimJob(ctx, job.ID, "worker")
	if err != nil || claimed.Attempt != 1 {
		t.Fatalf("claimed job=%+v err=%v", claimed, err)
	}
	if err := store.DeferJob(ctx, job.ID, "wrong-worker", 0, "SNAPSHOT_PENDING"); err == nil {
		t.Fatal("dependency deferral accepted the wrong lease owner")
	}
	if err := store.DeferJob(ctx, job.ID, "worker", 0, "SNAPSHOT_PENDING"); err != nil {
		t.Fatal(err)
	}
	claimed, err = store.ClaimJob(ctx, job.ID, "worker")
	if err != nil || claimed.Attempt != 1 {
		t.Fatalf("dependency wait consumed retry budget: job=%+v err=%v", claimed, err)
	}
	if _, err := store.db.ExecContext(ctx, `CREATE TRIGGER fail_defer_event BEFORE INSERT ON events BEGIN SELECT RAISE(ABORT,'fault'); END`); err != nil {
		t.Fatal(err)
	}
	if err := store.DeferJob(ctx, job.ID, "worker", 0, "SNAPSHOT_PENDING"); err == nil {
		t.Fatal("dependency deferral ignored event persistence failure")
	}
	if _, err := store.db.ExecContext(ctx, `DROP TRIGGER fail_defer_event`); err != nil {
		t.Fatal(err)
	}
	closed := openTestStore(t)
	if err := closed.Close(); err != nil {
		t.Fatal(err)
	}
	if err := closed.DeferJob(ctx, "missing", "worker", 0, "SNAPSHOT_PENDING"); err == nil {
		t.Fatal("dependency deferral succeeded after store closure")
	}
}

func TestColdLeaseRollsBackAtEveryPersistenceBoundary(t *testing.T) {
	tests := []struct {
		name    string
		trigger string
	}{
		{name: "slot update", trigger: `CREATE TRIGGER fail_cold_slot_update BEFORE UPDATE ON slots BEGIN SELECT RAISE(ABORT,'fault'); END`},
		{name: "repository state update", trigger: `CREATE TRIGGER fail_cold_repository_state BEFORE UPDATE ON slot_repositories BEGIN SELECT RAISE(ABORT,'fault'); END`},
		{name: "session insert", trigger: `CREATE TRIGGER fail_cold_session_insert BEFORE INSERT ON sessions BEGIN SELECT RAISE(ABORT,'fault'); END`},
		{name: "lease timestamp", trigger: `CREATE TRIGGER fail_cold_lease_timestamp BEFORE UPDATE OF last_leased_at ON repositories BEGIN SELECT RAISE(ABORT,'fault'); END`},
		{name: "job insert", trigger: `CREATE TRIGGER fail_cold_job_insert BEFORE INSERT ON jobs BEGIN SELECT RAISE(ABORT,'fault'); END`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := openTestStore(t)
			seedWorkspace(t, store)
			ctx := context.Background()
			root := t.TempDir()
			if _, err := store.CreateStandby(ctx, Slot{ID: "cold", WorkspaceID: "workspace", Generation: 1, RootID: testRootID, RelPath: "workspace/cold", State: "READY"}, []SlotRepository{{RepositoryID: "repository", WorktreePath: filepath.Join(root, "cold", "root"), State: "COLD"}}); err != nil {
				t.Fatal(err)
			}
			if _, err := store.db.ExecContext(ctx, test.trigger); err != nil {
				t.Fatal(err)
			}
			session := Session{ID: "session", WorkspaceID: "workspace", SlotID: "cold", AgentKind: "codex", TokenHash: HashToken("token")}
			if _, err := store.LeaseReadyWithCold(ctx, "cold", session); err == nil {
				t.Fatal("fault-injected cold lease succeeded")
			}
			slot, err := store.Slot(ctx, "cold")
			if err != nil || slot.State != "READY" || slot.OwnerSessionID != "" {
				t.Fatalf("cold lease did not roll back slot: slot=%+v err=%v", slot, err)
			}
			repository, err := store.SlotRepository(ctx, "cold", "repository")
			if err != nil || repository.State != "COLD" {
				t.Fatalf("cold lease did not roll back repository: repository=%+v err=%v", repository, err)
			}
			if _, err := store.SessionByID(ctx, "session"); !errors.Is(err, sql.ErrNoRows) {
				t.Fatalf("cold lease retained a partial session: %v", err)
			}
		})
	}
}

func TestReleaseUnboundSessionSchedulesRemoval(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	session := Session{ID: "unbound", SlotID: "unbound", State: "UNBOUND", AgentKind: "codex", TokenHash: HashToken("token")}
	if _, err := store.CreateSlotSession(ctx, Slot{ID: session.SlotID, State: "UNBOUND", RootID: testRootID, RelPath: filepath.Join("_unbound", session.SlotID)}, nil, session, ""); err != nil {
		t.Fatal(err)
	}
	job, changed, err := store.Release(ctx, session.ID, "", session.SlotID)
	if err != nil || !changed || job.Kind != "REMOVE" {
		t.Fatalf("unbound release job=%+v changed=%v err=%v", job, changed, err)
	}
	storedSession, err := store.SessionByID(ctx, session.ID)
	if err != nil || storedSession.State != "EXPIRED" {
		t.Fatalf("session after unbound release=%+v err=%v", storedSession, err)
	}
	storedSlot, err := store.Slot(ctx, session.SlotID)
	if err != nil || storedSlot.State != "REMOVING" || storedSlot.OwnerSessionID != "" {
		t.Fatalf("slot after unbound release=%+v err=%v", storedSlot, err)
	}
}

func TestLeaseReadyWithColdPromotesRepositoriesAndStartsPreparation(t *testing.T) {
	store := openTestStore(t)
	seedWorkspace(t, store)
	ctx := context.Background()
	root := t.TempDir()
	if _, err := store.CreateStandby(ctx, Slot{ID: "cold-slot", WorkspaceID: "workspace", Generation: 1, RootID: testRootID, RelPath: "workspace/cold-slot", State: "READY"}, []SlotRepository{{RepositoryID: "repository", WorktreePath: filepath.Join(root, "cold-slot", "repository"), State: "COLD", RequestedRef: "main", BaseOID: "head", Fingerprint: "fingerprint"}}); err != nil {
		t.Fatal(err)
	}
	session := Session{ID: "cold-session", WorkspaceID: "workspace", SlotID: "cold-slot", State: "ACTIVE", AgentKind: "codex", TokenHash: HashToken("token")}
	job, err := store.LeaseReadyWithCold(ctx, "cold-slot", session)
	if err != nil || job.Kind != "PREPARE" || job.SessionID != session.ID {
		t.Fatalf("cold lease job=%+v err=%v", job, err)
	}
	slot, err := store.Slot(ctx, "cold-slot")
	if err != nil || slot.State != "PREPARING" || slot.OwnerSessionID != session.ID {
		t.Fatalf("cold lease slot=%+v err=%v", slot, err)
	}
	repository, err := store.SlotRepository(ctx, "cold-slot", "repository")
	if err != nil || repository.State != "PREPARING" {
		t.Fatalf("cold lease repository=%+v err=%v", repository, err)
	}
	started, err := store.SessionByID(ctx, session.ID)
	if err != nil || started.State != "STARTING" {
		t.Fatalf("cold lease session=%+v err=%v", started, err)
	}
	rows, err := store.db.QueryContext(ctx, `SELECT repository_id,relative_path,ordinal FROM session_repositories WHERE session_id=?`, session.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	if !rows.Next() {
		t.Fatal("cold lease did not preserve current repository membership")
	}
	var repositoryID, relativePath string
	var ordinal int
	if err := rows.Scan(&repositoryID, &relativePath, &ordinal); err != nil {
		t.Fatal(err)
	}
	if repositoryID != "repository" || relativePath != "" || ordinal != 0 {
		t.Fatalf("cold lease membership=%q %q %d", repositoryID, relativePath, ordinal)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
}

func TestReleasePropagatesTransactionFaults(t *testing.T) {
	ctx := context.Background()

	t.Run("draining slot update fault", func(t *testing.T) {
		store := openTestStore(t)
		seedWorkspace(t, store)
		session := Session{ID: "release-drain-fault", WorkspaceID: "workspace", SlotID: "release-drain-fault", State: "ACTIVE", AgentKind: "codex", TokenHash: HashToken("release-drain-fault")}
		if _, err := store.CreateSlotSession(ctx, Slot{ID: "release-drain-fault", WorkspaceID: "workspace", Generation: 1, RootID: testRootID, RelPath: "workspace/release-drain-fault", State: "LEASED"}, nil, session, ""); err != nil {
			t.Fatal(err)
		}
		if _, err := store.db.Exec(`CREATE TRIGGER fail_release_drain BEFORE UPDATE OF state ON slots WHEN OLD.id='release-drain-fault' BEGIN SELECT RAISE(ABORT,'fault'); END`); err != nil {
			t.Fatal(err)
		}
		if _, _, err := store.Release(ctx, session.ID, session.WorkspaceID, session.SlotID); err == nil {
			t.Fatal("release succeeded despite a draining slot transition fault")
		}
	})

	t.Run("unbound session removal job insertion fault", func(t *testing.T) {
		store := openTestStore(t)
		seedWorkspace(t, store)
		session := Session{ID: "release-unbound-fault", WorkspaceID: "workspace", SlotID: "release-unbound-fault", State: "UNBOUND", AgentKind: "codex", TokenHash: HashToken("release-unbound-fault")}
		if _, err := store.CreateSlotSession(ctx, Slot{ID: "release-unbound-fault", WorkspaceID: "workspace", Generation: 1, RootID: testRootID, RelPath: "workspace/release-unbound-fault", State: "UNBOUND"}, nil, session, ""); err != nil {
			t.Fatal(err)
		}
		if _, err := store.db.Exec(`CREATE TRIGGER fail_release_unbound_job BEFORE INSERT ON jobs WHEN NEW.kind='REMOVE' BEGIN SELECT RAISE(ABORT,'fault'); END`); err != nil {
			t.Fatal(err)
		}
		if _, _, err := store.Release(ctx, session.ID, session.WorkspaceID, session.SlotID); err == nil {
			t.Fatal("release succeeded despite an unbound removal job insertion fault")
		}
	})

	t.Run("unbound session expiry fault", func(t *testing.T) {
		store := openTestStore(t)
		seedWorkspace(t, store)
		session := Session{ID: "release-unbound-session-fault", WorkspaceID: "workspace", SlotID: "release-unbound-session-fault", State: "UNBOUND", AgentKind: "codex", TokenHash: HashToken("release-unbound-session-fault")}
		if _, err := store.CreateSlotSession(ctx, Slot{ID: "release-unbound-session-fault", WorkspaceID: "workspace", Generation: 1, RootID: testRootID, RelPath: "workspace/release-unbound-session-fault", State: "UNBOUND"}, nil, session, ""); err != nil {
			t.Fatal(err)
		}
		if _, err := store.db.Exec(`CREATE TRIGGER fail_release_unbound_session BEFORE UPDATE OF state ON sessions WHEN OLD.id='release-unbound-session-fault' BEGIN SELECT RAISE(ABORT,'fault'); END`); err != nil {
			t.Fatal(err)
		}
		if _, _, err := store.Release(ctx, session.ID, session.WorkspaceID, session.SlotID); err == nil {
			t.Fatal("release succeeded despite an unbound session expiry fault")
		}
	})

	t.Run("unbound slot removal fault", func(t *testing.T) {
		store := openTestStore(t)
		seedWorkspace(t, store)
		session := Session{ID: "release-unbound-slot-fault", WorkspaceID: "workspace", SlotID: "release-unbound-slot-fault", State: "UNBOUND", AgentKind: "codex", TokenHash: HashToken("release-unbound-slot-fault")}
		if _, err := store.CreateSlotSession(ctx, Slot{ID: "release-unbound-slot-fault", WorkspaceID: "workspace", Generation: 1, RootID: testRootID, RelPath: "workspace/release-unbound-slot-fault", State: "UNBOUND"}, nil, session, ""); err != nil {
			t.Fatal(err)
		}
		if _, err := store.db.Exec(`CREATE TRIGGER fail_release_unbound_slot BEFORE UPDATE OF state ON slots WHEN OLD.id='release-unbound-slot-fault' AND NEW.state='REMOVING' BEGIN SELECT RAISE(ABORT,'fault'); END`); err != nil {
			t.Fatal(err)
		}
		if _, _, err := store.Release(ctx, session.ID, session.WorkspaceID, session.SlotID); err == nil {
			t.Fatal("release succeeded despite an unbound slot removal fault")
		}
	})
}

func TestLeaseReadyPropagatesTransactionFaults(t *testing.T) {
	ctx := context.Background()

	t.Run("repository lease timestamp fault", func(t *testing.T) {
		store := openTestStore(t)
		seedWorkspace(t, store)
		if _, err := store.CreateStandby(ctx, Slot{ID: "lease-ready-fault", WorkspaceID: "workspace", Generation: 1, RootID: testRootID, RelPath: "workspace/lease-ready-fault", State: "READY"}, nil); err != nil {
			t.Fatal(err)
		}
		if _, err := store.db.Exec(`CREATE TRIGGER fail_lease_ready_repo BEFORE UPDATE OF last_leased_at ON repositories WHEN OLD.id='repository' BEGIN SELECT RAISE(ABORT,'fault'); END`); err != nil {
			t.Fatal(err)
		}
		session := Session{ID: "lease-ready-fault", WorkspaceID: "workspace", State: "STARTING", AgentKind: "codex", TokenHash: HashToken("lease-ready-fault")}
		if err := store.LeaseReady(ctx, "lease-ready-fault", session); err == nil {
			t.Fatal("lease ready succeeded despite a repository lease timestamp fault")
		}
		if slot, err := store.Slot(ctx, "lease-ready-fault"); err != nil || slot.State != "READY" {
			t.Fatalf("rolled-back lease slot=%+v err=%v", slot, err)
		}
	})
}
