package daemon

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/HappyOnigiri/WX/internal/config"
	"github.com/HappyOnigiri/WX/internal/gitx"
	"github.com/HappyOnigiri/WX/internal/state"
	"github.com/HappyOnigiri/WX/internal/workspace"
)

// managerFixtureLogLimit は失敗時に出す Manager ログ末尾の上限バイト数。
// 長時間走る統合テストのログ全量はテスト出力を埋めるため、末尾だけを残す。
const managerFixtureLogLimit = 32 << 10

// managerFixtureDiagnosticsBudget は失敗時の診断取得に与える期限。
// DB が閉じている・ロックされている場合でも、元の失敗の報告を待たせない。
const managerFixtureDiagnosticsBudget = 2 * time.Second

func requireDaemonIntegration(t *testing.T) {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping daemon integration test in short mode")
	}
}

// diagnosticLog は Manager のログを容量制限付きで保持する io.Writer である。
// 複数の worker が同時に書くため mutex で保護し、上限を超えた分は先頭から捨てる。
type diagnosticLog struct {
	mu      sync.Mutex
	limit   int
	buf     []byte
	dropped int
}

func newDiagnosticLog(limit int) *diagnosticLog {
	return &diagnosticLog{limit: limit}
}

func (l *diagnosticLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.buf = append(l.buf, p...)
	if excess := len(l.buf) - l.limit; excess > 0 {
		l.buf = l.buf[excess:]
		l.dropped += excess
	}
	return len(p), nil
}

// tail は保持しているログ末尾を返す。捨てた分があることは呼び出し側に分かるよう先頭に記す。
func (l *diagnosticLog) tail() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.buf) == 0 && l.dropped == 0 {
		return ""
	}
	if l.dropped == 0 {
		return string(l.buf)
	}
	return fmt.Sprintf("... %d bytes dropped ...\n%s", l.dropped, l.buf)
}

// managerFixtureSetup は Manager を作る前の準備内容である。
// option は一時ディレクトリの作成後・Manager の生成前に呼ばれるので、Config の調整と事前のディスク準備に使える。
type managerFixtureSetup struct {
	Root   string
	Config *config.Config
}

type managerFixtureOption func(*managerFixtureSetup)

// managerFixture は daemon テストが共有する Manager の準備と後片付けを持つ。
// 所有するのは fixture が作った一時ディレクトリ・DB・Manager だけで、テストが自分で開いた資源は引き取らない。
type managerFixture struct {
	t            *testing.T
	Root         string
	DatabasePath string
	Config       config.Config
	Store        *state.Store
	Manager      *Manager
	logs         *diagnosticLog
}

// manualManagerFixture は worker と周期処理を起動しない手動駆動の Manager を返す。
// 復旧処理も所有権マーカーの補修も走らないため、テストが仕込んだ不整合はそのまま残る。
func manualManagerFixture(t *testing.T, options ...managerFixtureOption) *managerFixture {
	t.Helper()
	f := newManagerFixture(t, options...)
	f.Manager = testManager(t, f.Config, f.Store)
	f.Manager.log = slog.New(slog.NewTextHandler(f.logs, nil))
	t.Cleanup(f.cleanup)
	return f
}

// runningManagerFixture は New と同じ経路で、復旧処理と worker・周期処理を起動した Manager を返す。
func runningManagerFixture(t *testing.T, options ...managerFixtureOption) *managerFixture {
	t.Helper()
	f := newManagerFixture(t, options...)
	f.Manager = New(f.Config, f.Store, slog.New(slog.NewTextHandler(f.logs, nil)))
	t.Cleanup(f.cleanup)
	return f
}

// newManagerFixture は Manager を除く共通準備を行う。t.TempDir より後に cleanup を登録させ、削除順を保つ。
// macOS の /tmp 別名で Git の記録と path 表記がずれないよう、root を canonical path に揃える。
func newManagerFixture(t *testing.T, options ...managerFixtureOption) *managerFixture {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Defaults()
	cfg.Storage.WorktreeRoot = filepath.Join(root, "worktrees")
	setup := managerFixtureSetup{Root: root, Config: &cfg}
	for _, option := range options {
		option(&setup)
	}
	if err := config.NormalizePaths(&cfg); err != nil {
		t.Fatal(err)
	}
	databasePath := filepath.Join(root, "state.db")
	store, err := state.Open(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	return &managerFixture{t: t, Root: root, DatabasePath: databasePath, Config: cfg, Store: store, logs: newDiagnosticLog(managerFixtureLogLimit)}
}

// cleanup は worker と背景処理の停止・join、失敗時診断、DB close の順に片付ける。
// 一時ディレクトリの削除は t.TempDir の cleanup が最後に行う。
func (f *managerFixture) cleanup() {
	f.Manager.Close()
	f.reportDiagnostics()
	_ = f.Store.Close()
}

// reportDiagnostics は失敗したテストにだけ、Manager ログ末尾と slot・job の識別子と状態を出す。
// DB を意図的に閉じるテストでは取得失敗も診断として記録し、元の失敗を隠さない。
func (f *managerFixture) reportDiagnostics() {
	if !f.t.Failed() {
		return
	}
	if tail := f.logs.tail(); tail != "" {
		f.t.Logf("manager log tail:\n%s", tail)
	}
	ctx, cancel := context.WithTimeout(context.Background(), managerFixtureDiagnosticsBudget)
	defer cancel()
	details, detailsErr := f.Store.StatusDiagnostics(ctx)
	f.t.Logf("status diagnostics=%+v error=%v", details, detailsErr)
	suspensions, suspensionsErr := f.Store.StandbyReplenishmentDiagnostics(ctx)
	f.t.Logf("standby replenishment=%+v error=%v", suspensions, suspensionsErr)
	artifacts, artifactsErr := f.Store.SlotArtifacts(ctx)
	if artifactsErr != nil {
		f.t.Logf("slot artifacts error=%v", artifactsErr)
	}
	for _, artifact := range artifacts {
		slot, slotErr := f.Store.Slot(ctx, artifact.ID)
		repositories, repositoriesErr := f.Store.SlotRepositories(ctx, artifact.ID)
		f.t.Logf("slot=%+v error=%v repositories=%+v error=%v", slot, slotErr, repositories, repositoriesErr)
	}
	f.t.Logf("jobs:\n%s", f.jobDiagnostics(ctx))
}

// jobDiagnostics は job の識別子と状態を読む。Store に一覧APIがないため fixture が持つ DB を直接読む。
func (f *managerFixture) jobDiagnostics(ctx context.Context) string {
	raw, err := sql.Open("sqlite", f.DatabasePath)
	if err != nil {
		return fmt.Sprintf("open error=%v", err)
	}
	defer func() { _ = raw.Close() }()
	rows, err := raw.QueryContext(ctx, `SELECT id,kind,state,COALESCE(slot_id,''),COALESCE(session_id,''),COALESCE(error_code,'') FROM jobs ORDER BY id`)
	if err != nil {
		return fmt.Sprintf("query error=%v", err)
	}
	defer func() { _ = rows.Close() }()
	var report strings.Builder
	for rows.Next() {
		var id, kind, jobState, slotID, sessionID, errorCode string
		if err := rows.Scan(&id, &kind, &jobState, &slotID, &sessionID, &errorCode); err != nil {
			return fmt.Sprintf("scan error=%v", err)
		}
		fmt.Fprintf(&report, "job=%s kind=%s state=%s slot=%s session=%s error_code=%s\n", id, kind, jobState, slotID, sessionID, errorCode)
	}
	if err := rows.Err(); err != nil {
		fmt.Fprintf(&report, "rows error=%v\n", err)
	}
	return report.String()
}

// testManager は worker を起動しない部分初期化の Manager を組む。
// cfg と store を自分で用意するテスト向けの入口で、fixture 全体を任せる場合は manualManagerFixture を使う。
func testManager(t *testing.T, cfg config.Config, store *state.Store) *Manager {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	root, err := config.ExpandHome(cfg.Storage.WorktreeRoot)
	if err != nil {
		t.Fatal(err)
	}
	m := &Manager{
		cfg:      cfg,
		store:    store,
		git:      &gitx.Runner{Timeout: time.Second},
		log:      slog.New(slog.NewTextHandler(newDiagnosticLog(managerFixtureLogLimit), nil)),
		started:  time.Now(),
		roots:    map[string]bool{filepath.Clean(root): true},
		rootIDs:  map[string]string{},
		jobQueue: newJobQueue(cfg.Pool.PreparationConcurrency),
		ctx:      ctx,
		cancel:   cancel,

		slotUsage:   map[string]slotUsageSample{},
		sharedFiles: map[string]workspace.SharedFileCache{},
	}
	_, _ = tryRegisterTestRoot(m, filepath.Clean(root))
	return m
}

func waitReady(ctx context.Context, m *Manager, budget time.Duration, sessionID, token string) error {
	waitCtx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	return m.WaitReady(waitCtx, sessionID, token)
}

func waitUntil(t *testing.T, timeout time.Duration, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("condition was not met before timeout")
}

func TestDiagnosticLogKeepsBoundedTailUnderConcurrentWrites(t *testing.T) {
	t.Parallel()
	log := newDiagnosticLog(64)
	var writers sync.WaitGroup
	for writer := range 8 {
		writers.Add(1)
		go func() {
			defer writers.Done()
			for range 32 {
				if _, err := log.Write([]byte(fmt.Sprintf("writer-%d\n", writer))); err != nil {
					t.Errorf("write: %v", err)
				}
			}
		}()
	}
	writers.Wait()
	tail := log.tail()
	if !strings.HasPrefix(tail, "... ") || !strings.Contains(tail, " bytes dropped ...") {
		t.Fatalf("truncated tail did not report the dropped bytes: %q", tail)
	}
	body := tail[strings.Index(tail, "\n")+1:]
	if len(body) != 64 {
		t.Fatalf("retained tail=%d bytes, want the 64 byte limit: %q", len(body), body)
	}
	if !strings.Contains(body, "writer-") {
		t.Fatalf("retained tail lost the log content: %q", body)
	}
}

func TestDiagnosticLogTailIsEmptyUntilSomethingIsLogged(t *testing.T) {
	t.Parallel()
	if got := newDiagnosticLog(8).tail(); got != "" {
		t.Fatalf("empty log tail=%q", got)
	}
}

// 閉じた DB でも診断は取得失敗を報告して返る。元の失敗を隠す panic も無限待ちも起こさない。
func TestManagerFixtureDiagnosticsSurviveAClosedStore(t *testing.T) {
	t.Parallel()
	f := manualManagerFixture(t)
	if err := f.Store.Close(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), managerFixtureDiagnosticsBudget)
	defer cancel()
	if _, err := f.Store.StatusDiagnostics(ctx); err == nil {
		t.Fatal("closed store reported diagnostics")
	}
	if report := f.jobDiagnostics(ctx); !strings.Contains(report, "job=") && report != "" {
		t.Fatalf("job diagnostics after closure=%q, want the rows read from the file or an error", report)
	}
}
