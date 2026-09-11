package state

import (
	"context"
	"errors"
)

type SlotArtifact struct{ ID, Path, State string }

func (s *Store) SlotArtifacts(ctx context.Context) ([]SlotArtifact, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT sl.id,rt.path||'/'||sl.rel_path,sl.state FROM slots sl JOIN roots rt ON rt.id=sl.root_id ORDER BY 2`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var artifacts []SlotArtifact
	for rows.Next() {
		var artifact SlotArtifact
		if err := rows.Scan(&artifact.ID, &artifact.Path, &artifact.State); err != nil {
			return nil, err
		}
		artifacts = append(artifacts, artifact)
	}
	return artifacts, rows.Err()
}

func (s *Store) QuarantineMissingSlot(ctx context.Context, id, reason string) error {
	return s.SetSlotState(ctx, id, []string{"ALLOCATING", "REGISTERING", "PREPARING", "READY", "LEASED", "DRAINING", "SNAPSHOTTING", "SNAPSHOTTED", "UNBOUND", "RESTORING", "RETIRING", "REMOVING", "FAILED", "STALE"}, "QUARANTINED", reason)
}

// QuarantineArtifact は隔離記録を追加または更新し、今回はじめて記録したときだけ inserted=true を返す。
// detected_at は初回検出時刻を保つ（更新しない）ので、呼び出し側は毎周の再検出を新規と区別して警告を抑制できる。
func (s *Store) QuarantineArtifact(ctx context.Context, kind, path, reason string) (bool, error) {
	s.writer.Lock()
	defer s.writer.Unlock()
	at := now()
	var detectedAt string
	err := s.db.QueryRowContext(ctx, `INSERT INTO quarantined_artifacts(path,kind,reason,detected_at) VALUES(?,?,?,?) ON CONFLICT(path) DO UPDATE SET kind=excluded.kind,reason=excluded.reason RETURNING detected_at`, path, kind, reason, at).Scan(&detectedAt)
	if err != nil {
		return false, err
	}
	return detectedAt == at, nil
}

// PruneQuarantinedArtifacts は kinds の記録のうち present に無い path を消す。
// 再検出され続けるカテゴリだけを渡すこと。reconcile が再検出しない kind を含めると、消えてはいけない記録まで消える。
func (s *Store) PruneQuarantinedArtifacts(ctx context.Context, kinds []string, present map[string]bool) error {
	if len(kinds) == 0 {
		return nil
	}
	s.writer.Lock()
	defer s.writer.Unlock()
	query := `SELECT path FROM quarantined_artifacts WHERE kind IN (` + placeholders(len(kinds)) + `)`
	args := make([]any, 0, len(kinds))
	for _, kind := range kinds {
		args = append(args, kind)
	}
	stale, err := s.stalePaths(ctx, query, args, present)
	if err != nil {
		return err
	}
	for _, path := range stale {
		if _, err := s.db.ExecContext(ctx, `DELETE FROM quarantined_artifacts WHERE path=?`, path); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) stalePaths(ctx context.Context, query string, args []any, present map[string]bool) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var stale []string
	for rows.Next() {
		var path string
		if err := rows.Scan(&path); err != nil {
			return nil, err
		}
		if !present[path] {
			stale = append(stale, path)
		}
	}
	return stale, rows.Err()
}

// ForgetQuarantinedArtifact は解消済みの隔離記録を消す。存在しない path は成功として扱う。
func (s *Store) ForgetQuarantinedArtifact(ctx context.Context, path string) error {
	s.writer.Lock()
	defer s.writer.Unlock()
	_, err := s.db.ExecContext(ctx, `DELETE FROM quarantined_artifacts WHERE path=?`, path)
	return err
}

func (s *Store) QuarantineMissingRecoveryRef(ctx context.Context, ref string) error {
	s.writer.Lock()
	defer s.writer.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT DISTINCT session_id FROM snapshots WHERE head_recovery_ref=? OR worktree_recovery_ref=? OR index_recovery_ref=?`, ref, ref, ref)
	if err != nil {
		return err
	}
	defer rows.Close()
	var sessions []string
	for rows.Next() {
		var sessionID string
		if err := rows.Scan(&sessionID); err != nil {
			return err
		}
		sessions = append(sessions, sessionID)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE snapshots SET status='QUARANTINED' WHERE head_recovery_ref=? OR worktree_recovery_ref=? OR index_recovery_ref=?`, ref, ref, ref); err != nil {
		return err
	}
	for _, sessionID := range sessions {
		if _, err := tx.ExecContext(ctx, `UPDATE sessions SET state='QUARANTINED' WHERE id=? AND state<>'EXPIRED'`, sessionID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE slots SET state='QUARANTINED',failure_code='RECOVERY_REF_MISSING',updated_at=? WHERE id=(SELECT slot_id FROM sessions WHERE id=?) AND state<>'ARCHIVED'`, now(), sessionID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// QuarantinedRecovery は recovery ref を失って隔離された session 1 件の復元資産である。
// Slot* は session が最後に使った slot で、既に回収済みなら空になる。
type QuarantinedRecovery struct {
	SessionID          string
	SlotID             string
	SlotState          string
	SlotPath           string
	Snapshots          int
	WorkspaceSnapshots int
}

// QuarantinedRecoverySessions は workspace root に属する QUARANTINED の session を返す。
// sessions.state='QUARANTINED' を書くのは QuarantineMissingRecoveryRef だけなので、
// この抽出は recovery ref の欠損で行き止まりになった session だけを拾う。
func (s *Store) QuarantinedRecoverySessions(ctx context.Context, root string) ([]QuarantinedRecovery, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT se.id,COALESCE(se.slot_id,''),COALESCE(sl.state,''),COALESCE(rt.path||'/'||sl.rel_path,''),
		  (SELECT count(*) FROM snapshots sn WHERE sn.session_id=se.id),
		  (SELECT count(*) FROM workspace_snapshots ws WHERE ws.session_id=se.id)
		FROM sessions se JOIN workspaces w ON w.id=se.workspace_id
		LEFT JOIN slots sl ON sl.id=se.slot_id
		LEFT JOIN roots rt ON rt.id=sl.root_id
		WHERE w.root_path=? AND se.state='QUARANTINED' ORDER BY se.id`, root)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []QuarantinedRecovery
	for rows.Next() {
		var item QuarantinedRecovery
		if err := rows.Scan(&item.SessionID, &item.SlotID, &item.SlotState, &item.SlotPath, &item.Snapshots, &item.WorkspaceSnapshots); err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

// DiscardQuarantinedRecovery は QUARANTINED の session の復元資産を捨て、session を EXPIRED で終端させる。
// 記録された recovery ref がソースリポジトリに無いためこの snapshot からは復元できず、ここで失う復元手段は無い。
// slot は QUARANTINED のまま owner だけ外し、worktree の削除は呼び出し側の回収経路に委ねる。
func (s *Store) DiscardQuarantinedRecovery(ctx context.Context, sessionID string) error {
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
		return errors.New("quarantined recovery state has an active restore job")
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM snapshots WHERE session_id=?`, sessionID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM workspace_snapshots WHERE session_id=?`, sessionID); err != nil {
		return err
	}
	t := now()
	res, err := tx.ExecContext(ctx, `UPDATE sessions SET state='EXPIRED',pending_agent_session_id=NULL,released_at=COALESCE(released_at,?),archived_at=COALESCE(archived_at,?),expires_at=? WHERE id=? AND state='QUARANTINED'`, t, t, t, sessionID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return errors.New("session is not quarantined recovery state")
	}
	if _, err := tx.ExecContext(ctx, `UPDATE slots SET owner_session_id=NULL,updated_at=? WHERE id=(SELECT slot_id FROM sessions WHERE id=?) AND owner_session_id=? AND state='QUARANTINED'`, t, sessionID, sessionID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO events(time,level,kind,workspace_id,slot_id,session_id,message) SELECT ?,'warn','session_expired',workspace_id,slot_id,id,? FROM sessions WHERE id=?`,
		t, "quarantined recovery state discarded", sessionID); err != nil {
		return err
	}
	return tx.Commit()
}

// QuarantinedRecoveryGroup は workspace 1 つ分の、隔離された復元資産の件数である。
type QuarantinedRecoveryGroup struct {
	Root      string
	Sessions  int
	Snapshots int
}

// QuarantinedRecoveryGroups は recovery ref を失って隔離された session を workspace ごとに数える。
// 隔離した時点で doctor の ref 照合からは外れるため、診断はこの記録を根拠にする。
func (s *Store) QuarantinedRecoveryGroups(ctx context.Context) ([]QuarantinedRecoveryGroup, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT w.root_path,count(*),COALESCE(sum((SELECT count(*) FROM snapshots sn WHERE sn.session_id=se.id)),0)
		FROM sessions se JOIN workspaces w ON w.id=se.workspace_id
		WHERE se.state='QUARANTINED' GROUP BY w.root_path ORDER BY w.root_path`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []QuarantinedRecoveryGroup
	for rows.Next() {
		var group QuarantinedRecoveryGroup
		if err := rows.Scan(&group.Root, &group.Sessions, &group.Snapshots); err != nil {
			return nil, err
		}
		out = append(out, group)
	}
	return out, rows.Err()
}
