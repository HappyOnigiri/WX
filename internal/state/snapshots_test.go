package state

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/HappyOnigiri/WX/internal/discovery"
	"github.com/HappyOnigiri/WX/internal/domain"
)

func TestWorkspaceSnapshotMetadataGatesMultiRepositoryArchive(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	root := t.TempDir()
	workspace := discovery.Workspace{ID: "propos", Root: domain.CanonicalPath(root), Kind: "multi_repository", Repositories: []discovery.Repository{{ID: "repository", MainPath: domain.CanonicalPath(filepath.Join(root, "repository")), CommonDir: domain.CanonicalPath(filepath.Join(root, "repository.git")), RelativePath: "repository", DefaultBranch: "main"}}}
	registered, _, err := store.UpsertWorkspaceGeneration(ctx, workspace)
	if err != nil {
		t.Fatal(err)
	}
	workspaceID := string(registered.ID)
	session := Session{ID: "session", WorkspaceID: workspaceID, SlotID: "slot", State: "RELEASING", AgentKind: "codex", TokenHash: HashToken("token")}
	repositories := []SlotRepository{{RepositoryID: "repository", DirName: "repository", State: "LEASED", RequestedRef: "main", BaseOID: "base", Fingerprint: "fingerprint"}}
	if _, err := store.CreateSlotSession(ctx, Slot{ID: "slot", WorkspaceID: workspaceID, Generation: 1, RootID: testRootID, RelPath: filepath.Join(workspaceID, "slot"), State: "SNAPSHOTTING"}, repositories, session, ""); err != nil {
		t.Fatal(err)
	}
	expires := FormatTime(time.Now().Add(time.Hour))
	if err := store.MarkArchived(ctx, "session", "slot", expires); err == nil {
		t.Fatal("multi-repository session archived without a workspace root snapshot")
	}
	snapshot := WorkspaceSnapshot{SessionID: "session", RootID: testRootID, RelPath: filepath.Join("_recovery", "workspace-snapshots", "snapshot.tar"), SHA256: strings.Repeat("a", 64), Status: "ARCHIVED", CreatedAt: now(), ExpiresAt: expires}
	if err := store.SaveWorkspaceSnapshot(ctx, snapshot); err != nil {
		t.Fatal(err)
	}
	loaded, found, err := store.WorkspaceSnapshot(ctx, "session")
	// ArchivePath は join した root generation から read 時に組み立てるため、書き戻した値だけがこれを持ち、保存値は持たない。
	expected := snapshot
	expected.ArchivePath = filepath.Join(testRootPath, snapshot.RelPath)
	if err != nil || !found || loaded != expected {
		t.Fatalf("workspace snapshot found=%v loaded=%+v err=%v", found, loaded, err)
	}
	conflict := snapshot
	conflict.SHA256 = strings.Repeat("b", 64)
	if err := store.SaveWorkspaceSnapshot(ctx, conflict); err == nil {
		t.Fatal("conflicting workspace snapshot metadata was accepted")
	}
	if err := store.MarkArchived(ctx, "session", "slot", expires); err != nil {
		t.Fatal(err)
	}
	if err := store.ExpireSessionSnapshots(ctx, "session"); err != nil {
		t.Fatal(err)
	}
	if _, found, err := store.WorkspaceSnapshot(ctx, "session"); err != nil || found {
		t.Fatalf("expired workspace snapshot found=%v err=%v", found, err)
	}
}

func TestRecoveryRefQueriesAndPendingMappingFailure(t *testing.T) {
	store := openTestStore(t)
	seedWorkspace(t, store)
	ctx := context.Background()
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
	if _, err := recreateRestoreFixture(store, ctx, "child", "parent", "workspace", "agent", 1, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`UPDATE sessions SET agent_session_id=NULL WHERE id='parent'`); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.FinishPreparationWithRelease(ctx, "child"); err != nil {
		t.Fatalf("changed parent mapping error=%v", err)
	}

	snapshot := Snapshot{ID: "snapshot", SessionID: "parent", RepositoryID: "repository", HeadOID: "head", HeadRef: "refs/wx/recovery/head", IndexTreeOID: "index", WorktreeOID: "worktree", WorktreeRef: "refs/wx/recovery/worktree", Status: "ARCHIVED", CreatedAt: now(), ExpiresAt: now()}
	if err := store.SaveSnapshot(ctx, snapshot); err != nil {
		t.Fatal(err)
	}
	expectations, err := store.RecoveryRefExpectations(ctx, "repository")
	if err != nil || len(expectations) != 2 {
		t.Fatalf("recovery ref expectations=%+v err=%v", expectations, err)
	}
	if expectations[0].OID != "head" || expectations[1].OID != "worktree" || expectations[0].InFlight || expectations[1].InFlight {
		t.Fatalf("archived recovery ref expectations=%+v", expectations)
	}
}

func TestRecoveryRefExpectationsMarkActiveSnapshotJobInFlight(t *testing.T) {
	store := openTestStore(t)
	seedWorkspace(t, store)
	ctx := context.Background()
	session := Session{ID: "snapshotting", WorkspaceID: "workspace", SlotID: "snapshotting", State: "SNAPSHOTTING", AgentKind: "codex", TokenHash: HashToken("snapshotting")}
	job, err := store.CreateSlotSession(ctx, Slot{ID: session.SlotID, WorkspaceID: session.WorkspaceID, Generation: 1, RootID: testRootID, RelPath: filepath.Join(session.WorkspaceID, session.SlotID), State: "SNAPSHOTTING"}, nil, session, "SNAPSHOT")
	if err != nil {
		t.Fatal(err)
	}
	snapshot := Snapshot{ID: "snapshotting-snapshot", SessionID: session.ID, RepositoryID: "repository", HeadOID: "head", HeadRef: "refs/wx/recovery/snapshotting/head", IndexTreeOID: "index", WorktreeOID: "worktree", WorktreeRef: "refs/wx/recovery/snapshotting/worktree", Status: "ARCHIVED", CreatedAt: now(), ExpiresAt: FormatTime(time.Now().Add(time.Hour))}
	if err := store.SaveSnapshot(ctx, snapshot); err != nil {
		t.Fatal(err)
	}
	expectations, err := store.RecoveryRefExpectations(ctx, "repository")
	if err != nil || len(expectations) != 2 {
		t.Fatalf("in-flight expectations=%+v err=%v", expectations, err)
	}
	for _, expectation := range expectations {
		if !expectation.InFlight || expectation.SessionID != session.ID || expectation.SessionState != session.State {
			t.Fatalf("in-flight expectation=%+v", expectation)
		}
	}
	claimed, err := store.ClaimJob(ctx, job.ID, "test")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`UPDATE jobs SET lease_expires_at=? WHERE id=?`, FormatTime(time.Now().Add(-time.Minute)), claimed.ID); err != nil {
		t.Fatal(err)
	}
	expectations, err = store.RecoveryRefExpectations(ctx, "repository")
	if err != nil || len(expectations) != 2 {
		t.Fatalf("expired-lease expectations=%+v err=%v", expectations, err)
	}
	for _, expectation := range expectations {
		if expectation.InFlight {
			t.Fatalf("expired snapshot job remained in-flight: %+v", expectation)
		}
	}
	if err := store.FinishJob(ctx, claimed.ID, "test", errors.New("simulated archive failure")); err != nil {
		t.Fatal(err)
	}
	expectations, err = store.RecoveryRefExpectations(ctx, "repository")
	if err != nil || len(expectations) != 2 {
		t.Fatalf("failed-job expectations=%+v err=%v", expectations, err)
	}
	for _, expectation := range expectations {
		if expectation.InFlight {
			t.Fatalf("failed snapshot job remained in-flight: %+v", expectation)
		}
	}
}

func TestActiveRestoreProtectsSnapshotAndParentBindingCanTransfer(t *testing.T) {
	store := openTestStore(t)
	seedWorkspace(t, store)
	ctx := context.Background()
	parent := Session{ID: "parent", WorkspaceID: "workspace", SlotID: "parent", State: "ARCHIVED", AgentKind: "codex", TokenHash: HashToken("parent")}
	if _, err := store.CreateSlotSession(ctx, Slot{ID: "parent", WorkspaceID: "workspace", Generation: 1, RootID: testRootID, RelPath: "workspace/parent", State: "SNAPSHOTTED"}, nil, parent, ""); err != nil {
		t.Fatal(err)
	}
	if err := store.BindAgentSession(ctx, "parent", "agent"); err != nil {
		t.Fatal(err)
	}
	snapshot := Snapshot{ID: "snapshot", SessionID: "parent", RepositoryID: "repository", HeadOID: "head", HeadRef: "refs/wx/recovery/head", IndexTreeOID: "index", WorktreeOID: "worktree", WorktreeRef: "refs/wx/recovery/worktree", Status: "ARCHIVED", CreatedAt: now(), ExpiresAt: now()}
	if err := store.SaveSnapshot(ctx, snapshot); err != nil {
		t.Fatal(err)
	}
	resume := Session{ID: "resume", SlotID: "resume", State: "UNBOUND", AgentKind: "codex", TokenHash: HashToken("resume")}
	if _, err := store.CreateSlotSession(ctx, Slot{ID: "resume", State: "UNBOUND", RootID: testRootID, RelPath: "_unbound/resume"}, nil, resume, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := recreateRestoreFixture(store, ctx, "resume", "parent", "workspace", "agent", 1, nil); err != nil {
		t.Fatal(err)
	}
	if err := store.ExpireSessionSnapshots(ctx, "parent"); err == nil || !strings.Contains(err.Error(), "active restore") {
		t.Fatalf("active restore snapshot expiration error=%v", err)
	}

	transfer := Session{ID: "transfer", WorkspaceID: "workspace", SlotID: "transfer", ParentSessionID: "parent", State: "STARTING", AgentKind: "codex", TokenHash: HashToken("transfer")}
	if _, err := store.CreateSlotSession(ctx, Slot{ID: "transfer", WorkspaceID: "workspace", Generation: 1, RootID: testRootID, RelPath: "workspace/transfer", State: "LEASED"}, nil, transfer, ""); err != nil {
		t.Fatal(err)
	}
	if err := store.BindAgentSession(ctx, "transfer", "agent"); err != nil {
		t.Fatal(err)
	}
	mapped, err := store.FindByAgentSession(ctx, "codex", "agent")
	if err != nil || mapped.ID != "transfer" {
		t.Fatalf("transferred mapping=%+v err=%v", mapped, err)
	}
}

func TestSaveSnapshotIsIdempotentButRejectsConflict(t *testing.T) {
	store := openTestStore(t)
	seedWorkspace(t, store)
	ctx := context.Background()
	session := Session{ID: "session", WorkspaceID: "workspace", SlotID: "slot", State: "ACTIVE", AgentKind: "codex", TokenHash: HashToken("token")}
	if _, err := store.CreateSlotSession(ctx, Slot{ID: "slot", WorkspaceID: "workspace", Generation: 1, RootID: testRootID, RelPath: "workspace/slot", State: "LEASED"}, nil, session, ""); err != nil {
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

func TestSaveWorkspaceSnapshotPropagatesInsertionFault(t *testing.T) {
	store := openTestStore(t)
	seedWorkspace(t, store)
	ctx := context.Background()
	if _, err := store.CreateSlotSession(ctx, Slot{ID: "workspace-snapshot-fault", State: "ARCHIVED", RootID: testRootID, RelPath: "_unbound/workspace-snapshot-fault"}, nil, Session{ID: "workspace-snapshot-fault", SlotID: "workspace-snapshot-fault", State: "ARCHIVED", AgentKind: "codex", TokenHash: HashToken("workspace-snapshot-fault")}, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`CREATE TRIGGER fail_workspace_snapshot_insert BEFORE INSERT ON workspace_snapshots BEGIN SELECT RAISE(ABORT,'fault'); END`); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveWorkspaceSnapshot(ctx, WorkspaceSnapshot{SessionID: "workspace-snapshot-fault", RootID: testRootID, RelPath: "_recovery/workspace-snapshots/fault.tar", SHA256: "sha", Status: "ARCHIVED", CreatedAt: now(), ExpiresAt: now()}); err == nil {
		t.Fatal("workspace snapshot save succeeded despite an insertion fault")
	}
}

func TestExpireSessionSnapshotsPropagatesDeletionFaults(t *testing.T) {
	ctx := context.Background()

	t.Run("repository snapshot deletion fault", func(t *testing.T) {
		store := openTestStore(t)
		seedWorkspace(t, store)
		if _, err := store.CreateSlotSession(ctx, Slot{ID: "expire-fault", State: "ARCHIVED", RootID: testRootID, RelPath: "_unbound/expire-fault"}, nil, Session{ID: "expire-fault", SlotID: "expire-fault", State: "ARCHIVED", AgentKind: "codex", TokenHash: HashToken("expire-fault")}, ""); err != nil {
			t.Fatal(err)
		}
		if _, err := store.db.Exec(`INSERT INTO snapshots(id,session_id,repository_id,head_oid,head_recovery_ref,index_tree_oid,worktree_snapshot_oid,worktree_recovery_ref,status,created_at,expires_at) VALUES ('snap-fault','expire-fault','repository','head','refs/wx/head','tree','worktree','refs/wx/worktree','ARCHIVED',?,?)`, now(), now()); err != nil {
			t.Fatal(err)
		}
		if _, err := store.db.Exec(`CREATE TRIGGER fail_expire_snapshot BEFORE DELETE ON snapshots WHEN OLD.session_id='expire-fault' BEGIN SELECT RAISE(ABORT,'fault'); END`); err != nil {
			t.Fatal(err)
		}
		if err := store.ExpireSessionSnapshots(ctx, "expire-fault"); err == nil {
			t.Fatal("session snapshot expiry succeeded despite a repository snapshot deletion fault")
		}
	})

	t.Run("workspace snapshot deletion fault", func(t *testing.T) {
		store := openTestStore(t)
		seedWorkspace(t, store)
		if _, err := store.CreateSlotSession(ctx, Slot{ID: "expire-ws-fault", State: "ARCHIVED", RootID: testRootID, RelPath: "_unbound/expire-ws-fault"}, nil, Session{ID: "expire-ws-fault", SlotID: "expire-ws-fault", State: "ARCHIVED", AgentKind: "codex", TokenHash: HashToken("expire-ws-fault")}, ""); err != nil {
			t.Fatal(err)
		}
		if err := store.SaveWorkspaceSnapshot(ctx, WorkspaceSnapshot{SessionID: "expire-ws-fault", RootID: testRootID, RelPath: "_recovery/workspace-snapshots/expire.tar", SHA256: "sha", Status: "ARCHIVED", CreatedAt: now(), ExpiresAt: now()}); err != nil {
			t.Fatal(err)
		}
		if _, err := store.db.Exec(`CREATE TRIGGER fail_expire_workspace_snapshot BEFORE DELETE ON workspace_snapshots WHEN OLD.session_id='expire-ws-fault' BEGIN SELECT RAISE(ABORT,'fault'); END`); err != nil {
			t.Fatal(err)
		}
		if err := store.ExpireSessionSnapshots(ctx, "expire-ws-fault"); err == nil {
			t.Fatal("session snapshot expiry succeeded despite a workspace snapshot deletion fault")
		}
	})
}

// TestSaveSnapshotBackfillsMissingIndexRecoveryRefButStillRejectsMismatch は migration 010 の更新経路を検証する。
// index_recovery_ref 列の追加前に commit された row は ref が空なので、更新後の再実行では slot を quarantine せず
// その列を補完する。一方、index tree が実際に異なる row は拒否し続ける。
func TestSaveSnapshotRejectsConflictingIndexTree(t *testing.T) {
	store := openTestStore(t)
	seedWorkspace(t, store)
	ctx := context.Background()
	session := Session{ID: "session", WorkspaceID: "workspace", SlotID: "slot", State: "ACTIVE", AgentKind: "codex", TokenHash: HashToken("token")}
	if _, err := store.CreateSlotSession(ctx, Slot{ID: "slot", WorkspaceID: "workspace", Generation: 1, RootID: testRootID, RelPath: "workspace/slot", State: "LEASED"}, nil, session, ""); err != nil {
		t.Fatal(err)
	}
	original := Snapshot{ID: "snapshot", SessionID: "session", RepositoryID: "repository", HeadOID: "head", HeadRef: "refs/wx/recovery/session/repository/head", IndexTreeOID: "index", IndexRef: "refs/wx/recovery/session/repository/index", WorktreeOID: "worktree", WorktreeRef: "refs/wx/recovery/session/repository/worktree", Status: "ARCHIVED", CreatedAt: now(), ExpiresAt: FormatTime(time.Now().Add(time.Hour))}
	if err := store.SaveSnapshot(ctx, original); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveSnapshot(ctx, original); err != nil {
		t.Fatalf("re-saving the identical snapshot: %v", err)
	}
	conflicting := original
	conflicting.IndexTreeOID = "different"
	if err := store.SaveSnapshot(ctx, conflicting); err == nil {
		t.Fatal("a snapshot with a different index tree was accepted")
	}
}

func TestExpiredWorkspaceSnapshotWithoutRepositorySnapshots(t *testing.T) {
	store := openTestStore(t)
	seedWorkspace(t, store)
	ctx := t.Context()
	session := Session{ID: "interrupted", WorkspaceID: "workspace", SlotID: "interrupted", State: "EXPIRED", AgentKind: "codex", TokenHash: HashToken("token")}
	slot := Slot{ID: "interrupted", WorkspaceID: "workspace", Generation: 1, RootID: testRootID, RelPath: "workspace/interrupted", State: "ARCHIVED"}
	if _, err := store.CreateSlotSession(ctx, slot, nil, session, ""); err != nil {
		t.Fatal(err)
	}
	snapshot := WorkspaceSnapshot{SessionID: session.ID, RootID: testRootID, RelPath: "_recovery/workspace-snapshots/interrupted.tar", SHA256: strings.Repeat("a", 64), Status: "ARCHIVED", CreatedAt: now(), ExpiresAt: FormatTime(time.Now().Add(-time.Hour))}
	if err := store.SaveWorkspaceSnapshot(ctx, snapshot); err != nil {
		t.Fatal(err)
	}
	ids, err := store.ExpiredWorkspaceSnapshotSessions(ctx, now())
	if err != nil || len(ids) != 1 || ids[0] != session.ID {
		t.Fatalf("expired workspace archives=%v err=%v", ids, err)
	}
	if err := store.ExpireSessionSnapshots(ctx, session.ID); err != nil {
		t.Fatal(err)
	}
	ids, err = store.ExpiredWorkspaceSnapshotSessions(ctx, now())
	if err != nil || len(ids) != 0 {
		t.Fatalf("expired archive remains=%v err=%v", ids, err)
	}
}

// 作成途中と期限切れは復元の材料ではないため、実体を検査する doctor へ渡さない。
func TestActiveWorkspaceSnapshotsExcludesPendingAndExpiredArchives(t *testing.T) {
	store := openTestStore(t)
	seedWorkspace(t, store)
	ctx := context.Background()
	at := time.Now()
	for _, snapshot := range []struct {
		id, status string
		expires    time.Time
	}{
		{id: "active", status: "ARCHIVED", expires: at.Add(time.Hour)},
		{id: "pending", status: "PENDING", expires: at.Add(time.Hour)},
		{id: "expired", status: "ARCHIVED", expires: at.Add(-time.Hour)},
	} {
		createSessionSlot(t, store, snapshot.id, "ARCHIVED", "SNAPSHOTTED")
		if err := store.SaveWorkspaceSnapshot(ctx, WorkspaceSnapshot{
			SessionID: snapshot.id, RootID: testRootID, RelPath: filepath.Join("_recovery", snapshot.id+".tar"),
			SHA256: strings.Repeat("a", 64), Status: snapshot.status, CreatedAt: now(), ExpiresAt: FormatTime(snapshot.expires),
		}); err != nil {
			t.Fatal(err)
		}
	}
	active, err := store.ActiveWorkspaceSnapshots(ctx, FormatTime(at))
	if err != nil {
		t.Fatal(err)
	}
	if len(active) != 1 || active[0].SessionID != "active" {
		t.Fatalf("active workspace snapshots=%+v", active)
	}
	if active[0].ArchivePath != filepath.Join(testRootPath, "_recovery", "active.tar") {
		t.Fatalf("archive path=%q", active[0].ArchivePath)
	}
}
