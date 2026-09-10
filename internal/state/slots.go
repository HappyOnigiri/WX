package state

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
)

// Slot は root generation と root 相対 path で slot directory を位置付ける。Path は派生値で、read は roots.path から組み立て、write は RootID/RelPath を使う。
// single-repository workspace では slot directory の一階層下を指す daemon.Lease.Path と混同してはならない。
type Slot struct {
	PreparationStartedAt               string `json:"-"`
	EarlyReadyAt                       string `json:"-"`
	UpdateStartedAt, UpdateCompletedAt string `json:"-"`
	UpdateCopyMode                     string `json:"-"`
	// PrepareOverride は貸出要求が指定した準備設定の上書きを JSON で保持する。
	// 準備 job は貸出要求とは別のタイミングで走るため、worker はこの列から上書きを読む。
	PrepareOverride                                                                               string `json:"-"`
	PlacementHistoryComplete                                                                      bool   `json:"-"`
	ID, WorkspaceID, RootID, RelPath, Path, State, OwnerSessionID, FailureCode, FailureDetailPath string
	DirIdentity                                                                                   string
	Generation                                                                                    int
	CreatedAt, ReadyAt                                                                            string
}

// slotColumns は full-row の slot read 全てで共有する column list である。
// absolute な Slot.Path は保存せず、scanSlot が結合した root generation から組み立てるため、
// retired root も自身の slot を解決し続けられる。
const slotColumns = `sl.id,COALESCE(sl.workspace_id,''),sl.generation,sl.root_id,rt.path,sl.rel_path,COALESCE(sl.dir_identity,''),sl.state,COALESCE(sl.owner_session_id,''),sl.created_at,COALESCE(sl.ready_at,''),COALESCE(sl.failure_code,''),COALESCE(sl.failure_detail_path,''),COALESCE(sl.preparation_started_at,''),COALESCE(sl.early_ready_at,''),COALESCE(sl.update_started_at,''),COALESCE(sl.update_completed_at,''),COALESCE(sl.update_copy_mode,''),COALESCE(sl.prepare_override,''),sl.placement_history_complete`

// slotFrom は slotColumns が必要とする FROM clause である。
const slotFrom = ` FROM slots sl JOIN roots rt ON rt.id=sl.root_id`

// rowScanner は *sql.Row と *sql.Rows の双方を満たすため、single-row と multi-row の slot read は
// 1つの scan 実装を共有する。
type rowScanner interface {
	Scan(dest ...any) error
}

func scanSlot(row rowScanner) (Slot, error) {
	var x Slot
	var rootPath string
	if err := row.Scan(&x.ID, &x.WorkspaceID, &x.Generation, &x.RootID, &rootPath, &x.RelPath, &x.DirIdentity, &x.State, &x.OwnerSessionID, &x.CreatedAt, &x.ReadyAt, &x.FailureCode, &x.FailureDetailPath, &x.PreparationStartedAt, &x.EarlyReadyAt, &x.UpdateStartedAt, &x.UpdateCompletedAt, &x.UpdateCopyMode, &x.PrepareOverride, &x.PlacementHistoryComplete); err != nil {
		return Slot{}, err
	}
	x.Path = filepath.Join(rootPath, x.RelPath)
	return x, nil
}

// readySlotJoin は貸出候補になる READY slot の条件である。
// ReadySlot と ReadySlotCount は同じ集合を指す必要があるため、条件を1箇所で持つ。
const readySlotJoin = ` JOIN workspaces w ON w.id=sl.workspace_id WHERE sl.workspace_id=? AND sl.generation=w.generation AND sl.state='READY'`

func (s *Store) ReadySlot(ctx context.Context, workspaceID string) (Slot, bool, error) {
	x, err := scanSlot(s.db.QueryRowContext(ctx, `SELECT `+slotColumns+slotFrom+readySlotJoin+` ORDER BY sl.ready_at LIMIT 1`, workspaceID))
	if errors.Is(err, sql.ErrNoRows) {
		return Slot{}, false, nil
	}
	return x, err == nil, err
}

// ReadySlotCount は ReadySlot が返し得る候補の件数を返す。
// 貸出側が再試行の予算を実際の候補数に合わせるために使い、他の READY 集計とは条件が異なる。
func (s *Store) ReadySlotCount(ctx context.Context, workspaceID string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT count(*)`+slotFrom+readySlotJoin, workspaceID).Scan(&n)
	return n, err
}

func (s *Store) ReadySlots(ctx context.Context, workspaceID string) ([]Slot, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+slotColumns+slotFrom+` WHERE sl.workspace_id=? AND sl.state='READY' AND sl.owner_session_id IS NULL ORDER BY sl.ready_at,sl.id`, workspaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var slots []Slot
	for rows.Next() {
		slot, err := scanSlot(rows)
		if err != nil {
			return nil, err
		}
		slots = append(slots, slot)
	}
	return slots, rows.Err()
}

func (s *Store) SetSlotState(ctx context.Context, id string, from []string, to, code string) error {
	return s.SetSlotStateWithDetail(ctx, id, from, to, code, "")
}

// SetSlotStateWithDetail は slot 遷移と診断ファイルの場所を同じ CAS で保存する。
// failure_detail_path は失敗原因の追跡にだけ使い、所有権不明時の隔離判断は従来どおり state で行う。
func (s *Store) SetSlotStateWithDetail(ctx context.Context, id string, from []string, to, code, detailPath string) error {
	s.writer.Lock()
	defer s.writer.Unlock()
	args := append([]any{to, now(), nullString(code), nullString(detailPath), id}, stringsToAny(from)...)
	res, err := s.db.ExecContext(ctx, `UPDATE slots SET state=?,updated_at=?,failure_code=?,failure_detail_path=? WHERE id=? AND state IN (`+placeholders(len(from))+`)`, args...)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		return fmt.Errorf("slot %s state compare-and-swap failed", id)
	}
	message := "state=" + to + " failure_code=" + code
	if detailPath != "" {
		message += " detail_path=" + detailPath
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO events(time,level,kind,workspace_id,slot_id,message) SELECT ?,'info','slot_transition',workspace_id,id,? FROM slots WHERE id=?`, now(), message, id)
	return err
}

func (s *Store) ResetPreparationForRetry(ctx context.Context, id string) error {
	s.writer.Lock()
	defer s.writer.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `UPDATE slots SET state='PREPARING',failure_code=NULL,failure_detail_path=NULL,updated_at=? WHERE id=? AND state='FAILED'`, now(), id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE slot_repositories SET state='PREPARING' WHERE slot_id=? AND state='PREPARE_RUNNING'`, id); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) MarkReady(ctx context.Context, id string) error {
	if err := s.SetSlotState(ctx, id, []string{"PREPARING", "RESTORING"}, "READY", ""); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `UPDATE slots SET ready_at=? WHERE id=?`, now(), id)
	return err
}

func (s *Store) FinishPreparationWithRelease(ctx context.Context, id string) (Job, bool, error) {
	job, scheduled, _, _, err := s.FinishPreparationWithReplenishment(ctx, id)
	return job, scheduled, err
}

// FinishPreparationWithReplenishment は準備完了による slot/session 遷移と、
// 通常 session の補充許可を一つの transaction で確定する。
// 末尾の Job/boolean は、今回の成功で除外対象があり ENSURE_STANDBY を登録した場合だけ有効である。
func (s *Store) FinishPreparationWithReplenishment(ctx context.Context, id string) (Job, bool, Job, bool, error) {
	s.writer.Lock()
	defer s.writer.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Job{}, false, Job{}, false, err
	}
	defer tx.Rollback()
	t := now()
	var sessionID, sessionState, kind, parentID, pendingAgentID string
	err = tx.QueryRowContext(ctx, `SELECT COALESCE(se.id,''),COALESCE(se.state,''),COALESCE(se.agent_kind,''),COALESCE(se.parent_session_id,''),COALESCE(se.pending_agent_session_id,'') FROM slots sl LEFT JOIN sessions se ON se.id=sl.owner_session_id WHERE sl.id=?`, id).Scan(&sessionID, &sessionState, &kind, &parentID, &pendingAgentID)
	if err != nil {
		return Job{}, false, Job{}, false, err
	}
	targetState := "READY"
	if sessionID != "" {
		// ACTIVE は STARTING と同じ扱いにする。BindAgentSession は slot を見ずに owner session を昇格するため、
		// 準備完了前に SessionStart hook が到達すると session は ACTIVE、slot は PREPARING のままになる。
		// どちらにせよ slot はその session が占有しているので、行き先は LEASED が正しく、後続 UPDATE が0行でも無害である。
		switch sessionState {
		case "STARTING", "RESTORING", "ACTIVE":
			targetState = "LEASED"
		case "RELEASING":
			targetState = "DRAINING"
		default:
			return Job{}, false, Job{}, false, fmt.Errorf("slot %s has owner session %s in unexpected state %s", id, sessionID, sessionState)
		}
	}
	res, err := tx.ExecContext(ctx, `UPDATE slots SET state=?,ready_at=?,updated_at=? WHERE id=? AND state IN ('PREPARING','RESTORING')`, targetState, t, t, id)
	if err != nil {
		return Job{}, false, Job{}, false, err
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		return Job{}, false, Job{}, false, fmt.Errorf("slot %s preparation state changed", id)
	}
	if sessionState == "RESTORING" {
		if pendingAgentID != "" {
			if parentID == "" {
				return Job{}, false, Job{}, false, errors.New("restoring session has no parent for its pending agent mapping")
			}
			_, err = tx.ExecContext(ctx, `UPDATE sessions SET agent_session_id=NULL WHERE id=? AND agent_kind=? AND agent_session_id=?`, parentID, kind, pendingAgentID)
			if err != nil {
				return Job{}, false, Job{}, false, err
			}

			res, err = tx.ExecContext(ctx, `UPDATE sessions SET agent_session_id=?,pending_agent_session_id=NULL WHERE id=? AND state='RESTORING' AND NOT EXISTS (SELECT 1 FROM sessions other WHERE other.agent_kind=? AND other.agent_session_id=? AND other.id<>?)`, pendingAgentID, sessionID, kind, pendingAgentID, sessionID)
			if err != nil {
				return Job{}, false, Job{}, false, err
			}
			if n, _ := res.RowsAffected(); n != 1 {
				if _, err := tx.ExecContext(ctx, `INSERT INTO events(time,level,kind,session_id,message) VALUES(?,'warn','resume_mapping_conflict',?,?)`, t, sessionID, "restored workspace activated with pending agent mapping: "+pendingAgentID); err != nil {
					return Job{}, false, Job{}, false, err
				}
			}
		}
	}
	if _, err = tx.ExecContext(ctx, `UPDATE sessions SET state='ACTIVE',started_at=COALESCE(started_at,?) WHERE slot_id=? AND state IN ('STARTING','RESTORING')`, t, id); err != nil {
		return Job{}, false, Job{}, false, err
	}
	if targetState != "DRAINING" {
		var replenishJob Job
		var replenishCreated bool
		if sessionID != "" && sessionState != "RESTORING" && targetState == "LEASED" {
			var recordErr error
			replenishJob, replenishCreated, _, recordErr = recordStandbySuccessTx(ctx, tx, sessionID, false)
			if recordErr != nil {
				return Job{}, false, Job{}, false, recordErr
			}
		}
		if err := tx.Commit(); err != nil {
			return Job{}, false, Job{}, false, err
		}
		return Job{}, false, replenishJob, replenishCreated, nil
	}
	job, err := newJob("SNAPSHOT", "", id, sessionID)
	if err != nil {
		return Job{}, false, Job{}, false, err
	}
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(workspace_id,'') FROM sessions WHERE id=?`, sessionID).Scan(&job.WorkspaceID); err != nil {
		return Job{}, false, Job{}, false, err
	}
	if err := insertJob(ctx, tx, job); err != nil {
		return Job{}, false, Job{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return Job{}, false, Job{}, false, err
	}
	return job, true, Job{}, false, nil
}

func (s *Store) Slot(ctx context.Context, id string) (Slot, error) {
	return scanSlot(s.db.QueryRowContext(ctx, `SELECT `+slotColumns+slotFrom+` WHERE sl.id=?`, id))
}
