package state

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/HappyOnigiri/WX/internal/discovery"
)

func TestFailedStandbyBlocksUnboundedPoolRefill(t *testing.T) {
	store := openTestStore(t)
	seedWorkspace(t, store)
	ctx := context.Background()
	job, err := store.CreateStandby(ctx, Slot{ID: "blocked", WorkspaceID: "workspace", Generation: 1, RootID: testRootID, RelPath: "workspace/blocked", State: "PREPARING"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := store.StandbyCount(ctx, "workspace"); got != 1 {
		t.Fatalf("preparing standby count=%d", got)
	}
	if err := store.SetSlotState(ctx, "blocked", []string{"PREPARING"}, "FAILED", "PREPARE_FAILED"); err != nil {
		t.Fatal(err)
	}
	if got := store.StandbyCount(ctx, "workspace"); got != 1 {
		t.Fatalf("failed standby no longer blocks refill: count=%d", got)
	}
	claimed, err := store.ClaimJob(ctx, job.ID, "test")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.FinishJob(ctx, claimed.ID, "test", errors.New("prepare failed")); err != nil {
		t.Fatal(err)
	}
}

func TestStateMachineRejectsStaleAndIncompleteTransitions(t *testing.T) {
	store := openTestStore(t)
	seedWorkspace(t, store)
	ctx := context.Background()

	standby, err := store.CreateStandby(ctx, Slot{ID: "ready", WorkspaceID: "workspace", Generation: 1, RootID: testRootID, RelPath: "workspace/ready", State: "PREPARING"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetSlotState(ctx, "ready", []string{"PREPARING"}, "READY", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := store.LeaseReadyWithCold(ctx, "missing", Session{ID: "missing", WorkspaceID: "workspace", SlotID: "missing", AgentKind: "codex", TokenHash: HashToken("token")}); err == nil {
		t.Fatal("cold lease accepted a missing slot")
	}
	if _, err := store.LeaseReadyWithCold(ctx, "ready", Session{ID: "ready", WorkspaceID: "workspace", SlotID: "ready", AgentKind: "codex", TokenHash: HashToken("token")}); err == nil {
		t.Fatal("cold lease accepted a slot without COLD repositories")
	}
	claimed, err := store.ClaimJob(ctx, standby.ID, "test")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.FinishJob(ctx, claimed.ID, "test", nil); err != nil {
		t.Fatal(err)
	}

	active := Session{ID: "active", WorkspaceID: "workspace", SlotID: "active", State: "ACTIVE", AgentKind: "codex", TokenHash: HashToken("token")}
	if _, err := store.CreateSlotSession(ctx, Slot{ID: "active", WorkspaceID: "workspace", Generation: 1, RootID: testRootID, RelPath: "workspace/active", State: "LEASED"}, nil, active, ""); err != nil {
		t.Fatal(err)
	}
	archived := Session{ID: "archived", WorkspaceID: "workspace", SlotID: "archived", State: "ARCHIVED", AgentKind: "codex", TokenHash: HashToken("token")}
	if _, err := store.CreateSlotSession(ctx, Slot{ID: "archived", WorkspaceID: "workspace", Generation: 1, RootID: testRootID, RelPath: "workspace/archived", State: "SNAPSHOTTED"}, nil, archived, ""); err != nil {
		t.Fatal(err)
	}
	// 既定の一覧は実体が残る slot をすべて返すため、SNAPSHOTTED も貸出中・待機中と並ぶ。
	slots, err := store.ListSlots(ctx, false)
	if err != nil || len(slots) != 3 {
		t.Fatalf("live slot list=%+v err=%v", slots, err)
	}
	if slots[0].SlotID != "active" || slots[0].SessionID != active.ID || slots[1].SlotID != "archived" || slots[2].SlotID != "ready" || slots[2].SessionID != "" {
		t.Fatalf("live slots=%+v", slots)
	}
	// --all が足すのは slot を手放した session だけで、この時点ではまだ存在しない。
	if slots, err := store.ListSlots(ctx, true); err != nil || len(slots) != 3 {
		t.Fatalf("all slot list=%+v err=%v", slots, err)
	}
	if err := store.MarkSessionState(ctx, "active", []string{"ACTIVE"}, "EXPIRED"); err != nil {
		t.Fatal(err)
	}
	if err := store.Heartbeat(ctx, "active", "token"); err == nil {
		t.Fatal("expired session heartbeat succeeded")
	}
	if slots, err := store.ListSlots(ctx, false); err != nil || len(slots) != 3 || slots[0].SessionState != "EXPIRED" {
		t.Fatalf("expired session slot list=%+v err=%v", slots, err)
	}

	parent := Session{ID: "parent-unmapped", WorkspaceID: "workspace", SlotID: "parent-unmapped", State: "ARCHIVED", AgentKind: "codex", TokenHash: HashToken("parent")}
	if _, err := store.CreateSlotSession(ctx, Slot{ID: "parent-unmapped", WorkspaceID: "workspace", Generation: 1, RootID: testRootID, RelPath: "workspace/parent-unmapped", State: "SNAPSHOTTED"}, nil, parent, ""); err != nil {
		t.Fatal(err)
	}
	child := Session{ID: "child-unmapped", SlotID: "child-unmapped", State: "UNBOUND", AgentKind: "codex", TokenHash: HashToken("child")}
	if _, err := store.CreateSlotSession(ctx, Slot{ID: "child-unmapped", State: "UNBOUND", RootID: testRootID, RelPath: "_unbound/child-unmapped"}, nil, child, ""); err != nil {
		t.Fatal(err)
	}

	if err := store.MarkArchived(ctx, "active", "active", now()); err == nil {
		t.Fatal("invalid session archive transition succeeded")
	}
	if err := store.BeginSnapshot(ctx, "active", "active"); err == nil {
		t.Fatal("invalid snapshot transition succeeded")
	}
	if err := store.SetSlotRepositoryState(ctx, "ready", "missing", []string{"READY"}, "COLD"); err == nil {
		t.Fatal("missing slot repository transition succeeded")
	}
}

func TestTransactionalTransitionsRollBackOnCompanionStateChanges(t *testing.T) {
	store := openTestStore(t)
	seedWorkspace(t, store)
	ctx := context.Background()

	for _, operation := range []string{"archive", "snapshot"} {
		t.Run(operation, func(t *testing.T) {
			id := operation
			session := Session{ID: id, WorkspaceID: "workspace", SlotID: id, State: "ACTIVE", AgentKind: "codex", TokenHash: HashToken("token")}
			if _, err := store.CreateSlotSession(ctx, Slot{ID: id, WorkspaceID: "workspace", Generation: 1, RootID: testRootID, RelPath: filepath.Join("workspace", id), State: "LEASED"}, nil, session, ""); err != nil {
				t.Fatal(err)
			}
			if _, changed, err := store.Release(ctx, id, "workspace", id); err != nil || !changed {
				t.Fatalf("release changed=%v err=%v", changed, err)
			}
			if err := store.SetSlotState(ctx, id, []string{"DRAINING"}, "FAILED", "TEST"); err != nil {
				t.Fatal(err)
			}
			if operation == "archive" {
				if err := store.MarkArchived(ctx, id, id, now()); err == nil {
					t.Fatal("archive succeeded with a failed slot")
				}
			} else if err := store.BeginSnapshot(ctx, id, id); err == nil {
				t.Fatal("snapshot succeeded with a failed slot")
			}
			stored, err := store.SessionByID(ctx, id)
			if err != nil || stored.State != "RELEASING" {
				t.Fatalf("transaction did not roll back: session=%+v err=%v", stored, err)
			}
		})
	}

	parent := Session{ID: "parent", WorkspaceID: "workspace", SlotID: "parent", State: "ARCHIVED", AgentKind: "codex", TokenHash: HashToken("parent")}
	if _, err := store.CreateSlotSession(ctx, Slot{ID: "parent", WorkspaceID: "workspace", Generation: 1, RootID: testRootID, RelPath: "workspace/parent", State: "SNAPSHOTTED"}, nil, parent, ""); err != nil {
		t.Fatal(err)
	}
	if err := store.BindAgentSession(ctx, "parent", "agent"); err != nil {
		t.Fatal(err)
	}
	child := Session{ID: "child", SlotID: "child", State: "UNBOUND", AgentKind: "codex", TokenHash: HashToken("child")}
	if _, err := store.CreateSlotSession(ctx, Slot{ID: "child", State: "UNBOUND", RootID: testRootID, RelPath: "_unbound/child"}, nil, child, ""); err != nil {
		t.Fatal(err)
	}
	if err := store.SetSlotState(ctx, "child", []string{"UNBOUND"}, "QUARANTINED", "TEST"); err != nil {
		t.Fatal(err)
	}

	storedChild, err := store.SessionByID(ctx, "child")
	if err != nil || storedChild.State != "UNBOUND" {
		t.Fatalf("resume transaction did not roll back: child=%+v err=%v", storedChild, err)
	}
	if err := store.ExpireSessionSnapshots(ctx, "missing"); err == nil {
		t.Fatal("missing session snapshot expiration succeeded")
	}
}

func TestStoreFilesystemCreationFailures(t *testing.T) {
	root := t.TempDir()
	blocker := filepath.Join(root, "file")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(filepath.Join(blocker, "state.db")); err == nil {
		t.Fatal("database opened through a regular-file parent")
	}
	store := openTestStore(t)
	original := store.path
	store.path = filepath.Join(blocker, "state.db")
	if _, err := store.Backup(context.Background(), 1, time.Hour); err == nil {
		t.Fatal("backup created through a regular-file parent")
	}
	store.path = original
}

func TestCompanionTableFailuresRollBackStateTransactions(t *testing.T) {
	t.Run("create session repository metadata", func(t *testing.T) {
		store := openTestStore(t)
		seedWorkspace(t, store)
		if _, err := store.db.Exec(`DROP TABLE slot_repositories`); err != nil {
			t.Fatal(err)
		}
		session := Session{ID: "slot", WorkspaceID: "workspace", SlotID: "slot", State: "STARTING", AgentKind: "codex", TokenHash: HashToken("token")}
		_, err := store.CreateSlotSession(context.Background(), Slot{ID: "slot", WorkspaceID: "workspace", Generation: 1, RootID: testRootID, RelPath: "workspace/slot", State: "PREPARING"}, []SlotRepository{{RepositoryID: "repository", WorktreePath: "/wx/slot/repo"}}, session, "")
		if err == nil {
			t.Fatal("slot creation succeeded without repository metadata table")
		}
		var slots int
		if err := store.db.QueryRow(`SELECT count(*) FROM slots WHERE id='slot'`).Scan(&slots); err != nil || slots != 0 {
			t.Fatalf("slot transaction was not rolled back: slots=%d err=%v", slots, err)
		}
	})

	t.Run("create session row", func(t *testing.T) {
		store := openTestStore(t)
		seedWorkspace(t, store)
		if _, err := store.db.Exec(`DROP TABLE sessions`); err != nil {
			t.Fatal(err)
		}
		session := Session{ID: "slot", WorkspaceID: "workspace", SlotID: "slot", State: "STARTING", AgentKind: "codex", TokenHash: HashToken("token")}
		if _, err := store.CreateSlotSession(context.Background(), Slot{ID: "slot", WorkspaceID: "workspace", Generation: 1, RootID: testRootID, RelPath: "workspace/slot", State: "PREPARING"}, nil, session, ""); err == nil {
			t.Fatal("slot creation succeeded without sessions table")
		}
	})

	t.Run("standby durable job", func(t *testing.T) {
		store := openTestStore(t)
		seedWorkspace(t, store)
		if _, err := store.db.Exec(`DROP TABLE jobs`); err != nil {
			t.Fatal(err)
		}
		if _, err := store.CreateStandby(context.Background(), Slot{ID: "slot", WorkspaceID: "workspace", Generation: 1, RootID: testRootID, RelPath: "workspace/slot", State: "PREPARING"}, nil); err == nil {
			t.Fatal("standby creation succeeded without jobs table")
		}
	})

	t.Run("session durable job", func(t *testing.T) {
		store := openTestStore(t)
		seedWorkspace(t, store)
		if _, err := store.db.Exec(`DROP TABLE jobs`); err != nil {
			t.Fatal(err)
		}
		session := Session{ID: "slot", WorkspaceID: "workspace", SlotID: "slot", State: "STARTING", AgentKind: "codex", TokenHash: HashToken("token")}
		if _, err := store.CreateSlotSession(context.Background(), Slot{ID: "slot", WorkspaceID: "workspace", Generation: 1, RootID: testRootID, RelPath: "workspace/slot", State: "PREPARING"}, nil, session, "PREPARE"); err == nil {
			t.Fatal("slot creation succeeded without durable jobs table")
		}
	})

	t.Run("standby repository metadata", func(t *testing.T) {
		store := openTestStore(t)
		seedWorkspace(t, store)
		if _, err := store.db.Exec(`DROP TABLE slot_repositories`); err != nil {
			t.Fatal(err)
		}
		repository := SlotRepository{RepositoryID: "repository", DirName: "repository", State: "PREPARING"}
		if _, err := store.CreateStandby(context.Background(), Slot{ID: "slot", WorkspaceID: "workspace", Generation: 1, RootID: testRootID, RelPath: "workspace/slot", State: "PREPARING"}, []SlotRepository{repository}); err == nil {
			t.Fatal("standby creation succeeded without repository metadata table")
		}
	})

	t.Run("lease session row", func(t *testing.T) {
		store := openTestStore(t)
		seedWorkspace(t, store)
		ctx := context.Background()
		job, err := store.CreateStandby(ctx, Slot{ID: "slot", WorkspaceID: "workspace", Generation: 1, RootID: testRootID, RelPath: "workspace/slot", State: "PREPARING"}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.SetSlotState(ctx, "slot", []string{"PREPARING"}, "READY", ""); err != nil {
			t.Fatal(err)
		}
		claimed, err := store.ClaimJob(ctx, job.ID, "test")
		if err != nil {
			t.Fatal(err)
		}
		if err := store.FinishJob(ctx, claimed.ID, "test", nil); err != nil {
			t.Fatal(err)
		}
		if _, err := store.db.Exec(`DROP TABLE sessions`); err != nil {
			t.Fatal(err)
		}
		session := Session{ID: "slot", WorkspaceID: "workspace", SlotID: "slot", AgentKind: "codex", TokenHash: HashToken("token")}
		if err := store.LeaseReady(ctx, "slot", session); err == nil {
			t.Fatal("lease succeeded without sessions table")
		}
		slot, err := store.Slot(ctx, "slot")
		if err != nil || slot.State != "READY" {
			t.Fatalf("lease transaction did not roll back: slot=%+v err=%v", slot, err)
		}
	})

	t.Run("lease repository timestamp", func(t *testing.T) {
		store := openTestStore(t)
		seedWorkspace(t, store)
		ctx := context.Background()
		job, err := store.CreateStandby(ctx, Slot{ID: "slot", WorkspaceID: "workspace", Generation: 1, RootID: testRootID, RelPath: "workspace/slot", State: "PREPARING"}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.SetSlotState(ctx, "slot", []string{"PREPARING"}, "READY", ""); err != nil {
			t.Fatal(err)
		}
		claimed, _ := store.ClaimJob(ctx, job.ID, "test")
		if err := store.FinishJob(ctx, claimed.ID, "test", nil); err != nil {
			t.Fatal(err)
		}
		if _, err := store.db.Exec(`CREATE TRIGGER fail_repository_update BEFORE UPDATE ON repositories BEGIN SELECT RAISE(FAIL, 'injected repository update failure'); END`); err != nil {
			t.Fatal(err)
		}
		session := Session{ID: "slot", WorkspaceID: "workspace", SlotID: "slot", AgentKind: "codex", TokenHash: HashToken("token")}
		if err := store.LeaseReady(ctx, "slot", session); err == nil {
			t.Fatal("lease succeeded despite repository timestamp failure")
		}
	})

	t.Run("release durable job", func(t *testing.T) {
		store := openTestStore(t)
		seedWorkspace(t, store)
		ctx := context.Background()
		session := Session{ID: "slot", WorkspaceID: "workspace", SlotID: "slot", State: "ACTIVE", AgentKind: "codex", TokenHash: HashToken("token")}
		if _, err := store.CreateSlotSession(ctx, Slot{ID: "slot", WorkspaceID: "workspace", Generation: 1, RootID: testRootID, RelPath: "workspace/slot", State: "LEASED"}, nil, session, ""); err != nil {
			t.Fatal(err)
		}
		if _, err := store.db.Exec(`DROP TABLE jobs`); err != nil {
			t.Fatal(err)
		}
		if _, _, err := store.Release(ctx, "slot", "workspace", "slot"); err == nil {
			t.Fatal("release succeeded without durable jobs table")
		}
		stored, err := store.SessionByID(ctx, "slot")
		if err != nil || stored.State != "ACTIVE" {
			t.Fatalf("release transaction did not roll back: session=%+v err=%v", stored, err)
		}
	})

	for _, operation := range []string{"finish-preparation", "archive", "begin-snapshot"} {
		t.Run(operation, func(t *testing.T) {
			store := openTestStore(t)
			seedWorkspace(t, store)
			ctx := context.Background()
			sessionState, slotState := "STARTING", "PREPARING"
			if operation != "finish-preparation" {
				sessionState, slotState = "RELEASING", "DRAINING"
			}
			session := Session{ID: "slot", WorkspaceID: "workspace", SlotID: "slot", State: sessionState, AgentKind: "codex", TokenHash: HashToken("token")}
			if _, err := store.CreateSlotSession(ctx, Slot{ID: "slot", WorkspaceID: "workspace", Generation: 1, RootID: testRootID, RelPath: "workspace/slot", State: slotState}, nil, session, ""); err != nil {
				t.Fatal(err)
			}
			if operation == "finish-preparation" {
				if _, err := store.db.Exec(`DROP TABLE sessions`); err != nil {
					t.Fatal(err)
				}
				if _, _, err := store.FinishPreparationWithRelease(ctx, "slot"); err == nil {
					t.Fatal("preparation finish succeeded without sessions table")
				}
				return
			}
			if _, err := store.db.Exec(`CREATE TRIGGER fail_slot_update BEFORE UPDATE ON slots BEGIN SELECT RAISE(FAIL, 'injected slot update failure'); END`); err != nil {
				t.Fatal(err)
			}
			var err error
			if operation == "archive" {
				err = store.MarkArchived(ctx, "slot", "slot", now())
			} else {
				err = store.BeginSnapshot(ctx, "slot", "slot")
			}
			if err == nil {
				t.Fatalf("%s succeeded despite an injected slot update failure", operation)
			}
		})
	}

	t.Run("metadata pruning", func(t *testing.T) {
		store := openTestStore(t)
		if _, err := store.db.Exec(`DROP TABLE events`); err != nil {
			t.Fatal(err)
		}
		if err := store.PruneMetadata(context.Background(), now(), now(), now()); err == nil {
			t.Fatal("metadata pruning succeeded without events table")
		}
	})
}

func TestAdministrativeStateTransitionsAndQueries(t *testing.T) {
	store := openTestStore(t)
	seedWorkspace(t, store)
	ctx := context.Background()
	root := t.TempDir()
	job, err := store.CreateStandby(ctx, Slot{ID: "standby", WorkspaceID: "workspace", Generation: 1, RootID: testRootID, RelPath: "workspace/standby", State: "PREPARING"}, []SlotRepository{{RepositoryID: "repository", WorktreePath: filepath.Join(root, "standby", "repo"), State: "PREPARING", RequestedRef: "main", BaseOID: "abc", Fingerprint: "fp"}})
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := store.ClaimJob(ctx, job.ID, "owner")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.RenewJob(ctx, claimed.ID, "owner"); err != nil {
		t.Fatal(err)
	}
	if err := store.SetSlotRepositoryState(ctx, "standby", "repository", []string{"PREPARING"}, "READY"); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkReady(ctx, "standby"); err != nil {
		t.Fatal(err)
	}
	if store.StandbyCount(ctx, "workspace") == 0 {
		t.Fatal("ready standby was not counted")
	}
	if slots, err := store.ReadySlots(ctx, "workspace"); err != nil || len(slots) != 1 {
		t.Fatalf("ready slots=%+v err=%v", slots, err)
	}
	if err := store.FinishJob(ctx, claimed.ID, "owner", nil); err != nil {
		t.Fatal(err)
	}
	session := Session{ID: "standby", WorkspaceID: "workspace", SlotID: "standby", State: "ACTIVE", AgentKind: "codex", ClientPID: os.Getpid(), TokenHash: HashToken("token")}
	if err := store.LeaseReady(ctx, "standby", session); err != nil {
		t.Fatal(err)
	}
	if err := store.Heartbeat(ctx, "standby", "token"); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkSessionState(ctx, "standby", []string{"ACTIVE"}, "RELEASING"); err != nil {
		t.Fatal(err)
	}
	if slots, err := store.ListSlots(ctx, true); err != nil || len(slots) != 1 || slots[0].SessionID != "standby" {
		t.Fatalf("slots=%+v err=%v", slots, err)
	}
	if err := store.Ping(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestOwnershipAndCompareAndSwapFailuresAreRejected(t *testing.T) {
	store := openTestStore(t)
	seedWorkspace(t, store)
	ctx := context.Background()
	job, err := store.CreateJob(ctx, "PREPARE", "workspace", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ClaimJob(ctx, job.ID, "owner"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ClaimJob(ctx, job.ID, "owner"); err == nil {
		t.Fatal("running job was claimed twice")
	}
	if err := store.RenewJob(ctx, job.ID, "intruder"); err == nil {
		t.Fatal("job lease was renewed by another owner")
	}
	if err := store.RetryJob(ctx, job.ID, "intruder", time.Second, "retry"); err == nil {
		t.Fatal("job was retried by another owner")
	}
	if err := store.FinishJob(ctx, job.ID, "intruder", nil); err == nil {
		t.Fatal("job was finished by another owner")
	}
	if err := store.FinishJob(ctx, job.ID, "owner", errors.New("expected")); err != nil {
		t.Fatal(err)
	}
	session := Session{ID: "active", WorkspaceID: "workspace", SlotID: "active", State: "ACTIVE", AgentKind: "codex", TokenHash: HashToken("token")}
	if _, err := store.CreateSlotSession(ctx, Slot{ID: "active", WorkspaceID: "workspace", Generation: 1, RootID: testRootID, RelPath: "workspace/active", State: "LEASED"}, nil, session, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Session(ctx, "active", "wrong"); err == nil {
		t.Fatal("wrong session token succeeded")
	}
	if err := store.Heartbeat(ctx, "active", "wrong"); err == nil {
		t.Fatal("wrong heartbeat token succeeded")
	}
	if err := store.BindAgentSession(ctx, "active", "agent-one"); err != nil {
		t.Fatal(err)
	}
	if err := store.BindAgentSession(ctx, "active", "agent-two"); err == nil {
		t.Fatal("agent session was rebound ambiguously")
	}
	if err := store.SetSlotState(ctx, "active", []string{"READY"}, "STALE", "test"); err == nil {
		t.Fatal("slot state CAS mismatch succeeded")
	}
	if _, _, err := store.FinishPreparationWithRelease(ctx, "active"); err == nil {
		t.Fatal("leased slot finished preparation")
	}
	if err := store.MarkSessionState(ctx, "active", []string{"STARTING"}, "ACTIVE"); err == nil {
		t.Fatal("session state CAS mismatch succeeded")
	}
	if err := store.ForgetWorkspace(ctx, "/workspace"); err == nil {
		t.Fatal("active workspace was forgotten")
	}
	if _, changed, err := store.ScheduleRemoval(ctx, "active", "active"); err != nil || changed {
		t.Fatalf("active removal changed=%v err=%v", changed, err)
	}
	if err := store.AddRestoringRepositories(ctx, "active", nil); err == nil {
		t.Fatal("repositories were added to non-restoring slot")
	}
}

func TestArchiveAndForgetAdministrativePaths(t *testing.T) {
	store := openTestStore(t)
	seedWorkspace(t, store)
	ctx := context.Background()
	root := t.TempDir()
	job, err := store.CreateStandby(ctx, Slot{ID: "slot", WorkspaceID: "workspace", Generation: 1, RootID: testRootID, RelPath: "workspace/slot", State: "READY"}, []SlotRepository{{RepositoryID: "repository", WorktreePath: filepath.Join(root, "slot", "repo"), State: "READY", RequestedRef: "main", BaseOID: "abc", Fingerprint: "fp"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetSlotState(ctx, "slot", []string{"READY", "STALE"}, "ARCHIVED", ""); err != nil {
		t.Fatal(err)
	}
	if err := store.SetSlotState(ctx, "slot", []string{"ARCHIVED"}, "SNAPSHOTTED", ""); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkSlotArchived(ctx, "slot"); err != nil {
		t.Fatal(err)
	}
	claimed, err := store.ClaimJob(ctx, job.ID, "owner")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.FinishJob(ctx, claimed.ID, "owner", nil); err != nil {
		t.Fatal(err)
	}
	if err := store.ForgetWorkspace(ctx, "/workspace"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Workspace(ctx, "workspace"); err == nil {
		t.Fatal("forgotten workspace still exists")
	}
}

func TestOnlineBackupContainsCommittedRegistry(t *testing.T) {
	store := openTestStore(t)
	seedWorkspace(t, store)
	path, err := store.Backup(context.Background(), 2, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	info, err := filepath.Glob(filepath.Join(filepath.Dir(path), "*.db"))
	if err != nil || len(info) != 1 {
		t.Fatalf("backup files=%v err=%v", info, err)
	}
	backup, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer backup.Close()
	workspace, err := backup.Workspace(context.Background(), "workspace")
	if err != nil || len(workspace.Repositories) != 1 {
		t.Fatalf("backup workspace=%+v err=%v", workspace, err)
	}
}

func TestWALAllowsStatusReadWhileWriteTransactionIsOpen(t *testing.T) {
	store := openTestStore(t)
	seedWorkspace(t, store)
	ctx := context.Background()
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `UPDATE workspaces SET last_seen_at=? WHERE id='workspace'`, now()); err != nil {
		t.Fatal(err)
	}
	readDone := make(chan error, 1)
	go func() {
		_, err := store.Status(ctx)
		readDone <- err
	}()
	select {
	case err := <-readDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("WAL status read blocked behind writer")
	}
}

func TestEverySQLiteConnectionEnforcesPolicyPragmas(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	connections := make([]interface{ Close() error }, 0, 3)
	for range 3 {
		conn, err := store.db.Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		connections = append(connections, conn)
		var foreignKeys, busyTimeout int
		var journalMode string
		if err := conn.QueryRowContext(ctx, `PRAGMA foreign_keys`).Scan(&foreignKeys); err != nil {
			t.Fatal(err)
		}
		if err := conn.QueryRowContext(ctx, `PRAGMA busy_timeout`).Scan(&busyTimeout); err != nil {
			t.Fatal(err)
		}
		if err := conn.QueryRowContext(ctx, `PRAGMA journal_mode`).Scan(&journalMode); err != nil {
			t.Fatal(err)
		}
		if foreignKeys != 1 || busyTimeout != 5000 || journalMode != "wal" {
			t.Fatalf("pragmas foreign_keys=%d busy_timeout=%d journal_mode=%s", foreignKeys, busyTimeout, journalMode)
		}
	}
	for _, conn := range connections {
		_ = conn.Close()
	}
}

func TestCorruptDatabaseIsNotReplacedOrClaimed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	original := []byte("not a sqlite database; preserve for recovery")
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	if store, err := Open(path); err == nil {
		_ = store.Close()
		t.Fatal("corrupt database unexpectedly opened")
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, original) {
		t.Fatalf("corrupt database was modified: %q", after)
	}
}

func TestDamagedSchemaFailsEveryOperationWithoutRecreatingState(t *testing.T) {
	store := openTestStore(t)
	for _, table := range []string{"rpc_idempotency", "quarantined_artifacts", "workspace_snapshots", "snapshots", "jobs", "session_repositories", "sessions", "slot_repositories", "slots", "workspace_repositories", "repositories", "workspaces", "events"} {
		if _, err := store.db.Exec(`DROP TABLE ` + table); err != nil {
			t.Fatalf("drop %s: %v", table, err)
		}
	}
	ctx := context.Background()
	w := discovery.Workspace{ID: "w", Root: "/w", Kind: "repository"}
	slot := Slot{ID: "s", WorkspaceID: "w", State: "PREPARING", RootID: testRootID, RelPath: "w/s"}
	repository := SlotRepository{RepositoryID: "r", DirName: "r", State: "PREPARING"}
	session := Session{ID: "s", WorkspaceID: "w", SlotID: "s", State: "STARTING", AgentKind: "codex", TokenHash: HashToken("token")}
	snapshot := Snapshot{ID: "snapshot", SessionID: "s", RepositoryID: "r"}

	if _, _, err := store.UpsertWorkspaceGeneration(ctx, w); err == nil {
		t.Error("upsert succeeded against damaged schema")
	}
	if _, err := store.WorkspaceGeneration(ctx, "w"); err == nil {
		t.Error("generation lookup succeeded against damaged schema")
	}
	if _, err := store.CreateJob(ctx, "PREPARE", "w", "s", "s"); err == nil {
		t.Error("job creation succeeded against damaged schema")
	}
	if _, err := store.ClaimJob(ctx, "j", "owner"); err == nil {
		t.Error("job claim succeeded against damaged schema")
	}
	if err := store.RenewJob(ctx, "j", "owner"); err == nil {
		t.Error("job renewal succeeded against damaged schema")
	}
	if err := store.FinishJob(ctx, "j", "owner", nil); err == nil {
		t.Error("job finish succeeded against damaged schema")
	}
	if err := store.RetryJob(ctx, "j", "owner", time.Second, "retry"); err == nil {
		t.Error("job retry succeeded against damaged schema")
	}
	if _, err := store.RecoverJobs(ctx, false); err == nil {
		t.Error("job recovery succeeded against damaged schema")
	}
	if _, _, err := store.ReadySlot(ctx, "w"); err == nil {
		t.Error("ready lookup succeeded against damaged schema")
	}
	if _, err := store.ReadySlots(ctx, "w"); err == nil {
		t.Error("ready list succeeded against damaged schema")
	}
	if _, err := store.CreateSlotSession(ctx, slot, []SlotRepository{repository}, session, "PREPARE"); err == nil {
		t.Error("slot creation succeeded against damaged schema")
	}
	if _, err := store.CreateStandby(ctx, slot, []SlotRepository{repository}); err == nil {
		t.Error("standby creation succeeded against damaged schema")
	}
	for name, operation := range map[string]func() error{
		"lease":              func() error { return store.LeaseReady(ctx, "s", session) },
		"set slot":           func() error { return store.SetSlotState(ctx, "s", []string{"READY"}, "STALE", "test") },
		"finish preparation": func() error { _, _, err := store.FinishPreparationWithRelease(ctx, "s"); return err },
		"session state":      func() error { return store.MarkSessionState(ctx, "s", []string{"ACTIVE"}, "RELEASING") },
		"bind agent":         func() error { return store.BindAgentSession(ctx, "s", "agent") },

		"heartbeat":         func() error { return store.Heartbeat(ctx, "s", "token") },
		"add restore repos": func() error { return store.AddRestoringRepositories(ctx, "s", []SlotRepository{repository}) },
		"set repository":    func() error { return store.SetSlotRepositoryState(ctx, "s", "r", []string{"READY"}, "COLD") },
		"save snapshot":     func() error { return store.SaveSnapshot(ctx, snapshot) },
		"forget":            func() error { return store.ForgetWorkspace(ctx, "/w") },
		"mark archived":     func() error { return store.MarkArchived(ctx, "s", "s", now()) },
		"begin snapshot":    func() error { return store.BeginSnapshot(ctx, "s", "s") },
		"finish cold":       func() error { return store.FinishColdRepositoryRemoval(ctx, "s", "r") },
		"finish removal":    func() error { _, err := store.FinishRemoval(ctx, "s"); return err },
		"prune roots":       func() error { return store.PruneRoots(ctx) },
		"expire snapshots":  func() error { return store.ExpireSessionSnapshots(ctx, "s") },
		"prune":             func() error { return store.PruneMetadata(ctx, now(), now(), now()) },
	} {
		if err := operation(); err == nil {
			t.Errorf("%s succeeded against damaged schema", name)
		}
	}
	for name, operation := range map[string]func() error{
		"session":           func() error { _, err := store.Session(ctx, "s", "token"); return err },
		"session by id":     func() error { _, err := store.SessionByID(ctx, "s"); return err },
		"find agent":        func() error { _, err := store.FindByAgentSession(ctx, "codex", "agent"); return err },
		"orphans":           func() error { _, err := store.OrphanCandidates(ctx, now()); return err },
		"slot repos":        func() error { _, err := store.SlotRepositories(ctx, "s"); return err },
		"slot repo":         func() error { _, err := store.SlotRepository(ctx, "s", "r"); return err },
		"slot":              func() error { _, err := store.Slot(ctx, "s"); return err },
		"snapshots":         func() error { _, err := store.Snapshots(ctx, "s"); return err },
		"repository":        func() error { _, err := store.Repository(ctx, "r"); return err },
		"workspace":         func() error { _, err := store.Workspace(ctx, "w"); return err },
		"workspace roots":   func() error { _, err := store.WorkspaceRoots(ctx); return err },
		"status":            func() error { _, err := store.Status(ctx); return err },
		"slots":             func() error { _, err := store.ListSlots(ctx, true); return err },
		"standby gc":        func() error { _, err := store.StandbyGCCandidates(ctx, now(), 1); return err },
		"cold candidates":   func() error { _, err := store.ColdRepositoryCandidates(ctx, now()); return err },
		"expired snapshots": func() error { _, err := store.ExpiredSnapshots(ctx, now()); return err },
		"gc candidates":     func() error { _, err := store.GCCandidates(ctx, now()); return err },
	} {
		if err := operation(); err == nil {
			t.Errorf("%s succeeded against damaged schema", name)
		}
	}
	if _, err := store.LeaseReadyWithCold(ctx, "s", session); err == nil {
		t.Error("cold lease succeeded against damaged schema")
	}

	if _, _, err := store.Release(ctx, "s", "w", "s"); err == nil {
		t.Error("release succeeded against damaged schema")
	}
	if _, _, err := store.ScheduleColdRepositoryRemoval(ctx, ColdRepositoryCandidate{SlotID: "s", WorkspaceID: "w", RepositoryID: "r"}); err == nil {
		t.Error("cold removal scheduling succeeded against damaged schema")
	}
	if _, _, err := store.ScheduleRemoval(ctx, "s", "s"); err == nil {
		t.Error("removal scheduling succeeded against damaged schema")
	}
}

func TestResumeOrphanAndBackupNontrivialPaths(t *testing.T) {
	store := openTestStore(t)
	seedWorkspace(t, store)
	ctx := context.Background()
	root := t.TempDir()

	parent := Session{ID: "parent", WorkspaceID: "workspace", SlotID: "parent", State: "ACTIVE", AgentKind: "codex", AgentSessionID: "agent", TokenHash: HashToken("parent-token")}
	if _, err := store.CreateSlotSession(ctx, Slot{ID: "parent", WorkspaceID: "workspace", Generation: 1, RootID: testRootID, RelPath: "workspace/parent", State: "LEASED"}, nil, parent, ""); err != nil {
		t.Fatal(err)
	}
	if err := store.BindAgentSession(ctx, "parent", "agent"); err != nil {
		t.Fatal(err)
	}
	unbound := Session{ID: "child", SlotID: "child", State: "UNBOUND", AgentKind: "codex", TokenHash: HashToken("child-token")}
	if _, err := store.CreateSlotSession(ctx, Slot{ID: "child", State: "UNBOUND", RootID: testRootID, RelPath: "_unbound/child"}, nil, unbound, ""); err != nil {
		t.Fatal(err)
	}
	repositories := []SlotRepository{{RepositoryID: "repository", WorktreePath: filepath.Join(root, "child", "repo"), RequestedRef: "main", BaseOID: "abc", Fingerprint: "fp"}}

	job, err := recreateRestoreFixture(store, ctx, "child", "parent", "workspace", "agent", 1, repositories)
	if err != nil || job.Kind != "RESTORE" || job.RepositoryID != "" {
		t.Fatalf("resume job=%+v err=%v", job, err)
	}
	child, err := store.SessionByID(ctx, "child")
	if err != nil || child.State != "RESTORING" || child.ParentSessionID != "parent" {
		t.Fatalf("bound child=%+v err=%v", child, err)
	}
	mapped, err := store.FindByAgentSession(ctx, "codex", "agent")
	if err != nil || mapped.ID != "parent" {
		t.Fatalf("agent mapping moved before restore: mapped=%+v err=%v", mapped, err)
	}
	if err := store.AddRestoringRepositories(ctx, "child", repositories); err != nil {
		t.Fatalf("idempotent restore metadata: %v", err)
	}
	if err := store.AddRestoringRepositories(ctx, "child", append(repositories, SlotRepository{})); err == nil {
		t.Fatal("incomplete restore metadata was accepted")
	}

	if _, _, err := store.FinishPreparationWithRelease(ctx, "child"); err != nil {
		t.Fatalf("finish restore: %v", err)
	}
	mapped, err = store.FindByAgentSession(ctx, "codex", "agent")
	if err != nil || mapped.ID != "child" {
		t.Fatalf("agent mapping not moved after restore: mapped=%+v err=%v", mapped, err)
	}

	orphan := Session{ID: "orphan", WorkspaceID: "workspace", SlotID: "orphan", State: "STARTING", AgentKind: "codex", ClientPID: 999999, TokenHash: HashToken("orphan-token")}
	if _, err := store.CreateSlotSession(ctx, Slot{ID: "orphan", WorkspaceID: "workspace", Generation: 1, RootID: testRootID, RelPath: "workspace/orphan", State: "LEASED"}, nil, orphan, ""); err != nil {
		t.Fatal(err)
	}
	candidates, err := store.OrphanCandidates(ctx, time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano))
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, candidate := range candidates {
		found = found || candidate.ID == "orphan" && candidate.ClientPID == 999999
	}
	if !found {
		t.Fatalf("orphan candidates=%+v", candidates)
	}
	artifacts, err := store.SlotArtifacts(ctx)
	if err != nil || len(artifacts) != 3 {
		t.Fatalf("slot artifacts=%+v err=%v", artifacts, err)
	}
	registeredRepositories, err := store.Repositories(ctx)
	if err != nil || len(registeredRepositories) != 1 || registeredRepositories[0].ID != "repository" {
		t.Fatalf("repositories=%+v err=%v", registeredRepositories, err)
	}
	backupDir := store.path + ".backups"
	if err := os.MkdirAll(filepath.Join(backupDir, "keep-directory"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(backupDir, "ignore.txt"), []byte("not a backup"), 0o600); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if _, err := store.Backup(ctx, 1, 0); err != nil {
			t.Fatal(err)
		}
		time.Sleep(time.Millisecond)
	}
	backups, err := filepath.Glob(filepath.Join(backupDir, "*.db"))
	if err != nil || len(backups) != 1 {
		t.Fatalf("retained backups=%v err=%v", backups, err)
	}
}

func TestRestoreActivationPreservesParentMappingAtEveryHandoffFailure(t *testing.T) {
	tests := []struct {
		name    string
		trigger string
	}{
		{name: "parent mapping clear", trigger: `CREATE TRIGGER fail_parent_mapping_clear BEFORE UPDATE OF agent_session_id ON sessions WHEN OLD.id='parent' BEGIN SELECT RAISE(ABORT,'fault'); END`},
		{name: "child mapping set", trigger: `CREATE TRIGGER fail_child_mapping_set BEFORE UPDATE OF agent_session_id ON sessions WHEN OLD.id='child' BEGIN SELECT RAISE(ABORT,'fault'); END`},
		{name: "activation", trigger: `CREATE TRIGGER fail_restore_activation BEFORE UPDATE OF state ON sessions WHEN NEW.state='ACTIVE' BEGIN SELECT RAISE(ABORT,'fault'); END`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := openTestStore(t)
			seedWorkspace(t, store)
			ctx := context.Background()
			parent := Session{ID: "parent", WorkspaceID: "workspace", SlotID: "parent", State: "ARCHIVED", AgentKind: "codex", AgentSessionID: "agent", TokenHash: HashToken("parent")}
			if _, err := store.CreateSlotSession(ctx, Slot{ID: "parent", WorkspaceID: "workspace", Generation: 1, RootID: testRootID, RelPath: "workspace/parent", State: "SNAPSHOTTED"}, nil, parent, ""); err != nil {
				t.Fatal(err)
			}
			if err := store.BindAgentSession(ctx, "parent", "agent"); err != nil {
				t.Fatal(err)
			}
			child := Session{ID: "child", SlotID: "child", State: "UNBOUND", AgentKind: "codex", TokenHash: HashToken("child")}
			if _, err := store.CreateSlotSession(ctx, Slot{ID: "child", State: "UNBOUND", RootID: testRootID, RelPath: "_unbound/child"}, nil, child, ""); err != nil {
				t.Fatal(err)
			}
			if _, err := recreateRestoreFixture(store, ctx, "child", "parent", "workspace", "agent", 1, nil); err != nil {
				t.Fatal(err)
			}
			if _, err := store.db.ExecContext(ctx, test.trigger); err != nil {
				t.Fatal(err)
			}
			if _, _, err := store.FinishPreparationWithRelease(ctx, "child"); err == nil {
				t.Fatal("fault-injected restore activation succeeded")
			}
			mapped, err := store.FindByAgentSession(ctx, "codex", "agent")
			if err != nil || mapped.ID != "parent" {
				t.Fatalf("restore handoff lost parent mapping: mapped=%+v err=%v", mapped, err)
			}
			storedChild, err := store.SessionByID(ctx, "child")
			if err != nil || storedChild.State != "RESTORING" || storedChild.AgentSessionID != "" {
				t.Fatalf("restore handoff partially activated child: child=%+v err=%v", storedChild, err)
			}
		})
	}
}

// testRootID と testRootPath は openTestStore が登録する worktree root generation である。
// slot location は disk 上の root pathname ではなく root_id と記録済み inode identity で証明するため、固定 lexical path により test 間の Slot.Path 組立てを決定的にする。
const (
	testRootID   = "root01"
	testRootPath = "/wx"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	seedRoot(t, store, testRootID, testRootPath, "root-identity", true)
	return store
}

// seedRoot は一 worktree root generation を直接登録する。EnsureActiveRoot は random ID を引くため、expected value で generation を指す test は関数を呼ばず row を insert する。
func seedRoot(t *testing.T, store *Store, id, path, identity string, active bool) {
	t.Helper()
	activeFlag := 0
	if active {
		activeFlag = 1
	}
	if _, err := store.db.ExecContext(context.Background(), `INSERT INTO roots(id,path,identity,active,created_at) VALUES(?,?,?,?,?)`, id, path, identity, activeFlag, now()); err != nil {
		t.Fatal(err)
	}
}

// seedWorkspace は固定 ID の shared single-repository workspace fixture を直接登録する。UpsertWorkspaceGeneration は未観測 workspace の random proposal を意図的に再抽選するため、
// stable な ID を必要とする fixture は row を自ら insert する。後続の UpsertWorkspaceGeneration は repository の common Git directory でこの row に解決する。
// commentlint:allow-long -- fixture の固定 ID と通常の再抽選を使い分ける理由を説明する
func seedWorkspace(t *testing.T, store *Store) {
	t.Helper()
	// relative_path は意図的に空にする。置換前 fixture と同じく、single-repository workspace では workspace root と repository main path が同じ directory である。
	seedWorkspaceRows(t, store, "workspace", "/workspace", "repository", "repository", "/workspace", "/workspace/.git", "")
}

func seedWorkspaceRows(t *testing.T, store *Store, workspaceID, root, kind, repositoryID, mainPath, commonDir, relative string) {
	t.Helper()
	ctx := context.Background()
	if _, err := store.db.ExecContext(ctx, `INSERT INTO workspaces(id,root_path,kind,generation,discovery_state,first_seen_at,last_seen_at,last_reconciled_at) VALUES(?,?,?,1,'READY',?,?,?)`, workspaceID, root, kind, now(), now(), now()); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `INSERT INTO repositories(id,main_worktree_path,common_git_dir,default_branch,remote_name,first_seen_at,last_seen_at) VALUES(?,?,?,'main','',?,?)`, repositoryID, mainPath, commonDir, now(), now()); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `INSERT INTO workspace_repositories(workspace_id,repository_id,relative_path,ordinal) VALUES(?,?,?,0)`, workspaceID, repositoryID, relative); err != nil {
		t.Fatal(err)
	}
}
