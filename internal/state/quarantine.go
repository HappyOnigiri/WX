package state

import (
	"context"
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
