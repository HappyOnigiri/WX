package daemon

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
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
	executablePath       string
	executableBaseline   executableSnapshot
	executableWatch      bool
	prepareDetailDir     string
	restartPending       bool
	stopPending          bool
	lifecycleClaimed     bool
	lifecycleAttempts    int
	restartUnmanaged     bool
	inflightRequests     int
	inflightLifecycle    int
	lastLifecycleEnd     time.Time
	kickstart            func(context.Context) error
	terminate            func() error
	launchdManaged       func() bool
	jobs                 chan jobWork
	lifecycleChecks      chan struct{}
	reloads              chan struct{}
	ctx                  context.Context
	cancel               context.CancelFunc
	wg                   sync.WaitGroup
	workersMu            sync.Mutex
	workerStops          []chan struct{}
	workerSeq            int
	closed               bool
	logLevel             *slog.LevelVar
	backgroundMu         sync.Mutex
	backgroundWG         sync.WaitGroup
	backgroundClosing    bool
	reloadMu             sync.Mutex
	closeOnce            sync.Once
	closeDoneMu          sync.Mutex
	closeDone            chan struct{}
	// cleanDrivers は run ごとの進行管理が二重に走らないようにする。同じ run への再実行は既存の driver へ合流する。
	cleanDrivers map[string]bool
	// standbySuspensionWarned は補充停止の警告を workspace ごとに一度だけ出すための記録。
	standbySuspensionWarned map[string]bool
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
	m := &Manager{cfg: cfg, store: store, git: git, log: logger, started: started, prepareDetailDir: prepareDetailDir, lastReload: started, roots: map[string]bool{}, rootRefs: map[string]*managedRoot{}, retiredRefs: map[string][]*managedRoot{}, rootIdentities: map[string]string{}, rootIDs: map[string]string{}, rootUsage: map[string]rootUsageSample{}, slotUsage: map[string]slotUsageSample{}, sharedFiles: map[string]workspace.SharedFileCache{}, leases: map[string]func(){}, jobs: make(chan jobWork, 256), lifecycleChecks: make(chan struct{}, 1), reloads: make(chan struct{}, 1), ctx: managerCtx, cancel: managerCancel}
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
	m.resizeWorkers(cfg.Pool.PreparationConcurrency)
	m.reconcileStandbyReplenishments(context.Background())
	m.wg.Add(2)
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
		m.workersMu.Lock()
		m.closed = true
		m.workersMu.Unlock()
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
