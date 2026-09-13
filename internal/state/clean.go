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

// ErrCleanModeConflict は実行中の clean と mode か対象範囲が異なる要求を断る。
// mode と対象 workspace の両方が一致する再実行だけが既存 run へ合流する。
var ErrCleanModeConflict = errors.New("a wx clear with a different mode or scope is already running")

// clean run と target の state 名。遷移は SQL の compare-and-swap で検証する。
const (
	CleanRunRunning = "RUNNING"
	CleanRunDone    = "DONE"
)

// clean run の補充再開の進行状態。空文字は未着手で、CAS で RUNNING を 1 度だけ獲得する。
const (
	CleanReplenishPending = ""
	CleanReplenishRunning = "RUNNING"
	CleanReplenishDone    = "DONE"
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
	// LeaseKind は貸出の性質で、使用中の slot を残すときの案内先（agent の停止か wx release か）を分けるために使う。
	LeaseKind string
	// UnsavedSubmodules は snapshot に入らなかった submodule 作業の記録数で、`--discard` 無しの削除を断る根拠に使う。
	UnsavedSubmodules int
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
// WorkspaceID が空文字の run は全 workspace を対象にする。
// Replenish 以降は run を閉じた後の補充再開の指示と進行で、daemon 再起動後も同じ指示で再開できるよう run に持たせる。
type CleanRun struct {
	ID              string `json:"id"`
	Mode            string `json:"mode"`
	State           string `json:"state"`
	WorkspaceID     string `json:"workspace_id,omitempty"`
	Replenish       bool   `json:"replenish,omitempty"`
	ReplenishState  string `json:"replenish_state,omitempty"`
	ReplenishResult string `json:"replenish_result,omitempty"`
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
 (SELECT COUNT(*) FROM snapshots sn WHERE sn.session_id=COALESCE(se.parent_session_id,'') AND sn.status='ARCHIVED'),
 COALESCE(se.lease_kind,''),
 (SELECT COUNT(*) FROM unsaved_submodules us WHERE us.slot_id=sl.id)
 FROM slots sl JOIN roots rt ON rt.id=sl.root_id LEFT JOIN sessions se ON se.id=sl.owner_session_id
 WHERE sl.state<>'ARCHIVED' ORDER BY sl.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CleanCandidate
	for rows.Next() {
		var c CleanCandidate
		if err := rows.Scan(&c.SlotID, &c.WorkspaceID, &c.SlotState, &c.SessionID, &c.SessionState, &c.Path, &c.Repositories, &c.ParentSnapshots, &c.LeaseKind, &c.UnsavedSubmodules); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// cleanRunColumns は CleanRun を読む全経路で同じ列と順序を使い、scanCleanRun と対にする。
const cleanRunColumns = `id,mode,state,workspace_id,replenish,replenish_state,COALESCE(replenish_result,'')`

func scanCleanRun(row rowScanner) (CleanRun, error) {
	var run CleanRun
	if err := row.Scan(&run.ID, &run.Mode, &run.State, &run.WorkspaceID, &run.Replenish, &run.ReplenishState, &run.ReplenishResult); err != nil {
		return CleanRun{}, err
	}
	return run, nil
}

// ActiveCleanRun は実行中の clean を返す。CLI の合流判定と、貸出側の競合判定の説明に使う。
func (s *Store) ActiveCleanRun(ctx context.Context) (CleanRun, bool, error) {
	run, err := scanCleanRun(s.db.QueryRowContext(ctx, `SELECT `+cleanRunColumns+` FROM clean_runs WHERE state=? ORDER BY created_at LIMIT 1`, CleanRunRunning))
	if errors.Is(err, sql.ErrNoRows) {
		return CleanRun{}, false, nil
	}
	return run, err == nil, err
}

// BeginCleanRun は run と対象一式を 1 トランザクションで登録する。
// mode と対象 workspace が一致する実行中 run があれば新しい対象を作らずその ID を返し、終了要求と削除ジョブを重複させない。
// 合流するとき、要求側だけが補充再開を求めていれば既存 run の指示を上げる。
// 指示は上げるだけで下げない。後からの要求は追加であり、指示なしの再実行が先行の約束を取り消すのは驚きになるためである。
// commentlint:allow-long -- 合流の条件と、補充再開の指示を上げるだけにする理由を呼び出し側へ残す
func (s *Store) BeginCleanRun(ctx context.Context, run CleanRun, targets []CleanTarget, suspend []string) (string, bool, error) {
	s.writer.Lock()
	defer s.writer.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", false, err
	}
	defer tx.Rollback()
	var existingID, existingMode, existingWorkspace string
	err = tx.QueryRowContext(ctx, `SELECT id,mode,workspace_id FROM clean_runs WHERE state=? ORDER BY created_at LIMIT 1`, CleanRunRunning).Scan(&existingID, &existingMode, &existingWorkspace)
	switch {
	case err == nil && existingMode == run.Mode && existingWorkspace == run.WorkspaceID:
		if run.Replenish {
			if _, err := tx.ExecContext(ctx, `UPDATE clean_runs SET replenish=1,updated_at=? WHERE id=? AND replenish=0`, now(), existingID); err != nil {
				return "", false, err
			}
			return existingID, true, tx.Commit()
		}
		return existingID, true, nil
	case err == nil:
		return "", false, fmt.Errorf("%w: run %s is in %s mode with scope %q", ErrCleanModeConflict, existingID, existingMode, existingWorkspace)
	case !errors.Is(err, sql.ErrNoRows):
		return "", false, err
	}
	t := now()
	id := run.ID
	if _, err := tx.ExecContext(ctx, `INSERT INTO clean_runs(id,mode,state,workspace_id,replenish,created_at,updated_at) VALUES(?,?,?,?,?,?,?)`,
		id, run.Mode, CleanRunRunning, run.WorkspaceID, run.Replenish, t, t); err != nil {
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
		if _, err := tx.ExecContext(ctx, `INSERT INTO replenish_suspensions(workspace_id,reason,detail,suspended_at) VALUES(?,?,?,?) ON CONFLICT(workspace_id) DO UPDATE SET reason=excluded.reason,detail=excluded.detail,suspended_at=excluded.suspended_at`, workspaceID, SuspendReplenishReasonClean, id, t); err != nil {
			return "", false, err
		}
	}
	return id, false, tx.Commit()
}

// CleanRunByID は run 1 件と、その全対象を返す。
func (s *Store) CleanRunByID(ctx context.Context, id string) (CleanRun, []CleanTarget, error) {
	run, err := scanCleanRun(s.db.QueryRowContext(ctx, `SELECT `+cleanRunColumns+` FROM clean_runs WHERE id=?`, id))
	if err != nil {
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
	return s.cleanRuns(ctx, `WHERE state=? ORDER BY created_at`, CleanRunRunning)
}

// PendingCleanReplenishRuns は run を閉じたが補充再開が終わっていない run を返す。
// daemon が再開の直前や途中で落ちると RunningCleanRuns では拾えないため、起動時の掃き出しはこちらを使う。
func (s *Store) PendingCleanReplenishRuns(ctx context.Context) ([]CleanRun, error) {
	return s.cleanRuns(ctx, `WHERE state=? AND replenish=1 AND replenish_state<>? ORDER BY created_at`, CleanRunDone, CleanReplenishDone)
}

func (s *Store) cleanRuns(ctx context.Context, where string, args ...any) ([]CleanRun, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+cleanRunColumns+` FROM clean_runs `+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CleanRun
	for rows.Next() {
		run, err := scanCleanRun(rows)
		if err != nil {
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

// ClaimCleanReplenish は閉じた run の補充再開を 1 度だけ引き受ける。
// 引き受けられたときだけ true を返し、driver の再起動や再開の重複実行では false になる。
// fromRunning が true のときは中断で RUNNING に残った claim も引き受ける。daemon 起動時の掃き出しだけが指定する。
func (s *Store) ClaimCleanReplenish(ctx context.Context, runID string, fromRunning bool) (bool, error) {
	s.writer.Lock()
	defer s.writer.Unlock()
	states := []string{CleanReplenishPending}
	if fromRunning {
		states = append(states, CleanReplenishRunning)
	}
	args := append([]any{CleanReplenishRunning, now(), runID, CleanRunDone}, stringsToAny(states)...)
	res, err := s.db.ExecContext(ctx, `UPDATE clean_runs SET replenish_state=?,updated_at=? WHERE id=? AND state=? AND replenish=1 AND replenish_state IN (`+placeholders(len(states))+`)`, args...)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// FinishCleanReplenish は引き受け済みの補充再開を結果とともに閉じる。
func (s *Store) FinishCleanReplenish(ctx context.Context, runID, result string) error {
	s.writer.Lock()
	defer s.writer.Unlock()
	res, err := s.db.ExecContext(ctx, `UPDATE clean_runs SET replenish_state=?,replenish_result=?,updated_at=? WHERE id=? AND replenish_state=?`,
		CleanReplenishDone, result, now(), runID, CleanReplenishRunning)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return fmt.Errorf("clean run %s does not hold a replenish claim", runID)
	}
	return nil
}

// CleanSuspendedWorkspaces は、この run が止めた workspace の ID を返す。
// 後続の clean が同じ workspace を止めると detail が上書きされるので、先行 run の対象からは自然に外れる。
func (s *Store) CleanSuspendedWorkspaces(ctx context.Context, runID string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT workspace_id FROM replenish_suspensions WHERE reason=? AND detail=? ORDER BY workspace_id`, SuspendReplenishReasonClean, runID)
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

// ReplenishSuspended は workspace の待機用 worktree 補充が停止中かを返す。
// 停止は永続化してあるため、daemon 再起動後の定期 reconcile でも再生成されない。
func (s *Store) ReplenishSuspended(ctx context.Context, workspaceID string) (bool, error) {
	var present int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM replenish_suspensions WHERE workspace_id=?`, workspaceID).Scan(&present); err != nil {
		return false, err
	}
	return present > 0, nil
}

// ResumeReplenish は workspace の補充停止を解除する。
// 呼ぶのは手動起動（新規貸出・resume）が成功した時点だけで、停止理由では区別しない。
// `wx retry-standby` と `wx clear --replenish` は補充の予約まで同じ transaction で行うため、RetryStandbyReplenishment を通る。
func (s *Store) ResumeReplenish(ctx context.Context, workspaceID string) error {
	s.writer.Lock()
	defer s.writer.Unlock()
	_, err := s.db.ExecContext(ctx, `DELETE FROM replenish_suspensions WHERE workspace_id=?`, workspaceID)
	return err
}

// SuspendReplenishReasonClean は `wx clear` が待機用slotを削除した後の補充停止を表す。detail は clean run の ID である。
const SuspendReplenishReasonClean = "CLEAN"

// SuspendReplenishReasonStandbyFailure は待機用slotの準備が失敗した後の補充停止を表す。detail は失敗した job の ID である。
const SuspendReplenishReasonStandbyFailure = "STANDBY_PREPARE_FAILED"

// SuspendReplenishReasonForget は `wx forget` が待機用slotを回収する間の補充停止を表す。detail は workspace root path である。
// 解除に成功すれば行ごと消えるので、残っている停止は途中で失敗した forget を指す。
const SuspendReplenishReasonForget = "FORGET"

// SuspendReplenish は workspace の待機用 worktree 補充を停止する。
// 既に停止中なら理由を上書きせず、最初に止めた理由と時刻を残す。
func (s *Store) SuspendReplenish(ctx context.Context, workspaceID, reason, detail string) error {
	s.writer.Lock()
	defer s.writer.Unlock()
	_, err := s.db.ExecContext(ctx, `INSERT INTO replenish_suspensions(workspace_id,reason,detail,suspended_at) VALUES(?,?,?,?)
		ON CONFLICT(workspace_id) DO NOTHING`, workspaceID, reason, detail, now())
	return err
}

// SuspendFailedStandbyReplenishment は現行世代の未貸出 slot の準備失敗だけで補充を停止する。
// 構成更新で無効になった準備ジョブが新世代を止めないよう、対象の確認と停止の記録を同じ SQL で行う。
// 既存の停止理由は維持し、今回新しく停止を記録した場合だけ true を返す。
func (s *Store) SuspendFailedStandbyReplenishment(ctx context.Context, jobID string) (bool, error) {
	s.writer.Lock()
	defer s.writer.Unlock()
	res, err := s.db.ExecContext(ctx, `INSERT INTO replenish_suspensions(workspace_id,reason,detail,suspended_at)
		SELECT sl.workspace_id,?,j.id,? FROM jobs j
		JOIN slots sl ON sl.id=j.slot_id AND sl.workspace_id=j.workspace_id
		JOIN workspaces w ON w.id=sl.workspace_id AND w.generation=sl.generation
		WHERE j.id=? AND j.kind='PREPARE' AND j.session_id IS NULL AND sl.owner_session_id IS NULL
		  AND sl.state IN ('PREPARING','FAILED','QUARANTINED')
		ON CONFLICT(workspace_id) DO NOTHING`, SuspendReplenishReasonStandbyFailure, now(), jobID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
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

// ScheduleDiscardRemoval は実行中の処理がない終了済み slot を、保存を要求せず削除へ進める。
// 削除と競合する session/job の検査と予約を同じ transaction に閉じ、再起動後も通常の REMOVE として再開する。
func (s *Store) ScheduleDiscardRemoval(ctx context.Context, slotID string) (Job, bool, error) {
	s.writer.Lock()
	defer s.writer.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Job{}, false, err
	}
	defer tx.Rollback()
	job, err := newJob("REMOVE", "", slotID, "")
	if err != nil {
		return Job{}, false, err
	}
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(workspace_id,'') FROM slots WHERE id=?`, slotID).Scan(&job.WorkspaceID); err != nil {
		return Job{}, false, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE jobs SET state='FAILED',finished_at=?,error_code=? WHERE slot_id=? AND state='PENDING'
 AND EXISTS (SELECT 1 FROM slots sl WHERE sl.id=jobs.slot_id AND sl.state NOT IN ('ARCHIVED','REMOVING')
 AND NOT EXISTS (SELECT 1 FROM sessions se WHERE se.slot_id=sl.id AND se.state IN ('STARTING','ACTIVE','RESTORING','UNBOUND')))
 AND NOT EXISTS (SELECT 1 FROM jobs running WHERE running.slot_id=jobs.slot_id AND running.state='RUNNING')`, now(), JobErrorCodeDiscarded, slotID); err != nil {
		return Job{}, false, err
	}
	res, err := tx.ExecContext(ctx, `UPDATE slots SET state='REMOVING',owner_session_id=NULL,updated_at=? WHERE id=?
 AND state NOT IN ('ARCHIVED','REMOVING')
 AND NOT EXISTS (SELECT 1 FROM sessions WHERE slot_id=slots.id AND state IN ('STARTING','ACTIVE','RESTORING','UNBOUND'))
 AND NOT EXISTS (SELECT 1 FROM jobs WHERE slot_id=slots.id AND state IN ('PENDING','RUNNING'))`, now(), slotID)
	if err != nil {
		return Job{}, false, err
	}
	n, err := res.RowsAffected()
	if err != nil || n != 1 {
		return Job{}, false, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE sessions SET state='EXPIRED' WHERE slot_id=? AND state NOT IN ('STARTING','ACTIVE','RESTORING','UNBOUND','ARCHIVED','EXPIRED')`, slotID); err != nil {
		return Job{}, false, err
	}
	if err := insertJob(ctx, tx, job); err != nil {
		return Job{}, false, err
	}
	return job, true, tx.Commit()
}
