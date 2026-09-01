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
	"sync"
	"syscall"
	"time"

	"github.com/HappyOnigiri/WX/internal/archive"
	"github.com/HappyOnigiri/WX/internal/config"
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
}
type jobWork struct {
	id string
}

func New(cfg config.Config, store *state.Store, logger *slog.Logger) *Manager {
	git := &gitx.Runner{Timeout: cfg.Readiness.Timeout.Duration}
	started := time.Now()
	m := &Manager{cfg: cfg, store: store, git: git, log: logger, started: started, lastReload: started, roots: map[string]bool{}, jobs: make(chan jobWork, 256)}
	if root, err := config.ExpandHome(cfg.Storage.WorktreeRoot); err == nil {
		m.roots[filepath.Clean(root)] = true
	}
	for workerID := range cfg.Pool.PreparationConcurrency {
		owner := fmt.Sprintf("%d:%d", os.Getpid(), workerID)
		go func() {
			for work := range m.jobs {
				job, err := m.store.ClaimJob(context.Background(), work.id, owner)
				if err != nil {
					continue
				}
				jobCtx, cancel := context.WithCancel(context.Background())
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
				if finishErr := m.store.FinishJob(context.Background(), work.id, owner, err); finishErr != nil {
					m.log.Error("finish job failed", "job_id", work.id, "error", finishErr)
				}
			}
		}()
	}
	go m.maintainJobs()
	go m.maintainLifecycle()
	return m
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
	case m.jobs <- jobWork{id: job.ID}:
	default:
		// The durable PENDING row is picked up by maintainJobs once capacity frees up.
	}
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
	m.recoverJobs(true)
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		m.recoverJobs(false)
	}
}

func (m *Manager) maintainLifecycle() {
	m.reconcileOrphans(context.Background())
	m.maybeBackup(context.Background())
	_, _ = m.GC(context.Background(), false)
	interval := m.Config().Discovery.ReconcileInterval.Duration
	if interval <= 0 {
		interval = 10 * time.Minute
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for range ticker.C {
		_ = m.reloadConfig(false)
		m.reconcileOrphans(context.Background())
		m.maybeBackup(context.Background())
		if _, err := m.GC(context.Background(), false); err != nil {
			m.log.Error("automatic GC failed", "error", err)
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
	candidates, err := m.store.OrphanCandidates(ctx, time.Now().Add(-45*time.Second).UTC().Format(time.RFC3339Nano))
	if err != nil {
		m.log.Error("orphan reconciliation failed", "error", err)
		return
	}
	for _, candidate := range candidates {
		alive := candidate.ClientPID > 0 && syscall.Kill(candidate.ClientPID, 0) == nil
		if alive {
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
func (m *Manager) runRecoveredJob(ctx context.Context, job state.Job) error {
	switch job.Kind {
	case "PREPARE":
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
		return m.prepareSlot(ctx, job.SlotID, w, resolved, repos)
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
	w, err := m.store.Workspace(ctx, s.WorkspaceID)
	if err != nil {
		return err
	}
	repos, err := m.store.SlotRepositories(ctx, s.SlotID)
	if err != nil {
		return err
	}
	resolved, err := m.resolvedFromStored(ctx, w, repos)
	if err != nil {
		return err
	}
	snapshots, err := m.store.Snapshots(ctx, s.ParentSessionID)
	if err != nil {
		return err
	}
	by := map[string]state.Snapshot{}
	for _, snapshot := range snapshots {
		by[snapshot.RepositoryID] = snapshot
	}
	m.restoreSlot(ctx, s.SlotID, w, resolved, repos, by)
	return nil
}

func (m *Manager) ResolveAndLease(ctx context.Context, cwd string, branches []string, agent string, pid int) (Lease, error) {
	discoverer := discovery.Discoverer{Git: m.git, Config: m.Config()}
	w, err := discoverer.Resolve(ctx, cwd)
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
		if ready, ok, err := m.store.ReadySlot(ctx, string(w.ID)); err != nil {
			return Lease{}, err
		} else if ok {
			valid, err := m.readyMatches(ctx, ready, resolved)
			if err != nil {
				return Lease{}, err
			}
			if valid {
				token, err := state.TokenHex()
				if err != nil {
					return Lease{}, err
				}
				session := state.Session{ID: ready.ID, WorkspaceID: string(w.ID), SlotID: ready.ID, State: "ACTIVE", AgentKind: agent, ClientPID: pid, TokenHash: state.HashToken(token)}
				if err := m.store.LeaseReady(ctx, ready.ID, session); err == nil {
					_ = m.enqueue("ENSURE_STANDBY", string(w.ID), "", "")
					return Lease{SessionID: session.ID, Token: token, Path: ready.Path, SourceWorkspace: string(w.Root), Ready: true}, nil
				}
			}
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
	if err := os.MkdirAll(root, 0700); err != nil {
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
	go func() { _, _ = m.GC(context.Background(), false) }()
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
	for i, r := range resolved {
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
		if err := preparer.Prepare(ctx, r.Repository, repos[i].WorktreePath, r.OID, id); err != nil {
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
	if err := m.store.FinishPreparation(ctx, id); err != nil {
		m.log.Error("finish preparation failed", "slot_id", id, "error", err)
		return err
	}
	return nil
}
func (m *Manager) readyMatches(ctx context.Context, s state.Slot, resolved []pool.Resolved) (bool, error) {
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
	for _, r := range resolved {
		stored, ok := byID[string(r.Repository.ID)]
		if !ok || stored.BaseOID != r.OID {
			return false, nil
		}
		fp, err := workspace.Fingerprint(s.Generation, r.OID, r.Repository, m.Config())
		if err != nil {
			return false, err
		}
		if fp != stored.Fingerprint {
			return false, nil
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
		if err := os.MkdirAll(root, 0700); err != nil {
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
	if err := os.MkdirAll(root, 0700); err != nil {
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
	current, err := m.store.Session(ctx, id, token)
	if err != nil {
		return err
	}
	prior, err := m.store.FindByAgentSession(ctx, current.AgentKind, agentID)
	if errors.Is(err, sql.ErrNoRows) {
		return m.store.BindFreshSession(ctx, id, "", agentID)
	}
	if err != nil {
		return err
	}
	if prior.State != "EXPIRED" {
		return fmt.Errorf("--fresh is refused because wx session %s is %s, not EXPIRED", prior.ID, prior.State)
	}
	return m.store.BindFreshSession(ctx, id, prior.ID, agentID)
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
	if prior.State == "EXPIRED" || !snapshotsUsable(snaps, time.Now()) {
		return fmt.Errorf("wx recovery snapshot is expired or unavailable; stop this session and run wx resume %s %s resume %s, or rerun the native resume with --fresh only if local workspace state may be discarded", prior.ID, current.AgentKind, agentID)
	}
	w, err := m.store.Workspace(ctx, prior.WorkspaceID)
	if err != nil {
		return err
	}
	resolved := make([]pool.Resolved, 0, len(w.Repositories))
	snapByRepo := map[string]state.Snapshot{}
	for _, s := range snaps {
		snapByRepo[s.RepositoryID] = s
	}
	for _, repo := range w.Repositories {
		s, ok := snapByRepo[string(repo.ID)]
		if !ok {
			return fmt.Errorf("snapshot missing repository %s", repo.RelativePath)
		}
		resolved = append(resolved, pool.Resolved{Repository: repo, RequestedRef: s.HeadRef, OID: s.HeadOID})
	}
	slot, _ := m.store.Slot(ctx, id)
	generation, err := m.store.WorkspaceGeneration(ctx, string(w.ID))
	if err != nil {
		return err
	}
	repos, err := m.slotRepos(slot.Path, w, resolved, generation)
	if err != nil {
		return err
	}
	job, err := m.store.BindResumeSlot(ctx, id, prior.ID, string(w.ID), agentID, generation, repos)
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
		if err := archiveManager.Restore(ctx, r.Repository, repos[i].WorktreePath, id, snaps[string(r.Repository.ID)]); err != nil {
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
	expired := old.State == "EXPIRED" || !snapshotsUsable(snaps, time.Now())
	return map[string]any{"state": old.State, "expired": expired, "workspace_id": old.WorkspaceID}, nil
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

func (m *Manager) Resume(ctx context.Context, oldID, agent string, pid int, allowFresh bool) (Lease, error) {
	old, err := m.store.SessionByID(ctx, oldID)
	if err != nil {
		return Lease{}, err
	}
	snaps, err := m.store.Snapshots(ctx, oldID)
	if err != nil {
		return Lease{}, err
	}
	w, err := m.store.Workspace(ctx, old.WorkspaceID)
	if err != nil {
		return Lease{}, err
	}
	if old.State == "EXPIRED" || !snapshotsUsable(snaps, time.Now()) {
		if !allowFresh {
			return Lease{}, errors.New("session snapshot is EXPIRED; confirmation is required before creating a workspace from the current base")
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

func (m *Manager) Release(ctx context.Context, id, token, reason string) error {
	session, err := m.store.Session(ctx, id, token)
	if err != nil {
		return err
	}
	job, changed, err := m.store.Release(ctx, id, session.WorkspaceID, session.SlotID)
	if err != nil || !changed {
		return err
	}
	m.schedule(job)
	return nil
}
func (m *Manager) snapshotSession(ctx context.Context, s state.Session) error {
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
	return m.store.MarkArchived(ctx, s.ID, s.SlotID, expiry.Format(time.RFC3339Nano))
}

func (m *Manager) GC(ctx context.Context, dry bool) (int, error) {
	cfg := m.Config()
	nowTime := time.Now().UTC()
	before := nowTime.Add(-cfg.Retention.EndedWorktree.Duration).Format(time.RFC3339Nano)
	items, err := m.store.GCCandidates(ctx, before)
	if err != nil {
		return 0, err
	}
	standbys, err := m.store.StandbyGCCandidates(ctx, nowTime.Add(-cfg.Retention.HotStandby.Duration).Format(time.RFC3339Nano), cfg.Pool.WarmPerWorkspace)
	if err != nil {
		return 0, err
	}
	expired, err := m.store.ExpiredSnapshots(ctx, nowTime.Format(time.RFC3339Nano))
	if err != nil {
		return 0, err
	}
	expiredSessions := map[string][]state.Snapshot{}
	for _, snapshot := range expired {
		expiredSessions[snapshot.SessionID] = append(expiredSessions[snapshot.SessionID], snapshot)
	}
	total := len(items) + len(standbys) + len(expiredSessions)
	if dry {
		return total, nil
	}
	count := 0
	archiveManager := archive.Manager{Git: m.git, Preparer: &workspace.Preparer{Git: m.git, Config: cfg}}
	for _, item := range standbys {
		root, ok := m.rootForPath(item.Path)
		if !ok {
			continue
		}
		if err := m.removeSlotWorktrees(ctx, archiveManager, root, item.SlotID, "", item.Path); err != nil {
			m.log.Error("standby GC failed", "slot_id", item.SlotID, "error", err)
			continue
		}
		if err := m.store.MarkStandbyArchived(ctx, item.SlotID); err == nil {
			count++
		}
	}
	for _, item := range items {
		root, ok := m.rootForPath(item.Path)
		if !ok {
			continue
		}
		if err := m.removeSlotWorktrees(ctx, archiveManager, root, item.SlotID, item.SessionID, item.Path); err != nil {
			m.log.Error("ended worktree GC failed", "slot_id", item.SlotID, "error", err)
			continue
		}
		if err := m.store.MarkSlotArchived(ctx, item.SlotID); err == nil {
			count++
		}
	}
	for sessionID, snapshots := range expiredSessions {
		ok := true
		for _, snapshot := range snapshots {
			repo, err := m.store.Repository(ctx, snapshot.RepositoryID)
			if err != nil || archiveManager.DeleteSnapshotRefs(ctx, repo, snapshot) != nil {
				ok = false
				break
			}
		}
		if ok {
			if err := m.store.ExpireSessionSnapshots(ctx, sessionID); err == nil {
				count++
			}
		}
	}
	if err := m.store.PruneMetadata(ctx, nowTime.Add(-cfg.Retention.FailedJob.Duration).Format(time.RFC3339Nano), nowTime.Add(-cfg.Retention.EventLog.Duration).Format(time.RFC3339Nano), nowTime.Add(-cfg.Retention.ExpiredSessionTombstone.Duration).Format(time.RFC3339Nano)); err != nil {
		return count, err
	}
	return count, nil
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
	if _, err := os.Lstat(slotPath); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	return os.RemoveAll(slotPath)
}

func (m *Manager) Status(ctx context.Context) (map[string]any, error) {
	s, err := m.store.Status(ctx)
	if err != nil {
		return nil, err
	}
	m.mu.RLock()
	reloadAt, reloadError, backupAt, backupError := m.lastReload, m.reloadError, m.lastBackup, m.backupError
	m.mu.RUnlock()
	return map[string]any{"schema_version": 2, "protocol_version": 1, "uptime_seconds": int(time.Since(m.started).Seconds()), "config_path": must(config.Path()), "config_last_reload": reloadAt.UTC().Format(time.RFC3339Nano), "config_reload_error": reloadError, "sqlite_last_backup": formatOptionalTime(backupAt), "sqlite_backup_error": backupError, "workspaces": s.Workspaces, "repositories": s.Repositories, "slots": map[string]int{"ready": s.Ready, "leased": s.Leased, "failed": s.Failed, "quarantined": s.Quarantined}, "active_sessions": s.Active, "snapshots": s.Snapshots, "queued_jobs": s.Jobs}, nil
}
func formatOptionalTime(value time.Time) string {
	if value.IsZero() {
		return ""
	}
	return value.UTC().Format(time.RFC3339Nano)
}
func (m *Manager) Doctor(ctx context.Context) map[string]any {
	checks := map[string]any{}
	checks["config"] = "ok"
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
	return map[string]any{"schema_version": 1, "checks": checks}
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
	m.mu.Lock()
	m.cfg = cfg
	m.lastReload = time.Now()
	m.reloadError = ""
	if root, err := config.ExpandHome(cfg.Storage.WorktreeRoot); err == nil {
		m.roots[filepath.Clean(root)] = true
	}
	m.mu.Unlock()
	if runGC {
		go func() { _, _ = m.GC(context.Background(), false) }()
	}
	return nil
}
func must(v string, e error) string {
	if e != nil {
		return ""
	}
	return v
}
func JSON(v any) json.RawMessage { b, _ := json.Marshal(v); return b }
