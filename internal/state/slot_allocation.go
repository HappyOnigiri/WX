package state

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

func (s *Store) CreateSlotSession(ctx context.Context, slot Slot, repos []SlotRepository, session Session, jobKind string) (Job, error) {
	var job Job
	var err error
	if jobKind != "" {
		job, err = newJob(jobKind, slot.WorkspaceID, slot.ID, session.ID)
		if err != nil {
			return Job{}, err
		}
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
	if jobKind == "RESTORE" && session.ParentSessionID != "" {
		var count int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM sessions WHERE parent_session_id=? AND state='RESTORING'`, session.ParentSessionID).Scan(&count); err != nil {
			return Job{}, err
		}
		if count != 0 {
			return Job{}, errors.New("session is already being restored")
		}
	}
	t := now()
	_, err = tx.ExecContext(ctx, `INSERT INTO slots(id,workspace_id,generation,root_id,rel_path,dir_identity,state,owner_session_id,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?)`, slot.ID, nullString(slot.WorkspaceID), slot.Generation, slot.RootID, slot.RelPath, nullString(slot.DirIdentity), slot.State, nullString(session.ID), t, t)
	if err != nil {
		return Job{}, err
	}
	for _, r := range repos {
		_, err = tx.ExecContext(ctx, `INSERT INTO slot_repositories(slot_id,repository_id,dir_name,state,requested_ref,base_oid,prepare_fingerprint,compatibility_fingerprint) VALUES(?,?,?,?,?,?,?,?)`, slot.ID, r.RepositoryID, r.DirName, r.State, r.RequestedRef, r.BaseOID, r.Fingerprint, r.CompatibilityFingerprint)
		if err != nil {
			return Job{}, err
		}
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO sessions(`+sessionInsertColumns+`) VALUES(`+sessionInsertPlaceholders+`)`, sessionInsertArgs(session, t)...)
	if err != nil {
		return Job{}, err
	}
	if jobKind == "RESTORE" && session.ParentSessionID != "" {
		if err := copySessionRepositories(ctx, tx, session.ID, session.ParentSessionID); err != nil {
			return Job{}, err
		}
	} else if session.WorkspaceID != "" {
		if err := insertCurrentSessionRepositories(ctx, tx, session.ID, session.WorkspaceID, session.SlotID); err != nil {
			return Job{}, err
		}
	}
	if jobKind != "" {
		if err := insertJob(ctx, tx, job); err != nil {
			return Job{}, err
		}
	}
	if slot.WorkspaceID != "" {
		if _, err := tx.ExecContext(ctx, `UPDATE repositories SET last_leased_at=? WHERE id IN (SELECT repository_id FROM workspace_repositories WHERE workspace_id=?)`, t, slot.WorkspaceID); err != nil {
			return Job{}, err
		}
	}
	return job, tx.Commit()
}

// ReserveSlot は物理 directory を作る前に slot の位置だけを台帳へ予約する。
// owner_session_id はまだ存在しない session を指すが、待機枠と通常貸出の予約を区別するために使う。
func (s *Store) ReserveSlot(ctx context.Context, slot Slot) error {
	s.writer.Lock()
	defer s.writer.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := assertNoActiveClean(ctx, tx); err != nil {
		return err
	}
	if err := insertReservedSlotTx(ctx, tx, slot); err != nil {
		return err
	}
	return tx.Commit()
}

func insertReservedSlotTx(ctx context.Context, tx *sql.Tx, slot Slot) error {
	t := now()
	_, err := tx.ExecContext(ctx, `INSERT INTO slots(id,workspace_id,generation,root_id,rel_path,dir_identity,state,owner_session_id,created_at,updated_at) VALUES(?,?,?,?,?,NULL,'ALLOCATING',?,?,?)`, slot.ID, nullString(slot.WorkspaceID), slot.Generation, slot.RootID, slot.RelPath, nullString(slot.OwnerSessionID), t, t)
	return err
}

// ConfirmSlotCreation は descriptor から取得した slot directory identity を予約行へ CAS で記録する。
// identity の確定後だけ、session・repository・job の登録へ進める。
func (s *Store) ConfirmSlotCreation(ctx context.Context, id, dirIdentity string) error {
	if dirIdentity == "" {
		return errors.New("slot directory identity is required")
	}
	s.writer.Lock()
	defer s.writer.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	t := now()
	res, err := tx.ExecContext(ctx, `UPDATE slots SET state='REGISTERING',dir_identity=?,updated_at=? WHERE id=? AND state='ALLOCATING' AND dir_identity IS NULL`, dirIdentity, t, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return fmt.Errorf("slot %s creation reservation compare-and-swap failed (%s)", id, slotCASDetail(ctx, tx, id))
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO events(time,level,kind,workspace_id,slot_id,message) SELECT ?,'info','slot_transition',workspace_id,id,? FROM slots WHERE id=?`, t, "state=REGISTERING failure_code=", id)
	return err
}

// RegisterReservedSlotSession は identity を確定した予約 slot に session 一式を登録する。
// いずれかの INSERT が失敗しても予約行は残り、呼び出し側が CAS で隔離できる。
func (s *Store) RegisterReservedSlotSession(ctx context.Context, slotID string, repos []SlotRepository, session Session, slotState, jobKind string) (Job, error) {
	var job Job
	var err error
	if jobKind != "" {
		job, err = newJob(jobKind, session.WorkspaceID, session.SlotID, session.ID)
		if err != nil {
			return Job{}, err
		}
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
	if jobKind == "RESTORE" && session.ParentSessionID != "" {
		var count int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM sessions WHERE parent_session_id=? AND state='RESTORING'`, session.ParentSessionID).Scan(&count); err != nil {
			return Job{}, err
		}
		if count != 0 {
			return Job{}, errors.New("session is already being restored")
		}
	}
	t := now()
	for _, r := range repos {
		if _, err := tx.ExecContext(ctx, `INSERT INTO slot_repositories(slot_id,repository_id,dir_name,state,requested_ref,base_oid,prepare_fingerprint,compatibility_fingerprint) VALUES(?,?,?,?,?,?,?,?)`, slotID, r.RepositoryID, r.DirName, r.State, r.RequestedRef, r.BaseOID, r.Fingerprint, r.CompatibilityFingerprint); err != nil {
			return Job{}, err
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO sessions(`+sessionInsertColumns+`) VALUES(`+sessionInsertPlaceholders+`)`, sessionInsertArgs(session, t)...); err != nil {
		return Job{}, err
	}
	if jobKind == "RESTORE" && session.ParentSessionID != "" {
		if err := copySessionRepositories(ctx, tx, session.ID, session.ParentSessionID); err != nil {
			return Job{}, err
		}
	} else if session.WorkspaceID != "" {
		if err := insertCurrentSessionRepositories(ctx, tx, session.ID, session.WorkspaceID, session.SlotID); err != nil {
			return Job{}, err
		}
	}
	res, err := tx.ExecContext(ctx, `UPDATE slots SET state=?,owner_session_id=?,updated_at=? WHERE id=? AND state='REGISTERING' AND dir_identity IS NOT NULL AND owner_session_id=? AND COALESCE(workspace_id,'')=?`, slotState, session.ID, t, slotID, session.ID, session.WorkspaceID)
	if err != nil {
		return Job{}, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return Job{}, fmt.Errorf("slot %s registration compare-and-swap failed (%s)", slotID, slotCASDetail(ctx, tx, slotID))
	}
	if jobKind != "" {
		if err := insertJob(ctx, tx, job); err != nil {
			return Job{}, err
		}
	}
	if session.WorkspaceID != "" {
		if _, err := tx.ExecContext(ctx, `UPDATE repositories SET last_leased_at=? WHERE id IN (SELECT repository_id FROM workspace_repositories WHERE workspace_id=?)`, t, session.WorkspaceID); err != nil {
			return Job{}, err
		}
	}
	return job, tx.Commit()
}

// QuarantineReservedSlot は作成途中の予約が競合・失敗したときに物理実体を残したまま隔離する。
func (s *Store) QuarantineReservedSlot(ctx context.Context, id, code string) error {
	s.writer.Lock()
	defer s.writer.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	t := now()
	res, err := tx.ExecContext(ctx, `UPDATE slots SET state='QUARANTINED',owner_session_id=NULL,updated_at=?,failure_code=?,failure_detail_path=NULL WHERE id=? AND state IN ('ALLOCATING','REGISTERING')`, t, nullString(code), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return fmt.Errorf("slot %s reservation state compare-and-swap failed", id)
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO events(time,level,kind,workspace_id,slot_id,message) SELECT ?,'warn','slot_transition',workspace_id,id,? FROM slots WHERE id=?`, t, "state=QUARANTINED failure_code="+code, id)
	return err
}

// AbandonSlotReservation は既存 path と衝突した未作成の予約だけを取り消す。
// 他の実体を発見したことを削除権限へ変えないため、隔離 slot として残さない。
func (s *Store) AbandonSlotReservation(ctx context.Context, id string) error {
	s.writer.Lock()
	defer s.writer.Unlock()
	res, err := s.db.ExecContext(ctx, `DELETE FROM slots WHERE id=? AND state='ALLOCATING' AND dir_identity IS NULL`, id)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return fmt.Errorf("slot %s reservation changed", id)
	}
	return nil
}

// slotCASDetail は CAS 失敗時の行の実際の値を返す。予約が別経路で遷移したのかを、後から失敗メッセージだけで判別できるようにする。
func slotCASDetail(ctx context.Context, tx *sql.Tx, id string) string {
	var slotState, failureCode, identity, owner string
	err := tx.QueryRowContext(ctx, `SELECT state,COALESCE(failure_code,''),COALESCE(dir_identity,''),COALESCE(owner_session_id,'') FROM slots WHERE id=?`, id).Scan(&slotState, &failureCode, &identity, &owner)
	if err != nil {
		return fmt.Sprintf("row unavailable: %v", err)
	}
	return fmt.Sprintf("state=%s failure_code=%s dir_identity=%q owner=%q", slotState, failureCode, identity, owner)
}
