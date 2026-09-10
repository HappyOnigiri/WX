package state

import (
	"context"
	cryptorand "crypto/rand"
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

	sqlite "modernc.org/sqlite"

	"github.com/HappyOnigiri/WX/internal/domain"
	"github.com/HappyOnigiri/WX/migrations"
)

type Store struct {
	db     *sql.DB
	writer sync.Mutex
	path   string
	// backupGate は online backup と世代整理だけを直列化する。writer とは排他せず、backup 中も lease・heartbeat・release が進む。
	backupGate chan struct{}
	// closing は Close の開始を実行中の backup へ伝える。Close は backupGate を取り直して source 接続の返却を待つ。
	closing   chan struct{}
	closeOnce sync.Once
	// backupStepBarrier は step 間に並行書き込みを差し込む test hook である。production では nil のままにする。
	backupStepBarrier func()
}

const SchemaVersion = 9

// ErrPreviousWorktreeLayout は、wx が意図的に migration path を持たない旧 worktree layout の state database を示す。
var ErrPreviousWorktreeLayout = errors.New("wx database uses previous worktree layout")

// JSONSchemaVersion は `wx status --json` と `wx doctor --json` の出力形状の互換契約であり、SQLite migration 数の SchemaVersion とは独立である。
// scripted consumer が観測する形状を変える場合だけ上げる。2〜8 は restart・stop・daemon unavailable と root・workspace・quarantine の診断、回復案内を加えた。
// 9〜11 は measured_at・補充停止の理由・`wx slots` への置き換え、12〜16 は unmanaged・shared/exclusive・policy・resume の integrity・method Ping を加えた。
// 17 は `wx doctor` の checks map を、種別・原因・対処を持つ findings 配列へ置き換え、`wx status --json` の standby_replenishment に失敗情報を加えた。
// 18 は `wx slots --json` の各行へ貸出の種別・期限・親 session を、`wx status --json` の retention_seconds へ lease.ttl を加えた。
// 19 は `wx status --json` へ daemon 起動からの経過秒数 uptime_seconds を加えた。
// commentlint:allow-long -- schema 版ごとの変更点を辿れるようにするため
const JSONSchemaVersion = 19

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
	s := &Store{db: db, path: path, backupGate: make(chan struct{}, 1), closing: make(chan struct{})}
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

// Close は実行中の online backup を取り消し、その source 接続が pool へ戻るまで待ってから database を閉じる。
// Raw callback 実行中に db を閉じると source handle が消えるため、gate を取り直して backup の完了を確認する。
func (s *Store) Close() error {
	s.closeOnce.Do(func() { close(s.closing) })
	s.backupGate <- struct{}{}
	<-s.backupGate
	return s.db.Close()
}

func (s *Store) Ping(ctx context.Context) error { return s.db.PingContext(ctx) }

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
	// 旧 layout の判定は migration の前に行う。後段の migration が旧 schema に当たって別の error で落ちると、対処を示せなくなる。
	if applied > 0 {
		if err := s.verifySchema(ctx); err != nil {
			return err
		}
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
// それらは user_version=1 のため以降の migration も query も旧 schema に当たって失敗する。
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

const timestampFormat = "2006-01-02T15:04:05.000000000Z07:00"

// FormatTime は固定幅 RFC 3339 timestamp を返す。SQLite は wx の時刻を TEXT で保存するため、
// lexical comparison で時系列順を保つには固定の小数幅が必要である。
func FormatTime(value time.Time) string { return value.UTC().Format(timestampFormat) }

func now() string { return FormatTime(time.Now()) }

func HashToken(token string) []byte { sum := sha256.Sum256([]byte(token)); return sum[:] }

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
