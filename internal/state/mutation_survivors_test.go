package state

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
)

// TestMutationPruneQuarantinedArtifactsPropagatesDeleteFailure は、再検出されなかった隔離記録の削除失敗を隠さないことを固定する。
func TestMutationPruneQuarantinedArtifactsPropagatesDeleteFailure(t *testing.T) {
	t.Parallel()
	store := openTestStore(t)
	ctx := context.Background()
	if _, err := store.QuarantineArtifact(ctx, "unknown_refs", "gone", "missing"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `CREATE TRIGGER mutation_prune_delete_failure BEFORE DELETE ON quarantined_artifacts WHEN OLD.path='gone' BEGIN SELECT RAISE(ABORT,'prune delete failure'); END`); err != nil {
		t.Fatal(err)
	}
	err := store.PruneQuarantinedArtifacts(ctx, []string{"unknown_refs"}, nil)
	if err == nil || !strings.Contains(err.Error(), "prune delete failure") {
		t.Fatalf("prune delete error=%v, want trigger error", err)
	}
	var count int
	if err := store.db.QueryRowContext(ctx, `SELECT count(*) FROM quarantined_artifacts WHERE path='gone'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("failed prune removed the quarantine record: count=%d", count)
	}
}

// TestMutationBackupSyncFilePropagatesSyncFailure は、open 後の file.Sync 失敗を隠さないことを固定する。
func TestMutationBackupSyncFilePropagatesSyncFailure(t *testing.T) {
	t.Parallel()
	root, _, err := openBackupRootForTest(t, "/dev")
	if err != nil {
		t.Fatal(err)
	}
	if err := syncBackupFile(root, "null"); err == nil {
		t.Fatal("syncing /dev/null succeeded")
	}
}

// TestMutationBindAgentSessionPropagatesPendingRestoreFailure は、pending restore の bind 更新が失敗したら成功扱いにしないことを固定する。
func TestMutationBindAgentSessionPropagatesPendingRestoreFailure(t *testing.T) {
	t.Parallel()
	store, ctx, _, child := newPendingResumeFixture(t, false)
	if _, err := store.db.ExecContext(ctx, `CREATE TRIGGER mutation_bind_pending_failure BEFORE UPDATE OF started_at ON sessions WHEN OLD.id='pending-child' BEGIN SELECT RAISE(ABORT,'pending bind failure'); END`); err != nil {
		t.Fatal(err)
	}
	err := store.BindAgentSession(ctx, child.ID, child.PendingAgentSessionID)
	if err == nil || !strings.Contains(err.Error(), "pending bind failure") {
		t.Fatalf("pending restore bind error=%v, want trigger error", err)
	}
	var startedAt, heartbeatAt, pending string
	if err := store.db.QueryRowContext(ctx, `SELECT COALESCE(started_at,''),COALESCE(last_heartbeat_at,''),COALESCE(pending_agent_session_id,'') FROM sessions WHERE id=?`, child.ID).Scan(&startedAt, &heartbeatAt, &pending); err != nil {
		t.Fatal(err)
	}
	if startedAt != "" || heartbeatAt != "" || pending != child.PendingAgentSessionID {
		t.Fatalf("failed pending bind changed started_at=%q heartbeat_at=%q pending=%q", startedAt, heartbeatAt, pending)
	}
}

// TestMutationRegisterReservedSlotSessionCoversRestoreAndOptionalBranches は、通常貸出・復元貸出・workspace 無しの各登録経路を観測する。
func TestMutationRegisterReservedSlotSessionCoversRestoreAndOptionalBranches(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("prepare persists job and membership", func(t *testing.T) {
		store := openTestStore(t)
		seedWorkspace(t, store)
		session := Session{ID: "registered-session", WorkspaceID: "workspace", SlotID: "registered-slot", State: "STARTING", AgentKind: "codex", TokenHash: HashToken("registered-session")}
		slot := Slot{ID: session.SlotID, WorkspaceID: session.WorkspaceID, Generation: 1, RootID: testRootID, RelPath: "workspace/registered-slot", OwnerSessionID: session.ID}
		if err := store.ReserveSlot(ctx, slot); err != nil {
			t.Fatal(err)
		}
		if err := store.ConfirmSlotCreation(ctx, slot.ID, "registered-identity"); err != nil {
			t.Fatal(err)
		}
		job, err := store.RegisterReservedSlotSession(ctx, slot.ID, []SlotRepository{{RepositoryID: "repository", DirName: "repository", State: "PREPARING", RequestedRef: "main", BaseOID: "base", Fingerprint: "fingerprint"}}, session, "PREPARING", "PREPARE")
		if err != nil {
			t.Fatal(err)
		}
		var jobs, memberships int
		if err := store.db.QueryRowContext(ctx, `SELECT count(*) FROM jobs WHERE id=? AND kind='PREPARE'`, job.ID).Scan(&jobs); err != nil {
			t.Fatal(err)
		}
		if err := store.db.QueryRowContext(ctx, `SELECT count(*) FROM session_repositories WHERE session_id=?`, session.ID).Scan(&memberships); err != nil {
			t.Fatal(err)
		}
		if job.ID == "" || jobs != 1 || memberships != 1 {
			t.Fatalf("registered job=%+v jobs=%d memberships=%d", job, jobs, memberships)
		}
	})

	t.Run("restore copies parent membership and rejects a second restore", func(t *testing.T) {
		store := openTestStore(t)
		seedWorkspace(t, store)
		parent := Session{ID: "restore-parent", WorkspaceID: "workspace", SlotID: "restore-parent", State: "ARCHIVED", AgentKind: "codex", TokenHash: HashToken("restore-parent")}
		if _, err := store.CreateSlotSession(ctx, Slot{ID: parent.SlotID, WorkspaceID: parent.WorkspaceID, Generation: 1, RootID: testRootID, RelPath: "workspace/restore-parent", State: "SNAPSHOTTED"}, nil, parent, ""); err != nil {
			t.Fatal(err)
		}
		if _, err := store.db.ExecContext(ctx, `INSERT INTO session_repositories(session_id,repository_id,relative_path,ordinal) VALUES('restore-parent','repository','',0)`); err != nil {
			t.Fatal(err)
		}
		register := func(id string) Session {
			session := Session{ID: id, WorkspaceID: "workspace", SlotID: id, ParentSessionID: parent.ID, State: "RESTORING", AgentKind: "codex", TokenHash: HashToken(id)}
			slot := Slot{ID: id, WorkspaceID: "workspace", Generation: 1, RootID: testRootID, RelPath: "workspace/" + id, OwnerSessionID: id}
			if err := store.ReserveSlot(ctx, slot); err != nil {
				t.Fatal(err)
			}
			if err := store.ConfirmSlotCreation(ctx, id, "identity-"+id); err != nil {
				t.Fatal(err)
			}
			return session
		}
		first := register("restore-child")
		if _, err := store.RegisterReservedSlotSession(ctx, first.SlotID, nil, first, "RESTORING", "RESTORE"); err != nil {
			t.Fatal(err)
		}
		var memberships int
		if err := store.db.QueryRowContext(ctx, `SELECT count(*) FROM session_repositories WHERE session_id='restore-child' AND repository_id='repository'`).Scan(&memberships); err != nil {
			t.Fatal(err)
		}
		if memberships != 1 {
			t.Fatalf("restored session memberships=%d, want copied parent membership", memberships)
		}
		second := register("restore-child-two")
		if _, err := store.RegisterReservedSlotSession(ctx, second.SlotID, nil, second, "RESTORING", "RESTORE"); err == nil || !strings.Contains(err.Error(), "already being restored") {
			t.Fatalf("second restore error=%v, want concurrent restore rejection", err)
		}
		if _, err := store.SessionByID(ctx, second.ID); !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("rejected restore left session=%v err=%v", second.ID, err)
		}
	})

	t.Run("empty workspace skips optional writes", func(t *testing.T) {
		store := openTestStore(t)
		seedWorkspace(t, store)
		if _, err := store.db.ExecContext(ctx, `INSERT INTO workspaces(id,root_path,kind,generation,discovery_state,first_seen_at,last_seen_at,last_reconciled_at) VALUES('', '/empty', 'repository', 1, 'READY', ?, ?, ?)`, now(), now(), now()); err != nil {
			t.Fatal(err)
		}
		if _, err := store.db.ExecContext(ctx, `INSERT INTO workspace_repositories(workspace_id,repository_id,relative_path,ordinal) VALUES('', 'repository', '', 0)`); err != nil {
			t.Fatal(err)
		}
		session := Session{ID: "empty-workspace-session", SlotID: "empty-workspace-slot", State: "STARTING", AgentKind: "codex", TokenHash: HashToken("empty-workspace-session")}
		slot := Slot{ID: session.SlotID, Generation: 1, RootID: testRootID, RelPath: "_unbound/empty-workspace-slot", OwnerSessionID: session.ID}
		if err := store.ReserveSlot(ctx, slot); err != nil {
			t.Fatal(err)
		}
		if err := store.ConfirmSlotCreation(ctx, slot.ID, "empty-workspace-identity"); err != nil {
			t.Fatal(err)
		}
		job, err := store.RegisterReservedSlotSession(ctx, slot.ID, []SlotRepository{{RepositoryID: "repository", DirName: "repository", State: "PREPARING", RequestedRef: "main", BaseOID: "base", Fingerprint: "fingerprint"}}, session, "PREPARING", "")
		if err != nil {
			t.Fatal(err)
		}
		if job.ID != "" {
			t.Fatalf("empty job kind created job=%+v", job)
		}
		var memberships, jobs int
		if err := store.db.QueryRowContext(ctx, `SELECT count(*) FROM session_repositories WHERE session_id=?`, session.ID).Scan(&memberships); err != nil {
			t.Fatal(err)
		}
		if err := store.db.QueryRowContext(ctx, `SELECT count(*) FROM jobs WHERE slot_id=?`, slot.ID).Scan(&jobs); err != nil {
			t.Fatal(err)
		}
		if memberships != 0 || jobs != 0 {
			t.Fatalf("empty workspace optional rows memberships=%d jobs=%d", memberships, jobs)
		}
		var leasedAt string
		if err := store.db.QueryRowContext(ctx, `SELECT COALESCE(last_leased_at,'') FROM repositories WHERE id='repository'`).Scan(&leasedAt); err != nil {
			t.Fatal(err)
		}
		if leasedAt != "" {
			t.Fatalf("empty workspace updated repository lease time=%q", leasedAt)
		}
	})
}

// TestMutationSetSlotStateWithDetailPublishesDetail は、遷移イベントにも診断ファイルの場所を残すことを固定する。
func TestMutationSetSlotStateWithDetailPublishesDetail(t *testing.T) {
	t.Parallel()
	store := openTestStore(t)
	seedWorkspace(t, store)
	ctx := context.Background()
	if _, err := store.CreateStandby(ctx, Slot{ID: "detail-event", WorkspaceID: "workspace", Generation: 1, RootID: testRootID, RelPath: "workspace/detail-event", State: "PREPARING"}, nil); err != nil {
		t.Fatal(err)
	}
	if err := store.SetSlotStateWithDetail(ctx, "detail-event", []string{"PREPARING"}, "FAILED", "PREPARE_FAILED", "/logs/prepare.log"); err != nil {
		t.Fatal(err)
	}
	var message string
	if err := store.db.QueryRowContext(ctx, `SELECT message FROM events WHERE slot_id='detail-event' ORDER BY id DESC LIMIT 1`).Scan(&message); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(message, "detail_path=/logs/prepare.log") {
		t.Fatalf("slot transition event=%q, missing detail path", message)
	}
}

// TestMutationPublishStandbyUpdatePlacementsPropagatesDeleteFailure は、staging の破棄失敗を隠さないことを固定する。
func TestMutationPublishStandbyUpdatePlacementsPropagatesDeleteFailure(t *testing.T) {
	t.Parallel()
	store := openTestStore(t)
	seedWorkspace(t, store)
	ctx := context.Background()
	if _, err := store.CreateStandby(ctx, Slot{ID: "publish-fault", WorkspaceID: "workspace", Generation: 1, RootID: testRootID, RelPath: "workspace/publish-fault", State: "PREPARING"}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `INSERT INTO slot_update_placements(slot_id,repository_id,relative_path,kind,source_path,content_sha256) VALUES('publish-fault','repository','config','copy','/source/config','hash')`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `CREATE TRIGGER mutation_publish_delete_failure BEFORE DELETE ON slot_update_placements WHEN OLD.slot_id='publish-fault' BEGIN SELECT RAISE(ABORT,'publish delete failure'); END`); err != nil {
		t.Fatal(err)
	}
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	err = publishStandbyUpdatePlacements(ctx, tx, "publish-fault")
	if err == nil || !strings.Contains(err.Error(), "publish delete failure") {
		t.Fatalf("publish delete error=%v, want trigger error", err)
	}
	if rollbackErr := tx.Rollback(); rollbackErr != nil {
		t.Fatal(rollbackErr)
	}
}

// TestMutationFillSlotRepositoriesPropagatesBothLookupErrors は slot/session membership 読み取りの失敗をそれぞれ返すことを固定する。
func TestMutationFillSlotRepositoriesPropagatesBothLookupErrors(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	t.Run("slot lookup", func(t *testing.T) {
		store := openTestStore(t)
		if _, err := store.db.ExecContext(ctx, `DROP TABLE slot_repositories`); err != nil {
			t.Fatal(err)
		}
		if err := store.fillSlotRepositories(ctx, nil); err == nil {
			t.Fatal("slot repository lookup error was hidden")
		}
	})
	t.Run("session lookup", func(t *testing.T) {
		store := openTestStore(t)
		if _, err := store.db.ExecContext(ctx, `DROP TABLE session_repositories`); err != nil {
			t.Fatal(err)
		}
		if err := store.fillSlotRepositories(ctx, nil); err == nil {
			t.Fatal("session repository lookup error was hidden")
		}
	})
}

// TestMutationCompleteStandbyUpdateRepositoriesRejectsEmptyRepositorySet は空の repository 集合を成功公開しないことを固定する。
func TestMutationCompleteStandbyUpdateRepositoriesRejectsEmptyRepositorySet(t *testing.T) {
	t.Parallel()
	store := openTestStore(t)
	seedWorkspace(t, store)
	ctx := context.Background()
	if _, err := store.CreateStandby(ctx, Slot{ID: "empty-update", WorkspaceID: "workspace", Generation: 1, RootID: testRootID, RelPath: "workspace/empty-update", State: "PREPARING"}, nil); err != nil {
		t.Fatal(err)
	}
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	err = completeStandbyUpdateRepositories(ctx, tx, "empty-update")
	if err == nil || !strings.Contains(err.Error(), "incomplete") {
		t.Fatalf("empty repository completion error=%v, want incomplete", err)
	}
	if rollbackErr := tx.Rollback(); rollbackErr != nil {
		t.Fatal(rollbackErr)
	}
}
