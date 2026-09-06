package state

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/HappyOnigiri/WX/internal/domain"
)

type Job struct {
	ID, Kind, WorkspaceID, SlotID, SessionID, RepositoryID, State string
	ErrorCode, ErrorDetailPath                                    string
	Attempt                                                       int
}

const jobLease = 30 * time.Second

func newJob(kind, workspaceID, slotID, sessionID string) (Job, error) {
	id, err := domain.NewID()
	if err != nil {
		return Job{}, err
	}
	return Job{ID: id, Kind: kind, WorkspaceID: workspaceID, SlotID: slotID, SessionID: sessionID, State: "PENDING"}, nil
}

func insertJob(ctx context.Context, tx *sql.Tx, job Job) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO jobs(id,kind,workspace_id,slot_id,session_id,repository_id,state,attempt,not_before) VALUES(?,?,?,?,?,?,'PENDING',0,NULL)`, job.ID, job.Kind, nullString(job.WorkspaceID), nullString(job.SlotID), nullString(job.SessionID), nullString(job.RepositoryID))
	return err
}

func (s *Store) CreateJob(ctx context.Context, kind, workspaceID, slotID, sessionID string) (Job, error) {
	job, err := newJob(kind, workspaceID, slotID, sessionID)
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
	if err := insertJob(ctx, tx, job); err != nil {
		return Job{}, err
	}
	return job, tx.Commit()
}

func (s *Store) ClaimJob(ctx context.Context, id, owner string) (Job, error) {
	s.writer.Lock()
	defer s.writer.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Job{}, err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `UPDATE jobs SET state='RUNNING',attempt=attempt+1,started_at=?,lease_owner=?,lease_expires_at=? WHERE id=? AND state='PENDING' AND (not_before IS NULL OR not_before<=?)`, now(), owner, FormatTime(time.Now().Add(jobLease)), id, now())
	if err != nil {
		return Job{}, err
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		return Job{}, errors.New("job is not pending")
	}
	// ここでは UPDATE の RETURNING ではなく別の SELECT が必要である。RETURNING は文自身が書いた row image だけを返し、
	// 副作用で動く AFTER trigger の変更を反映しない。trigger が claim 直後の row を削除した場合も、別 SELECT なら消失を検出して
	// claim を失敗させ transaction を rollback できるが、RETURNING では見逃す。
	var j Job
	if err := tx.QueryRowContext(ctx, `SELECT id,kind,COALESCE(workspace_id,''),COALESCE(slot_id,''),COALESCE(session_id,''),COALESCE(repository_id,''),state,attempt,COALESCE(error_code,''),COALESCE(error_detail_path,'') FROM jobs WHERE id=?`, id).Scan(&j.ID, &j.Kind, &j.WorkspaceID, &j.SlotID, &j.SessionID, &j.RepositoryID, &j.State, &j.Attempt, &j.ErrorCode, &j.ErrorDetailPath); err != nil {
		return Job{}, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO events(time,level,kind,workspace_id,slot_id,session_id,repository_id,message) VALUES(?,?,?,?,?,?,?,?)`, now(), "info", "job_started", nullString(j.WorkspaceID), nullString(j.SlotID), nullString(j.SessionID), nullString(j.RepositoryID), fmt.Sprintf("kind=%s attempt=%d", j.Kind, j.Attempt)); err != nil {
		return Job{}, err
	}
	return j, tx.Commit()
}

func (s *Store) RenewJob(ctx context.Context, id, owner string) error {
	s.writer.Lock()
	defer s.writer.Unlock()
	res, err := s.db.ExecContext(ctx, `UPDATE jobs SET lease_expires_at=? WHERE id=? AND state='RUNNING' AND lease_owner=?`, FormatTime(time.Now().Add(jobLease)), id, owner)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return errors.New("job lease is no longer owned")
	}
	return nil
}

func (s *Store) FinishJob(ctx context.Context, id, owner string, runErr error) error {
	return s.FinishJobWithDetail(ctx, id, owner, runErr, "", "")
}

// FinishJobWithDetail は失敗した job の診断ファイルを job row に固定する。
// detail path は command 出力を含み得るため、作成側が 0600 を保証したものだけを受け取る。
func (s *Store) FinishJobWithDetail(ctx context.Context, id, owner string, runErr error, failureCode, detailPath string) error {
	s.writer.Lock()
	defer s.writer.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	stateName := "SUCCEEDED"
	level := "info"
	var code any
	if runErr != nil {
		stateName = "FAILED"
		level = "error"
		if failureCode == "" {
			failureCode = "JOB_FAILED"
		}
		code = failureCode
	}
	var startedAt string
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(started_at,'') FROM jobs WHERE id=? AND state='RUNNING' AND lease_owner=?`, id, owner).Scan(&startedAt); err != nil {
		return errors.New("job cannot be finished without its active lease")
	}
	finishedAt := time.Now()
	res, err := tx.ExecContext(ctx, `UPDATE jobs SET state=?,finished_at=?,lease_owner=NULL,lease_expires_at=NULL,error_code=?,error_detail_path=? WHERE id=? AND state='RUNNING' AND lease_owner=?`, stateName, FormatTime(finishedAt), code, nullString(detailPath), id, owner)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return errors.New("job cannot be finished without its active lease")
	}
	elapsed := time.Duration(0)
	if started, parseErr := time.Parse(timestampFormat, startedAt); parseErr == nil {
		elapsed = finishedAt.Sub(started)
	}
	message := fmt.Sprintf("state=%s elapsed=%s", stateName, elapsed)
	if code != nil {
		message += " failure_code=" + fmt.Sprint(code)
	}
	if detailPath != "" {
		message += " detail_path=" + detailPath
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO events(time,level,kind,workspace_id,slot_id,session_id,repository_id,message) SELECT ?,?,kind,workspace_id,slot_id,session_id,repository_id,? FROM jobs WHERE id=?`, FormatTime(finishedAt), level, message, id); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) RetryJob(ctx context.Context, id, owner string, delay time.Duration, code string) error {
	s.writer.Lock()
	defer s.writer.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `UPDATE jobs SET state='PENDING',not_before=?,lease_owner=NULL,lease_expires_at=NULL,error_code=? WHERE id=? AND state='RUNNING' AND lease_owner=?`, FormatTime(time.Now().Add(delay)), code, id, owner)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return errors.New("job cannot be retried without its active lease")
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO events(time,level,kind,workspace_id,slot_id,session_id,repository_id,message) SELECT ?,'warn','job_retry',workspace_id,slot_id,session_id,repository_id,? FROM jobs WHERE id=?`, now(), fmt.Sprintf("delay=%s failure_code=%s", delay, code), id); err != nil {
		return err
	}
	return tx.Commit()
}

// DeferJob は retry budget を消費せず、durable な依存関係を待つ。
func (s *Store) DeferJob(ctx context.Context, id, owner string, delay time.Duration, code string) error {
	s.writer.Lock()
	defer s.writer.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `UPDATE jobs SET state='PENDING',attempt=CASE WHEN attempt>0 THEN attempt-1 ELSE 0 END,not_before=?,lease_owner=NULL,lease_expires_at=NULL,error_code=? WHERE id=? AND state='RUNNING' AND lease_owner=?`, FormatTime(time.Now().Add(delay)), code, id, owner)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return errors.New("job cannot be deferred without its active lease")
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO events(time,level,kind,workspace_id,slot_id,session_id,repository_id,message) SELECT ?,'info','job_dependency_wait',workspace_id,slot_id,session_id,repository_id,? FROM jobs WHERE id=?`, now(), fmt.Sprintf("delay=%s dependency=%s", delay, code), id); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) RecoverJobs(ctx context.Context, reclaimAll bool) ([]Job, error) {
	s.writer.Lock()
	defer s.writer.Unlock()
	query := `UPDATE jobs SET state='PENDING',lease_owner=NULL,lease_expires_at=NULL WHERE state='RUNNING' AND lease_expires_at<=?`
	args := []any{now()}
	if reclaimAll {
		query = `UPDATE jobs SET state='PENDING',lease_owner=NULL,lease_expires_at=NULL WHERE state='RUNNING'`
		args = nil
	}
	if _, err := s.db.ExecContext(ctx, query, args...); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id,kind,COALESCE(workspace_id,''),COALESCE(slot_id,''),COALESCE(session_id,''),COALESCE(repository_id,''),state,attempt,COALESCE(error_code,''),COALESCE(error_detail_path,'') FROM jobs WHERE state='PENDING' ORDER BY not_before,id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Job
	for rows.Next() {
		var j Job
		if err := rows.Scan(&j.ID, &j.Kind, &j.WorkspaceID, &j.SlotID, &j.SessionID, &j.RepositoryID, &j.State, &j.Attempt, &j.ErrorCode, &j.ErrorDetailPath); err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

func (s *Store) EnsureRecoveryJobs(ctx context.Context) ([]Job, error) {
	s.writer.Lock()
	defer s.writer.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT CASE sl.state WHEN 'PREPARING' THEN 'PREPARE' WHEN 'RESTORING' THEN 'RESTORE' WHEN 'DRAINING' THEN 'SNAPSHOT' WHEN 'SNAPSHOTTING' THEN 'SNAPSHOT' WHEN 'REMOVING' THEN 'REMOVE' END,COALESCE(sl.workspace_id,''),sl.id,COALESCE(se.id,''),'' FROM slots sl LEFT JOIN sessions se ON se.slot_id=sl.id AND (se.state IN ('STARTING','RESTORING','RELEASING','SNAPSHOTTING') OR (sl.state='REMOVING' AND se.state='ARCHIVED')) WHERE sl.state IN ('PREPARING','RESTORING','DRAINING','SNAPSHOTTING','REMOVING') AND NOT EXISTS (SELECT 1 FROM jobs j WHERE j.slot_id=sl.id AND j.state IN ('PENDING','RUNNING')) UNION ALL SELECT 'REMOVE_REPOSITORY',COALESCE(sl.workspace_id,''),sl.id,'',sr.repository_id FROM slots sl JOIN slot_repositories sr ON sr.slot_id=sl.id WHERE sl.state='RETIRING' AND sr.state='RETIRING' AND NOT EXISTS (SELECT 1 FROM jobs j WHERE j.slot_id=sl.id AND j.repository_id=sr.repository_id AND j.kind='REMOVE_REPOSITORY' AND j.state IN ('PENDING','RUNNING'))`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var candidates []Job
	for rows.Next() {
		var candidate Job
		if err := rows.Scan(&candidate.Kind, &candidate.WorkspaceID, &candidate.SlotID, &candidate.SessionID, &candidate.RepositoryID); err != nil {
			return nil, err
		}
		candidates = append(candidates, candidate)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	for index := range candidates {
		job, err := newJob(candidates[index].Kind, candidates[index].WorkspaceID, candidates[index].SlotID, candidates[index].SessionID)
		if err != nil {
			return nil, err
		}
		job.RepositoryID = candidates[index].RepositoryID
		if err := insertJob(ctx, tx, job); err != nil {
			return nil, err
		}
		candidates[index] = job
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return candidates, nil
}
