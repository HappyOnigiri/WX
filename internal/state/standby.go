package state

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// standbyQuery は待機枠を数える SQL。READY へ戻らない QUARANTINED と REMOVING は数えない。隔離を数えると補充が恒久的に止まる。
// 削除中を数えると返却直後の枠が削除の完了まで埋まり、その間に走った補充の確認が不足なしと判断して、次の reconcile まで枠が欠ける。
// 完了後に READY へ戻る RETIRING とリトライ中の FAILED は枠に残し、通常セッション成功時点で記録された除外だけを計算から外す。
const standbyQuery = `SELECT count(*) FROM slots sl JOIN workspaces w ON w.id=sl.workspace_id
	WHERE sl.workspace_id=? AND sl.generation=w.generation AND sl.owner_session_id IS NULL
	AND sl.state IN ('ALLOCATING','REGISTERING','PREPARING','READY','FAILED','RETIRING')
	AND (sl.state<>'FAILED' OR NOT EXISTS (
		SELECT 1 FROM standby_replenish_exclusions ex
		WHERE ex.slot_id=sl.id AND ex.workspace_id=sl.workspace_id AND ex.generation=sl.generation
	))`

// standbyCountTx は現行 generation の待機枠を writer transaction 内で数える。
func standbyCountTx(ctx context.Context, tx *sql.Tx, workspaceID string) (int, error) {
	var n int
	err := tx.QueryRowContext(ctx, standbyQuery, workspaceID).Scan(&n)
	return n, err
}

func (s *Store) StandbyCount(ctx context.Context, workspaceID string) int {
	var n int
	if err := s.db.QueryRowContext(ctx, standbyQuery, workspaceID).Scan(&n); err != nil {
		return 0
	}
	return n
}

// ensureStandbyJobTx は workspace の ENSURE_STANDBY を1件だけ確保し、今回新しく登録したかを返す。
// 既に PENDING・RUNNING があるときはそれを返す。補充の確認は不足を数え直すだけなので、同じ確認を積み増しても意味がない。
func ensureStandbyJobTx(ctx context.Context, tx *sql.Tx, workspaceID string) (Job, bool, error) {
	var job Job
	err := tx.QueryRowContext(ctx, `SELECT id,state FROM jobs WHERE kind='ENSURE_STANDBY' AND workspace_id=? AND state IN ('PENDING','RUNNING')
		ORDER BY CASE state WHEN 'PENDING' THEN 0 ELSE 1 END,id LIMIT 1`, workspaceID).Scan(&job.ID, &job.State)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		job, err = newJob("ENSURE_STANDBY", workspaceID, "", "")
		if err != nil {
			return Job{}, false, err
		}
		if err := insertJob(ctx, tx, job); err != nil {
			return Job{}, false, err
		}
		return job, true, nil
	case err != nil:
		return Job{}, false, err
	default:
		job.Kind = "ENSURE_STANDBY"
		job.WorkspaceID = workspaceID
		return job, false, nil
	}
}

// StandbyReplenishmentRetry は補充停止の手動解除結果と、再補充を促す job を返す。
// Suspended は解除前に停止が記録されていたかを表し、slot の削除や状態変更は行わない。
type StandbyReplenishmentRetry struct {
	Generation int
	Suspended  bool
	Job        Job
}

// RetryStandbyReplenishment は補充停止を解除し、ENSURE_STANDBY を一度だけ予約する。
// 停止理由（clean 由来か standby 失敗か）では区別せず、隔離 slot の状態と実体には触れない。
func (s *Store) RetryStandbyReplenishment(ctx context.Context, workspaceID string) (StandbyReplenishmentRetry, error) {
	s.writer.Lock()
	defer s.writer.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return StandbyReplenishmentRetry{}, err
	}
	defer tx.Rollback()
	if err := assertNoActiveClean(ctx, tx); err != nil {
		return StandbyReplenishmentRetry{}, err
	}
	var generation int
	if err := tx.QueryRowContext(ctx, `SELECT generation FROM workspaces WHERE id=?`, workspaceID).Scan(&generation); err != nil {
		return StandbyReplenishmentRetry{}, err
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM replenish_suspensions WHERE workspace_id=?`, workspaceID)
	if err != nil {
		return StandbyReplenishmentRetry{}, err
	}
	removed, _ := res.RowsAffected()
	job, _, err := ensureStandbyJobTx(ctx, tx, workspaceID)
	if err != nil {
		return StandbyReplenishmentRetry{}, err
	}
	if err := tx.Commit(); err != nil {
		return StandbyReplenishmentRetry{}, err
	}
	return StandbyReplenishmentRetry{Generation: generation, Suspended: removed > 0, Job: job}, nil
}

// recordStandbySuccessTx は通常 session の準備成功を一度だけ記録し、
// その時点で既に終了している FAILED の待機失敗だけを同じ transaction で除外する（QUARANTINED は枠に数えないので対象外）。
// ensureAfterSuccess は READY 貸出や起動回収のように、失敗が無くても補充を再確認する経路で指定する。
func recordStandbySuccessTx(ctx context.Context, tx *sql.Tx, sessionID string, ensureAfterSuccess bool) (Job, bool, bool, error) {
	var workspaceID string
	var generation int
	var sessionState string
	err := tx.QueryRowContext(ctx, `SELECT se.workspace_id,sl.generation,se.state
		FROM sessions se JOIN slots sl ON sl.id=se.slot_id JOIN workspaces w ON w.id=se.workspace_id
		WHERE se.id=? AND sl.workspace_id=se.workspace_id AND sl.owner_session_id=se.id
		  AND sl.state='LEASED' AND sl.generation=w.generation
		  AND se.state IN ('STARTING','ACTIVE')
		  AND NOT EXISTS (SELECT 1 FROM jobs j WHERE j.session_id=se.id AND j.kind='RESTORE')
		  AND (se.parent_session_id IS NULL OR EXISTS (SELECT 1 FROM sessions parent WHERE parent.id=se.parent_session_id AND parent.state='EXPIRED'))`, sessionID).
		Scan(&workspaceID, &generation, &sessionState)
	if errors.Is(err, sql.ErrNoRows) {
		return Job{}, false, false, nil
	}
	if err != nil {
		return Job{}, false, false, err
	}
	if sessionState != "STARTING" && sessionState != "ACTIVE" {
		return Job{}, false, false, nil
	}
	t := now()
	res, err := tx.ExecContext(ctx, `INSERT INTO standby_replenish_successes(session_id,workspace_id,recorded_at) VALUES(?,?,?) ON CONFLICT(session_id) DO NOTHING`, sessionID, workspaceID, t)
	if err != nil {
		return Job{}, false, false, err
	}
	inserted, _ := res.RowsAffected()
	if inserted != 1 {
		return Job{}, false, false, nil
	}
	res, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO standby_replenish_exclusions(slot_id,workspace_id,generation,success_session_id,excluded_at)
		SELECT sl.id,sl.workspace_id,sl.generation,?,?
		FROM slots sl JOIN workspaces w ON w.id=sl.workspace_id
		WHERE sl.workspace_id=? AND sl.generation=w.generation AND sl.owner_session_id IS NULL
		  AND sl.state='FAILED'
		  AND NOT EXISTS (SELECT 1 FROM jobs j WHERE j.slot_id=sl.id AND j.state IN ('PENDING','RUNNING'))`, sessionID, t, workspaceID)
	if err != nil {
		return Job{}, false, false, err
	}
	excluded, _ := res.RowsAffected()
	if excluded == 0 && !ensureAfterSuccess {
		return Job{}, false, true, nil
	}
	job, err := newJob("ENSURE_STANDBY", workspaceID, "", "")
	if err != nil {
		return Job{}, false, false, err
	}
	if err := insertJob(ctx, tx, job); err != nil {
		return Job{}, false, false, err
	}
	return job, true, true, nil
}

// RecordStandbySuccess は、既に LEASED になった通常 session を起点に補充許可を回収する。
// 同じ session を再度渡しても、新しい失敗 slotを同じ成功で許可しない。
func (s *Store) RecordStandbySuccess(ctx context.Context, sessionID string) (Job, bool, error) {
	s.writer.Lock()
	defer s.writer.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Job{}, false, err
	}
	defer tx.Rollback()
	job, created, _, err := recordStandbySuccessTx(ctx, tx, sessionID, true)
	if err != nil {
		return Job{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return Job{}, false, err
	}
	return job, created, nil
}

// RecoverStandbyReplenishments は起動・定期 reconcile 時点で未記録の通常成功を一度だけ回収する。
// 終了済み session と復元 session は対象にしない。
func (s *Store) RecoverStandbyReplenishments(ctx context.Context) ([]Job, error) {
	s.writer.Lock()
	defer s.writer.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT se.id FROM sessions se JOIN slots sl ON sl.id=se.slot_id JOIN workspaces w ON w.id=se.workspace_id
		WHERE sl.workspace_id=se.workspace_id AND sl.owner_session_id=se.id AND sl.state='LEASED'
		  AND sl.generation=w.generation AND se.state IN ('STARTING','ACTIVE')
		  AND NOT EXISTS (SELECT 1 FROM jobs j WHERE j.session_id=se.id AND j.kind='RESTORE')
		  AND (se.parent_session_id IS NULL OR EXISTS (SELECT 1 FROM sessions parent WHERE parent.id=se.parent_session_id AND parent.state='EXPIRED')) ORDER BY se.id`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var sessionIDs []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return nil, err
		}
		sessionIDs = append(sessionIDs, id)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var jobs []Job
	for _, sessionID := range sessionIDs {
		job, created, _, err := recordStandbySuccessTx(ctx, tx, sessionID, true)
		if err != nil {
			return nil, err
		}
		if created {
			jobs = append(jobs, job)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return jobs, nil
}

// ReserveStandbyIfNeeded は待機枠の再確認と物理作成前の slot 予約を同じ transaction で行う。
// 予約中の slot も待機枠に数えるため、並行した補充が上限を越えない。
func (s *Store) ReserveStandbyIfNeeded(ctx context.Context, slot Slot, limit int) (bool, error) {
	if limit <= 0 {
		return false, nil
	}
	s.writer.Lock()
	defer s.writer.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	if err := assertNoActiveClean(ctx, tx); err != nil {
		return false, err
	}
	var currentGeneration int
	if err := tx.QueryRowContext(ctx, `SELECT generation FROM workspaces WHERE id=?`, slot.WorkspaceID).Scan(&currentGeneration); err != nil {
		return false, err
	}
	if currentGeneration != slot.Generation {
		if err := tx.Commit(); err != nil {
			return false, err
		}
		return false, nil
	}
	count, err := standbyCountTx(ctx, tx, slot.WorkspaceID)
	if err != nil {
		return false, err
	}
	if count >= limit {
		if err := tx.Commit(); err != nil {
			return false, err
		}
		return false, nil
	}
	if err := insertReservedSlotTx(ctx, tx, slot); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}

// RegisterReservedStandby は identity を確定した予約 slot に待機用 repository と job を登録する。
func (s *Store) RegisterReservedStandby(ctx context.Context, slotID string, repos []SlotRepository) (Job, error) {
	job, err := newJob("PREPARE", "", slotID, "")
	if err != nil {
		return Job{}, err
	}
	s.writer.Lock()
	defer s.writer.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Job{}, err
	}
	defer tx.Rollback()
	if err := assertNoActiveClean(ctx, tx); err != nil {
		return Job{}, err
	}
	var workspaceID string
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(workspace_id,'') FROM slots WHERE id=?`, slotID).Scan(&workspaceID); err != nil {
		return Job{}, err
	}
	job.WorkspaceID = workspaceID
	for _, r := range repos {
		if _, err := tx.ExecContext(ctx, `INSERT INTO slot_repositories(slot_id,repository_id,dir_name,state,requested_ref,base_oid,prepare_fingerprint,compatibility_fingerprint) VALUES(?,?,?,?,?,?,?,?)`, slotID, r.RepositoryID, r.DirName, r.State, r.RequestedRef, r.BaseOID, r.Fingerprint, r.CompatibilityFingerprint); err != nil {
			return Job{}, err
		}
	}
	t := now()
	res, err := tx.ExecContext(ctx, `UPDATE slots SET state='PREPARING',updated_at=? WHERE id=? AND state='REGISTERING' AND dir_identity IS NOT NULL AND owner_session_id IS NULL`, t, slotID)
	if err != nil {
		return Job{}, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return Job{}, fmt.Errorf("slot %s standby registration compare-and-swap failed (%s)", slotID, slotCASDetail(ctx, tx, slotID))
	}
	if err := insertJob(ctx, tx, job); err != nil {
		return Job{}, err
	}
	return job, tx.Commit()
}

func (s *Store) CreateStandby(ctx context.Context, slot Slot, repos []SlotRepository) (Job, error) {
	job, err := newJob("PREPARE", slot.WorkspaceID, slot.ID, "")
	if err != nil {
		return Job{}, err
	}
	s.writer.Lock()
	defer s.writer.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Job{}, err
	}
	defer tx.Rollback()
	if err := assertNoActiveClean(ctx, tx); err != nil {
		return Job{}, err
	}
	if err := insertStandbySlotTx(ctx, tx, slot, repos, job); err != nil {
		return Job{}, err
	}
	return job, tx.Commit()
}

// CreateStandbyIfNeeded は standby 枠の再検証と slot/job 登録を同じ transaction で行う。
// 別の reconcile が先に不足分を埋めた場合や隔離上限に達していた場合は、作成側が物理 directory を所有権確認後に片付けられるよう false を返す。
func (s *Store) CreateStandbyIfNeeded(ctx context.Context, slot Slot, repos []SlotRepository, limit int) (Job, bool, error) {
	if limit <= 0 {
		return Job{}, false, nil
	}
	s.writer.Lock()
	defer s.writer.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Job{}, false, err
	}
	defer tx.Rollback()
	if err := assertNoActiveClean(ctx, tx); err != nil {
		return Job{}, false, err
	}
	var currentGeneration int
	if err := tx.QueryRowContext(ctx, `SELECT generation FROM workspaces WHERE id=?`, slot.WorkspaceID).Scan(&currentGeneration); err != nil {
		return Job{}, false, err
	}
	if currentGeneration != slot.Generation {
		if err := tx.Commit(); err != nil {
			return Job{}, false, err
		}
		return Job{}, false, nil
	}
	count, err := standbyCountTx(ctx, tx, slot.WorkspaceID)
	if err != nil {
		return Job{}, false, err
	}
	if count >= limit {
		if err := tx.Commit(); err != nil {
			return Job{}, false, err
		}
		return Job{}, false, nil
	}
	job, err := newJob("PREPARE", slot.WorkspaceID, slot.ID, "")
	if err != nil {
		return Job{}, false, err
	}
	if err := insertStandbySlotTx(ctx, tx, slot, repos, job); err != nil {
		return Job{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return Job{}, false, err
	}
	return job, true, nil
}

func insertStandbySlotTx(ctx context.Context, tx *sql.Tx, slot Slot, repos []SlotRepository, job Job) error {
	t := now()
	_, err := tx.ExecContext(ctx, `INSERT INTO slots(id,workspace_id,generation,root_id,rel_path,dir_identity,state,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?)`, slot.ID, slot.WorkspaceID, slot.Generation, slot.RootID, slot.RelPath, nullString(slot.DirIdentity), slot.State, t, t)
	if err != nil {
		return err
	}
	for _, r := range repos {
		if _, err = tx.ExecContext(ctx, `INSERT INTO slot_repositories(slot_id,repository_id,dir_name,state,requested_ref,base_oid,prepare_fingerprint,compatibility_fingerprint) VALUES(?,?,?,?,?,?,?,?)`, slot.ID, r.RepositoryID, r.DirName, r.State, r.RequestedRef, r.BaseOID, r.Fingerprint, r.CompatibilityFingerprint); err != nil {
			return err
		}
	}
	if err := insertJob(ctx, tx, job); err != nil {
		return err
	}
	return nil
}
