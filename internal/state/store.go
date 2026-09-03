package state

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	cryptorand "crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	sqlite "modernc.org/sqlite"

	"github.com/HappyOnigiri/WX/internal/discovery"
	"github.com/HappyOnigiri/WX/internal/domain"
	"github.com/HappyOnigiri/WX/migrations"
)

type Store struct {
	db     *sql.DB
	writer sync.Mutex
	path   string
	rpcKey []byte
}

const SchemaVersion = 9

func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	if err := os.Chmod(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	rpcKey, err := loadOrCreateRPCKey(path + ".rpc-key")
	if err != nil {
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
	s := &Store{db: db, path: path, rpcKey: rpcKey}
	if err := s.init(context.Background()); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}
func (s *Store) Close() error                   { return s.db.Close() }
func (s *Store) Ping(ctx context.Context) error { return s.db.PingContext(ctx) }

func (s *Store) BeginRPCRequest(ctx context.Context, key, method, params string, expiresAt time.Time) ([]byte, string, string, bool, error) {
	s.writer.Lock()
	defer s.writer.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, "", "", false, err
	}
	defer tx.Rollback()
	paramsHash := rpcParamsHash(params)
	res, err := tx.ExecContext(ctx, `INSERT INTO rpc_idempotency(idempotency_key,method,params,result,error_code,error_message,completed_at,expires_at,state) VALUES(?,?,?,NULL,NULL,NULL,?,?,'PENDING') ON CONFLICT(idempotency_key) DO NOTHING`, key, method, paramsHash, now(), FormatTime(expiresAt))
	if err != nil {
		return nil, "", "", false, err
	}
	inserted, _ := res.RowsAffected()
	if inserted == 1 {
		return nil, "", "", true, tx.Commit()
	}
	var storedMethod, storedParams, requestState string
	var encrypted []byte
	if err := tx.QueryRowContext(ctx, `SELECT method,params,state,result FROM rpc_idempotency WHERE idempotency_key=? AND expires_at>?`, key, now()).Scan(&storedMethod, &storedParams, &requestState, &encrypted); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, "IDEMPOTENCY_EXPIRED", "idempotency reservation expired before it could be reused", false, nil
		}
		return nil, "", "", false, err
	}
	if storedMethod != method || storedParams != paramsHash {
		return nil, "IDEMPOTENCY_KEY_REUSE", "idempotency key was reused with a different method or payload", false, nil
	}
	if requestState == "PENDING" {
		return nil, "IDEMPOTENCY_INDETERMINATE", "a prior request crossed the durable mutation boundary without committing its response; wx will not execute it again", false, nil
	}
	if requestState != "COMPLETED" {
		return nil, "", "", false, fmt.Errorf("unknown idempotency reservation state %q", requestState)
	}
	result, errorCode, errorMessage, err := s.decryptRPCResult(key, method, paramsHash, encrypted)
	return result, errorCode, errorMessage, false, err
}

func (s *Store) CompleteRPCRequest(ctx context.Context, key, method, params string, result []byte, errorCode, errorMessage string, expiresAt time.Time) error {
	s.writer.Lock()
	defer s.writer.Unlock()
	paramsHash := rpcParamsHash(params)
	encrypted, err := s.encryptRPCResult(key, method, paramsHash, result, errorCode, errorMessage)
	if err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx, `UPDATE rpc_idempotency SET result=?,error_code=NULL,error_message=NULL,completed_at=?,expires_at=?,state='COMPLETED' WHERE idempotency_key=? AND method=? AND params=? AND state='PENDING'`, encrypted, now(), FormatTime(expiresAt), key, method, paramsHash)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return errors.New("idempotency reservation is not pending for this request")
	}
	return nil
}

type rpcResultEnvelope struct {
	Result       []byte `json:"result,omitempty"`
	ErrorCode    string `json:"error_code,omitempty"`
	ErrorMessage string `json:"error_message,omitempty"`
}

func rpcParamsHash(params string) string {
	digest := sha256.Sum256([]byte(params))
	return hex.EncodeToString(digest[:])
}

func (s *Store) encryptRPCResult(key, method, paramsHash string, result []byte, errorCode, errorMessage string) ([]byte, error) {
	block, err := aes.NewCipher(s.rpcKey)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := cryptorand.Read(nonce); err != nil {
		return nil, err
	}
	plain, err := json.Marshal(rpcResultEnvelope{Result: result, ErrorCode: errorCode, ErrorMessage: errorMessage})
	if err != nil {
		return nil, err
	}
	associated := []byte(key + "\x00" + method + "\x00" + paramsHash)
	encrypted := make([]byte, len(nonce), len(nonce)+len(plain)+aead.Overhead())
	copy(encrypted, nonce)
	return aead.Seal(encrypted, nonce, plain, associated), nil
}

func (s *Store) decryptRPCResult(key, method, paramsHash string, encrypted []byte) ([]byte, string, string, error) {
	block, err := aes.NewCipher(s.rpcKey)
	if err != nil {
		return nil, "", "", err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, "", "", err
	}
	if len(encrypted) < aead.NonceSize() {
		return nil, "", "", errors.New("encrypted RPC result is truncated")
	}
	nonce, ciphertext := encrypted[:aead.NonceSize()], encrypted[aead.NonceSize():]
	associated := []byte(key + "\x00" + method + "\x00" + paramsHash)
	plain, err := aead.Open(nil, nonce, ciphertext, associated)
	if err != nil {
		return nil, "", "", errors.New("encrypted RPC result authentication failed")
	}
	var envelope rpcResultEnvelope
	if err := json.Unmarshal(plain, &envelope); err != nil {
		return nil, "", "", err
	}
	return envelope.Result, envelope.ErrorCode, envelope.ErrorMessage, nil
}

func loadOrCreateRPCKey(path string) ([]byte, error) {
	for {
		info, err := os.Lstat(path)
		if err == nil {
			if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
				return nil, errors.New("RPC idempotency key file is not an owner-only regular file")
			}
			key, err := os.ReadFile(path)
			if err != nil {
				return nil, err
			}
			if len(key) != 32 {
				return nil, errors.New("RPC idempotency key file has an invalid length")
			}
			return key, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		key := make([]byte, 32)
		if _, err := cryptorand.Read(key); err != nil {
			return nil, err
		}
		// The key is written to a private temporary name first and only made
		// visible at path via Link, which fails atomically if a concurrent
		// caller already published one. Publishing through O_CREATE|O_EXCL at
		// path directly would let another goroutine observe the filename via
		// Lstat before Write/Sync/Close finished, reading a torn (partial or
		// empty) key instead of retrying against the eventual winner.
		temporary, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp-*")
		if err != nil {
			return nil, err
		}
		temporaryPath := temporary.Name()
		written, writeErr := temporary.Write(key)
		if writeErr == nil && written != len(key) {
			writeErr = io.ErrShortWrite
		}
		if writeErr == nil {
			writeErr = temporary.Sync()
		}
		closeErr := temporary.Close()
		if writeErr != nil {
			_ = os.Remove(temporaryPath)
			return nil, writeErr
		}
		if closeErr != nil {
			_ = os.Remove(temporaryPath)
			return nil, closeErr
		}
		linkErr := os.Link(temporaryPath, path)
		_ = os.Remove(temporaryPath)
		if errors.Is(linkErr, os.ErrExist) {
			continue
		}
		if linkErr != nil {
			return nil, linkErr
		}
		if directory, err := os.Open(filepath.Dir(path)); err == nil {
			syncErr := directory.Sync()
			_ = directory.Close()
			if syncErr != nil {
				return nil, syncErr
			}
		} else {
			return nil, err
		}
		return key, nil
	}
}

func (s *Store) Backup(ctx context.Context, generations int, retention time.Duration) (string, error) {
	s.writer.Lock()
	defer s.writer.Unlock()
	dir := s.path + ".backups"
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	destination := filepath.Join(dir, time.Now().UTC().Format("20060102T150405.000000000Z")+".db")
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return "", err
	}
	defer func() { _ = conn.Close() }()
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
	if err := os.Chmod(destination, 0o600); err != nil {
		return "", err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() > entries[j].Name() })
	cutoff := time.Now().Add(-retention)
	backupIndex := 0
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".db" {
			continue
		}
		info, infoErr := entry.Info()
		if infoErr != nil {
			return "", infoErr
		}
		if backupIndex >= generations || (retention > 0 && info.ModTime().Before(cutoff)) {
			if err := os.Remove(filepath.Join(dir, entry.Name())); err != nil {
				return "", err
			}
		}
		backupIndex++
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

const timestampFormat = "2006-01-02T15:04:05.000000000Z07:00"

// FormatTime returns a fixed-width RFC 3339 timestamp. SQLite stores wx times
// as TEXT, so a fixed fractional width is required for lexical comparisons to
// preserve chronological order.
func FormatTime(value time.Time) string { return value.UTC().Format(timestampFormat) }

func now() string                   { return FormatTime(time.Now()) }
func HashToken(token string) []byte { sum := sha256.Sum256([]byte(token)); return sum[:] }

func (s *Store) UpsertWorkspace(ctx context.Context, w discovery.Workspace) error {
	_, err := s.UpsertWorkspaceGeneration(ctx, w)
	return err
}

// CanonicalWorkspace returns the workspace identity already registered for a
// repository common directory. Repository workspace IDs are derived from the
// common directory for new registrations, but older databases may contain an
// ID derived from the former main-worktree path. Keeping that existing ID is
// important: slot paths contain it, and changing it would strand the existing
// pool and create a second workspace after a main-worktree move.
func (s *Store) CanonicalWorkspace(ctx context.Context, w discovery.Workspace) (discovery.Workspace, error) {
	s.writer.Lock()
	defer s.writer.Unlock()
	return s.canonicalWorkspace(ctx, w)
}

func (s *Store) canonicalWorkspace(ctx context.Context, w discovery.Workspace) (discovery.Workspace, error) {
	if w.Kind != "repository" || len(w.Repositories) != 1 {
		return w, nil
	}
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT w.id FROM workspaces w JOIN workspace_repositories wr ON wr.workspace_id=w.id JOIN repositories r ON r.id=wr.repository_id WHERE w.kind='repository' AND r.common_git_dir=? ORDER BY w.id`, w.Repositories[0].CommonDir)
	if err != nil {
		return w, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return w, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return w, err
	}
	if len(ids) > 1 {
		return w, fmt.Errorf("Git common directory %s belongs to multiple registered workspaces", w.Repositories[0].CommonDir)
	}
	if len(ids) == 1 {
		w.ID = domain.WorkspaceID(ids[0])
	}
	return w, nil
}

func (s *Store) UpsertWorkspaceGeneration(ctx context.Context, w discovery.Workspace) (int, error) {
	s.writer.Lock()
	defer s.writer.Unlock()
	canonical, err := s.canonicalWorkspace(ctx, w)
	if err != nil {
		return 0, err
	}
	w = canonical
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
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var id string
			var member membership
			if err := rows.Scan(&id, &member.rel, &member.ordinal); err != nil {
				_ = rows.Close()
				return 0, err
			}
			oldMembers[id] = member
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return 0, err
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
type (
	SlotRepository struct{ RepositoryID, WorktreePath, State, RequestedRef, BaseOID, Fingerprint string }
	Session        struct {
		ID, WorkspaceID, SlotID, ParentSessionID, State, AgentKind, AgentSessionID, CreatedAt, ReleasedAt, ArchivedAt, ExpiresAt string
		TokenHash                                                                                                                []byte
		ClientPID, AgentPID                                                                                                      int
	}
)

type (
	Snapshot               struct{ ID, SessionID, RepositoryID, HeadOID, HeadRef, IndexTreeOID, WorktreeOID, WorktreeRef, Status, CreatedAt, ExpiresAt string }
	RecoveryRefExpectation struct {
		Ref, OID, SessionID, SessionState string
		InFlight                          bool
	}
	WorkspaceSnapshot struct {
		SessionID, ArchivePath, SHA256, Status, CreatedAt, ExpiresAt string
	}
	Job struct {
		ID, Kind, WorkspaceID, SlotID, SessionID, RepositoryID, State string
		Attempt                                                       int
	}
)

const jobLease = 30 * time.Second

func newJob(kind, workspaceID, slotID, sessionID string) (Job, error) {
	id, err := domain.NewID()
	if err != nil {
		return Job{}, err
	}
	return Job{ID: id, Kind: kind, WorkspaceID: workspaceID, SlotID: slotID, SessionID: sessionID, State: "PENDING"}, nil
}

func insertCurrentSessionRepositories(ctx context.Context, tx *sql.Tx, sessionID, workspaceID, slotID string) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO session_repositories(session_id,repository_id,relative_path,ordinal) SELECT ?,sr.repository_id,wr.relative_path,wr.ordinal FROM slot_repositories sr JOIN workspace_repositories wr ON wr.workspace_id=? AND wr.repository_id=sr.repository_id WHERE sr.slot_id=? ORDER BY wr.ordinal`, sessionID, workspaceID, slotID)
	return err
}

func copySessionRepositories(ctx context.Context, tx *sql.Tx, sessionID, parentSessionID string) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO session_repositories(session_id,repository_id,relative_path,ordinal) SELECT ?,repository_id,relative_path,ordinal FROM session_repositories WHERE session_id=? ORDER BY ordinal`, sessionID, parentSessionID)
	return err
}

func updateWorkspaceLeaseTimestamps(ctx context.Context, tx *sql.Tx, workspaceID, leasedAt string) error {
	_, err := tx.ExecContext(ctx, `UPDATE repositories SET last_leased_at=? WHERE id IN (SELECT repository_id FROM workspace_repositories WHERE workspace_id=?)`, leasedAt, workspaceID)
	return err
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
	var j Job
	if err := tx.QueryRowContext(ctx, `SELECT id,kind,COALESCE(workspace_id,''),COALESCE(slot_id,''),COALESCE(session_id,''),COALESCE(repository_id,''),state,attempt FROM jobs WHERE id=?`, id).Scan(&j.ID, &j.Kind, &j.WorkspaceID, &j.SlotID, &j.SessionID, &j.RepositoryID, &j.State, &j.Attempt); err != nil {
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
		code = "JOB_FAILED"
	}
	var startedAt string
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(started_at,'') FROM jobs WHERE id=? AND state='RUNNING' AND lease_owner=?`, id, owner).Scan(&startedAt); err != nil {
		return errors.New("job cannot be finished without its active lease")
	}
	finishedAt := time.Now()
	res, err := tx.ExecContext(ctx, `UPDATE jobs SET state=?,finished_at=?,lease_owner=NULL,lease_expires_at=NULL,error_code=? WHERE id=? AND state='RUNNING' AND lease_owner=?`, stateName, FormatTime(finishedAt), code, id, owner)
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

// DeferJob waits for a durable dependency without consuming the retry budget.
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

func (s *Store) ReadySlot(ctx context.Context, workspaceID string) (Slot, bool, error) {
	var x Slot
	err := s.db.QueryRowContext(ctx, `SELECT sl.id,sl.workspace_id,sl.generation,sl.path,sl.state,COALESCE(sl.owner_session_id,''),sl.created_at,COALESCE(sl.ready_at,'') FROM slots sl JOIN workspaces w ON w.id=sl.workspace_id WHERE sl.workspace_id=? AND sl.generation=w.generation AND sl.state='READY' ORDER BY sl.ready_at LIMIT 1`, workspaceID).Scan(&x.ID, &x.WorkspaceID, &x.Generation, &x.Path, &x.State, &x.OwnerSessionID, &x.CreatedAt, &x.ReadyAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Slot{}, false, nil
	}
	return x, err == nil, err
}

func (s *Store) ReadySlots(ctx context.Context, workspaceID string) ([]Slot, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,workspace_id,generation,path,state,COALESCE(owner_session_id,''),created_at,COALESCE(ready_at,'') FROM slots WHERE workspace_id=? AND state='READY' AND owner_session_id IS NULL ORDER BY ready_at,id`, workspaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var slots []Slot
	for rows.Next() {
		var slot Slot
		if err := rows.Scan(&slot.ID, &slot.WorkspaceID, &slot.Generation, &slot.Path, &slot.State, &slot.OwnerSessionID, &slot.CreatedAt, &slot.ReadyAt); err != nil {
			return nil, err
		}
		slots = append(slots, slot)
	}
	return slots, rows.Err()
}

func (s *Store) HasStandby(ctx context.Context, workspaceID string) bool {
	return s.StandbyCount(ctx, workspaceID) > 0
}

func (s *Store) StandbyCount(ctx context.Context, workspaceID string) int {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM slots sl JOIN workspaces w ON w.id=sl.workspace_id WHERE sl.workspace_id=? AND sl.generation=w.generation AND sl.owner_session_id IS NULL AND sl.state IN ('PREPARING','READY','FAILED','QUARANTINED','RETIRING','REMOVING')`, workspaceID).Scan(&n)
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
	if err := insertCurrentSessionRepositories(ctx, tx, session.ID, session.WorkspaceID, slotID); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE repositories SET last_leased_at=? WHERE id IN (SELECT repository_id FROM workspace_repositories WHERE workspace_id=?)`, now(), session.WorkspaceID)
	if err != nil {
		return err
	}
	return tx.Commit()
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
	if _, err = tx.ExecContext(ctx, `INSERT INTO sessions(id,workspace_id,slot_id,state,agent_kind,client_pid,session_token_hash,created_at) VALUES(?,?,?,?,?,?,?,?)`, session.ID, session.WorkspaceID, slotID, "STARTING", session.AgentKind, session.ClientPID, session.TokenHash, now()); err != nil {
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
	_, err = s.db.ExecContext(ctx, `INSERT INTO events(time,level,kind,workspace_id,slot_id,message) SELECT ?,'info','slot_transition',workspace_id,id,? FROM slots WHERE id=?`, now(), "state="+to+" failure_code="+code, id)
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
	if _, err := tx.ExecContext(ctx, `UPDATE slots SET state='PREPARING',failure_code=NULL,updated_at=? WHERE id=? AND state='FAILED'`, now(), id); err != nil {
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

func (s *Store) FinishPreparation(ctx context.Context, id string) error {
	_, _, err := s.FinishPreparationWithRelease(ctx, id)
	return err
}

func (s *Store) FinishPreparationWithRelease(ctx context.Context, id string) (Job, bool, error) {
	s.writer.Lock()
	defer s.writer.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Job{}, false, err
	}
	defer tx.Rollback()
	t := now()
	var sessionID, sessionState, kind, parentID, pendingAgentID string
	err = tx.QueryRowContext(ctx, `SELECT COALESCE(se.id,''),COALESCE(se.state,''),COALESCE(se.agent_kind,''),COALESCE(se.parent_session_id,''),COALESCE(se.pending_agent_session_id,'') FROM slots sl LEFT JOIN sessions se ON se.id=sl.owner_session_id WHERE sl.id=?`, id).Scan(&sessionID, &sessionState, &kind, &parentID, &pendingAgentID)
	if err != nil {
		return Job{}, false, err
	}
	targetState := "READY"
	if sessionID != "" {
		switch sessionState {
		case "STARTING", "RESTORING":
			targetState = "LEASED"
		case "RELEASING":
			targetState = "DRAINING"
		default:
			return Job{}, false, fmt.Errorf("slot %s has owner session %s in unexpected state %s", id, sessionID, sessionState)
		}
	}
	res, err := tx.ExecContext(ctx, `UPDATE slots SET state=?,ready_at=?,updated_at=? WHERE id=? AND state IN ('PREPARING','RESTORING')`, targetState, t, t, id)
	if err != nil {
		return Job{}, false, err
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		return Job{}, false, fmt.Errorf("slot %s preparation state changed", id)
	}
	if sessionState == "RESTORING" {
		if pendingAgentID != "" {
			if parentID == "" {
				return Job{}, false, errors.New("restoring session has no parent for its pending agent mapping")
			}
			res, err = tx.ExecContext(ctx, `UPDATE sessions SET agent_session_id=NULL WHERE id=? AND agent_kind=? AND agent_session_id=?`, parentID, kind, pendingAgentID)
			if err != nil {
				return Job{}, false, err
			}
			if n, _ := res.RowsAffected(); n != 1 {
				return Job{}, false, errors.New("resume parent agent mapping changed before restore completed")
			}
			res, err = tx.ExecContext(ctx, `UPDATE sessions SET agent_session_id=?,pending_agent_session_id=NULL WHERE id=? AND state='RESTORING'`, pendingAgentID, sessionID)
			if err != nil {
				return Job{}, false, err
			}
			if n, _ := res.RowsAffected(); n != 1 {
				return Job{}, false, errors.New("restoring session changed before activation")
			}
		}
	}
	if _, err = tx.ExecContext(ctx, `UPDATE sessions SET state='ACTIVE',started_at=COALESCE(started_at,?) WHERE slot_id=? AND state IN ('STARTING','RESTORING')`, t, id); err != nil {
		return Job{}, false, err
	}
	if targetState != "DRAINING" {
		return Job{}, false, tx.Commit()
	}
	job, err := newJob("SNAPSHOT", "", id, sessionID)
	if err != nil {
		return Job{}, false, err
	}
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(workspace_id,'') FROM sessions WHERE id=?`, sessionID).Scan(&job.WorkspaceID); err != nil {
		return Job{}, false, err
	}
	if err := insertJob(ctx, tx, job); err != nil {
		return Job{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return Job{}, false, err
	}
	return job, true, nil
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
	err := s.db.QueryRowContext(ctx, `SELECT id,COALESCE(workspace_id,''),slot_id,COALESCE(parent_session_id,''),state,agent_kind,COALESCE(agent_session_id,''),COALESCE(client_pid,0),COALESCE(agent_pid,0),session_token_hash,created_at,COALESCE(released_at,''),COALESCE(archived_at,''),COALESCE(expires_at,'') FROM sessions WHERE id=?`, id).Scan(&x.ID, &x.WorkspaceID, &x.SlotID, &x.ParentSessionID, &x.State, &x.AgentKind, &x.AgentSessionID, &x.ClientPID, &x.AgentPID, &x.TokenHash, &x.CreatedAt, &x.ReleasedAt, &x.ArchivedAt, &x.ExpiresAt)
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
	err := s.db.QueryRowContext(ctx, `SELECT id,COALESCE(workspace_id,''),slot_id,COALESCE(parent_session_id,''),state,agent_kind,COALESCE(agent_session_id,''),COALESCE(client_pid,0),COALESCE(agent_pid,0),session_token_hash,created_at,COALESCE(released_at,''),COALESCE(archived_at,''),COALESCE(expires_at,'') FROM sessions WHERE id=?`, id).Scan(&x.ID, &x.WorkspaceID, &x.SlotID, &x.ParentSessionID, &x.State, &x.AgentKind, &x.AgentSessionID, &x.ClientPID, &x.AgentPID, &x.TokenHash, &x.CreatedAt, &x.ReleasedAt, &x.ArchivedAt, &x.ExpiresAt)
	return x, err
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

func (s *Store) BindFreshResumeSlot(ctx context.Context, sessionID, parentSessionID, workspaceID, agentID string, generation int, repos []SlotRepository) (Job, error) {
	job, err := newJob("PREPARE", workspaceID, sessionID, sessionID)
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
	if parentSessionID != "" {
		res, err := tx.ExecContext(ctx, `UPDATE sessions SET agent_session_id=NULL WHERE id=? AND state='EXPIRED' AND agent_session_id=?`, parentSessionID, agentID)
		if err != nil {
			return Job{}, err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return Job{}, errors.New("fresh resume parent mapping changed")
		}
	}
	res, err := tx.ExecContext(ctx, `UPDATE sessions SET workspace_id=?,parent_session_id=?,agent_session_id=?,state='STARTING',started_at=COALESCE(started_at,?),last_heartbeat_at=? WHERE id=? AND state='UNBOUND'`, workspaceID, nullString(parentSessionID), agentID, now(), now(), sessionID)
	if err != nil {
		return Job{}, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return Job{}, errors.New("fresh resume session is no longer UNBOUND")
	}
	res, err = tx.ExecContext(ctx, `UPDATE slots SET workspace_id=?,generation=?,state='PREPARING',updated_at=? WHERE id=? AND state='UNBOUND'`, workspaceID, generation, now(), sessionID)
	if err != nil {
		return Job{}, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return Job{}, errors.New("fresh resume slot is no longer UNBOUND")
	}
	for _, repository := range repos {
		if _, err := tx.ExecContext(ctx, `INSERT INTO slot_repositories(slot_id,repository_id,worktree_path,state,requested_ref,base_oid,prepare_fingerprint) VALUES(?,?,?,?,?,?,?)`, sessionID, repository.RepositoryID, repository.WorktreePath, "PREPARING", repository.RequestedRef, repository.BaseOID, repository.Fingerprint); err != nil {
			return Job{}, err
		}
	}
	if err := insertCurrentSessionRepositories(ctx, tx, sessionID, workspaceID, sessionID); err != nil {
		return Job{}, err
	}
	if err := updateWorkspaceLeaseTimestamps(ctx, tx, workspaceID, now()); err != nil {
		return Job{}, err
	}
	if err := insertJob(ctx, tx, job); err != nil {
		return Job{}, err
	}
	return job, tx.Commit()
}

func (s *Store) FindByAgentSession(ctx context.Context, kind, agentID string) (Session, error) {
	var x Session
	err := s.db.QueryRowContext(ctx, `SELECT id,COALESCE(workspace_id,''),slot_id,COALESCE(parent_session_id,''),state,agent_kind,COALESCE(agent_session_id,''),COALESCE(client_pid,0),COALESCE(agent_pid,0),session_token_hash,created_at,COALESCE(released_at,''),COALESCE(archived_at,''),COALESCE(expires_at,'') FROM sessions WHERE agent_kind=? AND agent_session_id=?`, kind, agentID).Scan(&x.ID, &x.WorkspaceID, &x.SlotID, &x.ParentSessionID, &x.State, &x.AgentKind, &x.AgentSessionID, &x.ClientPID, &x.AgentPID, &x.TokenHash, &x.CreatedAt, &x.ReleasedAt, &x.ArchivedAt, &x.ExpiresAt)
	return x, err
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

func (s *Store) OrphanCandidates(ctx context.Context, heartbeatBefore string) ([]OrphanCandidate, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,COALESCE(workspace_id,''),slot_id,COALESCE(client_pid,0),COALESCE(agent_pid,0) FROM sessions WHERE state IN ('STARTING','ACTIVE','UNBOUND','RESTORING') AND COALESCE(last_heartbeat_at,created_at)<=?`, heartbeatBefore)
	if err != nil {
		return nil, err
	}
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
	var mappedParent string
	if err := tx.QueryRowContext(ctx, `SELECT id FROM sessions WHERE id=? AND agent_kind=? AND agent_session_id=?`, parentSessionID, kind, agentID).Scan(&mappedParent); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Job{}, errors.New("resume parent agent mapping changed")
		}
		return Job{}, err
	}
	res, err := tx.ExecContext(ctx, `UPDATE sessions SET workspace_id=?,parent_session_id=?,pending_agent_session_id=?,state='RESTORING' WHERE id=? AND state='UNBOUND'`, workspaceID, parentSessionID, agentID, sessionID)
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
	if err := copySessionRepositories(ctx, tx, sessionID, parentSessionID); err != nil {
		return Job{}, err
	}
	if err := updateWorkspaceLeaseTimestamps(ctx, tx, workspaceID, now()); err != nil {
		return Job{}, err
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

func (s *Store) AddRestoringRepositories(ctx context.Context, slotID string, repos []SlotRepository) error {
	s.writer.Lock()
	defer s.writer.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var slotState string
	if err := tx.QueryRowContext(ctx, `SELECT state FROM slots WHERE id=?`, slotID).Scan(&slotState); err != nil {
		return err
	}
	if slotState != "RESTORING" {
		return errors.New("resume slot is no longer RESTORING")
	}
	var existing int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM slot_repositories WHERE slot_id=?`, slotID).Scan(&existing); err != nil {
		return err
	}
	if existing != 0 {
		if existing == len(repos) {
			return nil
		}
		return errors.New("resume repository metadata is incomplete")
	}
	for _, repo := range repos {
		if _, err := tx.ExecContext(ctx, `INSERT INTO slot_repositories(slot_id,repository_id,worktree_path,state,requested_ref,base_oid,prepare_fingerprint) VALUES(?,?,?,?,?,?,?)`, slotID, repo.RepositoryID, repo.WorktreePath, "RESTORING", repo.RequestedRef, repo.BaseOID, repo.Fingerprint); err != nil {
			return err
		}
	}
	return tx.Commit()
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

func (s *Store) SaveWorkspaceSnapshot(ctx context.Context, x WorkspaceSnapshot) error {
	s.writer.Lock()
	defer s.writer.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	_, err = tx.ExecContext(ctx, `INSERT INTO workspace_snapshots(session_id,archive_path,sha256,status,created_at,expires_at) VALUES(?,?,?,?,?,?) ON CONFLICT(session_id) DO NOTHING`, x.SessionID, x.ArchivePath, x.SHA256, x.Status, x.CreatedAt, x.ExpiresAt)
	if err != nil {
		return err
	}
	var existing WorkspaceSnapshot
	if err := tx.QueryRowContext(ctx, `SELECT session_id,archive_path,sha256,status,created_at,expires_at FROM workspace_snapshots WHERE session_id=?`, x.SessionID).Scan(&existing.SessionID, &existing.ArchivePath, &existing.SHA256, &existing.Status, &existing.CreatedAt, &existing.ExpiresAt); err != nil {
		return err
	}
	if existing.ArchivePath != x.ArchivePath || existing.SHA256 != x.SHA256 || existing.Status != x.Status || existing.ExpiresAt != x.ExpiresAt {
		return errors.New("workspace snapshot metadata conflicts with an existing recovery snapshot")
	}
	return tx.Commit()
}

func (s *Store) WorkspaceSnapshot(ctx context.Context, sessionID string) (WorkspaceSnapshot, bool, error) {
	var x WorkspaceSnapshot
	err := s.db.QueryRowContext(ctx, `SELECT session_id,archive_path,sha256,status,created_at,expires_at FROM workspace_snapshots WHERE session_id=?`, sessionID).Scan(&x.SessionID, &x.ArchivePath, &x.SHA256, &x.Status, &x.CreatedAt, &x.ExpiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return WorkspaceSnapshot{}, false, nil
	}
	return x, err == nil, err
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

func (s *Store) WorkspaceByRoot(ctx context.Context, root string) (discovery.Workspace, error) {
	var id string
	if err := s.db.QueryRowContext(ctx, `SELECT id FROM workspaces WHERE root_path=?`, root).Scan(&id); err != nil {
		return discovery.Workspace{}, err
	}
	return s.Workspace(ctx, id)
}

func (s *Store) SessionWorkspace(ctx context.Context, sessionID string) (discovery.Workspace, error) {
	var w discovery.Workspace
	err := s.db.QueryRowContext(ctx, `SELECT w.id,w.root_path,w.kind FROM sessions se JOIN workspaces w ON w.id=se.workspace_id WHERE se.id=?`, sessionID).Scan(&w.ID, &w.Root, &w.Kind)
	if err != nil {
		return w, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT r.id,r.main_worktree_path,r.common_git_dir,sr.relative_path,r.default_branch FROM session_repositories sr JOIN repositories r ON r.id=sr.repository_id WHERE sr.session_id=? ORDER BY sr.ordinal`, sessionID)
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
	if err := rows.Err(); err != nil {
		return w, err
	}
	if len(w.Repositories) == 0 {
		return w, errors.New("session has no recorded repository membership")
	}
	return w, nil
}

func (s *Store) WorkspaceRoots(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT root_path FROM workspaces ORDER BY root_path`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var roots []string
	for rows.Next() {
		var root string
		if err := rows.Scan(&root); err != nil {
			return nil, err
		}
		roots = append(roots, root)
	}
	return roots, rows.Err()
}

type Status struct{ Workspaces, Repositories, Ready, Leased, Failed, Active, Snapshots, Jobs, Quarantined int }

type (
	WorkspaceDiagnostic struct {
		ID           string `json:"id"`
		Root         string `json:"root"`
		Generation   int    `json:"generation"`
		Repositories int    `json:"repositories"`
		Ready        int    `json:"ready"`
		Leased       int    `json:"leased"`
		Failed       int    `json:"failed"`
	}
	SessionDiagnostic struct {
		ID         string `json:"id"`
		Agent      string `json:"agent"`
		State      string `json:"state"`
		CreatedAt  string `json:"created_at"`
		BaseOIDs   string `json:"base_oids"`
		AgeSeconds int64  `json:"age_seconds"`
	}
	RepositoryDiagnostic struct {
		ID               string `json:"id"`
		MainPath         string `json:"main_path"`
		LastUsedAt       string `json:"last_used_at,omitempty"`
		StandbyReadyAt   string `json:"standby_ready_at,omitempty"`
		StandbyExpiresAt string `json:"standby_expires_at,omitempty"`
		Hot              bool   `json:"hot"`
	}
	JobDiagnostic struct {
		Pending int `json:"pending"`
		Running int `json:"running"`
		Failed  int `json:"failed"`
	}
	SnapshotDiagnostic struct {
		Count          int    `json:"count"`
		EarliestExpiry string `json:"earliest_expiry,omitempty"`
	}
	QuarantineDiagnostic struct {
		ID          string `json:"id"`
		Path        string `json:"path"`
		FailureCode string `json:"failure_code,omitempty"`
	}
	StatusDiagnostics struct {
		Workspaces   []WorkspaceDiagnostic  `json:"workspaces"`
		Sessions     []SessionDiagnostic    `json:"sessions"`
		Repositories []RepositoryDiagnostic `json:"repositories"`
		Jobs         JobDiagnostic          `json:"jobs"`
		Snapshots    SnapshotDiagnostic     `json:"snapshots"`
		Quarantine   []QuarantineDiagnostic `json:"quarantine"`
	}
)

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
	var liveRecovery int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM sessions WHERE workspace_id=? AND state<>'EXPIRED'`, id).Scan(&liveRecovery); err != nil {
		return err
	}
	if liveRecovery > 0 {
		return errors.New("workspace has live session mappings; expire recovery state before forgetting it")
	}
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM snapshots sn JOIN sessions se ON se.id=sn.session_id WHERE se.workspace_id=?`, id).Scan(&liveRecovery); err != nil {
		return err
	}
	if liveRecovery > 0 {
		return errors.New("workspace has recovery snapshots; expire recovery state before forgetting it")
	}
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM workspace_snapshots ws JOIN sessions se ON se.id=ws.session_id WHERE se.workspace_id=?`, id).Scan(&liveRecovery); err != nil {
		return err
	}
	if liveRecovery > 0 {
		return errors.New("workspace has a workspace recovery snapshot; expire recovery state before forgetting it")
	}
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM jobs j WHERE j.state IN ('PENDING','RUNNING') AND (j.workspace_id=? OR EXISTS (SELECT 1 FROM sessions se WHERE se.id=j.session_id AND se.workspace_id=?))`, id, id).Scan(&liveRecovery); err != nil {
		return err
	}
	if liveRecovery > 0 {
		return errors.New("workspace has pending recovery jobs; wait for them before forgetting it")
	}
	if _, err := tx.ExecContext(ctx, `UPDATE slots SET workspace_id=NULL WHERE workspace_id=?`, id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE sessions SET workspace_id=NULL WHERE workspace_id=?`, id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE jobs SET workspace_id=NULL WHERE workspace_id=?`, id); err != nil {
		return err
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

func (s *Store) StatusDiagnostics(ctx context.Context) (StatusDiagnostics, error) {
	var out StatusDiagnostics
	workspaceRows, err := s.db.QueryContext(ctx, `SELECT w.id,w.root_path,w.generation,(SELECT count(*) FROM workspace_repositories wr WHERE wr.workspace_id=w.id),(SELECT count(*) FROM slots sl WHERE sl.workspace_id=w.id AND sl.state='READY'),(SELECT count(*) FROM slots sl WHERE sl.workspace_id=w.id AND sl.state='LEASED'),(SELECT count(*) FROM slots sl WHERE sl.workspace_id=w.id AND sl.state IN ('FAILED','QUARANTINED')) FROM workspaces w ORDER BY w.root_path`)
	if err != nil {
		return out, err
	}
	defer workspaceRows.Close()
	for workspaceRows.Next() {
		var item WorkspaceDiagnostic
		if err := workspaceRows.Scan(&item.ID, &item.Root, &item.Generation, &item.Repositories, &item.Ready, &item.Leased, &item.Failed); err != nil {
			return out, err
		}
		out.Workspaces = append(out.Workspaces, item)
	}
	if err := workspaceRows.Err(); err != nil {
		return out, err
	}
	if err := workspaceRows.Close(); err != nil {
		return out, err
	}
	sessionRows, err := s.db.QueryContext(ctx, `SELECT se.id,se.agent_kind,se.state,se.created_at,COALESCE(group_concat(sr.base_oid,','),'') FROM sessions se LEFT JOIN slot_repositories sr ON sr.slot_id=se.slot_id WHERE se.state<>'EXPIRED' GROUP BY se.id ORDER BY se.created_at DESC`)
	if err != nil {
		return out, err
	}
	defer sessionRows.Close()
	for sessionRows.Next() {
		var item SessionDiagnostic
		if err := sessionRows.Scan(&item.ID, &item.Agent, &item.State, &item.CreatedAt, &item.BaseOIDs); err != nil {
			return out, err
		}
		out.Sessions = append(out.Sessions, item)
	}
	if err := sessionRows.Err(); err != nil {
		return out, err
	}
	if err := sessionRows.Close(); err != nil {
		return out, err
	}
	repositoryRows, err := s.db.QueryContext(ctx, `SELECT r.id,r.main_worktree_path,COALESCE(r.last_leased_at,''),COALESCE(MAX(CASE WHEN sl.owner_session_id IS NULL AND sl.state='READY' AND sr.state='READY' THEN sl.ready_at END),''),CASE WHEN count(CASE WHEN sl.state IN ('READY','LEASED') AND sr.state IN ('READY','LEASED') THEN 1 END)>0 THEN 1 ELSE 0 END FROM repositories r LEFT JOIN slot_repositories sr ON sr.repository_id=r.id LEFT JOIN slots sl ON sl.id=sr.slot_id GROUP BY r.id ORDER BY r.main_worktree_path`)
	if err != nil {
		return out, err
	}
	defer repositoryRows.Close()
	for repositoryRows.Next() {
		var item RepositoryDiagnostic
		if err := repositoryRows.Scan(&item.ID, &item.MainPath, &item.LastUsedAt, &item.StandbyReadyAt, &item.Hot); err != nil {
			return out, err
		}
		out.Repositories = append(out.Repositories, item)
	}
	if err := repositoryRows.Err(); err != nil {
		return out, err
	}
	if err := repositoryRows.Close(); err != nil {
		return out, err
	}
	if err := s.db.QueryRowContext(ctx, `SELECT count(CASE WHEN state='PENDING' THEN 1 END),count(CASE WHEN state='RUNNING' THEN 1 END),count(CASE WHEN state='FAILED' THEN 1 END) FROM jobs`).Scan(&out.Jobs.Pending, &out.Jobs.Running, &out.Jobs.Failed); err != nil {
		return out, err
	}
	if err := s.db.QueryRowContext(ctx, `SELECT count(*),COALESCE(MIN(expires_at),'') FROM snapshots WHERE status='ARCHIVED'`).Scan(&out.Snapshots.Count, &out.Snapshots.EarliestExpiry); err != nil {
		return out, err
	}
	quarantineRows, err := s.db.QueryContext(ctx, `SELECT id,path,COALESCE(failure_code,'') FROM slots WHERE state='QUARANTINED' UNION ALL SELECT '',path,reason FROM quarantined_artifacts ORDER BY path`)
	if err != nil {
		return out, err
	}
	defer quarantineRows.Close()
	for quarantineRows.Next() {
		var item QuarantineDiagnostic
		if err := quarantineRows.Scan(&item.ID, &item.Path, &item.FailureCode); err != nil {
			return out, err
		}
		out.Quarantine = append(out.Quarantine, item)
	}
	return out, quarantineRows.Err()
}

func (s *Store) MarkArchived(ctx context.Context, sessionID, slotID, expiry string) error {
	s.writer.Lock()
	defer s.writer.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var missingWorkspaceSnapshot int
	if err := tx.QueryRowContext(ctx, `SELECT CASE WHEN w.kind='multi_repository' AND NOT EXISTS (SELECT 1 FROM workspace_snapshots ws WHERE ws.session_id=se.id AND ws.status='ARCHIVED') THEN 1 ELSE 0 END FROM sessions se JOIN workspaces w ON w.id=se.workspace_id WHERE se.id=?`, sessionID).Scan(&missingWorkspaceSnapshot); err != nil {
		return err
	}
	if missingWorkspaceSnapshot != 0 {
		return errors.New("multi-repository session has no archived workspace snapshot")
	}
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
	var sessionState string
	if err := tx.QueryRowContext(ctx, `SELECT state FROM sessions WHERE id=?`, sessionID).Scan(&sessionState); err != nil {
		return Job{}, false, err
	}
	if sessionState == "UNBOUND" || sessionState == "RESTORING" {
		job, err = newJob("REMOVE", workspaceID, slotID, "")
		if err != nil {
			return Job{}, false, err
		}
		timestamp := now()
		res, updateErr := tx.ExecContext(ctx, `UPDATE sessions SET state='EXPIRED',pending_agent_session_id=NULL,released_at=?,archived_at=?,expires_at=? WHERE id=? AND state IN ('UNBOUND','RESTORING')`, timestamp, timestamp, timestamp, sessionID)
		if updateErr != nil {
			return Job{}, false, updateErr
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return Job{}, false, nil
		}
		res, err = tx.ExecContext(ctx, `UPDATE slots SET state='REMOVING',owner_session_id=NULL,updated_at=? WHERE id=? AND state IN ('UNBOUND','RESTORING')`, timestamp, slotID)
		if err != nil {
			return Job{}, false, err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return Job{}, false, errors.New("unbound slot state changed before release")
		}
		if err := insertJob(ctx, tx, job); err != nil {
			return Job{}, false, err
		}
		return job, true, tx.Commit()
	}
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
	var slotState string
	if err := tx.QueryRowContext(ctx, `SELECT state FROM slots WHERE id=? AND owner_session_id=?`, slotID, sessionID).Scan(&slotState); err != nil {
		return Job{}, false, err
	}
	if slotState == "PREPARING" {
		if err := tx.Commit(); err != nil {
			return Job{}, false, err
		}
		return Job{}, false, nil
	}
	if slotState != "DRAINING" {
		return Job{}, false, fmt.Errorf("slot %s cannot be released from %s", slotID, slotState)
	}
	if err := insertJob(ctx, tx, job); err != nil {
		return Job{}, false, err
	}
	return job, true, tx.Commit()
}

type GCCandidate struct{ SlotID, SessionID, Path string }

type SlotArtifact struct{ ID, Path, State string }

func (s *Store) SlotArtifacts(ctx context.Context) ([]SlotArtifact, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,path,state FROM slots ORDER BY path`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var artifacts []SlotArtifact
	for rows.Next() {
		var artifact SlotArtifact
		if err := rows.Scan(&artifact.ID, &artifact.Path, &artifact.State); err != nil {
			return nil, err
		}
		artifacts = append(artifacts, artifact)
	}
	return artifacts, rows.Err()
}

func (s *Store) QuarantineMissingSlot(ctx context.Context, id, reason string) error {
	return s.SetSlotState(ctx, id, []string{"PREPARING", "READY", "LEASED", "DRAINING", "SNAPSHOTTING", "SNAPSHOTTED", "UNBOUND", "RESTORING", "RETIRING", "REMOVING", "FAILED", "STALE"}, "QUARANTINED", reason)
}

func (s *Store) QuarantineArtifact(ctx context.Context, kind, path, reason string) error {
	s.writer.Lock()
	defer s.writer.Unlock()
	_, err := s.db.ExecContext(ctx, `INSERT INTO quarantined_artifacts(path,kind,reason,detected_at) VALUES(?,?,?,?) ON CONFLICT(path) DO UPDATE SET kind=excluded.kind,reason=excluded.reason,detected_at=excluded.detected_at`, path, kind, reason, now())
	return err
}

func (s *Store) QuarantineMissingRecoveryRef(ctx context.Context, ref string) error {
	s.writer.Lock()
	defer s.writer.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT DISTINCT session_id FROM snapshots WHERE head_recovery_ref=? OR worktree_recovery_ref=?`, ref, ref)
	if err != nil {
		return err
	}
	defer rows.Close()
	var sessions []string
	for rows.Next() {
		var sessionID string
		if err := rows.Scan(&sessionID); err != nil {
			return err
		}
		sessions = append(sessions, sessionID)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE snapshots SET status='QUARANTINED' WHERE head_recovery_ref=? OR worktree_recovery_ref=?`, ref, ref); err != nil {
		return err
	}
	for _, sessionID := range sessions {
		if _, err := tx.ExecContext(ctx, `UPDATE sessions SET state='QUARANTINED' WHERE id=? AND state<>'EXPIRED'`, sessionID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE slots SET state='QUARANTINED',failure_code='RECOVERY_REF_MISSING',updated_at=? WHERE id=(SELECT slot_id FROM sessions WHERE id=?) AND state<>'ARCHIVED'`, now(), sessionID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) Repositories(ctx context.Context) ([]discovery.Repository, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,main_worktree_path,common_git_dir,default_branch FROM repositories ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var repositories []discovery.Repository
	for rows.Next() {
		var repository discovery.Repository
		if err := rows.Scan(&repository.ID, &repository.MainPath, &repository.CommonDir, &repository.DefaultBranch); err != nil {
			return nil, err
		}
		repositories = append(repositories, repository)
	}
	return repositories, rows.Err()
}

func (s *Store) RecoveryRefs(ctx context.Context, repositoryID string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT head_recovery_ref FROM snapshots WHERE repository_id=? UNION SELECT worktree_recovery_ref FROM snapshots WHERE repository_id=? ORDER BY 1`, repositoryID, repositoryID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var refs []string
	for rows.Next() {
		var ref string
		if err := rows.Scan(&ref); err != nil {
			return nil, err
		}
		refs = append(refs, ref)
	}
	return refs, rows.Err()
}

// RecoveryRefExpectations returns the durable ownership proof for each
// published recovery ref. A snapshot job that has already committed its
// metadata but has not finished publishing refs is marked InFlight while its
// pending job or unexpired worker lease is present; reconcile may wait for
// those missing refs while still quarantining every ref whose name or object
// ID is not exactly accounted for.
func (s *Store) RecoveryRefExpectations(ctx context.Context, repositoryID string) ([]RecoveryRefExpectation, error) {
	at := now()
	rows, err := s.db.QueryContext(ctx, `
		SELECT sn.head_recovery_ref,sn.head_oid,sn.session_id,se.state,
		   CASE WHEN se.state IN ('RELEASING','SNAPSHOTTING') AND EXISTS (SELECT 1 FROM jobs j WHERE j.kind='SNAPSHOT' AND j.session_id=sn.session_id AND (j.state='PENDING' OR (j.state='RUNNING' AND j.lease_expires_at>?))) THEN 1 ELSE 0 END
		FROM snapshots sn JOIN sessions se ON se.id=sn.session_id
		WHERE sn.repository_id=? AND sn.status='ARCHIVED'
		UNION ALL
		SELECT sn.worktree_recovery_ref,sn.worktree_snapshot_oid,sn.session_id,se.state,
		   CASE WHEN se.state IN ('RELEASING','SNAPSHOTTING') AND EXISTS (SELECT 1 FROM jobs j WHERE j.kind='SNAPSHOT' AND j.session_id=sn.session_id AND (j.state='PENDING' OR (j.state='RUNNING' AND j.lease_expires_at>?))) THEN 1 ELSE 0 END
		FROM snapshots sn JOIN sessions se ON se.id=sn.session_id
		WHERE sn.repository_id=? AND sn.status='ARCHIVED'
		ORDER BY 1`, at, repositoryID, at, repositoryID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var refs []RecoveryRefExpectation
	for rows.Next() {
		var ref RecoveryRefExpectation
		var inFlight int
		if err := rows.Scan(&ref.Ref, &ref.OID, &ref.SessionID, &ref.SessionState, &inFlight); err != nil {
			return nil, err
		}
		ref.InFlight = inFlight != 0
		refs = append(refs, ref)
	}
	return refs, rows.Err()
}

type StandbyGCCandidate struct{ SlotID, WorkspaceID, Path, State string }

type ColdRepositoryCandidate struct{ SlotID, WorkspaceID, RepositoryID, WorktreePath string }

func (s *Store) ColdRepositoryCandidates(ctx context.Context, hotBefore string) ([]ColdRepositoryCandidate, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT sl.id,sl.workspace_id,sr.repository_id,sr.worktree_path FROM slots sl JOIN slot_repositories sr ON sr.slot_id=sl.id JOIN repositories r ON r.id=sr.repository_id WHERE sl.owner_session_id IS NULL AND sl.state='READY' AND sr.state='READY' AND (r.last_leased_at IS NULL OR r.last_leased_at<=?) ORDER BY sl.id,sr.repository_id`, hotBefore)
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
		_ = lastLeased
		if candidate.State == "STALE" || kept[candidate.WorkspaceID] >= warm {
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
	return s.SetSlotState(ctx, slotID, []string{"REMOVING", "ARCHIVED"}, "ARCHIVED", "")
}

func (s *Store) DrainRoot(ctx context.Context, root string) error {
	s.writer.Lock()
	defer s.writer.Unlock()
	prefix := strings.TrimSuffix(filepath.Clean(root), string(filepath.Separator)) + string(filepath.Separator) + "%"
	_, err := s.db.ExecContext(ctx, `UPDATE slots SET state='STALE',failure_code='CONFIG_ROOT_RETIRED',updated_at=? WHERE owner_session_id IS NULL AND path LIKE ? ESCAPE '\' AND state IN ('PREPARING','READY')`, now(), prefix)
	return err
}

func (s *Store) ExpiredSnapshots(ctx context.Context, before string) ([]Snapshot, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT sn.id,sn.session_id,sn.repository_id,sn.head_oid,sn.head_recovery_ref,sn.index_tree_oid,sn.worktree_snapshot_oid,sn.worktree_recovery_ref,sn.status,sn.created_at,sn.expires_at FROM snapshots sn JOIN sessions se ON se.id=sn.session_id JOIN slots sl ON sl.id=se.slot_id WHERE se.state='ARCHIVED' AND sl.state='ARCHIVED' AND sn.status='ARCHIVED' AND sn.expires_at<=? AND NOT EXISTS (SELECT 1 FROM sessions child JOIN jobs j ON j.session_id=child.id WHERE child.parent_session_id=se.id AND j.kind='RESTORE' AND j.state IN ('PENDING','RUNNING')) ORDER BY sn.session_id,sn.repository_id`, before)
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
	if _, err := tx.ExecContext(ctx, `DELETE FROM workspace_snapshots WHERE session_id=?`, sessionID); err != nil {
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
	if _, err := tx.ExecContext(ctx, `DELETE FROM rpc_idempotency WHERE expires_at<=?`, now()); err != nil {
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
