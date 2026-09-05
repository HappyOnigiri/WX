package state

import (
	"context"
	cryptorand "crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
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

	sqlite "modernc.org/sqlite"

	"github.com/HappyOnigiri/WX/internal/discovery"
	"github.com/HappyOnigiri/WX/internal/domain"
	"github.com/HappyOnigiri/WX/migrations"
)

type Store struct {
	db     *sql.DB
	writer sync.Mutex
	path   string
}

const SchemaVersion = 3

// ErrPreviousWorktreeLayout は、wx が意図的に migration path を持たない旧 worktree layout の state database を示す。
var ErrPreviousWorktreeLayout = errors.New("wx database uses previous worktree layout")

// JSONSchemaVersion は `wx status --json` と `wx doctor --json` の出力形状の互換契約であり、SQLite migration 数の SchemaVersion とは独立である。
// scripted consumer が観測する形状を変える場合だけ上げる。2 は restart_pending、3 は stop_pending/pid、4 は daemon-unavailable diagnostics を追加した。
// 5 は `wx status --json` の worktree_root_error と `wx doctor --json` の worktree_root check を追加した。
const JSONSchemaVersion = 5

func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	if err := os.Chmod(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	dsn := (&url.URL{Scheme: "file", Path: path}).String() + "?_defensive=1&_journal_mode=wal&_foreign_keys=on&_busy_timeout=5000&_synchronous=normal"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// connection-local policy は DSN に含める。status reader は WAL を並行利用し、write は writer が直列化する。
	db.SetMaxOpenConns(8)
	s := &Store{db: db, path: path}
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
	var storedMethod, storedParams, requestState, errorCode, errorMessage string
	var result []byte
	if err := tx.QueryRowContext(ctx, `SELECT method,params,state,result,COALESCE(error_code,''),COALESCE(error_message,'') FROM rpc_idempotency WHERE idempotency_key=? AND expires_at>?`, key, now()).Scan(&storedMethod, &storedParams, &requestState, &result, &errorCode, &errorMessage); err != nil {
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
	return result, errorCode, errorMessage, false, nil
}

func (s *Store) CompleteRPCRequest(ctx context.Context, key, method, params string, result []byte, errorCode, errorMessage string, expiresAt time.Time) error {
	s.writer.Lock()
	defer s.writer.Unlock()
	paramsHash := rpcParamsHash(params)
	res, err := s.db.ExecContext(ctx, `UPDATE rpc_idempotency SET result=?,error_code=?,error_message=?,completed_at=?,expires_at=?,state='COMPLETED' WHERE idempotency_key=? AND method=? AND params=? AND state='PENDING'`, result, nullString(errorCode), nullString(errorMessage), now(), FormatTime(expiresAt), key, method, paramsHash)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return errors.New("idempotency reservation is not pending for this request")
	}
	return nil
}

func rpcParamsHash(params string) string {
	digest := sha256.Sum256([]byte(params))
	return hex.EncodeToString(digest[:])
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
	var applied int
	if err := s.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&applied); err != nil {
		return err
	}
	for i, name := range entries {
		version := i + 1
		if version <= applied {
			continue
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
			_, err = tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version=%d", version))
		}
		if err != nil {
			tx.Rollback()
			return fmt.Errorf("migration %s: %w", name, err)
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return s.verifySchema(ctx)
}

// verifySchema は worktree layout 変更前の wx が作成した database に対し、対処可能な message で Open を失敗させる。
// それらは user_version=1 のため migration loop では何も適用されず、以降の query が存在しない column で失敗する。
// 旧 schema の migration path は持たないためである。
func (s *Store) verifySchema(ctx context.Context) error {
	var present int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type='table' AND name='roots'`).Scan(&present); err != nil {
		return err
	}
	if present == 0 {
		return fmt.Errorf("%w: %s was created by a wx release that used the previous worktree layout and cannot be migrated; stop the daemon, remove that file, and remove the old worktree root once no session needs it", ErrPreviousWorktreeLayout, s.path)
	}
	return nil
}

// sqliteConstraintPrimaryKey と sqliteConstraintUnique は INSERT が primary-key/unique race に負けたときの SQLite extended result code である。
// wx は短い random ID を生成するため、再抽選すべき衝突と本当の失敗を区別する信号はこの二つだけである。
const (
	sqliteConstraintUnique     = 2067
	sqliteConstraintPrimaryKey = 1555
)

// IsIDCollision は重複生成 identifier による constraint violation かを返す。
// 呼び出し元は新しい identifier で retry し、他の error はそのまま返す。
func IsIDCollision(err error) bool {
	var sqliteErr *sqlite.Error
	if errors.As(err, &sqliteErr) {
		code := sqliteErr.Code()
		return code == sqliteConstraintUnique || code == sqliteConstraintPrimaryKey
	}
	return false
}

// idCollisionAttempts は identifier retry 回数の上限である。6桁 base36 は約 21.8 億通りなので、10回連続失敗は偶然以外の問題として error にする。
const idCollisionAttempts = 10

// newUnusedShortID は指定 table の id column にない short ID を引くまで再試行する。
// 呼び出し元は write lock を保持し、transaction 中はそれを querier に渡す。これにより返却値は INSERT 時にも未使用である。
func newUnusedShortID(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, table string,
) (string, error) {
	for range idCollisionAttempts {
		id, err := domain.NewShortID()
		if err != nil {
			return "", err
		}
		var taken int
		// table は package 内の定数 literal（`roots`、`workspaces`）であり caller input ではない。連結しても statement に untrusted text は入らない。
		err = q.QueryRowContext(ctx, `SELECT count(*) FROM `+table+` WHERE id=?`, id).Scan(&taken)
		if err != nil {
			return "", err
		}
		if taken == 0 {
			return id, nil
		}
	}
	return "", fmt.Errorf("could not find an unused %s identifier in %d attempts", table, idCollisionAttempts)
}

// Root は storage.worktree_root の一世代である。設定 root が変わっても既存 slot は移動せず、以前の row を active=0 で残して解決を維持する。
// 新しい slot だけを active row の下に作る。
type Root struct {
	ID, Path, Identity string
	Active             bool
}

// EnsureActiveRoot は path を active worktree root generation として登録し durable ID を返す。既登録 path は同じ ID を保ち、他の row は retired にする。
// inode identity の不一致は durable な参照がない場合だけ許可する。ユーザーが root を削除し wx が再作成しても state を取り残さない場合である。
// live slot/snapshot がある不一致は、記録済み location が wx の作成していない directory を指すため拒否する。
func (s *Store) EnsureActiveRoot(ctx context.Context, path, identity string) (string, error) {
	if path == "" || identity == "" {
		return "", errors.New("worktree root path and identity are required")
	}
	s.writer.Lock()
	defer s.writer.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer func() { _ = tx.Rollback() }()
	t := now()
	var id, storedIdentity string
	err = tx.QueryRowContext(ctx, `SELECT id,identity FROM roots WHERE path=?`, path).Scan(&id, &storedIdentity)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		id, err = newUnusedShortID(ctx, tx, "roots")
		if err != nil {
			return "", err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO roots(id,path,identity,active,created_at) VALUES(?,?,?,1,?)`, id, path, identity, t); err != nil {
			return "", err
		}
	case err != nil:
		return "", err
	case storedIdentity != identity:
		var referencing int
		if err := tx.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM slots WHERE root_id=?)+(SELECT count(*) FROM workspace_snapshots WHERE root_id=?)`, id, id).Scan(&referencing); err != nil {
			return "", err
		}
		if referencing > 0 {
			return "", fmt.Errorf("%w: worktree root %s inode changed (recorded %s, found %s) while %d durable rows still reference it", ErrOwnership, path, storedIdentity, identity, referencing)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE roots SET identity=?,active=1,retired_at=NULL WHERE id=?`, identity, id); err != nil {
			return "", err
		}
	default:
		if _, err := tx.ExecContext(ctx, `UPDATE roots SET active=1,retired_at=NULL WHERE id=?`, id); err != nil {
			return "", err
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE roots SET active=0,retired_at=COALESCE(retired_at,?) WHERE id<>? AND active=1`, t, id); err != nil {
		return "", err
	}
	return id, tx.Commit()
}

// Roots は登録済み root generation を全て返し、daemon が durable slot の依存する descriptor を再 pin できるようにする。
func (s *Store) Roots(ctx context.Context) ([]Root, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,path,identity,active FROM roots ORDER BY created_at,id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Root
	for rows.Next() {
		var root Root
		if err := rows.Scan(&root.ID, &root.Path, &root.Identity, &root.Active); err != nil {
			return nil, err
		}
		out = append(out, root)
	}
	return out, rows.Err()
}

// PruneRoots は durable な参照がなくなった retired root generation を削除する。
// directory 自体は disk に残す。wx は configured root を削除せず、参照可能にしていた row だけを削除する。
func (s *Store) PruneRoots(ctx context.Context) error {
	s.writer.Lock()
	defer s.writer.Unlock()
	_, err := s.db.ExecContext(ctx, `DELETE FROM roots WHERE active=0 AND NOT EXISTS (SELECT 1 FROM slots WHERE slots.root_id=roots.id) AND NOT EXISTS (SELECT 1 FROM workspace_snapshots WHERE workspace_snapshots.root_id=roots.id)`)
	return err
}

const timestampFormat = "2006-01-02T15:04:05.000000000Z07:00"

// FormatTime は固定幅 RFC 3339 timestamp を返す。SQLite は wx の時刻を TEXT で保存するため、
// lexical comparison で時系列順を保つには固定の小数幅が必要である。
func FormatTime(value time.Time) string { return value.UTC().Format(timestampFormat) }

func now() string                   { return FormatTime(time.Now()) }
func HashToken(token string) []byte { sum := sha256.Sum256([]byte(token)); return sum[:] }

// CanonicalWorkspace は discovery の新しい proposal を、既登録 workspace の identity に置き換えて返す。single-repository は Git common directory、
// multi-repository は canonical root path で証明し、workspace ID 自体では証明しない。
// slot directory 名である ID を変えると既存 pool を取り残し、main worktree 移動後に重複 workspace を作る。
// commentlint:allow-long -- workspace identity を ID 以外で証明する理由を説明する
func (s *Store) CanonicalWorkspace(ctx context.Context, w discovery.Workspace) (discovery.Workspace, error) {
	s.writer.Lock()
	defer s.writer.Unlock()
	registered, found, err := s.registeredWorkspaceID(ctx, w)
	if err != nil {
		return w, err
	}
	if found {
		w.ID = domain.WorkspaceID(registered)
	}
	return w, nil
}

// registeredWorkspaceID は既登録 workspace の durable ID を解決し、wx が未観測なら found=false を返す。
func (s *Store) registeredWorkspaceID(ctx context.Context, w discovery.Workspace) (string, bool, error) {
	var query string
	var argument any
	switch {
	case w.Kind == "repository" && len(w.Repositories) == 1:
		query = `SELECT DISTINCT w.id FROM workspaces w JOIN workspace_repositories wr ON wr.workspace_id=w.id JOIN repositories r ON r.id=wr.repository_id WHERE w.kind='repository' AND r.common_git_dir=? ORDER BY w.id`
		argument = string(w.Repositories[0].CommonDir)
	case w.Kind == "multi_repository":
		query = `SELECT id FROM workspaces WHERE kind='multi_repository' AND root_path=? ORDER BY id`
		argument = string(w.Root)
	default:
		return "", false, nil
	}
	rows, err := s.db.QueryContext(ctx, query, argument)
	if err != nil {
		return "", false, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return "", false, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return "", false, err
	}
	if len(ids) > 1 {
		return "", false, fmt.Errorf("workspace identity %v belongs to multiple registered workspaces", argument)
	}
	if len(ids) == 0 {
		return "", false, nil
	}
	return ids[0], true, nil
}

// UpsertWorkspaceGeneration は workspace を登録し、解決済み durable ID を持つ値を返す。呼び出し元は返却値を使う。
// その ID が slot directory 名であり、既知 workspace や generated ID 再抽選時には discovery の proposal と異なる。
func (s *Store) UpsertWorkspaceGeneration(ctx context.Context, w discovery.Workspace) (discovery.Workspace, int, error) {
	s.writer.Lock()
	defer s.writer.Unlock()
	registered, found, err := s.registeredWorkspaceID(ctx, w)
	if err != nil {
		return w, 0, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return w, 0, err
	}
	defer tx.Rollback()
	if found {
		w.ID = domain.WorkspaceID(registered)
	} else {
		// discovery の proposal は random なので、無関係な workspace と衝突し得る。
		// transaction 内で引き直し、下の INSERT がその row を奪わないようにする。
		id, idErr := newUnusedShortID(ctx, tx, "workspaces")
		if idErr != nil {
			return w, 0, idErr
		}
		w.ID = domain.WorkspaceID(id)
	}
	t := now()
	generation := 1
	existing := false
	var oldKind string
	err = tx.QueryRowContext(ctx, `SELECT generation,kind FROM workspaces WHERE id=?`, w.ID).Scan(&generation, &oldKind)
	if err == nil {
		existing = true
	} else if !errors.Is(err, sql.ErrNoRows) {
		return w, 0, err
	}
	type membership struct {
		rel     string
		ordinal int
	}
	oldMembers := map[string]membership{}
	if existing {
		rows, err := tx.QueryContext(ctx, `SELECT repository_id,relative_path,ordinal FROM workspace_repositories WHERE workspace_id=?`, w.ID)
		if err != nil {
			return w, 0, err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var id string
			var member membership
			if err := rows.Scan(&id, &member.rel, &member.ordinal); err != nil {
				_ = rows.Close()
				return w, 0, err
			}
			oldMembers[id] = member
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return w, 0, err
		}
		if err := rows.Close(); err != nil {
			return w, 0, err
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
		return w, 0, err
	}
	if existing {
		if _, err := tx.ExecContext(ctx, `DELETE FROM workspace_repositories WHERE workspace_id=?`, w.ID); err != nil {
			return w, 0, err
		}
	}
	for i, r := range w.Repositories {
		_, err = tx.ExecContext(ctx, `INSERT INTO repositories(id,main_worktree_path,common_git_dir,default_branch,remote_name,first_seen_at,last_seen_at) VALUES(?,?,?,?,?,?,?) ON CONFLICT(id) DO UPDATE SET main_worktree_path=excluded.main_worktree_path,common_git_dir=excluded.common_git_dir,default_branch=excluded.default_branch,remote_name=CASE WHEN excluded.remote_name<>'' THEN excluded.remote_name ELSE repositories.remote_name END,last_seen_at=excluded.last_seen_at`, r.ID, r.MainPath, r.CommonDir, r.DefaultBranch, r.RemoteName, t, t)
		if err != nil {
			return w, 0, err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO workspace_repositories(workspace_id,repository_id,relative_path,ordinal) VALUES(?,?,?,?) ON CONFLICT(workspace_id,repository_id) DO UPDATE SET relative_path=excluded.relative_path,ordinal=excluded.ordinal`, w.ID, r.ID, r.RelativePath, i)
		if err != nil {
			return w, 0, err
		}
	}
	if changed {
		if _, err := tx.ExecContext(ctx, `UPDATE slots SET state='STALE',updated_at=? WHERE workspace_id=? AND generation<? AND owner_session_id IS NULL AND state IN ('PREPARING','READY')`, t, w.ID, generation); err != nil {
			return w, 0, err
		}
	}
	return w, generation, tx.Commit()
}

func (s *Store) WorkspaceGeneration(ctx context.Context, workspaceID string) (int, error) {
	var generation int
	err := s.db.QueryRowContext(ctx, `SELECT generation FROM workspaces WHERE id=?`, workspaceID).Scan(&generation)
	return generation, err
}

// Slot は root generation と root 相対 path で slot directory を位置付ける。Path は派生値で、read は roots.path から組み立て、write は RootID/RelPath を使う。
// single-repository workspace では slot directory の一階層下を指す daemon.Lease.Path と混同してはならない。
type Slot struct {
	ID, WorkspaceID, RootID, RelPath, Path, State, OwnerSessionID, FailureCode, FailureDetailPath string
	DirIdentity                                                                                   string
	Generation                                                                                    int
	CreatedAt, ReadyAt                                                                            string
}
type (
	// SlotRepository は slot 内の directory 名で repository worktree を位置付ける。WorktreePath は Slot.Path と同様に派生する。
	SlotRepository struct {
		RepositoryID, DirName, DirIdentity, WorktreePath, State, RequestedRef, BaseOID, Fingerprint string
	}
	Session struct {
		ID, WorkspaceID, SlotID, ParentSessionID, State, AgentKind, AgentSessionID, CreatedAt, ReleasedAt, ArchivedAt, ExpiresAt string
		TokenHash                                                                                                                []byte
		ClientPID, AgentPID                                                                                                      int
	}
)

type (
	Snapshot struct {
		ID, SessionID, RepositoryID, HeadOID, HeadRef, IndexTreeOID, IndexRef, WorktreeOID, WorktreeRef, Status, CreatedAt, ExpiresAt string
	}
	RecoveryRefExpectation struct {
		Ref, OID, SessionID, SessionState string
		InFlight                          bool
	}
	// WorkspaceSnapshot は Slot と同様に bundle archive を位置付ける。RootID/RelPath が authority で、ArchivePath は派生値である。
	WorkspaceSnapshot struct {
		SessionID, RootID, RelPath, ArchivePath, SHA256, Status, CreatedAt, ExpiresAt string
	}
	Job struct {
		ID, Kind, WorkspaceID, SlotID, SessionID, RepositoryID, State string
		ErrorCode, ErrorDetailPath                                    string
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

// slotColumns は full-row の slot read 全てで共有する column list である。
// absolute な Slot.Path は保存せず、scanSlot が結合した root generation から組み立てるため、
// retired root も自身の slot を解決し続けられる。
const slotColumns = `sl.id,COALESCE(sl.workspace_id,''),sl.generation,sl.root_id,rt.path,sl.rel_path,COALESCE(sl.dir_identity,''),sl.state,COALESCE(sl.owner_session_id,''),sl.created_at,COALESCE(sl.ready_at,''),COALESCE(sl.failure_code,''),COALESCE(sl.failure_detail_path,'')`

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
	if err := row.Scan(&x.ID, &x.WorkspaceID, &x.Generation, &x.RootID, &rootPath, &x.RelPath, &x.DirIdentity, &x.State, &x.OwnerSessionID, &x.CreatedAt, &x.ReadyAt, &x.FailureCode, &x.FailureDetailPath); err != nil {
		return Slot{}, err
	}
	x.Path = filepath.Join(rootPath, x.RelPath)
	return x, nil
}

func (s *Store) ReadySlot(ctx context.Context, workspaceID string) (Slot, bool, error) {
	x, err := scanSlot(s.db.QueryRowContext(ctx, `SELECT `+slotColumns+slotFrom+` JOIN workspaces w ON w.id=sl.workspace_id WHERE sl.workspace_id=? AND sl.generation=w.generation AND sl.state='READY' ORDER BY sl.ready_at LIMIT 1`, workspaceID))
	if errors.Is(err, sql.ErrNoRows) {
		return Slot{}, false, nil
	}
	return x, err == nil, err
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

// standbyQuery は待機枠を数える SQL。QUARANTINED は枠に含めない。
// 隔離は終端状態で READY へ戻らないため、数えると補充が恒久的に止まる。
// リトライ中の一時状態である FAILED は枠に残し、通常セッション成功時点で記録された除外だけを計算から外す。
const standbyQuery = `SELECT count(*) FROM slots sl JOIN workspaces w ON w.id=sl.workspace_id
	WHERE sl.workspace_id=? AND sl.generation=w.generation AND sl.owner_session_id IS NULL
	AND sl.state IN ('PREPARING','READY','FAILED','RETIRING','REMOVING')
	AND (sl.state<>'FAILED' OR NOT EXISTS (
		SELECT 1 FROM standby_replenish_exclusions ex
		WHERE ex.slot_id=sl.id AND ex.workspace_id=sl.workspace_id AND ex.generation=sl.generation
	))`

// quarantinedStandbyQuery は貸出前に隔離された slot を数える SQL。
// last_used_at が NULL の slot だけを対象にし、一度使われた slot の隔離（snapshot 失敗など）を補充抑制に持ち込まない。
const quarantinedStandbyQuery = `SELECT count(*) FROM slots sl JOIN workspaces w ON w.id=sl.workspace_id
	WHERE sl.workspace_id=? AND sl.generation=w.generation AND sl.state='QUARANTINED' AND sl.last_used_at IS NULL`

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

// QuarantinedStandbyCount は現行 generation で貸出前に隔離された slot を数える。
// 補充側は、準備が壊れた workspace で隔離 slot が無制限に積み上がらないよう、この数を上限として使う。
func (s *Store) QuarantinedStandbyCount(ctx context.Context, workspaceID string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, quarantinedStandbyQuery, workspaceID).Scan(&n)
	return n, err
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
	t := now()
	_, err = tx.ExecContext(ctx, `INSERT INTO slots(id,workspace_id,generation,root_id,rel_path,dir_identity,state,owner_session_id,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?)`, slot.ID, nullString(slot.WorkspaceID), slot.Generation, slot.RootID, slot.RelPath, nullString(slot.DirIdentity), slot.State, nullString(session.ID), t, t)
	if err != nil {
		return Job{}, err
	}
	for _, r := range repos {
		_, err = tx.ExecContext(ctx, `INSERT INTO slot_repositories(slot_id,repository_id,dir_name,state,requested_ref,base_oid,prepare_fingerprint) VALUES(?,?,?,?,?,?,?)`, slot.ID, r.RepositoryID, r.DirName, r.State, r.RequestedRef, r.BaseOID, r.Fingerprint)
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
	if err := assertNoActiveClean(ctx, tx); err != nil {
		return Job{}, err
	}
	if err := insertStandbySlotTx(ctx, tx, slot, repos, job); err != nil {
		return Job{}, err
	}
	return job, tx.Commit()
}

// CreateStandbyIfNeeded は standby 枠の再検証と slot/job 登録を同じ transaction で行う。
// 別の reconcile が先に不足分を埋めた場合は、作成側が物理 directory を所有権確認後に片付けられるよう false を返す。
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
		if _, err = tx.ExecContext(ctx, `INSERT INTO slot_repositories(slot_id,repository_id,dir_name,state,requested_ref,base_oid,prepare_fingerprint) VALUES(?,?,?,?,?,?,?)`, slot.ID, r.RepositoryID, r.DirName, r.State, r.RequestedRef, r.BaseOID, r.Fingerprint); err != nil {
			return err
		}
	}
	if err := insertJob(ctx, tx, job); err != nil {
		return err
	}
	return nil
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
	_, err = tx.ExecContext(ctx, `INSERT INTO sessions(id,workspace_id,slot_id,state,agent_kind,client_pid,session_token_hash,created_at) VALUES(?,?,?,?,?,?,?,?)`, session.ID, session.WorkspaceID, slotID, session.State, session.AgentKind, session.ClientPID, session.TokenHash, now())
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
			res, err = tx.ExecContext(ctx, `UPDATE sessions SET agent_session_id=NULL WHERE id=? AND agent_kind=? AND agent_session_id=?`, parentID, kind, pendingAgentID)
			if err != nil {
				return Job{}, false, Job{}, false, err
			}
			if n, _ := res.RowsAffected(); n != 1 {
				return Job{}, false, Job{}, false, errors.New("resume parent agent mapping changed before restore completed")
			}
			res, err = tx.ExecContext(ctx, `UPDATE sessions SET agent_session_id=?,pending_agent_session_id=NULL WHERE id=? AND state='RESTORING'`, pendingAgentID, sessionID)
			if err != nil {
				return Job{}, false, Job{}, false, err
			}
			if n, _ := res.RowsAffected(); n != 1 {
				return Job{}, false, Job{}, false, errors.New("restoring session changed before activation")
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

// placeholders は caller が渡した slice から作る IN(...) clause 用に、n 個の "?" bind marker を
// comma で連結して返す。
func placeholders(n int) string { return strings.TrimRight(strings.Repeat("?,", n), ",") }

func stringsToAny(v []string) []any {
	a := make([]any, len(v))
	for i := range v {
		a[i] = v[i]
	}
	return a
}

// sessionColumns は full-row の session read 全てで共有する column list である。
const sessionColumns = `id,COALESCE(workspace_id,''),slot_id,COALESCE(parent_session_id,''),state,agent_kind,COALESCE(agent_session_id,''),COALESCE(client_pid,0),COALESCE(agent_pid,0),session_token_hash,created_at,COALESCE(released_at,''),COALESCE(archived_at,''),COALESCE(expires_at,'')`

func scanSession(row *sql.Row) (Session, error) {
	var x Session
	err := row.Scan(&x.ID, &x.WorkspaceID, &x.SlotID, &x.ParentSessionID, &x.State, &x.AgentKind, &x.AgentSessionID, &x.ClientPID, &x.AgentPID, &x.TokenHash, &x.CreatedAt, &x.ReleasedAt, &x.ArchivedAt, &x.ExpiresAt)
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
	if err := assertNoActiveClean(ctx, tx); err != nil {
		return Job{}, err
	}
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
		if _, err := tx.ExecContext(ctx, `INSERT INTO slot_repositories(slot_id,repository_id,dir_name,state,requested_ref,base_oid,prepare_fingerprint) VALUES(?,?,?,?,?,?,?)`, sessionID, repository.RepositoryID, repository.DirName, "PREPARING", repository.RequestedRef, repository.BaseOID, repository.Fingerprint); err != nil {
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
	if err := assertNoActiveClean(ctx, tx); err != nil {
		return Job{}, err
	}
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
		if _, err = tx.ExecContext(ctx, `INSERT INTO slot_repositories(slot_id,repository_id,dir_name,state,requested_ref,base_oid,prepare_fingerprint) VALUES(?,?,?,?,?,?,?)`, sessionID, r.RepositoryID, r.DirName, "RESTORING", r.RequestedRef, r.BaseOID, r.Fingerprint); err != nil {
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

// slotRepositoryColumns は slot-repository read 全てで共有する。SlotRepository.WorktreePath は保存せず、
// 結合した root generation と slot path から組み立てるため、設定 worktree root の変更後も同じ row を解決できる。
const slotRepositoryColumns = `sr.repository_id,sr.dir_name,COALESCE(sr.dir_identity,''),rt.path,sl.rel_path,sr.state,sr.requested_ref,sr.base_oid,sr.prepare_fingerprint`

const slotRepositoryFrom = ` FROM slot_repositories sr JOIN slots sl ON sl.id=sr.slot_id JOIN roots rt ON rt.id=sl.root_id`

func scanSlotRepository(row rowScanner) (SlotRepository, error) {
	var x SlotRepository
	var rootPath, slotRel string
	if err := row.Scan(&x.RepositoryID, &x.DirName, &x.DirIdentity, &rootPath, &slotRel, &x.State, &x.RequestedRef, &x.BaseOID, &x.Fingerprint); err != nil {
		return SlotRepository{}, err
	}
	x.WorktreePath = filepath.Join(rootPath, slotRel, x.DirName)
	return x, nil
}

func (s *Store) SlotRepositories(ctx context.Context, slotID string) ([]SlotRepository, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+slotRepositoryColumns+slotRepositoryFrom+` WHERE sr.slot_id=? ORDER BY sr.repository_id`, slotID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SlotRepository
	for rows.Next() {
		x, err := scanSlotRepository(rows)
		if err != nil {
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
		if _, err := tx.ExecContext(ctx, `INSERT INTO slot_repositories(slot_id,repository_id,dir_name,state,requested_ref,base_oid,prepare_fingerprint) VALUES(?,?,?,?,?,?,?)`, slotID, repo.RepositoryID, repo.DirName, "RESTORING", repo.RequestedRef, repo.BaseOID, repo.Fingerprint); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) SlotRepository(ctx context.Context, slotID, repositoryID string) (SlotRepository, error) {
	return scanSlotRepository(s.db.QueryRowContext(ctx, `SELECT `+slotRepositoryColumns+slotRepositoryFrom+` WHERE sr.slot_id=? AND sr.repository_id=?`, slotID, repositoryID))
}

// RecordSlotRepositoryIdentity は repository worktree directory 作成直後の inode identity を保存する。
// ownership validation はこれを比較し、identity を提示できる呼び出し元で record がなければ path 比較へ戻さずフェイルクローズする。
func (s *Store) RecordSlotRepositoryIdentity(ctx context.Context, slotID, repositoryID, identity string) error {
	if identity == "" {
		return errors.New("slot repository directory identity is required")
	}
	s.writer.Lock()
	defer s.writer.Unlock()
	res, err := s.db.ExecContext(ctx, `UPDATE slot_repositories SET dir_identity=? WHERE slot_id=? AND repository_id=?`, identity, slotID, repositoryID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return fmt.Errorf("slot repository %s/%s is not registered", slotID, repositoryID)
	}
	return nil
}

func (s *Store) SetSlotRepositoryState(ctx context.Context, slotID, repositoryID string, from []string, to string) error {
	s.writer.Lock()
	defer s.writer.Unlock()
	args := append([]any{to, slotID, repositoryID}, stringsToAny(from)...)
	res, err := s.db.ExecContext(ctx, `UPDATE slot_repositories SET state=? WHERE slot_id=? AND repository_id=? AND state IN (`+placeholders(len(from))+`)`, args...)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return fmt.Errorf("slot repository %s/%s state compare-and-swap failed", slotID, repositoryID)
	}
	return nil
}

func (s *Store) Slot(ctx context.Context, id string) (Slot, error) {
	return scanSlot(s.db.QueryRowContext(ctx, `SELECT `+slotColumns+slotFrom+` WHERE sl.id=?`, id))
}

// SaveSnapshot は repository recovery snapshot を保存するか、同一 session/repository の既存 record が同じ object/ref を指すことを検証する。
// SQL の ON CONFLICT は全 immutable field が一致する場合だけ status を no-op 更新する。不一致では既存 row を変えず、RETURNING も row を返さない。
func (s *Store) SaveSnapshot(ctx context.Context, x Snapshot) error {
	s.writer.Lock()
	defer s.writer.Unlock()
	var ok int
	err := s.db.QueryRowContext(ctx, `INSERT INTO snapshots(id,session_id,repository_id,head_oid,head_recovery_ref,index_tree_oid,index_recovery_ref,worktree_snapshot_oid,worktree_recovery_ref,status,created_at,expires_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(session_id,repository_id) DO UPDATE SET status=excluded.status
		WHERE snapshots.id=excluded.id AND snapshots.head_oid=excluded.head_oid AND snapshots.head_recovery_ref=excluded.head_recovery_ref
		  AND snapshots.index_tree_oid=excluded.index_tree_oid AND snapshots.index_recovery_ref=excluded.index_recovery_ref
		  AND snapshots.worktree_snapshot_oid=excluded.worktree_snapshot_oid AND snapshots.worktree_recovery_ref=excluded.worktree_recovery_ref
		RETURNING 1`,
		x.ID, x.SessionID, x.RepositoryID, x.HeadOID, x.HeadRef, x.IndexTreeOID, x.IndexRef, x.WorktreeOID, x.WorktreeRef, x.Status, x.CreatedAt, x.ExpiresAt).Scan(&ok)
	if errors.Is(err, sql.ErrNoRows) {
		return errors.New("snapshot metadata conflicts with an existing recovery snapshot")
	}
	return err
}

func (s *Store) Snapshots(ctx context.Context, sessionID string) ([]Snapshot, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,session_id,repository_id,head_oid,head_recovery_ref,index_tree_oid,index_recovery_ref,worktree_snapshot_oid,worktree_recovery_ref,status,created_at,expires_at FROM snapshots WHERE session_id=? ORDER BY repository_id`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Snapshot
	for rows.Next() {
		var x Snapshot
		if err := rows.Scan(&x.ID, &x.SessionID, &x.RepositoryID, &x.HeadOID, &x.HeadRef, &x.IndexTreeOID, &x.IndexRef, &x.WorktreeOID, &x.WorktreeRef, &x.Status, &x.CreatedAt, &x.ExpiresAt); err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

// SaveWorkspaceSnapshot は workspace bundle の recovery snapshot を記録するか、同じ session の既存 write が
// 完全に同じ archive を指すことを検証する。比較を SQL に移す方法は SaveSnapshot を参照する。
func (s *Store) SaveWorkspaceSnapshot(ctx context.Context, x WorkspaceSnapshot) error {
	s.writer.Lock()
	defer s.writer.Unlock()
	var ok int
	err := s.db.QueryRowContext(ctx, `INSERT INTO workspace_snapshots(session_id,root_id,rel_path,sha256,status,created_at,expires_at) VALUES(?,?,?,?,?,?,?)
		ON CONFLICT(session_id) DO UPDATE SET status=excluded.status
		WHERE workspace_snapshots.root_id=excluded.root_id AND workspace_snapshots.rel_path=excluded.rel_path AND workspace_snapshots.sha256=excluded.sha256
		  AND workspace_snapshots.status=excluded.status AND workspace_snapshots.expires_at=excluded.expires_at
		RETURNING 1`,
		x.SessionID, x.RootID, x.RelPath, x.SHA256, x.Status, x.CreatedAt, x.ExpiresAt).Scan(&ok)
	if errors.Is(err, sql.ErrNoRows) {
		return errors.New("workspace snapshot metadata conflicts with an existing recovery snapshot")
	}
	return err
}

func (s *Store) WorkspaceSnapshot(ctx context.Context, sessionID string) (WorkspaceSnapshot, bool, error) {
	var x WorkspaceSnapshot
	var rootPath string
	err := s.db.QueryRowContext(ctx, `SELECT ws.session_id,ws.root_id,rt.path,ws.rel_path,ws.sha256,ws.status,ws.created_at,ws.expires_at FROM workspace_snapshots ws JOIN roots rt ON rt.id=ws.root_id WHERE ws.session_id=?`, sessionID).Scan(&x.SessionID, &x.RootID, &rootPath, &x.RelPath, &x.SHA256, &x.Status, &x.CreatedAt, &x.ExpiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return WorkspaceSnapshot{}, false, nil
	}
	x.ArchivePath = filepath.Join(rootPath, x.RelPath)
	return x, err == nil, err
}

func (s *Store) Repository(ctx context.Context, id string) (discovery.Repository, error) {
	var r discovery.Repository
	err := s.db.QueryRowContext(ctx, `SELECT id,main_worktree_path,common_git_dir,default_branch,remote_name FROM repositories WHERE id=?`, id).Scan(&r.ID, &r.MainPath, &r.CommonDir, &r.DefaultBranch, &r.RemoteName)
	return r, err
}

func (s *Store) Workspace(ctx context.Context, id string) (discovery.Workspace, error) {
	var w discovery.Workspace
	err := s.db.QueryRowContext(ctx, `SELECT id,root_path,kind FROM workspaces WHERE id=?`, id).Scan(&w.ID, &w.Root, &w.Kind)
	if err != nil {
		return w, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT r.id,r.main_worktree_path,r.common_git_dir,wr.relative_path,r.default_branch,r.remote_name FROM workspace_repositories wr JOIN repositories r ON r.id=wr.repository_id WHERE wr.workspace_id=? ORDER BY wr.ordinal`, id)
	if err != nil {
		return w, err
	}
	defer rows.Close()
	for rows.Next() {
		var r discovery.Repository
		if err := rows.Scan(&r.ID, &r.MainPath, &r.CommonDir, &r.RelativePath, &r.DefaultBranch, &r.RemoteName); err != nil {
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

// SessionWorkspaceKind は session の workspace kind（"repository" または "multi_repository"）だけを解決し、
// SessionWorkspace が要求する repository membership は要求しない。kind だけで分岐する caller は repository list に触れず、
// membership が0件でも root-snapshot 処理を判定できる必要があるためである。
func (s *Store) SessionWorkspaceKind(ctx context.Context, sessionID string) (string, error) {
	var kind string
	err := s.db.QueryRowContext(ctx, `SELECT w.kind FROM sessions se JOIN workspaces w ON w.id=se.workspace_id WHERE se.id=?`, sessionID).Scan(&kind)
	return kind, err
}

// SessionWorkspace は session の workspace に少なくとも1つの repository membership row（session_repositories）も要求する。
// 0件では repository set を検証できず、w.Repositories を反復する caller がその証明に依存するためである。
// w.Kind だけが必要な caller は、この要求を持たない SessionWorkspaceKind を使う。
func (s *Store) SessionWorkspace(ctx context.Context, sessionID string) (discovery.Workspace, error) {
	var w discovery.Workspace
	err := s.db.QueryRowContext(ctx, `SELECT w.id,w.root_path,w.kind FROM sessions se JOIN workspaces w ON w.id=se.workspace_id WHERE se.id=?`, sessionID).Scan(&w.ID, &w.Root, &w.Kind)
	if err != nil {
		return w, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT r.id,r.main_worktree_path,r.common_git_dir,sr.relative_path,r.default_branch,r.remote_name FROM session_repositories sr JOIN repositories r ON r.id=sr.repository_id WHERE sr.session_id=? ORDER BY sr.ordinal`, sessionID)
	if err != nil {
		return w, err
	}
	defer rows.Close()
	for rows.Next() {
		var r discovery.Repository
		if err := rows.Scan(&r.ID, &r.MainPath, &r.CommonDir, &r.RelativePath, &r.DefaultBranch, &r.RemoteName); err != nil {
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
	// ここで FAILED を安全とは扱わない。下で workspace_id を消すと ValidateWorktreeOwnership は workspace link を
	// 必要とするため、その slot の物理 path の所有権を二度と証明できず、FAILED worktree が回収不能な leak になる。
	// caller（Manager.Forget）は先に ScheduleFailedSlotRemoval で FAILED slot を retired にするため、他に危険な状態がなければ詰まらない。
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM slots WHERE workspace_id=? AND state NOT IN ('ARCHIVED')`, id).Scan(&unsafe); err != nil {
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
	if _, err := tx.ExecContext(ctx, `DELETE FROM standby_replenish_exclusions WHERE workspace_id=?`, id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM standby_replenish_successes WHERE workspace_id=?`, id); err != nil {
		return err
	}
	// jobs.workspace_id には foreign key がなく、監査目的で削除後の workspace を参照できるため、手動で clear する必要がある唯一の参照である。
	// slots.workspace_id と sessions.workspace_id は NULL へ cascade し、workspace_repositories row は各列の foreign key により削除へ cascade する。
	if _, err := tx.ExecContext(ctx, `UPDATE jobs SET workspace_id=NULL WHERE workspace_id=?`, id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM workspaces WHERE id=?`, id); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) Status(ctx context.Context) (Status, error) {
	var x Status
	err := s.db.QueryRowContext(ctx, `SELECT
		(SELECT count(*) FROM workspaces),
		(SELECT count(*) FROM repositories),
		(SELECT count(*) FROM slots WHERE state='READY'),
		(SELECT count(*) FROM slots WHERE state='LEASED'),
		(SELECT count(*) FROM slots WHERE state='FAILED'),
		(SELECT count(*) FROM sessions WHERE state='ACTIVE'),
		(SELECT count(*) FROM snapshots WHERE status='ARCHIVED'),
		(SELECT count(*) FROM jobs WHERE state IN ('PENDING','RUNNING')),
		(SELECT count(*) FROM slots WHERE state='QUARANTINED')`).
		Scan(&x.Workspaces, &x.Repositories, &x.Ready, &x.Leased, &x.Failed, &x.Active, &x.Snapshots, &x.Jobs, &x.Quarantined)
	return x, err
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
	quarantineRows, err := s.db.QueryContext(ctx, `SELECT sl.id,rt.path||'/'||sl.rel_path,COALESCE(sl.failure_code,'') FROM slots sl JOIN roots rt ON rt.id=sl.root_id WHERE sl.state='QUARANTINED' UNION ALL SELECT '',path,reason FROM quarantined_artifacts ORDER BY 2`)
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
	expired, err := expireQuarantinedOwnerTx(ctx, tx, sessionID, slotID)
	if err != nil {
		return Job{}, false, err
	}
	if expired {
		return Job{}, false, tx.Commit()
	}
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
	rows, err := s.db.QueryContext(ctx, `SELECT sl.id,rt.path||'/'||sl.rel_path,sl.state FROM slots sl JOIN roots rt ON rt.id=sl.root_id ORDER BY 2`)
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
	rows, err := tx.QueryContext(ctx, `SELECT DISTINCT session_id FROM snapshots WHERE head_recovery_ref=? OR worktree_recovery_ref=? OR index_recovery_ref=?`, ref, ref, ref)
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
	if _, err := tx.ExecContext(ctx, `UPDATE snapshots SET status='QUARANTINED' WHERE head_recovery_ref=? OR worktree_recovery_ref=? OR index_recovery_ref=?`, ref, ref, ref); err != nil {
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
	rows, err := s.db.QueryContext(ctx, `SELECT id,main_worktree_path,common_git_dir,default_branch,remote_name FROM repositories ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var repositories []discovery.Repository
	for rows.Next() {
		var repository discovery.Repository
		if err := rows.Scan(&repository.ID, &repository.MainPath, &repository.CommonDir, &repository.DefaultBranch, &repository.RemoteName); err != nil {
			return nil, err
		}
		repositories = append(repositories, repository)
	}
	return repositories, rows.Err()
}

// RecoveryRefExpectations は公開済み recovery ref ごとの durable ownership proof を返す。metadata を commit 済みで ref 公開未完了の snapshot job は、
// pending job または未期限 worker lease があれば InFlight とする。reconcile はその ref を待てるが、name/object ID を厳密に説明できない ref は quarantine する。
// commentlint:allow-long -- ref 公開途中の job と未知 ref の扱いを区別するため
func (s *Store) RecoveryRefExpectations(ctx context.Context, repositoryID string) ([]RecoveryRefExpectation, error) {
	at := now()
	rows, err := s.db.QueryContext(ctx, `
		WITH inflight AS (
			SELECT session_id FROM jobs
			WHERE kind='SNAPSHOT' AND (state='PENDING' OR (state='RUNNING' AND lease_expires_at>?))
		)
		SELECT sn.head_recovery_ref,sn.head_oid,sn.session_id,se.state,
		   CASE WHEN se.state IN ('RELEASING','SNAPSHOTTING') AND EXISTS (SELECT 1 FROM inflight i WHERE i.session_id=sn.session_id) THEN 1 ELSE 0 END
		FROM snapshots sn JOIN sessions se ON se.id=sn.session_id
		WHERE sn.repository_id=? AND sn.status='ARCHIVED'
		UNION ALL
		SELECT sn.worktree_recovery_ref,sn.worktree_snapshot_oid,sn.session_id,se.state,
		   CASE WHEN se.state IN ('RELEASING','SNAPSHOTTING') AND EXISTS (SELECT 1 FROM inflight i WHERE i.session_id=sn.session_id) THEN 1 ELSE 0 END
		FROM snapshots sn JOIN sessions se ON se.id=sn.session_id
		WHERE sn.repository_id=? AND sn.status='ARCHIVED'
		UNION ALL
		SELECT sn.index_recovery_ref,sn.index_tree_oid,sn.session_id,se.state,
		   CASE WHEN se.state IN ('RELEASING','SNAPSHOTTING') AND EXISTS (SELECT 1 FROM inflight i WHERE i.session_id=sn.session_id) THEN 1 ELSE 0 END
		FROM snapshots sn JOIN sessions se ON se.id=sn.session_id
		WHERE sn.repository_id=? AND sn.status='ARCHIVED' AND sn.index_recovery_ref<>''
		ORDER BY 1`, at, repositoryID, repositoryID, repositoryID)
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

// HotRepositoryIDs は hot_standby window、つまり hotBefore より後に lease された repository を返す。
// 一度も lease されていない repository（last_leased_at IS NULL）は retention.hot_standby 上まだ「使用済み」でなく、
// 実際の lease 前に replacement standby を先読み作成してはならないため除外する。
func (s *Store) HotRepositoryIDs(ctx context.Context, hotBefore string) (map[string]bool, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM repositories WHERE last_leased_at IS NOT NULL AND last_leased_at>?`, hotBefore)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}

type ColdRepositoryCandidate struct{ SlotID, WorkspaceID, RepositoryID, WorktreePath string }

func (s *Store) ColdRepositoryCandidates(ctx context.Context, hotBefore string) ([]ColdRepositoryCandidate, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT sl.id,sl.workspace_id,sr.repository_id,rt.path||'/'||sl.rel_path||'/'||sr.dir_name FROM slots sl JOIN roots rt ON rt.id=sl.root_id JOIN slot_repositories sr ON sr.slot_id=sl.id JOIN repositories r ON r.id=sr.repository_id WHERE sl.owner_session_id IS NULL AND sl.state='READY' AND sr.state='READY' AND (r.last_leased_at IS NULL OR r.last_leased_at<=?) ORDER BY sl.id,sr.repository_id`, hotBefore)
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
	rows, err := s.db.QueryContext(ctx, `SELECT sl.id,sl.workspace_id,rt.path||'/'||sl.rel_path,sl.state,COALESCE(sl.ready_at,sl.created_at) FROM slots sl JOIN roots rt ON rt.id=sl.root_id WHERE sl.owner_session_id IS NULL AND sl.state IN ('READY','STALE') ORDER BY sl.workspace_id,COALESCE(sl.ready_at,sl.created_at) DESC,sl.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	kept := map[string]int{}
	var out []StandbyGCCandidate
	for rows.Next() {
		var candidate StandbyGCCandidate
		var readyAt string
		if err := rows.Scan(&candidate.SlotID, &candidate.WorkspaceID, &candidate.Path, &candidate.State, &readyAt); err != nil {
			return nil, err
		}
		if candidate.State == "STALE" || kept[candidate.WorkspaceID] >= warm {
			out = append(out, candidate)
			continue
		}
		kept[candidate.WorkspaceID]++
	}
	return out, rows.Err()
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
	s.writer.Lock()
	defer s.writer.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	t := now()
	res, err := tx.ExecContext(ctx, `UPDATE slots SET state='ARCHIVED',updated_at=?,failure_code=NULL,failure_detail_path=NULL WHERE id=? AND state IN ('REMOVING','ARCHIVED')`, t, slotID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return fmt.Errorf("slot %s state compare-and-swap failed", slotID)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO events(time,level,kind,workspace_id,slot_id,message) SELECT ?,'info','slot_transition',workspace_id,id,? FROM slots WHERE id=?`, t, "state=ARCHIVED failure_code=", slotID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM standby_replenish_exclusions WHERE slot_id=?`, slotID); err != nil {
		return err
	}
	return tx.Commit()
}

// FailedSlotIDs は workspace に属する未所有の FAILED slot を返す。
func (s *Store) FailedSlotIDs(ctx context.Context, workspaceID string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM slots WHERE workspace_id=? AND owner_session_id IS NULL AND state='FAILED'`, workspaceID)
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

// ScheduleFailedSlotRemoval は FAILED slot の物理 worktree を retired にして row を ARCHIVED へ進める。
// ScheduleRemoval（READY/STALE/SNAPSHOTTED）とは別に、ForgetWorkspace が非 ARCHIVED slot を拒否しても
// 回収不能な FAILED worktree を残さないために用意する。FAILED を安全済みと扱えない理由は ForgetWorkspace を参照する。
func (s *Store) ScheduleFailedSlotRemoval(ctx context.Context, slotID string) (Job, bool, error) {
	job, err := newJob("REMOVE", "", slotID, "")
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
	res, err := tx.ExecContext(ctx, `UPDATE slots SET state='REMOVING',owner_session_id=NULL,updated_at=? WHERE id=? AND owner_session_id IS NULL AND state='FAILED'`, now(), slotID)
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

func (s *Store) ExpiredSnapshots(ctx context.Context, before string) ([]Snapshot, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT sn.id,sn.session_id,sn.repository_id,sn.head_oid,sn.head_recovery_ref,sn.index_tree_oid,sn.index_recovery_ref,sn.worktree_snapshot_oid,sn.worktree_recovery_ref,sn.status,sn.created_at,sn.expires_at FROM snapshots sn JOIN sessions se ON se.id=sn.session_id JOIN slots sl ON sl.id=se.slot_id WHERE se.state='ARCHIVED' AND sl.state='ARCHIVED' AND sn.status='ARCHIVED' AND sn.expires_at<=? AND NOT EXISTS (SELECT 1 FROM sessions child JOIN jobs j ON j.session_id=child.id WHERE child.parent_session_id=se.id AND j.kind='RESTORE' AND j.state IN ('PENDING','RUNNING')) ORDER BY sn.session_id,sn.repository_id`, before)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Snapshot
	for rows.Next() {
		var snapshot Snapshot
		if err := rows.Scan(&snapshot.ID, &snapshot.SessionID, &snapshot.RepositoryID, &snapshot.HeadOID, &snapshot.HeadRef, &snapshot.IndexTreeOID, &snapshot.IndexRef, &snapshot.WorktreeOID, &snapshot.WorktreeRef, &snapshot.Status, &snapshot.CreatedAt, &snapshot.ExpiresAt); err != nil {
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

// CountMetadataCandidates は指定 threshold で PruneMetadata が remove または tombstone する row 数を、変更せずに返す。
// `wx gc --dry-run` を支え、報告値に TTL 切れ event と完了 job metadata を含めて最高の GC priority tier と一致させる。
func (s *Store) CountMetadataCandidates(ctx context.Context, failedBefore, eventBefore, tombstoneBefore string) (int, error) {
	var jobs, events, tombstones, idempotency int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM jobs WHERE (state='SUCCEEDED' OR state='FAILED') AND COALESCE(finished_at,started_at,not_before)<=?`, failedBefore).Scan(&jobs); err != nil {
		return 0, err
	}
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM events WHERE time<=?`, eventBefore).Scan(&events); err != nil {
		return 0, err
	}
	// PruneMetadata は agent_session_id を消して tombstone 化するため、処理済み session は候補ではない。
	// 数えると `wx gc --dry-run` が何も変わらない同じ作業を報告し続ける。
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sessions WHERE state='EXPIRED' AND expires_at<=? AND agent_session_id IS NOT NULL`, tombstoneBefore).Scan(&tombstones); err != nil {
		return 0, err
	}
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM rpc_idempotency WHERE expires_at<=?`, now()).Scan(&idempotency); err != nil {
		return 0, err
	}
	return jobs + events + tombstones + idempotency, nil
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
	rows, err := s.db.QueryContext(ctx, `SELECT sl.id,se.id,rt.path||'/'||sl.rel_path FROM slots sl JOIN roots rt ON rt.id=sl.root_id JOIN sessions se ON se.slot_id=sl.id WHERE sl.state='SNAPSHOTTED' AND se.archived_at<=?`, before)
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
	b := make([]byte, 32)
	if _, err := cryptorand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
