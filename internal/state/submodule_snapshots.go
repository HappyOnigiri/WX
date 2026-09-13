package state

import "context"

// SubmoduleSnapshot は 1 submodule 分の recovery snapshot である。
// HeadRef は子の HEAD が branch を指していたときの ref 名で、detached なら空になる。
// CapsuleOID は保存した全 object の到達性を 1 本で支える commit で、CapsuleRef はそれを `<common>/modules/<Name>` で公開する ref である。
type SubmoduleSnapshot struct {
	SessionID, RepositoryID, Path, Name             string
	HeadOID, HeadRef, IndexTreeOID, WorktreeTreeOID string
	GitStateOID, CapsuleOID, CapsuleRef, CreatedAt  string
}

// SubmoduleRecoveryRef は診断が 1 件の capsule ref を照合するための期待値である。
// ExpiresAt は ref を支える親 snapshot の期限で、期限切れの ref を問題から外す判断に使う。
type SubmoduleRecoveryRef struct {
	RepositoryID, Name, Path, Ref, OID, SessionID, ExpiresAt string
}

// ReplaceSubmoduleSnapshots は 1 repository 分の子 snapshot を丸ごと置き換える。
// EnsureRecoveryJobs による snapshot のやり直しで、解消済みの古い行が復元対象として残らないようにする。
// 行は snapshots への外部キーを持つため、呼び出し側は SaveSnapshot の後に実行する。
func (s *Store) ReplaceSubmoduleSnapshots(ctx context.Context, sessionID, repositoryID string, entries []SubmoduleSnapshot) error {
	s.writer.Lock()
	defer s.writer.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM submodule_snapshots WHERE session_id=? AND repository_id=?`, sessionID, repositoryID); err != nil {
		return err
	}
	createdAt := now()
	for _, entry := range entries {
		if _, err := tx.ExecContext(ctx, `INSERT INTO submodule_snapshots(session_id,repository_id,path,name,head_oid,head_ref,index_tree_oid,worktree_tree_oid,git_state_oid,capsule_oid,capsule_recovery_ref,created_at)
			VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`,
			sessionID, repositoryID, entry.Path, entry.Name, entry.HeadOID, entry.HeadRef, entry.IndexTreeOID, entry.WorktreeTreeOID, entry.GitStateOID, entry.CapsuleOID, entry.CapsuleRef, createdAt); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// SubmoduleSnapshots は 1 repository 分の子 snapshot を path 順に返す。
// 復元は親より先に子を戻すため、この順序で一巡できるようにしている。
func (s *Store) SubmoduleSnapshots(ctx context.Context, sessionID, repositoryID string) ([]SubmoduleSnapshot, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT session_id,repository_id,path,name,head_oid,head_ref,index_tree_oid,worktree_tree_oid,git_state_oid,capsule_oid,capsule_recovery_ref,created_at
		FROM submodule_snapshots WHERE session_id=? AND repository_id=? ORDER BY path`, sessionID, repositoryID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SubmoduleSnapshot
	for rows.Next() {
		var x SubmoduleSnapshot
		if err := rows.Scan(&x.SessionID, &x.RepositoryID, &x.Path, &x.Name, &x.HeadOID, &x.HeadRef, &x.IndexTreeOID, &x.WorktreeTreeOID, &x.GitStateOID, &x.CapsuleOID, &x.CapsuleRef, &x.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

// SubmoduleRecoveryRefExpectations は 1 repository のローカル module に載っているはずの capsule ref を返す。
// 親の RecoveryRefExpectations と違い in-flight の区別を持たない。ref の公開は SNAPSHOT job が
// DB 行を書いた後に行うため、行があって ref が無い時点で既に取りこぼしである。
func (s *Store) SubmoduleRecoveryRefExpectations(ctx context.Context, repositoryID string) ([]SubmoduleRecoveryRef, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT ss.repository_id,ss.name,ss.path,ss.capsule_recovery_ref,ss.capsule_oid,ss.session_id,sn.expires_at
		FROM submodule_snapshots ss JOIN snapshots sn ON sn.session_id=ss.session_id AND sn.repository_id=ss.repository_id
		WHERE ss.repository_id=? AND sn.status='ARCHIVED' ORDER BY ss.name,ss.capsule_recovery_ref`, repositoryID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SubmoduleRecoveryRef
	for rows.Next() {
		var x SubmoduleRecoveryRef
		if err := rows.Scan(&x.RepositoryID, &x.Name, &x.Path, &x.Ref, &x.OID, &x.SessionID, &x.ExpiresAt); err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}
