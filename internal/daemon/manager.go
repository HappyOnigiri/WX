package daemon

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/HappyOnigiri/WX/internal/archive"
	"github.com/HappyOnigiri/WX/internal/config"
	"github.com/HappyOnigiri/WX/internal/diag"
	"github.com/HappyOnigiri/WX/internal/discovery"
	"github.com/HappyOnigiri/WX/internal/domain"
	"github.com/HappyOnigiri/WX/internal/gitx"
	"github.com/HappyOnigiri/WX/internal/pool"
	"github.com/HappyOnigiri/WX/internal/state"
	"github.com/HappyOnigiri/WX/internal/workspace"
)

type Lease struct {
	SessionID       string `json:"session_id"`
	Token           string `json:"token"`
	Path            string `json:"path"`
	RootIdentity    string `json:"root_identity,omitempty"`
	SourceWorkspace string `json:"source_workspace,omitempty"`
	Ready           bool   `json:"ready"`
}

// GCReason は GC が対象を処理できなかった理由を対象単位で返す。
// 安全のために保留した場合も、削除できなかった事実を呼出元が区別できるようにする。
type GCReason struct {
	Target string `json:"target"`
	Status string `json:"status"`
	Reason string `json:"reason"`
}

// GCResult は GC の予約・完了・保留・失敗を分けて報告する。
// REMOVE job は予約時点では物理削除が完了していないため、Scheduled と Completed を混同しない。
type GCResult struct {
	Candidates int        `json:"candidates"`
	Scheduled  int        `json:"scheduled"`
	Completed  int        `json:"completed"`
	Pending    int        `json:"pending"`
	Failed     int        `json:"failed"`
	Reasons    []GCReason `json:"reasons"`
}

// gcProgress は GC の内部処理で結果と呼出元へ返すエラーを同時に集約する。
type gcProgress struct {
	GCResult
	errs []error
}

func newGCProgress() gcProgress {
	return gcProgress{GCResult: GCResult{Reasons: []GCReason{}}}
}

func (p *gcProgress) addIssue(target, status, reason string, cause error) {
	if cause != nil {
		if reason == "" {
			reason = cause.Error()
		} else {
			reason = fmt.Sprintf("%s: %v", reason, cause)
		}
		p.errs = append(p.errs, fmt.Errorf("%s: %w", target, cause))
	}
	p.Reasons = append(p.Reasons, GCReason{Target: target, Status: status, Reason: reason})
}

func (p *gcProgress) addPending(target, reason string, cause error) {
	p.Pending++
	p.addIssue(target, "pending", reason, cause)
}

func (p *gcProgress) addFailed(target, reason string, cause error) {
	p.Failed++
	p.addIssue(target, "failed", reason, cause)
}

func (p *gcProgress) merge(other gcProgress) {
	p.Scheduled += other.Scheduled
	p.Completed += other.Completed
	p.Pending += other.Pending
	p.Failed += other.Failed
	p.Reasons = append(p.Reasons, other.Reasons...)
	p.errs = append(p.errs, other.errs...)
}

func (p *gcProgress) err() error {
	return errors.Join(p.errs...)
}

type managedRoot struct {
	// retired rootは操作またはleaseの参照が残る間だけ開き、最後のreleaseで閉じる。
	root     *os.Root
	identity string
	refs     int
	retired  bool
	closed   bool
}

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
	rootError            string
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
	// standbyQuarantineWarned は隔離上限による補充停止の警告を workspace ごとに一度だけ出すための記録。
	standbyQuarantineWarned map[string]bool
}
type jobWork struct {
	id string
}

type (
	retryableJobError      struct{ error }
	dependencyPendingError struct{ error }
)

const maxJobAttempts = 8

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
	m := &Manager{cfg: cfg, store: store, git: git, log: logger, started: started, prepareDetailDir: prepareDetailDir, lastReload: started, roots: map[string]bool{}, rootRefs: map[string]*managedRoot{}, retiredRefs: map[string][]*managedRoot{}, rootIdentities: map[string]string{}, rootIDs: map[string]string{}, leases: map[string]func(){}, jobs: make(chan jobWork, 256), lifecycleChecks: make(chan struct{}, 1), reloads: make(chan struct{}, 1), ctx: managerCtx, cancel: managerCancel}
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

func (m *Manager) beginRootClose() {
	// 新規のdescriptor取得を止め、貸出中のroot参照を解放する。
	// 進行中の操作は完了まで参照を保持し、closeRootHandles が待機する。
	m.mu.Lock()
	m.ensureRootStateLocked()
	m.rootClosing = true
	leases := make([]func(), 0, len(m.leases))
	for id, release := range m.leases {
		leases = append(leases, release)
		delete(m.leases, id)
	}
	m.mu.Unlock()
	for _, release := range leases {
		if release != nil {
			release()
		}
	}
}

func (m *Manager) closeRootHandles() {
	m.mu.Lock()
	m.ensureRootStateLocked()
	for m.rootReferenceCountLocked() > 0 {
		m.rootCond.Wait()
	}
	for path, entry := range m.rootRefs {
		if entry != nil && !entry.closed && entry.root != nil {
			entry.closed = true
			_ = entry.root.Close()
		}
		delete(m.rootRefs, path)
	}
	for path, entries := range m.retiredRefs {
		for _, entry := range entries {
			if entry != nil && !entry.closed && entry.root != nil {
				entry.closed = true
				_ = entry.root.Close()
			}
		}
		delete(m.retiredRefs, path)
	}
	m.mu.Unlock()
}

func (m *Manager) rootReferenceCountLocked() int {
	count := 0
	for _, entry := range m.rootRefs {
		if entry != nil && !entry.closed {
			count += entry.refs
		}
	}
	for _, entries := range m.retiredRefs {
		for _, entry := range entries {
			if entry != nil && !entry.closed {
				count += entry.refs
			}
		}
	}
	return count
}

func (m *Manager) rootHasReferencesLocked(path string) bool {
	if entry := m.rootRefs[path]; entry != nil && !entry.closed && entry.refs > 0 {
		return true
	}
	for _, entry := range m.retiredRefs[path] {
		if entry != nil && !entry.closed && entry.refs > 0 {
			return true
		}
	}
	return false
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

func (m *Manager) resizeWorkers(target int) {
	m.workersMu.Lock()
	defer m.workersMu.Unlock()
	if m.closed {
		return
	}
	for len(m.workerStops) < target {
		stop := make(chan struct{})
		workerID := m.workerSeq
		m.workerSeq++
		m.workerStops = append(m.workerStops, stop)
		m.wg.Add(1)
		go m.runWorker(workerID, stop)
	}
	for len(m.workerStops) > target {
		last := len(m.workerStops) - 1
		close(m.workerStops[last])
		m.workerStops = m.workerStops[:last]
	}
}

func (m *Manager) runWorker(workerID int, stop <-chan struct{}) {
	defer m.wg.Done()
	owner := fmt.Sprintf("%d:%d", os.Getpid(), workerID)
	for {
		select {
		case <-stop:
			return
		default:
		}
		var work jobWork
		select {
		case <-m.ctx.Done():
			return
		case <-stop:
			return
		case work = <-m.jobs:
		}
		job, err := m.store.ClaimJob(context.Background(), work.id, owner)
		if err != nil {
			m.log.Debug("claim job skipped", "job_id", work.id, "worker", owner, "error", err)
			continue
		}
		jobCtx, cancel := context.WithCancel(m.ctx)
		done := make(chan struct{})
		m.startBackground(func() {
			ticker := time.NewTicker(10 * time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-ticker.C:
					if err := m.store.RenewJob(context.Background(), job.ID, owner); err != nil {
						m.log.Error("renew job lease failed", "job_id", job.ID, "error", err)
						cancel()
						return
					}
				case <-done:
					return
				}
			}
		})
		err = m.runRecoveredJob(jobCtx, job)
		close(done)
		cancel()
		var pending dependencyPendingError
		if errors.As(err, &pending) {
			delay := 5 * time.Second
			if deferErr := m.store.DeferJob(context.Background(), work.id, owner, delay, "DEPENDENCY_PENDING"); deferErr != nil {
				m.log.Error("defer dependency-bound job failed", "job_id", work.id, "error", deferErr)
				m.releaseLease(job.SessionID)
			} else {
				m.scheduleDelayed(job, delay)
			}
			continue
		}
		var retryable retryableJobError
		if errors.As(err, &retryable) {
			m.log.Debug("job attempt failed and will be retried", "job_id", job.ID, "kind", job.Kind, "attempt", job.Attempt, "error", err)
			if job.Attempt >= maxJobAttempts {
				m.log.Error("job exhausted retry limit", "job_id", job.ID, "attempt", job.Attempt, "error", err)
				_ = m.store.SetSlotState(context.Background(), job.SlotID, []string{"PREPARING", "RESTORING", "FAILED", "REMOVING", "RETIRING"}, "QUARANTINED", "JOB_RETRY_EXHAUSTED")
				if finishErr := m.finishJob(context.Background(), work.id, owner, err); finishErr != nil {
					m.log.Error("finish exhausted job failed", "job_id", work.id, "error", finishErr)
				}
				m.releaseLease(job.SessionID)
				continue
			}
			delay := time.Duration(1<<min(job.Attempt, 6)) * time.Second
			if retryErr := m.store.RetryJob(context.Background(), work.id, owner, delay, "DEPENDENCY_PENDING"); retryErr != nil {
				m.log.Error("reschedule job failed", "job_id", work.id, "error", retryErr)
				m.releaseLease(job.SessionID)
			} else {
				m.scheduleDelayed(job, delay)
			}
			continue
		}
		if finishErr := m.finishJob(context.Background(), work.id, owner, err); finishErr != nil {
			m.log.Error("finish job failed", "job_id", work.id, "error", finishErr)
		}
		m.releaseLease(job.SessionID)
	}
}

func (m *Manager) Config() config.Config { m.mu.RLock(); defer m.mu.RUnlock(); return m.cfg }

func (m *Manager) newPreparer(cfg config.Config, slot state.Slot) *workspace.Preparer {
	// worktreeを再利用・削除する操作には必ずStoreによる所有権証明を渡す。
	slotPath := slot.Path
	if root, ok := m.rootForPath(slotPath); ok {
		cfg.Storage.WorktreeRoot = root
	}
	root, err := config.ExpandHome(cfg.Storage.WorktreeRoot)
	var ownedRoot *os.Root
	if err == nil {
		ownedRoot = m.rootHandleForPath(slotPath)
		if ownedRoot == nil && slotPath == "" {
			ownedRoot = m.rootHandleForPath(root)
		}
	}
	return &workspace.Preparer{
		Git: m.git, Config: cfg, Ownership: m.store, SlotPath: slotPath,
		DetailDir: m.prepareDetailDir,
		OwnedRoot: ownedRoot, RootPath: filepath.Clean(root),
		RootID: slot.RootID, SlotRelPath: slot.RelPath,
	}
}

func (m *Manager) newArchiveManager(cfg config.Config, slot state.Slot) archive.Manager {
	preparer := m.newPreparer(cfg, slot)
	return archive.Manager{Git: m.git, Preparer: preparer, Ownership: m.store}
}

func (m *Manager) enqueue(kind, workspaceID, slotID, sessionID string) error {
	job, err := m.store.CreateJob(context.Background(), kind, workspaceID, slotID, sessionID)
	if err != nil {
		return err
	}
	m.schedule(job)
	return nil
}

func (m *Manager) schedule(job state.Job) {
	select {
	case <-m.ctx.Done():
	case m.jobs <- jobWork{id: job.ID}:
	default:
	}
}

func (m *Manager) scheduleDelayed(job state.Job, delay time.Duration) {
	m.startBackground(func() {
		timer := time.NewTimer(delay)
		defer timer.Stop()
		select {
		case <-m.ctx.Done():
		case <-timer.C:
			m.schedule(job)
		}
	})
}

func (m *Manager) recoverJobs(reclaimAll bool) {
	jobs, err := m.store.RecoverJobs(context.Background(), reclaimAll)
	if err != nil {
		m.log.Error("recover jobs failed", "error", err)
		return
	}
	for _, job := range jobs {
		m.schedule(job)
	}
}

func (m *Manager) reconcileStandbyReplenishments(ctx context.Context) {
	jobs, err := m.store.RecoverStandbyReplenishments(ctx)
	if err != nil {
		m.log.Error("recover standby replenishment successes failed", "error", err)
		return
	}
	for _, job := range jobs {
		m.schedule(job)
	}
}

const lifecycleCheckInterval = time.Second

func (m *Manager) maintainJobs() {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	lifecycle := time.NewTimer(lifecycleCheckInterval)
	lifecycle.Stop()
	defer lifecycle.Stop()
	armed := false
	rearm := func() {
		if !armed && m.lifecyclePending() {
			lifecycle.Reset(lifecycleCheckInterval)
			armed = true
		}
	}
	for {
		select {
		case <-m.ctx.Done():
			return
		case <-m.lifecycleChecks:
			m.runPendingLifecycle()
			rearm()
		case <-lifecycle.C:
			armed = false
			m.runPendingLifecycle()
			rearm()
		case <-ticker.C:
			m.recoverJobs(false)
			m.detectExecutableReplacement()
			m.runPendingLifecycle()
			rearm()
		}
	}
}

func (m *Manager) maintainLifecycle() {
	m.resumeCleanRuns(m.ctx)
	m.reconcileStandbyReplenishments(m.ctx)
	m.reconcileRegistry(m.ctx)
	m.reconcileArtifacts(m.ctx)
	m.reconcileOrphans(m.ctx)
	m.maybeBackup(m.ctx)
	m.runBackgroundGC()
	for {
		interval := m.Config().Discovery.ReconcileInterval.Duration
		if interval <= 0 {
			interval = 10 * time.Minute
		}
		timer := time.NewTimer(interval)
		select {
		case <-m.ctx.Done():
			timer.Stop()
			return
		case <-m.reloads:
			timer.Stop()
			continue
		case <-timer.C:
			_ = m.reloadConfig(false)
			m.reconcileStandbyReplenishments(m.ctx)
			m.reconcileRegistry(m.ctx)
			m.reconcileArtifacts(m.ctx)
			m.reconcileOrphans(m.ctx)
			m.maybeBackup(m.ctx)
			m.runBackgroundGC()
		}
	}
}

// runBackgroundGC は自動 GC の保留・失敗をログへ残す。
// background 処理は呼出元へ返せないため、結果を捨てずに後から調査できる形にする。
func (m *Manager) runBackgroundGC() {
	result, err := m.GC(m.ctx, false)
	if err == nil && result.Pending == 0 && result.Failed == 0 {
		return
	}
	attrs := []any{
		"candidates", result.Candidates,
		"scheduled", result.Scheduled,
		"completed", result.Completed,
		"pending", result.Pending,
		"failed", result.Failed,
		"reasons", result.Reasons,
	}
	if err != nil {
		attrs = append(attrs, "error", err)
	}
	m.log.Error("automatic GC incomplete", attrs...)
}

func (m *Manager) reconcileArtifacts(ctx context.Context) {
	if jobs, err := m.store.EnsureRecoveryJobs(ctx); err != nil {
		m.log.Error("reconstruct crash recovery jobs failed", "error", err)
	} else {
		for _, job := range jobs {
			m.schedule(job)
		}
	}
	if artifacts, err := m.store.SlotArtifacts(ctx); err == nil {
		for _, artifact := range artifacts {
			if artifact.State == "ARCHIVED" || artifact.State == "REMOVING" || artifact.State == "PREPARING" {
				continue
			}
			exists, statErr := m.ownedPathExists(artifact.Path)
			if statErr != nil {
				m.log.Warn("skip artifact path reconciliation without root ownership proof", "slot_id", artifact.ID, "path", artifact.Path, "error", statErr)
				continue
			}
			if !exists {
				if err := m.store.QuarantineMissingSlot(ctx, artifact.ID, "OWNED_PATH_MISSING"); err != nil {
					m.log.Error("quarantine missing owned path failed", "slot_id", artifact.ID, "error", err)
				}
			}
		}
	}
	diagnostics := m.artifactDiagnostics(ctx)
	for _, category := range []string{"unknown_paths", "missing_paths", "unknown_refs", "missing_refs", "errors"} {
		items, _ := diagnostics[category].([]string)
		for _, item := range items {
			switch category {
			case "unknown_paths", "unknown_refs":
				_ = m.store.QuarantineArtifact(ctx, category, item, "ownership could not be proven during reconciliation")
			case "missing_refs":
				if _, ref, ok := strings.Cut(item, ":"); ok {
					_ = m.store.QuarantineMissingRecoveryRef(ctx, ref)
				}
			}
			m.log.Warn("startup artifact quarantined for manual inspection", "category", category, "artifact", item)
		}
	}
}

func (m *Manager) artifactDiagnostics(ctx context.Context) map[string]any {
	unknownPaths, missingPaths := []string{}, []string{}
	unknownRefs, missingRefs := []string{}, []string{}
	diagnosticErrors := []string{}
	artifacts, err := m.store.SlotArtifacts(ctx)
	if err != nil {
		return map[string]any{"errors": []string{err.Error()}}
	}
	expectedPaths := map[string]state.SlotArtifact{}
	for _, artifact := range artifacts {
		clean := filepath.Clean(artifact.Path)
		expectedPaths[clean] = artifact
		if artifact.State == "ARCHIVED" || artifact.State == "REMOVING" {
			continue
		}
		exists, statErr := m.ownedPathExists(clean)
		if statErr != nil {
			diagnosticErrors = append(diagnosticErrors, fmt.Sprintf("inspect slot %s: %v", artifact.ID, statErr))
		} else if !exists {
			missingPaths = append(missingPaths, fmt.Sprintf("%s (%s, %s)", clean, artifact.ID, artifact.State))
		}
	}
	roots, rootsErr := m.rootPathsFromStore(ctx)
	if rootsErr != nil {
		diagnosticErrors = append(diagnosticErrors, fmt.Sprintf("list worktree root generations: %v", rootsErr))
	}
	for _, root := range roots {
		paths, pathsErr := m.ownedRootArtifactPaths(root)
		if pathsErr != nil {
			diagnosticErrors = append(diagnosticErrors, fmt.Sprintf("inspect root %s: %v", root, pathsErr))
			continue
		}
		for _, path := range paths {
			clean := filepath.Clean(path)
			if _, exists := expectedPaths[clean]; !exists {
				unknownPaths = append(unknownPaths, clean)
			}
		}
	}
	repositories, err := m.store.Repositories(ctx)
	if err != nil {
		diagnosticErrors = append(diagnosticErrors, err.Error())
	} else {
		for _, repository := range repositories {
			expectedList, refsErr := m.store.RecoveryRefExpectations(ctx, string(repository.ID))
			if refsErr != nil {
				diagnosticErrors = append(diagnosticErrors, fmt.Sprintf("read recovery refs for %s: %v", repository.ID, refsErr))
				continue
			}
			expected := map[string]state.RecoveryRefExpectation{}
			for _, ref := range expectedList {
				expected[ref.Ref] = ref
			}
			listed, listErr := m.git.Run(ctx, string(repository.MainPath), "for-each-ref", "--format=%(refname) %(objectname)", "refs/wx/recovery")
			if listErr != nil {
				diagnosticErrors = append(diagnosticErrors, fmt.Sprintf("list recovery refs for %s: %v", repository.ID, listErr))
				continue
			}
			actual := map[string]bool{}
			for _, line := range strings.Split(strings.TrimSpace(listed.Stdout), "\n") {
				fields := strings.Fields(line)
				if len(fields) == 0 {
					continue
				}
				if len(fields) != 2 {
					diagnosticErrors = append(diagnosticErrors, fmt.Sprintf("parse recovery ref listing for %s: %q", repository.ID, line))
					continue
				}
				ref, oid := fields[0], fields[1]
				actual[ref] = true
				want, known := expected[ref]
				if !known || want.OID != oid {
					unknownRefs = append(unknownRefs, fmt.Sprintf("%s:%s", repository.ID, ref))
				}
			}
			for ref, expectation := range expected {
				if !actual[ref] && !expectation.InFlight {
					missingRefs = append(missingRefs, fmt.Sprintf("%s:%s", repository.ID, ref))
				}
			}
		}
	}
	for _, values := range [][]string{unknownPaths, missingPaths, unknownRefs, missingRefs, diagnosticErrors} {
		sort.Strings(values)
	}
	return map[string]any{
		"unknown_paths": unknownPaths, "missing_paths": missingPaths,
		"unknown_refs": unknownRefs, "missing_refs": missingRefs, "errors": diagnosticErrors,
	}
}

func (m *Manager) resolveRegisteredWorkspace(ctx context.Context, root string, discoverer *discovery.Discoverer) (discovery.Workspace, error) {
	workspaceRecord, err := discoverer.Resolve(ctx, root)
	if err == nil {
		return m.store.CanonicalWorkspace(ctx, workspaceRecord)
	}

	registered, lookupErr := m.store.WorkspaceByRoot(ctx, root)
	if lookupErr != nil {
		return discovery.Workspace{}, err
	}
	if registered.Kind != "repository" || len(registered.Repositories) != 1 {
		return discovery.Workspace{}, err
	}
	recovered, commonErr := discoverer.ResolveFromCommonDir(ctx, string(registered.Repositories[0].CommonDir))
	if commonErr != nil {
		return discovery.Workspace{}, fmt.Errorf("rediscover workspace root %s: %w; common-directory recovery failed: %w", root, err, commonErr)
	}
	if len(recovered.Repositories) != 1 || recovered.Repositories[0].CommonDir != registered.Repositories[0].CommonDir {
		return discovery.Workspace{}, fmt.Errorf("rediscover workspace root %s: %w; common-directory identity did not match", root, err)
	}
	return m.store.CanonicalWorkspace(ctx, recovered)
}

func (m *Manager) reconcileRegistry(ctx context.Context) {
	m.reconcileStandbyReplenishments(ctx)
	roots, err := m.store.WorkspaceRoots(ctx)
	if err != nil {
		m.log.Error("workspace registry reconcile failed", "error", err)
		return
	}
	discoverer := discovery.Discoverer{Git: m.git, Config: m.Config()}
	for _, root := range roots {
		workspaceRecord, err := m.resolveRegisteredWorkspace(ctx, root, &discoverer)
		if err != nil {
			m.log.Error("workspace rediscovery failed", "workspace_root", root, "error", err)
			continue
		}
		workspaceRecord, _, err = m.store.UpsertWorkspaceGeneration(ctx, workspaceRecord)
		if err != nil {
			m.log.Error("workspace registry update failed", "workspace_id", workspaceRecord.ID, "error", err)
			continue
		}
		resolved, err := pool.ResolveBranches(ctx, m.git, workspaceRecord, nil)
		if err != nil {
			m.log.Error("workspace base reconcile failed", "workspace_id", workspaceRecord.ID, "error", err)
			continue
		}
		readySlots, err := m.store.ReadySlots(ctx, string(workspaceRecord.ID))
		if err != nil {
			m.log.Error("READY registry read failed", "workspace_id", workspaceRecord.ID, "error", err)
			continue
		}
		for _, slot := range readySlots {
			valid, validationErr := m.readyMatches(ctx, slot, resolved)
			if validationErr != nil || !valid {
				_ = m.store.SetSlotState(ctx, slot.ID, []string{"READY"}, "STALE", "READY_RECONCILE_FAILED")
				m.log.Warn("READY slot failed startup reconciliation", "slot_id", slot.ID, "error", validationErr)
			}
		}
		if err := m.ensureStandby(ctx, workspaceRecord); err != nil {
			m.log.Error("workspace standby reconcile failed", "workspace_id", workspaceRecord.ID, "error", err)
		}
	}
}

func (m *Manager) maybeBackup(ctx context.Context) {
	m.mu.RLock()
	last := m.lastBackup
	cfg := m.cfg
	m.mu.RUnlock()
	if !last.IsZero() && time.Since(last) < 24*time.Hour {
		return
	}
	_, err := m.store.Backup(ctx, cfg.Storage.BackupGenerations, cfg.Storage.BackupRetention.Duration)
	m.mu.Lock()
	defer m.mu.Unlock()
	if err != nil {
		m.backupError = err.Error()
		m.log.Error("SQLite online backup failed", "error", err)
		return
	}
	m.lastBackup = time.Now()
	m.backupError = ""
}

func (m *Manager) reconcileOrphans(ctx context.Context) {
	candidates, err := m.store.OrphanCandidates(ctx, state.FormatTime(time.Now().Add(-45*time.Second)))
	if err != nil {
		m.log.Error("orphan reconciliation failed", "error", err)
		return
	}
	for _, candidate := range candidates {
		if processAlive(candidate.ClientPID) || processAlive(candidate.AgentPID) {
			continue
		}
		job, changed, quarantineExpired, err := m.store.ReleaseWithOutcome(ctx, candidate.ID, candidate.WorkspaceID, candidate.SlotID)
		if err != nil {
			m.log.Error("orphan release failed", "session_id", candidate.ID, "error", err)
			continue
		}
		if quarantineExpired {
			m.log.Warn("session expired without a recovery snapshot: slot is quarantined", "session_id", candidate.ID, "slot_id", candidate.SlotID)
		}
		if changed {
			m.schedule(job)
		} else {
			m.releaseLease(candidate.ID)
		}
	}
}

func (m *Manager) finishJob(ctx context.Context, id, owner string, runErr error) error {
	var prepareErr *workspace.PrepareCommandError
	if errors.As(runErr, &prepareErr) {
		failureCode := "PREPARE_FAILED"
		if prepareErr.FailureID != "" {
			failureCode += ":" + prepareErr.FailureID
		}
		return m.store.FinishJobWithDetail(ctx, id, owner, runErr, failureCode, prepareErr.DetailPath)
	}
	return m.store.FinishJob(ctx, id, owner, runErr)
}

func (m *Manager) Heartbeat(ctx context.Context, id, token string) error {
	return m.store.Heartbeat(ctx, id, token)
}

func (m *Manager) RegisterAgentProcess(ctx context.Context, id, token string, pid int) error {
	return m.store.RegisterAgentProcess(ctx, id, token, pid)
}

func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

func (m *Manager) runRecoveredJob(ctx context.Context, job state.Job) error {
	switch job.Kind {
	case "PREPARE":
		if job.Attempt > 1 {
			if err := m.store.ResetPreparationForRetry(ctx, job.SlotID); err != nil {
				return retryableJobError{err}
			}
		}
		w, err := m.store.Workspace(ctx, job.WorkspaceID)
		if err != nil {
			return err
		}
		repos, err := m.store.SlotRepositories(ctx, job.SlotID)
		if err != nil {
			return err
		}
		resolved, err := m.resolvedFromStored(ctx, w, repos)
		if err != nil {
			return err
		}
		if err := m.prepareSlotWithJob(ctx, job.SlotID, w, resolved, repos, job); err != nil {
			if errors.Is(err, state.ErrOwnership) {
				return err
			}
			var prepareErr *workspace.PrepareCommandError
			if errors.As(err, &prepareErr) {
				// prepare command は副作用を持つため、終了コードだけを根拠に再実行しない。
				return err
			}
			return retryableJobError{err}
		}
		return nil
	case "ENSURE_STANDBY":
		w, err := m.store.Workspace(ctx, job.WorkspaceID)
		if err != nil {
			return err
		}
		return m.ensureStandby(ctx, w)
	case "SNAPSHOT":
		s, err := m.store.SessionByID(ctx, job.SessionID)
		if err != nil {
			return err
		}
		return m.snapshotSession(ctx, s)
	case "RESTORE":
		return m.resumeRestoreJob(ctx, job.SessionID)
	case "REMOVE":
		if err := m.removeSlotJob(ctx, job); err != nil {
			if errors.Is(err, state.ErrOwnership) {
				return err
			}
			return retryableJobError{err}
		}
		return nil
	case "REMOVE_REPOSITORY":
		if err := m.removeColdRepositoryJob(ctx, job); err != nil {
			if errors.Is(err, state.ErrOwnership) {
				return err
			}
			return retryableJobError{err}
		}
		return nil
	default:
		return fmt.Errorf("unknown persistent job kind %s", job.Kind)
	}
}

func (m *Manager) resolvedFromStored(ctx context.Context, w discovery.Workspace, repos []state.SlotRepository) ([]pool.Resolved, error) {
	by := map[string]discovery.Repository{}
	for _, r := range w.Repositories {
		by[string(r.ID)] = r
	}
	out := make([]pool.Resolved, 0, len(repos))
	for _, sr := range repos {
		repo, ok := by[sr.RepositoryID]
		if !ok {
			return nil, fmt.Errorf("repository %s left workspace", sr.RepositoryID)
		}
		out = append(out, pool.Resolved{Repository: repo, RequestedRef: sr.RequestedRef, OID: sr.BaseOID})
	}
	return out, nil
}

func (m *Manager) resumeRestoreJob(ctx context.Context, sessionID string) error {
	s, err := m.store.SessionByID(ctx, sessionID)
	if err != nil {
		return err
	}
	if s.ParentSessionID == "" {
		return errors.New("restore session has no parent snapshot")
	}
	parent, err := m.store.SessionByID(ctx, s.ParentSessionID)
	if err != nil {
		return err
	}
	snapshots, err := m.store.Snapshots(ctx, s.ParentSessionID)
	if err != nil {
		return err
	}
	if parent.State == "RELEASING" || parent.State == "SNAPSHOTTING" {
		parentSlot, slotErr := m.store.Slot(ctx, parent.SlotID)
		if slotErr != nil {
			return slotErr
		}
		if parentSlot.State == "FAILED" || parentSlot.State == "QUARANTINED" {
			_ = m.store.SetSlotState(ctx, s.SlotID, []string{"RESTORING"}, "QUARANTINED", "SNAPSHOT_UNAVAILABLE")
			return fmt.Errorf("parent snapshot job failed with slot state %s", parentSlot.State)
		}
		return dependencyPendingError{errors.New("parent snapshot is still being archived")}
	}
	if !snapshotsUsable(snapshots, time.Now()) {
		_ = m.store.SetSlotState(ctx, s.SlotID, []string{"RESTORING"}, "QUARANTINED", "SNAPSHOT_UNAVAILABLE")
		return errors.New("parent recovery snapshot is expired or incomplete")
	}
	w, err := m.store.SessionWorkspace(ctx, s.ParentSessionID)
	if err != nil {
		return err
	}
	usable, err := m.recoveryUsable(ctx, s.ParentSessionID, w, snapshots, time.Now())
	if err != nil || !usable {
		_ = m.store.SetSlotState(ctx, s.SlotID, []string{"RESTORING"}, "QUARANTINED", "SNAPSHOT_UNAVAILABLE")
		if err != nil {
			return fmt.Errorf("validate parent recovery snapshot: %w", err)
		}
		return errors.New("parent recovery snapshot is expired or incomplete")
	}
	repos, err := m.store.SlotRepositories(ctx, s.SlotID)
	if err != nil {
		return err
	}
	by := map[string]state.Snapshot{}
	for _, snapshot := range snapshots {
		by[snapshot.RepositoryID] = snapshot
	}
	if len(repos) == 0 {
		resolved := make([]pool.Resolved, 0, len(w.Repositories))
		for _, repo := range w.Repositories {
			snapshot, ok := by[string(repo.ID)]
			if !ok {
				_ = m.store.SetSlotState(ctx, s.SlotID, []string{"RESTORING"}, "QUARANTINED", "SNAPSHOT_INCOMPLETE")
				return fmt.Errorf("snapshot missing repository %s", repo.RelativePath)
			}
			resolved = append(resolved, pool.Resolved{Repository: repo, RequestedRef: snapshot.HeadRef, OID: snapshot.HeadOID})
		}
		slot, err := m.store.Slot(ctx, s.SlotID)
		if err != nil {
			return err
		}
		repos, err = m.slotRepos(slot.Path, w, resolved, slot.Generation, nil)
		if err != nil {
			return err
		}
		for i := range repos {
			repos[i].State = "RESTORING"
		}
		if err := m.store.AddRestoringRepositories(ctx, s.SlotID, repos); err != nil {
			return err
		}
	}
	resolved, err := m.resolvedFromStored(ctx, w, repos)
	if err != nil {
		return err
	}
	return m.restoreSlot(ctx, s.SlotID, w, resolved, repos, by)
}

func (m *Manager) ResolveAndLease(ctx context.Context, cwd string, branches []string, agent string, pid int) (Lease, error) {
	discoverer := discovery.Discoverer{Git: m.git, Config: m.Config()}
	w, err := discoverer.Resolve(ctx, cwd)
	if err != nil {
		return Lease{}, err
	}
	return m.leaseWorkspace(ctx, w, branches, agent, pid, false)
}

func (m *Manager) leaseWorkspace(ctx context.Context, w discovery.Workspace, branches []string, agent string, pid int, cold bool) (Lease, error) {
	var err error
	w, err = m.store.CanonicalWorkspace(ctx, w)
	if err != nil {
		return Lease{}, err
	}
	w, generation, err := m.store.UpsertWorkspaceGeneration(ctx, w)
	if err != nil {
		return Lease{}, err
	}
	resolved, err := pool.ResolveBranches(ctx, m.git, w, branches)
	if err != nil {
		return Lease{}, err
	}
	for attempts := 0; !cold && attempts < m.Config().Pool.WarmPerWorkspace+1; attempts++ {
		ready, ok, err := m.store.ReadySlot(ctx, string(w.ID))
		if err != nil {
			return Lease{}, err
		}
		if !ok {
			break
		}
		lease, leased, leaseErr := func() (Lease, bool, error) {
			releaseRoot, holdErr := m.holdRootForPath(ready.Path)
			if holdErr != nil {
				m.quarantineOwnershipFailure(ready.ID, []string{"READY"}, holdErr)
				return Lease{}, false, holdErr
			}
			defer releaseRoot()
			valid, matchErr := m.readyMatches(ctx, ready, resolved)
			if matchErr != nil {
				// 併走するleaseがREADYを奪った場合は所有権自体は疑わしくないため、隔離せず次の候補へ回す。
				if errors.Is(matchErr, state.ErrOwnership) && !errors.Is(matchErr, state.ErrSlotStateIneligible) {
					m.quarantineOwnershipFailure(ready.ID, []string{"READY"}, matchErr)
				}
				return Lease{}, false, matchErr
			}
			if !valid {
				return Lease{}, false, nil
			}
			repositories, repositoryErr := m.store.SlotRepositories(ctx, ready.ID)
			if repositoryErr != nil {
				return Lease{}, false, repositoryErr
			}
			leasePathValue := leasePath(ready.Path, w.Kind, repositories)
			rootIdentity, identityErr := m.ensureLeaseRoot(ready.Path, leasePathValue)
			if identityErr != nil {
				_ = m.store.SetSlotState(context.Background(), ready.ID, []string{"READY"}, "QUARANTINED", "LEASE_ROOT_OWNERSHIP_UNCERTAIN")
				return Lease{}, false, fmt.Errorf("pin ready lease root: %w", identityErr)
			}
			token, tokenErr := state.TokenHex()
			if tokenErr != nil {
				return Lease{}, false, tokenErr
			}
			hasCold := false
			for _, repository := range repositories {
				hasCold = hasCold || repository.State == "COLD"
			}
			sessionState := "ACTIVE"
			if hasCold {
				sessionState = "STARTING"
			}
			session := state.Session{ID: ready.ID, WorkspaceID: string(w.ID), SlotID: ready.ID, State: sessionState, AgentKind: agent, ClientPID: pid, TokenHash: state.HashToken(token)}
			if retainErr := m.retainLease(session.ID, leasePathValue); retainErr != nil {
				return Lease{}, false, retainErr
			}
			if hasCold {
				job, leaseErr := m.store.LeaseReadyWithCold(ctx, ready.ID, session)
				if leaseErr == nil {
					m.schedule(job)
					return Lease{SessionID: session.ID, Token: token, Path: leasePathValue, RootIdentity: rootIdentity, SourceWorkspace: string(w.Root), Ready: false}, true, nil
				}
				m.releaseLease(session.ID)
				return Lease{}, false, nil
			}
			if replenishJob, replenished, leaseErr := m.store.LeaseReadyWithReplenishment(ctx, ready.ID, session); leaseErr == nil {
				m.handleNormalSessionSuccess(ctx, w, replenishJob, replenished)
				return Lease{SessionID: session.ID, Token: token, Path: leasePathValue, RootIdentity: rootIdentity, SourceWorkspace: string(w.Root), Ready: true}, true, nil
			}
			m.releaseLease(session.ID)
			return Lease{}, false, nil
		}()
		if leaseErr != nil {
			// READY取得後に状態が変わっただけならslotをSTALEにせず、残るREADYまたはcold allocateで応答する。
			if errors.Is(leaseErr, state.ErrSlotStateIneligible) {
				continue
			}
			return Lease{}, leaseErr
		}
		if leased {
			return lease, nil
		}
		if len(branches) > 0 {
			break
		}
		_ = m.store.SetSlotState(ctx, ready.ID, []string{"READY"}, "STALE", "READY_VALIDATION_FAILED")
	}
	return m.allocate(ctx, w, resolved, generation, agent, pid, "STARTING", "")
}

func (m *Manager) allocate(ctx context.Context, w discovery.Workspace, resolved []pool.Resolved, generation int, agent string, pid int, sessionState, parent string) (Lease, error) {
	rootPath, rootID, err := m.activeRoot()
	if err != nil {
		return Lease{}, err
	}
	token, err := state.TokenHex()
	if err != nil {
		return Lease{}, err
	}
	slotState := "PREPARING"
	jobKind := "PREPARE"
	if sessionState == "RESTORING" {
		slotState = "RESTORING"
		jobKind = "RESTORE"
	}
	var lastErr error
	for range idAllocationAttempts {
		id, idErr := newSlotID()
		if idErr != nil {
			return Lease{}, idErr
		}
		lease, retry, allocErr := m.allocateWithID(ctx, id, rootPath, rootID, token, w, resolved, generation, agent, pid, sessionState, slotState, jobKind, parent)
		if allocErr == nil {
			return lease, nil
		}
		if !retry {
			return Lease{}, allocErr
		}
		lastErr = allocErr
	}
	return Lease{}, fmt.Errorf("allocate slot: %w", lastErr)
}

const idAllocationAttempts = 10

func (m *Manager) allocateWithID(ctx context.Context, id, rootPath, rootID, token string, w discovery.Workspace, resolved []pool.Resolved, generation int, agent string, pid int, sessionState, slotState, jobKind, parent string) (Lease, bool, error) {
	relPath, err := slotRelPath(string(w.ID), id, false)
	if err != nil {
		return Lease{}, false, err
	}
	slotPath := filepath.Join(rootPath, relPath)
	releaseRoot, err := m.holdRootForPath(slotPath)
	if err != nil {
		return Lease{}, false, err
	}
	defer releaseRoot()
	repos, err := m.slotRepos(slotPath, w, resolved, generation, nil)
	if err != nil {
		return Lease{}, false, err
	}
	if sessionState == "RESTORING" {
		for i := range repos {
			repos[i].State = "RESTORING"
		}
	}
	leasePathValue := leasePath(slotPath, w.Kind, repos)
	slotIdentity, leaseIdentity, err := m.createSlotRoot(slotPath, leasePathValue)
	if err != nil {
		return Lease{}, false, err
	}
	session := state.Session{ID: id, WorkspaceID: string(w.ID), SlotID: id, ParentSessionID: parent, State: sessionState, AgentKind: agent, ClientPID: pid, TokenHash: state.HashToken(token)}
	if err := m.retainLease(id, leasePathValue); err != nil {
		return Lease{}, false, err
	}
	job, err := m.store.CreateSlotSession(ctx, state.Slot{ID: id, WorkspaceID: string(w.ID), Generation: generation, RootID: rootID, RelPath: relPath, DirIdentity: slotIdentity, State: slotState}, repos, session, jobKind)
	if err != nil {
		m.releaseLease(id)
		return Lease{}, state.IsIDCollision(err), err
	}
	m.schedule(job)
	m.startBackground(m.runBackgroundGC)
	return Lease{SessionID: id, Token: token, Path: leasePathValue, RootIdentity: leaseIdentity, SourceWorkspace: string(w.Root), Ready: false}, false, nil
}

// workspace未確定slot用の予約namespaceであり、通常のworkspace IDには"_"接頭辞を許さない。
const unboundNamespace = "_unbound"

func slotRelPath(workspaceID, slotID string, unbound bool) (string, error) {
	// 作成側とorphan scanの列挙側で共有するroot相対layoutを生成する。
	if err := validateLayoutComponent("slot id", slotID); err != nil {
		return "", err
	}
	if unbound {
		return filepath.Join(unboundNamespace, slotID), nil
	}
	if err := validateLayoutComponent("workspace id", workspaceID); err != nil {
		return "", err
	}
	return filepath.Join(workspaceID, slotID), nil
}

func validateLayoutComponent(kind, value string) error {
	// root外への脱出とwxの予約namespaceとの衝突を拒否する。
	if value == "" || value == "." || value == ".." || strings.ContainsAny(value, `/\`) || strings.HasPrefix(value, "_") {
		return fmt.Errorf("%w: %s %q cannot be a wx layout path component", state.ErrOwnership, kind, value)
	}
	return nil
}

func leasePath(slotPath, kind string, repos []state.SlotRepository) string {
	// 単一repositoryだけはagentのCWDをworktreeにし、slotの所有権markerを親に残す。
	if kind == "repository" && len(repos) == 1 && repos[0].DirName != "" {
		return filepath.Join(slotPath, repos[0].DirName)
	}
	return slotPath
}

var errManagerClosed = errors.New("daemon manager is closed")

func (m *Manager) registerRootGeneration(ctx context.Context, root, identity string) {
	// root IDなしのslotは再発見できないため、失敗時は後続のallocationをfail closedにする。
	if identity == "" {
		m.log.Error("worktree root generation cannot be registered without an identity", "path", root)
		m.setRootError(fmt.Sprintf("worktree root %s has no readable inode identity", root))
		return
	}
	id, err := m.store.EnsureActiveRoot(ctx, root, identity)
	if err != nil {
		m.log.Error("register worktree root generation failed", "path", root, "error", err)
		m.setRootError(err.Error())
		return
	}
	m.mu.Lock()
	m.ensureRootStateLocked()
	m.rootIDs[root] = id
	m.rootError = ""
	m.mu.Unlock()
}

func (m *Manager) setRootError(message string) {
	m.mu.Lock()
	m.rootError = message
	m.mu.Unlock()
}

func (m *Manager) loadRootGenerations(ctx context.Context) {
	// 旧rootのslotも寿命まで扱えるよう、SQLiteが参照するgenerationをretired rootとしてpinする。
	roots, err := m.store.Roots(ctx)
	if err != nil {
		m.log.Error("load worktree root generations failed", "error", err)
		return
	}
	for _, root := range roots {
		if root.Identity == "" {
			m.log.Error("worktree root generation has no recorded identity", "path", root.Path)
			continue
		}
		m.mu.Lock()
		m.ensureRootStateLocked()
		pinned := m.rootIdentities[root.Path]
		if pinned == "" {
			m.rootIdentities[root.Path] = root.Identity
		}
		m.mu.Unlock()
		if pinned != "" {
			if pinned != root.Identity {
				m.log.Error("worktree root generation is not the pinned directory", "path", root.Path, "recorded", root.Identity, "pinned", pinned)
				continue
			}
			m.mu.Lock()
			m.rootIDs[root.Path] = root.ID
			m.mu.Unlock()
			continue
		}
		_, release, openErr := m.existingRootDescriptor(root.Path)
		if openErr != nil {
			m.log.Warn("retired worktree root generation is unavailable", "path", root.Path, "error", openErr)
			continue
		}
		release()
		m.mu.Lock()
		m.rootIDs[root.Path] = root.ID
		m.mu.Unlock()
	}
}

func (m *Manager) activeRoot() (string, string, error) {
	root, err := config.ExpandHome(m.Config().Storage.WorktreeRoot)
	if err != nil {
		return "", "", err
	}
	root = filepath.Clean(root)
	m.mu.RLock()
	id := m.rootIDs[root]
	rootError := m.rootError
	m.mu.RUnlock()
	if id == "" {
		if rootError != "" {
			return "", "", fmt.Errorf("%w: worktree root %s has no registered generation: %s", state.ErrOwnership, root, rootError)
		}
		return "", "", fmt.Errorf("%w: worktree root %s has no registered generation", state.ErrOwnership, root)
	}
	return root, id, nil
}

func (m *Manager) rootIDForPath(path string) (string, string, error) {
	// 遅延jobとrecoveryは設定変更後の旧rootにも属し得るため、active rootだけに絞らない。
	root, ok := m.rootForPath(path)
	if !ok {
		return "", "", fmt.Errorf("%w: path is outside known wx roots", state.ErrOwnership)
	}
	m.mu.RLock()
	id := m.rootIDs[root]
	m.mu.RUnlock()
	if id == "" {
		return "", "", fmt.Errorf("%w: worktree root %s has no registered generation", state.ErrOwnership, root)
	}
	return root, id, nil
}

func (m *Manager) ensureRootStateLocked() {
	if m.rootRefs == nil {
		m.rootRefs = map[string]*managedRoot{}
	}
	if m.retiredRefs == nil {
		m.retiredRefs = map[string][]*managedRoot{}
	}
	if m.rootIdentities == nil {
		m.rootIdentities = map[string]string{}
	}
	if m.rootIDs == nil {
		m.rootIDs = map[string]string{}
	}
	if m.roots == nil {
		m.roots = map[string]bool{}
	}
	if m.leases == nil {
		m.leases = map[string]func(){}
	}
	if m.rootCond == nil {
		m.rootCond = sync.NewCond(&m.mu)
	}
}

func (m *Manager) acquireRootLocked(root string, includeRetired bool) (*os.Root, *managedRoot, bool, error) {
	m.ensureRootStateLocked()
	if m.rootClosing {
		return nil, nil, false, errManagerClosed
	}
	if entry := m.rootRefs[root]; entry != nil && !entry.closed && !entry.retired && entry.root != nil {
		entry.refs++
		return entry.root, entry, true, nil
	}
	if includeRetired {
		retired := m.retiredRefs[root]
		for index := len(retired) - 1; index >= 0; index-- {
			entry := retired[index]
			if entry != nil && !entry.closed && entry.root != nil {
				entry.refs++
				return entry.root, entry, true, nil
			}
		}
	}
	return nil, nil, false, nil
}

func rootReleaseOnce(m *Manager, path string, entry *managedRoot) func() {
	var once sync.Once
	return func() {
		once.Do(func() { m.releaseRoot(path, entry) })
	}
}

func (m *Manager) releaseRoot(path string, entry *managedRoot) {
	if entry == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if entry.closed || entry.refs <= 0 {
		return
	}
	entry.refs--
	if entry.refs == 0 && (entry.retired || m.rootClosing) {
		m.closeRootLocked(path, entry)
	}
	if m.rootCond != nil {
		m.rootCond.Broadcast()
	}
}

func (m *Manager) closeRootLocked(path string, entry *managedRoot) {
	if entry == nil || entry.closed {
		return
	}
	entry.closed = true
	if m.rootRefs[path] == entry {
		delete(m.rootRefs, path)
	}
	if entry.root != nil {
		_ = entry.root.Close()
	}
	if retired := m.retiredRefs[path]; len(retired) > 0 {
		kept := retired[:0]
		for _, candidate := range retired {
			if candidate != entry {
				kept = append(kept, candidate)
			}
		}
		if len(kept) == 0 {
			delete(m.retiredRefs, path)
		} else {
			m.retiredRefs[path] = kept
		}
	}
	if !m.roots[path] {
		delete(m.roots, path)
	}
}

func (m *Manager) adoptRoot(path string, opened *os.Root, active bool) (*os.Root, func(), error) {
	// 競合で既存descriptorが勝った場合とshutdown開始後は、開いたdescriptorを必ず閉じる。
	path = filepath.Clean(path)
	identity, err := descriptorIdentity(opened)
	if err != nil {
		_ = opened.Close()
		return nil, func() {}, fmt.Errorf("inspect opened worktree root: %w", err)
	}
	m.mu.Lock()
	m.ensureRootStateLocked()
	if m.rootClosing {
		m.mu.Unlock()
		_ = opened.Close()
		return nil, func() {}, errManagerClosed
	}
	if expected := m.rootIdentities[path]; expected != "" && expected != identity {
		m.mu.Unlock()
		_ = opened.Close()
		return nil, func() {}, fmt.Errorf("worktree root inode changed for %s (expected %s, got %s)", path, expected, identity)
	}
	if existing := m.rootRefs[path]; existing != nil && !existing.closed && !existing.retired && existing.root != nil {
		existing.refs++
		m.mu.Unlock()
		_ = opened.Close()
		return existing.root, rootReleaseOnce(m, path, existing), nil
	}
	current, known := m.roots[path]
	isActive := active && (!known || current)
	entry := &managedRoot{root: opened, identity: identity, refs: 1, retired: !isActive}
	m.rootIdentities[path] = identity
	if isActive {
		m.rootRefs[path] = entry
		m.roots[path] = true
	} else {
		m.retiredRefs[path] = append(m.retiredRefs[path], entry)
	}
	m.mu.Unlock()
	return opened, rootReleaseOnce(m, path, entry), nil
}

func (m *Manager) rootDescriptor(root string) (*os.Root, func(), error) {
	root = filepath.Clean(root)
	m.mu.Lock()
	ownedRoot, entry, found, err := m.acquireRootLocked(root, false)
	m.mu.Unlock()
	if err != nil {
		return nil, func() {}, err
	}
	if found {
		return ownedRoot, rootReleaseOnce(m, root, entry), nil
	}
	_, opened, err := ensureWorktreeRootDescriptor(root)
	if err != nil {
		return nil, func() {}, err
	}
	return m.adoptRoot(root, opened, true)
}

func (m *Manager) existingRootDescriptor(root string) (*os.Root, func(), error) {
	// READY検証やreconcileでは、欠けたrootを作り直して所有権を偽装してはならない。
	root = filepath.Clean(root)
	m.mu.Lock()
	ownedRoot, entry, found, err := m.acquireRootLocked(root, true)
	m.mu.Unlock()
	if err != nil {
		return nil, func() {}, err
	}
	if found {
		return ownedRoot, rootReleaseOnce(m, root, entry), nil
	}
	ownedRoot, _, err = domain.OpenOwnedRoot(root, root)
	if err != nil {
		return nil, func() {}, err
	}
	return m.adoptRoot(root, ownedRoot, false)
}

func descriptorIdentity(root *os.Root) (string, error) {
	if root == nil {
		return "", errors.New("worktree root descriptor is nil")
	}
	info, err := root.Lstat(".")
	if err != nil {
		return "", err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return "", errors.New("worktree root descriptor is not a physical directory")
	}
	return domain.FileIdentity(info)
}

func (m *Manager) retireRootLocked(path string) {
	entry := m.rootRefs[path]
	if entry == nil {
		delete(m.roots, path)
		return
	}
	if entry.closed {
		return
	}
	delete(m.rootRefs, path)
	entry.retired = true
	if entry.refs == 0 {
		m.closeRootLocked(path, entry)
		return
	}
	m.retiredRefs[path] = append(m.retiredRefs[path], entry)
}

func (m *Manager) createSlotRoot(slotPath, leasePathValue string) (string, string, error) {
	// allocationはmanagerがpinしたroot descriptorで行い、返すinode identityでclient側の置換検出を可能にする。
	root, err := config.ExpandHome(m.Config().Storage.WorktreeRoot)
	if err != nil {
		return "", "", err
	}
	root = filepath.Clean(root)
	if !domain.IsWithin(root, slotPath) || !domain.IsWithin(root, leasePathValue) {
		return "", "", fmt.Errorf("slot path %s is outside wx worktree root", slotPath)
	}
	owner, closeOwner, err := m.rootDescriptor(root)
	if err != nil {
		return "", "", err
	}
	defer closeOwner()
	if err := verifyRootDescriptorPath(root, owner); err != nil {
		return "", "", err
	}
	relativeSlot, ok := relativeWithinRoot(root, slotPath)
	if !ok {
		return "", "", errors.New("slot path is outside wx worktree root")
	}
	relativeLease, ok := relativeWithinRoot(root, leasePathValue)
	if !ok {
		return "", "", errors.New("lease path is outside wx worktree root")
	}
	m.mu.RLock()
	barrier := m.beforeSlotRootCreate
	m.mu.RUnlock()
	if barrier != nil {
		barrier()
	}
	if err := owner.MkdirAll(relativeLease, 0o700); err != nil {
		return "", "", fmt.Errorf("create slot root safely: %w", err)
	}
	slotIdentity, err := directoryIdentityAt(owner, relativeSlot)
	if err != nil {
		return "", "", fmt.Errorf("open allocated slot root: %w", err)
	}
	if relativeLease == relativeSlot {
		return slotIdentity, slotIdentity, nil
	}
	leaseIdentity, err := directoryIdentityAt(owner, relativeLease)
	if err != nil {
		return "", "", fmt.Errorf("open allocated lease root: %w", err)
	}
	return slotIdentity, leaseIdentity, nil
}

func directoryIdentityAt(owner *os.Root, relative string) (string, error) {
	directory, identity, err := domain.OpenDirectoryAt(owner, relative)
	if err != nil {
		return "", err
	}
	if err := directory.Close(); err != nil {
		return "", err
	}
	return identity, nil
}

func (m *Manager) ensureLeaseRoot(slotPath, leasePathValue string) (string, error) {
	// COLD eviction後もclientが直ちにCWDを開けるよう、既存root descriptor経由でlease directoryを復元する。
	if leasePathValue == slotPath {
		return m.ownedDirectoryIdentity(slotPath)
	}
	root, ok := m.rootForPath(slotPath)
	if !ok {
		return "", fmt.Errorf("%w: lease path %s has no registered worktree root", state.ErrOwnership, leasePathValue)
	}
	root = filepath.Clean(root)
	if !domain.IsWithin(root, leasePathValue) {
		return "", fmt.Errorf("lease path %s is outside wx worktree root", leasePathValue)
	}
	owner, closeOwner, err := m.existingRootDescriptor(root)
	if err != nil {
		return "", err
	}
	defer closeOwner()
	if err := verifyRootDescriptorPath(root, owner); err != nil {
		return "", err
	}
	relative, ok := relativeWithinRoot(root, leasePathValue)
	if !ok || relative == "." {
		return "", fmt.Errorf("%w: lease path is outside wx worktree root", state.ErrOwnership)
	}
	if err := owner.MkdirAll(relative, 0o700); err != nil {
		return "", fmt.Errorf("create lease root safely: %w", err)
	}
	return directoryIdentityAt(owner, relative)
}

func (m *Manager) ownedDirectoryIdentity(path string) (string, error) {
	// path名ではなく所有root descriptorからinode identityを取得し、置換をfail closedにする。
	root, ok := m.rootForPath(path)
	if !ok {
		var err error
		root, err = config.ExpandHome(m.Config().Storage.WorktreeRoot)
		if err != nil {
			return "", err
		}
	}
	root = filepath.Clean(root)
	if !domain.IsWithin(root, path) {
		return "", fmt.Errorf("lease path %s is outside wx worktree root", path)
	}
	owner, closeOwner, err := m.existingRootDescriptor(root)
	if err != nil {
		return "", err
	}
	defer closeOwner()
	relative, err := filepath.Rel(root, filepath.Clean(path))
	if err != nil {
		return "", err
	}
	directory, identity, err := domain.OpenDirectoryAt(owner, relative)
	if err != nil {
		return "", fmt.Errorf("open lease root: %w", err)
	}
	if err := directory.Close(); err != nil {
		return "", err
	}
	return identity, nil
}

func (m *Manager) slotRepos(slotPath string, w discovery.Workspace, resolved []pool.Resolved, generation int, hot map[string]bool) ([]state.SlotRepository, error) {
	// hotにないrepositoryはCOLDとして記録し、実際のlease時までcheckoutを遅らせる。
	cfg := m.Config()
	out := make([]state.SlotRepository, 0, len(resolved))
	taken := map[string]bool{}
	for _, r := range resolved {
		dirName := workspace.UniqueDirName(workspace.RepositoryDirName(r.Repository, cfg), taken)
		if err := validateLayoutComponent("repository directory", dirName); err != nil {
			return nil, err
		}
		fp, err := workspace.Fingerprint(generation, r.OID, r.Repository, cfg)
		if err != nil {
			return nil, err
		}
		repoState := "PREPARING"
		if hot != nil && !hot[string(r.Repository.ID)] {
			repoState = "COLD"
		}
		out = append(out, state.SlotRepository{RepositoryID: string(r.Repository.ID), DirName: dirName, WorktreePath: filepath.Join(slotPath, dirName), State: repoState, RequestedRef: r.RequestedRef, BaseOID: r.OID, Fingerprint: fp})
	}
	return out, nil
}

func newSlotID() (string, error) { return domain.NewShortID() }

func (m *Manager) prepareSlot(ctx context.Context, id string, w discovery.Workspace, resolved []pool.Resolved, repos []state.SlotRepository) error {
	return m.prepareSlotWithJob(ctx, id, w, resolved, repos, state.Job{})
}

func (m *Manager) prepareSlotWithJob(ctx context.Context, id string, w discovery.Workspace, resolved []pool.Resolved, repos []state.SlotRepository, job state.Job) error {
	slot, err := m.store.Slot(ctx, id)
	if err != nil {
		return err
	}
	if slot.State == "READY" || slot.State == "LEASED" {
		return nil
	}
	if slot.State != "PREPARING" && slot.State != "RESTORING" {
		return fmt.Errorf("slot %s cannot be prepared from %s", id, slot.State)
	}
	releaseRoot, err := m.holdRootForPath(slot.Path)
	if err != nil {
		m.quarantineOwnershipFailure(id, []string{"PREPARING", "RESTORING"}, err)
		return err
	}
	defer releaseRoot()
	preparer := m.newPreparer(m.Config(), slot)
	if len(repos) != len(resolved) {
		return errors.New("slot repository metadata does not match resolved workspace")
	}
	for _, r := range resolved {
		stored, err := m.store.SlotRepository(ctx, id, string(r.Repository.ID))
		if err != nil {
			return err
		}
		if stored.State == "COLD" {
			continue
		}
		if stored.State == "READY" {
			if err := preparer.ValidateSlotWorktreeOwnership(ctx, r.Repository, stored.WorktreePath, r.OID, id); err != nil {
				m.quarantineOwnershipFailure(id, []string{"PREPARING", "RESTORING"}, err)
				return err
			}
			continue
		}
		if stored.State == "PREPARE_RUNNING" {
			if override := m.Config().Repositories[string(r.Repository.MainPath)]; len(override.Prepare.Command) > 0 {
				err := errors.New("prepare command completion is ambiguous after interruption")
				_ = m.store.SetSlotState(ctx, id, []string{"PREPARING", "RESTORING"}, "QUARANTINED", "PREPARE_AMBIGUOUS")
				return err
			}
		} else if err := m.store.SetSlotRepositoryState(ctx, id, string(r.Repository.ID), []string{"PREPARING", "RESTORING"}, "PREPARE_RUNNING"); err != nil {
			return err
		}
		if err := preparer.Prepare(ctx, r.Repository, stored.WorktreePath, r.OID, id); err != nil {
			m.log.Error("slot preparation failed", "job_id", job.ID, "session_id", job.SessionID, "slot_id", id, "repository_id", r.Repository.ID, "error", err)
			if errors.Is(err, state.ErrOwnership) {
				_ = m.store.SetSlotState(context.Background(), id, []string{"PREPARING", "RESTORING"}, "QUARANTINED", "WORKTREE_OWNERSHIP_UNCERTAIN")
			} else {
				failureCode, detailPath := "PREPARE_FAILED", ""
				var prepareErr *workspace.PrepareCommandError
				if errors.As(err, &prepareErr) {
					detailPath = prepareErr.DetailPath
					if prepareErr.FailureID != "" {
						failureCode += ":" + prepareErr.FailureID
					}
				}
				_ = m.store.SetSlotStateWithDetail(ctx, id, []string{"PREPARING", "RESTORING"}, "FAILED", failureCode, detailPath)
			}
			return err
		}
		identity, identityErr := preparer.WorktreeIdentity(stored.WorktreePath)
		if identityErr != nil {
			m.quarantineOwnershipFailure(id, []string{"PREPARING", "RESTORING"}, fmt.Errorf("%w: capture prepared worktree identity: %w", state.ErrOwnership, identityErr))
			return identityErr
		}
		if err := m.store.RecordSlotRepositoryIdentity(ctx, id, string(r.Repository.ID), identity); err != nil {
			return err
		}
		if err := m.store.SetSlotRepositoryState(ctx, id, string(r.Repository.ID), []string{"PREPARE_RUNNING"}, "READY"); err != nil {
			return err
		}
	}
	if w.Kind == "multi_repository" {
		slot, err := m.store.Slot(ctx, id)
		if err != nil {
			return err
		}
		dirIdentity, identityErr := m.ownedDirectoryIdentity(slot.Path)
		if identityErr != nil {
			m.quarantineOwnershipFailure(id, []string{"PREPARING", "RESTORING"}, fmt.Errorf("%w: read slot directory identity: %w", state.ErrOwnership, identityErr))
			return identityErr
		}
		if err := m.store.ValidateSlotOwnership(context.Background(), state.SlotOwnershipRequest{SlotID: id, WorkspaceID: slot.WorkspaceID, RootID: slot.RootID, RelPath: slot.RelPath, DirIdentity: dirIdentity, AllowedSlotStates: []string{"PREPARING", "RESTORING"}}); err != nil {
			if errors.Is(err, state.ErrOwnership) {
				_ = m.store.SetSlotState(context.Background(), id, []string{"PREPARING", "RESTORING"}, "QUARANTINED", "WORKTREE_OWNERSHIP_UNCERTAIN")
			}
			return err
		}
		if err := m.materializeWorkspaceRoot(string(w.Root), slot.Path, m.Config().Workspaces[string(w.Root)]); err != nil {
			m.log.Error("workspace root materialization failed", "slot_id", id, "error", err)
			if errors.Is(err, state.ErrOwnership) {
				_ = m.store.SetSlotState(context.Background(), id, []string{"PREPARING", "RESTORING"}, "QUARANTINED", "WORKTREE_OWNERSHIP_UNCERTAIN")
			} else {
				_ = m.store.SetSlotState(ctx, id, []string{"PREPARING", "RESTORING"}, "FAILED", "ROOT_MATERIALIZATION_FAILED")
			}
			return err
		}
	}
	normalPreparation := false
	if slot.OwnerSessionID != "" {
		if owner, ownerErr := m.store.SessionByID(ctx, slot.OwnerSessionID); ownerErr == nil {
			normalPreparation = owner.State != "RESTORING"
		}
	}
	releaseJob, released, replenishJob, replenished, err := m.store.FinishPreparationWithReplenishment(ctx, id)
	if err != nil {
		m.log.Error("finish preparation failed", "slot_id", id, "error", err)
		return err
	}
	if released {
		m.schedule(releaseJob)
		return nil
	}
	if normalPreparation && m.standbyReplenishmentEnabled(w) {
		m.handleNormalSessionSuccess(ctx, w, replenishJob, replenished)
	}
	return nil
}

func (m *Manager) materializeWorkspaceRoot(source, slotPath string, rules config.Workspace) error {
	// SQLiteの所有権確認後もroot置換の窓を作らないよう、pathベースでmaterializeしない。
	root, ok := m.rootForPath(slotPath)
	if !ok {
		return fmt.Errorf("%w: slot path is outside known wx roots", state.ErrOwnership)
	}
	owner, closeOwner, err := m.existingRootDescriptor(root)
	if err != nil {
		return fmt.Errorf("%w: open slot root namespace: %w", state.ErrOwnership, err)
	}
	defer closeOwner()
	if err := verifyRootDescriptorPath(root, owner); err != nil {
		return err
	}
	relative, ok := relativeWithinRoot(root, slotPath)
	if !ok {
		return fmt.Errorf("%w: slot path is outside wx root", state.ErrOwnership)
	}
	destination, err := domain.OpenRootAt(owner, relative)
	if err != nil {
		return fmt.Errorf("%w: open slot root namespace: %w", state.ErrOwnership, err)
	}
	defer func() { _ = destination.Close() }()
	if err := workspace.MaterializeRootAt(source, destination, rules); err != nil {
		return err
	}
	if err := verifyRootDescriptorPath(root, owner); err != nil {
		return err
	}
	return nil
}

func (m *Manager) readyMatches(ctx context.Context, s state.Slot, resolved []pool.Resolved) (bool, error) {
	root, ok := m.rootForPath(s.Path)
	if !ok {
		configured, configuredErr := config.ExpandHome(m.Config().Storage.WorktreeRoot)
		if configuredErr != nil || !domain.IsWithin(configured, s.Path) {
			return false, nil
		}
	}
	owner, closeOwner, err := m.existingRootDescriptor(root)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("%w: open ready slot root: %w", state.ErrOwnership, err)
	}
	defer closeOwner()
	relativeSlot, ok := relativeWithinRoot(root, s.Path)
	if !ok {
		return false, fmt.Errorf("%w: ready slot path is outside wx root", state.ErrOwnership)
	}
	slotInfo, err := owner.Lstat(relativeSlot)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("%w: open ready slot root: %w", state.ErrOwnership, err)
	}
	if slotInfo.Mode()&os.ModeSymlink != 0 || !slotInfo.IsDir() {
		return false, nil
	}
	slotDirectory, _, err := domain.OpenDirectoryAt(owner, relativeSlot)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("%w: open ready slot root: %w", state.ErrOwnership, err)
	}
	if err := slotDirectory.Close(); err != nil {
		return false, fmt.Errorf("%w: close ready slot root: %w", state.ErrOwnership, err)
	}
	return m.readyRepositoriesMatch(ctx, s, resolved, root, owner)
}

func (m *Manager) readyRepositoriesMatch(ctx context.Context, s state.Slot, resolved []pool.Resolved, root string, owner *os.Root) (bool, error) {
	repos, err := m.store.SlotRepositories(ctx, s.ID)
	if err != nil {
		return false, err
	}
	if len(repos) != len(resolved) {
		return false, nil
	}
	byID := map[string]state.SlotRepository{}
	for _, r := range repos {
		byID[r.RepositoryID] = r
	}
	preparer := m.newPreparer(m.Config(), s)
	for _, r := range resolved {
		stored, ok := byID[string(r.Repository.ID)]
		if !ok || (stored.State != "READY" && stored.State != "COLD") || stored.BaseOID != r.OID {
			return false, nil
		}
		fp, err := workspace.Fingerprint(s.Generation, r.OID, r.Repository, m.Config())
		if err != nil {
			return false, err
		}
		if fp != stored.Fingerprint {
			return false, nil
		}
		if stored.State == "COLD" {
			unmaterialized, coldErr := coldWorktreeUnmaterialized(owner, root, stored.WorktreePath)
			if coldErr != nil || !unmaterialized {
				return false, coldErr
			}
			continue
		}
		relative, ok := relativeWithinRoot(root, stored.WorktreePath)
		if !ok {
			return false, fmt.Errorf("%w: ready worktree path is outside wx root", state.ErrOwnership)
		}
		info, err := owner.Lstat(relative)
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		if err != nil {
			return false, fmt.Errorf("%w: inspect ready worktree path: %w", state.ErrOwnership, err)
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return false, nil
		}
		directory, _, openErr := domain.OpenDirectoryAt(owner, relative)
		if openErr != nil {
			return false, fmt.Errorf("%w: open ready worktree path: %w", state.ErrOwnership, openErr)
		}
		if closeErr := directory.Close(); closeErr != nil {
			return false, fmt.Errorf("%w: close ready worktree path: %w", state.ErrOwnership, closeErr)
		}
		if err := preparer.ValidateReady(ctx, r.Repository, stored.WorktreePath, r.OID); err != nil {
			return false, err
		}
	}
	return true, nil
}

func coldWorktreeUnmaterialized(owner *os.Root, root, worktreePath string) (bool, error) {
	relative, ok := relativeWithinRoot(root, worktreePath)
	if !ok {
		return false, fmt.Errorf("%w: cold worktree path is outside wx root", state.ErrOwnership)
	}
	info, err := owner.Lstat(relative)
	if errors.Is(err, os.ErrNotExist) {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("%w: inspect cold worktree path: %w", state.ErrOwnership, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return false, nil
	}
	directory, _, openErr := domain.OpenDirectoryAt(owner, relative)
	if openErr != nil {
		return false, fmt.Errorf("%w: inspect cold worktree path: %w", state.ErrOwnership, openErr)
	}
	entries, readErr := directory.Readdirnames(1)
	closeErr := directory.Close()
	if readErr != nil && !errors.Is(readErr, io.EOF) {
		return false, fmt.Errorf("%w: inspect cold worktree path: %w", state.ErrOwnership, readErr)
	}
	if closeErr != nil {
		return false, fmt.Errorf("%w: close cold worktree path: %w", state.ErrOwnership, closeErr)
	}
	return len(entries) == 0, nil
}

// standbyQuarantineLimit は、貸出前の隔離 slot をこの数まで許して補充を続ける上限。
// 隔離 slot は待機枠に数えないので環境が直れば補充で自己回復するが、GC も clean も隔離実体を消さないため、
// 準備が壊れ続ける workspace で無制限に worktree が積み上がるのを防ぐ。
const standbyQuarantineLimit = 3

func (m *Manager) ensureStandby(ctx context.Context, w discovery.Workspace) error {
	cfg := m.Config()
	if !m.standbyReplenishmentEnabled(w) {
		return nil
	}
	// clean 後の補充停止は永続化してあるため、定期 reconcile と補充ジョブの双方でここを通る。
	if m.replenishSuspended(ctx, string(w.ID)) {
		return nil
	}
	quarantined, err := m.store.QuarantinedStandbyCount(ctx, string(w.ID))
	if err != nil {
		return err
	}
	if quarantined >= standbyQuarantineLimit {
		// 隔離実体を消す製品内の経路が無く、片付けるまで補充は再開しない。
		// 10 分ごとの reconcile から呼ばれるため、上限へ達したことは workspace ごとに一度だけ Warn で伝える。
		if m.markStandbyQuarantineWarned(string(w.ID)) {
			m.log.Warn("standby replenishment stopped until quarantined slots are removed", "workspace_id", w.ID, "quarantined", quarantined, "limit", standbyQuarantineLimit)
		} else {
			m.log.Debug("standby replenishment stopped by quarantined slots", "workspace_id", w.ID, "quarantined", quarantined)
		}
		return nil
	}
	m.clearStandbyQuarantineWarned(string(w.ID))
	needed := cfg.Pool.WarmPerWorkspace - m.store.StandbyCount(ctx, string(w.ID))
	if needed <= 0 {
		return nil
	}
	resolved, err := pool.ResolveBranches(ctx, m.git, w, nil)
	if err != nil {
		m.log.Error("resolve standby base failed", "workspace_id", w.ID, "error", err)
		return err
	}
	generation, err := m.store.WorkspaceGeneration(ctx, string(w.ID))
	if err != nil {
		return err
	}
	hotBefore := state.FormatTime(time.Now().UTC().Add(-cfg.Retention.HotStandby.Duration))
	hot, err := m.store.HotRepositoryIDs(ctx, hotBefore)
	if err != nil {
		return err
	}
	rootPath, rootID, err := m.activeRoot()
	if err != nil {
		return err
	}
	for range needed {
		job, err := m.createStandbySlot(ctx, rootPath, rootID, w, resolved, generation, hot)
		if err != nil {
			return err
		}
		if job.ID != "" {
			m.schedule(job)
		}
	}
	return nil
}

// markStandbyQuarantineWarned は隔離上限の警告をまだ出していない workspace で true を返し、以降は false を返す。
func (m *Manager) markStandbyQuarantineWarned(workspaceID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.standbyQuarantineWarned == nil {
		m.standbyQuarantineWarned = map[string]bool{}
	}
	if m.standbyQuarantineWarned[workspaceID] {
		return false
	}
	m.standbyQuarantineWarned[workspaceID] = true
	return true
}

// clearStandbyQuarantineWarned は上限を下回った workspace の記録を消し、再発時に再び警告できるようにする。
func (m *Manager) clearStandbyQuarantineWarned(workspaceID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.standbyQuarantineWarned, workspaceID)
}

func (m *Manager) standbyReplenishmentEnabled(w discovery.Workspace) bool {
	cfg := m.Config()
	return cfg.WorktreeMode(string(w.Root)) == "hot" && cfg.Pool.WarmPerWorkspace >= 1 && cfg.Retention.HotStandby.Duration > 0
}

func (m *Manager) handleNormalSessionSuccess(ctx context.Context, w discovery.Workspace, replenishJob state.Job, replenished bool) {
	if !m.standbyReplenishmentEnabled(w) {
		return
	}
	m.resumeReplenish(ctx, string(w.ID))
	if replenished {
		m.schedule(replenishJob)
		return
	}
	// 除外対象が無い成功も、貸出で減った通常の待機枠を補う必要がある。
	_ = m.enqueue("ENSURE_STANDBY", string(w.ID), "", "")
}

func (m *Manager) createStandbySlot(ctx context.Context, rootPath, rootID string, w discovery.Workspace, resolved []pool.Resolved, generation int, hot map[string]bool) (state.Job, error) {
	var lastErr error
	for range idAllocationAttempts {
		id, err := newSlotID()
		if err != nil {
			return state.Job{}, err
		}
		relPath, err := slotRelPath(string(w.ID), id, false)
		if err != nil {
			return state.Job{}, err
		}
		slotPath := filepath.Join(rootPath, relPath)
		slotIdentity, _, err := m.createSlotRoot(slotPath, slotPath)
		if err != nil {
			if cleanupErr := m.cleanupStandbySlotCandidate(ctx, rootPath, slotPath, id, rootID, relPath, slotIdentity); cleanupErr != nil {
				return state.Job{}, cleanupErr
			}
			return state.Job{}, err
		}
		repos, err := m.slotRepos(slotPath, w, resolved, generation, hot)
		if err != nil {
			if cleanupErr := m.cleanupStandbySlotCandidate(ctx, rootPath, slotPath, id, rootID, relPath, slotIdentity); cleanupErr != nil {
				return state.Job{}, cleanupErr
			}
			return state.Job{}, err
		}
		job, created, err := m.store.CreateStandbyIfNeeded(ctx, state.Slot{ID: id, WorkspaceID: string(w.ID), Generation: generation, RootID: rootID, RelPath: relPath, DirIdentity: slotIdentity, State: "PREPARING"}, repos, m.Config().Pool.WarmPerWorkspace, standbyQuarantineLimit)
		if err == nil {
			if created {
				return job, nil
			}
			if cleanupErr := m.cleanupStandbySlotCandidate(ctx, rootPath, slotPath, id, rootID, relPath, slotIdentity); cleanupErr != nil {
				return state.Job{}, cleanupErr
			}
			return state.Job{}, nil
		}
		if state.IsIDCollision(err) {
			// slot IDの衝突では同じ物理pathを既存slotが所有している可能性がある。
			// その場合は新規作成途中の実体ではないため、所有中のworktreeを削除せず再抽選する。
			existing, lookupErr := m.store.Slot(ctx, id)
			if lookupErr == nil && existing.RootID == rootID && existing.RelPath == relPath {
				lastErr = err
				continue
			}
			if lookupErr != nil && !errors.Is(lookupErr, sql.ErrNoRows) {
				return state.Job{}, lookupErr
			}
		}
		if cleanupErr := m.cleanupStandbySlotCandidate(ctx, rootPath, slotPath, id, rootID, relPath, slotIdentity); cleanupErr != nil {
			return state.Job{}, cleanupErr
		}
		if !state.IsIDCollision(err) {
			return state.Job{}, err
		}
		lastErr = err
	}
	return state.Job{}, fmt.Errorf("create standby slot: %w", lastErr)
}

func (m *Manager) cleanupStandbySlotCandidate(ctx context.Context, rootPath, slotPath, slotID, rootID, relPath, expectedIdentity string) error {
	registered, err := m.store.Slot(ctx, slotID)
	if err == nil {
		if registered.RootID == rootID && registered.RelPath == relPath {
			return nil
		}
	} else if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	return m.removeUnregisteredSlotRoot(ctx, rootPath, slotPath, expectedIdentity)
}

func (m *Manager) removeUnregisteredSlotRoot(ctx context.Context, rootPath, slotPath, expectedIdentity string) error {
	owner, release, err := m.existingRootDescriptor(rootPath)
	if err != nil {
		return err
	}
	defer release()
	if err := verifyRootDescriptorPath(rootPath, owner); err != nil {
		return err
	}
	relative, ok := relativeWithinRoot(rootPath, slotPath)
	if !ok || relative == "." {
		return fmt.Errorf("%w: unregistered standby slot is outside worktree root", state.ErrOwnership)
	}
	actual, identityErr := directoryIdentityAt(owner, relative)
	if errors.Is(identityErr, os.ErrNotExist) {
		return nil
	}
	if identityErr != nil {
		_ = m.store.QuarantineArtifact(ctx, "standby_slot", slotPath, "ownership could not be proven for an unregistered slot")
		return fmt.Errorf("%w: inspect unregistered standby slot: %w", state.ErrOwnership, identityErr)
	}
	if actual != expectedIdentity {
		_ = m.store.QuarantineArtifact(ctx, "standby_slot", slotPath, "unregistered slot inode changed before cleanup")
		return fmt.Errorf("%w: unregistered standby slot identity changed", state.ErrOwnership)
	}
	if err := owner.RemoveAll(relative); err != nil {
		_ = m.store.QuarantineArtifact(ctx, "standby_slot", slotPath, "unregistered slot cleanup failed")
		return err
	}
	return verifyRootDescriptorPath(rootPath, owner)
}

func (m *Manager) AllocateResumeSlot(ctx context.Context, agent string, pid int) (Lease, error) {
	// workspace未確定のresumeはslot directoryをleaseし、SessionStartでbind後にworktreeをその配下へ作る。
	rootPath, rootID, err := m.activeRoot()
	if err != nil {
		return Lease{}, err
	}
	token, err := state.TokenHex()
	if err != nil {
		return Lease{}, err
	}
	var lastErr error
	for range idAllocationAttempts {
		id, idErr := newSlotID()
		if idErr != nil {
			return Lease{}, idErr
		}
		lease, retry, allocErr := m.allocateResumeSlotWithID(ctx, id, rootPath, rootID, token, agent, pid)
		if allocErr == nil {
			return lease, nil
		}
		if !retry {
			return Lease{}, allocErr
		}
		lastErr = allocErr
	}
	return Lease{}, fmt.Errorf("allocate resume slot: %w", lastErr)
}

func (m *Manager) allocateResumeSlotWithID(ctx context.Context, id, rootPath, rootID, token, agent string, pid int) (Lease, bool, error) {
	relPath, err := slotRelPath("", id, true)
	if err != nil {
		return Lease{}, false, err
	}
	slotPath := filepath.Join(rootPath, relPath)
	releaseRoot, err := m.holdRootForPath(slotPath)
	if err != nil {
		return Lease{}, false, err
	}
	defer releaseRoot()
	slotIdentity, _, err := m.createSlotRoot(slotPath, slotPath)
	if err != nil {
		return Lease{}, false, err
	}
	if err := m.retainLease(id, slotPath); err != nil {
		return Lease{}, false, err
	}
	session := state.Session{ID: id, SlotID: id, State: "UNBOUND", AgentKind: agent, ClientPID: pid, TokenHash: state.HashToken(token)}
	if _, err := m.store.CreateSlotSession(ctx, state.Slot{ID: id, Generation: 0, RootID: rootID, RelPath: relPath, DirIdentity: slotIdentity, State: "UNBOUND"}, nil, session, ""); err != nil {
		m.releaseLease(id)
		return Lease{}, state.IsIDCollision(err), err
	}
	return Lease{SessionID: id, Token: token, Path: slotPath, RootIdentity: slotIdentity}, false, nil
}

func (m *Manager) WaitReady(ctx context.Context, id, token string) error {
	if _, err := m.store.Session(ctx, id, token); err != nil {
		return err
	}
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		slot, err := m.store.Slot(ctx, id)
		if err != nil {
			return err
		}
		switch slot.State {
		case "READY", "LEASED":
			return nil
		case "FAILED", "QUARANTINED":
			failureID := slot.FailureCode
			if failureID == "" {
				failureID = "UNKNOWN"
			}
			for _, prefix := range []string{"PREPARE_FAILED:", "RESTORE_FAILED:"} {
				if strings.HasPrefix(failureID, prefix) {
					failureID = strings.TrimPrefix(failureID, prefix)
					break
				}
			}
			metadata := readPrepareDiagnostic(slot.FailureDetailPath)
			if metadata.FailureID != "" {
				failureID = metadata.FailureID
			}
			detailPath := slot.FailureDetailPath
			if detailPath == "" {
				detailPath = "unavailable"
			}
			exitCode := "unknown"
			if metadata.HasExitCode {
				exitCode = strconv.Itoa(metadata.ExitCode)
			}
			return fmt.Errorf("workspace readiness failed: state=%s failure_id=%s detail_path=%s exit_code=%s timed_out=%t canceled=%t; run `wx status` or `wx doctor` for details", slot.State, failureID, detailPath, exitCode, metadata.TimedOut, metadata.Canceled)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

type prepareDiagnosticMetadata struct {
	FailureID   string
	ExitCode    int
	HasExitCode bool
	TimedOut    bool
	Canceled    bool
}

func readPrepareDiagnostic(path string) prepareDiagnosticMetadata {
	var metadata prepareDiagnosticMetadata
	if path == "" {
		return metadata
	}
	file, err := os.Open(path)
	if err != nil {
		return metadata
	}
	defer func() { _ = file.Close() }()
	data, err := io.ReadAll(io.LimitReader(file, 8<<10))
	if err != nil {
		return metadata
	}
	for _, line := range strings.Split(string(data), "\n") {
		key, value, ok := strings.Cut(line, ": ")
		if !ok {
			continue
		}
		switch key {
		case "failure_id":
			metadata.FailureID = strings.TrimSpace(value)
		case "exit_code":
			if exitCode, parseErr := strconv.Atoi(strings.TrimSpace(value)); parseErr == nil {
				metadata.ExitCode, metadata.HasExitCode = exitCode, true
			}
		case "timed_out":
			metadata.TimedOut, _ = strconv.ParseBool(strings.TrimSpace(value))
		case "canceled":
			metadata.Canceled, _ = strconv.ParseBool(strings.TrimSpace(value))
		}
	}
	return metadata
}

func (m *Manager) BindAgentSession(ctx context.Context, id, token, agentID string) error {
	if _, err := m.store.Session(ctx, id, token); err != nil {
		return err
	}
	return m.store.BindAgentSession(ctx, id, agentID)
}

func (m *Manager) PrepareFreshResume(ctx context.Context, id, token, agentID, cwd string, branches []string) error {
	current, err := m.store.Session(ctx, id, token)
	if err != nil {
		return err
	}
	fail := func(code string, cause error) error {
		_ = m.store.SetSlotState(ctx, current.SlotID, []string{"UNBOUND", "PREPARING"}, "FAILED", code)
		return cause
	}
	prior, err := m.store.FindByAgentSession(ctx, current.AgentKind, agentID)
	parentID := ""
	var w discovery.Workspace
	switch {
	case errors.Is(err, sql.ErrNoRows):
		if cwd == "" {
			return fail("FRESH_SOURCE_UNKNOWN", errors.New("--fresh source workspace is unavailable"))
		}
		discoverer := discovery.Discoverer{Git: m.git, Config: m.Config()}
		w, err = discoverer.Resolve(ctx, cwd)
		if err == nil {
			w, err = m.store.CanonicalWorkspace(ctx, w)
		}
	case err != nil:
		return fail("FRESH_LOOKUP_FAILED", err)
	case prior.State != "EXPIRED":
		return fail("FRESH_RESUME_REFUSED", fmt.Errorf("--fresh is refused because wx session %s is %s, not EXPIRED", prior.ID, prior.State))
	default:
		parentID = prior.ID
		w, err = m.store.Workspace(ctx, prior.WorkspaceID)
	}
	if err != nil {
		return fail("FRESH_SOURCE_FAILED", err)
	}
	w, generation, err := m.store.UpsertWorkspaceGeneration(ctx, w)
	if err != nil {
		return fail("FRESH_STATE_FAILED", err)
	}
	resolved, err := pool.ResolveBranches(ctx, m.git, w, branches)
	if err != nil {
		return fail("FRESH_RESOLVE_FAILED", err)
	}
	slot, err := m.store.Slot(ctx, current.SlotID)
	if err != nil {
		return fail("FRESH_STATE_FAILED", err)
	}
	repositories, err := m.slotRepos(slot.Path, w, resolved, generation, nil)
	if err != nil {
		return fail("FRESH_LAYOUT_FAILED", err)
	}
	job, err := m.store.BindFreshResumeSlot(ctx, id, parentID, string(w.ID), agentID, generation, repositories)
	if err != nil {
		return fail("FRESH_BIND_FAILED", err)
	}
	m.schedule(job)
	return nil
}

func (m *Manager) BindAndRestoreResume(ctx context.Context, id, token, agentID string) error {
	current, err := m.store.Session(ctx, id, token)
	if err != nil {
		return err
	}
	prior, err := m.store.FindByAgentSession(ctx, current.AgentKind, agentID)
	if err != nil {
		return fmt.Errorf("no wx recovery mapping for agent session %s; rerun with --fresh only if local workspace state is no longer needed", agentID)
	}
	snaps, err := m.store.Snapshots(ctx, prior.ID)
	if err != nil {
		return err
	}
	switch prior.State {
	case "RELEASING", "SNAPSHOTTING":
	case "ARCHIVED":
		w, workspaceErr := m.store.SessionWorkspace(ctx, prior.ID)
		if workspaceErr != nil {
			return workspaceErr
		}
		usable, usableErr := m.recoveryUsable(ctx, prior.ID, w, snaps, time.Now())
		if usableErr != nil {
			return fmt.Errorf("validate wx recovery snapshot: %w", usableErr)
		}
		if !usable {
			return fmt.Errorf("wx recovery snapshot is expired or unavailable; stop this session and run wx resume %s %s resume %s, or rerun the native resume with --fresh only if local workspace state may be discarded", prior.ID, current.AgentKind, agentID)
		}
	case "EXPIRED":
		return fmt.Errorf("wx recovery snapshot is expired or unavailable; stop this session and run wx resume %s %s resume %s, or rerun the native resume with --fresh only if local workspace state may be discarded", prior.ID, current.AgentKind, agentID)
	default:
		return fmt.Errorf("wx session %s is %s and cannot be resumed without first completing release", prior.ID, prior.State)
	}
	w, err := m.store.SessionWorkspace(ctx, prior.ID)
	if err != nil {
		return err
	}
	generation, err := m.store.WorkspaceGeneration(ctx, string(w.ID))
	if err != nil {
		return err
	}
	job, err := m.store.BindResumeSlot(ctx, id, prior.ID, string(w.ID), agentID, generation, nil)
	if err != nil {
		return err
	}
	m.schedule(job)
	return nil
}

func (m *Manager) restoreSlot(ctx context.Context, id string, w discovery.Workspace, resolved []pool.Resolved, repos []state.SlotRepository, snaps map[string]state.Snapshot) error {
	slotState, err := m.store.Slot(ctx, id)
	if err != nil {
		return err
	}
	if slotState.State == "READY" || slotState.State == "LEASED" {
		return nil
	}
	if slotState.State != "RESTORING" {
		return fmt.Errorf("slot %s cannot be restored from %s", id, slotState.State)
	}
	releaseRoot, err := m.holdRootForPath(slotState.Path)
	if err != nil {
		m.quarantineOwnershipFailure(id, []string{"RESTORING"}, err)
		return err
	}
	defer releaseRoot()
	archiveManager := m.newArchiveManager(m.Config(), slotState)
	if len(repos) != len(resolved) {
		return errors.New("restore repository metadata does not match resolved workspace")
	}
	for i, r := range resolved {
		stored, err := m.store.SlotRepository(ctx, id, string(r.Repository.ID))
		if err != nil {
			return err
		}
		if stored.State == "READY" {
			if err := archiveManager.Preparer.ValidateRestoringSlotWorktreeOwnership(ctx, r.Repository, stored.WorktreePath, r.OID, id); err != nil {
				m.quarantineOwnershipFailure(id, []string{"RESTORING"}, err)
				return err
			}
			continue
		}
		if stored.State == "RESTORE_RUNNING" {
			if override := m.Config().Repositories[string(r.Repository.MainPath)]; len(override.Prepare.Command) > 0 {
				err := errors.New("restore preparation command completion is ambiguous after interruption")
				_ = m.store.SetSlotState(ctx, id, []string{"RESTORING"}, "QUARANTINED", "RESTORE_AMBIGUOUS")
				return err
			}
		} else if err := m.store.SetSlotRepositoryState(ctx, id, string(r.Repository.ID), []string{"RESTORING"}, "RESTORE_RUNNING"); err != nil {
			return err
		}
		repositoryPath := repos[i].WorktreePath // #nosec G602 -- equal slice lengths are checked before the loop.
		if err := archiveManager.Restore(ctx, r.Repository, repositoryPath, id, snaps[string(r.Repository.ID)]); err != nil {
			m.log.Error("restore failed", "slot_id", id, "repository_id", r.Repository.ID, "error", err)
			var prepareErr *workspace.PrepareCommandError
			if errors.As(err, &prepareErr) {
				failureCode := "RESTORE_FAILED"
				if prepareErr.FailureID != "" {
					failureCode += ":" + prepareErr.FailureID
				}
				_ = m.store.SetSlotStateWithDetail(ctx, id, []string{"RESTORING"}, "QUARANTINED", failureCode, prepareErr.DetailPath)
			} else {
				_ = m.store.SetSlotState(ctx, id, []string{"RESTORING"}, "QUARANTINED", "RESTORE_FAILED")
			}
			return err
		}
		identity, identityErr := archiveManager.Preparer.WorktreeIdentity(repositoryPath)
		if identityErr != nil {
			m.quarantineOwnershipFailure(id, []string{"RESTORING"}, fmt.Errorf("%w: capture restored worktree identity: %w", state.ErrOwnership, identityErr))
			return identityErr
		}
		if err := m.store.RecordSlotRepositoryIdentity(ctx, id, string(r.Repository.ID), identity); err != nil {
			return err
		}
		if err := m.store.SetSlotRepositoryState(ctx, id, string(r.Repository.ID), []string{"RESTORE_RUNNING"}, "READY"); err != nil {
			return err
		}
	}
	if w.Kind == "multi_repository" {
		slot, err := m.store.Slot(ctx, id)
		if err != nil {
			return err
		}
		dirIdentity, identityErr := m.ownedDirectoryIdentity(slot.Path)
		if identityErr != nil {
			m.quarantineOwnershipFailure(id, []string{"RESTORING"}, fmt.Errorf("%w: read slot directory identity: %w", state.ErrOwnership, identityErr))
			return identityErr
		}
		if err := m.store.ValidateSlotOwnership(context.Background(), state.SlotOwnershipRequest{SlotID: id, WorkspaceID: slot.WorkspaceID, RootID: slot.RootID, RelPath: slot.RelPath, DirIdentity: dirIdentity, AllowedSlotStates: []string{"RESTORING"}}); err != nil {
			if errors.Is(err, state.ErrOwnership) {
				_ = m.store.SetSlotState(context.Background(), id, []string{"RESTORING"}, "QUARANTINED", "WORKTREE_OWNERSHIP_UNCERTAIN")
			}
			return err
		}
		if err := m.materializeWorkspaceRoot(string(w.Root), slot.Path, m.Config().Workspaces[string(w.Root)]); err != nil {
			if errors.Is(err, state.ErrOwnership) {
				_ = m.store.SetSlotState(context.Background(), id, []string{"RESTORING"}, "QUARANTINED", "WORKTREE_OWNERSHIP_UNCERTAIN")
			} else {
				_ = m.store.SetSlotState(ctx, id, []string{"RESTORING"}, "FAILED", "ROOT_MATERIALIZATION_FAILED")
			}
			return err
		}
		session, err := m.store.SessionByID(ctx, id)
		if err != nil {
			return err
		}
		rootSnapshot, found, err := m.store.WorkspaceSnapshot(ctx, session.ParentSessionID)
		if err != nil {
			return err
		}
		if !found {
			_ = m.store.SetSlotState(ctx, id, []string{"RESTORING"}, "QUARANTINED", "SNAPSHOT_INCOMPLETE")
			return errors.New("multi-repository recovery snapshot has no workspace root archive")
		}
		targetRoot, targetOK := m.rootForPath(slot.Path)
		archiveRoot, archiveOK := m.rootForPath(rootSnapshot.ArchivePath)
		if !targetOK || !archiveOK {
			_ = m.store.SetSlotState(ctx, id, []string{"RESTORING"}, "QUARANTINED", "SNAPSHOT_INCOMPLETE")
			return errors.New("workspace root recovery paths are outside known wx roots")
		}
		targetRootHandle, closeTargetRoot, targetRootErr := m.existingRootDescriptor(targetRoot)
		if targetRootErr != nil {
			_ = m.store.SetSlotState(ctx, id, []string{"RESTORING"}, "QUARANTINED", "SNAPSHOT_INCOMPLETE")
			return fmt.Errorf("open workspace restore target root: %w", targetRootErr)
		}
		defer closeTargetRoot()
		archiveRootHandle, closeArchiveRoot, archiveRootErr := m.existingRootDescriptor(archiveRoot)
		if archiveRootErr != nil {
			_ = m.store.SetSlotState(ctx, id, []string{"RESTORING"}, "QUARANTINED", "SNAPSHOT_INCOMPLETE")
			return fmt.Errorf("open workspace archive root: %w", archiveRootErr)
		}
		defer closeArchiveRoot()
		if err := archive.RestoreWorkspaceAt(ctx, slot.Path, targetRoot, targetRootHandle, archiveRoot, archiveRootHandle, rootSnapshot, workspaceRecoveryExclusions(w, repos, m.Config())); err != nil {
			_ = m.store.SetSlotState(ctx, id, []string{"RESTORING"}, "QUARANTINED", "RESTORE_FAILED")
			return fmt.Errorf("restore workspace root: %w", err)
		}
	}
	_, _, err = m.store.FinishPreparationWithRelease(ctx, id)
	return err
}

func (m *Manager) ResumeStatus(ctx context.Context, oldID string) (map[string]any, error) {
	old, err := m.store.SessionByID(ctx, oldID)
	if err != nil {
		return nil, err
	}
	snaps, err := m.store.Snapshots(ctx, oldID)
	if err != nil {
		return nil, err
	}
	pending := old.State == "RELEASING" || old.State == "SNAPSHOTTING"
	expired := old.State == "EXPIRED"
	if !pending && !expired {
		w, err := m.store.SessionWorkspace(ctx, oldID)
		if err != nil {
			return nil, err
		}
		usable, err := m.recoveryUsable(ctx, oldID, w, snaps, time.Now())
		if err != nil {
			return nil, err
		}
		expired = !usable
	}
	return map[string]any{"state": old.State, "expired": expired, "pending": pending, "workspace_id": old.WorkspaceID}, nil
}

func snapshotsUsable(snaps []state.Snapshot, at time.Time) bool {
	if len(snaps) == 0 {
		return false
	}
	for _, snapshot := range snaps {
		expires, err := time.Parse(time.RFC3339Nano, snapshot.ExpiresAt)
		if err != nil || !expires.After(at) {
			return false
		}
	}
	return true
}

func (m *Manager) recoveryUsable(ctx context.Context, sessionID string, w discovery.Workspace, snapshots []state.Snapshot, at time.Time) (bool, error) {
	if !snapshotsUsable(snapshots, at) {
		return false, nil
	}
	if w.Kind != "multi_repository" {
		return true, nil
	}
	rootSnapshot, found, err := m.store.WorkspaceSnapshot(ctx, sessionID)
	if err != nil || !found {
		return false, err
	}
	root, ok := m.rootForPath(rootSnapshot.ArchivePath)
	if !ok {
		return false, errors.New("workspace snapshot archive is outside known wx roots")
	}
	owner, releaseOwner, err := m.existingRootDescriptor(root)
	if err != nil {
		return false, fmt.Errorf("open workspace snapshot owner: %w", err)
	}
	defer releaseOwner()
	if err := archive.ValidateWorkspaceSnapshotAt(root, owner, rootSnapshot, at); err != nil {
		return false, err
	}
	return true, nil
}

func workspaceRecoveryExclusions(w discovery.Workspace, repos []state.SlotRepository, cfg config.Config) []string {
	// bundle root内のworktree、slot marker、.worktreelinkをsnapshot/prune対象から外す。
	// source相対pathではなくslot_repositories.dir_nameを使わないとnested repositoryを消し得る。
	links := cfg.Workspaces[string(w.Root)].Link
	excluded := make([]string, 0, 2*len(repos)+len(links))
	for _, repository := range repos {
		if repository.DirName == "" {
			continue
		}
		excluded = append(excluded, repository.DirName, workspace.OwnershipMarkerName(repository.RepositoryID))
	}
	excluded = append(excluded, links...)
	return excluded
}

func (m *Manager) Resume(ctx context.Context, oldID, agent string, pid int, allowFresh bool) (Lease, error) {
	old, err := m.store.SessionByID(ctx, oldID)
	if err != nil {
		return Lease{}, err
	}
	snaps, err := m.store.Snapshots(ctx, oldID)
	if err != nil {
		return Lease{}, err
	}
	if old.State == "RELEASING" || old.State == "SNAPSHOTTING" {
		old, snaps, err = m.waitForSnapshot(ctx, oldID)
		if err != nil {
			return Lease{}, err
		}
	}
	usable := false
	var archivedWorkspace discovery.Workspace
	if old.State != "EXPIRED" {
		archivedWorkspace, err = m.store.SessionWorkspace(ctx, oldID)
		if err != nil {
			return Lease{}, err
		}
		usable, err = m.recoveryUsable(ctx, oldID, archivedWorkspace, snaps, time.Now())
		if err != nil {
			return Lease{}, fmt.Errorf("validate recovery snapshot: %w", err)
		}
	}
	if old.State == "EXPIRED" || !usable {
		if !allowFresh {
			return Lease{}, errors.New("session snapshot is EXPIRED; confirmation is required before creating a workspace from the current base")
		}
		w, err := m.store.Workspace(ctx, old.WorkspaceID)
		if err != nil {
			return Lease{}, err
		}
		resolved, err := pool.ResolveBranches(ctx, m.git, w, nil)
		if err != nil {
			return Lease{}, err
		}
		generation, err := m.store.WorkspaceGeneration(ctx, string(w.ID))
		if err != nil {
			return Lease{}, err
		}
		return m.allocate(ctx, w, resolved, generation, agent, pid, "STARTING", oldID)
	}
	w := archivedWorkspace
	resolved := make([]pool.Resolved, 0, len(w.Repositories))
	for _, repo := range w.Repositories {
		var found *state.Snapshot
		for i := range snaps {
			if snaps[i].RepositoryID == string(repo.ID) {
				found = &snaps[i]
				break
			}
		}
		if found == nil {
			return Lease{}, errors.New("incomplete recovery snapshot")
		}
		resolved = append(resolved, pool.Resolved{Repository: repo, RequestedRef: found.HeadRef, OID: found.HeadOID})
	}
	generation, err := m.store.WorkspaceGeneration(ctx, string(w.ID))
	if err != nil {
		return Lease{}, err
	}
	lease, err := m.allocate(ctx, w, resolved, generation, agent, pid, "RESTORING", oldID)
	if err != nil {
		return Lease{}, err
	}
	return lease, nil
}

func (m *Manager) waitForSnapshot(ctx context.Context, sessionID string) (state.Session, []state.Snapshot, error) {
	waitCtx, cancel := context.WithTimeout(ctx, m.Config().Readiness.Timeout.Duration)
	defer cancel()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		session, err := m.store.SessionByID(waitCtx, sessionID)
		if err != nil {
			return state.Session{}, nil, err
		}
		snapshots, err := m.store.Snapshots(waitCtx, sessionID)
		if err != nil {
			return state.Session{}, nil, err
		}
		if session.State == "ARCHIVED" {
			w, workspaceErr := m.store.SessionWorkspace(waitCtx, sessionID)
			if workspaceErr != nil {
				return session, snapshots, workspaceErr
			}
			usable, usableErr := m.recoveryUsable(waitCtx, sessionID, w, snapshots, time.Now())
			if usableErr != nil {
				return session, snapshots, usableErr
			}
			if usable {
				return session, snapshots, nil
			}
		}
		if session.State == "EXPIRED" {
			return session, snapshots, errors.New("session recovery snapshot expired while waiting for archive")
		}
		slot, err := m.store.Slot(waitCtx, session.SlotID)
		if err == nil && (slot.State == "FAILED" || slot.State == "QUARANTINED") {
			return session, snapshots, fmt.Errorf("session archive failed: slot is %s", slot.State)
		}
		select {
		case <-waitCtx.Done():
			return session, snapshots, waitCtx.Err()
		case <-ticker.C:
		}
	}
}

func (m *Manager) Release(ctx context.Context, id, token, reason string) error {
	session, err := m.store.Session(ctx, id, token)
	if err != nil {
		return err
	}
	if reason == "session-end-hook" && (processAlive(session.ClientPID) || processAlive(session.AgentPID)) {
		return nil
	}
	job, changed, quarantineExpired, err := m.store.ReleaseWithOutcome(ctx, id, session.WorkspaceID, session.SlotID)
	if err != nil {
		return err
	}
	if quarantineExpired {
		// clientはReleaseの応答を読まないため、snapshotを作らずに終端したことはログだけが残す。
		m.log.Warn("session expired without a recovery snapshot: slot is quarantined", "session_id", id, "slot_id", session.SlotID, "reason", reason)
	}
	if !changed {
		m.releaseLease(id)
		return nil
	}
	m.schedule(job)
	return nil
}

func (m *Manager) snapshotSession(ctx context.Context, s state.Session) error {
	if processAlive(s.AgentPID) {
		return dependencyPendingError{fmt.Errorf("agent process %d is still active", s.AgentPID)}
	}
	slot, err := m.store.Slot(ctx, s.SlotID)
	if err != nil {
		return err
	}
	if s.State == "ARCHIVED" && (slot.State == "SNAPSHOTTED" || slot.State == "ARCHIVED") {
		return nil
	}
	if slot.State == "DRAINING" || slot.State == "SNAPSHOTTING" {
		if err := m.store.BeginSnapshot(ctx, s.ID, s.SlotID); err != nil {
			return err
		}
	} else {
		return fmt.Errorf("slot %s cannot be snapshotted from %s", s.SlotID, slot.State)
	}
	repos, err := m.store.SlotRepositories(ctx, s.SlotID)
	if err != nil {
		return err
	}
	releasedAt, err := time.Parse(time.RFC3339Nano, s.ReleasedAt)
	if err != nil {
		return fmt.Errorf("session %s has invalid release time: %w", s.ID, err)
	}
	expiry := releasedAt.Add(m.Config().Retention.RecoverySnapshot.Duration)
	releaseRoot, err := m.holdRootForPath(slot.Path)
	if err != nil {
		m.quarantineOwnershipFailure(s.SlotID, []string{"SNAPSHOTTING"}, err)
		return err
	}
	defer releaseRoot()
	archiveManager := m.newArchiveManager(m.Config(), slot)
	for _, sr := range repos {
		repo, err := m.store.Repository(ctx, sr.RepositoryID)
		if err != nil {
			return err
		}
		_, err = archiveManager.SnapshotWithPersistence(ctx, repo, sr.WorktreePath, s.ID, expiry, func(snapshot state.Snapshot) error {
			return m.store.SaveSnapshot(ctx, snapshot)
		})
		if err != nil {
			m.log.Error("snapshot failed", "session_id", s.ID, "repository_id", repo.ID, "error", err)
			_ = m.store.SetSlotState(ctx, s.SlotID, []string{"SNAPSHOTTING"}, "QUARANTINED", "SNAPSHOT_FAILED")
			return err
		}
	}
	workspaceKind, err := m.store.SessionWorkspaceKind(ctx, s.ID)
	if err != nil {
		return err
	}
	if workspaceKind == "multi_repository" {
		ownershipRoot, ownershipRootID, rootIDErr := m.rootIDForPath(slot.Path)
		if rootIDErr != nil {
			return rootIDErr
		}
		ownershipRootHandle, closeOwnershipRoot, rootErr := m.existingRootDescriptor(ownershipRoot)
		if rootErr != nil {
			return fmt.Errorf("open workspace archive root: %w", rootErr)
		}
		defer closeOwnershipRoot()
		rootSnapshot, found, err := m.store.WorkspaceSnapshot(ctx, s.ID)
		if err != nil {
			return err
		}
		if found {
			if err := archive.ValidateWorkspaceSnapshotAt(ownershipRoot, ownershipRootHandle, rootSnapshot, time.Now()); err != nil {
				_ = m.store.SetSlotState(ctx, s.SlotID, []string{"SNAPSHOTTING"}, "QUARANTINED", "SNAPSHOT_FAILED")
				return fmt.Errorf("validate workspace root snapshot: %w", err)
			}
		} else {
			w, err := m.store.SessionWorkspace(ctx, s.ID)
			if err != nil {
				return err
			}
			rootSnapshot, err = archive.SnapshotWorkspaceAt(ctx, slot.Path, ownershipRoot, ownershipRootID, ownershipRootHandle, s.ID, workspaceRecoveryExclusions(w, repos, m.Config()), expiry)
			if err != nil {
				m.log.Error("workspace root snapshot failed", "session_id", s.ID, "error", err)
				_ = m.store.SetSlotState(ctx, s.SlotID, []string{"SNAPSHOTTING"}, "QUARANTINED", "SNAPSHOT_FAILED")
				return err
			}
			if err := m.store.SaveWorkspaceSnapshot(ctx, rootSnapshot); err != nil {
				return err
			}
		}
	}
	return m.store.MarkArchived(ctx, s.ID, s.SlotID, state.FormatTime(expiry))
}

func (m *Manager) GC(ctx context.Context, dry bool) (GCResult, error) {
	progress := newGCProgress()
	cfg := m.Config()
	nowTime := time.Now().UTC()
	failedBefore := state.FormatTime(nowTime.Add(-cfg.Retention.FailedJob.Duration))
	eventBefore := state.FormatTime(nowTime.Add(-cfg.Retention.EventLog.Duration))
	tombstoneBefore := state.FormatTime(nowTime.Add(-cfg.Retention.ExpiredSessionTombstone.Duration))
	metadataCount, err := m.store.CountMetadataCandidates(ctx, failedBefore, eventBefore, tombstoneBefore)
	if err != nil {
		progress.addFailed("metadata", "metadata candidate query failed", err)
		return progress.GCResult, progress.err()
	}
	before := state.FormatTime(nowTime.Add(-cfg.Retention.EndedWorktree.Duration))
	items, err := m.store.GCCandidates(ctx, before)
	if err != nil {
		progress.addFailed("ended worktrees", "ended worktree candidate query failed", err)
		return progress.GCResult, progress.err()
	}
	standbys, err := m.store.StandbyGCCandidates(ctx, state.FormatTime(nowTime.Add(-cfg.Retention.HotStandby.Duration)), cfg.Pool.WarmPerWorkspace)
	if err != nil {
		progress.addFailed("standby worktrees", "standby candidate query failed", err)
		return progress.GCResult, progress.err()
	}
	var cold []state.ColdRepositoryCandidate
	if cfg.Pool.WarmPerWorkspace > 0 {
		cold, err = m.store.ColdRepositoryCandidates(ctx, state.FormatTime(nowTime.Add(-cfg.Retention.HotStandby.Duration)))
		if err != nil {
			progress.addFailed("cold repositories", "cold repository candidate query failed", err)
			return progress.GCResult, progress.err()
		}
	}
	expired, err := m.store.ExpiredSnapshots(ctx, state.FormatTime(nowTime))
	if err != nil {
		progress.addFailed("snapshots", "expired snapshot candidate query failed", err)
		return progress.GCResult, progress.err()
	}
	expiredSessions := map[string][]state.Snapshot{}
	for _, snapshot := range expired {
		expiredSessions[snapshot.SessionID] = append(expiredSessions[snapshot.SessionID], snapshot)
	}
	wholeSlotRemoval := map[string]bool{}
	for _, standby := range standbys {
		wholeSlotRemoval[standby.SlotID] = true
	}
	totalCold := 0
	for _, candidate := range cold {
		if !wholeSlotRemoval[candidate.SlotID] {
			totalCold++
		}
	}
	progress.Candidates = metadataCount + len(items) + len(standbys) + len(expiredSessions) + totalCold
	if dry {
		// dry-run は状態を変更せず、候補が処理されずに残る見込みを pending として報告する。
		progress.Pending = progress.Candidates
		return progress.GCResult, nil
	}
	if err := m.store.PruneMetadata(ctx, failedBefore, eventBefore, tombstoneBefore); err != nil {
		progress.addFailed("metadata", "metadata pruning failed", err)
	} else {
		progress.Completed += metadataCount
	}
	progress.merge(m.scheduleColdRepositoryRemovals(ctx, cold, wholeSlotRemoval))
	progress.merge(m.scheduleStandbyRemovals(ctx, standbys))
	progress.merge(m.scheduleEndedWorktreeRemovals(ctx, items))
	archiveManager := m.newArchiveManager(cfg, state.Slot{})
	progress.merge(m.expireWorkspaceSnapshots(ctx, expiredSessions, &archiveManager))
	// retired rootはSQLiteの参照が消えた後にrowだけをpruneし、設定済みdirectory自体は削除しない。
	if err := m.store.PruneRoots(ctx); err != nil {
		progress.addFailed("roots", "retired worktree root metadata pruning failed", err)
	}
	return progress.GCResult, progress.err()
}

func (m *Manager) quarantineCleanupFailure(slotID string, runErr error) error {
	if !errors.Is(runErr, state.ErrOwnership) {
		return runErr
	}
	if quarantineErr := m.store.QuarantineMissingSlot(context.Background(), slotID, "WORKTREE_OWNERSHIP_UNCERTAIN"); quarantineErr != nil {
		m.log.Error("quarantine cleanup ownership failure", "slot_id", slotID, "error", quarantineErr)
		return errors.Join(runErr, fmt.Errorf("quarantine slot after ownership failure: %w", quarantineErr))
	}
	return runErr
}

func (m *Manager) scheduleColdRepositoryRemovals(ctx context.Context, candidates []state.ColdRepositoryCandidate, wholeSlotRemoval map[string]bool) gcProgress {
	progress := newGCProgress()
	for _, candidate := range candidates {
		if wholeSlotRemoval[candidate.SlotID] {
			continue
		}
		target := fmt.Sprintf("cold repository %s/%s", candidate.SlotID, candidate.RepositoryID)
		if _, release, err := m.holdVerifiedRootForPath(candidate.WorktreePath); err != nil {
			reasonErr := m.quarantineCleanupFailure(candidate.SlotID, err)
			reason := "cleanup ownership verification failed; the artifact was not deleted"
			if errors.Is(err, state.ErrOwnership) {
				reason = "ownership could not be proven; the artifact was quarantined instead of deleted"
			}
			progress.addPending(target, reason, reasonErr)
			continue
		} else {
			release()
		}
		job, changed, err := m.store.ScheduleColdRepositoryRemoval(ctx, candidate)
		if err != nil {
			m.log.Error("cold repository removal scheduling failed", "slot_id", candidate.SlotID, "repository_id", candidate.RepositoryID, "error", err)
			progress.addFailed(target, "cold repository removal reservation failed", err)
			continue
		}
		if !changed {
			progress.addPending(target, "candidate changed before removal reservation", nil)
			continue
		}
		m.schedule(job)
		progress.Scheduled++
	}
	return progress
}

func (m *Manager) scheduleStandbyRemovals(ctx context.Context, candidates []state.StandbyGCCandidate) gcProgress {
	progress := newGCProgress()
	for _, candidate := range candidates {
		progress.merge(m.scheduleRemovalCandidate(ctx, candidate.SlotID, candidate.Path, "", "standby removal scheduling failed"))
	}
	return progress
}

func (m *Manager) scheduleEndedWorktreeRemovals(ctx context.Context, candidates []state.GCCandidate) gcProgress {
	progress := newGCProgress()
	for _, candidate := range candidates {
		progress.merge(m.scheduleRemovalCandidate(ctx, candidate.SlotID, candidate.Path, candidate.SessionID, "ended worktree removal scheduling failed"))
	}
	return progress
}

func (m *Manager) scheduleRemovalCandidate(ctx context.Context, slotID, path, sessionID, logMessage string) gcProgress {
	progress := newGCProgress()
	target := "worktree " + slotID
	if _, release, err := m.holdVerifiedRootForPath(path); err != nil {
		reasonErr := m.quarantineCleanupFailure(slotID, err)
		reason := "cleanup ownership verification failed; the artifact was not deleted"
		if errors.Is(err, state.ErrOwnership) {
			reason = "ownership could not be proven; the artifact was quarantined instead of deleted"
		}
		progress.addPending(target, reason, reasonErr)
		return progress
	} else {
		release()
	}
	job, changed, err := m.store.ScheduleRemoval(ctx, slotID, sessionID)
	if err != nil {
		m.log.Error(logMessage, "slot_id", slotID, "error", err)
		progress.addFailed(target, logMessage, err)
		return progress
	}
	if !changed {
		progress.addPending(target, "candidate changed before removal reservation", nil)
		return progress
	}
	m.schedule(job)
	progress.Scheduled++
	return progress
}

func (m *Manager) expireWorkspaceSnapshots(ctx context.Context, expiredSessions map[string][]state.Snapshot, archiveManager *archive.Manager) gcProgress {
	progress := newGCProgress()
	sessionIDs := make([]string, 0, len(expiredSessions))
	for sessionID := range expiredSessions {
		sessionIDs = append(sessionIDs, sessionID)
	}
	sort.Strings(sessionIDs)
	for _, sessionID := range sessionIDs {
		snapshots := expiredSessions[sessionID]
		ok := true
		var rootSnapshot state.WorkspaceSnapshot
		var rootSnapshotOwner string
		var rootSnapshotOwnerHandle *os.Root
		var rootSnapshotOwnerRelease func()
		workspaceKind, workspaceErr := m.store.SessionWorkspaceKind(ctx, sessionID)
		if workspaceErr != nil {
			progress.addPending("snapshots "+sessionID, "session workspace metadata could not be read", workspaceErr)
			ok = false
		} else if workspaceKind == "multi_repository" {
			var found bool
			rootSnapshot, found, workspaceErr = m.store.WorkspaceSnapshot(ctx, sessionID)
			if workspaceErr != nil {
				progress.addPending("workspace snapshot "+sessionID, "workspace snapshot metadata could not be read", workspaceErr)
				ok = false
			} else if !found {
				progress.addPending("workspace snapshot "+sessionID, "workspace snapshot metadata is incomplete", errors.New("workspace snapshot is missing"))
				ok = false
			} else if owner, releaseOwner, ownerErr := m.holdVerifiedRootForPath(rootSnapshot.ArchivePath); ownerErr != nil {
				quarantineErr := m.store.QuarantineArtifact(context.Background(), "workspace_snapshot", rootSnapshot.ArchivePath, "ownership could not be proven during cleanup")
				if quarantineErr != nil {
					ownerErr = errors.Join(ownerErr, fmt.Errorf("quarantine workspace snapshot failed: %w", quarantineErr))
				}
				progress.addPending("workspace snapshot "+sessionID, "ownership could not be proven; the artifact was quarantined instead of deleted", ownerErr)
				ok = false
			} else {
				rootSnapshotOwner = owner
				rootSnapshotOwnerRelease = releaseOwner
				rootSnapshotOwnerHandle = m.rootHandleForRoot(owner)
				if rootSnapshotOwnerHandle == nil {
					progress.addPending("workspace snapshot "+sessionID, "workspace snapshot ownership handle is unavailable", errors.New("workspace snapshot ownership root descriptor is unavailable"))
					ok = false
				} else {
					if validateErr := archive.ValidateWorkspaceSnapshotAt(owner, rootSnapshotOwnerHandle, rootSnapshot, time.Time{}); validateErr != nil {
						progress.addPending("workspace snapshot "+sessionID, "workspace snapshot metadata or artifact validation failed", validateErr)
						ok = false
					}
				}
			}
		}
		if !ok {
			if rootSnapshotOwnerRelease != nil {
				rootSnapshotOwnerRelease()
			}
			continue
		}
		for _, snapshot := range snapshots {
			repo, err := m.store.Repository(ctx, snapshot.RepositoryID)
			if err != nil {
				progress.addFailed("snapshot "+sessionID+"/"+snapshot.RepositoryID, "snapshot repository metadata could not be read", err)
				ok = false
				break
			}
			if err := archiveManager.DeleteSnapshotRefs(ctx, repo, snapshot); err != nil {
				progress.addFailed("snapshot "+sessionID+"/"+snapshot.RepositoryID, "snapshot recovery refs could not be deleted", err)
				ok = false
				break
			}
		}
		if ok && rootSnapshot.SessionID != "" {
			if err := archive.DeleteWorkspaceSnapshotAt(rootSnapshotOwner, rootSnapshotOwnerHandle, rootSnapshot); err != nil {
				progress.addFailed("workspace snapshot "+sessionID, "workspace snapshot archive could not be deleted", err)
				ok = false
			}
		}
		if ok {
			if err := m.store.ExpireSessionSnapshots(ctx, sessionID); err != nil {
				progress.addFailed("snapshots "+sessionID, "snapshot metadata could not be expired", err)
			} else {
				progress.Completed++
			}
		}
		if rootSnapshotOwnerRelease != nil {
			rootSnapshotOwnerRelease()
		}
	}
	return progress
}

func (m *Manager) removeSlotJob(ctx context.Context, job state.Job) error {
	if job.SessionID != "" {
		session, err := m.store.SessionByID(ctx, job.SessionID)
		if err != nil {
			return err
		}
		if processAlive(session.AgentPID) {
			return dependencyPendingError{fmt.Errorf("agent process %d is still active", session.AgentPID)}
		}
	}
	slot, err := m.store.Slot(ctx, job.SlotID)
	if err != nil {
		return err
	}
	if slot.State == "ARCHIVED" {
		return nil
	}
	if slot.State != "REMOVING" {
		return fmt.Errorf("slot %s cannot be removed from %s", slot.ID, slot.State)
	}
	root, releaseRoot, err := m.holdVerifiedRootForPath(slot.Path)
	if err != nil {
		m.quarantineOwnershipFailure(slot.ID, []string{"REMOVING"}, err)
		return err
	}
	defer releaseRoot()
	archiveManager := m.newArchiveManager(m.Config(), slot)
	if err := m.removeSlotWorktrees(ctx, archiveManager, root, slot, job.SessionID); err != nil {
		m.quarantineOwnershipFailure(slot.ID, []string{"REMOVING"}, err)
		return err
	}
	return m.store.FinishRemoval(ctx, slot.ID)
}

func (m *Manager) removeColdRepositoryJob(ctx context.Context, job state.Job) error {
	slot, err := m.store.Slot(ctx, job.SlotID)
	if err != nil {
		return err
	}
	repositoryState, err := m.store.SlotRepository(ctx, job.SlotID, job.RepositoryID)
	if err != nil {
		return err
	}
	if repositoryState.State == "COLD" {
		return nil
	}
	if repositoryState.State != "RETIRING" || slot.State != "RETIRING" {
		return fmt.Errorf("repository %s/%s cannot retire from %s/%s", slot.ID, job.RepositoryID, slot.State, repositoryState.State)
	}
	root, releaseRoot, err := m.holdVerifiedRootForPath(slot.Path)
	if err != nil {
		m.quarantineOwnershipFailure(slot.ID, []string{"RETIRING"}, err)
		return err
	}
	if !domain.IsWithin(root, repositoryState.WorktreePath) {
		releaseRoot()
		err := fmt.Errorf("%w: cold repository path is outside wx ownership root", state.ErrOwnership)
		m.quarantineOwnershipFailure(slot.ID, []string{"RETIRING"}, err)
		return err
	}
	defer releaseRoot()
	repository, err := m.store.Repository(ctx, job.RepositoryID)
	if err != nil {
		return err
	}
	archiveManager := m.newArchiveManager(m.Config(), slot)
	if err := archiveManager.RemoveWorktree(ctx, repository, root, repositoryState.WorktreePath, repositoryState.BaseOID); err != nil {
		m.quarantineOwnershipFailure(slot.ID, []string{"RETIRING"}, err)
		return err
	}
	// repositoryをretireしてもslot directoryとownership markerは残し、後のslot削除で証明できるようにする。
	return m.store.FinishColdRepositoryRemoval(ctx, slot.ID, job.RepositoryID)
}

func (m *Manager) quarantineOwnershipFailure(slotID string, from []string, runErr error) {
	if !errors.Is(runErr, state.ErrOwnership) || m.store == nil {
		return
	}
	if err := m.store.SetSlotState(context.Background(), slotID, from, "QUARANTINED", "WORKTREE_OWNERSHIP_UNCERTAIN"); err != nil && m.log != nil {
		m.log.Error("quarantine uncertain worktree ownership failed", "slot_id", slotID, "error", err)
	}
}

func (m *Manager) rootForPath(path string) (string, bool) {
	// active/retired rootが重なる場合は最長一致を選び、map順序で別のdescriptorを借りない。
	m.mu.RLock()
	defer m.mu.RUnlock()
	path = filepath.Clean(path)
	best := ""
	for root := range m.roots {
		if domain.IsWithin(root, path) {
			if best == "" || len(root) > len(best) {
				best = root
			}
		}
	}
	for root := range m.rootIdentities {
		if domain.IsWithin(root, path) && (best == "" || len(root) > len(best)) {
			best = root
		}
	}
	return best, best != ""
}

func rootPathsOverlap(first, second string) bool {
	first, second = filepath.Clean(first), filepath.Clean(second)
	return first == second || domain.IsWithin(first, second) || domain.IsWithin(second, first)
}

func (m *Manager) rootHandleForPath(path string) *os.Root {
	// 周囲のholdRootForPathがdescriptorの寿命を保持する。ここで参照数は増減させない。
	root, ok := m.rootForPath(path)
	if !ok {
		return nil
	}
	return m.rootHandleForRoot(root)
}

func (m *Manager) rootHandleForRoot(root string) *os.Root {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if entry := m.rootRefs[root]; entry != nil && !entry.closed && entry.root != nil {
		return entry.root
	}
	entries := m.retiredRefs[root]
	for index := len(entries) - 1; index >= 0; index-- {
		if entry := entries[index]; entry != nil && !entry.closed && entry.root != nil {
			return entry.root
		}
	}
	return nil
}

func (m *Manager) ownedPathExists(path string) (bool, error) {
	// 置換・消失したhistorical rootは通常の欠損ではなく所有権エラーとして扱う。
	root, ok := m.rootForPath(path)
	if !ok {
		return false, fmt.Errorf("%w: path is outside known wx roots", state.ErrOwnership)
	}
	release, err := m.holdRootForPath(path)
	if err != nil {
		return false, err
	}
	defer release()
	owner := m.rootHandleForPath(path)
	if owner == nil {
		return false, fmt.Errorf("%w: root descriptor is unavailable", state.ErrOwnership)
	}
	if err := verifyRootDescriptorPath(root, owner); err != nil {
		return false, err
	}
	relative, ok := relativeWithinRoot(root, path)
	if !ok {
		return false, fmt.Errorf("%w: path is outside root", state.ErrOwnership)
	}
	_, err = owner.Lstat(relative)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("%w: inspect owned path: %w", state.ErrOwnership, err)
	}
	return true, nil
}

func (m *Manager) rootPathsFromStore(ctx context.Context) ([]string, error) {
	// Storeを読めないdegraded状態ではlive descriptor集合へfallbackし、診断を空にしない。
	rows, err := m.store.Roots(ctx)
	if err == nil {
		paths := make([]string, 0, len(rows))
		for _, row := range rows {
			paths = append(paths, filepath.Clean(row.Path))
		}
		return paths, nil
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	paths := make([]string, 0, len(m.roots))
	for root := range m.roots {
		paths = append(paths, root)
	}
	return paths, err
}

func (m *Manager) ownedRootArtifactPaths(root string) ([]string, error) {
	// 列挙単位は<workspace-id>/<slot-id>または_unbound/<slot-id>のslot directoryである。
	// configurable root内の無関係なdirectoryをartifactと誤認しないよう、namespaceの形も検証する。
	root = filepath.Clean(root)
	m.mu.RLock()
	active, known := m.roots[root]
	m.mu.RUnlock()
	var release func()
	var err error
	if known && active {
		_, release, err = m.rootDescriptor(root)
	} else {
		_, release, err = m.existingRootDescriptor(root)
	}
	if err != nil {
		return nil, err
	}
	defer release()
	owner := m.rootHandleForRoot(root)
	if owner == nil {
		return nil, fmt.Errorf("%w: root descriptor is unavailable", state.ErrOwnership)
	}
	if err := verifyRootDescriptorPath(root, owner); err != nil {
		return nil, err
	}
	entries, err := fs.ReadDir(owner.FS(), ".")
	if errors.Is(err, os.ErrNotExist) {
		entries = nil
	} else if err != nil {
		return nil, fmt.Errorf("%w: inspect wx root namespace: %w", state.ErrOwnership, err)
	}
	paths := make([]string, 0)
	for _, entry := range entries {
		if entry.Type()&os.ModeSymlink != 0 || !entry.IsDir() {
			continue
		}
		if entry.Name() != unboundNamespace && !domain.ValidShortID(entry.Name()) {
			continue
		}
		slotPaths, readErr := ownedSlotDirectories(owner, root, entry.Name())
		if readErr != nil {
			return nil, readErr
		}
		paths = append(paths, slotPaths...)
	}
	return paths, nil
}

func ownedSlotDirectories(owner *os.Root, root, namespace string) ([]string, error) {
	slots, err := fs.ReadDir(owner.FS(), namespace)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("%w: inspect workspace slots: %w", state.ErrOwnership, err)
	}
	paths := make([]string, 0, len(slots))
	for _, slotEntry := range slots {
		if slotEntry.Type()&os.ModeSymlink != 0 || !slotEntry.IsDir() {
			continue
		}
		info, infoErr := owner.Lstat(filepath.FromSlash(path.Join(namespace, slotEntry.Name())))
		if errors.Is(infoErr, os.ErrNotExist) {
			continue
		}
		if infoErr != nil {
			return nil, fmt.Errorf("%w: inspect slot directory: %w", state.ErrOwnership, infoErr)
		}
		if info.IsDir() {
			paths = append(paths, filepath.Join(root, namespace, slotEntry.Name()))
		}
	}
	return paths, nil
}

func (m *Manager) holdRootForPath(path string) (func(), error) {
	// 同期操作の全期間でrootを保持し、reload後もworkerがretired descriptorを使い切れるようにする。
	root, ok := m.rootForPath(path)
	if !ok {
		configured, expandErr := config.ExpandHome(m.Config().Storage.WorktreeRoot)
		if expandErr != nil {
			return func() {}, fmt.Errorf("%w: resolve configured wx root: %w", state.ErrOwnership, expandErr)
		}
		if !domain.IsWithin(configured, path) {
			return func() {}, nil
		}
		root = filepath.Clean(configured)
	}
	m.mu.RLock()
	active, known := m.roots[root]
	m.mu.RUnlock()
	var release func()
	var err error
	if ok {
		if !known || !active {
			_, release, err = m.existingRootDescriptor(root)
		} else {
			_, release, err = m.rootDescriptor(root)
		}
	} else {
		_, release, err = m.rootDescriptor(root)
	}
	if err != nil {
		if errors.Is(err, errManagerClosed) {
			return func() {}, errManagerClosed
		}
		return func() {}, fmt.Errorf("%w: hold wx root descriptor: %w", state.ErrOwnership, err)
	}
	return release, nil
}

func (m *Manager) holdVerifiedRootForPath(path string) (string, func(), error) {
	// 遅延cleanupはREMOVINGへ遷移する前に、historical rootのdescriptorとpath名の両方を検証する。
	root, ok := m.rootForPath(path)
	if !ok {
		return "", func() {}, fmt.Errorf("%w: path is outside known wx roots", state.ErrOwnership)
	}
	release, err := m.holdRootForPath(path)
	if err != nil {
		return "", func() {}, err
	}
	owner := m.rootHandleForRoot(root)
	if owner == nil {
		release()
		return "", func() {}, fmt.Errorf("%w: root descriptor is unavailable", state.ErrOwnership)
	}
	if err := verifyRootDescriptorPath(root, owner); err != nil {
		release()
		return "", func() {}, err
	}
	return root, release, nil
}

func (m *Manager) retainLease(sessionID, path string) error {
	// foreground leaseへroot参照を移し、retired rootもsessionのreleaseまで閉じない。
	_, ok := m.rootForPath(path)
	if !ok {
		configured, err := config.ExpandHome(m.Config().Storage.WorktreeRoot)
		if err != nil || !domain.IsWithin(configured, path) {
			if err == nil {
				err = errors.New("lease path is outside known wx roots")
			}
			return fmt.Errorf("%w: %w", state.ErrOwnership, err)
		}
	}
	release, err := m.holdRootForPath(path)
	if err != nil {
		return err
	}
	m.mu.Lock()
	m.ensureRootStateLocked()
	if m.rootClosing {
		m.mu.Unlock()
		release()
		return errManagerClosed
	}
	previous := m.leases[sessionID]
	m.leases[sessionID] = release
	m.mu.Unlock()
	if previous != nil {
		previous()
	}
	return nil
}

func (m *Manager) releaseLease(sessionID string) {
	m.mu.Lock()
	release := m.leases[sessionID]
	delete(m.leases, sessionID)
	m.mu.Unlock()
	if release != nil {
		release()
	}
}

func (m *Manager) removeSlotWorktrees(ctx context.Context, archiveManager archive.Manager, root string, slot state.Slot, sessionID string) error {
	slotID, slotPath := slot.ID, slot.Path
	if !domain.IsWithin(root, slotPath) {
		return fmt.Errorf("%w: slot path is outside wx root", state.ErrOwnership)
	}
	repos, err := m.store.SlotRepositories(ctx, slotID)
	if err != nil {
		return err
	}
	expected := map[string]string{}
	if sessionID != "" {
		workspaceKind, err := m.store.SessionWorkspaceKind(ctx, sessionID)
		if err != nil {
			return removalMetadataFailure("resolve session workspace before removal", err)
		}
		if workspaceKind == "multi_repository" {
			rootSnapshot, found, err := m.store.WorkspaceSnapshot(ctx, sessionID)
			if err != nil {
				return removalMetadataFailure("read workspace root snapshot before removal", err)
			}
			if !found {
				return removalMetadataFailure("workspace root snapshot metadata is incomplete for worktree removal", errors.New("metadata row is missing"))
			}
			archiveRoot, ok := m.rootForPath(rootSnapshot.ArchivePath)
			if !ok {
				return removalMetadataFailure("workspace root snapshot is outside known wx roots", errors.New("archive path is not owned"))
			}
			archiveRootHandle, closeArchiveRoot, rootErr := m.existingRootDescriptor(archiveRoot)
			if rootErr != nil {
				return removalMetadataFailure("open workspace root snapshot owner", rootErr)
			}
			defer closeArchiveRoot()
			if err := archive.ValidateWorkspaceSnapshotAt(archiveRoot, archiveRootHandle, rootSnapshot, time.Now()); err != nil {
				return removalMetadataFailure("validate workspace root snapshot before removal", err)
			}
		}
		snapshots, err := m.store.Snapshots(ctx, sessionID)
		if err != nil {
			return removalMetadataFailure("read repository snapshots before removal", err)
		}
		for _, snapshot := range snapshots {
			expected[snapshot.RepositoryID] = snapshot.HeadOID
		}
	}
	for _, sr := range repos {
		repo, err := m.store.Repository(ctx, sr.RepositoryID)
		if err != nil || !domain.IsWithin(root, sr.WorktreePath) {
			return fmt.Errorf("%w: slot repository ownership validation failed", state.ErrOwnership)
		}
		expectedHead := sr.BaseOID
		if sessionID != "" {
			var ok bool
			expectedHead, ok = expected[sr.RepositoryID]
			if !ok {
				return removalMetadataFailure("snapshot metadata is incomplete for worktree removal", errors.New("repository snapshot row is missing"))
			}
		}
		if err := archiveManager.RemoveWorktree(ctx, repo, root, sr.WorktreePath, expectedHead); err != nil {
			return err
		}
	}
	ownedRoot, closeOwnedRoot, err := m.existingRootDescriptor(root)
	if err != nil {
		return fmt.Errorf("%w: open slot root for removal: %w", state.ErrOwnership, err)
	}
	defer closeOwnedRoot()
	if err := verifyRootDescriptorPath(root, ownedRoot); err != nil {
		return err
	}
	relativeSlot, ok := relativeWithinRoot(root, slotPath)
	if !ok || relativeSlot == "." {
		return fmt.Errorf("%w: open slot root for removal: slot path is outside wx root", state.ErrOwnership)
	}
	// 物理directoryを先に検査し、消失済みならSQLiteの証明より前に正常終了する。
	info, err := ownedRoot.Lstat(relativeSlot)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return fmt.Errorf("%w: inspect slot root for removal: %w", state.ErrOwnership, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("%w: slot root is not a physical directory", state.ErrOwnership)
	}
	dirIdentity, err := directoryIdentityAt(ownedRoot, relativeSlot)
	if err != nil {
		return fmt.Errorf("%w: read slot directory identity for removal: %w", state.ErrOwnership, err)
	}
	// 破壊操作直前の証明にはdiskから再取得したidentityを渡し、row自身との比較にしない。
	if err := m.store.ValidateSlotOwnership(context.Background(), state.SlotOwnershipRequest{SlotID: slotID, RootID: slot.RootID, RelPath: slot.RelPath, DirIdentity: dirIdentity, AllowedSlotStates: []string{"REMOVING"}}); err != nil {
		return err
	}
	if err := ownedRoot.RemoveAll(relativeSlot); err != nil {
		return err
	}
	return verifyRootDescriptorPath(root, ownedRoot)
}

func relativeWithinRoot(root, path string) (relative string, ok bool) {
	// filepath.IsLocalで".."、絶対path、未cleanなroot相対pathをまとめて拒否する。
	relative, err := filepath.Rel(filepath.Clean(root), filepath.Clean(path))
	if err != nil || !filepath.IsLocal(relative) {
		return "", false
	}
	return relative, true
}

func verifyRootDescriptorPath(path string, owner *os.Root) error {
	// descriptorが固定したinodeとpath名がまだ同じnamespaceを指すことを確認してから状態を更新する。
	if owner == nil {
		return fmt.Errorf("%w: wx root descriptor is unavailable", state.ErrOwnership)
	}
	current, _, err := domain.OpenOwnedRoot(path, path)
	if err != nil {
		return fmt.Errorf("%w: wx root path changed: %w", state.ErrOwnership, err)
	}
	defer func() { _ = current.Close() }()
	heldInfo, err := owner.Lstat(".")
	if err != nil {
		return fmt.Errorf("%w: inspect pinned wx root: %w", state.ErrOwnership, err)
	}
	currentInfo, err := current.Lstat(".")
	if err != nil {
		return fmt.Errorf("%w: inspect wx root path: %w", state.ErrOwnership, err)
	}
	if !os.SameFile(heldInfo, currentInfo) {
		return fmt.Errorf("%w: wx root path names a different directory", state.ErrOwnership)
	}
	return nil
}

func removalMetadataFailure(message string, err error) error {
	if err == nil || errors.Is(err, state.ErrOwnership) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return fmt.Errorf("%w: %s: %w", state.ErrOwnership, message, err)
}

func (m *Manager) Status(ctx context.Context) (map[string]any, error) {
	s, err := m.store.Status(ctx)
	if err != nil {
		return nil, err
	}
	details, err := m.store.StatusDiagnostics(ctx)
	if err != nil {
		return nil, err
	}
	m.mu.RLock()
	reloadAt, reloadError, backupAt, backupError := m.lastReload, m.reloadError, m.lastBackup, m.backupError
	rootError := m.rootError
	restartPending, stopPending := m.restartPending, m.stopPending
	cfg := m.cfg
	roots := make(map[string]bool, len(m.roots))
	for root, active := range m.roots {
		roots[root] = active
	}
	m.mu.RUnlock()
	if rows, rootsErr := m.store.Roots(ctx); rootsErr == nil {
		for _, row := range rows {
			path := filepath.Clean(row.Path)
			if _, known := roots[path]; !known {
				roots[path] = row.Active
			}
		}
	}
	for index := range details.Repositories {
		details.Repositories[index].Hot = false
		if leasedAt, parseErr := time.Parse(time.RFC3339Nano, details.Repositories[index].LastUsedAt); parseErr == nil {
			expiresAt := leasedAt.Add(cfg.Retention.HotStandby.Duration)
			details.Repositories[index].StandbyExpiresAt = state.FormatTime(expiresAt)
			details.Repositories[index].Hot = time.Now().Before(expiresAt)
		}
	}
	for index := range details.Sessions {
		if createdAt, parseErr := time.Parse(time.RFC3339Nano, details.Sessions[index].CreatedAt); parseErr == nil {
			details.Sessions[index].AgeSeconds = int64(time.Since(createdAt).Seconds())
		}
	}
	type rootStatus struct {
		Path           string `json:"path"`
		Active         bool   `json:"active"`
		Bytes          int64  `json:"bytes"`
		AllocatedBytes int64  `json:"allocated_bytes"`
		Measurement    string `json:"measurement"`
		Error          string `json:"error,omitempty"`
	}
	rootStatuses := make([]rootStatus, 0, len(roots))
	for root, active := range roots {
		bytes, allocated, usageErr := m.rootDirectoryUsage(root)
		item := rootStatus{Path: root, Active: active, Bytes: bytes, AllocatedBytes: allocated, Measurement: "st_blocks_x_512"}
		if usageErr != nil && !errors.Is(usageErr, os.ErrNotExist) {
			item.Error = usageErr.Error()
		}
		rootStatuses = append(rootStatuses, item)
	}
	sort.Slice(rootStatuses, func(i, j int) bool { return rootStatuses[i].Path < rootStatuses[j].Path })
	return map[string]any{
		"schema_version": state.JSONSchemaVersion, "db_schema_version": state.SchemaVersion, "daemon_version": daemonVersion(), "protocol_version": 1, "uptime_seconds": int(time.Since(m.started).Seconds()),
		"pid":         os.Getpid(),
		"config_path": must(config.Path()), "config_last_reload": reloadAt.UTC().Format(time.RFC3339Nano), "config_reload_error": reloadError, "worktree_root_error": rootError,
		"sqlite_last_backup": formatOptionalTime(backupAt), "sqlite_backup_error": backupError, "restart_pending": restartPending, "stop_pending": stopPending,
		"workspaces": s.Workspaces, "repositories": s.Repositories,
		"slots":           map[string]int{"ready": s.Ready, "leased": s.Leased, "failed": s.Failed, "quarantined": s.Quarantined},
		"active_sessions": s.Active, "snapshots": s.Snapshots, "queued_jobs": s.Jobs, "worktree_roots": rootStatuses,
		"workspace_details": details.Workspaces, "session_details": details.Sessions, "repository_details": details.Repositories,
		"job_details": details.Jobs, "snapshot_details": details.Snapshots, "quarantine": details.Quarantine,
		"retention_seconds": map[string]int64{
			"hot_standby": cfg.Retention.HotStandby.Milliseconds() / 1000, "ended_worktree": cfg.Retention.EndedWorktree.Milliseconds() / 1000,
			"recovery_snapshot": cfg.Retention.RecoverySnapshot.Milliseconds() / 1000, "expired_session_tombstone": cfg.Retention.ExpiredSessionTombstone.Milliseconds() / 1000,
			"failed_job": cfg.Retention.FailedJob.Milliseconds() / 1000, "event_log": cfg.Retention.EventLog.Milliseconds() / 1000,
		},
	}, nil
}

func (m *Manager) rootDirectoryUsage(root string) (int64, int64, error) {
	// path walkではreload後の置換directoryへ渡り得るため、statusもpin済みdescriptor経由で測定する。
	root = filepath.Clean(root)
	m.mu.RLock()
	_, known := m.roots[root]
	m.mu.RUnlock()
	if !known {
		return 0, 0, fmt.Errorf("%w: root is not registered", state.ErrOwnership)
	}
	_, release, err := m.existingRootDescriptor(root)
	if err != nil {
		return 0, 0, err
	}
	defer release()
	owner := m.rootHandleForRoot(root)
	if owner == nil {
		return 0, 0, fmt.Errorf("%w: root descriptor is unavailable", state.ErrOwnership)
	}
	if err := verifyRootDescriptorPath(root, owner); err != nil {
		return 0, 0, err
	}
	var bytes, allocated int64
	walkErr := fs.WalkDir(owner.FS(), ".", func(_ string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		info, infoErr := entry.Info()
		if infoErr != nil {
			return infoErr
		}
		if info.Mode().IsRegular() {
			bytes += info.Size()
		}
		if stat, ok := info.Sys().(*syscall.Stat_t); ok {
			allocated += stat.Blocks * 512
		}
		return nil
	})
	return bytes, allocated, walkErr
}

func daemonVersion() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "unknown"
	}
	if info.Main.Version != "" && info.Main.Version != "(devel)" {
		return info.Main.Version
	}
	for _, setting := range info.Settings {
		if setting.Key == "vcs.revision" {
			return setting.Value
		}
	}
	return "devel"
}

func formatOptionalTime(value time.Time) string {
	if value.IsZero() {
		return ""
	}
	return value.UTC().Format(time.RFC3339Nano)
}

func (m *Manager) Doctor(ctx context.Context) map[string]any {
	m.mu.RLock()
	reloadError, restartPending, cfg := m.reloadError, m.restartPending, m.cfg
	rootError := m.rootError
	m.mu.RUnlock()
	var checks map[string]any
	if restartPending {
		checks = diag.SharedChecksWithoutLaunchAgent(ctx, cfg, reloadError, m.git)
	} else {
		checks = diag.SharedChecks(ctx, cfg, reloadError, m.git)
	}
	if restartPending {
		checks["launch_agent"] = "restart pending; LaunchAgent content check deferred"
	}

	if err := m.store.Ping(ctx); err != nil {
		checks["sqlite"] = err.Error()
	} else {
		checks["sqlite"] = "ok"
	}
	if rootError == "" {
		checks["worktree_root"] = "ok"
	} else {
		checks["worktree_root"] = rootError
	}
	checks["worktree_registration"] = m.registrationDiagnostics(ctx)
	checks["artifact_ownership"] = m.artifactDiagnostics(ctx)
	return map[string]any{"schema_version": state.JSONSchemaVersion, "db_schema_version": state.SchemaVersion, "checks": checks}
}

func diagnosticPath(path string, requiredType os.FileMode, requiredPerm os.FileMode) string {
	return diag.DiagnosticPath(path, requiredType, requiredPerm)
}

func (m *Manager) registrationDiagnostics(ctx context.Context) map[string]any {
	result := map[string]any{"checked": 0, "invalid": []map[string]string{}}
	invalid := []map[string]string{}
	checked := 0
	roots, err := m.store.WorkspaceRoots(ctx)
	if err != nil {
		return map[string]any{"checked": 0, "error": err.Error()}
	}
	discoverer := discovery.Discoverer{Git: m.git, Config: m.Config()}
	for _, root := range roots {
		workspaceRecord, resolveErr := m.resolveRegisteredWorkspace(ctx, root, &discoverer)
		if resolveErr != nil {
			invalid = append(invalid, map[string]string{"workspace_root": root, "error": resolveErr.Error()})
			continue
		}
		resolved, resolveErr := pool.ResolveBranches(ctx, m.git, workspaceRecord, nil)
		if resolveErr != nil {
			invalid = append(invalid, map[string]string{"workspace_root": root, "error": resolveErr.Error()})
			continue
		}
		slots, slotsErr := m.store.ReadySlots(ctx, string(workspaceRecord.ID))
		if slotsErr != nil {
			invalid = append(invalid, map[string]string{"workspace_root": root, "error": slotsErr.Error()})
			continue
		}
		for _, slot := range slots {
			checked++
			valid, validationErr := m.readyMatches(ctx, slot, resolved)
			if validationErr != nil || !valid {
				detail := "READY invariants do not match current repository state"
				if validationErr != nil {
					detail = validationErr.Error()
				}
				invalid = append(invalid, map[string]string{"workspace_root": root, "slot_id": slot.ID, "path": slot.Path, "error": detail})
			}
		}
	}
	result["checked"] = checked
	result["invalid"] = invalid
	return result
}

func (m *Manager) Sessions(ctx context.Context, all bool) ([]state.SessionSummary, error) {
	return m.store.ListSessions(ctx, all)
}

func (m *Manager) Forget(ctx context.Context, path string) error {
	canonical, err := domain.Canonicalize(path)
	if err != nil {
		return err
	}
	// FAILED slotを先にretireしないとworkspace IDが消え、worktreeの所有権を永久に証明できなくなる。
	if w, lookupErr := m.store.WorkspaceByRoot(ctx, string(canonical)); lookupErr == nil {
		failed, failedErr := m.store.FailedSlotIDs(ctx, string(w.ID))
		if failedErr != nil {
			return failedErr
		}
		for _, slotID := range failed {
			if err := m.retireFailedSlotForForget(ctx, slotID); err != nil {
				return fmt.Errorf("retire failed slot %s before forgetting workspace: %w", slotID, err)
			}
		}
	}
	return m.store.ForgetWorkspace(ctx, string(canonical))
}

func (m *Manager) retireFailedSlotForForget(ctx context.Context, slotID string) error {
	// 即時に消せなくてもREMOVING jobを残し、通常のrecoveryが完了するまでForgetを収束させない。
	job, changed, err := m.store.ScheduleFailedSlotRemoval(ctx, slotID)
	if err != nil {
		return err
	}
	if !changed {
		return nil
	}
	claimed, err := m.store.ClaimJob(ctx, job.ID, "wx-forget")
	if err != nil {
		return err
	}
	if runErr := m.runRecoveredJob(ctx, claimed); runErr != nil {
		return runErr
	}
	return m.store.FinishJob(ctx, claimed.ID, "wx-forget", nil)
}

func (m *Manager) ReloadConfig() error {
	return m.reloadConfig(true)
}

func (m *Manager) reloadConfig(runGC bool) error {
	m.reloadMu.Lock()
	defer m.reloadMu.Unlock()
	cfg, err := config.Load()
	if err != nil {
		m.mu.Lock()
		m.lastReload = time.Now()
		m.reloadError = err.Error()
		m.mu.Unlock()
		return err
	}
	m.mu.RLock()
	configuredRoot := m.cfg.Storage.WorktreeRoot
	m.mu.RUnlock()
	oldConfiguredRoot, oldRootErr := config.ExpandHome(configuredRoot)
	if oldRootErr != nil {
		oldConfiguredRoot = configuredRoot
	}
	oldConfiguredRoot = filepath.Clean(oldConfiguredRoot)
	newRoot, newHandle, err := ensureWorktreeRootDescriptor(cfg.Storage.WorktreeRoot)
	if err != nil {
		m.mu.Lock()
		m.lastReload = time.Now()
		m.reloadError = err.Error()
		m.mu.Unlock()
		return fmt.Errorf("validate worktree root: %w", err)
	}
	m.mu.Lock()
	m.ensureRootStateLocked()
	if m.rootClosing {
		m.mu.Unlock()
		_ = newHandle.Close()
		return errManagerClosed
	}
	oldRoot := oldConfiguredRoot
	if oldRoot != newRoot {
		// 旧rootのslotは移動・STALE化せず寿命まで使い、新規slotだけを新rootに置く。
		for existingPath := range m.roots {
			if existingPath == newRoot || !rootPathsOverlap(existingPath, newRoot) || !m.rootHasReferencesLocked(existingPath) {
				continue
			}
			reloadErr := fmt.Errorf("cannot reload overlapping worktree root %s while it has in-flight references", existingPath)
			m.lastReload = time.Now()
			m.reloadError = reloadErr.Error()
			m.mu.Unlock()
			_ = newHandle.Close()
			return fmt.Errorf("validate worktree root: %w", reloadErr)
		}
	}
	var existingHandle *os.Root
	if existing := m.rootRefs[newRoot]; existing != nil && !existing.closed && !existing.retired {
		existingHandle = existing.root
	}
	newIdentity, newIdentityErr := descriptorIdentity(newHandle)
	if expected := m.rootIdentities[newRoot]; expected != "" && (newIdentityErr != nil || expected != newIdentity) {
		reloadErr := fmt.Errorf("worktree root inode changed (expected %s, got %s)", expected, newIdentity)
		if newIdentityErr != nil {
			reloadErr = fmt.Errorf("inspect reloaded worktree root: %w", newIdentityErr)
		}
		m.lastReload = time.Now()
		m.reloadError = reloadErr.Error()
		m.mu.Unlock()
		_ = newHandle.Close()
		return fmt.Errorf("validate worktree root: %w", reloadErr)
	}
	if existingHandle != nil {
		existingInfo, existingErr := existingHandle.Lstat(".")
		newInfo, newInfoErr := newHandle.Lstat(".")
		if existingErr != nil || newInfoErr != nil || !os.SameFile(existingInfo, newInfo) {
			reloadErr := errors.New("configured worktree root inode changed")
			if existingErr != nil {
				reloadErr = fmt.Errorf("inspect current worktree root: %w", existingErr)
			} else if newInfoErr != nil {
				reloadErr = fmt.Errorf("inspect reloaded worktree root: %w", newInfoErr)
			}
			m.lastReload = time.Now()
			m.reloadError = reloadErr.Error()
			m.mu.Unlock()
			_ = newHandle.Close()
			return fmt.Errorf("validate worktree root: %w", reloadErr)
		}
	}
	if existing := m.rootRefs[newRoot]; existing != nil && !existing.closed && !existing.retired {
		_ = newHandle.Close()
		newHandle = nil
	}
	if oldRoot != newRoot {
		m.roots[oldRoot] = false
		m.roots[newRoot] = true
		m.retireRootLocked(oldRoot)
	}
	if newHandle != nil {
		m.rootIdentities[newRoot] = newIdentity
		m.rootRefs[newRoot] = &managedRoot{root: newHandle, identity: newIdentity}
	}
	m.cfg = cfg
	m.git.SetTimeout(cfg.Readiness.Timeout.Duration)
	if m.logLevel != nil {
		m.logLevel.Set(slogLevel(cfg.Logging.Level))
	}
	m.lastReload = time.Now()
	m.reloadError = ""
	m.roots[newRoot] = true
	m.mu.Unlock()
	// EnsureActiveRootは旧rootをinactiveとして残し、そのslotを再発見可能にする。
	m.registerRootGeneration(context.Background(), newRoot, newIdentity)
	m.loadRootGenerations(context.Background())
	m.resizeWorkers(cfg.Pool.PreparationConcurrency)
	select {
	case m.reloads <- struct{}{}:
	default:
	}
	if runGC {
		m.startBackground(func() {
			m.reconcileRegistry(m.ctx)
			m.runBackgroundGC()
		})
	}
	return nil
}

func ensureWorktreeRootDescriptor(value string) (string, *os.Root, error) {
	root, err := config.ExpandHome(value)
	if err != nil {
		return "", nil, err
	}
	root = filepath.Clean(root)
	ownedRoot, err := domain.EnsurePhysicalDirectoryRoot(root, 0o700)
	if err != nil {
		return "", nil, fmt.Errorf("open physical worktree root: %w", err)
	}
	if err := ownedRoot.Chmod(".", 0o700); err != nil {
		_ = ownedRoot.Close()
		return "", nil, err
	}
	return root, ownedRoot, nil
}

func must(v string, e error) string {
	if e != nil {
		return ""
	}
	return v
}

// leaseWithPolicy は RPC の新規作成要求を検証する。一時許可は設定や standby の補充対象を変更しない。
func (m *Manager) leaseWithPolicy(ctx context.Context, cwd string, branches []string, agent string, pid int, force bool) (Lease, error) {
	discoverer := discovery.Discoverer{Git: m.git, Config: m.Config()}
	w, err := discoverer.Resolve(ctx, cwd)
	if err != nil {
		return Lease{}, err
	}
	mode := m.Config().WorktreeMode(string(w.Root))
	if !force && mode != "hot" && mode != "cold" {
		return Lease{}, errors.New("worktree creation is not authorized; select a worktree policy or use --worktree")
	}
	return m.leaseWorkspace(ctx, w, branches, agent, pid, force || mode == "cold")
}
