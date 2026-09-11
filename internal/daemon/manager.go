package daemon

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/HappyOnigiri/WX/internal/config"
	"github.com/HappyOnigiri/WX/internal/gitx"
	"github.com/HappyOnigiri/WX/internal/state"
	"github.com/HappyOnigiri/WX/internal/workspace"
)

type Manager struct {
	mu                   sync.RWMutex
	cfg                  config.Config
	store                *state.Store
	git                  *gitx.Runner
	log                  *slog.Logger
	started              time.Time
	lastReload           time.Time
	reloadError          string
	lastBackup           time.Time
	backupError          string
	roots                map[string]bool
	rootRefs             map[string]*managedRoot
	retiredRefs          map[string][]*managedRoot
	rootIdentities       map[string]string
	rootIDs              map[string]string
	rootUsage            map[string]rootUsageSample
	slotUsage            map[string]slotUsageSample
	sharedFiles          map[string]workspace.SharedFileCache
	rootError            string
	rootRetryLogged      string
	rootCond             *sync.Cond
	rootClosing          bool
	leases               map[string]func()
	beforeSlotRootCreate func()
	beforeRootClose      func()
	// beforeJobRun は job の実行直前に呼ぶ試験用の barrier。実行枠のクラス分離を実際の配送経路で確かめるために持つ。
	beforeJobRun       func(state.Job)
	executablePath     string
	executableBaseline executableSnapshot
	executableWatch    bool
	prepareDetailDir   string
	restartPending     bool
	stopPending        bool
	lifecycleClaimed   bool
	lifecycleAttempts  int
	lifecycleRetryAt   time.Time
	restartUnmanaged   bool
	inflightRequests   int
	inflightLifecycle  int
	lastLifecycleEnd   time.Time
	kickstart          func(context.Context) error
	terminate          func() error
	launchdManaged     func() bool
	jobQueue           *jobQueue
	// slotLocks は同じ slot へ書く準備・復元・保存・削除を直列化する。
	// prepare が common-directory lock を手放す区間の排他をこれが引き受けるため、全 Preparer と archive.Manager で共有する。
	slotLocks         gitx.KeyedLocks
	jobSeq            atomic.Uint64
	lifecycleChecks   chan struct{}
	reloads           chan struct{}
	ctx               context.Context
	cancel            context.CancelFunc
	wg                sync.WaitGroup
	logLevel          *slog.LevelVar
	backgroundMu      sync.Mutex
	backgroundWG      sync.WaitGroup
	backgroundClosing bool
	reloadMu          sync.Mutex
	closeOnce         sync.Once
	closeDoneMu       sync.Mutex
	closeDone         chan struct{}
	// cleanDrivers は run ごとの進行管理が二重に走らないようにする。同じ run への再実行は既存の driver へ合流する。
	cleanDrivers map[string]bool
	// standbySuspensionWarned は補充停止の警告を workspace ごとに一度だけ出すための記録。
	standbySuspensionWarned map[string]bool
	// inFlightReservations は自プロセスで進行中の slot 予約。reconcile が中断された確保と取り違えないために持つ。
	inFlightReservations map[string]bool
	// leasingWorkspaces は貸出処理が進行中の workspace の件数。補充が使用中の workspace を cold と判定しないために持つ。
	leasingWorkspaces map[string]int
	// idleStandbyRefreshes は workspace ごとに idle 更新を最後に始めた時刻。頻繁な fingerprint の変化で更新が連鎖しないための歯止め。
	idleStandbyRefreshes map[string]time.Time
	// maintenanceMu は registry reconcile と GC の一巡を1本に保つ。running 中の要求は dirty へ集約する。
	maintenanceMu      sync.Mutex
	maintenanceRunning bool
	maintenanceDirty   bool
	// usageMu は使用量の測り直し要求を1本に保つ。running 中の要求は dirty へ集約する。
	usageMu      sync.Mutex
	usageRunning bool
	usageDirty   bool
	// beforeMaintenanceSweep は一巡の開始を数え、止めるための test 用 barrier。production では nil のままにする。
	beforeMaintenanceSweep func()
	// prepareMeasurements は直近の準備の区間内訳。`wx bench` の診断専用で、状態としては扱わない。
	prepareMeasurements []PrepareMeasurement
}

func New(cfg config.Config, store *state.Store, logger *slog.Logger, exclusiveStartup ...bool) *Manager {
	git := &gitx.Runner{Timeout: cfg.Readiness.Timeout.Duration}
	executable, executableErr := os.Executable()
	if executableErr == nil {
		git.FDHelper = executable
	}
	started := time.Now()
	managerCtx, managerCancel := context.WithCancel(context.Background())
	reclaimAll := len(exclusiveStartup) > 0 && exclusiveStartup[0]
	prepareDetailDir := ""
	if reclaimAll {
		if logPath, logErr := config.LogPath(); logErr == nil {
			prepareDetailDir = filepath.Join(filepath.Dir(logPath), "details")
		}
	}
	m := &Manager{cfg: cfg, store: store, git: git, log: logger, started: started, prepareDetailDir: prepareDetailDir, lastReload: started, roots: map[string]bool{}, rootRefs: map[string]*managedRoot{}, retiredRefs: map[string][]*managedRoot{}, rootIdentities: map[string]string{}, rootIDs: map[string]string{}, rootUsage: map[string]rootUsageSample{}, slotUsage: map[string]slotUsageSample{}, sharedFiles: map[string]workspace.SharedFileCache{}, leases: map[string]func(){}, jobQueue: newJobQueue(cfg.Pool.PreparationConcurrency), lifecycleChecks: make(chan struct{}, 1), reloads: make(chan struct{}, 1), ctx: managerCtx, cancel: managerCancel}
	m.rootCond = sync.NewCond(&m.mu)
	m.watchExecutable(executable, executableErr)
	if root, ownedRoot, err := ensureWorktreeRootDescriptor(cfg.Storage.WorktreeRoot); err == nil {
		m.roots[root] = true
		identity, identityErr := descriptorIdentity(ownedRoot)
		if identityErr != nil {
			logger.Error("worktree root identity is unavailable", "path", root, "error", identityErr)
		}
		m.rootIdentities[root] = identity
		m.rootRefs[root] = &managedRoot{root: ownedRoot, identity: identity}
		m.registerRootGeneration(context.Background(), root, identity)
	} else {
		logger.Error("worktree root is unavailable", "path", cfg.Storage.WorktreeRoot, "error", err)
	}
	m.loadRootGenerations(context.Background())
	m.recoverJobs(reclaimAll)
	m.reconcileStandbyReplenishments(context.Background())
	m.wg.Add(3)
	go func() { defer m.wg.Done(); m.dispatchJobs() }()
	go func() { defer m.wg.Done(); m.maintainJobs() }()
	go func() { defer m.wg.Done(); m.maintainLifecycle() }()
	return m
}

func (m *Manager) Close() {
	m.closeDoneMu.Lock()
	if m.closeDone == nil {
		m.closeDone = make(chan struct{})
	}
	done := m.closeDone
	m.closeDoneMu.Unlock()
	m.closeOnce.Do(func() {
		m.jobQueue.close()
		m.backgroundMu.Lock()
		m.backgroundClosing = true
		m.backgroundMu.Unlock()
		m.mu.RLock()
		beforeRootClose := m.beforeRootClose
		m.mu.RUnlock()
		if beforeRootClose != nil {
			beforeRootClose()
		}
		m.beginRootClose()
		if m.cancel != nil {
			m.cancel()
		}
		m.wg.Wait()
		m.backgroundWG.Wait()
		m.closeRootHandles()
		close(done)
	})
	<-done
}

func (m *Manager) startBackground(fn func()) bool {
	m.backgroundMu.Lock()
	if m.backgroundClosing {
		m.backgroundMu.Unlock()
		return false
	}
	m.backgroundWG.Add(1)
	m.backgroundMu.Unlock()
	go func() {
		defer m.backgroundWG.Done()
		fn()
	}()
	return true
}

func (m *Manager) Config() config.Config { m.mu.RLock(); defer m.mu.RUnlock(); return m.cfg }
