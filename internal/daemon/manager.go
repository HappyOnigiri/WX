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
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/HappyOnigiri/WX/internal/archive"
	"github.com/HappyOnigiri/WX/internal/config"
	"github.com/HappyOnigiri/WX/internal/discovery"
	"github.com/HappyOnigiri/WX/internal/domain"
	"github.com/HappyOnigiri/WX/internal/gitx"
	"github.com/HappyOnigiri/WX/internal/hookconfig"
	"github.com/HappyOnigiri/WX/internal/launchd"
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

// managedRoot is the lifetime record for one descriptor-bound wx root. A
// retired root remains open while an operation or lease still owns a
// reference; the final release closes it and removes it from the manager's
// handle set.
type managedRoot struct {
	root     *os.Root
	identity string
	refs     int
	retired  bool
	closed   bool
}

type Manager struct {
	mu          sync.RWMutex
	cfg         config.Config
	store       *state.Store
	git         *gitx.Runner
	log         *slog.Logger
	started     time.Time
	lastReload  time.Time
	reloadError string
	lastBackup  time.Time
	backupError string
	roots       map[string]bool
	rootRefs    map[string]*managedRoot
	retiredRefs map[string][]*managedRoot
	// rootIdentities keeps the physical identity of every root pathname that
	// has been admitted during this manager lifetime. A retired descriptor may
	// be reopened for cleanup only when it still names the same inode; a
	// replacement at the old pathname fails closed instead of becoming a new
	// generation for an old slot/job.
	rootIdentities map[string]string
	rootCond       *sync.Cond
	rootClosing    bool
	leases         map[string]func()
	// beforeSlotRootCreate is a deterministic adversarial-test barrier. It is
	// invoked after the pinned root descriptor and relative namespace are ready
	// but before the first descriptor-relative mkdir. Production managers leave
	// it nil.
	beforeSlotRootCreate func()
	// beforeRootClose is a deterministic shutdown barrier for lifecycle tests.
	// Production managers leave it nil.
	beforeRootClose func()
	// executablePath and executableBaseline record the daemon's own binary as
	// it was at startup, so a later replacement (make install renames a new
	// inode over the pathname) can be detected and followed by a restart.
	// executableWatch is false when that baseline could not be taken, which
	// disables the watch rather than letting an unavailable baseline read as
	// "unchanged".
	executablePath     string
	executableBaseline executableSnapshot
	executableWatch    bool
	restartPending     bool
	restartRequested   bool
	restartUnmanaged   bool
	inflightRequests   int
	// kickstart and launchdManaged are test seams for the restart path.
	// Production managers leave them nil and take the launchd implementations.
	kickstart         func(context.Context) error
	launchdManaged    func() bool
	restartChecks     chan struct{}
	jobs              chan jobWork
	reloads           chan struct{}
	ctx               context.Context
	cancel            context.CancelFunc
	wg                sync.WaitGroup
	workersMu         sync.Mutex
	workerStops       []chan struct{}
	workerSeq         int
	closed            bool
	logLevel          *slog.LevelVar
	backgroundMu      sync.Mutex
	backgroundWG      sync.WaitGroup
	backgroundClosing bool
	reloadMu          sync.Mutex
	closeOnce         sync.Once
	closeDoneMu       sync.Mutex
	closeDone         chan struct{}
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
	m := &Manager{cfg: cfg, store: store, git: git, log: logger, started: started, lastReload: started, roots: map[string]bool{}, rootRefs: map[string]*managedRoot{}, retiredRefs: map[string][]*managedRoot{}, rootIdentities: map[string]string{}, leases: map[string]func(){}, jobs: make(chan jobWork, 256), reloads: make(chan struct{}, 1), restartChecks: make(chan struct{}, 1), ctx: managerCtx, cancel: managerCancel}
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
	} else {
		logger.Error("worktree root is unavailable", "path", cfg.Storage.WorktreeRoot, "error", err)
	}
	// Reclaim durable jobs before workers and lifecycle reconciliation can race
	// over stale RUNNING leases or snapshot-ref ownership.
	m.recoverJobs(reclaimAll)
	m.resizeWorkers(cfg.Pool.PreparationConcurrency)
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

// beginRootClose prevents new descriptor acquisitions before cancellation is
// broadcast. Existing references are deliberately left alive until their
// operations return, while durable lease references are released because a
// closing daemon cannot service those leases anymore.
func (m *Manager) beginRootClose() {
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
				if finishErr := m.store.FinishJob(context.Background(), work.id, owner, err); finishErr != nil {
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
		if finishErr := m.store.FinishJob(context.Background(), work.id, owner, err); finishErr != nil {
			m.log.Error("finish job failed", "job_id", work.id, "error", finishErr)
		}
		m.releaseLease(job.SessionID)
	}
}

func (m *Manager) Config() config.Config { m.mu.RLock(); defer m.mu.RUnlock(); return m.cfg }

// newPreparer and newArchiveManager are the single construction points for
// filesystem operations that can reuse or remove a worktree. Production
// callers always provide the state store so those operations cannot fall back
// to a forgeable marker/Git-lock-only proof.
func (m *Manager) newPreparer(cfg config.Config, slotPath string) *workspace.Preparer {
	// A preparation job may outlive a config reload. Keep its filesystem
	// namespace tied to the active or retired root that contains the durable
	// slot path, while still using the newly loaded config for repository rules.
	// The caller must hold the root for the complete Preparer operation; this
	// function only borrows the already-pinned descriptor.
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
	return &workspace.Preparer{Git: m.git, Config: cfg, Ownership: m.store, SlotPath: slotPath, OwnedRoot: ownedRoot, RootPath: filepath.Clean(root)}
}

func (m *Manager) newArchiveManager(cfg config.Config, slotPath string) archive.Manager {
	preparer := m.newPreparer(cfg, slotPath)
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
		// The durable PENDING row is picked up by maintainJobs once capacity frees up.
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

func (m *Manager) maintainJobs() {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-m.ctx.Done():
			return
		case <-m.restartChecks:
			// The last in-flight RPC just returned. Re-evaluate immediately
			// instead of waiting out the rest of the tick, so a pending restart
			// takes the first idle moment it gets.
			m.restartIfReplaced()
		case <-ticker.C:
			m.recoverJobs(false)
			m.detectExecutableReplacement()
			m.restartIfReplaced()
		}
	}
}

func (m *Manager) maintainLifecycle() {
	m.reconcileRegistry(m.ctx)
	m.reconcileArtifacts(m.ctx)
	m.reconcileOrphans(m.ctx)
	m.maybeBackup(m.ctx)
	_, _ = m.GC(m.ctx, false)
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
			m.reconcileRegistry(m.ctx)
			m.reconcileArtifacts(m.ctx)
			m.reconcileOrphans(m.ctx)
			m.maybeBackup(m.ctx)
			if _, err := m.GC(m.ctx, false); err != nil && m.ctx.Err() == nil {
				m.log.Error("automatic GC failed", "error", err)
			}
		}
	}
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
	m.mu.RLock()
	roots := make([]string, 0, len(m.roots))
	for root := range m.roots {
		roots = append(roots, root)
	}
	m.mu.RUnlock()
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

	// The stored root is a cache of the last observed main-worktree path. If it
	// moved, rediscover a repository from its canonical Git common directory
	// before treating the registry entry as lost. This keeps existing slots and
	// session mappings attached to the same workspace identity.
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
		if _, err := m.store.UpsertWorkspaceGeneration(ctx, workspaceRecord); err != nil {
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
		job, changed, err := m.store.Release(ctx, candidate.ID, candidate.WorkspaceID, candidate.SlotID)
		if err != nil {
			m.log.Error("orphan release failed", "session_id", candidate.ID, "error", err)
			continue
		}
		if changed {
			m.schedule(job)
		} else {
			m.releaseLease(candidate.ID)
		}
	}
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
		if err := m.prepareSlot(ctx, job.SlotID, w, resolved, repos); err != nil {
			if errors.Is(err, state.ErrOwnership) {
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
	w, err = m.store.CanonicalWorkspace(ctx, w)
	if err != nil {
		return Lease{}, err
	}
	generation, err := m.store.UpsertWorkspaceGeneration(ctx, w)
	if err != nil {
		return Lease{}, err
	}
	resolved, err := pool.ResolveBranches(ctx, m.git, w, branches)
	if err != nil {
		return Lease{}, err
	}
	// The READY pool is consulted even for an explicit --branch request: if
	// the branch already resolves to exactly what a READY slot holds (e.g.
	// the same branch was just used, or --branch main with nothing new to
	// fetch), readyMatches accepts it like any other exact match. A slot
	// whose base ref genuinely differs from the request still fails the
	// match below and falls through to a fresh allocation, so this does not
	// create a per-branch hot pool: ensureStandby's replacement always
	// resolves against main, unaffected by what any single lease requested.
	for attempts := 0; attempts < m.Config().Pool.WarmPerWorkspace+1; attempts++ {
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
				if errors.Is(matchErr, state.ErrOwnership) {
					m.quarantineOwnershipFailure(ready.ID, []string{"READY"}, matchErr)
				}
				return Lease{}, false, matchErr
			}
			if !valid {
				return Lease{}, false, nil
			}
			rootIdentity, identityErr := m.leaseRootIdentity(ready.Path)
			if identityErr != nil {
				_ = m.store.SetSlotState(context.Background(), ready.ID, []string{"READY"}, "QUARANTINED", "LEASE_ROOT_OWNERSHIP_UNCERTAIN")
				return Lease{}, false, fmt.Errorf("pin ready lease root: %w", identityErr)
			}
			token, tokenErr := state.TokenHex()
			if tokenErr != nil {
				return Lease{}, false, tokenErr
			}
			repositories, repositoryErr := m.store.SlotRepositories(ctx, ready.ID)
			if repositoryErr != nil {
				return Lease{}, false, repositoryErr
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
			if retainErr := m.retainLease(session.ID, ready.Path); retainErr != nil {
				return Lease{}, false, retainErr
			}
			if hasCold {
				job, leaseErr := m.store.LeaseReadyWithCold(ctx, ready.ID, session)
				if leaseErr == nil {
					m.schedule(job)
					_ = m.enqueue("ENSURE_STANDBY", string(w.ID), "", "")
					return Lease{SessionID: session.ID, Token: token, Path: ready.Path, RootIdentity: rootIdentity, SourceWorkspace: string(w.Root), Ready: false}, true, nil
				}
				m.releaseLease(session.ID)
				return Lease{}, false, nil
			}
			if leaseErr := m.store.LeaseReady(ctx, ready.ID, session); leaseErr == nil {
				_ = m.enqueue("ENSURE_STANDBY", string(w.ID), "", "")
				return Lease{SessionID: session.ID, Token: token, Path: ready.Path, RootIdentity: rootIdentity, SourceWorkspace: string(w.Root), Ready: true}, true, nil
			}
			m.releaseLease(session.ID)
			return Lease{}, false, nil
		}()
		if leaseErr != nil {
			return Lease{}, leaseErr
		}
		if leased {
			return lease, nil
		}
		if len(branches) > 0 {
			// A mismatch against an explicit --branch request says nothing
			// about the slot itself: it is still a valid main-based standby
			// for the next request that wants main. Retiring it here would
			// let one --branch lease wipe the whole warm pool and turn the
			// following plain `wx` invocation into a cold start too, which is
			// the opposite of what consulting the pool is for. Leave it READY
			// and allocate a fresh slot instead. ReadySlot would keep
			// returning this same slot, so stop looking rather than
			// re-validating it.
			break
		}
		_ = m.store.SetSlotState(ctx, ready.ID, []string{"READY"}, "STALE", "READY_VALIDATION_FAILED")
	}
	return m.allocate(ctx, w, resolved, generation, agent, pid, "STARTING", "")
}

func (m *Manager) allocate(ctx context.Context, w discovery.Workspace, resolved []pool.Resolved, generation int, agent string, pid int, sessionState, parent string) (Lease, error) {
	id, err := domain.NewID()
	if err != nil {
		return Lease{}, err
	}
	token, err := state.TokenHex()
	if err != nil {
		return Lease{}, err
	}
	root, err := m.slotRoot(string(w.ID), id, false)
	if err != nil {
		return Lease{}, err
	}
	releaseRoot, err := m.holdRootForPath(root)
	if err != nil {
		return Lease{}, err
	}
	defer releaseRoot()
	rootIdentity, err := m.createSlotRoot(root)
	if err != nil {
		return Lease{}, err
	}
	repos, err := m.slotRepos(root, w, resolved, generation, nil)
	if err != nil {
		return Lease{}, err
	}
	slotState := "PREPARING"
	if sessionState == "RESTORING" {
		slotState = "RESTORING"
		for i := range repos {
			repos[i].State = "RESTORING"
		}
	}
	session := state.Session{ID: id, WorkspaceID: string(w.ID), SlotID: id, ParentSessionID: parent, State: sessionState, AgentKind: agent, ClientPID: pid, TokenHash: state.HashToken(token)}
	if err := m.retainLease(id, root); err != nil {
		return Lease{}, err
	}
	jobKind := "PREPARE"
	if sessionState == "RESTORING" {
		jobKind = "RESTORE"
	}
	job, err := m.store.CreateSlotSession(ctx, state.Slot{ID: id, WorkspaceID: string(w.ID), Generation: generation, Path: root, State: slotState}, repos, session, jobKind)
	if err != nil {
		m.releaseLease(id)
		return Lease{}, err
	}
	m.schedule(job)
	_ = m.enqueue("ENSURE_STANDBY", string(w.ID), "", "")
	m.startBackground(func() { _, _ = m.GC(m.ctx, false) })
	return Lease{SessionID: id, Token: token, Path: root, RootIdentity: rootIdentity, SourceWorkspace: string(w.Root), Ready: false}, nil
}

func (m *Manager) slotRoot(workspaceID, id string, unbound bool) (string, error) {
	root, err := config.ExpandHome(m.Config().Storage.WorktreeRoot)
	if err != nil {
		return "", err
	}
	if unbound {
		return filepath.Join(root, "unbound", id, "root"), nil
	}
	return filepath.Join(root, "workspaces", workspaceID, "slots", id, "root"), nil
}

var errManagerClosed = errors.New("daemon manager is closed")

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
	// A closed retired generation can no longer service a path-based operation.
	// Drop its lookup entry so repeated root rotations do not grow the active
	// pathname registry indefinitely. rootIdentities intentionally remains as a
	// small inode tombstone to reject replacement at a previously used path.
	if !m.roots[path] {
		delete(m.roots, path)
	}
}

// adoptRoot registers an opened descriptor and returns one reference to its
// caller. The descriptor is closed if another caller won the registration
// race or if shutdown started while it was being opened.
func (m *Manager) adoptRoot(path string, opened *os.Root, active bool) (*os.Root, func(), error) {
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

// existingRootDescriptor pins an already-created root without creating it.
// Readiness/reconcile checks must not recreate a missing root merely because a
// durable READY row still refers to it; allocation is the only path allowed to
// create a new root namespace.
func (m *Manager) existingRootDescriptor(root string) (*os.Root, func(), error) {
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

// createSlotRoot performs allocation through the manager's pinned worktree
// root. The returned identity is sent with the lease so the foreground client
// can reject a replacement before it opens its own descriptor.
func (m *Manager) createSlotRoot(path string) (string, error) {
	root, err := config.ExpandHome(m.Config().Storage.WorktreeRoot)
	if err != nil {
		return "", err
	}
	root = filepath.Clean(root)
	if !domain.IsWithin(root, path) {
		return "", fmt.Errorf("slot path %s is outside wx worktree root", path)
	}
	owner, closeOwner, err := m.rootDescriptor(root)
	if err != nil {
		return "", err
	}
	defer closeOwner()
	// The manager-held descriptor is the write/lease authority. Verify that
	// the configured pathname still names the same root before returning a
	// lease; if it was replaced, the client must not receive a path that could
	// resolve to a different namespace.
	if err := verifyRootDescriptorPath(root, owner); err != nil {
		return "", err
	}
	relative, ok := relativeWithinRoot(root, path)
	if !ok {
		return "", errors.New("slot path is outside wx worktree root")
	}
	m.mu.RLock()
	barrier := m.beforeSlotRootCreate
	m.mu.RUnlock()
	if barrier != nil {
		barrier()
	}
	if err := owner.MkdirAll(relative, 0o700); err != nil {
		return "", fmt.Errorf("create slot root safely: %w", err)
	}
	directory, identity, err := domain.OpenDirectoryAt(owner, relative)
	if err != nil {
		return "", fmt.Errorf("open allocated slot root: %w", err)
	}
	if err := directory.Close(); err != nil {
		return "", err
	}
	return identity, nil
}

func (m *Manager) leaseRootIdentity(path string) (string, error) {
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

// slotRepos builds the per-repository rows for a new slot. hot, when
// non-nil, restricts which repositories are actually checked out: a
// repository absent from hot is registered as COLD instead of PREPARING, so
// prepareSlot leaves it unmaterialized until a later lease pulls it in via
// the existing cold-materialize path (LeaseReadyWithCold). Pass nil for
// on-demand allocation, where every requested repository must be built
// immediately regardless of recent usage.
func (m *Manager) slotRepos(root string, w discovery.Workspace, resolved []pool.Resolved, generation int, hot map[string]bool) ([]state.SlotRepository, error) {
	out := make([]state.SlotRepository, 0, len(resolved))
	for _, r := range resolved {
		target := root
		if w.Kind == "multi_repository" {
			target = filepath.Join(root, r.Repository.RelativePath)
		}
		fp, err := workspace.Fingerprint(generation, r.OID, r.Repository, m.Config())
		if err != nil {
			return nil, err
		}
		repoState := "PREPARING"
		if hot != nil && !hot[string(r.Repository.ID)] {
			repoState = "COLD"
		}
		out = append(out, state.SlotRepository{RepositoryID: string(r.Repository.ID), WorktreePath: target, State: repoState, RequestedRef: r.RequestedRef, BaseOID: r.OID, Fingerprint: fp})
	}
	return out, nil
}

func (m *Manager) prepareSlot(ctx context.Context, id string, w discovery.Workspace, resolved []pool.Resolved, repos []state.SlotRepository) error {
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
	preparer := m.newPreparer(m.Config(), slot.Path)
	if len(repos) != len(resolved) {
		return errors.New("slot repository metadata does not match resolved workspace")
	}
	for _, r := range resolved {
		stored, err := m.store.SlotRepository(ctx, id, string(r.Repository.ID))
		if err != nil {
			return err
		}
		if stored.State == "COLD" {
			// Registered by ensureStandby's hot_standby filter: this
			// repository is outside the recently-used window, so the
			// replacement bundle leaves it unchecked-out. It is
			// materialized later by the existing cold-lease path
			// (LeaseReadyWithCold) instead of here.
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
			m.log.Error("slot preparation failed", "slot_id", id, "repository_id", r.Repository.ID, "error", err)
			if errors.Is(err, state.ErrOwnership) {
				_ = m.store.SetSlotState(context.Background(), id, []string{"PREPARING", "RESTORING"}, "QUARANTINED", "WORKTREE_OWNERSHIP_UNCERTAIN")
			} else {
				_ = m.store.SetSlotState(ctx, id, []string{"PREPARING", "RESTORING"}, "FAILED", "PREPARE_FAILED")
			}
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
		if err := m.store.ValidateSlotOwnership(context.Background(), state.SlotOwnershipRequest{SlotID: id, WorkspaceID: slot.WorkspaceID, Path: slot.Path, AllowedSlotStates: []string{"PREPARING", "RESTORING"}}); err != nil {
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
	releaseJob, released, err := m.store.FinishPreparationWithRelease(ctx, id)
	if err != nil {
		m.log.Error("finish preparation failed", "slot_id", id, "error", err)
		return err
	}
	if released {
		m.schedule(releaseJob)
	}
	return nil
}

// materializeWorkspaceRoot opens the slot root from the same manager-held
// descriptor used for allocation and repository worktrees. A path-based
// MaterializeRoot call here would reintroduce a root replacement window after
// the SQLite ownership check above.
func (m *Manager) materializeWorkspaceRoot(source, slotPath string, rules config.Workspace) error {
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
	preparer := m.newPreparer(m.Config(), s.Path)
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
			relative, ok := relativeWithinRoot(root, stored.WorktreePath)
			if !ok {
				return false, fmt.Errorf("%w: cold worktree path is outside wx root", state.ErrOwnership)
			}
			if filepath.Clean(stored.WorktreePath) == filepath.Clean(s.Path) {
				// A single-repository workspace has no bundle subdirectory:
				// the repository's worktree path is the slot root itself,
				// which createSlotRoot always creates before any
				// repository state is decided. Its mere existence proves
				// nothing; only an empty root means the repository was
				// genuinely left unchecked-out.
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
				if len(entries) != 0 {
					return false, nil
				}
				continue
			}
			if _, err := owner.Lstat(relative); err == nil {
				return false, nil
			} else if !errors.Is(err, os.ErrNotExist) {
				return false, fmt.Errorf("%w: inspect cold worktree path: %w", state.ErrOwnership, err)
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

func (m *Manager) ensureStandby(ctx context.Context, w discovery.Workspace) error {
	cfg := m.Config()
	if cfg.Pool.WarmPerWorkspace < 1 || cfg.Retention.HotStandby.Duration == 0 {
		return nil
	}
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
	// Only repositories used within the hot_standby window get an actual
	// checkout in the replacement bundle; the rest are registered COLD so
	// this prefetch does not `git worktree add` repositories the next GC
	// pass would immediately retire again.
	hotBefore := state.FormatTime(time.Now().UTC().Add(-cfg.Retention.HotStandby.Duration))
	hot, err := m.store.HotRepositoryIDs(ctx, hotBefore)
	if err != nil {
		return err
	}
	for range needed {
		id, err := domain.NewID()
		if err != nil {
			return err
		}
		root, err := m.slotRoot(string(w.ID), id, false)
		if err != nil {
			return err
		}
		if _, err := m.createSlotRoot(root); err != nil {
			return err
		}
		repos, err := m.slotRepos(root, w, resolved, generation, hot)
		if err != nil {
			return err
		}
		job, err := m.store.CreateStandby(ctx, state.Slot{ID: id, WorkspaceID: string(w.ID), Generation: generation, Path: root, State: "PREPARING"}, repos)
		if err != nil {
			return err
		}
		m.schedule(job)
	}
	return nil
}

func (m *Manager) AllocateResumeSlot(ctx context.Context, agent string, pid int) (Lease, error) {
	id, err := domain.NewID()
	if err != nil {
		return Lease{}, err
	}
	token, err := state.TokenHex()
	if err != nil {
		return Lease{}, err
	}
	root, err := m.slotRoot("", id, true)
	if err != nil {
		return Lease{}, err
	}
	releaseRoot, err := m.holdRootForPath(root)
	if err != nil {
		return Lease{}, err
	}
	defer releaseRoot()
	rootIdentity, err := m.createSlotRoot(root)
	if err != nil {
		return Lease{}, err
	}
	if err := m.retainLease(id, root); err != nil {
		return Lease{}, err
	}
	session := state.Session{ID: id, SlotID: id, State: "UNBOUND", AgentKind: agent, ClientPID: pid, TokenHash: state.HashToken(token)}
	if _, err := m.store.CreateSlotSession(ctx, state.Slot{ID: id, Generation: 0, Path: root, State: "UNBOUND"}, nil, session, ""); err != nil {
		m.releaseLease(id)
		return Lease{}, err
	}
	return Lease{SessionID: id, Token: token, Path: root, RootIdentity: rootIdentity}, nil
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
			return fmt.Errorf("workspace readiness failed: state=%s failure_id=%s; run `wx status` or `wx doctor` for details", slot.State, failureID)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
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
	generation, err := m.store.UpsertWorkspaceGeneration(ctx, w)
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
	_ = m.enqueue("ENSURE_STANDBY", string(w.ID), "", "")
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
		// The durable RESTORE job waits for the parent SNAPSHOT job to commit.
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
	archiveManager := m.newArchiveManager(m.Config(), slotState.Path)
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
			_ = m.store.SetSlotState(ctx, id, []string{"RESTORING"}, "QUARANTINED", "RESTORE_FAILED")
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
		if err := m.store.ValidateSlotOwnership(context.Background(), state.SlotOwnershipRequest{SlotID: id, WorkspaceID: slot.WorkspaceID, Path: slot.Path, AllowedSlotStates: []string{"RESTORING"}}); err != nil {
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
		if err := archive.RestoreWorkspaceAt(ctx, slot.Path, targetRoot, targetRootHandle, archiveRoot, archiveRootHandle, rootSnapshot, workspaceRecoveryExclusions(w, m.Config())); err != nil {
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

func workspaceRecoveryExclusions(w discovery.Workspace, cfg config.Config) []string {
	excluded := make([]string, 0, len(w.Repositories)+len(cfg.Workspaces[string(w.Root)].Link))
	for _, repository := range w.Repositories {
		excluded = append(excluded, repository.RelativePath)
	}
	excluded = append(excluded, cfg.Workspaces[string(w.Root)].Link...)
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
	// SessionEnd may run before the wrapped agent exits (for example during a
	// session switch). The foreground client is the authoritative process
	// owner; its client-exit notification or orphan reconciliation confirms that
	// no writer remains before snapshotting.
	if reason == "session-end-hook" && (processAlive(session.ClientPID) || processAlive(session.AgentPID)) {
		return nil
	}
	job, changed, err := m.store.Release(ctx, id, session.WorkspaceID, session.SlotID)
	if err != nil {
		return err
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
	archiveManager := m.newArchiveManager(m.Config(), slot.Path)
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
		ownershipRoot, ok := m.rootForPath(slot.Path)
		if !ok {
			return errors.New("workspace bundle is outside known wx roots")
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
			rootSnapshot, err = archive.SnapshotWorkspaceAt(ctx, slot.Path, ownershipRoot, ownershipRootHandle, s.ID, workspaceRecoveryExclusions(w, m.Config()), expiry)
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

func (m *Manager) GC(ctx context.Context, dry bool) (int, error) {
	cfg := m.Config()
	nowTime := time.Now().UTC()
	failedBefore := state.FormatTime(nowTime.Add(-cfg.Retention.FailedJob.Duration))
	eventBefore := state.FormatTime(nowTime.Add(-cfg.Retention.EventLog.Duration))
	tombstoneBefore := state.FormatTime(nowTime.Add(-cfg.Retention.ExpiredSessionTombstone.Duration))
	metadataCount, err := m.store.CountMetadataCandidates(ctx, failedBefore, eventBefore, tombstoneBefore)
	if err != nil {
		return 0, err
	}
	before := state.FormatTime(nowTime.Add(-cfg.Retention.EndedWorktree.Duration))
	items, err := m.store.GCCandidates(ctx, before)
	if err != nil {
		return 0, err
	}
	standbys, err := m.store.StandbyGCCandidates(ctx, state.FormatTime(nowTime.Add(-cfg.Retention.HotStandby.Duration)), cfg.Pool.WarmPerWorkspace)
	if err != nil {
		return 0, err
	}
	var cold []state.ColdRepositoryCandidate
	if cfg.Pool.WarmPerWorkspace > 0 {
		cold, err = m.store.ColdRepositoryCandidates(ctx, state.FormatTime(nowTime.Add(-cfg.Retention.HotStandby.Duration)))
		if err != nil {
			return 0, err
		}
	}
	expired, err := m.store.ExpiredSnapshots(ctx, state.FormatTime(nowTime))
	if err != nil {
		return 0, err
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
	total := metadataCount + len(items) + len(standbys) + len(expiredSessions) + totalCold
	if dry {
		return total, nil
	}
	// Highest GC priority tier runs first: TTL-expired events and finished
	// job metadata carry no physical worktree, so pruning them never
	// competes with the filesystem reclamation below for time or I/O.
	if err := m.store.PruneMetadata(ctx, failedBefore, eventBefore, tombstoneBefore); err != nil {
		return 0, err
	}
	count := metadataCount
	count += m.scheduleColdRepositoryRemovals(ctx, cold, wholeSlotRemoval)
	count += m.scheduleStandbyRemovals(ctx, standbys)
	count += m.scheduleEndedWorktreeRemovals(ctx, items)
	archiveManager := m.newArchiveManager(cfg, "")
	count += m.expireWorkspaceSnapshots(ctx, expiredSessions, &archiveManager)
	return count, nil
}

func (m *Manager) quarantineCleanupFailure(slotID string, runErr error) {
	if !errors.Is(runErr, state.ErrOwnership) {
		return
	}
	if quarantineErr := m.store.QuarantineMissingSlot(context.Background(), slotID, "WORKTREE_OWNERSHIP_UNCERTAIN"); quarantineErr != nil {
		m.log.Error("quarantine cleanup ownership failure", "slot_id", slotID, "error", quarantineErr)
	}
}

func (m *Manager) scheduleColdRepositoryRemovals(ctx context.Context, candidates []state.ColdRepositoryCandidate, wholeSlotRemoval map[string]bool) int {
	count := 0
	for _, candidate := range candidates {
		if wholeSlotRemoval[candidate.SlotID] {
			continue
		}
		if _, release, err := m.holdVerifiedRootForPath(candidate.WorktreePath); err != nil {
			m.quarantineCleanupFailure(candidate.SlotID, err)
			continue
		} else {
			release()
		}
		job, changed, err := m.store.ScheduleColdRepositoryRemoval(ctx, candidate)
		if err != nil {
			m.log.Error("cold repository removal scheduling failed", "slot_id", candidate.SlotID, "repository_id", candidate.RepositoryID, "error", err)
			continue
		}
		if changed {
			m.schedule(job)
			count++
		}
	}
	return count
}

func (m *Manager) scheduleStandbyRemovals(ctx context.Context, candidates []state.StandbyGCCandidate) int {
	count := 0
	for _, candidate := range candidates {
		count += m.scheduleRemovalCandidate(ctx, candidate.SlotID, candidate.Path, "", "standby removal scheduling failed")
	}
	return count
}

func (m *Manager) scheduleEndedWorktreeRemovals(ctx context.Context, candidates []state.GCCandidate) int {
	count := 0
	for _, candidate := range candidates {
		count += m.scheduleRemovalCandidate(ctx, candidate.SlotID, candidate.Path, candidate.SessionID, "ended worktree removal scheduling failed")
	}
	return count
}

func (m *Manager) scheduleRemovalCandidate(ctx context.Context, slotID, path, sessionID, logMessage string) int {
	if _, release, err := m.holdVerifiedRootForPath(path); err != nil {
		m.quarantineCleanupFailure(slotID, err)
		return 0
	} else {
		release()
	}
	job, changed, err := m.store.ScheduleRemoval(ctx, slotID, sessionID)
	if err != nil {
		m.log.Error(logMessage, "slot_id", slotID, "error", err)
		return 0
	}
	if !changed {
		return 0
	}
	m.schedule(job)
	return 1
}

func (m *Manager) expireWorkspaceSnapshots(ctx context.Context, expiredSessions map[string][]state.Snapshot, archiveManager *archive.Manager) int {
	count := 0
	for sessionID, snapshots := range expiredSessions {
		ok := true
		var rootSnapshot state.WorkspaceSnapshot
		var rootSnapshotOwner string
		var rootSnapshotOwnerHandle *os.Root
		var rootSnapshotOwnerRelease func()
		workspaceKind, workspaceErr := m.store.SessionWorkspaceKind(ctx, sessionID)
		if workspaceErr != nil {
			ok = false
		} else if workspaceKind == "multi_repository" {
			var found bool
			rootSnapshot, found, workspaceErr = m.store.WorkspaceSnapshot(ctx, sessionID)
			if workspaceErr != nil || !found {
				ok = false
			} else if owner, releaseOwner, ownerErr := m.holdVerifiedRootForPath(rootSnapshot.ArchivePath); ownerErr != nil {
				_ = m.store.QuarantineArtifact(context.Background(), "workspace_snapshot", rootSnapshot.ArchivePath, "ownership could not be proven during cleanup")
				ok = false
			} else {
				rootSnapshotOwner = owner
				rootSnapshotOwnerRelease = releaseOwner
				rootSnapshotOwnerHandle = m.rootHandleForRoot(owner)
				if rootSnapshotOwnerHandle == nil {
					ok = false
				} else {
					ok = archive.ValidateWorkspaceSnapshotAt(owner, rootSnapshotOwnerHandle, rootSnapshot, time.Time{}) == nil
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
			if err != nil || archiveManager.DeleteSnapshotRefs(ctx, repo, snapshot) != nil {
				ok = false
				break
			}
		}
		if ok && rootSnapshot.SessionID != "" {
			ok = archive.DeleteWorkspaceSnapshotAt(rootSnapshotOwner, rootSnapshotOwnerHandle, rootSnapshot) == nil
		}
		if ok {
			if err := m.store.ExpireSessionSnapshots(ctx, sessionID); err == nil {
				count++
			}
		}
		if rootSnapshotOwnerRelease != nil {
			rootSnapshotOwnerRelease()
		}
	}
	return count
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
	archiveManager := m.newArchiveManager(m.Config(), slot.Path)
	if err := m.removeSlotWorktrees(ctx, archiveManager, root, slot.ID, job.SessionID, slot.Path); err != nil {
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
	archiveManager := m.newArchiveManager(m.Config(), slot.Path)
	if err := archiveManager.RemoveWorktree(ctx, repository, root, repositoryState.WorktreePath, repositoryState.BaseOID); err != nil {
		m.quarantineOwnershipFailure(slot.ID, []string{"RETIRING"}, err)
		return err
	}
	if filepath.Clean(repositoryState.WorktreePath) == filepath.Clean(slot.Path) {
		if err := m.store.ValidateSlotOwnership(context.Background(), state.SlotOwnershipRequest{SlotID: slot.ID, WorkspaceID: slot.WorkspaceID, Path: slot.Path, AllowedSlotStates: []string{"RETIRING"}}); err != nil {
			m.quarantineOwnershipFailure(slot.ID, []string{"RETIRING"}, err)
			return err
		}
		ownedRoot, closeOwnedRoot, err := m.existingRootDescriptor(root)
		if err != nil {
			return fmt.Errorf("%w: recreate cold workspace shell: %w", state.ErrOwnership, err)
		}
		defer closeOwnedRoot()
		if err := verifyRootDescriptorPath(root, ownedRoot); err != nil {
			return err
		}
		relativeSlot, ok := relativeWithinRoot(root, slot.Path)
		if !ok || relativeSlot == "." {
			return fmt.Errorf("%w: recreate cold workspace shell: slot path is outside wx root", state.ErrOwnership)
		}
		if err := ownedRoot.MkdirAll(relativeSlot, 0o700); err != nil {
			return fmt.Errorf("recreate cold workspace shell: %w", err)
		}
		if err := verifyRootDescriptorPath(root, ownedRoot); err != nil {
			return err
		}
	}
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
	m.mu.RLock()
	defer m.mu.RUnlock()
	path = filepath.Clean(path)
	// Retired and active roots may overlap after a configuration reload. Pick
	// the most-specific root so a slot below a nested root uses that root's
	// pinned descriptor instead of depending on randomized map iteration.
	best := ""
	for root := range m.roots {
		if domain.IsWithin(root, path) {
			if best == "" || len(root) > len(best) {
				best = root
			}
		}
	}
	// A closed retired descriptor is removed from m.roots to keep the active
	// pathname registry bounded, but its inode tombstone remains durable for
	// this manager lifetime. Recovery archives and delayed removal jobs can
	// outlive the lease that kept their root open, so retain the historical
	// pathname as a candidate. Callers that need filesystem access still go
	// through existingRootDescriptor, which reopens it only if the pathname
	// names the same physical inode; a replacement therefore fails closed.
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

// rootHandleForPath borrows a descriptor whose lifetime is owned by the
// caller's surrounding holdRootForPath reference. It never increments or
// releases a reference itself; returning a descriptor without an enclosing
// hold would allow a reload to close it while a Preparer still uses it.
func (m *Manager) rootHandleForPath(path string) *os.Root {
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

// ownedPathExists checks an artifact through the descriptor generation that
// owns its lexical root. A missing or replaced historical root is reported as
// an ownership error, never as an ordinary missing path that reconciliation
// could quarantine automatically.
func (m *Manager) ownedPathExists(path string) (bool, error) {
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

func (m *Manager) ownedRootArtifactPaths(root string) ([]string, error) {
	root = filepath.Clean(root)
	m.mu.RLock()
	active, known := m.roots[root]
	m.mu.RUnlock()
	var release func()
	var err error
	if known && active {
		_, release, err = m.rootDescriptor(root)
	} else {
		// This function is invoked once per exact root-generation entry from
		// artifactDiagnostics. Do not resolve the probe through rootForPath:
		// an overlapping nested root could otherwise pin a different generation
		// while rootHandleForRoot below borrows this one.
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
	entries, err := fs.ReadDir(owner.FS(), "workspaces")
	if errors.Is(err, os.ErrNotExist) {
		entries = nil
	} else if err != nil {
		return nil, fmt.Errorf("%w: inspect workspaces namespace: %w", state.ErrOwnership, err)
	}
	paths := make([]string, 0)
	for _, workspaceEntry := range entries {
		if workspaceEntry.Type()&os.ModeSymlink != 0 || !workspaceEntry.IsDir() {
			continue
		}
		slotsRelative := path.Join("workspaces", workspaceEntry.Name(), "slots")
		slots, readErr := fs.ReadDir(owner.FS(), slotsRelative)
		if errors.Is(readErr, os.ErrNotExist) {
			continue
		}
		if readErr != nil {
			return nil, fmt.Errorf("%w: inspect workspace slots: %w", state.ErrOwnership, readErr)
		}
		for _, slotEntry := range slots {
			if slotEntry.Type()&os.ModeSymlink != 0 || !slotEntry.IsDir() {
				continue
			}
			slotRootRelative := path.Join(slotsRelative, slotEntry.Name(), "root")
			rootInfo, infoErr := owner.Lstat(filepath.FromSlash(slotRootRelative))
			if errors.Is(infoErr, os.ErrNotExist) {
				continue
			}
			if infoErr != nil {
				return nil, fmt.Errorf("%w: inspect slot root: %w", state.ErrOwnership, infoErr)
			}
			if rootInfo.IsDir() {
				paths = append(paths, filepath.Join(root, "workspaces", workspaceEntry.Name(), "slots", slotEntry.Name(), "root"))
			}
		}
	}
	unboundEntries, err := fs.ReadDir(owner.FS(), "unbound")
	if errors.Is(err, os.ErrNotExist) {
		unboundEntries = nil
	} else if err != nil {
		return nil, fmt.Errorf("%w: inspect unbound namespace: %w", state.ErrOwnership, err)
	}
	for _, entry := range unboundEntries {
		if entry.Type()&os.ModeSymlink != 0 || !entry.IsDir() {
			continue
		}
		relative := path.Join("unbound", entry.Name(), "root")
		info, infoErr := owner.Lstat(filepath.FromSlash(relative))
		if errors.Is(infoErr, os.ErrNotExist) {
			continue
		}
		if infoErr != nil {
			return nil, fmt.Errorf("%w: inspect unbound root: %w", state.ErrOwnership, infoErr)
		}
		if info.IsDir() {
			paths = append(paths, filepath.Join(root, "unbound", entry.Name(), "root"))
		}
	}
	return paths, nil
}

// holdRootForPath acquires a reference for the complete lifetime of one
// synchronous operation. In particular, a worker that starts before reload
// keeps the retired root alive until its preparer/archive call returns.
func (m *Manager) holdRootForPath(path string) (func(), error) {
	root, ok := m.rootForPath(path)
	if !ok {
		configured, expandErr := config.ExpandHome(m.Config().Storage.WorktreeRoot)
		if expandErr != nil {
			return func() {}, fmt.Errorf("%w: resolve configured wx root: %w", state.ErrOwnership, expandErr)
		}
		if !domain.IsWithin(configured, path) {
			// Repository worktrees and other archive inputs may intentionally live
			// outside wx's managed root. Such path-based callers do not need a root
			// reference; their own ownership checks decide whether they are usable.
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

// holdVerifiedRootForPath is used by delayed cleanup jobs. A path may still
// belong to a closed historical generation, so rootForPath alone is not
// sufficient; the descriptor and its current lexical pathname must both be
// validated before a job is allowed to transition durable state to REMOVING.
func (m *Manager) holdVerifiedRootForPath(path string) (string, func(), error) {
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

// retainLease transfers one root reference from the allocation/validation
// operation to the durable foreground lease. A retired root therefore remains
// pinned while the client can still release or use that session.
func (m *Manager) retainLease(sessionID, path string) error {
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

func (m *Manager) removeSlotWorktrees(ctx context.Context, archiveManager archive.Manager, root, slotID, sessionID, slotPath string) error {
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
	if err := m.store.ValidateSlotOwnership(context.Background(), state.SlotOwnershipRequest{SlotID: slotID, Path: slotPath, AllowedSlotStates: []string{"REMOVING"}}); err != nil {
		return err
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
	info, err := ownedRoot.Lstat(relativeSlot)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return fmt.Errorf("%w: inspect slot root for removal: %w", state.ErrOwnership, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("%w: slot root is not a physical directory", state.ErrOwnership)
	}
	// Root.RemoveAll does not follow symlink leaves and cannot traverse outside
	// the descriptor-owned root. It also removes bundle rules and empty nested
	// repository parents left after the registered worktrees are removed.
	if err := ownedRoot.RemoveAll(relativeSlot); err != nil {
		return err
	}
	return verifyRootDescriptorPath(root, ownedRoot)
}

// relativeWithinRoot resolves path relative to root and reports whether the
// result stays inside root. filepath.IsLocal rejects "..", an absolute
// result, and any unclean input in one call, replacing the
// rel == ".." || strings.HasPrefix(rel, "../") || filepath.IsAbs(rel) guard
// this package used to repeat at every root-relative path computation.
func relativeWithinRoot(root, path string) (relative string, ok bool) {
	relative, err := filepath.Rel(filepath.Clean(root), filepath.Clean(path))
	if err != nil || !filepath.IsLocal(relative) {
		return "", false
	}
	return relative, true
}

// verifyRootDescriptorPath ensures a descriptor-bound operation has not
// detached its durable lexical root from the inode the manager owns. The
// descriptor keeps a replacement from redirecting the syscall; this check
// keeps the caller from committing state for a pathname that now names a
// different namespace.
func verifyRootDescriptorPath(path string, owner *os.Root) error {
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
	restartPending := m.restartPending
	cfg := m.cfg
	roots := make(map[string]bool, len(m.roots))
	for root, active := range m.roots {
		roots[root] = active
	}
	m.mu.RUnlock()
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
		"config_path": must(config.Path()), "config_last_reload": reloadAt.UTC().Format(time.RFC3339Nano), "config_reload_error": reloadError,
		"sqlite_last_backup": formatOptionalTime(backupAt), "sqlite_backup_error": backupError, "restart_pending": restartPending,
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

// rootDirectoryUsage measures a root through its pinned descriptor. A
// pathname walk could cross into a replacement directory after reload and
// report bytes for a namespace that wx does not own, so status uses the same
// generation and reference accounting as mutating operations.
func (m *Manager) rootDirectoryUsage(root string) (int64, int64, error) {
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
	checks := map[string]any{}
	m.mu.RLock()
	reloadError := m.reloadError
	m.mu.RUnlock()
	if reloadError == "" {
		checks["config"] = "ok"
	} else {
		checks["config"] = reloadError
	}
	if _, err := m.git.Run(ctx, "", "--version"); err != nil {
		checks["git"] = err.Error()
	} else {
		checks["git"] = "ok"
	}
	if err := m.store.Ping(ctx); err != nil {
		checks["sqlite"] = err.Error()
	} else {
		checks["sqlite"] = "ok"
	}
	checks["socket"] = diagnosticPath(must(config.SocketPath()), os.ModeSocket, 0o600)
	checks["state_database"] = diagnosticPath(must(config.StatePath()), 0, 0o600)
	checks["launch_agent"] = diagnosticPath(must(launchd.PlistPath()), 0, 0o600)
	root, rootErr := config.ExpandHome(m.Config().Storage.WorktreeRoot)
	if rootErr != nil {
		checks["worktree_root"] = rootErr.Error()
	} else {
		checks["worktree_root"] = diagnosticPath(root, os.ModeDir, 0o700)
	}
	hooks := map[string]string{}
	for _, agentKind := range []string{"claude", "codex"} {
		if hookconfig.Available(agentKind) {
			hooks[agentKind] = "ok"
		} else {
			hooks[agentKind] = "missing or invalid readiness hooks; foreground readiness remains required"
		}
	}
	checks["hooks"] = hooks
	checks["worktree_registration"] = m.registrationDiagnostics(ctx)
	checks["artifact_ownership"] = m.artifactDiagnostics(ctx)
	return map[string]any{"schema_version": state.JSONSchemaVersion, "db_schema_version": state.SchemaVersion, "checks": checks}
}

func diagnosticPath(path string, requiredType os.FileMode, requiredPerm os.FileMode) string {
	if path == "" {
		return "path unavailable"
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err.Error()
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return "unsafe symlink"
	}
	if requiredType == os.ModeDir && !info.IsDir() {
		return "not a directory"
	}
	if requiredType == os.ModeSocket && info.Mode()&os.ModeSocket == 0 {
		return "not a Unix socket"
	}
	if requiredType == 0 && !info.Mode().IsRegular() {
		return "not a regular file"
	}
	if info.Mode().Perm() != requiredPerm {
		return fmt.Sprintf("unsafe permissions %04o; expected %04o", info.Mode().Perm(), requiredPerm)
	}
	return "ok"
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
	// FAILED slots are retired before ForgetWorkspace's own safety check runs
	// (which now refuses them too): otherwise clearing workspace_id would
	// leave a FAILED slot's physical worktree with no way to ever prove
	// ownership again, permanently leaking it instead of removing it.
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

// retireFailedSlotForForget physically removes a FAILED slot's worktree so
// Forget's caller does not have to reason about it. If removal cannot
// complete immediately (e.g. a transient fault), the slot is left REMOVING
// with its REMOVE job persisted; the daemon's normal job recovery retries it
// in the background, and ForgetWorkspace continues to refuse the workspace
// (REMOVING is not ARCHIVED) until it converges, exactly as for any other
// in-flight removal.
func (m *Manager) retireFailedSlotForForget(ctx context.Context, slotID string) error {
	job, changed, err := m.store.ScheduleFailedSlotRemoval(ctx, slotID)
	if err != nil {
		return err
	}
	if !changed {
		// Already resolved concurrently (removed, retried, or re-quarantined).
		return nil
	}
	// Claim and finish the job exactly as the normal worker loop would
	// (runWorker), so it does not linger as a PENDING job that
	// ForgetWorkspace's own "pending recovery jobs" check would then refuse.
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
		if err := m.store.DrainRoot(context.Background(), oldRoot); err != nil {
			if newHandle != nil {
				_ = newHandle.Close()
			}
			m.lastReload = time.Now()
			m.reloadError = err.Error()
			m.mu.Unlock()
			return fmt.Errorf("drain retired worktree root: %w", err)
		}
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
	m.resizeWorkers(cfg.Pool.PreparationConcurrency)
	select {
	case m.reloads <- struct{}{}:
	default:
	}
	if runGC {
		m.startBackground(func() {
			m.reconcileRegistry(m.ctx)
			_, _ = m.GC(m.ctx, false)
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
