package state

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// ErrCleanInProgress は clean 実行中に新規の貸出・復元・待機用作成を断る。
// 予約済み slot を再貸出しないため、判定は貸出側の書き込みトランザクション内で行う。
var ErrCleanInProgress = errors.New("wx clear is in progress; retry once it finishes")

// ErrCleanModeConflict は実行中の clean と異なる mode の要求を断る。同じ mode の再実行は既存 run へ合流する。
var ErrCleanModeConflict = errors.New("a wx clear with a different mode is already running")

// clean run と target の state 名。遷移は SQL の compare-and-swap で検証する。
const (
	CleanRunRunning = "RUNNING"
	CleanRunDone    = "DONE"
)

// CleanCandidate は clean の対象選定に必要な、slot 1 件分の読み取り専用スナップショットである。
type CleanCandidate struct {
	SlotID       string
	WorkspaceID  string
	SlotState    string
	SessionID    string
	SessionState string
	Path         string
	// Repositories は slot に登録された repository 行数で、UNBOUND slot に worktree 実体がないことの根拠に使う。
	Repositories int
	// ParentSnapshots は復元元 session に残る ARCHIVED snapshot 数で、RESTORING slot を削除してよいかの根拠に使う。
	ParentSnapshots int
}

// CleanTarget は clean run が追跡する 1 対象の永続状態である。
type CleanTarget struct {
	SlotID            string `json:"slot_id"`
	WorkspaceID       string `json:"workspace_id,omitempty"`
	SessionID         string `json:"session_id,omitempty"`
	Path              string `json:"path"`
	State             string `json:"state"`
	Reason            string `json:"reason,omitempty"`
	TerminateDeadline string `json:"terminate_deadline,omitempty"`
}

// CleanRun は受付済みの clean 1 件を表す。
type CleanRun struct {
	ID    string `json:"id"`
	Mode  string `json:"mode"`
	State string `json:"state"`
}

// TerminationRequest は clean が session へ出した期限付きの終了要求である。daemon は signal を送らず、client が応答する。
type TerminationRequest struct {
	SessionID string
	RequestID string
	Deadline  string
	State     string
}

// CleanCandidates は ARCHIVED 以外の全 slot を、workspace と root 世代を問わず返す。
// 対象にするか否かの判定は呼び出し側の方針であり、ここでは事実だけを読む。
func (s *Store) CleanCandidates(ctx context.Context) ([]CleanCandidate, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT sl.id,COALESCE(sl.workspace_id,''),sl.state,COALESCE(sl.owner_session_id,''),COALESCE(se.state,''),rt.path||'/'||sl.rel_path,
 (SELECT COUNT(*) FROM slot_repositories sr WHERE sr.slot_id=sl.id),
 (SELECT COUNT(*) FROM snapshots sn WHERE sn.session_id=COALESCE(se.parent_session_id,'') AND sn.status='ARCHIVED')
 FROM slots sl JOIN roots rt ON rt.id=sl.root_id LEFT JOIN sessions se ON se.id=sl.owner_session_id
 WHERE sl.state<>'ARCHIVED' ORDER BY sl.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CleanCandidate
	for rows.Next() {
		var c CleanCandidate
		if err := rows.Scan(&c.SlotID, &c.WorkspaceID, &c.SlotState, &c.SessionID, &c.SessionState, &c.Path, &c.Repositories, &c.ParentSnapshots); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ActiveCleanRun は実行中の clean を返す。CLI の合流判定と、貸出側の競合判定の説明に使う。
func (s *Store) ActiveCleanRun(ctx context.Context) (CleanRun, bool, error) {
	var run CleanRun
	err := s.db.QueryRowContext(ctx, `SELECT id,mode,state FROM clean_runs WHERE state=? ORDER BY created_at LIMIT 1`, CleanRunRunning).Scan(&run.ID, &run.Mode, &run.State)
	if errors.Is(err, sql.ErrNoRows) {
		return CleanRun{}, false, nil
	}
	return run, err == nil, err
}

// BeginCleanRun は run と対象一式を 1 トランザクションで登録する。
// 同じ mode の実行中 run があれば新しい対象を作らずその ID を返し、終了要求と削除ジョブを重複させない。
func (s *Store) BeginCleanRun(ctx context.Context, id, mode string, targets []CleanTarget, suspend []string) (string, bool, error) {
	s.writer.Lock()
	defer s.writer.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", false, err
	}
	defer tx.Rollback()
	var existingID, existingMode string
	err = tx.QueryRowContext(ctx, `SELECT id,mode FROM clean_runs WHERE state=? ORDER BY created_at LIMIT 1`, CleanRunRunning).Scan(&existingID, &existingMode)
	switch {
	case err == nil && existingMode == mode:
		return existingID, true, nil
	case err == nil:
		return "", false, fmt.Errorf("%w: run %s is in %s mode", ErrCleanModeConflict, existingID, existingMode)
	case !errors.Is(err, sql.ErrNoRows):
		return "", false, err
	}
	t := now()
	if _, err := tx.ExecContext(ctx, `INSERT INTO clean_runs(id,mode,state,created_at,updated_at) VALUES(?,?,?,?,?)`, id, mode, CleanRunRunning, t, t); err != nil {
		return "", false, err
	}
	for _, target := range targets {
		if _, err := tx.ExecContext(ctx, `INSERT INTO clean_targets(run_id,slot_id,workspace_id,session_id,path,state,reason,updated_at) VALUES(?,?,?,?,?,?,?,?)`,
			id, target.SlotID, target.WorkspaceID, target.SessionID, target.Path, target.State, target.Reason, t); err != nil {
			return "", false, err
		}
	}
	for _, workspaceID := range suspend {
		if workspaceID == "" {
			continue
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO replenish_suspensions(workspace_id,run_id,suspended_at) VALUES(?,?,?) ON CONFLICT(workspace_id) DO UPDATE SET run_id=excluded.run_id,suspended_at=excluded.suspended_at`, workspaceID, id, t); err != nil {
			return "", false, err
		}
	}
	return id, false, tx.Commit()
}

// CleanRunByID は run 1 件と、その全対象を返す。
func (s *Store) CleanRunByID(ctx context.Context, id string) (CleanRun, []CleanTarget, error) {
	var run CleanRun
	if err := s.db.QueryRowContext(ctx, `SELECT id,mode,state FROM clean_runs WHERE id=?`, id).Scan(&run.ID, &run.Mode, &run.State); err != nil {
		return CleanRun{}, nil, err
	}
	targets, err := s.CleanTargets(ctx, id)
	return run, targets, err
}

// CleanTargets は run の対象を slot ID 順に返す。
func (s *Store) CleanTargets(ctx context.Context, runID string) ([]CleanTarget, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT slot_id,workspace_id,session_id,path,state,reason,COALESCE(terminate_deadline,'') FROM clean_targets WHERE run_id=? ORDER BY slot_id`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CleanTarget
	for rows.Next() {
		var target CleanTarget
		if err := rows.Scan(&target.SlotID, &target.WorkspaceID, &target.SessionID, &target.Path, &target.State, &target.Reason, &target.TerminateDeadline); err != nil {
			return nil, err
		}
		out = append(out, target)
	}
	return out, rows.Err()
}

// RunningCleanRuns は daemon 再起動後に再開すべき run を返す。
func (s *Store) RunningCleanRuns(ctx context.Context) ([]CleanRun, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,mode,state FROM clean_runs WHERE state=? ORDER BY created_at`, CleanRunRunning)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CleanRun
	for rows.Next() {
		var run CleanRun
		if err := rows.Scan(&run.ID, &run.Mode, &run.State); err != nil {
			return nil, err
		}
		out = append(out, run)
	}
	return out, rows.Err()
}

// SetCleanTargetState は from のいずれかにいる対象だけを to へ進める。
func (s *Store) SetCleanTargetState(ctx context.Context, runID, slotID string, from []string, to, reason string) error {
	s.writer.Lock()
	defer s.writer.Unlock()
	args := append([]any{to, reason, now(), runID, slotID}, stringsToAny(from)...)
	res, err := s.db.ExecContext(ctx, `UPDATE clean_targets SET state=?,reason=?,updated_at=? WHERE run_id=? AND slot_id=? AND state IN (`+placeholders(len(from))+`)`, args...)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return fmt.Errorf("clean target %s cannot move to %s from its current state", slotID, to)
	}
	return nil
}

// RequestSessionTermination は終了要求の記録と対象の TERMINATING 化を同時に行う。
// 既に要求済みの session には新しい要求を作らず、CLI の再実行で終了要求が重複しない。
func (s *Store) RequestSessionTermination(ctx context.Context, runID, slotID, sessionID, requestID string, deadline time.Time) error {
	s.writer.Lock()
	defer s.writer.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	t := now()
	deadlineText := FormatTime(deadline)
	res, err := tx.ExecContext(ctx, `INSERT INTO session_termination_requests(session_id,request_id,run_id,requested_at,deadline,state) VALUES(?,?,?,?,?,'PENDING') ON CONFLICT(session_id) DO NOTHING`, sessionID, requestID, runID, t, deadlineText)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		if err := tx.QueryRowContext(ctx, `SELECT deadline FROM session_termination_requests WHERE session_id=?`, sessionID).Scan(&deadlineText); err != nil {
			return err
		}
	}
	res, err = tx.ExecContext(ctx, `UPDATE clean_targets SET state='TERMINATING',terminate_deadline=?,updated_at=? WHERE run_id=? AND slot_id=? AND state='PENDING'`, deadlineText, t, runID, slotID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return fmt.Errorf("clean target %s cannot begin termination from its current state", slotID)
	}
	return tx.Commit()
}

// PendingTermination は session が受け取るべき未応答の終了要求を返す。heartbeat と agent 登録の応答に載せる。
func (s *Store) PendingTermination(ctx context.Context, sessionID string) (TerminationRequest, bool, error) {
	var request TerminationRequest
	request.SessionID = sessionID
	err := s.db.QueryRowContext(ctx, `SELECT request_id,deadline,state FROM session_termination_requests WHERE session_id=? AND state='PENDING'`, sessionID).Scan(&request.RequestID, &request.Deadline, &request.State)
	if errors.Is(err, sql.ErrNoRows) {
		return TerminationRequest{}, false, nil
	}
	return request, err == nil, err
}

// FinishTermination は要求を CONFIRMED か TIMED_OUT で閉じる。期限切れ後に届いた応答は受け付けない。
func (s *Store) FinishTermination(ctx context.Context, sessionID, requestID, to string) error {
	s.writer.Lock()
	defer s.writer.Unlock()
	res, err := s.db.ExecContext(ctx, `UPDATE session_termination_requests SET state=? WHERE session_id=? AND request_id=? AND state='PENDING'`, to, sessionID, requestID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return fmt.Errorf("termination request %s for session %s is no longer pending", requestID, sessionID)
	}
	return nil
}

// FinishCleanRun は未終了の対象が無い run を閉じる。対象が残っていれば何もせず false を返す。
func (s *Store) FinishCleanRun(ctx context.Context, runID string) (bool, error) {
	s.writer.Lock()
	defer s.writer.Unlock()
	t := now()
	res, err := s.db.ExecContext(ctx, `UPDATE clean_runs SET state=?,updated_at=?,finished_at=? WHERE id=? AND state=? AND NOT EXISTS (SELECT 1 FROM clean_targets t WHERE t.run_id=clean_runs.id AND t.state NOT IN ('DONE','FAILED','SKIPPED','QUARANTINED'))`,
		CleanRunDone, t, t, runID, CleanRunRunning)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// ReplenishSuspended は workspace の待機用 worktree 補充が停止中かを返す。
// 停止は永続化してあるため、daemon 再起動後の定期 reconcile でも再生成されない。
func (s *Store) ReplenishSuspended(ctx context.Context, workspaceID string) (bool, error) {
	var present int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM replenish_suspensions WHERE workspace_id=?`, workspaceID).Scan(&present); err != nil {
		return false, err
	}
	return present > 0, nil
}

// ResumeReplenish は workspace の補充停止を解除する。clean 後の新規貸出・resume の成功時だけ呼ぶ。
func (s *Store) ResumeReplenish(ctx context.Context, workspaceID string) error {
	s.writer.Lock()
	defer s.writer.Unlock()
	_, err := s.db.ExecContext(ctx, `DELETE FROM replenish_suspensions WHERE workspace_id=?`, workspaceID)
	return err
}

// assertNoActiveClean は貸出側の書き込みトランザクションから clean の実行を検査する。
// 対象予約と同じ writer lock の下で判定するため、予約済み slot が新しい session へ渡ることはない。
func assertNoActiveClean(ctx context.Context, tx *sql.Tx) error {
	var running int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM clean_runs WHERE state=?`, CleanRunRunning).Scan(&running); err != nil {
		return err
	}
	if running > 0 {
		return ErrCleanInProgress
	}
	return nil
}
