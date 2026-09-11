package state

import (
	"context"
	"testing"
)

// seedRepositoryMembership は slot と session の repository 所属を直接登録する。
// CreateSlotSession に repository を渡さない fixture でも、forget 後に残る履歴 row を再現するために使う。
func seedRepositoryMembership(t *testing.T, store *Store, slotID, sessionID, repositoryID string) {
	t.Helper()
	ctx := context.Background()
	if _, err := store.db.ExecContext(ctx, `INSERT INTO slot_repositories(slot_id,repository_id,dir_name,state,requested_ref,base_oid,prepare_fingerprint) VALUES(?,?,'repository','COLD','main','oid','fingerprint')`, slotID, repositoryID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `INSERT INTO session_repositories(session_id,repository_id,relative_path,ordinal) VALUES(?,?,'',0)`, sessionID, repositoryID); err != nil {
		t.Fatal(err)
	}
}

// repositoryRows は repositories と、その所属を記録する表の行数を返す。
func repositoryRows(t *testing.T, store *Store, repositoryID string) (repositories, slotMembership, sessionMembership int) {
	t.Helper()
	ctx := context.Background()
	for _, query := range []struct {
		sql   string
		count *int
	}{
		{`SELECT count(*) FROM repositories WHERE id=?`, &repositories},
		{`SELECT count(*) FROM slot_repositories WHERE repository_id=?`, &slotMembership},
		{`SELECT count(*) FROM session_repositories WHERE repository_id=?`, &sessionMembership},
	} {
		if err := store.db.QueryRowContext(ctx, query.sql, repositoryID).Scan(query.count); err != nil {
			t.Fatal(err)
		}
	}
	return repositories, slotMembership, sessionMembership
}

// forget は workspace を消しても repository 記録を残していたため、実体が消えた path を doctor が毎回検査し、
// 恒久的に失敗していた。forget と同じ transaction で記録を消すことを確認する。
func TestForgetWorkspaceRemovesTheRepositoryRecordItLeavesBehind(t *testing.T) {
	store := openTestStore(t)
	seedWorkspace(t, store)
	ctx := context.Background()
	session := Session{ID: "session", WorkspaceID: "workspace", SlotID: "slot", State: "EXPIRED", AgentKind: "codex", TokenHash: HashToken("token")}
	slot := Slot{ID: "slot", WorkspaceID: "workspace", Generation: 1, RootID: testRootID, RelPath: "workspace/slot", State: "ARCHIVED"}
	if _, err := store.CreateSlotSession(ctx, slot, nil, session, ""); err != nil {
		t.Fatal(err)
	}
	seedRepositoryMembership(t, store, "slot", "session", "repository")
	if err := store.ForgetWorkspace(ctx, "/workspace"); err != nil {
		t.Fatal(err)
	}
	repositories, slotMembership, sessionMembership := repositoryRows(t, store, "repository")
	if repositories != 0 || slotMembership != 0 || sessionMembership != 0 {
		t.Fatalf("rows left after forget: repositories=%d slot_repositories=%d session_repositories=%d", repositories, slotMembership, sessionMembership)
	}
}

// PruneRepositories は forget が記録を残していた頃の DB を回収する保守経路である。
// 終了していない slot がまだ使う repository は残し、doctor の検査対象から外さない。
func TestPruneRepositoriesCollectsLeftoversAndKeepsRepositoriesInUse(t *testing.T) {
	store := openTestStore(t)
	seedWorkspace(t, store)
	ctx := context.Background()
	session := Session{ID: "session", WorkspaceID: "workspace", SlotID: "slot", State: "EXPIRED", AgentKind: "codex", TokenHash: HashToken("token")}
	slot := Slot{ID: "slot", WorkspaceID: "workspace", Generation: 1, RootID: testRootID, RelPath: "workspace/slot", State: "ARCHIVED"}
	if _, err := store.CreateSlotSession(ctx, slot, nil, session, ""); err != nil {
		t.Fatal(err)
	}
	seedRepositoryMembership(t, store, "slot", "session", "repository")
	// forget が repository 記録を消さなかった頃の DB と同じ形にする。
	if _, err := store.db.ExecContext(ctx, `DELETE FROM workspace_repositories WHERE workspace_id='workspace'`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `DELETE FROM workspaces WHERE id='workspace'`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE slots SET workspace_id=NULL WHERE id='slot'`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE sessions SET workspace_id=NULL WHERE id='session'`); err != nil {
		t.Fatal(err)
	}
	// 同じ repository を使う READY slot がある間は、記録がまだ必要なので消さない。
	if _, err := store.db.ExecContext(ctx, `INSERT INTO slots(id,workspace_id,generation,root_id,rel_path,state,created_at,updated_at) VALUES('live',NULL,1,?,'workspace/live','READY',?,?)`, testRootID, now(), now()); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `INSERT INTO slot_repositories(slot_id,repository_id,dir_name,state,requested_ref,base_oid,prepare_fingerprint) VALUES('live','repository','repository','READY','main','oid','fingerprint')`); err != nil {
		t.Fatal(err)
	}
	if removed, err := store.PruneRepositories(ctx); err != nil || removed != 0 {
		t.Fatalf("prune removed a repository in use: removed=%d err=%v", removed, err)
	}
	if repositories, _, _ := repositoryRows(t, store, "repository"); repositories != 1 {
		t.Fatalf("repositories=%d, want the record of the live slot", repositories)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE slots SET state='ARCHIVED' WHERE id='live'`); err != nil {
		t.Fatal(err)
	}
	removed, err := store.PruneRepositories(ctx)
	if err != nil || removed != 1 {
		t.Fatalf("prune after the slot ended: removed=%d err=%v", removed, err)
	}
	repositories, slotMembership, sessionMembership := repositoryRows(t, store, "repository")
	if repositories != 0 || slotMembership != 0 || sessionMembership != 0 {
		t.Fatalf("rows left after prune: repositories=%d slot_repositories=%d session_repositories=%d", repositories, slotMembership, sessionMembership)
	}
}

// 復元に必要な snapshot が残る repository は、slot も session も終わっていても消さない。
func TestPruneRepositoriesKeepsRepositoriesWithRecoverySnapshots(t *testing.T) {
	store := openTestStore(t)
	seedWorkspace(t, store)
	ctx := context.Background()
	session := Session{ID: "session", WorkspaceID: "workspace", SlotID: "slot", State: "EXPIRED", AgentKind: "codex", TokenHash: HashToken("token")}
	slot := Slot{ID: "slot", WorkspaceID: "workspace", Generation: 1, RootID: testRootID, RelPath: "workspace/slot", State: "ARCHIVED"}
	if _, err := store.CreateSlotSession(ctx, slot, nil, session, ""); err != nil {
		t.Fatal(err)
	}
	seedRepositoryMembership(t, store, "slot", "session", "repository")
	if _, err := store.db.ExecContext(ctx, `INSERT INTO snapshots(id,session_id,repository_id,head_oid,head_recovery_ref,index_tree_oid,worktree_snapshot_oid,worktree_recovery_ref,status,created_at,expires_at) VALUES('snapshot','session','repository','head','refs/wx/recovery/head','index','worktree','refs/wx/recovery/worktree','ARCHIVED',?,?)`, now(), now()); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `DELETE FROM workspace_repositories WHERE workspace_id='workspace'`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `DELETE FROM workspaces WHERE id='workspace'`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE slots SET workspace_id=NULL WHERE id='slot'`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE sessions SET workspace_id=NULL WHERE id='session'`); err != nil {
		t.Fatal(err)
	}
	if removed, err := store.PruneRepositories(ctx); err != nil || removed != 0 {
		t.Fatalf("prune removed a repository a snapshot still needs: removed=%d err=%v", removed, err)
	}
}
