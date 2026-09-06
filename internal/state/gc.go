package state

import (
	"context"
	"fmt"
)

type GCCandidate struct{ SlotID, SessionID, Path string }

type StandbyGCCandidate struct{ SlotID, WorkspaceID, Path, State string }

// HotRepositoryIDs は hot_standby window、つまり hotBefore より後に lease された repository を返す。
// last_leased_at をまだ書いていない進行中の貸出も hot に含める。使用中の repository を cold と判定すると、補充が COLD の待機枠を作るためである。
// 一度も lease されておらず進行中の貸出も無い repository は、実際の lease 前に standby を先読み作成しないよう除外する。
func (s *Store) HotRepositoryIDs(ctx context.Context, hotBefore string) (map[string]bool, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT r.id FROM repositories r WHERE (r.last_leased_at IS NOT NULL AND r.last_leased_at>?) OR EXISTS (SELECT 1 FROM slot_repositories sr JOIN slots sl ON sl.id=sr.slot_id WHERE sr.repository_id=r.id AND sl.owner_session_id IS NOT NULL AND sl.state IN ('ALLOCATING','REGISTERING','PREPARING','RESTORING','LEASED'))`, hotBefore)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}

type ColdRepositoryCandidate struct{ SlotID, WorkspaceID, RepositoryID, WorktreePath string }

func (s *Store) ColdRepositoryCandidates(ctx context.Context, hotBefore string) ([]ColdRepositoryCandidate, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT sl.id,sl.workspace_id,sr.repository_id,rt.path||'/'||sl.rel_path||'/'||sr.dir_name FROM slots sl JOIN roots rt ON rt.id=sl.root_id JOIN slot_repositories sr ON sr.slot_id=sl.id JOIN repositories r ON r.id=sr.repository_id WHERE sl.owner_session_id IS NULL AND sl.state='READY' AND sr.state='READY' AND (r.last_leased_at IS NULL OR r.last_leased_at<=?) ORDER BY sl.id,sr.repository_id`, hotBefore)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ColdRepositoryCandidate
	for rows.Next() {
		var candidate ColdRepositoryCandidate
		if err := rows.Scan(&candidate.SlotID, &candidate.WorkspaceID, &candidate.RepositoryID, &candidate.WorktreePath); err != nil {
			return nil, err
		}
		out = append(out, candidate)
	}
	return out, rows.Err()
}

func (s *Store) ScheduleColdRepositoryRemoval(ctx context.Context, candidate ColdRepositoryCandidate) (Job, bool, error) {
	job, err := newJob("REMOVE_REPOSITORY", candidate.WorkspaceID, candidate.SlotID, "")
	if err != nil {
		return Job{}, false, err
	}
	job.RepositoryID = candidate.RepositoryID
	s.writer.Lock()
	defer s.writer.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Job{}, false, err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `UPDATE slot_repositories SET state='RETIRING' WHERE slot_id=? AND repository_id=? AND state='READY' AND EXISTS (SELECT 1 FROM slots WHERE id=? AND owner_session_id IS NULL AND state IN ('READY','RETIRING'))`, candidate.SlotID, candidate.RepositoryID, candidate.SlotID)
	if err != nil {
		return Job{}, false, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return Job{}, false, nil
	}
	if _, err := tx.ExecContext(ctx, `UPDATE slots SET state='RETIRING',updated_at=? WHERE id=? AND state IN ('READY','RETIRING')`, now(), candidate.SlotID); err != nil {
		return Job{}, false, err
	}
	if err := insertJob(ctx, tx, job); err != nil {
		return Job{}, false, err
	}
	return job, true, tx.Commit()
}

func (s *Store) FinishColdRepositoryRemoval(ctx context.Context, slotID, repositoryID string) error {
	s.writer.Lock()
	defer s.writer.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `UPDATE slot_repositories SET state='COLD' WHERE slot_id=? AND repository_id=? AND state IN ('RETIRING','COLD')`, slotID, repositoryID); err != nil {
		return err
	}
	var remaining int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM slot_repositories WHERE slot_id=? AND state='RETIRING'`, slotID).Scan(&remaining); err != nil {
		return err
	}
	if remaining == 0 {
		if _, err := tx.ExecContext(ctx, `UPDATE slots SET state='READY',updated_at=? WHERE id=? AND state='RETIRING'`, now(), slotID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) StandbyGCCandidates(ctx context.Context, hotBefore string, warm int) ([]StandbyGCCandidate, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT sl.id,sl.workspace_id,rt.path||'/'||sl.rel_path,sl.state,COALESCE(sl.ready_at,sl.created_at) FROM slots sl JOIN roots rt ON rt.id=sl.root_id WHERE sl.owner_session_id IS NULL AND sl.state IN ('READY','STALE') ORDER BY sl.workspace_id,COALESCE(sl.ready_at,sl.created_at) DESC,sl.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	kept := map[string]int{}
	var out []StandbyGCCandidate
	for rows.Next() {
		var candidate StandbyGCCandidate
		var readyAt string
		if err := rows.Scan(&candidate.SlotID, &candidate.WorkspaceID, &candidate.Path, &candidate.State, &readyAt); err != nil {
			return nil, err
		}
		if candidate.State == "STALE" || kept[candidate.WorkspaceID] >= warm {
			out = append(out, candidate)
			continue
		}
		kept[candidate.WorkspaceID]++
	}
	return out, rows.Err()
}

func (s *Store) ScheduleRemoval(ctx context.Context, slotID, sessionID string) (Job, bool, error) {
	job, err := newJob("REMOVE", "", slotID, sessionID)
	if err != nil {
		return Job{}, false, err
	}
	s.writer.Lock()
	defer s.writer.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Job{}, false, err
	}
	defer tx.Rollback()
	var workspaceID string
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(workspace_id,'') FROM slots WHERE id=?`, slotID).Scan(&workspaceID); err != nil {
		return Job{}, false, err
	}
	job.WorkspaceID = workspaceID
	res, err := tx.ExecContext(ctx, `UPDATE slots SET state='REMOVING',owner_session_id=NULL,updated_at=? WHERE id=? AND ((owner_session_id IS NULL AND state IN ('READY','STALE')) OR state='SNAPSHOTTED')`, now(), slotID)
	if err != nil {
		return Job{}, false, err
	}
	changed, _ := res.RowsAffected()
	if changed == 0 {
		return Job{}, false, nil
	}
	if err := insertJob(ctx, tx, job); err != nil {
		return Job{}, false, err
	}
	return job, true, tx.Commit()
}

func (s *Store) FinishRemoval(ctx context.Context, slotID string) error {
	s.writer.Lock()
	defer s.writer.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	t := now()
	res, err := tx.ExecContext(ctx, `UPDATE slots SET state='ARCHIVED',updated_at=?,failure_code=NULL,failure_detail_path=NULL WHERE id=? AND state IN ('REMOVING','ARCHIVED')`, t, slotID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return fmt.Errorf("slot %s state compare-and-swap failed", slotID)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO events(time,level,kind,workspace_id,slot_id,message) SELECT ?,'info','slot_transition',workspace_id,id,? FROM slots WHERE id=?`, t, "state=ARCHIVED failure_code=", slotID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM standby_replenish_exclusions WHERE slot_id=?`, slotID); err != nil {
		return err
	}
	return tx.Commit()
}

// FailedSlotIDs は workspace に属する未所有の FAILED slot を返す。
func (s *Store) FailedSlotIDs(ctx context.Context, workspaceID string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM slots WHERE workspace_id=? AND owner_session_id IS NULL AND state='FAILED'`, workspaceID)
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

// ScheduleFailedSlotRemoval は FAILED slot の物理 worktree を retired にして row を ARCHIVED へ進める。
// ScheduleRemoval（READY/STALE/SNAPSHOTTED）とは別に、ForgetWorkspace が非 ARCHIVED slot を拒否しても
// 回収不能な FAILED worktree を残さないために用意する。FAILED を安全済みと扱えない理由は ForgetWorkspace を参照する。
func (s *Store) ScheduleFailedSlotRemoval(ctx context.Context, slotID string) (Job, bool, error) {
	job, err := newJob("REMOVE", "", slotID, "")
	if err != nil {
		return Job{}, false, err
	}
	s.writer.Lock()
	defer s.writer.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Job{}, false, err
	}
	defer tx.Rollback()
	var workspaceID string
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(workspace_id,'') FROM slots WHERE id=?`, slotID).Scan(&workspaceID); err != nil {
		return Job{}, false, err
	}
	job.WorkspaceID = workspaceID
	res, err := tx.ExecContext(ctx, `UPDATE slots SET state='REMOVING',owner_session_id=NULL,updated_at=? WHERE id=? AND owner_session_id IS NULL AND state='FAILED'`, now(), slotID)
	if err != nil {
		return Job{}, false, err
	}
	changed, _ := res.RowsAffected()
	if changed == 0 {
		return Job{}, false, nil
	}
	if err := insertJob(ctx, tx, job); err != nil {
		return Job{}, false, err
	}
	return job, true, tx.Commit()
}

// CountMetadataCandidates は指定 threshold で PruneMetadata が remove または tombstone する row 数を、変更せずに返す。
// `wx gc --dry-run` を支え、報告値に TTL 切れ event と完了 job metadata を含めて最高の GC priority tier と一致させる。
func (s *Store) CountMetadataCandidates(ctx context.Context, failedBefore, eventBefore, tombstoneBefore string) (int, error) {
	var jobs, events, tombstones, idempotency int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM jobs WHERE (state='SUCCEEDED' OR state='FAILED') AND COALESCE(finished_at,started_at,not_before)<=?`, failedBefore).Scan(&jobs); err != nil {
		return 0, err
	}
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM events WHERE time<=?`, eventBefore).Scan(&events); err != nil {
		return 0, err
	}
	// PruneMetadata は agent_session_id を消して tombstone 化するため、処理済み session は候補ではない。
	// 数えると `wx gc --dry-run` が何も変わらない同じ作業を報告し続ける。
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sessions WHERE state='EXPIRED' AND expires_at<=? AND agent_session_id IS NOT NULL`, tombstoneBefore).Scan(&tombstones); err != nil {
		return 0, err
	}
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM rpc_idempotency WHERE expires_at<=?`, now()).Scan(&idempotency); err != nil {
		return 0, err
	}
	return jobs + events + tombstones + idempotency, nil
}

func (s *Store) PruneMetadata(ctx context.Context, failedBefore, eventBefore, tombstoneBefore string) error {
	s.writer.Lock()
	defer s.writer.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM jobs WHERE (state='SUCCEEDED' OR state='FAILED') AND COALESCE(finished_at,started_at,not_before)<=?`, failedBefore); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM events WHERE time<=?`, eventBefore); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE sessions SET agent_session_id=NULL WHERE state='EXPIRED' AND expires_at<=?`, tombstoneBefore); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM rpc_idempotency WHERE expires_at<=?`, now()); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) GCCandidates(ctx context.Context, before string) ([]GCCandidate, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT sl.id,se.id,rt.path||'/'||sl.rel_path FROM slots sl JOIN roots rt ON rt.id=sl.root_id JOIN sessions se ON se.slot_id=sl.id WHERE sl.state='SNAPSHOTTED' AND se.archived_at<=?`, before)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []GCCandidate
	for rows.Next() {
		var x GCCandidate
		if err := rows.Scan(&x.SlotID, &x.SessionID, &x.Path); err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

// QuarantinedGCCandidate は retention を過ぎた隔離 slot の削除候補である。
type QuarantinedGCCandidate struct{ SlotID, Path, FailureCode string }

// QuarantinedGCCandidates は保持期限を過ぎた隔離・失敗 slot を返す。
// owner や実行中 job が残る行は処理が終わるまで候補にしない。
func (s *Store) QuarantinedGCCandidates(ctx context.Context, before string) ([]QuarantinedGCCandidate, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT sl.id,rt.path||'/'||sl.rel_path,COALESCE(sl.failure_code,'')
		FROM slots sl JOIN roots rt ON rt.id=sl.root_id
		WHERE sl.state IN ('QUARANTINED','FAILED') AND sl.owner_session_id IS NULL AND sl.updated_at<=? AND NOT EXISTS (SELECT 1 FROM jobs j WHERE j.slot_id=sl.id AND j.state='RUNNING') ORDER BY sl.id`, before)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []QuarantinedGCCandidate
	for rows.Next() {
		var candidate QuarantinedGCCandidate
		if err := rows.Scan(&candidate.SlotID, &candidate.Path, &candidate.FailureCode); err != nil {
			return nil, err
		}
		out = append(out, candidate)
	}
	return out, rows.Err()
}

// ScheduleQuarantinedRemoval は隔離 slot を REMOVING へ移し、REMOVE job を予約する。
// 過去の待機 job は取り消し、DB 登録範囲の回収を再試行する。
func (s *Store) ScheduleQuarantinedRemoval(ctx context.Context, slotID string) (Job, bool, error) {
	job, err := newJob("REMOVE", "", slotID, "")
	if err != nil {
		return Job{}, false, err
	}
	s.writer.Lock()
	defer s.writer.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Job{}, false, err
	}
	defer tx.Rollback()
	var workspaceID string
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(workspace_id,'') FROM slots WHERE id=?`, slotID).Scan(&workspaceID); err != nil {
		return Job{}, false, err
	}
	job.WorkspaceID = workspaceID
	res, err := tx.ExecContext(ctx, `UPDATE slots SET state='REMOVING',updated_at=? WHERE id=? AND owner_session_id IS NULL AND state IN ('QUARANTINED','FAILED') AND NOT EXISTS (SELECT 1 FROM jobs j WHERE j.slot_id=slots.id AND j.state='RUNNING')`, now(), slotID)
	if err != nil {
		return Job{}, false, err
	}
	if changed, _ := res.RowsAffected(); changed == 0 {
		return Job{}, false, nil
	}
	if _, err := tx.ExecContext(ctx, `UPDATE jobs SET state='FAILED',finished_at=?,error_code='SUPERSEDED_BY_REMOVAL' WHERE slot_id=? AND state='PENDING'`, now(), slotID); err != nil {
		return Job{}, false, err
	}
	if err := insertJob(ctx, tx, job); err != nil {
		return Job{}, false, err
	}
	return job, true, tx.Commit()
}

func (s *Store) MarkSlotArchived(ctx context.Context, id string) error {
	return s.SetSlotState(ctx, id, []string{"SNAPSHOTTED"}, "ARCHIVED", "")
}
