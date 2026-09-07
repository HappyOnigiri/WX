// Package metacache は会話メタデータの解析結果を再生成可能な SQLite へ保存し、未変更 JSONL の再解析を省く。
// daemon の state.db とは独立した cache であり、破損・書き込み不能・schema 差異では利用を諦めて呼び出し側の直接走査へ戻す。
package metacache

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"

	_ "modernc.org/sqlite" // database/sql の "sqlite" driver を登録する。
)

// schemaVersion は cache 表の形状の版であり、不一致の DB は作り直す。
// 解析器の版は entry ごとの検証値に含めるため、ここでは扱わない。
const schemaVersion = 1

const entriesDDL = `CREATE TABLE IF NOT EXISTS entries (
	agent TEXT NOT NULL,
	path TEXT NOT NULL,
	identity TEXT NOT NULL,
	size INTEGER NOT NULL,
	mtime_ns INTEGER NOT NULL,
	ctime_ns INTEGER NOT NULL,
	parser_version INTEGER NOT NULL,
	native_id TEXT NOT NULL,
	title TEXT NOT NULL,
	cwd TEXT NOT NULL,
	excluded INTEGER NOT NULL,
	PRIMARY KEY (agent, path)
) WITHOUT ROWID`

const upsertEntry = `INSERT INTO entries (agent, path, identity, size, mtime_ns, ctime_ns, parser_version, native_id, title, cwd, excluded)
	VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	ON CONFLICT(agent, path) DO UPDATE SET identity = excluded.identity, size = excluded.size, mtime_ns = excluded.mtime_ns,
		ctime_ns = excluded.ctime_ns, parser_version = excluded.parser_version, native_id = excluded.native_id,
		title = excluded.title, cwd = excluded.cwd, excluded = excluded.excluded`

// Entry は保存済みの検証値と解析結果の組である。
type Entry struct {
	Validator Validator
	Record    Record
}

// Record は Session の組み立てに必要な解析結果だけを表し、会話本文は保持しない。
// Excluded は subagent やメタデータ不足で一覧から除いた結果で、再解析を避けるために保存する。
type Record struct {
	NativeID string
	Title    string
	CWD      string
	Excluded bool
}

// Validator は保存済み Record を再利用してよいかを決めるファイル属性である。
// macOS では mtime を復元した同一 inode 上書きも検出するため ctime を含める。
type Validator struct {
	Identity      string
	Size          int64
	MtimeNS       int64
	CtimeNS       int64
	ParserVersion int
}

// Cache は cache DB への接続を保持する。
// nil receiver は「cache 無し」として全メソッドが無害に振る舞うため、呼び出し側は取得失敗を分岐せずに走査を続けられる。
type Cache struct {
	db       *sql.DB
	mu       sync.Mutex
	disabled bool
}

// DefaultPath はユーザーの cache directory 内の cache DB の path を返す。
func DefaultPath() (string, error) {
	dir, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "wx", "sessions.db"), nil
}

// Open は cache DB を開く。壊れた DB は一度だけ作り直して開き直す。
func Open(path string) (*Cache, error) {
	cache, err := open(path)
	if err == nil {
		return cache, nil
	}
	if removeErr := removeDatabase(path); removeErr != nil {
		return nil, errors.Join(err, removeErr)
	}
	return open(path)
}

func open(path string) (*Cache, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	dsn := (&url.URL{Scheme: "file", Path: path}).String() + "?_journal_mode=wal&_busy_timeout=3000&_synchronous=normal"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// 複数 CLI が同じ DB を触るため、書き込みは短い transaction と busy timeout に任せる。
	db.SetMaxOpenConns(4)
	cache := &Cache{db: db}
	if err := cache.init(context.Background()); err != nil {
		_ = db.Close()
		return nil, err
	}
	_ = os.Chmod(path, 0o600)
	return cache, nil
}

func removeDatabase(path string) error {
	var errs []error
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if err := os.Remove(path + suffix); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (c *Cache) init(ctx context.Context) error {
	if _, err := c.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS cache_meta (key TEXT PRIMARY KEY, value INTEGER NOT NULL)`); err != nil {
		return err
	}
	var version int
	err := c.db.QueryRowContext(ctx, `SELECT value FROM cache_meta WHERE key = 'schema_version'`).Scan(&version)
	switch {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return err
	case version != schemaVersion:
		if _, err := c.db.ExecContext(ctx, `DROP TABLE IF EXISTS entries`); err != nil {
			return err
		}
	}
	if _, err := c.db.ExecContext(ctx, entriesDDL); err != nil {
		return err
	}
	_, err = c.db.ExecContext(ctx, `INSERT INTO cache_meta (key, value) VALUES ('schema_version', ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value`, schemaVersion)
	return err
}

// Close は cache DB を閉じる。
func (c *Cache) Close() error {
	if c == nil {
		return nil
	}
	return c.db.Close()
}

// Snapshot は agent の保存済み entry を path 別に一括で読む。
// 走査の先頭で 1 回だけ問い合わせ、ファイルごとの往復をなくす。読み取りに失敗した cache は空として扱う。
func (c *Cache) Snapshot(ctx context.Context, agent string) map[string]Entry {
	if c == nil {
		return nil
	}
	const query = `SELECT path, identity, size, mtime_ns, ctime_ns, parser_version, native_id, title, cwd, excluded FROM entries WHERE agent = ?`
	rows, err := c.db.QueryContext(ctx, query, agent)
	if err != nil {
		return nil
	}
	defer func() { _ = rows.Close() }()
	snapshot := map[string]Entry{}
	for rows.Next() {
		var path string
		var entry Entry
		var excluded int
		if err := rows.Scan(&path, &entry.Validator.Identity, &entry.Validator.Size, &entry.Validator.MtimeNS, &entry.Validator.CtimeNS,
			&entry.Validator.ParserVersion, &entry.Record.NativeID, &entry.Record.Title, &entry.Record.CWD, &excluded); err != nil {
			return nil
		}
		entry.Record.Excluded = excluded != 0
		snapshot[path] = entry
	}
	if err := rows.Err(); err != nil {
		return nil
	}
	return snapshot
}

// Save は解析結果をまとめて 1 つの transaction で保存する。
// 書き込みに失敗した cache はそれ以降使わず、呼び出し側は走査だけを続ける。
func (c *Cache) Save(ctx context.Context, agent string, entries map[string]Entry) {
	if c == nil || len(entries) == 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.disabled {
		return
	}
	if err := c.saveAll(ctx, agent, entries); err != nil {
		c.disabled = true
	}
}

func (c *Cache) saveAll(ctx context.Context, agent string, entries map[string]Entry) error {
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	statement, err := tx.PrepareContext(ctx, upsertEntry)
	if err != nil {
		return err
	}
	defer func() { _ = statement.Close() }()
	for path, entry := range entries {
		excluded := 0
		if entry.Record.Excluded {
			excluded = 1
		}
		validator := entry.Validator
		if _, err := statement.ExecContext(ctx, agent, path, validator.Identity, validator.Size, validator.MtimeNS, validator.CtimeNS,
			validator.ParserVersion, entry.Record.NativeID, entry.Record.Title, entry.Record.CWD, excluded); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Prune は走査を完走した root 配下から、今回列挙されなかった entry を消す。
// 途中で失敗・中断した走査で呼ぶと未走査のファイルを消してしまうため、呼び出し側は完走したときだけ呼ぶ。
func (c *Cache) Prune(ctx context.Context, agent, root string, seen map[string]struct{}) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.disabled {
		return
	}
	stale, err := c.stalePaths(ctx, agent, root, seen)
	if err != nil {
		c.disabled = true
		return
	}
	for _, path := range stale {
		if _, err := c.db.ExecContext(ctx, `DELETE FROM entries WHERE agent = ? AND path = ?`, agent, path); err != nil {
			c.disabled = true
			return
		}
	}
}

func (c *Cache) stalePaths(ctx context.Context, agent, root string, seen map[string]struct{}) ([]string, error) {
	prefix := strings.TrimRight(filepath.Clean(root), string(filepath.Separator)) + string(filepath.Separator)
	rows, err := c.db.QueryContext(ctx, `SELECT path FROM entries WHERE agent = ?`, agent)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var stale []string
	for rows.Next() {
		var path string
		if err := rows.Scan(&path); err != nil {
			return nil, err
		}
		if _, kept := seen[path]; kept || !strings.HasPrefix(path, prefix) {
			continue
		}
		stale = append(stale, path)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return stale, nil
}
