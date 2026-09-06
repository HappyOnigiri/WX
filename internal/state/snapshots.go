package state

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
)

type Snapshot struct {
	ID, SessionID, RepositoryID, HeadOID, HeadRef, IndexTreeOID, IndexRef, WorktreeOID, WorktreeRef, Status, CreatedAt, ExpiresAt string
}

type RecoveryRefExpectation struct {
	Ref, OID, SessionID, SessionState string
	InFlight                          bool
}

// WorkspaceSnapshot は Slot と同様に bundle archive を位置付ける。RootID/RelPath が authority で、ArchivePath は派生値である。
type WorkspaceSnapshot struct {
	SessionID, RootID, RelPath, ArchivePath, SHA256, Status, CreatedAt, ExpiresAt string
}

// SaveSnapshot は repository recovery snapshot を保存するか、同一 session/repository の既存 record が同じ object/ref を指すことを検証する。
// SQL の ON CONFLICT は全 immutable field が一致する場合だけ status を no-op 更新する。不一致では既存 row を変えず、RETURNING も row を返さない。
func (s *Store) SaveSnapshot(ctx context.Context, x Snapshot) error {
	s.writer.Lock()
	defer s.writer.Unlock()
	var ok int
	err := s.db.QueryRowContext(ctx, `INSERT INTO snapshots(id,session_id,repository_id,head_oid,head_recovery_ref,index_tree_oid,index_recovery_ref,worktree_snapshot_oid,worktree_recovery_ref,status,created_at,expires_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(session_id,repository_id) DO UPDATE SET status=excluded.status
		WHERE snapshots.id=excluded.id AND snapshots.head_oid=excluded.head_oid AND snapshots.head_recovery_ref=excluded.head_recovery_ref
		  AND snapshots.index_tree_oid=excluded.index_tree_oid AND snapshots.index_recovery_ref=excluded.index_recovery_ref
		  AND snapshots.worktree_snapshot_oid=excluded.worktree_snapshot_oid AND snapshots.worktree_recovery_ref=excluded.worktree_recovery_ref
		RETURNING 1`,
		x.ID, x.SessionID, x.RepositoryID, x.HeadOID, x.HeadRef, x.IndexTreeOID, x.IndexRef, x.WorktreeOID, x.WorktreeRef, x.Status, x.CreatedAt, x.ExpiresAt).Scan(&ok)
	if errors.Is(err, sql.ErrNoRows) {
		return errors.New("snapshot metadata conflicts with an existing recovery snapshot")
	}
	return err
}

func (s *Store) Snapshots(ctx context.Context, sessionID string) ([]Snapshot, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,session_id,repository_id,head_oid,head_recovery_ref,index_tree_oid,index_recovery_ref,worktree_snapshot_oid,worktree_recovery_ref,status,created_at,expires_at FROM snapshots WHERE session_id=? ORDER BY repository_id`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Snapshot
	for rows.Next() {
		var x Snapshot
		if err := rows.Scan(&x.ID, &x.SessionID, &x.RepositoryID, &x.HeadOID, &x.HeadRef, &x.IndexTreeOID, &x.IndexRef, &x.WorktreeOID, &x.WorktreeRef, &x.Status, &x.CreatedAt, &x.ExpiresAt); err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

// SaveWorkspaceSnapshot は workspace bundle の recovery snapshot を記録するか、同じ session の既存 write が
// 完全に同じ archive を指すことを検証する。比較を SQL に移す方法は SaveSnapshot を参照する。
func (s *Store) SaveWorkspaceSnapshot(ctx context.Context, x WorkspaceSnapshot) error {
	s.writer.Lock()
	defer s.writer.Unlock()
	var ok int
	err := s.db.QueryRowContext(ctx, `INSERT INTO workspace_snapshots(session_id,root_id,rel_path,sha256,status,created_at,expires_at) VALUES(?,?,?,?,?,?,?)
		ON CONFLICT(session_id) DO UPDATE SET status=excluded.status
		WHERE workspace_snapshots.root_id=excluded.root_id AND workspace_snapshots.rel_path=excluded.rel_path AND workspace_snapshots.sha256=excluded.sha256
		  AND workspace_snapshots.status=excluded.status AND workspace_snapshots.expires_at=excluded.expires_at
		RETURNING 1`,
		x.SessionID, x.RootID, x.RelPath, x.SHA256, x.Status, x.CreatedAt, x.ExpiresAt).Scan(&ok)
	if errors.Is(err, sql.ErrNoRows) {
		return errors.New("workspace snapshot metadata conflicts with an existing recovery snapshot")
	}
	return err
}

func (s *Store) WorkspaceSnapshot(ctx context.Context, sessionID string) (WorkspaceSnapshot, bool, error) {
	var x WorkspaceSnapshot
	var rootPath string
	err := s.db.QueryRowContext(ctx, `SELECT ws.session_id,ws.root_id,rt.path,ws.rel_path,ws.sha256,ws.status,ws.created_at,ws.expires_at FROM workspace_snapshots ws JOIN roots rt ON rt.id=ws.root_id WHERE ws.session_id=?`, sessionID).Scan(&x.SessionID, &x.RootID, &rootPath, &x.RelPath, &x.SHA256, &x.Status, &x.CreatedAt, &x.ExpiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return WorkspaceSnapshot{}, false, nil
	}
	x.ArchivePath = filepath.Join(rootPath, x.RelPath)
	return x, err == nil, err
}

func (s *Store) MarkArchived(ctx context.Context, sessionID, slotID, expiry string) error {
	s.writer.Lock()
	defer s.writer.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var missingWorkspaceSnapshot int
	if err := tx.QueryRowContext(ctx, `SELECT CASE WHEN w.kind='multi_repository' AND NOT EXISTS (SELECT 1 FROM workspace_snapshots ws WHERE ws.session_id=se.id AND ws.status='ARCHIVED') THEN 1 ELSE 0 END FROM sessions se JOIN workspaces w ON w.id=se.workspace_id WHERE se.id=?`, sessionID).Scan(&missingWorkspaceSnapshot); err != nil {
		return err
	}
	if missingWorkspaceSnapshot != 0 {
		return errors.New("multi-repository session has no archived workspace snapshot")
	}
	res, err := tx.ExecContext(ctx, `UPDATE sessions SET state='ARCHIVED',archived_at=COALESCE(archived_at,?),expires_at=COALESCE(expires_at,?) WHERE id=? AND state IN ('RELEASING','SNAPSHOTTING','ARCHIVED')`, now(), expiry, sessionID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return errors.New("session cannot be archived from its current state")
	}
	res, err = tx.ExecContext(ctx, `UPDATE slots SET state='SNAPSHOTTED',updated_at=? WHERE id=? AND state IN ('DRAINING','SNAPSHOTTING','SNAPSHOTTED')`, now(), slotID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return errors.New("slot cannot be marked snapshotted from its current state")
	}
	return tx.Commit()
}

func (s *Store) BeginSnapshot(ctx context.Context, sessionID, slotID string) error {
	s.writer.Lock()
	defer s.writer.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `UPDATE sessions SET state='SNAPSHOTTING' WHERE id=? AND state IN ('RELEASING','SNAPSHOTTING')`, sessionID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return errors.New("session cannot begin snapshot from its current state")
	}
	res, err = tx.ExecContext(ctx, `UPDATE slots SET state='SNAPSHOTTING',updated_at=? WHERE id=? AND state IN ('DRAINING','SNAPSHOTTING')`, now(), slotID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return errors.New("slot cannot begin snapshot from its current state")
	}
	return tx.Commit()
}

// RecoveryRefExpectations は公開済み recovery ref ごとの durable ownership proof を返す。metadata を commit 済みで ref 公開未完了の snapshot job は、
// pending job または未期限 worker lease があれば InFlight とする。reconcile はその ref を待てるが、name/object ID を厳密に説明できない ref は quarantine する。
// commentlint:allow-long -- ref 公開途中の job と未知 ref の扱いを区別するため
func (s *Store) RecoveryRefExpectations(ctx context.Context, repositoryID string) ([]RecoveryRefExpectation, error) {
	at := now()
	rows, err := s.db.QueryContext(ctx, `
		WITH inflight AS (
			SELECT session_id FROM jobs
			WHERE kind='SNAPSHOT' AND (state='PENDING' OR (state='RUNNING' AND lease_expires_at>?))
		)
		SELECT sn.head_recovery_ref,sn.head_oid,sn.session_id,se.state,
		   CASE WHEN se.state IN ('RELEASING','SNAPSHOTTING') AND EXISTS (SELECT 1 FROM inflight i WHERE i.session_id=sn.session_id) THEN 1 ELSE 0 END
		FROM snapshots sn JOIN sessions se ON se.id=sn.session_id
		WHERE sn.repository_id=? AND sn.status='ARCHIVED'
		UNION ALL
		SELECT sn.worktree_recovery_ref,sn.worktree_snapshot_oid,sn.session_id,se.state,
		   CASE WHEN se.state IN ('RELEASING','SNAPSHOTTING') AND EXISTS (SELECT 1 FROM inflight i WHERE i.session_id=sn.session_id) THEN 1 ELSE 0 END
		FROM snapshots sn JOIN sessions se ON se.id=sn.session_id
		WHERE sn.repository_id=? AND sn.status='ARCHIVED'
		UNION ALL
		SELECT sn.index_recovery_ref,sn.index_tree_oid,sn.session_id,se.state,
		   CASE WHEN se.state IN ('RELEASING','SNAPSHOTTING') AND EXISTS (SELECT 1 FROM inflight i WHERE i.session_id=sn.session_id) THEN 1 ELSE 0 END
		FROM snapshots sn JOIN sessions se ON se.id=sn.session_id
		WHERE sn.repository_id=? AND sn.status='ARCHIVED' AND sn.index_recovery_ref<>''
		ORDER BY 1`, at, repositoryID, repositoryID, repositoryID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var refs []RecoveryRefExpectation
	for rows.Next() {
		var ref RecoveryRefExpectation
		var inFlight int
		if err := rows.Scan(&ref.Ref, &ref.OID, &ref.SessionID, &ref.SessionState, &inFlight); err != nil {
			return nil, err
		}
		ref.InFlight = inFlight != 0
		refs = append(refs, ref)
	}
	return refs, rows.Err()
}

func (s *Store) ExpiredSnapshots(ctx context.Context, before string) ([]Snapshot, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT sn.id,sn.session_id,sn.repository_id,sn.head_oid,sn.head_recovery_ref,sn.index_tree_oid,sn.index_recovery_ref,sn.worktree_snapshot_oid,sn.worktree_recovery_ref,sn.status,sn.created_at,sn.expires_at FROM snapshots sn JOIN sessions se ON se.id=sn.session_id JOIN slots sl ON sl.id=se.slot_id WHERE se.state IN ('ARCHIVED','EXPIRED') AND sl.state='ARCHIVED' AND sn.status='ARCHIVED' AND sn.expires_at<=? AND NOT EXISTS (SELECT 1 FROM sessions child JOIN jobs j ON j.session_id=child.id WHERE child.parent_session_id=se.id AND j.kind='RESTORE' AND j.state IN ('PENDING','RUNNING')) ORDER BY sn.session_id,sn.repository_id`, before)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Snapshot
	for rows.Next() {
		var snapshot Snapshot
		if err := rows.Scan(&snapshot.ID, &snapshot.SessionID, &snapshot.RepositoryID, &snapshot.HeadOID, &snapshot.HeadRef, &snapshot.IndexTreeOID, &snapshot.IndexRef, &snapshot.WorktreeOID, &snapshot.WorktreeRef, &snapshot.Status, &snapshot.CreatedAt, &snapshot.ExpiresAt); err != nil {
			return nil, err
		}
		out = append(out, snapshot)
	}
	return out, rows.Err()
}

func (s *Store) ExpireSessionSnapshots(ctx context.Context, sessionID string) error {
	s.writer.Lock()
	defer s.writer.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var activeRestore int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM sessions child JOIN jobs j ON j.session_id=child.id WHERE child.parent_session_id=? AND j.kind='RESTORE' AND j.state IN ('PENDING','RUNNING')`, sessionID).Scan(&activeRestore); err != nil {
		return err
	}
	if activeRestore != 0 {
		return errors.New("recovery snapshot has an active restore job")
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM snapshots WHERE session_id=?`, sessionID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM workspace_snapshots WHERE session_id=?`, sessionID); err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx, `UPDATE sessions SET state='EXPIRED' WHERE id=? AND state IN ('ARCHIVED','EXPIRED')`, sessionID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return errors.New("session cannot expire from its current state")
	}
	return tx.Commit()
}

// ExpiredWorkspaceSnapshotSessions は repository snapshot を保存する前に中断した archive も回収候補にする。
func (s *Store) ExpiredWorkspaceSnapshotSessions(ctx context.Context, before string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT ws.session_id FROM workspace_snapshots ws JOIN sessions se ON se.id=ws.session_id JOIN slots sl ON sl.id=se.slot_id
 WHERE sl.state='ARCHIVED' AND se.state IN ('ARCHIVED','EXPIRED') AND ws.expires_at<=?
 AND NOT EXISTS (SELECT 1 FROM sessions child JOIN jobs j ON j.session_id=child.id WHERE child.parent_session_id=se.id AND j.kind='RESTORE' AND j.state IN ('PENDING','RUNNING'))`, before)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}
