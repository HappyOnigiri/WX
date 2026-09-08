package state

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"errors"
	"fmt"
)

type Session struct {
	ID, WorkspaceID, SlotID, ParentSessionID, State, AgentKind, AgentSessionID, PendingAgentSessionID, CreatedAt, ReleasedAt, ArchivedAt, ExpiresAt string
	// LeaseKind は貸出の性質（agent / path / shell / command）である。
	// LeaseExpiresAt は agent 以外の貸出に付く lease.ttl の期限で、
	// LeaseOwnerSessionID は wx new を呼んだ親 session を指す。
	LeaseKind, LeaseExpiresAt, LeaseOwnerSessionID string
	TokenHash                                      []byte
	ClientPID, AgentPID                            int
}

// LeaseKind の値。agent は既存の agent 起動で、既定値でもある。
const (
	LeaseKindAgent   = "agent"
	LeaseKindPath    = "path"
	LeaseKindShell   = "shell"
	LeaseKindCommand = "command"
)

// sessionColumns は full-row の session read 全てで共有する column list である。
const sessionColumns = `id,COALESCE(workspace_id,''),slot_id,COALESCE(parent_session_id,''),state,agent_kind,COALESCE(agent_session_id,''),COALESCE(pending_agent_session_id,''),lease_kind,COALESCE(lease_expires_at,''),COALESCE(lease_owner_session_id,''),COALESCE(client_pid,0),COALESCE(agent_pid,0),session_token_hash,created_at,COALESCE(released_at,''),COALESCE(archived_at,''),COALESCE(expires_at,'')`

// sessionInsertColumns と sessionInsertPlaceholders は session の新規登録で共有する。
// 貸出属性の列を足し忘れた登録経路が残らないよう、列名と placeholder を1か所で持つ。
const (
	sessionInsertColumns      = `id,workspace_id,slot_id,parent_session_id,state,agent_kind,lease_kind,lease_expires_at,lease_owner_session_id,client_pid,session_token_hash,requested_branch_spec,created_at,pending_agent_session_id`
	sessionInsertPlaceholders = `?,?,?,?,?,?,?,?,?,?,?,?,?,?`
)

// sessionInsertArgs は sessionInsertColumns と同じ順で登録引数を組む。
// lease_kind が空の Session は従来の agent 起動として扱う。
func sessionInsertArgs(session Session, createdAt string) []any {
	kind := session.LeaseKind
	if kind == "" {
		kind = LeaseKindAgent
	}
	return []any{
		session.ID, nullString(session.WorkspaceID), session.SlotID, nullString(session.ParentSessionID), session.State, session.AgentKind,
		kind, nullString(session.LeaseExpiresAt), nullString(session.LeaseOwnerSessionID),
		session.ClientPID, session.TokenHash, "", createdAt, nullString(session.PendingAgentSessionID),
	}
}

func (s *Store) LeaseReady(ctx context.Context, slotID string, session Session) error {
	_, _, err := s.LeaseReadyWithReplenishment(ctx, slotID, session)
	return err
}

// LeaseReadyWithReplenishment は検証済み READY slot の通常貸出を補充許可の記録と同じ transaction で確定する。
// 戻り値の Job は、貸出直後の不足を再確認する ENSURE_STANDBY が新規登録された場合に有効である。
func (s *Store) LeaseReadyWithReplenishment(ctx context.Context, slotID string, session Session) (Job, bool, error) {
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
	res, err := tx.ExecContext(ctx, `UPDATE slots SET state='LEASED',owner_session_id=?,last_used_at=?,updated_at=? WHERE id=? AND state='READY'`, session.ID, now(), now(), slotID)
	if err != nil {
		return Job{}, false, err
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		return Job{}, false, errors.New("slot is no longer READY")
	}
	var coldRepositories int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM slot_repositories WHERE slot_id=? AND state='COLD'`, slotID).Scan(&coldRepositories); err != nil {
		return Job{}, false, err
	}
	if coldRepositories != 0 {
		return Job{}, false, errors.New("slot has COLD repositories; use the cold preparation path")
	}
	session.SlotID = slotID
	_, err = tx.ExecContext(ctx, `INSERT INTO sessions(`+sessionInsertColumns+`) VALUES(`+sessionInsertPlaceholders+`)`, sessionInsertArgs(session, now())...)
	if err != nil {
		return Job{}, false, err
	}
	if err := insertCurrentSessionRepositories(ctx, tx, session.ID, session.WorkspaceID, slotID); err != nil {
		return Job{}, false, err
	}
	_, err = tx.ExecContext(ctx, `UPDATE repositories SET last_leased_at=? WHERE id IN (SELECT repository_id FROM workspace_repositories WHERE workspace_id=?)`, now(), session.WorkspaceID)
	if err != nil {
		return Job{}, false, err
	}
	replenishJob, created, _, err := recordStandbySuccessTx(ctx, tx, session.ID, true)
	if err != nil {
		return Job{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return Job{}, false, err
	}
	return replenishJob, created, nil
}

func (s *Store) LeaseReadyWithCold(ctx context.Context, slotID string, session Session) (Job, error) {
	job, err := newJob("PREPARE", session.WorkspaceID, slotID, session.ID)
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
	res, err := tx.ExecContext(ctx, `UPDATE slots SET state='PREPARING',owner_session_id=?,last_used_at=?,updated_at=? WHERE id=? AND state='READY' AND owner_session_id IS NULL`, session.ID, now(), now(), slotID)
	if err != nil {
		return Job{}, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return Job{}, errors.New("slot is no longer READY")
	}
	res, err = tx.ExecContext(ctx, `UPDATE slot_repositories SET state='PREPARING' WHERE slot_id=? AND state='COLD'`, slotID)
	if err != nil {
		return Job{}, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return Job{}, errors.New("slot has no COLD repositories")
	}
	session.SlotID, session.State = slotID, "STARTING"
	if _, err = tx.ExecContext(ctx, `INSERT INTO sessions(`+sessionInsertColumns+`) VALUES(`+sessionInsertPlaceholders+`)`, sessionInsertArgs(session, now())...); err != nil {
		return Job{}, err
	}
	if err := insertCurrentSessionRepositories(ctx, tx, session.ID, session.WorkspaceID, slotID); err != nil {
		return Job{}, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE repositories SET last_leased_at=? WHERE id IN (SELECT repository_id FROM workspace_repositories WHERE workspace_id=?)`, now(), session.WorkspaceID); err != nil {
		return Job{}, err
	}
	if err := insertJob(ctx, tx, job); err != nil {
		return Job{}, err
	}
	return job, tx.Commit()
}

func (s *Store) MarkSessionState(ctx context.Context, id string, from []string, to string) error {
	s.writer.Lock()
	defer s.writer.Unlock()
	args := append([]any{to, to, now(), to, now(), id}, stringsToAny(from)...)
	res, err := s.db.ExecContext(ctx, `UPDATE sessions SET state=?,started_at=CASE WHEN ?='ACTIVE' THEN COALESCE(started_at,?) ELSE started_at END,released_at=CASE WHEN ?='RELEASING' THEN COALESCE(released_at,?) ELSE released_at END WHERE id=? AND state IN (`+placeholders(len(from))+`)`, args...)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		return fmt.Errorf("session %s state compare-and-swap failed", id)
	}
	return nil
}

func scanSession(row *sql.Row) (Session, error) {
	var x Session
	err := row.Scan(&x.ID, &x.WorkspaceID, &x.SlotID, &x.ParentSessionID, &x.State, &x.AgentKind, &x.AgentSessionID, &x.PendingAgentSessionID,
		&x.LeaseKind, &x.LeaseExpiresAt, &x.LeaseOwnerSessionID,
		&x.ClientPID, &x.AgentPID, &x.TokenHash, &x.CreatedAt, &x.ReleasedAt, &x.ArchivedAt, &x.ExpiresAt)
	return x, err
}

func (s *Store) Session(ctx context.Context, id, token string) (Session, error) {
	x, err := scanSession(s.db.QueryRowContext(ctx, `SELECT `+sessionColumns+` FROM sessions WHERE id=?`, id))
	if err != nil {
		return Session{}, err
	}
	if subtle.ConstantTimeCompare(x.TokenHash, HashToken(token)) != 1 {
		return Session{}, errors.New("session authentication failed")
	}
	return x, nil
}

func (s *Store) SessionByID(ctx context.Context, id string) (Session, error) {
	return scanSession(s.db.QueryRowContext(ctx, `SELECT `+sessionColumns+` FROM sessions WHERE id=?`, id))
}

func (s *Store) RegisterAgentProcess(ctx context.Context, id, token string, pid int) error {
	if pid <= 0 {
		return errors.New("agent process ID must be positive")
	}
	if _, err := s.Session(ctx, id, token); err != nil {
		return err
	}
	s.writer.Lock()
	defer s.writer.Unlock()
	res, err := s.db.ExecContext(ctx, `UPDATE sessions SET agent_pid=? WHERE id=? AND state IN ('STARTING','ACTIVE','UNBOUND','RESTORING')`, pid, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return errors.New("session is no longer active")
	}
	return nil
}

func (s *Store) BindAgentSession(ctx context.Context, id, agentID string, replaces ...string) error {
	if len(replaces) > 1 {
		return errors.New("at most one agent session replacement source is allowed")
	}
	replacesAgentID := ""
	if len(replaces) == 1 {
		replacesAgentID = replaces[0]
	}
	s.writer.Lock()
	defer s.writer.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var kind, parent, sessionState, pending, currentAgentID string
	if err := tx.QueryRowContext(ctx, `SELECT agent_kind,COALESCE(parent_session_id,''),state,COALESCE(pending_agent_session_id,''),COALESCE(agent_session_id,'') FROM sessions WHERE id=?`, id).Scan(&kind, &parent, &sessionState, &pending, &currentAgentID); err != nil {
		return err
	}
	if replacesAgentID != "" {
		// Rewind/fork は現在の native mapping を旧 ID と照合してから、同一 transaction で新 IDへ移す。
		// 旧 ID が別の hook で変化済みなら、到着順が逆転した遅延 hook として拒否する。
		if currentAgentID != replacesAgentID {
			return errors.New("agent session mapping changed before replacement")
		}
		var ownerID string
		err := tx.QueryRowContext(ctx, `SELECT id FROM sessions WHERE agent_kind=? AND agent_session_id=? AND id<>?`, kind, agentID, id).Scan(&ownerID)
		switch {
		case err == nil:
			return errors.New("replacement agent session is already bound")
		case !errors.Is(err, sql.ErrNoRows):
			return err
		}
		res, err := tx.ExecContext(ctx, `UPDATE sessions SET agent_session_id=?,state=CASE WHEN state='STARTING' THEN 'ACTIVE' ELSE state END,started_at=COALESCE(started_at,?),last_heartbeat_at=? WHERE id=? AND agent_session_id=?`, agentID, now(), now(), id, replacesAgentID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return errors.New("agent session mapping changed during replacement")
		}
		return tx.Commit()
	}
	if sessionState == "RESTORING" {
		if pending == agentID {
			res, err := tx.ExecContext(ctx, `UPDATE sessions SET started_at=COALESCE(started_at,?),last_heartbeat_at=? WHERE id=? AND state='RESTORING' AND pending_agent_session_id=?`, now(), now(), id, agentID)
			if err != nil {
				return err
			}
			if n, _ := res.RowsAffected(); n != 1 {
				return errors.New("restoring session changed during binding")
			}
			return tx.Commit()
		}
		if _, err := tx.ExecContext(ctx, `UPDATE sessions SET pending_agent_session_id=NULL WHERE id=? AND state='RESTORING'`, id); err != nil {
			return err
		}
	}
	if parent != "" {
		if _, err := tx.ExecContext(ctx, `UPDATE sessions SET agent_session_id=NULL WHERE agent_kind=? AND agent_session_id=? AND id<>?`, kind, agentID, id); err != nil {
			return err
		}
	}
	res, err := tx.ExecContext(ctx, `UPDATE sessions SET agent_session_id=?,state=CASE WHEN state='STARTING' THEN 'ACTIVE' ELSE state END,started_at=COALESCE(started_at,?),last_heartbeat_at=? WHERE id=? AND (agent_session_id IS NULL OR agent_session_id=?)`, agentID, now(), now(), id, agentID)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		return errors.New("agent session is already bound or mapping is ambiguous")
	}
	return tx.Commit()
}

func (s *Store) FindByAgentSession(ctx context.Context, kind, agentID string) (Session, error) {
	return scanSession(s.db.QueryRowContext(ctx, `SELECT `+sessionColumns+` FROM sessions WHERE agent_kind=? AND agent_session_id=?`, kind, agentID))
}

func (s *Store) Heartbeat(ctx context.Context, id, token string) error {
	if _, err := s.Session(ctx, id, token); err != nil {
		return err
	}
	s.writer.Lock()
	defer s.writer.Unlock()
	res, err := s.db.ExecContext(ctx, `UPDATE sessions SET last_heartbeat_at=? WHERE id=? AND state IN ('STARTING','ACTIVE','UNBOUND','RESTORING')`, now(), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return errors.New("session is no longer active")
	}
	return nil
}

type OrphanCandidate struct {
	ID, WorkspaceID, SlotID string
	ClientPID, AgentPID     int
}

// OrphanCandidates は heartbeat が切れた使用中 session を返す。
// path 貸出（wx new）は heartbeat を張らないため除外する。除外を忘れると wx new の worktree は
// 45 秒で保存・返却される。shell / command 貸出は client を持つので回収対象に残す。
func (s *Store) OrphanCandidates(ctx context.Context, heartbeatBefore string) ([]OrphanCandidate, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,COALESCE(workspace_id,''),slot_id,COALESCE(client_pid,0),COALESCE(agent_pid,0) FROM sessions
		WHERE state IN ('STARTING','ACTIVE','UNBOUND','RESTORING') AND lease_kind<>'path' AND COALESCE(last_heartbeat_at,created_at)<=?`, heartbeatBefore)
	if err != nil {
		return nil, err
	}
	return scanOrphanCandidates(rows)
}

// ExpiredLeaseCandidates は lease.ttl の期限が来た agent 以外の貸出を返す。
// agent 起動は期限を持たず、期限による返却も既存の返却経路（保存してから返却）を通る。
func (s *Store) ExpiredLeaseCandidates(ctx context.Context, expiredAt string) ([]OrphanCandidate, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,COALESCE(workspace_id,''),slot_id,COALESCE(client_pid,0),COALESCE(agent_pid,0) FROM sessions
		WHERE state IN ('STARTING','ACTIVE') AND lease_kind<>'agent' AND lease_expires_at IS NOT NULL AND lease_expires_at<=?`, expiredAt)
	if err != nil {
		return nil, err
	}
	return scanOrphanCandidates(rows)
}

// OrphanedChildLeases は親 session が使用中でなくなった子貸出を返す。
// 親の終了で SubAgent 用の worktree もまとめて保存・返却するための抽出であり、
// resume chain 専用の parent_session_id とは別に lease_owner_session_id で結ぶ。
func (s *Store) OrphanedChildLeases(ctx context.Context) ([]OrphanCandidate, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT ch.id,COALESCE(ch.workspace_id,''),ch.slot_id,COALESCE(ch.client_pid,0),COALESCE(ch.agent_pid,0)
		FROM sessions ch JOIN sessions pa ON pa.id=ch.lease_owner_session_id
		WHERE ch.state IN ('STARTING','ACTIVE') AND pa.state NOT IN ('STARTING','ACTIVE','UNBOUND','RESTORING')`)
	if err != nil {
		return nil, err
	}
	return scanOrphanCandidates(rows)
}

func scanOrphanCandidates(rows *sql.Rows) ([]OrphanCandidate, error) {
	defer rows.Close()
	var out []OrphanCandidate
	for rows.Next() {
		var candidate OrphanCandidate
		if err := rows.Scan(&candidate.ID, &candidate.WorkspaceID, &candidate.SlotID, &candidate.ClientPID, &candidate.AgentPID); err != nil {
			return nil, err
		}
		out = append(out, candidate)
	}
	return out, rows.Err()
}

// expireQuarantinedOwnerTx は隔離 slot を owner に持つ session を終端させ、slot の owner だけを外す。
// 隔離 slot は DRAINING へ進めないので、これを行わないと orphan reconcile が同じ解放を無限に再試行する。
// slot は QUARANTINED のまま残し、worktree にも snapshot にも触れない。
func expireQuarantinedOwnerTx(ctx context.Context, tx *sql.Tx, sessionID, slotID string) (bool, error) {
	t := now()
	res, err := tx.ExecContext(ctx, `UPDATE sessions SET state='EXPIRED',pending_agent_session_id=NULL,released_at=COALESCE(released_at,?),archived_at=COALESCE(archived_at,?),expires_at=?
		WHERE id=? AND state IN ('STARTING','ACTIVE','UNBOUND','RESTORING')
		  AND EXISTS (SELECT 1 FROM slots sl WHERE sl.id=? AND sl.owner_session_id=? AND sl.state='QUARANTINED')`, t, t, t, sessionID, slotID, sessionID)
	if err != nil {
		return false, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return false, nil
	}
	res, err = tx.ExecContext(ctx, `UPDATE slots SET owner_session_id=NULL,updated_at=? WHERE id=? AND owner_session_id=? AND state='QUARANTINED'`, t, slotID, sessionID)
	if err != nil {
		return false, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return false, errors.New("quarantined slot ownership changed before release")
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO events(time,level,kind,workspace_id,slot_id,session_id,message) SELECT ?,'warn','session_expired',workspace_id,id,?,? FROM slots WHERE id=?`,
		t, sessionID, "owner released from QUARANTINED slot", slotID); err != nil {
		return false, err
	}
	return true, nil
}

// Release は返却を進め、SNAPSHOT または REMOVE ジョブを作ったときに changed=true を返す。
// 隔離 slot による終端を区別する必要がある呼び出し側は ReleaseWithOutcome を使う。
func (s *Store) Release(ctx context.Context, sessionID, workspaceID, slotID string) (Job, bool, error) {
	job, changed, _, err := s.ReleaseWithOutcome(ctx, sessionID, workspaceID, slotID)
	return job, changed, err
}

// ReleaseWithOutcome は Release の結果に加えて、隔離 slot のため snapshot を作らず session を終端したかを返す。
// この終端では復旧 snapshot が残らないため、呼び出し側は成功として黙って閉じずに記録する。
func (s *Store) ReleaseWithOutcome(ctx context.Context, sessionID, workspaceID, slotID string) (Job, bool, bool, error) {
	job, err := newJob("SNAPSHOT", workspaceID, slotID, sessionID)
	if err != nil {
		return Job{}, false, false, err
	}
	s.writer.Lock()
	defer s.writer.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Job{}, false, false, err
	}
	defer tx.Rollback()
	expired, err := expireQuarantinedOwnerTx(ctx, tx, sessionID, slotID)
	if err != nil {
		return Job{}, false, false, err
	}
	if expired {
		return Job{}, false, true, tx.Commit()
	}
	var sessionState string
	if err := tx.QueryRowContext(ctx, `SELECT state FROM sessions WHERE id=?`, sessionID).Scan(&sessionState); err != nil {
		return Job{}, false, false, err
	}
	if sessionState == "UNBOUND" || sessionState == "RESTORING" {
		job, err = newJob("REMOVE", workspaceID, slotID, "")
		if err != nil {
			return Job{}, false, false, err
		}
		timestamp := now()
		res, updateErr := tx.ExecContext(ctx, `UPDATE sessions SET state='EXPIRED',pending_agent_session_id=NULL,released_at=?,archived_at=?,expires_at=? WHERE id=? AND state IN ('UNBOUND','RESTORING')`, timestamp, timestamp, timestamp, sessionID)
		if updateErr != nil {
			return Job{}, false, false, updateErr
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return Job{}, false, false, nil
		}
		res, err = tx.ExecContext(ctx, `UPDATE slots SET state='REMOVING',owner_session_id=NULL,updated_at=? WHERE id=? AND state IN ('UNBOUND','RESTORING')`, timestamp, slotID)
		if err != nil {
			return Job{}, false, false, err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return Job{}, false, false, errors.New("unbound slot state changed before release")
		}
		if err := insertJob(ctx, tx, job); err != nil {
			return Job{}, false, false, err
		}
		return job, true, false, tx.Commit()
	}
	res, err := tx.ExecContext(ctx, `UPDATE sessions SET state='RELEASING',released_at=COALESCE(released_at,?) WHERE id=? AND state IN ('STARTING','ACTIVE')`, now(), sessionID)
	if err != nil {
		return Job{}, false, false, err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return Job{}, false, false, nil
	}
	if _, err = tx.ExecContext(ctx, `UPDATE slots SET state='DRAINING',updated_at=? WHERE owner_session_id=? AND state='LEASED'`, now(), sessionID); err != nil {
		return Job{}, false, false, err
	}
	var slotState string
	if err := tx.QueryRowContext(ctx, `SELECT state FROM slots WHERE id=? AND owner_session_id=?`, slotID, sessionID).Scan(&slotState); err != nil {
		return Job{}, false, false, err
	}
	if slotState == "PREPARING" {
		if err := tx.Commit(); err != nil {
			return Job{}, false, false, err
		}
		return Job{}, false, false, nil
	}
	if slotState != "DRAINING" {
		return Job{}, false, false, fmt.Errorf("slot %s cannot be released from %s", slotID, slotState)
	}
	if err := insertJob(ctx, tx, job); err != nil {
		return Job{}, false, false, err
	}
	return job, true, false, tx.Commit()
}
