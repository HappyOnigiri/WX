package daemon

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
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
	rootHandles map[string]*os.Root
	// beforeSlotRootCreate is a deterministic adversarial-test barrier. It is
	// invoked after the pinned root descriptor and relative namespace are ready
	// but before the first descriptor-relative mkdir. Production managers leave
	// it nil.
	beforeSlotRootCreate func()
	jobs                 chan jobWork
	reloads              chan struct{}
	reclaimAll           bool
	ctx                  context.Context
	cancel               context.CancelFunc
	wg                   sync.WaitGroup
	workersMu            sync.Mutex
	workerStops          []chan struct{}
	workerSeq            int
	closed               bool
	logLevel             *slog.LevelVar
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
	if executable, err := os.Executable(); err == nil {
		git.FDHelper = executable
	}
	started := time.Now()
	managerCtx, managerCancel := context.WithCancel(context.Background())
	reclaimAll := len(exclusiveStartup) > 0 && exclusiveStartup[0]
	m := &Manager{cfg: cfg, store: store, git: git, log: logger, started: started, lastReload: started, roots: map[string]bool{}, rootHandles: map[string]*os.Root{}, jobs: make(chan jobWork, 256), reloads: make(chan struct{}, 1), reclaimAll: reclaimAll, ctx: managerCtx, cancel: managerCancel}
	if root, ownedRoot, err := ensureWorktreeRootDescriptor(cfg.Storage.WorktreeRoot); err == nil {
		m.roots[root] = true
		m.rootHandles[root] = ownedRoot
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
	m.workersMu.Lock()
	m.closed = true
	m.workersMu.Unlock()
	if m.cancel != nil {
		m.cancel()
	}
	m.wg.Wait()
	m.mu.Lock()
	for root, handle := range m.rootHandles {
		if handle != nil {
			_ = handle.Close()
		}
		delete(m.rootHandles, root)
	}
	m.mu.Unlock()
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
		go func() {
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
		}()
		err = m.runRecoveredJob(jobCtx, job)
		close(done)
		cancel()
		var pending dependencyPendingError
		if errors.As(err, &pending) {
			delay := 5 * time.Second
			if deferErr := m.store.DeferJob(context.Background(), work.id, owner, delay, "DEPENDENCY_PENDING"); deferErr != nil {
				m.log.Error("defer dependency-bound job failed", "job_id", work.id, "error", deferErr)
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
				continue
			}
			delay := time.Duration(1<<min(job.Attempt, 6)) * time.Second
			if retryErr := m.store.RetryJob(context.Background(), work.id, owner, delay, "DEPENDENCY_PENDING"); retryErr != nil {
				m.log.Error("reschedule job failed", "job_id", work.id, "error", retryErr)
			} else {
				m.scheduleDelayed(job, delay)
			}
			continue
		}
		if finishErr := m.store.FinishJob(context.Background(), work.id, owner, err); finishErr != nil {
			m.log.Error("finish job failed", "job_id", work.id, "error", finishErr)
		}
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
	if root, ok := m.rootForPath(slotPath); ok {
		cfg.Storage.WorktreeRoot = root
	}
	root, err := config.ExpandHome(cfg.Storage.WorktreeRoot)
	var ownedRoot *os.Root
	if err == nil {
		m.mu.RLock()
		ownedRoot = m.rootHandles[filepath.Clean(root)]
		m.mu.RUnlock()
		if ownedRoot == nil {
			ownedRoot, _, _ = m.rootDescriptor(root)
		}
	}
	return &workspace.Preparer{Git: m.git, Config: cfg, Ownership: m.store, SlotPath: slotPath, OwnedRoot: ownedRoot, RootPath: filepath.Clean(root), RequireOwnedRoot: true}
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
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		timer := time.NewTimer(delay)
		defer timer.Stop()
		select {
		case <-m.ctx.Done():
		case <-timer.C:
			m.schedule(job)
		}
	}()
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
		case <-ticker.C:
			m.recoverJobs(false)
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
			if _, statErr := os.Lstat(artifact.Path); errors.Is(statErr, os.ErrNotExist) {
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
		if _, statErr := os.Lstat(clean); errors.Is(statErr, os.ErrNotExist) {
			missingPaths = append(missingPaths, fmt.Sprintf("%s (%s, %s)", clean, artifact.ID, artifact.State))
		} else if statErr != nil {
			diagnosticErrors = append(diagnosticErrors, fmt.Sprintf("inspect slot %s: %v", artifact.ID, statErr))
		}
	}
	m.mu.RLock()
	roots := make([]string, 0, len(m.roots))
	for root := range m.roots {
		roots = append(roots, root)
	}
	m.mu.RUnlock()
	for _, root := range roots {
		for _, pattern := range []string{filepath.Join(root, "workspaces", "*", "slots", "*", "root"), filepath.Join(root, "unbound", "*", "root")} {
			matches, globErr := filepath.Glob(pattern)
			if globErr != nil {
				diagnosticErrors = append(diagnosticErrors, globErr.Error())
				continue
			}
			for _, path := range matches {
				clean := filepath.Clean(path)
				if _, exists := expectedPaths[clean]; !exists {
					unknownPaths = append(unknownPaths, clean)
				}
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
		return discovery.Workspace{}, fmt.Errorf("rediscover workspace root %s: %w; common-directory recovery failed: %v", root, err, commonErr)
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
		repos, err = m.slotRepos(slot.Path, w, resolved, slot.Generation)
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
	if len(branches) == 0 {
		for attempts := 0; attempts < m.Config().Pool.WarmPerWorkspace+1; attempts++ {
			ready, ok, err := m.store.ReadySlot(ctx, string(w.ID))
			if err != nil {
				return Lease{}, err
			}
			if !ok {
				break
			}
			valid, err := m.readyMatches(ctx, ready, resolved)
			if err != nil {
				if errors.Is(err, state.ErrOwnership) {
					m.quarantineOwnershipFailure(ready.ID, []string{"READY"}, err)
				}
				return Lease{}, err
			}
			if valid {
				rootIdentity, identityErr := m.leaseRootIdentity(ready.Path)
				if identityErr != nil {
					_ = m.store.SetSlotState(context.Background(), ready.ID, []string{"READY"}, "QUARANTINED", "LEASE_ROOT_OWNERSHIP_UNCERTAIN")
					return Lease{}, fmt.Errorf("pin ready lease root: %w", identityErr)
				}
				token, err := state.TokenHex()
				if err != nil {
					return Lease{}, err
				}
				repositories, err := m.store.SlotRepositories(ctx, ready.ID)
				if err != nil {
					return Lease{}, err
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
				if hasCold {
					job, leaseErr := m.store.LeaseReadyWithCold(ctx, ready.ID, session)
					if leaseErr == nil {
						m.schedule(job)
						_ = m.enqueue("ENSURE_STANDBY", string(w.ID), "", "")
						return Lease{SessionID: session.ID, Token: token, Path: ready.Path, RootIdentity: rootIdentity, SourceWorkspace: string(w.Root), Ready: false}, nil
					}
					continue
				}
				if err := m.store.LeaseReady(ctx, ready.ID, session); err == nil {
					_ = m.enqueue("ENSURE_STANDBY", string(w.ID), "", "")
					return Lease{SessionID: session.ID, Token: token, Path: ready.Path, RootIdentity: rootIdentity, SourceWorkspace: string(w.Root), Ready: true}, nil
				}
				continue
			}
			_ = m.store.SetSlotState(ctx, ready.ID, []string{"READY"}, "STALE", "READY_VALIDATION_FAILED")
		}
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
	rootIdentity, err := m.createSlotRoot(root)
	if err != nil {
		return Lease{}, err
	}
	repos, err := m.slotRepos(root, w, resolved, generation)
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
	jobKind := "PREPARE"
	if sessionState == "RESTORING" {
		jobKind = "RESTORE"
	}
	job, err := m.store.CreateSlotSession(ctx, state.Slot{ID: id, WorkspaceID: string(w.ID), Generation: generation, Path: root, State: slotState}, repos, session, jobKind)
	if err != nil {
		return Lease{}, err
	}
	m.schedule(job)
	_ = m.enqueue("ENSURE_STANDBY", string(w.ID), "", "")
	go func() { _, _ = m.GC(m.ctx, false) }()
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

func (m *Manager) rootDescriptor(root string) (*os.Root, func(), error) {
	root = filepath.Clean(root)
	m.mu.RLock()
	ownedRoot := m.rootHandles[root]
	m.mu.RUnlock()
	if ownedRoot != nil {
		return ownedRoot, func() {}, nil
	}
	_, ownedRoot, err := ensureWorktreeRootDescriptor(root)
	if err != nil {
		return nil, func() {}, err
	}
	// Keep the first descriptor for the manager lifetime. This also covers
	// managers constructed by tests/recovery code without going through New;
	// returning a short-lived descriptor here would reopen the root on every
	// operation and reintroduce the rename/replacement window between calls.
	m.mu.Lock()
	if m.rootHandles == nil {
		m.rootHandles = map[string]*os.Root{}
	}
	if m.roots == nil {
		m.roots = map[string]bool{}
	}
	if existing := m.rootHandles[root]; existing != nil {
		m.mu.Unlock()
		_ = ownedRoot.Close()
		return existing, func() {}, nil
	}
	m.rootHandles[root] = ownedRoot
	m.roots[root] = true
	m.mu.Unlock()
	return ownedRoot, func() {}, nil
}

// existingRootDescriptor pins an already-created root without creating it.
// Readiness/reconcile checks must not recreate a missing root merely because a
// durable READY row still refers to it; allocation is the only path allowed to
// create a new root namespace.
func (m *Manager) existingRootDescriptor(root string) (*os.Root, func(), error) {
	root = filepath.Clean(root)
	m.mu.RLock()
	ownedRoot := m.rootHandles[root]
	m.mu.RUnlock()
	if ownedRoot != nil {
		return ownedRoot, func() {}, nil
	}
	ownedRoot, _, err := domain.OpenOwnedRoot(root, root)
	if err != nil {
		return nil, func() {}, err
	}
	m.mu.Lock()
	if m.rootHandles == nil {
		m.rootHandles = map[string]*os.Root{}
	}
	if existing := m.rootHandles[root]; existing != nil {
		m.mu.Unlock()
		_ = ownedRoot.Close()
		return existing, func() {}, nil
	}
	m.rootHandles[root] = ownedRoot
	if m.roots == nil {
		m.roots = map[string]bool{}
	}
	m.roots[root] = true
	m.mu.Unlock()
	return ownedRoot, func() {}, nil
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
	currentRoot, _, currentErr := domain.OpenOwnedRoot(root, root)
	if currentErr != nil {
		return "", fmt.Errorf("configured worktree root changed: %w", currentErr)
	}
	defer func() { _ = currentRoot.Close() }()
	heldInfo, heldErr := owner.Lstat(".")
	currentInfo, currentInfoErr := currentRoot.Lstat(".")
	if heldErr != nil || currentInfoErr != nil || !os.SameFile(heldInfo, currentInfo) {
		if heldErr != nil {
			return "", fmt.Errorf("inspect pinned worktree root: %w", heldErr)
		}
		if currentInfoErr != nil {
			return "", fmt.Errorf("inspect configured worktree root: %w", currentInfoErr)
		}
		return "", errors.New("configured worktree root changed")
	}
	relative, err := filepath.Rel(root, filepath.Clean(path))
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.IsAbs(relative) {
		if err == nil {
			err = errors.New("slot path is outside wx worktree root")
		}
		return "", err
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

func (m *Manager) slotRepos(root string, w discovery.Workspace, resolved []pool.Resolved, generation int) ([]state.SlotRepository, error) {
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
		out = append(out, state.SlotRepository{RepositoryID: string(r.Repository.ID), WorktreePath: target, State: "PREPARING", RequestedRef: r.RequestedRef, BaseOID: r.OID, Fingerprint: fp})
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
	preparer := m.newPreparer(m.Config(), slot.Path)
	if len(repos) != len(resolved) {
		return errors.New("slot repository metadata does not match resolved workspace")
	}
	for _, r := range resolved {
		stored, err := m.store.SlotRepository(ctx, id, string(r.Repository.ID))
		if err != nil {
			return err
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
		if _, err := m.store.ValidateSlotOwnership(context.Background(), state.SlotOwnershipRequest{SlotID: id, WorkspaceID: slot.WorkspaceID, Path: slot.Path, AllowedSlotStates: []string{"PREPARING", "RESTORING"}}); err != nil {
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
		return fmt.Errorf("%w: open slot root namespace: %v", state.ErrOwnership, err)
	}
	defer closeOwner()
	if err := verifyRootDescriptorPath(root, owner); err != nil {
		return err
	}
	relative, err := filepath.Rel(filepath.Clean(root), filepath.Clean(slotPath))
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.IsAbs(relative) {
		if err == nil {
			err = errors.New("slot path is outside wx root")
		}
		return fmt.Errorf("%w: %v", state.ErrOwnership, err)
	}
	destination, err := domain.OpenRootAt(owner, relative)
	if err != nil {
		return fmt.Errorf("%w: open slot root namespace: %v", state.ErrOwnership, err)
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
		root = filepath.Clean(configured)
	}
	owner, closeOwner, err := m.existingRootDescriptor(root)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("%w: open ready slot root: %v", state.ErrOwnership, err)
	}
	defer closeOwner()
	relativeSlot, err := filepath.Rel(filepath.Clean(root), filepath.Clean(s.Path))
	if err != nil || relativeSlot == ".." || strings.HasPrefix(relativeSlot, ".."+string(filepath.Separator)) || filepath.IsAbs(relativeSlot) {
		if err == nil {
			err = errors.New("ready slot path is outside wx root")
		}
		return false, fmt.Errorf("%w: %v", state.ErrOwnership, err)
	}
	slotInfo, err := owner.Lstat(relativeSlot)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("%w: open ready slot root: %v", state.ErrOwnership, err)
	}
	if slotInfo.Mode()&os.ModeSymlink != 0 || !slotInfo.IsDir() {
		return false, nil
	}
	slotDirectory, _, err := domain.OpenDirectoryAt(owner, relativeSlot)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("%w: open ready slot root: %v", state.ErrOwnership, err)
	}
	if err := slotDirectory.Close(); err != nil {
		return false, fmt.Errorf("%w: close ready slot root: %v", state.ErrOwnership, err)
	}
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
			relative, relErr := filepath.Rel(filepath.Clean(root), filepath.Clean(stored.WorktreePath))
			if relErr != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.IsAbs(relative) {
				return false, fmt.Errorf("%w: cold worktree path is outside wx root", state.ErrOwnership)
			}
			if _, err := owner.Lstat(relative); err == nil {
				return false, nil
			} else if !errors.Is(err, os.ErrNotExist) {
				return false, fmt.Errorf("%w: inspect cold worktree path: %v", state.ErrOwnership, err)
			}
			continue
		}
		relative, relErr := filepath.Rel(filepath.Clean(root), filepath.Clean(stored.WorktreePath))
		if relErr != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.IsAbs(relative) {
			return false, fmt.Errorf("%w: ready worktree path is outside wx root", state.ErrOwnership)
		}
		info, err := owner.Lstat(relative)
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		if err != nil {
			return false, fmt.Errorf("%w: inspect ready worktree path: %v", state.ErrOwnership, err)
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return false, nil
		}
		directory, _, openErr := domain.OpenDirectoryAt(owner, relative)
		if openErr != nil {
			return false, fmt.Errorf("%w: open ready worktree path: %v", state.ErrOwnership, openErr)
		}
		if closeErr := directory.Close(); closeErr != nil {
			return false, fmt.Errorf("%w: close ready worktree path: %v", state.ErrOwnership, closeErr)
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
		repos, err := m.slotRepos(root, w, resolved, generation)
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
	rootIdentity, err := m.createSlotRoot(root)
	if err != nil {
		return Lease{}, err
	}
	session := state.Session{ID: id, SlotID: id, State: "UNBOUND", AgentKind: agent, ClientPID: pid, TokenHash: state.HashToken(token)}
	if _, err := m.store.CreateSlotSession(ctx, state.Slot{ID: id, Generation: 0, Path: root, State: "UNBOUND"}, nil, session, ""); err != nil {
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
			return fmt.Errorf("workspace readiness failed: %s", slot.State)
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

func (m *Manager) ValidateFreshResume(ctx context.Context, id, token, agentID string) error {
	return m.PrepareFreshResume(ctx, id, token, agentID, "", nil)
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
	repositories, err := m.slotRepos(slot.Path, w, resolved, generation)
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
		if _, err := m.store.ValidateSlotOwnership(context.Background(), state.SlotOwnershipRequest{SlotID: id, WorkspaceID: slot.WorkspaceID, Path: slot.Path, AllowedSlotStates: []string{"RESTORING"}}); err != nil {
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
	return m.store.FinishPreparation(ctx, id)
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
	owner, _, err := m.existingRootDescriptor(root)
	if err != nil {
		return false, fmt.Errorf("open workspace snapshot owner: %w", err)
	}
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
	if err != nil || !changed {
		return err
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
	w, err := m.store.SessionWorkspace(ctx, s.ID)
	if err != nil {
		return err
	}
	if w.Kind == "multi_repository" {
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
	total := len(items) + len(standbys) + len(expiredSessions) + totalCold
	if dry {
		return total, nil
	}
	count := 0
	for _, candidate := range cold {
		if wholeSlotRemoval[candidate.SlotID] {
			continue
		}
		job, changed, scheduleErr := m.store.ScheduleColdRepositoryRemoval(ctx, candidate)
		if scheduleErr != nil {
			m.log.Error("cold repository removal scheduling failed", "slot_id", candidate.SlotID, "repository_id", candidate.RepositoryID, "error", scheduleErr)
			continue
		}
		if changed {
			m.schedule(job)
			count++
		}
	}
	for _, item := range standbys {
		if _, ok := m.rootForPath(item.Path); !ok {
			continue
		}
		job, changed, err := m.store.ScheduleRemoval(ctx, item.SlotID, "")
		if err != nil {
			m.log.Error("standby removal scheduling failed", "slot_id", item.SlotID, "error", err)
			continue
		}
		if changed {
			m.schedule(job)
			count++
		}
	}
	for _, item := range items {
		if _, ok := m.rootForPath(item.Path); !ok {
			continue
		}
		job, changed, err := m.store.ScheduleRemoval(ctx, item.SlotID, item.SessionID)
		if err != nil {
			m.log.Error("ended worktree removal scheduling failed", "slot_id", item.SlotID, "error", err)
			continue
		}
		if changed {
			m.schedule(job)
			count++
		}
	}
	archiveManager := m.newArchiveManager(cfg, "")
	for sessionID, snapshots := range expiredSessions {
		ok := true
		var rootSnapshot state.WorkspaceSnapshot
		var rootSnapshotOwner string
		var rootSnapshotOwnerHandle *os.Root
		w, workspaceErr := m.store.SessionWorkspace(ctx, sessionID)
		if workspaceErr != nil {
			ok = false
		} else if w.Kind == "multi_repository" {
			var found bool
			rootSnapshot, found, workspaceErr = m.store.WorkspaceSnapshot(ctx, sessionID)
			if workspaceErr != nil || !found {
				ok = false
			} else if owner, owned := m.rootForPath(rootSnapshot.ArchivePath); !owned {
				ok = false
			} else {
				rootSnapshotOwner = owner
				rootSnapshotOwnerHandle, _, workspaceErr = m.existingRootDescriptor(owner)
				if workspaceErr != nil {
					ok = false
				} else {
					ok = archive.ValidateWorkspaceSnapshotAt(owner, rootSnapshotOwnerHandle, rootSnapshot, time.Time{}) == nil
				}
			}
		}
		if !ok {
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
	}
	if err := m.store.PruneMetadata(ctx, state.FormatTime(nowTime.Add(-cfg.Retention.FailedJob.Duration)), state.FormatTime(nowTime.Add(-cfg.Retention.EventLog.Duration)), state.FormatTime(nowTime.Add(-cfg.Retention.ExpiredSessionTombstone.Duration))); err != nil {
		return count, err
	}
	return count, nil
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
	root, ok := m.rootForPath(slot.Path)
	if !ok {
		return fmt.Errorf("%w: slot path is outside every current or retired wx root", state.ErrOwnership)
	}
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
	root, ok := m.rootForPath(slot.Path)
	if !ok || !domain.IsWithin(root, repositoryState.WorktreePath) {
		return fmt.Errorf("%w: cold repository path is outside wx ownership root", state.ErrOwnership)
	}
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
		if _, err := m.store.ValidateSlotOwnership(context.Background(), state.SlotOwnershipRequest{SlotID: slot.ID, WorkspaceID: slot.WorkspaceID, Path: slot.Path, AllowedSlotStates: []string{"RETIRING"}}); err != nil {
			m.quarantineOwnershipFailure(slot.ID, []string{"RETIRING"}, err)
			return err
		}
		ownedRoot, closeOwnedRoot, err := m.existingRootDescriptor(root)
		if err != nil {
			return fmt.Errorf("%w: recreate cold workspace shell: %v", state.ErrOwnership, err)
		}
		defer closeOwnedRoot()
		if err := verifyRootDescriptorPath(root, ownedRoot); err != nil {
			return err
		}
		relativeSlot, relativeErr := filepath.Rel(filepath.Clean(root), filepath.Clean(slot.Path))
		if relativeErr != nil || relativeSlot == "." || relativeSlot == ".." || strings.HasPrefix(relativeSlot, ".."+string(filepath.Separator)) || filepath.IsAbs(relativeSlot) {
			if relativeErr == nil {
				relativeErr = errors.New("slot path is outside wx root")
			}
			return fmt.Errorf("%w: recreate cold workspace shell: %v", state.ErrOwnership, relativeErr)
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
	return best, best != ""
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
		w, err := m.store.SessionWorkspace(ctx, sessionID)
		if err != nil {
			return removalMetadataFailure("resolve session workspace before removal", err)
		}
		if w.Kind == "multi_repository" {
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
			archiveRootHandle, _, rootErr := m.existingRootDescriptor(archiveRoot)
			if rootErr != nil {
				return removalMetadataFailure("open workspace root snapshot owner", rootErr)
			}
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
	if _, err := m.store.ValidateSlotOwnership(context.Background(), state.SlotOwnershipRequest{SlotID: slotID, Path: slotPath, AllowedSlotStates: []string{"REMOVING"}}); err != nil {
		return err
	}
	ownedRoot, closeOwnedRoot, err := m.existingRootDescriptor(root)
	if err != nil {
		return fmt.Errorf("%w: open slot root for removal: %v", state.ErrOwnership, err)
	}
	defer closeOwnedRoot()
	if err := verifyRootDescriptorPath(root, ownedRoot); err != nil {
		return err
	}
	relativeSlot, relativeErr := filepath.Rel(filepath.Clean(root), filepath.Clean(slotPath))
	if relativeErr != nil || relativeSlot == "." || relativeSlot == ".." || strings.HasPrefix(relativeSlot, ".."+string(filepath.Separator)) || filepath.IsAbs(relativeSlot) {
		if relativeErr == nil {
			relativeErr = errors.New("slot path is outside wx root")
		}
		return fmt.Errorf("%w: open slot root for removal: %v", state.ErrOwnership, relativeErr)
	}
	info, err := ownedRoot.Lstat(relativeSlot)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return fmt.Errorf("%w: inspect slot root for removal: %v", state.ErrOwnership, err)
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
		return fmt.Errorf("%w: wx root path changed: %v", state.ErrOwnership, err)
	}
	defer func() { _ = current.Close() }()
	heldInfo, err := owner.Lstat(".")
	if err != nil {
		return fmt.Errorf("%w: inspect pinned wx root: %v", state.ErrOwnership, err)
	}
	currentInfo, err := current.Lstat(".")
	if err != nil {
		return fmt.Errorf("%w: inspect wx root path: %v", state.ErrOwnership, err)
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
	return fmt.Errorf("%w: %s: %v", state.ErrOwnership, message, err)
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
		bytes, usageErr := directoryUsage(root)
		allocated, allocatedErr := allocatedDirectoryUsage(root)
		item := rootStatus{Path: root, Active: active, Bytes: bytes, AllocatedBytes: allocated, Measurement: "st_blocks_x_512"}
		if usageErr != nil && !errors.Is(usageErr, os.ErrNotExist) {
			item.Error = usageErr.Error()
		} else if allocatedErr != nil && !errors.Is(allocatedErr, os.ErrNotExist) {
			item.Error = allocatedErr.Error()
		}
		rootStatuses = append(rootStatuses, item)
	}
	sort.Slice(rootStatuses, func(i, j int) bool { return rootStatuses[i].Path < rootStatuses[j].Path })
	return map[string]any{
		"schema_version": state.SchemaVersion, "daemon_version": daemonVersion(), "protocol_version": 1, "uptime_seconds": int(time.Since(m.started).Seconds()),
		"config_path": must(config.Path()), "config_last_reload": reloadAt.UTC().Format(time.RFC3339Nano), "config_reload_error": reloadError,
		"sqlite_last_backup": formatOptionalTime(backupAt), "sqlite_backup_error": backupError,
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

func directoryUsage(root string) (int64, error) {
	var total int64
	err := filepath.Walk(root, func(_ string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if info.Mode().IsRegular() {
			total += info.Size()
		}
		return nil
	})
	return total, err
}

func allocatedDirectoryUsage(root string) (int64, error) {
	var total int64
	err := filepath.Walk(root, func(_ string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if stat, ok := info.Sys().(*syscall.Stat_t); ok {
			total += stat.Blocks * 512
		}
		return nil
	})
	return total, err
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
		if diagnosticHooksAvailable(agentKind) {
			hooks[agentKind] = "ok"
		} else {
			hooks[agentKind] = "missing or invalid readiness hooks; foreground readiness remains required"
		}
	}
	checks["hooks"] = hooks
	checks["worktree_registration"] = m.registrationDiagnostics(ctx)
	checks["artifact_ownership"] = m.artifactDiagnostics(ctx)
	return map[string]any{"schema_version": state.SchemaVersion, "checks": checks}
}

func diagnosticHooksAvailable(agentKind string) bool {
	return hookconfig.Available(agentKind)
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
	return m.store.ForgetWorkspace(ctx, string(canonical))
}

func (m *Manager) ReloadConfig() error {
	return m.reloadConfig(true)
}

func (m *Manager) reloadConfig(runGC bool) error {
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
	m.mu.RLock()
	existingHandle := m.rootHandles[filepath.Clean(newRoot)]
	m.mu.RUnlock()
	if existingHandle != nil {
		_ = newHandle.Close()
		newHandle = nil
	}
	m.mu.Lock()
	if m.roots == nil {
		m.roots = map[string]bool{}
	}
	oldRoot := oldConfiguredRoot
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
	}
	if newHandle != nil {
		if m.rootHandles == nil {
			m.rootHandles = map[string]*os.Root{}
		}
		m.rootHandles[newRoot] = newHandle
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
		go func() {
			m.reconcileRegistry(m.ctx)
			_, _ = m.GC(m.ctx, false)
		}()
	}
	return nil
}

func ensureWorktreeRoot(value string) (string, error) {
	root, ownedRoot, err := ensureWorktreeRootDescriptor(value)
	if err != nil {
		return "", err
	}
	if err := ownedRoot.Close(); err != nil {
		return "", err
	}
	return root, nil
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
func JSON(v any) json.RawMessage { b, _ := json.Marshal(v); return b }
