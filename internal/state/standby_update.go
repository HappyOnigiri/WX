package state

import (
	"context"
	"errors"
	"fmt"
)

type Placement struct {
	RepositoryID, RelativePath, Kind, SourcePath, ContentSHA256 string
}

func (s *Store) Placements(ctx context.Context, slotID string) ([]Placement, error) {
	return s.readPlacements(ctx, "slot_placements", slotID)
}

func (s *Store) UpdatePlacements(ctx context.Context, slotID string) ([]Placement, error) {
	return s.readPlacements(ctx, "slot_update_placements", slotID)
}

// ReplaceUpdatePlacements は更新先OIDで省略されたlinkを反映し、公開予定の配置履歴を確定する。
func (s *Store) ReplaceUpdatePlacements(ctx context.Context, slotID string, placements []Placement) error {
	s.writer.Lock()
	defer s.writer.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var stateName string
	if err := tx.QueryRowContext(ctx, `SELECT state FROM slots WHERE id=?`, slotID).Scan(&stateName); err != nil {
		return err
	}
	if stateName != "PREPARING" {
		return fmt.Errorf("slot %s cannot finalize update placements from %s", slotID, stateName)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM slot_update_placements WHERE slot_id=?`, slotID); err != nil {
		return err
	}
	for _, placement := range placements {
		if _, err := tx.ExecContext(ctx, `INSERT INTO slot_update_placements(slot_id,repository_id,relative_path,kind,source_path,content_sha256) VALUES(?,?,?,?,?,?)`, slotID, placement.RepositoryID, placement.RelativePath, placement.Kind, placement.SourcePath, placement.ContentSHA256); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) readPlacements(ctx context.Context, table, slotID string) ([]Placement, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT repository_id,relative_path,kind,source_path,content_sha256 FROM `+table+` WHERE slot_id=? ORDER BY repository_id,relative_path`, slotID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Placement
	for rows.Next() {
		var placement Placement
		if err := rows.Scan(&placement.RepositoryID, &placement.RelativePath, &placement.Kind, &placement.SourcePath, &placement.ContentSHA256); err != nil {
			return nil, err
		}
		out = append(out, placement)
	}
	return out, rows.Err()
}

// ReplacePlacements は配置と最終検証が成功した時点の完全な配置履歴を置き換える。
func (s *Store) ReplacePlacements(ctx context.Context, slotID string, placements []Placement) error {
	s.writer.Lock()
	defer s.writer.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var stateName string
	if err := tx.QueryRowContext(ctx, `SELECT state FROM slots WHERE id=?`, slotID).Scan(&stateName); err != nil {
		return err
	}
	if stateName != "PREPARING" && stateName != "RESTORING" {
		return fmt.Errorf("slot %s cannot record placements from %s", slotID, stateName)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM slot_placements WHERE slot_id=?`, slotID); err != nil {
		return err
	}
	for _, placement := range placements {
		if _, err := tx.ExecContext(ctx, `INSERT INTO slot_placements(slot_id,repository_id,relative_path,kind,source_path,content_sha256) VALUES(?,?,?,?,?,?)`, slotID, placement.RepositoryID, placement.RelativePath, placement.Kind, placement.SourcePath, placement.ContentSHA256); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE slots SET placement_history_complete=1,updated_at=? WHERE id=? AND state=?`, now(), slotID, stateName); err != nil {
		return err
	}
	return tx.Commit()
}

// ErrStandbyNotUpdateable は予約の直前に slot が更新可能な READY standby ではなくなったことを示す。
// 併走する貸出や GC に奪われただけなので、呼び出し元は slot の状態を変えず次の候補へ回す合図に使う。
var ErrStandbyNotUpdateable = errors.New("slot is no longer an updateable READY standby")

// ReserveStandbyUpdate は検証済み READY slot の貸出予約と UPDATE job を一つの transaction で作る。
func (s *Store) ReserveStandbyUpdate(ctx context.Context, slotID string, session Session, targets []SlotRepository, placements []Placement, copyMode string) (Job, error) {
	job, err := newJob("UPDATE", session.WorkspaceID, slotID, session.ID)
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
	t := now()
	res, err := tx.ExecContext(ctx, `UPDATE slots SET state='PREPARING',owner_session_id=?,last_used_at=?,updated_at=?,preparation_started_at=NULL,early_ready_at=NULL,update_started_at=NULL,update_completed_at=NULL,update_copy_mode=? WHERE id=? AND state='READY' AND owner_session_id IS NULL AND placement_history_complete=1`, session.ID, t, t, copyMode, slotID)
	if err != nil {
		return Job{}, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return Job{}, ErrStandbyNotUpdateable
	}
	for _, target := range targets {
		res, err := tx.ExecContext(ctx, `UPDATE slot_repositories SET state='UPDATE_PENDING',update_requested_ref=?,update_base_oid=?,update_fingerprint=?,update_compatibility_fingerprint=? WHERE slot_id=? AND repository_id=? AND state='READY' AND base_oid=? AND prepare_fingerprint=? AND compatibility_fingerprint=?`, target.RequestedRef, target.BaseOID, target.Fingerprint, target.CompatibilityFingerprint, slotID, target.RepositoryID, target.UpdateBaseOID, target.UpdateFingerprint, target.CompatibilityFingerprint)
		if err != nil {
			return Job{}, err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return Job{}, fmt.Errorf("repository %s is no longer updateable", target.RepositoryID)
		}
	}
	var repositoryCount int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM slot_repositories WHERE slot_id=?`, slotID).Scan(&repositoryCount); err != nil || repositoryCount != len(targets) {
		if err != nil {
			return Job{}, err
		}
		return Job{}, errors.New("slot repository set changed before update reservation")
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM slot_update_placements WHERE slot_id=?`, slotID); err != nil {
		return Job{}, err
	}
	for _, placement := range placements {
		if _, err := tx.ExecContext(ctx, `INSERT INTO slot_update_placements(slot_id,repository_id,relative_path,kind,source_path,content_sha256) VALUES(?,?,?,?,?,?)`, slotID, placement.RepositoryID, placement.RelativePath, placement.Kind, placement.SourcePath, placement.ContentSHA256); err != nil {
			return Job{}, err
		}
	}
	session.SlotID, session.State = slotID, "STARTING"
	if _, err := tx.ExecContext(ctx, `INSERT INTO sessions(`+sessionInsertColumns+`) VALUES(`+sessionInsertPlaceholders+`)`, sessionInsertArgs(session, t)...); err != nil {
		return Job{}, err
	}
	if err := insertCurrentSessionRepositories(ctx, tx, session.ID, session.WorkspaceID, slotID); err != nil {
		return Job{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE repositories SET last_leased_at=? WHERE id IN (SELECT repository_id FROM workspace_repositories WHERE workspace_id=?)`, t, session.WorkspaceID); err != nil {
		return Job{}, err
	}
	if err := insertJob(ctx, tx, job); err != nil {
		return Job{}, err
	}
	return job, tx.Commit()
}

func (s *Store) BeginStandbyUpdate(ctx context.Context, slotID string) error {
	s.writer.Lock()
	defer s.writer.Unlock()
	res, err := s.db.ExecContext(ctx, `UPDATE slots SET update_started_at=?,updated_at=? WHERE id=? AND state='PREPARING' AND update_started_at IS NULL AND EXISTS (SELECT 1 FROM jobs WHERE slot_id=? AND kind='UPDATE')`, now(), now(), slotID, slotID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return fmt.Errorf("slot %s update start compare-and-swap failed", slotID)
	}
	return nil
}

// FinishStandbyUpdate は全repositoryの実体検証後に、更新先と配置履歴をまとめて公開する。
func (s *Store) FinishStandbyUpdate(ctx context.Context, slotID string) (Job, bool, Job, bool, error) {
	s.writer.Lock()
	defer s.writer.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Job{}, false, Job{}, false, err
	}
	defer tx.Rollback()
	t := now()
	res, err := tx.ExecContext(ctx, `UPDATE slot_repositories SET state='READY',requested_ref=update_requested_ref,base_oid=update_base_oid,prepare_fingerprint=update_fingerprint,compatibility_fingerprint=update_compatibility_fingerprint,update_requested_ref=NULL,update_base_oid=NULL,update_fingerprint=NULL,update_compatibility_fingerprint=NULL WHERE slot_id=? AND state='UPDATE_RUNNING'`, slotID)
	if err != nil {
		return Job{}, false, Job{}, false, err
	}
	updated, _ := res.RowsAffected()
	var total int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM slot_repositories WHERE slot_id=?`, slotID).Scan(&total); err != nil {
		return Job{}, false, Job{}, false, err
	}
	if updated != int64(total) || total == 0 {
		return Job{}, false, Job{}, false, errors.New("standby update repository completion is incomplete")
	}
	var sessionID, sessionState, workspaceID string
	if err := tx.QueryRowContext(ctx, `SELECT se.id,se.state,COALESCE(se.workspace_id,'') FROM slots sl JOIN sessions se ON se.id=sl.owner_session_id WHERE sl.id=?`, slotID).Scan(&sessionID, &sessionState, &workspaceID); err != nil {
		return Job{}, false, Job{}, false, err
	}
	targetState := "LEASED"
	if sessionState == "RELEASING" {
		targetState = "DRAINING"
	} else if sessionState != "STARTING" && sessionState != "ACTIVE" {
		return Job{}, false, Job{}, false, fmt.Errorf("updated slot owner session is in unexpected state %s", sessionState)
	}
	res, err = tx.ExecContext(ctx, `UPDATE slots SET state=?,ready_at=?,updated_at=?,update_completed_at=?,update_copy_mode=NULL WHERE id=? AND state='PREPARING' AND update_started_at IS NOT NULL AND owner_session_id IS NOT NULL`, targetState, t, t, t, slotID)
	if err != nil {
		return Job{}, false, Job{}, false, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return Job{}, false, Job{}, false, errors.New("standby update slot completion changed")
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM slot_placements WHERE slot_id=?`, slotID); err != nil {
		return Job{}, false, Job{}, false, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO slot_placements(slot_id,repository_id,relative_path,kind,source_path,content_sha256) SELECT slot_id,repository_id,relative_path,kind,source_path,content_sha256 FROM slot_update_placements WHERE slot_id=?`, slotID); err != nil {
		return Job{}, false, Job{}, false, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM slot_update_placements WHERE slot_id=?`, slotID); err != nil {
		return Job{}, false, Job{}, false, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE sessions SET state='ACTIVE',started_at=COALESCE(started_at,?) WHERE slot_id=? AND state='STARTING'`, t, slotID); err != nil {
		return Job{}, false, Job{}, false, err
	}
	if targetState == "DRAINING" {
		releaseJob, err := newJob("SNAPSHOT", workspaceID, slotID, sessionID)
		if err != nil {
			return Job{}, false, Job{}, false, err
		}
		if err := insertJob(ctx, tx, releaseJob); err != nil {
			return Job{}, false, Job{}, false, err
		}
		return releaseJob, true, Job{}, false, tx.Commit()
	}
	job, created, _, err := recordStandbySuccessTx(ctx, tx, sessionID, true)
	if err != nil {
		return Job{}, false, Job{}, false, err
	}
	return Job{}, false, job, created, tx.Commit()
}

func (s *Store) MarkRepositoryUpdateRunning(ctx context.Context, slotID, repositoryID string) error {
	return s.SetSlotRepositoryState(ctx, slotID, repositoryID, []string{"UPDATE_PENDING"}, "UPDATE_RUNNING")
}
