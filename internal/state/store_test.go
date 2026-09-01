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

func TestCreateSlotSessionCommitsJobAtomically(t *testing.T) {
	store := openTestStore(t)
	seedWorkspace(t, store)
	ctx := context.Background()
	session := Session{ID: "session", WorkspaceID: "workspace", SlotID: "slot", State: "STARTING", AgentKind: "codex", TokenHash: HashToken("token")}
	job, err := store.CreateSlotSession(ctx, Slot{ID: "slot", WorkspaceID: "workspace", Generation: 1, Path: filepath.Join(t.TempDir(), "slot"), State: "PREPARING"}, []SlotRepository{{RepositoryID: "repository", WorktreePath: filepath.Join(t.TempDir(), "worktree"), State: "PREPARING", RequestedRef: "main", BaseOID: "abc", Fingerprint: "fingerprint"}}, session, "PREPARE")
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

func TestReleaseCreatesExactlyOneSnapshotJob(t *testing.T) {
	store := openTestStore(t)
	seedWorkspace(t, store)
	ctx := context.Background()
	session := Session{ID: "session", WorkspaceID: "workspace", SlotID: "slot", State: "ACTIVE", AgentKind: "codex", TokenHash: HashToken("token")}
	if _, err := store.CreateSlotSession(ctx, Slot{ID: "slot", WorkspaceID: "workspace", Generation: 1, Path: filepath.Join(t.TempDir(), "slot"), State: "LEASED"}, nil, session, ""); err != nil {
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

func TestRecoverJobsReclaimsOnlyExpiredLease(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	job, err := store.CreateJob(ctx, "ENSURE_STANDBY", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ClaimJob(ctx, job.ID, "owner"); err != nil {
		t.Fatal(err)
	}
	jobs, err := store.RecoverJobs(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 0 {
		t.Fatalf("live lease was reclaimed: %+v", jobs)
	}
	if _, err := store.db.Exec(`UPDATE jobs SET lease_expires_at=? WHERE id=?`, time.Now().Add(-time.Minute).UTC().Format(time.RFC3339Nano), job.ID); err != nil {
		t.Fatal(err)
	}
	jobs, err = store.RecoverJobs(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 || jobs[0].ID != job.ID || jobs[0].State != "PENDING" {
		t.Fatalf("expired lease recovery=%+v", jobs)
	}
}

func TestAdministrativeStateTransitionsAndQueries(t *testing.T) {
	store := openTestStore(t)
	seedWorkspace(t, store)
	ctx := context.Background()
	root := t.TempDir()
	job, err := store.CreateStandby(ctx, Slot{ID: "standby", WorkspaceID: "workspace", Generation: 1, Path: filepath.Join(root, "standby"), State: "PREPARING"}, []SlotRepository{{RepositoryID: "repository", WorktreePath: filepath.Join(root, "standby", "repo"), State: "PREPARING", RequestedRef: "main", BaseOID: "abc", Fingerprint: "fp"}})
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
	if !store.HasStandby(ctx, "workspace") {
		t.Fatal("ready standby was not counted")
	}
	if slots, err := store.ReadySlots(ctx, "workspace"); err != nil || len(slots) != 1 {
		t.Fatalf("ready slots=%+v err=%v", slots, err)
	}
	if err := store.FinishJob(ctx, claimed.ID, "owner", nil); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordLease(ctx, "workspace"); err != nil {
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
	if sessions, err := store.ListSessions(ctx, true); err != nil || len(sessions) != 1 || sessions[0].ID != "standby" {
		t.Fatalf("sessions=%+v err=%v", sessions, err)
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
	root := t.TempDir()
	session := Session{ID: "active", WorkspaceID: "workspace", SlotID: "active", State: "ACTIVE", AgentKind: "codex", TokenHash: HashToken("token")}
	if _, err := store.CreateSlotSession(ctx, Slot{ID: "active", WorkspaceID: "workspace", Generation: 1, Path: filepath.Join(root, "active"), State: "LEASED"}, nil, session, ""); err != nil {
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
	if err := store.FinishPreparation(ctx, "active"); err == nil {
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

func TestDrainArchiveAndForgetAdministrativePaths(t *testing.T) {
	store := openTestStore(t)
	seedWorkspace(t, store)
	ctx := context.Background()
	root := t.TempDir()
	job, err := store.CreateStandby(ctx, Slot{ID: "slot", WorkspaceID: "workspace", Generation: 1, Path: filepath.Join(root, "slot"), State: "READY"}, []SlotRepository{{RepositoryID: "repository", WorktreePath: filepath.Join(root, "slot", "repo"), State: "READY", RequestedRef: "main", BaseOID: "abc", Fingerprint: "fp"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.DrainRoot(ctx, root); err != nil {
		t.Fatal(err)
	}
	if slot, err := store.Slot(ctx, "slot"); err != nil || slot.State != "STALE" {
		t.Fatalf("drained slot=%+v err=%v", slot, err)
	}
	if err := store.MarkStandbyArchived(ctx, "slot"); err != nil {
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

func TestSaveSnapshotIsIdempotentButRejectsConflict(t *testing.T) {
	store := openTestStore(t)
	seedWorkspace(t, store)
	ctx := context.Background()
	session := Session{ID: "session", WorkspaceID: "workspace", SlotID: "slot", State: "ACTIVE", AgentKind: "codex", TokenHash: HashToken("token")}
	if _, err := store.CreateSlotSession(ctx, Slot{ID: "slot", WorkspaceID: "workspace", Generation: 1, Path: filepath.Join(t.TempDir(), "slot"), State: "LEASED"}, nil, session, ""); err != nil {
		t.Fatal(err)
	}
	snapshot := Snapshot{ID: "snapshot", SessionID: "session", RepositoryID: "repository", HeadOID: "head", HeadRef: "refs/wx/head", IndexTreeOID: "index", WorktreeOID: "worktree", WorktreeRef: "refs/wx/worktree", Status: "ARCHIVED", CreatedAt: now(), ExpiresAt: time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano)}
	if err := store.SaveSnapshot(ctx, snapshot); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveSnapshot(ctx, snapshot); err != nil {
		t.Fatalf("idempotent save: %v", err)
	}
	snapshot.WorktreeOID = "different"
	if err := store.SaveSnapshot(ctx, snapshot); err == nil {
		t.Fatal("conflicting snapshot was accepted")
	}
}

func TestWorkspaceMembershipChangeAdvancesGenerationAndStalesOldStandby(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	w := discovery.Workspace{ID: "workspace", Root: "/workspace", Kind: "multi_repository", Repositories: []discovery.Repository{{ID: "repository", MainPath: "/workspace/repository", CommonDir: "/workspace/repository/.git", RelativePath: "repository", DefaultBranch: "main"}}}
	generation, err := store.UpsertWorkspaceGeneration(ctx, w)
	if err != nil || generation != 1 {
		t.Fatalf("initial generation=%d err=%v", generation, err)
	}
	job, err := store.CreateStandby(ctx, Slot{ID: "slot", WorkspaceID: "workspace", Generation: generation, Path: filepath.Join(t.TempDir(), "slot"), State: "READY"}, []SlotRepository{{RepositoryID: "repository", WorktreePath: filepath.Join(t.TempDir(), "worktree"), State: "READY", RequestedRef: "main", BaseOID: "abc", Fingerprint: "fingerprint"}})
	if err != nil || job.Kind != "PREPARE" {
		t.Fatalf("create standby job=%+v err=%v", job, err)
	}
	if generation, err = store.UpsertWorkspaceGeneration(ctx, w); err != nil || generation != 1 {
		t.Fatalf("unchanged generation=%d err=%v", generation, err)
	}
	w.Repositories = append(w.Repositories, discovery.Repository{ID: "repository-2", MainPath: "/workspace/repository-2", CommonDir: "/workspace/repository-2/.git", RelativePath: "repository-2", DefaultBranch: "main"})
	if generation, err = store.UpsertWorkspaceGeneration(ctx, w); err != nil || generation != 2 {
		t.Fatalf("changed generation=%d err=%v", generation, err)
	}
	slot, err := store.Slot(ctx, "slot")
	if err != nil || slot.State != "STALE" {
		t.Fatalf("old slot=%+v err=%v", slot, err)
	}
	loaded, err := store.Workspace(ctx, "workspace")
	if err != nil || len(loaded.Repositories) != 2 {
		t.Fatalf("workspace repositories=%d err=%v", len(loaded.Repositories), err)
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

func TestClosedStoreFailsDatabaseOperationsWithoutPanicking(t *testing.T) {
	store := openTestStore(t)
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	w := discovery.Workspace{ID: "w", Root: "/w", Kind: "repository"}
	slot := Slot{ID: "s", WorkspaceID: "w", Path: "/wx/s", State: "PREPARING"}
	repository := SlotRepository{RepositoryID: "r", WorktreePath: "/wx/s/r", State: "PREPARING"}
	session := Session{ID: "s", WorkspaceID: "w", SlotID: "s", State: "STARTING", AgentKind: "codex", TokenHash: HashToken("token")}
	snapshot := Snapshot{ID: "snapshot", SessionID: "s", RepositoryID: "r"}
	_ = store.Ping(ctx)
	_, _ = store.Backup(ctx, 1, time.Hour)
	_ = store.UpsertWorkspace(ctx, w)
	_, _ = store.UpsertWorkspaceGeneration(ctx, w)
	_, _ = store.WorkspaceGeneration(ctx, "w")
	_, _ = store.CreateJob(ctx, "PREPARE", "w", "s", "s")
	_, _ = store.ClaimJob(ctx, "j", "owner")
	_ = store.RenewJob(ctx, "j", "owner")
	_ = store.FinishJob(ctx, "j", "owner", nil)
	_ = store.RetryJob(ctx, "j", "owner", time.Second, "retry")
	_, _ = store.RecoverJobs(ctx, false)
	_, _, _ = store.ReadySlot(ctx, "w")
	_, _ = store.ReadySlots(ctx, "w")
	_ = store.HasStandby(ctx, "w")
	_, _ = store.CreateSlotSession(ctx, slot, []SlotRepository{repository}, session, "PREPARE")
	_, _ = store.CreateStandby(ctx, slot, []SlotRepository{repository})
	_ = store.LeaseReady(ctx, "s", session)
	_, _ = store.LeaseReadyWithCold(ctx, "s", session)
	_ = store.RecordLease(ctx, "w")
	_ = store.SetSlotState(ctx, "s", []string{"READY"}, "STALE", "test")
	_ = store.MarkReady(ctx, "s")
	_ = store.FinishPreparation(ctx, "s")
	_ = store.MarkSessionState(ctx, "s", []string{"ACTIVE"}, "RELEASING")
	_, _ = store.Session(ctx, "s", "token")
	_, _ = store.SessionByID(ctx, "s")
	_ = store.BindAgentSession(ctx, "s", "agent")
	_ = store.BindFreshSession(ctx, "s", "token", "agent")
	_ = store.Heartbeat(ctx, "s", "token")
	_, _ = store.OrphanCandidates(ctx, now())
	_, _ = store.BindResumeSlot(ctx, "s", "parent", "w", "agent", 1, nil)
	_, _ = store.SlotRepositories(ctx, "s")
	_ = store.AddRestoringRepositories(ctx, "s", []SlotRepository{repository})
	_, _ = store.SlotRepository(ctx, "s", "r")
	_ = store.SetSlotRepositoryState(ctx, "s", "r", []string{"READY"}, "COLD")
	_, _ = store.Slot(ctx, "s")
	_ = store.SaveSnapshot(ctx, snapshot)
	_, _ = store.Snapshots(ctx, "s")
	_, _ = store.Repository(ctx, "r")
	_, _ = store.Workspace(ctx, "w")
	_, _ = store.WorkspaceRoots(ctx)
	_, _ = store.Status(ctx)
	_, _ = store.ListSessions(ctx, true)
	_ = store.ForgetWorkspace(ctx, "/w")
	_ = store.MarkArchived(ctx, "s", "s", now())
	_ = store.BeginSnapshot(ctx, "s", "s")
	_, _, _ = store.Release(ctx, "s", "w", "s")
	_, _ = store.StandbyGCCandidates(ctx, now(), 1)
	_, _ = store.ColdRepositoryCandidates(ctx, now())
	_, _, _ = store.ScheduleColdRepositoryRemoval(ctx, ColdRepositoryCandidate{SlotID: "s", WorkspaceID: "w", RepositoryID: "r"})
	_ = store.FinishColdRepositoryRemoval(ctx, "s", "r")
	_ = store.MarkStandbyArchived(ctx, "s")
	_, _, _ = store.ScheduleRemoval(ctx, "s", "")
	_ = store.FinishRemoval(ctx, "s")
	_ = store.DrainRoot(ctx, "/wx")
	_, _ = store.ExpiredSnapshots(ctx, now())
	_ = store.ExpireSessionSnapshots(ctx, "s")
	_ = store.PruneMetadata(ctx, now(), now(), now())
	_, _ = store.GCCandidates(ctx, now())
	_ = store.MarkSlotArchived(ctx, "s")
}

func TestDamagedSchemaFailsEveryOperationWithoutRecreatingState(t *testing.T) {
	store := openTestStore(t)
	for _, table := range []string{"snapshots", "jobs", "sessions", "slot_repositories", "slots", "workspace_repositories", "repositories", "workspaces", "events", "schema_migrations"} {
		if _, err := store.db.Exec(`DROP TABLE ` + table); err != nil {
			t.Fatalf("drop %s: %v", table, err)
		}
	}
	ctx := context.Background()
	w := discovery.Workspace{ID: "w", Root: "/w", Kind: "repository"}
	slot := Slot{ID: "s", WorkspaceID: "w", Path: "/wx/s", State: "PREPARING"}
	repository := SlotRepository{RepositoryID: "r", WorktreePath: "/wx/s/r", State: "PREPARING"}
	session := Session{ID: "s", WorkspaceID: "w", SlotID: "s", State: "STARTING", AgentKind: "codex", TokenHash: HashToken("token")}
	snapshot := Snapshot{ID: "snapshot", SessionID: "s", RepositoryID: "r"}

	if _, err := store.UpsertWorkspaceGeneration(ctx, w); err == nil {
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
		"record lease":       func() error { return store.RecordLease(ctx, "w") },
		"set slot":           func() error { return store.SetSlotState(ctx, "s", []string{"READY"}, "STALE", "test") },
		"finish preparation": func() error { return store.FinishPreparation(ctx, "s") },
		"session state":      func() error { return store.MarkSessionState(ctx, "s", []string{"ACTIVE"}, "RELEASING") },
		"bind agent":         func() error { return store.BindAgentSession(ctx, "s", "agent") },
		"bind fresh":         func() error { return store.BindFreshSession(ctx, "s", "", "agent") },
		"heartbeat":          func() error { return store.Heartbeat(ctx, "s", "token") },
		"add restore repos":  func() error { return store.AddRestoringRepositories(ctx, "s", []SlotRepository{repository}) },
		"set repository":     func() error { return store.SetSlotRepositoryState(ctx, "s", "r", []string{"READY"}, "COLD") },
		"save snapshot":      func() error { return store.SaveSnapshot(ctx, snapshot) },
		"forget":             func() error { return store.ForgetWorkspace(ctx, "/w") },
		"mark archived":      func() error { return store.MarkArchived(ctx, "s", "s", now()) },
		"begin snapshot":     func() error { return store.BeginSnapshot(ctx, "s", "s") },
		"finish cold":        func() error { return store.FinishColdRepositoryRemoval(ctx, "s", "r") },
		"finish removal":     func() error { return store.FinishRemoval(ctx, "s") },
		"drain root":         func() error { return store.DrainRoot(ctx, "/wx") },
		"expire snapshots":   func() error { return store.ExpireSessionSnapshots(ctx, "s") },
		"prune":              func() error { return store.PruneMetadata(ctx, now(), now(), now()) },
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
		"sessions":          func() error { _, err := store.ListSessions(ctx, true); return err },
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
	if _, err := store.BindResumeSlot(ctx, "s", "parent", "w", "agent", 1, nil); err == nil {
		t.Error("resume binding succeeded against damaged schema")
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
	if _, err := store.CreateSlotSession(ctx, Slot{ID: "parent", WorkspaceID: "workspace", Generation: 1, Path: filepath.Join(root, "parent"), State: "LEASED"}, nil, parent, ""); err != nil {
		t.Fatal(err)
	}
	unbound := Session{ID: "child", SlotID: "child", State: "UNBOUND", AgentKind: "codex", TokenHash: HashToken("child-token")}
	if _, err := store.CreateSlotSession(ctx, Slot{ID: "child", Path: filepath.Join(root, "child"), State: "UNBOUND"}, nil, unbound, ""); err != nil {
		t.Fatal(err)
	}
	repositories := []SlotRepository{{RepositoryID: "repository", WorktreePath: filepath.Join(root, "child", "repo"), RequestedRef: "main", BaseOID: "abc", Fingerprint: "fp"}}
	job, err := store.BindResumeSlot(ctx, "child", "parent", "workspace", "agent", 1, repositories)
	if err != nil || job.Kind != "RESTORE" || job.RepositoryID != "" {
		t.Fatalf("resume job=%+v err=%v", job, err)
	}
	child, err := store.SessionByID(ctx, "child")
	if err != nil || child.State != "RESTORING" || child.ParentSessionID != "parent" {
		t.Fatalf("bound child=%+v err=%v", child, err)
	}
	if _, err := store.FindByAgentSession(ctx, "codex", "agent"); err != nil {
		t.Fatal(err)
	}
	if err := store.AddRestoringRepositories(ctx, "child", repositories); err != nil {
		t.Fatalf("idempotent restore metadata: %v", err)
	}
	if err := store.AddRestoringRepositories(ctx, "child", append(repositories, SlotRepository{})); err == nil {
		t.Fatal("incomplete restore metadata was accepted")
	}
	if _, err := store.BindResumeSlot(ctx, "child", "parent", "workspace", "agent", 1, nil); err == nil {
		t.Fatal("bound resume slot was rebound")
	}

	orphan := Session{ID: "orphan", WorkspaceID: "workspace", SlotID: "orphan", State: "STARTING", AgentKind: "codex", ClientPID: 999999, TokenHash: HashToken("orphan-token")}
	if _, err := store.CreateSlotSession(ctx, Slot{ID: "orphan", WorkspaceID: "workspace", Generation: 1, Path: filepath.Join(root, "orphan"), State: "LEASED"}, nil, orphan, ""); err != nil {
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

func openTestStore(t *testing.T) *Store {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func seedWorkspace(t *testing.T, store *Store) {
	t.Helper()
	w := discovery.Workspace{ID: "workspace", Root: "/workspace", Kind: "repository", Repositories: []discovery.Repository{{ID: "repository", MainPath: "/workspace", CommonDir: "/workspace/.git", DefaultBranch: "main"}}}
	if err := store.UpsertWorkspace(context.Background(), w); err != nil {
		t.Fatal(err)
	}
}
