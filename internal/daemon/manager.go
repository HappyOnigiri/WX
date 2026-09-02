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
	"github.com/HappyOnigiri/WX/internal/launchd"
	"github.com/HappyOnigiri/WX/internal/pool"
	"github.com/HappyOnigiri/WX/internal/state"
	"github.com/HappyOnigiri/WX/internal/workspace"
)

type Lease struct {
	SessionID       string `json:"session_id"`
	Token           string `json:"token"`
	Path            string `json:"path"`
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
	jobs        chan jobWork
	reloads     chan struct{}
	reclaimAll  bool
	ctx         context.Context
	cancel      context.CancelFunc
	wg          sync.WaitGroup
	workersMu   sync.Mutex
	workerStops []chan struct{}
	workerSeq   int
	closed      bool
	logLevel    *slog.LevelVar
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
	started := time.Now()
	managerCtx, managerCancel := context.WithCancel(context.Background())
	reclaimAll := len(exclusiveStartup) > 0 && exclusiveStartup[0]
	m := &Manager{cfg: cfg, store: store, git: git, log: logger, started: started, lastReload: started, roots: map[string]bool{}, jobs: make(chan jobWork, 256), reloads: make(chan struct{}, 1), reclaimAll: reclaimAll, ctx: managerCtx, cancel: managerCancel}
	if root, err := ensureWorktreeRoot(cfg.Storage.WorktreeRoot); err == nil {
		m.roots[root] = true
	} else {
		logger.Error("worktree root is unavailable", "path", cfg.Storage.WorktreeRoot, "error", err)
	}
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
	m.recoverJobs(m.reclaimAll)
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
			expectedList, refsErr := m.store.RecoveryRefs(ctx, string(repository.ID))
			if refsErr != nil {
				diagnosticErrors = append(diagnosticErrors, fmt.Sprintf("read recovery refs for %s: %v", repository.ID, refsErr))
				continue
			}
			expected := map[string]bool{}
			for _, ref := range expectedList {
				expected[ref] = true
			}
			listed, listErr := m.git.Run(ctx, string(repository.MainPath), "for-each-ref", "--format=%(refname)", "refs/wx/recovery")
			if listErr != nil {
				diagnosticErrors = append(diagnosticErrors, fmt.Sprintf("list recovery refs for %s: %v", repository.ID, listErr))
				continue
			}
			actual := map[string]bool{}
			for _, ref := range strings.Fields(listed.Stdout) {
				actual[ref] = true
				if !expected[ref] {
					unknownRefs = append(unknownRefs, fmt.Sprintf("%s:%s", repository.ID, ref))
				}
			}
			for ref := range expected {
				if !actual[ref] {
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
			return retryableJobError{err}
		}
		return nil
	case "REMOVE_REPOSITORY":
		if err := m.removeColdRepositoryJob(ctx, job); err != nil {
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
	if !snapshotsUsable(snapshots, time.Now()) {
		if parent.State == "RELEASING" || parent.State == "SNAPSHOTTING" {
			return dependencyPendingError{errors.New("parent snapshot is still being archived")}
		}
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
				return Lease{}, err
			}
			if valid {
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
						return Lease{SessionID: session.ID, Token: token, Path: ready.Path, SourceWorkspace: string(w.Root), Ready: false}, nil
					}
					continue
				}
				if err := m.store.LeaseReady(ctx, ready.ID, session); err == nil {
					_ = m.enqueue("ENSURE_STANDBY", string(w.ID), "", "")
					return Lease{SessionID: session.ID, Token: token, Path: ready.Path, SourceWorkspace: string(w.Root), Ready: true}, nil
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
	if err := os.MkdirAll(root, 0o700); err != nil {
		return Lease{}, err
	}
	if err := os.Chmod(root, 0o700); err != nil {
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
	return Lease{SessionID: id, Token: token, Path: root, SourceWorkspace: string(w.Root), Ready: false}, nil
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
	preparer := workspace.Preparer{Git: m.git, Config: m.Config()}
	if len(repos) != len(resolved) {
		return errors.New("slot repository metadata does not match resolved workspace")
	}
	for _, r := range resolved {
		stored, err := m.store.SlotRepository(ctx, id, string(r.Repository.ID))
		if err != nil {
			return err
		}
		if stored.State == "READY" {
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
			_ = m.store.SetSlotState(ctx, id, []string{"PREPARING", "RESTORING"}, "FAILED", "PREPARE_FAILED")
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
		if err := workspace.MaterializeRoot(string(w.Root), slot.Path, m.Config().Workspaces[string(w.Root)]); err != nil {
			m.log.Error("workspace root materialization failed", "slot_id", id, "error", err)
			_ = m.store.SetSlotState(ctx, id, []string{"PREPARING", "RESTORING"}, "FAILED", "ROOT_MATERIALIZATION_FAILED")
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

func (m *Manager) readyMatches(ctx context.Context, s state.Slot, resolved []pool.Resolved) (bool, error) {
	rootInfo, err := os.Lstat(s.Path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if rootInfo.Mode()&os.ModeSymlink != 0 || !rootInfo.IsDir() {
		return false, nil
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
	preparer := workspace.Preparer{Git: m.git, Config: m.Config()}
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
			if _, err := os.Lstat(stored.WorktreePath); err == nil {
				return false, nil
			} else if !errors.Is(err, os.ErrNotExist) {
				return false, err
			}
			continue
		}
		info, err := os.Lstat(stored.WorktreePath)
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return false, nil
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
		if err := os.MkdirAll(root, 0o700); err != nil {
			return err
		}
		if err := os.Chmod(root, 0o700); err != nil {
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
	if err := os.MkdirAll(root, 0o700); err != nil {
		return Lease{}, err
	}
	if err := os.Chmod(root, 0o700); err != nil {
		return Lease{}, err
	}
	session := state.Session{ID: id, SlotID: id, State: "UNBOUND", AgentKind: agent, ClientPID: pid, TokenHash: state.HashToken(token)}
	if _, err := m.store.CreateSlotSession(ctx, state.Slot{ID: id, Generation: 0, Path: root, State: "UNBOUND"}, nil, session, ""); err != nil {
		return Lease{}, err
	}
	return Lease{SessionID: id, Token: token, Path: root}, nil
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
	archiveManager := archive.Manager{Git: m.git, Preparer: &workspace.Preparer{Git: m.git, Config: m.Config()}}
	if len(repos) != len(resolved) {
		return errors.New("restore repository metadata does not match resolved workspace")
	}
	for i, r := range resolved {
		stored, err := m.store.SlotRepository(ctx, id, string(r.Repository.ID))
		if err != nil {
			return err
		}
		if stored.State == "READY" {
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
		if err := workspace.MaterializeRoot(string(w.Root), slot.Path, m.Config().Workspaces[string(w.Root)]); err != nil {
			_ = m.store.SetSlotState(ctx, id, []string{"RESTORING"}, "FAILED", "ROOT_MATERIALIZATION_FAILED")
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
		if err := archive.RestoreWorkspace(ctx, slot.Path, targetRoot, archiveRoot, rootSnapshot, workspaceRecoveryExclusions(w, m.Config())); err != nil {
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
	if err := archive.ValidateWorkspaceSnapshot(root, rootSnapshot, at); err != nil {
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
	archiveManager := archive.Manager{Git: m.git, Preparer: &workspace.Preparer{Git: m.git, Config: m.Config()}}
	for _, sr := range repos {
		repo, err := m.store.Repository(ctx, sr.RepositoryID)
		if err != nil {
			return err
		}
		snap, err := archiveManager.Snapshot(ctx, repo, sr.WorktreePath, s.ID, expiry)
		if err != nil {
			m.log.Error("snapshot failed", "session_id", s.ID, "repository_id", repo.ID, "error", err)
			_ = m.store.SetSlotState(ctx, s.SlotID, []string{"SNAPSHOTTING"}, "QUARANTINED", "SNAPSHOT_FAILED")
			return err
		}
		if err := m.store.SaveSnapshot(ctx, snap); err != nil {
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
		rootSnapshot, found, err := m.store.WorkspaceSnapshot(ctx, s.ID)
		if err != nil {
			return err
		}
		if found {
			if err := archive.ValidateWorkspaceSnapshot(ownershipRoot, rootSnapshot, time.Now()); err != nil {
				_ = m.store.SetSlotState(ctx, s.SlotID, []string{"SNAPSHOTTING"}, "QUARANTINED", "SNAPSHOT_FAILED")
				return fmt.Errorf("validate workspace root snapshot: %w", err)
			}
		} else {
			rootSnapshot, err = archive.SnapshotWorkspace(ctx, slot.Path, ownershipRoot, s.ID, workspaceRecoveryExclusions(w, m.Config()), expiry)
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
	archiveManager := archive.Manager{Git: m.git, Preparer: &workspace.Preparer{Git: m.git, Config: cfg}}
	for sessionID, snapshots := range expiredSessions {
		ok := true
		var rootSnapshot state.WorkspaceSnapshot
		var rootSnapshotOwner string
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
				ok = archive.ValidateWorkspaceSnapshot(owner, rootSnapshot, time.Time{}) == nil
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
			ok = archive.DeleteWorkspaceSnapshot(rootSnapshotOwner, rootSnapshot) == nil
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
		return errors.New("slot path is outside every current or retired wx root")
	}
	archiveManager := archive.Manager{Git: m.git, Preparer: &workspace.Preparer{Git: m.git, Config: m.Config()}}
	if err := m.removeSlotWorktrees(ctx, archiveManager, root, slot.ID, job.SessionID, slot.Path); err != nil {
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
		return errors.New("cold repository path is outside wx ownership root")
	}
	repository, err := m.store.Repository(ctx, job.RepositoryID)
	if err != nil {
		return err
	}
	archiveManager := archive.Manager{Git: m.git, Preparer: &workspace.Preparer{Git: m.git, Config: m.Config()}}
	if err := archiveManager.RemoveWorktree(ctx, repository, root, repositoryState.WorktreePath, repositoryState.BaseOID); err != nil {
		return err
	}
	if filepath.Clean(repositoryState.WorktreePath) == filepath.Clean(slot.Path) {
		ownedRoot, relativeSlot, err := domain.OpenOwnedRoot(root, slot.Path)
		if err != nil {
			return err
		}
		defer func() { _ = ownedRoot.Close() }()
		if err := ownedRoot.MkdirAll(relativeSlot, 0o700); err != nil {
			return fmt.Errorf("recreate cold workspace shell: %w", err)
		}
	}
	return m.store.FinishColdRepositoryRemoval(ctx, slot.ID, job.RepositoryID)
}

func (m *Manager) rootForPath(path string) (string, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for root := range m.roots {
		if domain.IsWithin(root, path) {
			return root, true
		}
	}
	return "", false
}

func (m *Manager) removeSlotWorktrees(ctx context.Context, archiveManager archive.Manager, root, slotID, sessionID, slotPath string) error {
	if !domain.IsWithin(root, slotPath) {
		return errors.New("slot path is outside wx root")
	}
	repos, err := m.store.SlotRepositories(ctx, slotID)
	if err != nil {
		return err
	}
	expected := map[string]string{}
	if sessionID != "" {
		w, err := m.store.SessionWorkspace(ctx, sessionID)
		if err != nil {
			return err
		}
		if w.Kind == "multi_repository" {
			rootSnapshot, found, err := m.store.WorkspaceSnapshot(ctx, sessionID)
			if err != nil {
				return err
			}
			if !found {
				return errors.New("workspace root snapshot metadata is incomplete for worktree removal")
			}
			archiveRoot, ok := m.rootForPath(rootSnapshot.ArchivePath)
			if !ok {
				return errors.New("workspace root snapshot is outside known wx roots")
			}
			if err := archive.ValidateWorkspaceSnapshot(archiveRoot, rootSnapshot, time.Now()); err != nil {
				return fmt.Errorf("validate workspace root snapshot before removal: %w", err)
			}
		}
		snapshots, err := m.store.Snapshots(ctx, sessionID)
		if err != nil {
			return err
		}
		for _, snapshot := range snapshots {
			expected[snapshot.RepositoryID] = snapshot.HeadOID
		}
	}
	for _, sr := range repos {
		repo, err := m.store.Repository(ctx, sr.RepositoryID)
		if err != nil || !domain.IsWithin(root, sr.WorktreePath) {
			return errors.New("slot repository ownership validation failed")
		}
		expectedHead := sr.BaseOID
		if sessionID != "" {
			var ok bool
			expectedHead, ok = expected[sr.RepositoryID]
			if !ok {
				return errors.New("snapshot metadata is incomplete for worktree removal")
			}
		}
		if err := archiveManager.RemoveWorktree(ctx, repo, root, sr.WorktreePath, expectedHead); err != nil {
			return err
		}
	}
	ownedRoot, relativeSlot, err := domain.OpenOwnedRoot(root, slotPath)
	if err != nil {
		return err
	}
	defer func() { _ = ownedRoot.Close() }()
	info, err := ownedRoot.Lstat(relativeSlot)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return errors.New("slot root is not a physical directory")
	}
	// Root.RemoveAll does not follow symlink leaves and cannot traverse outside
	// the descriptor-owned root. It also removes bundle rules and empty nested
	// repository parents left after the registered worktrees are removed.
	return ownedRoot.RemoveAll(relativeSlot)
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
	home, err := os.UserHomeDir()
	if err != nil {
		return false
	}
	paths := []string{filepath.Join(home, ".claude", "settings.json"), filepath.Join(home, ".claude", "settings.local.json")}
	if agentKind == "codex" {
		paths = []string{filepath.Join(home, ".codex", "hooks.json")}
	}
	required := map[string]string{"SessionStart": "session-start", "UserPromptSubmit": "user-prompt-submit", "PreToolUse": "pre-tool-use"}
	found := map[string]bool{}
	for _, path := range paths {
		data, readErr := os.ReadFile(path)
		if readErr != nil || len(data) > 4<<20 {
			continue
		}
		var document struct {
			Hooks map[string]any `json:"hooks"`
		}
		if json.Unmarshal(data, &document) != nil {
			continue
		}
		for event, command := range required {
			if diagnosticHookTreeContainsCommand(document.Hooks[event], command) {
				found[event] = true
			}
		}
	}
	return found["SessionStart"] && found["UserPromptSubmit"] && found["PreToolUse"]
}

func diagnosticHookTreeContainsCommand(value any, event string) bool {
	switch typed := value.(type) {
	case []any:
		for _, child := range typed {
			if diagnosticHookTreeContainsCommand(child, event) {
				return true
			}
		}
	case map[string]any:
		if disabled, _ := typed["disabled"].(bool); disabled {
			return false
		}
		if command, ok := typed["command"].(string); ok {
			fields := strings.Fields(command)
			if len(fields) == 3 && filepath.Base(strings.Trim(fields[0], `"'`)) == "wx" && fields[1] == "hook" && fields[2] == event {
				return true
			}
		}
		for key, child := range typed {
			if key != "command" && diagnosticHookTreeContainsCommand(child, event) {
				return true
			}
		}
	}
	return false
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
	newRoot, err := ensureWorktreeRoot(cfg.Storage.WorktreeRoot)
	if err != nil {
		m.mu.Lock()
		m.lastReload = time.Now()
		m.reloadError = err.Error()
		m.mu.Unlock()
		return fmt.Errorf("validate worktree root: %w", err)
	}
	m.mu.Lock()
	oldRoot := filepath.Clean(m.cfg.Storage.WorktreeRoot)
	if oldRoot != newRoot {
		if err := m.store.DrainRoot(context.Background(), oldRoot); err != nil {
			m.lastReload = time.Now()
			m.reloadError = err.Error()
			m.mu.Unlock()
			return fmt.Errorf("drain retired worktree root: %w", err)
		}
		m.roots[oldRoot] = false
		m.roots[newRoot] = true
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
	root, err := config.ExpandHome(value)
	if err != nil {
		return "", err
	}
	root = filepath.Clean(root)
	if err := domain.EnsurePhysicalDirectory(root, 0o700); err != nil {
		return "", err
	}
	info, err := os.Lstat(root)
	if err != nil {
		return "", err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return "", fmt.Errorf("%s is not a physical directory", root)
	}
	if err := domain.ValidatePhysicalPath(root, false); err != nil {
		return "", err
	}
	if err := os.Chmod(root, 0o700); err != nil {
		return "", err
	}
	return root, nil
}

func must(v string, e error) string {
	if e != nil {
		return ""
	}
	return v
}
func JSON(v any) json.RawMessage { b, _ := json.Marshal(v); return b }
