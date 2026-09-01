package state

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/HappyOnigiri/WX/internal/discovery"
	"github.com/HappyOnigiri/WX/internal/domain"
	"github.com/HappyOnigiri/WX/migrations"
	sqlite "modernc.org/sqlite"
)

type Store struct {
	db     *sql.DB
	writer sync.Mutex
	path   string
}

func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	dsn := (&url.URL{Scheme: "file", Path: path}).String() + "?_defensive=1&_journal_mode=wal&_foreign_keys=on&_busy_timeout=5000&_synchronous=normal"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// Connection-local policies are encoded in the DSN, so status readers can
	// use WAL concurrently while writes remain serialized by writer.
	db.SetMaxOpenConns(8)
	s := &Store{db: db, path: path}
	if err := s.init(context.Background()); err != nil {
		db.Close()
		return nil, err
	}
	if err := os.Chmod(path, 0600); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}
func (s *Store) Close() error                   { return s.db.Close() }
func (s *Store) Ping(ctx context.Context) error { return s.db.PingContext(ctx) }

func (s *Store) Backup(ctx context.Context, generations int, retention time.Duration) (string, error) {
	s.writer.Lock()
	defer s.writer.Unlock()
	dir := s.path + ".backups"
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", err
	}
	destination := filepath.Join(dir, time.Now().UTC().Format("20060102T150405.000000000Z")+".db")
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return "", err
	}
	defer conn.Close()
	err = conn.Raw(func(driverConn any) error {
		backuper, ok := driverConn.(interface {
			NewBackup(string) (*sqlite.Backup, error)
		})
		if !ok {
			return errors.New("SQLite driver does not support online backup")
		}
		backup, err := backuper.NewBackup(destination)
		if err != nil {
			return err
		}
		if _, err := backup.Step(-1); err != nil {
			_ = backup.Finish()
			return err
		}
		return backup.Finish()
	})
	if err != nil {
		return "", err
	}
	if err := os.Chmod(destination, 0600); err != nil {
		return "", err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() > entries[j].Name() })
	cutoff := time.Now().Add(-retention)
	for i, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".db" {
			continue
		}
		info, infoErr := entry.Info()
		if infoErr != nil {
			return "", infoErr
		}
		if i >= generations || (retention > 0 && info.ModTime().Before(cutoff)) {
			if err := os.Remove(filepath.Join(dir, entry.Name())); err != nil {
				return "", err
			}
		}
	}
	return destination, nil
}

func (s *Store) init(ctx context.Context) error {
	for _, pragma := range []string{"PRAGMA journal_mode=WAL", "PRAGMA foreign_keys=ON", "PRAGMA busy_timeout=5000"} {
		if _, err := s.db.ExecContext(ctx, pragma); err != nil {
			return err
		}
	}
	entries, err := fs.Glob(migrations.FS, "*.sql")
	if err != nil {
		return err
	}
	sort.Strings(entries)
	for i, name := range entries {
		version := i + 1
		var exists int
		err := s.db.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_master WHERE type='table' AND name='schema_migrations'").Scan(&exists)
		if err != nil {
			return err
		}
		if exists > 0 {
			var applied int
			if err := s.db.QueryRowContext(ctx, "SELECT count(*) FROM schema_migrations WHERE version=?", version).Scan(&applied); err != nil {
				return err
			}
			if applied > 0 {
				continue
			}
		}
		sqlText, err := migrations.FS.ReadFile(name)
		if err != nil {
			return err
		}
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, string(sqlText)); err == nil {
			_, err = tx.ExecContext(ctx, "INSERT INTO schema_migrations(version,applied_at) VALUES(?,?)", version, now())
		}
		if err != nil {
			tx.Rollback()
			return fmt.Errorf("migration %s: %w", name, err)
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

func now() string                   { return time.Now().UTC().Format(time.RFC3339Nano) }
func HashToken(token string) []byte { sum := sha256.Sum256([]byte(token)); return sum[:] }

func (s *Store) UpsertWorkspace(ctx context.Context, w discovery.Workspace) error {
	_, err := s.UpsertWorkspaceGeneration(ctx, w)
	return err
}

func (s *Store) UpsertWorkspaceGeneration(ctx context.Context, w discovery.Workspace) (int, error) {
	s.writer.Lock()
	defer s.writer.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	t := now()
	generation := 1
	existing := false
	var oldKind string
	err = tx.QueryRowContext(ctx, `SELECT generation,kind FROM workspaces WHERE id=?`, w.ID).Scan(&generation, &oldKind)
	if err == nil {
		existing = true
	} else if !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}
	type membership struct {
		rel     string
		ordinal int
	}
	oldMembers := map[string]membership{}
	if existing {
		rows, err := tx.QueryContext(ctx, `SELECT repository_id,relative_path,ordinal FROM workspace_repositories WHERE workspace_id=?`, w.ID)
		if err != nil {
			return 0, err
		}
		for rows.Next() {
			var id string
			var member membership
			if err := rows.Scan(&id, &member.rel, &member.ordinal); err != nil {
				rows.Close()
				return 0, err
			}
			oldMembers[id] = member
		}
		if err := rows.Close(); err != nil {
			return 0, err
		}
	}
	changed := existing && oldKind != w.Kind
	if len(oldMembers) != len(w.Repositories) && existing {
		changed = true
	}
	for i, repo := range w.Repositories {
		old, ok := oldMembers[string(repo.ID)]
		if existing && (!ok || old.rel != repo.RelativePath || old.ordinal != i) {
			changed = true
		}
	}
	if changed {
		generation++
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO workspaces(id,root_path,kind,generation,discovery_state,first_seen_at,last_seen_at,last_reconciled_at) VALUES(?,?,?,?,'READY',?,?,?) ON CONFLICT(id) DO UPDATE SET root_path=excluded.root_path,kind=excluded.kind,generation=excluded.generation,last_seen_at=excluded.last_seen_at,last_reconciled_at=excluded.last_reconciled_at`, w.ID, w.Root, w.Kind, generation, t, t, t)
	if err != nil {
		return 0, err
	}
	if existing {
		if _, err := tx.ExecContext(ctx, `DELETE FROM workspace_repositories WHERE workspace_id=?`, w.ID); err != nil {
			return 0, err
		}
	}
	for i, r := range w.Repositories {
		_, err = tx.ExecContext(ctx, `INSERT INTO repositories(id,main_worktree_path,common_git_dir,default_branch,first_seen_at,last_seen_at) VALUES(?,?,?,?,?,?) ON CONFLICT(id) DO UPDATE SET main_worktree_path=excluded.main_worktree_path,common_git_dir=excluded.common_git_dir,default_branch=excluded.default_branch,last_seen_at=excluded.last_seen_at`, r.ID, r.MainPath, r.CommonDir, r.DefaultBranch, t, t)
		if err != nil {
			return 0, err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO workspace_repositories(workspace_id,repository_id,relative_path,ordinal) VALUES(?,?,?,?) ON CONFLICT(workspace_id,repository_id) DO UPDATE SET relative_path=excluded.relative_path,ordinal=excluded.ordinal`, w.ID, r.ID, r.RelativePath, i)
		if err != nil {
			return 0, err
		}
	}
	if changed {
		if _, err := tx.ExecContext(ctx, `UPDATE slots SET state='STALE',updated_at=? WHERE workspace_id=? AND generation<? AND owner_session_id IS NULL AND state IN ('PREPARING','READY')`, t, w.ID, generation); err != nil {
			return 0, err
		}
	}
	return generation, tx.Commit()
}

func (s *Store) WorkspaceGeneration(ctx context.Context, workspaceID string) (int, error) {
	var generation int
	err := s.db.QueryRowContext(ctx, `SELECT generation FROM workspaces WHERE id=?`, workspaceID).Scan(&generation)
	return generation, err
}

type Slot struct {
	ID, WorkspaceID, Path, State, OwnerSessionID string
	Generation                                   int
	CreatedAt, ReadyAt                           string
}
type SlotRepository struct{ RepositoryID, WorktreePath, State, RequestedRef, BaseOID, Fingerprint string }
type Session struct {
	ID, WorkspaceID, SlotID, ParentSessionID, State, AgentKind, AgentSessionID, CreatedAt, ReleasedAt, ArchivedAt, ExpiresAt string
	TokenHash                                                                                                                []byte
	ClientPID                                                                                                                int
}
type Snapshot struct{ ID, SessionID, RepositoryID, HeadOID, HeadRef, IndexTreeOID, WorktreeOID, WorktreeRef, Status, CreatedAt, ExpiresAt string }
type Job struct {
	ID, Kind, WorkspaceID, SlotID, SessionID, RepositoryID, State string
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
	_, err := tx.ExecContext(ctx, `INSERT INTO jobs(id,kind,workspace_id,slot_id,session_id,state,attempt,not_before) VALUES(?,?,?,?,?,'PENDING',0,?)`, job.ID, job.Kind, nullString(job.WorkspaceID), nullString(job.SlotID), nullString(job.SessionID), now())
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
	res, err := tx.ExecContext(ctx, `UPDATE jobs SET state='RUNNING',attempt=attempt+1,started_at=?,lease_owner=?,lease_expires_at=? WHERE id=? AND state='PENDING' AND (not_before IS NULL OR not_before<=?)`, now(), owner, time.Now().Add(jobLease).UTC().Format(time.RFC3339Nano), id, now())
	if err != nil {
		return Job{}, err
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		return Job{}, errors.New("job is not pending")
	}
	var j Job
	if err := tx.QueryRowContext(ctx, `SELECT id,kind,COALESCE(workspace_id,''),COALESCE(slot_id,''),COALESCE(session_id,''),COALESCE(repository_id,''),state,attempt FROM jobs WHERE id=?`, id).Scan(&j.ID, &j.Kind, &j.WorkspaceID, &j.SlotID, &j.SessionID, &j.RepositoryID, &j.State, &j.Attempt); err != nil {
		return Job{}, err
	}
	return j, tx.Commit()
}

func (s *Store) RenewJob(ctx context.Context, id, owner string) error {
	s.writer.Lock()
	defer s.writer.Unlock()
	res, err := s.db.ExecContext(ctx, `UPDATE jobs SET lease_expires_at=? WHERE id=? AND state='RUNNING' AND lease_owner=?`, time.Now().Add(jobLease).UTC().Format(time.RFC3339Nano), id, owner)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return errors.New("job lease is no longer owned")
	}
	return nil
}

func (s *Store) FinishJob(ctx context.Context, id, owner string, runErr error) error {
	s.writer.Lock()
	defer s.writer.Unlock()
	stateName := "SUCCEEDED"
	var code any
	if runErr != nil {
		stateName = "FAILED"
		code = "JOB_FAILED"
	}
	res, err := s.db.ExecContext(ctx, `UPDATE jobs SET state=?,finished_at=?,lease_owner=NULL,lease_expires_at=NULL,error_code=? WHERE id=? AND state='RUNNING' AND lease_owner=?`, stateName, now(), code, id, owner)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return errors.New("job cannot be finished without its active lease")
	}
	return nil
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
	rows, err := s.db.QueryContext(ctx, `SELECT id,kind,COALESCE(workspace_id,''),COALESCE(slot_id,''),COALESCE(session_id,''),COALESCE(repository_id,''),state,attempt FROM jobs WHERE state='PENDING' ORDER BY not_before,id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Job
	for rows.Next() {
		var j Job
		if err := rows.Scan(&j.ID, &j.Kind, &j.WorkspaceID, &j.SlotID, &j.SessionID, &j.RepositoryID, &j.State, &j.Attempt); err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

func (s *Store) ReadySlot(ctx context.Context, workspaceID string) (Slot, bool, error) {
	var x Slot
	err := s.db.QueryRowContext(ctx, `SELECT sl.id,sl.workspace_id,sl.generation,sl.path,sl.state,COALESCE(sl.owner_session_id,''),sl.created_at,COALESCE(sl.ready_at,'') FROM slots sl JOIN workspaces w ON w.id=sl.workspace_id WHERE sl.workspace_id=? AND sl.generation=w.generation AND sl.state='READY' ORDER BY sl.ready_at LIMIT 1`, workspaceID).Scan(&x.ID, &x.WorkspaceID, &x.Generation, &x.Path, &x.State, &x.OwnerSessionID, &x.CreatedAt, &x.ReadyAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Slot{}, false, nil
	}
	return x, err == nil, err
}

func (s *Store) HasStandby(ctx context.Context, workspaceID string) bool {
	return s.StandbyCount(ctx, workspaceID) > 0
}

func (s *Store) StandbyCount(ctx context.Context, workspaceID string) int {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM slots sl JOIN workspaces w ON w.id=sl.workspace_id WHERE sl.workspace_id=? AND sl.generation=w.generation AND sl.owner_session_id IS NULL AND sl.state IN ('PREPARING','READY')`, workspaceID).Scan(&n)
	if err != nil {
		return 0
	}
	return n
}

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
	t := now()
	_, err = tx.ExecContext(ctx, `INSERT INTO slots(id,workspace_id,generation,path,state,owner_session_id,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?)`, slot.ID, nullString(slot.WorkspaceID), slot.Generation, slot.Path, slot.State, nullString(session.ID), t, t)
	if err != nil {
		return Job{}, err
	}
	for _, r := range repos {
		_, err = tx.ExecContext(ctx, `INSERT INTO slot_repositories(slot_id,repository_id,worktree_path,state,requested_ref,base_oid,prepare_fingerprint) VALUES(?,?,?,?,?,?,?)`, slot.ID, r.RepositoryID, r.WorktreePath, r.State, r.RequestedRef, r.BaseOID, r.Fingerprint)
		if err != nil {
			return Job{}, err
		}
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO sessions(id,workspace_id,slot_id,parent_session_id,state,agent_kind,client_pid,session_token_hash,requested_branch_spec,created_at) VALUES(?,?,?,?,?,?,?,?,?,?)`, session.ID, nullString(session.WorkspaceID), session.SlotID, nullString(session.ParentSessionID), session.State, session.AgentKind, session.ClientPID, session.TokenHash, "", t)
	if err != nil {
		return Job{}, err
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
	t := now()
	_, err = tx.ExecContext(ctx, `INSERT INTO slots(id,workspace_id,generation,path,state,created_at,updated_at) VALUES(?,?,?,?,?,?,?)`, slot.ID, slot.WorkspaceID, slot.Generation, slot.Path, slot.State, t, t)
	if err != nil {
		return Job{}, err
	}
	for _, r := range repos {
		if _, err = tx.ExecContext(ctx, `INSERT INTO slot_repositories(slot_id,repository_id,worktree_path,state,requested_ref,base_oid,prepare_fingerprint) VALUES(?,?,?,?,?,?,?)`, slot.ID, r.RepositoryID, r.WorktreePath, r.State, r.RequestedRef, r.BaseOID, r.Fingerprint); err != nil {
			return Job{}, err
		}
	}
	if err := insertJob(ctx, tx, job); err != nil {
		return Job{}, err
	}
	return job, tx.Commit()
}

func (s *Store) LeaseReady(ctx context.Context, slotID string, session Session) error {
	s.writer.Lock()
	defer s.writer.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `UPDATE slots SET state='LEASED',owner_session_id=?,last_used_at=?,updated_at=? WHERE id=? AND state='READY'`, session.ID, now(), now(), slotID)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		return errors.New("slot is no longer READY")
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO sessions(id,workspace_id,slot_id,state,agent_kind,client_pid,session_token_hash,created_at) VALUES(?,?,?,?,?,?,?,?)`, session.ID, session.WorkspaceID, slotID, session.State, session.AgentKind, session.ClientPID, session.TokenHash, now())
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE repositories SET last_leased_at=? WHERE id IN (SELECT repository_id FROM workspace_repositories WHERE workspace_id=?)`, now(), session.WorkspaceID)
	if err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) RecordLease(ctx context.Context, workspaceID string) error {
	s.writer.Lock()
	defer s.writer.Unlock()
	_, err := s.db.ExecContext(ctx, `UPDATE repositories SET last_leased_at=? WHERE id IN (SELECT repository_id FROM workspace_repositories WHERE workspace_id=?)`, now(), workspaceID)
	return err
}

func (s *Store) SetSlotState(ctx context.Context, id string, from []string, to, code string) error {
	s.writer.Lock()
	defer s.writer.Unlock()
	placeholders := strings.TrimRight(strings.Repeat("?,", len(from)), ",")
	args := []any{to, now(), nullString(code), id}
	for _, v := range from {
		args = append(args, v)
	}
	res, err := s.db.ExecContext(ctx, `UPDATE slots SET state=?,updated_at=?,failure_code=? WHERE id=? AND state IN (`+placeholders+`)`, args...)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		return fmt.Errorf("slot %s state compare-and-swap failed", id)
	}
	return nil
}
func (s *Store) MarkReady(ctx context.Context, id string) error {
	if err := s.SetSlotState(ctx, id, []string{"PREPARING", "RESTORING"}, "READY", ""); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `UPDATE slots SET ready_at=? WHERE id=?`, now(), id)
	return err
}
func (s *Store) FinishPreparation(ctx context.Context, id string) error {
	s.writer.Lock()
	defer s.writer.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	t := now()
	res, err := tx.ExecContext(ctx, `UPDATE slots SET state=CASE WHEN owner_session_id IS NULL THEN 'READY' ELSE 'LEASED' END,ready_at=?,updated_at=? WHERE id=? AND state IN ('PREPARING','RESTORING')`, t, t, id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		return fmt.Errorf("slot %s preparation state changed", id)
	}
	if _, err = tx.ExecContext(ctx, `UPDATE sessions SET state='ACTIVE',started_at=COALESCE(started_at,?) WHERE slot_id=? AND state IN ('STARTING','RESTORING')`, t, id); err != nil {
		return err
	}
	return tx.Commit()
}
func (s *Store) MarkSessionState(ctx context.Context, id string, from []string, to string) error {
	s.writer.Lock()
	defer s.writer.Unlock()
	ph := strings.TrimRight(strings.Repeat("?,", len(from)), ",")
	args := []any{to, id}
	for _, v := range from {
		args = append(args, v)
	}
	res, err := s.db.ExecContext(ctx, `UPDATE sessions SET state=?,started_at=CASE WHEN ?='ACTIVE' THEN COALESCE(started_at,`+quoteNow()+`) ELSE started_at END,released_at=CASE WHEN ?='RELEASING' THEN COALESCE(released_at,`+quoteNow()+`) ELSE released_at END WHERE id=? AND state IN (`+ph+`)`, append([]any{to, to, to, id}, stringsToAny(from)...)...)
	_ = args
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		return fmt.Errorf("session %s state compare-and-swap failed", id)
	}
	return nil
}
func quoteNow() string { return "'" + now() + "'" }
func stringsToAny(v []string) []any {
	a := make([]any, len(v))
	for i := range v {
		a[i] = v[i]
	}
	return a
}

func (s *Store) Session(ctx context.Context, id, token string) (Session, error) {
	var x Session
	err := s.db.QueryRowContext(ctx, `SELECT id,COALESCE(workspace_id,''),slot_id,COALESCE(parent_session_id,''),state,agent_kind,COALESCE(agent_session_id,''),COALESCE(client_pid,0),session_token_hash,created_at,COALESCE(released_at,''),COALESCE(archived_at,''),COALESCE(expires_at,'') FROM sessions WHERE id=?`, id).Scan(&x.ID, &x.WorkspaceID, &x.SlotID, &x.ParentSessionID, &x.State, &x.AgentKind, &x.AgentSessionID, &x.ClientPID, &x.TokenHash, &x.CreatedAt, &x.ReleasedAt, &x.ArchivedAt, &x.ExpiresAt)
	if err != nil {
		return Session{}, err
	}
	if !equalHash(x.TokenHash, HashToken(token)) {
		return Session{}, errors.New("session authentication failed")
	}
	return x, nil
}
func (s *Store) SessionByID(ctx context.Context, id string) (Session, error) {
	var x Session
	err := s.db.QueryRowContext(ctx, `SELECT id,COALESCE(workspace_id,''),slot_id,COALESCE(parent_session_id,''),state,agent_kind,COALESCE(agent_session_id,''),COALESCE(client_pid,0),session_token_hash,created_at,COALESCE(released_at,''),COALESCE(archived_at,''),COALESCE(expires_at,'') FROM sessions WHERE id=?`, id).Scan(&x.ID, &x.WorkspaceID, &x.SlotID, &x.ParentSessionID, &x.State, &x.AgentKind, &x.AgentSessionID, &x.ClientPID, &x.TokenHash, &x.CreatedAt, &x.ReleasedAt, &x.ArchivedAt, &x.ExpiresAt)
	return x, err
}
func equalHash(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	var v byte
	for i := range a {
		v |= a[i] ^ b[i]
	}
	return v == 0
}

func (s *Store) BindAgentSession(ctx context.Context, id, agentID string) error {
	s.writer.Lock()
	defer s.writer.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var kind, parent string
	if err := tx.QueryRowContext(ctx, `SELECT agent_kind,COALESCE(parent_session_id,'') FROM sessions WHERE id=?`, id).Scan(&kind, &parent); err != nil {
		return err
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

func (s *Store) BindFreshSession(ctx context.Context, id, parentID, agentID string) error {
	s.writer.Lock()
	defer s.writer.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var kind string
	if err := tx.QueryRowContext(ctx, `SELECT agent_kind FROM sessions WHERE id=?`, id).Scan(&kind); err != nil {
		return err
	}
	if parentID != "" {
		if _, err := tx.ExecContext(ctx, `UPDATE sessions SET agent_session_id=NULL WHERE id=? AND state='EXPIRED'`, parentID); err != nil {
			return err
		}
	}
	res, err := tx.ExecContext(ctx, `UPDATE sessions SET parent_session_id=?,agent_session_id=?,state='ACTIVE',started_at=COALESCE(started_at,?),last_heartbeat_at=? WHERE id=? AND state IN ('STARTING','ACTIVE')`, nullString(parentID), agentID, now(), now(), id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		return errors.New("fresh session state changed")
	}
	return tx.Commit()
}
func (s *Store) FindByAgentSession(ctx context.Context, kind, agentID string) (Session, error) {
	var x Session
	err := s.db.QueryRowContext(ctx, `SELECT id,COALESCE(workspace_id,''),slot_id,COALESCE(parent_session_id,''),state,agent_kind,COALESCE(agent_session_id,''),COALESCE(client_pid,0),session_token_hash,created_at,COALESCE(released_at,''),COALESCE(archived_at,''),COALESCE(expires_at,'') FROM sessions WHERE agent_kind=? AND agent_session_id=?`, kind, agentID).Scan(&x.ID, &x.WorkspaceID, &x.SlotID, &x.ParentSessionID, &x.State, &x.AgentKind, &x.AgentSessionID, &x.ClientPID, &x.TokenHash, &x.CreatedAt, &x.ReleasedAt, &x.ArchivedAt, &x.ExpiresAt)
	return x, err
}

func (s *Store) Heartbeat(ctx context.Context, id, token string) error {
	if _, err := s.Session(ctx, id, token); err != nil {
		return err
	}
	s.writer.Lock()
	defer s.writer.Unlock()
	res, err := s.db.ExecContext(ctx, `UPDATE sessions SET last_heartbeat_at=? WHERE id=? AND state IN ('STARTING','ACTIVE','RESTORING')`, now(), id)
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
	ClientPID               int
}

func (s *Store) OrphanCandidates(ctx context.Context, heartbeatBefore string) ([]OrphanCandidate, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,COALESCE(workspace_id,''),slot_id,COALESCE(client_pid,0) FROM sessions WHERE state IN ('STARTING','ACTIVE') AND COALESCE(last_heartbeat_at,created_at)<=?`, heartbeatBefore)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []OrphanCandidate
	for rows.Next() {
		var candidate OrphanCandidate
		if err := rows.Scan(&candidate.ID, &candidate.WorkspaceID, &candidate.SlotID, &candidate.ClientPID); err != nil {
			return nil, err
		}
		out = append(out, candidate)
	}
	return out, rows.Err()
}
func (s *Store) BindResumeSlot(ctx context.Context, sessionID, parentSessionID, workspaceID, agentID string, generation int, repos []SlotRepository) (Job, error) {
	job, err := newJob("RESTORE", workspaceID, sessionID, sessionID)
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
	var kind string
	if err := tx.QueryRowContext(ctx, `SELECT agent_kind FROM sessions WHERE id=?`, sessionID).Scan(&kind); err != nil {
		return Job{}, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE sessions SET agent_session_id=NULL WHERE agent_kind=? AND agent_session_id=? AND id<>?`, kind, agentID, sessionID); err != nil {
		return Job{}, err
	}
	res, err := tx.ExecContext(ctx, `UPDATE sessions SET workspace_id=?,parent_session_id=?,agent_session_id=?,state='RESTORING' WHERE id=? AND state='UNBOUND'`, workspaceID, parentSessionID, agentID, sessionID)
	if err != nil {
		return Job{}, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return Job{}, errors.New("resume session is no longer UNBOUND")
	}
	if res, err = tx.ExecContext(ctx, `UPDATE slots SET workspace_id=?,generation=?,state='RESTORING',updated_at=? WHERE id=? AND state='UNBOUND'`, workspaceID, generation, now(), sessionID); err != nil {
		return Job{}, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return Job{}, errors.New("resume slot is no longer UNBOUND")
	}
	for _, r := range repos {
		if _, err = tx.ExecContext(ctx, `INSERT INTO slot_repositories(slot_id,repository_id,worktree_path,state,requested_ref,base_oid,prepare_fingerprint) VALUES(?,?,?,?,?,?,?)`, sessionID, r.RepositoryID, r.WorktreePath, "RESTORING", r.RequestedRef, r.BaseOID, r.Fingerprint); err != nil {
			return Job{}, err
		}
	}
	if err := insertJob(ctx, tx, job); err != nil {
		return Job{}, err
	}
	return job, tx.Commit()
}
func (s *Store) SlotRepositories(ctx context.Context, slotID string) ([]SlotRepository, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT repository_id,worktree_path,state,requested_ref,base_oid,prepare_fingerprint FROM slot_repositories WHERE slot_id=? ORDER BY repository_id`, slotID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SlotRepository
	for rows.Next() {
		var x SlotRepository
		if err := rows.Scan(&x.RepositoryID, &x.WorktreePath, &x.State, &x.RequestedRef, &x.BaseOID, &x.Fingerprint); err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

func (s *Store) SlotRepository(ctx context.Context, slotID, repositoryID string) (SlotRepository, error) {
	var x SlotRepository
	err := s.db.QueryRowContext(ctx, `SELECT repository_id,worktree_path,state,requested_ref,base_oid,prepare_fingerprint FROM slot_repositories WHERE slot_id=? AND repository_id=?`, slotID, repositoryID).Scan(&x.RepositoryID, &x.WorktreePath, &x.State, &x.RequestedRef, &x.BaseOID, &x.Fingerprint)
	return x, err
}

func (s *Store) SetSlotRepositoryState(ctx context.Context, slotID, repositoryID string, from []string, to string) error {
	s.writer.Lock()
	defer s.writer.Unlock()
	placeholders := strings.TrimRight(strings.Repeat("?,", len(from)), ",")
	args := []any{to, slotID, repositoryID}
	for _, value := range from {
		args = append(args, value)
	}
	res, err := s.db.ExecContext(ctx, `UPDATE slot_repositories SET state=? WHERE slot_id=? AND repository_id=? AND state IN (`+placeholders+`)`, args...)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return fmt.Errorf("slot repository %s/%s state compare-and-swap failed", slotID, repositoryID)
	}
	return nil
}
func (s *Store) Slot(ctx context.Context, id string) (Slot, error) {
	var x Slot
	err := s.db.QueryRowContext(ctx, `SELECT id,COALESCE(workspace_id,''),generation,path,state,COALESCE(owner_session_id,''),created_at,COALESCE(ready_at,'') FROM slots WHERE id=?`, id).Scan(&x.ID, &x.WorkspaceID, &x.Generation, &x.Path, &x.State, &x.OwnerSessionID, &x.CreatedAt, &x.ReadyAt)
	return x, err
}

func (s *Store) SaveSnapshot(ctx context.Context, x Snapshot) error {
	s.writer.Lock()
	defer s.writer.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	_, err = tx.ExecContext(ctx, `INSERT INTO snapshots(id,session_id,repository_id,head_oid,head_recovery_ref,index_tree_oid,worktree_snapshot_oid,worktree_recovery_ref,status,created_at,expires_at) VALUES(?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(session_id,repository_id) DO NOTHING`, x.ID, x.SessionID, x.RepositoryID, x.HeadOID, x.HeadRef, x.IndexTreeOID, x.WorktreeOID, x.WorktreeRef, x.Status, x.CreatedAt, x.ExpiresAt)
	if err != nil {
		return err
	}
	var existing Snapshot
	if err := tx.QueryRowContext(ctx, `SELECT id,session_id,repository_id,head_oid,head_recovery_ref,index_tree_oid,worktree_snapshot_oid,worktree_recovery_ref,status,created_at,expires_at FROM snapshots WHERE session_id=? AND repository_id=?`, x.SessionID, x.RepositoryID).Scan(&existing.ID, &existing.SessionID, &existing.RepositoryID, &existing.HeadOID, &existing.HeadRef, &existing.IndexTreeOID, &existing.WorktreeOID, &existing.WorktreeRef, &existing.Status, &existing.CreatedAt, &existing.ExpiresAt); err != nil {
		return err
	}
	if existing.ID != x.ID || existing.HeadOID != x.HeadOID || existing.HeadRef != x.HeadRef || existing.IndexTreeOID != x.IndexTreeOID || existing.WorktreeOID != x.WorktreeOID || existing.WorktreeRef != x.WorktreeRef {
		return errors.New("snapshot metadata conflicts with an existing recovery snapshot")
	}
	return tx.Commit()
}
func (s *Store) Snapshots(ctx context.Context, sessionID string) ([]Snapshot, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,session_id,repository_id,head_oid,head_recovery_ref,index_tree_oid,worktree_snapshot_oid,worktree_recovery_ref,status,created_at,expires_at FROM snapshots WHERE session_id=? ORDER BY repository_id`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Snapshot
	for rows.Next() {
		var x Snapshot
		if err := rows.Scan(&x.ID, &x.SessionID, &x.RepositoryID, &x.HeadOID, &x.HeadRef, &x.IndexTreeOID, &x.WorktreeOID, &x.WorktreeRef, &x.Status, &x.CreatedAt, &x.ExpiresAt); err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}
func (s *Store) Repository(ctx context.Context, id string) (discovery.Repository, error) {
	var r discovery.Repository
	err := s.db.QueryRowContext(ctx, `SELECT id,main_worktree_path,common_git_dir,default_branch FROM repositories WHERE id=?`, id).Scan(&r.ID, &r.MainPath, &r.CommonDir, &r.DefaultBranch)
	return r, err
}
func (s *Store) Workspace(ctx context.Context, id string) (discovery.Workspace, error) {
	var w discovery.Workspace
	err := s.db.QueryRowContext(ctx, `SELECT id,root_path,kind FROM workspaces WHERE id=?`, id).Scan(&w.ID, &w.Root, &w.Kind)
	if err != nil {
		return w, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT r.id,r.main_worktree_path,r.common_git_dir,wr.relative_path,r.default_branch FROM workspace_repositories wr JOIN repositories r ON r.id=wr.repository_id WHERE wr.workspace_id=? ORDER BY wr.ordinal`, id)
	if err != nil {
		return w, err
	}
	defer rows.Close()
	for rows.Next() {
		var r discovery.Repository
		if err := rows.Scan(&r.ID, &r.MainPath, &r.CommonDir, &r.RelativePath, &r.DefaultBranch); err != nil {
			return w, err
		}
		w.Repositories = append(w.Repositories, r)
	}
	return w, rows.Err()
}

type Status struct{ Workspaces, Repositories, Ready, Leased, Failed, Active, Snapshots, Jobs, Quarantined int }

type SessionSummary struct {
	ID             string `json:"id"`
	WorkspaceID    string `json:"workspace_id,omitempty"`
	State          string `json:"state"`
	AgentKind      string `json:"agent"`
	AgentSessionID string `json:"agent_session_id,omitempty"`
	CreatedAt      string `json:"created_at"`
	ArchivedAt     string `json:"archived_at,omitempty"`
	ExpiresAt      string `json:"expires_at,omitempty"`
}

func (s *Store) ListSessions(ctx context.Context, all bool) ([]SessionSummary, error) {
	q := `SELECT id,COALESCE(workspace_id,''),state,agent_kind,COALESCE(agent_session_id,''),created_at,COALESCE(archived_at,''),COALESCE(expires_at,'') FROM sessions`
	if !all {
		q += ` WHERE state<>'EXPIRED'`
	}
	q += ` ORDER BY created_at DESC`
	rows, err := s.db.QueryContext(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SessionSummary
	for rows.Next() {
		var x SessionSummary
		if err := rows.Scan(&x.ID, &x.WorkspaceID, &x.State, &x.AgentKind, &x.AgentSessionID, &x.CreatedAt, &x.ArchivedAt, &x.ExpiresAt); err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

func (s *Store) ForgetWorkspace(ctx context.Context, root string) error {
	s.writer.Lock()
	defer s.writer.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var id string
	if err := tx.QueryRowContext(ctx, `SELECT id FROM workspaces WHERE root_path=?`, root).Scan(&id); err != nil {
		return err
	}
	var unsafe int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM slots WHERE workspace_id=? AND state NOT IN ('ARCHIVED','FAILED')`, id).Scan(&unsafe); err != nil {
		return err
	}
	if unsafe > 0 {
		return errors.New("workspace has active, ready, or unarchived slots")
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM workspace_repositories WHERE workspace_id=?`, id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM workspaces WHERE id=?`, id); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) Status(ctx context.Context) (Status, error) {
	var x Status
	queries := []struct {
		dst *int
		q   string
	}{{&x.Workspaces, "SELECT count(*) FROM workspaces"}, {&x.Repositories, "SELECT count(*) FROM repositories"}, {&x.Ready, "SELECT count(*) FROM slots WHERE state='READY'"}, {&x.Leased, "SELECT count(*) FROM slots WHERE state='LEASED'"}, {&x.Failed, "SELECT count(*) FROM slots WHERE state='FAILED'"}, {&x.Active, "SELECT count(*) FROM sessions WHERE state='ACTIVE'"}, {&x.Snapshots, "SELECT count(*) FROM snapshots WHERE status='ARCHIVED'"}, {&x.Jobs, "SELECT count(*) FROM jobs WHERE state IN ('PENDING','RUNNING')"}, {&x.Quarantined, "SELECT count(*) FROM slots WHERE state='QUARANTINED'"}}
	for _, q := range queries {
		if err := s.db.QueryRowContext(ctx, q.q).Scan(q.dst); err != nil {
			return x, err
		}
	}
	return x, nil
}

func (s *Store) MarkArchived(ctx context.Context, sessionID, slotID, expiry string) error {
	s.writer.Lock()
	defer s.writer.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `UPDATE sessions SET state='ARCHIVED',archived_at=COALESCE(archived_at,?),expires_at=COALESCE(expires_at,?) WHERE id=? AND state IN ('RELEASING','SNAPSHOTTING','ARCHIVED')`, now(), expiry, sessionID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return errors.New("session cannot be archived from its current state")
	}
	res, err = tx.ExecContext(ctx, `UPDATE slots SET state='SNAPSHOTTED',updated_at=? WHERE id=? AND state IN ('DRAINING','SNAPSHOTTING','SNAPSHOTTED')`, now(), slotID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return errors.New("slot cannot be marked snapshotted from its current state")
	}
	return tx.Commit()
}

func (s *Store) BeginSnapshot(ctx context.Context, sessionID, slotID string) error {
	s.writer.Lock()
	defer s.writer.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `UPDATE sessions SET state='SNAPSHOTTING' WHERE id=? AND state IN ('RELEASING','SNAPSHOTTING')`, sessionID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return errors.New("session cannot begin snapshot from its current state")
	}
	res, err = tx.ExecContext(ctx, `UPDATE slots SET state='SNAPSHOTTING',updated_at=? WHERE id=? AND state IN ('DRAINING','SNAPSHOTTING')`, now(), slotID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return errors.New("slot cannot begin snapshot from its current state")
	}
	return tx.Commit()
}

func (s *Store) Release(ctx context.Context, sessionID, workspaceID, slotID string) (Job, bool, error) {
	job, err := newJob("SNAPSHOT", workspaceID, slotID, sessionID)
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
	res, err := tx.ExecContext(ctx, `UPDATE sessions SET state='RELEASING',released_at=COALESCE(released_at,?) WHERE id=? AND state IN ('STARTING','ACTIVE')`, now(), sessionID)
	if err != nil {
		return Job{}, false, err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return Job{}, false, nil
	}
	if _, err = tx.ExecContext(ctx, `UPDATE slots SET state='DRAINING',updated_at=? WHERE owner_session_id=? AND state='LEASED'`, now(), sessionID); err != nil {
		return Job{}, false, err
	}
	if err := insertJob(ctx, tx, job); err != nil {
		return Job{}, false, err
	}
	return job, true, tx.Commit()
}

type GCCandidate struct{ SlotID, SessionID, Path string }

type StandbyGCCandidate struct{ SlotID, WorkspaceID, Path, State string }

func (s *Store) StandbyGCCandidates(ctx context.Context, hotBefore string, warm int) ([]StandbyGCCandidate, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT sl.id,sl.workspace_id,sl.path,sl.state,COALESCE(sl.ready_at,sl.created_at),COALESCE(MIN(r.last_leased_at),'') FROM slots sl LEFT JOIN slot_repositories sr ON sr.slot_id=sl.id LEFT JOIN repositories r ON r.id=sr.repository_id WHERE sl.owner_session_id IS NULL AND sl.state IN ('READY','STALE') GROUP BY sl.id ORDER BY sl.workspace_id,COALESCE(sl.ready_at,sl.created_at) DESC,sl.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	kept := map[string]int{}
	var out []StandbyGCCandidate
	for rows.Next() {
		var candidate StandbyGCCandidate
		var readyAt, lastLeased string
		if err := rows.Scan(&candidate.SlotID, &candidate.WorkspaceID, &candidate.Path, &candidate.State, &readyAt, &lastLeased); err != nil {
			return nil, err
		}
		cold := lastLeased == "" || lastLeased <= hotBefore
		if candidate.State == "STALE" || cold || kept[candidate.WorkspaceID] >= warm {
			out = append(out, candidate)
			continue
		}
		kept[candidate.WorkspaceID]++
	}
	return out, rows.Err()
}

func (s *Store) MarkStandbyArchived(ctx context.Context, slotID string) error {
	return s.SetSlotState(ctx, slotID, []string{"READY", "STALE"}, "ARCHIVED", "")
}

func (s *Store) ExpiredSnapshots(ctx context.Context, before string) ([]Snapshot, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT sn.id,sn.session_id,sn.repository_id,sn.head_oid,sn.head_recovery_ref,sn.index_tree_oid,sn.worktree_snapshot_oid,sn.worktree_recovery_ref,sn.status,sn.created_at,sn.expires_at FROM snapshots sn JOIN sessions se ON se.id=sn.session_id WHERE se.state='ARCHIVED' AND sn.status='ARCHIVED' AND sn.expires_at<=? AND NOT EXISTS (SELECT 1 FROM sessions child JOIN jobs j ON j.session_id=child.id WHERE child.parent_session_id=se.id AND j.kind='RESTORE' AND j.state IN ('PENDING','RUNNING')) ORDER BY sn.session_id,sn.repository_id`, before)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Snapshot
	for rows.Next() {
		var snapshot Snapshot
		if err := rows.Scan(&snapshot.ID, &snapshot.SessionID, &snapshot.RepositoryID, &snapshot.HeadOID, &snapshot.HeadRef, &snapshot.IndexTreeOID, &snapshot.WorktreeOID, &snapshot.WorktreeRef, &snapshot.Status, &snapshot.CreatedAt, &snapshot.ExpiresAt); err != nil {
			return nil, err
		}
		out = append(out, snapshot)
	}
	return out, rows.Err()
}

func (s *Store) ExpireSessionSnapshots(ctx context.Context, sessionID string) error {
	s.writer.Lock()
	defer s.writer.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var activeRestore int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM sessions child JOIN jobs j ON j.session_id=child.id WHERE child.parent_session_id=? AND j.kind='RESTORE' AND j.state IN ('PENDING','RUNNING')`, sessionID).Scan(&activeRestore); err != nil {
		return err
	}
	if activeRestore != 0 {
		return errors.New("recovery snapshot has an active restore job")
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM snapshots WHERE session_id=?`, sessionID); err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx, `UPDATE sessions SET state='EXPIRED' WHERE id=? AND state IN ('ARCHIVED','EXPIRED')`, sessionID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return errors.New("session cannot expire from its current state")
	}
	return tx.Commit()
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
	return tx.Commit()
}

func (s *Store) GCCandidates(ctx context.Context, before string) ([]GCCandidate, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT sl.id,se.id,sl.path FROM slots sl JOIN sessions se ON se.slot_id=sl.id WHERE sl.state='SNAPSHOTTED' AND se.archived_at<=?`, before)
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
func (s *Store) MarkSlotArchived(ctx context.Context, id string) error {
	return s.SetSlotState(ctx, id, []string{"SNAPSHOTTED"}, "ARCHIVED", "")
}

func nullString(v string) any {
	if v == "" {
		return nil
	}
	return v
}
func TokenHex() (string, error) {
	v, err := domain.NewID()
	if err != nil {
		return "", err
	}
	b := sha256.Sum256([]byte(v + now()))
	return hex.EncodeToString(b[:]), nil
}
