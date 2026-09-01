package state

import (
	"bytes"
	"context"
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
	if err := os.WriteFile(path, original, 0600); err != nil {
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
