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
	mu       sync.RWMutex
	cfg      config.Config
	store    *state.Store
	git      *gitx.Runner
	discover *discovery.Discoverer
	prepare  *workspace.Preparer
	archive  *archive.Manager
	log      *slog.Logger
	started  time.Time
	jobs     chan jobWork
}
type jobWork struct {
	id  string
	run func() error
}

func New(cfg config.Config, store *state.Store, logger *slog.Logger) *Manager {
	git := &gitx.Runner{Timeout: cfg.Readiness.Timeout.Duration}
	p := &workspace.Preparer{Git: git, Config: cfg}
	m := &Manager{cfg: cfg, store: store, git: git, log: logger, started: time.Now(), jobs: make(chan jobWork, 256)}
	m.discover = &discovery.Discoverer{Git: git, Config: cfg}
	m.prepare = p
	m.archive = &archive.Manager{Git: git, Preparer: p}
	for range cfg.Pool.PreparationConcurrency {
		go func() {
			for work := range m.jobs {
				if _, err := m.store.ClaimJob(context.Background(), work.id, fmt.Sprintf("%d", os.Getpid())); err != nil {
					continue
				}
				err := work.run()
				if finishErr := m.store.FinishJob(context.Background(), work.id, err); finishErr != nil {
					m.log.Error("finish job failed", "job_id", work.id, "error", finishErr)
				}
			}
		}()
	}
	go m.recoverJobs()
	return m
}

func (m *Manager) Config() config.Config { m.mu.RLock(); defer m.mu.RUnlock(); return m.cfg }
func (m *Manager) enqueue(kind, workspaceID, slotID, sessionID string, fn func() error) error {
	job, err := m.store.CreateJob(context.Background(), kind, workspaceID, slotID, sessionID)
	if err != nil {
		return err
	}
	m.jobs <- jobWork{id: job.ID, run: fn}
	return nil
}

func (m *Manager) recoverJobs() {
	jobs, err := m.store.RecoverJobs(context.Background())
	if err != nil {
		m.log.Error("recover jobs failed", "error", err)
		return
	}
	for _, job := range jobs {
		j := job
		m.jobs <- jobWork{id: j.ID, run: func() error { return m.runRecoveredJob(context.Background(), j) }}
	}
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
		m.prepareSlot(ctx, job.SlotID, w, resolved, repos)
		return nil
	case "ENSURE_STANDBY":
		w, err := m.store.Workspace(ctx, job.WorkspaceID)
		if err != nil {
			return err
		}
		m.ensureStandby(ctx, w)
		return nil
	case "SNAPSHOT":
		s, err := m.store.SessionByID(ctx, job.SessionID)
		if err != nil {
			return err
		}
		m.snapshotSession(ctx, s)
		return nil
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
	if err := m.store.UpsertWorkspace(ctx, w); err != nil {
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
					_ = m.enqueue("ENSURE_STANDBY", string(w.ID), "", "", func() error { m.ensureStandby(context.Background(), w); return nil })
					return Lease{SessionID: session.ID, Token: token, Path: ready.Path, SourceWorkspace: string(w.Root), Ready: true}, nil
				}
			}
		}
	}
	return m.allocate(ctx, w, resolved, agent, pid, "STARTING", "")
}

func (m *Manager) allocate(ctx context.Context, w discovery.Workspace, resolved []pool.Resolved, agent string, pid int, sessionState, parent string) (Lease, error) {
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
	repos, err := m.slotRepos(root, w, resolved)
	if err != nil {
		return Lease{}, err
	}
	slotState := "PREPARING"
	if sessionState == "RESTORING" {
		slotState = "RESTORING"
	}
	session := state.Session{ID: id, WorkspaceID: string(w.ID), SlotID: id, ParentSessionID: parent, State: sessionState, AgentKind: agent, ClientPID: pid, TokenHash: state.HashToken(token)}
	if err := m.store.CreateSlotSession(ctx, state.Slot{ID: id, WorkspaceID: string(w.ID), Generation: 1, Path: root, State: slotState}, repos, session); err != nil {
		return Lease{}, err
	}
	if err := m.store.RecordLease(ctx, string(w.ID)); err != nil {
		return Lease{}, err
	}
	if sessionState != "RESTORING" {
		if err := m.enqueue("PREPARE", string(w.ID), id, id, func() error { m.prepareSlot(context.Background(), id, w, resolved, repos); return nil }); err != nil {
			return Lease{}, err
		}
	}
	_ = m.enqueue("ENSURE_STANDBY", string(w.ID), "", "", func() error { m.ensureStandby(context.Background(), w); return nil })
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
func (m *Manager) slotRepos(root string, w discovery.Workspace, resolved []pool.Resolved) ([]state.SlotRepository, error) {
	out := make([]state.SlotRepository, 0, len(resolved))
	for _, r := range resolved {
		target := root
		if w.Kind == "multi_repository" {
			target = filepath.Join(root, r.Repository.RelativePath)
		}
		fp, err := workspace.Fingerprint(1, r.OID, r.Repository, m.Config())
		if err != nil {
			return nil, err
		}
		out = append(out, state.SlotRepository{RepositoryID: string(r.Repository.ID), WorktreePath: target, State: "PREPARING", RequestedRef: r.RequestedRef, BaseOID: r.OID, Fingerprint: fp})
	}
	return out, nil
}
func (m *Manager) prepareSlot(ctx context.Context, id string, w discovery.Workspace, resolved []pool.Resolved, repos []state.SlotRepository) {
	preparer := workspace.Preparer{Git: m.git, Config: m.Config()}
	for i, r := range resolved {
		if err := preparer.Prepare(ctx, r.Repository, repos[i].WorktreePath, r.OID, id); err != nil {
			m.log.Error("slot preparation failed", "slot_id", id, "repository_id", r.Repository.ID, "error", err)
			_ = m.store.SetSlotState(ctx, id, []string{"PREPARING", "RESTORING"}, "FAILED", "PREPARE_FAILED")
			return
		}
	}
	if w.Kind == "multi_repository" {
		slot, err := m.store.Slot(ctx, id)
		if err != nil {
			return
		}
		if err := workspace.MaterializeRoot(string(w.Root), slot.Path, m.Config().Workspaces[string(w.Root)]); err != nil {
			m.log.Error("workspace root materialization failed", "slot_id", id, "error", err)
			_ = m.store.SetSlotState(ctx, id, []string{"PREPARING", "RESTORING"}, "FAILED", "ROOT_MATERIALIZATION_FAILED")
			return
		}
	}
	if err := m.store.FinishPreparation(ctx, id); err != nil {
		m.log.Error("finish preparation failed", "slot_id", id, "error", err)
	}
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
		fp, err := workspace.Fingerprint(1, r.OID, r.Repository, m.Config())
		if err != nil {
			return false, err
		}
		if fp != stored.Fingerprint {
			return false, nil
		}
	}
	return true, nil
}

func (m *Manager) ensureStandby(ctx context.Context, w discovery.Workspace) {
	cfg := m.Config()
	if cfg.Pool.WarmPerWorkspace < 1 || cfg.Retention.HotStandby.Duration == 0 {
		return
	}
	if m.store.HasStandby(ctx, string(w.ID)) {
		return
	}
	resolved, err := pool.ResolveBranches(ctx, m.git, w, nil)
	if err != nil {
		m.log.Error("resolve standby base failed", "workspace_id", w.ID, "error", err)
		return
	}
	id, err := domain.NewID()
	if err != nil {
		return
	}
	root, err := m.slotRoot(string(w.ID), id, false)
	if err != nil {
		return
	}
	if err := os.MkdirAll(root, 0700); err != nil {
		return
	}
	repos, err := m.slotRepos(root, w, resolved)
	if err != nil {
		return
	}
	if err := m.store.CreateStandby(ctx, state.Slot{ID: id, WorkspaceID: string(w.ID), Generation: 1, Path: root, State: "PREPARING"}, repos); err != nil {
		return
	}
	m.prepareSlot(ctx, id, w, resolved, repos)
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
	if err := m.store.CreateSlotSession(ctx, state.Slot{ID: id, Generation: 0, Path: root, State: "UNBOUND"}, nil, session); err != nil {
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
	if len(snaps) == 0 {
		return errors.New("wx recovery snapshot is expired or unavailable")
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
	repos, err := m.slotRepos(slot.Path, w, resolved)
	if err != nil {
		return err
	}
	if err := m.store.BindResumeSlot(ctx, id, prior.ID, string(w.ID), agentID, repos); err != nil {
		return err
	}
	return m.enqueue("RESTORE", string(w.ID), id, id, func() error { m.restoreSlot(context.Background(), id, w, resolved, repos, snapByRepo); return nil })
}
func (m *Manager) restoreSlot(ctx context.Context, id string, w discovery.Workspace, resolved []pool.Resolved, repos []state.SlotRepository, snaps map[string]state.Snapshot) {
	archiveManager := archive.Manager{Git: m.git, Preparer: &workspace.Preparer{Git: m.git, Config: m.Config()}}
	for i, r := range resolved {
		if err := archiveManager.Restore(ctx, r.Repository, repos[i].WorktreePath, id, snaps[string(r.Repository.ID)]); err != nil {
			m.log.Error("restore failed", "slot_id", id, "repository_id", r.Repository.ID, "error", err)
			_ = m.store.SetSlotState(ctx, id, []string{"RESTORING"}, "QUARANTINED", "RESTORE_FAILED")
			return
		}
	}
	if w.Kind == "multi_repository" {
		slot, err := m.store.Slot(ctx, id)
		if err != nil {
			return
		}
		if err := workspace.MaterializeRoot(string(w.Root), slot.Path, m.Config().Workspaces[string(w.Root)]); err != nil {
			_ = m.store.SetSlotState(ctx, id, []string{"RESTORING"}, "FAILED", "ROOT_MATERIALIZATION_FAILED")
			return
		}
	}
	_ = m.store.FinishPreparation(ctx, id)
}

func (m *Manager) Resume(ctx context.Context, oldID, agent string, pid int) (Lease, error) {
	old, err := m.store.SessionByID(ctx, oldID)
	if err != nil {
		return Lease{}, err
	}
	snaps, err := m.store.Snapshots(ctx, oldID)
	if err != nil {
		return Lease{}, err
	}
	if len(snaps) == 0 {
		return Lease{}, errors.New("session snapshot is EXPIRED or unavailable")
	}
	w, err := m.store.Workspace(ctx, old.WorkspaceID)
	if err != nil {
		return Lease{}, err
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
	lease, err := m.allocate(ctx, w, resolved, agent, pid, "RESTORING", oldID)
	if err != nil {
		return Lease{}, err
	}
	snapMap := map[string]state.Snapshot{}
	for _, s := range snaps {
		snapMap[s.RepositoryID] = s
	} // Replace the generic preparation with restore; state CAS ensures only one succeeds.
	if err := m.enqueue("RESTORE", string(w.ID), lease.SessionID, lease.SessionID, func() error {
		m.restoreSlot(context.Background(), lease.SessionID, w, resolved, mustRepos(m.store.SlotRepositories(context.Background(), lease.SessionID)), snapMap)
		return nil
	}); err != nil {
		return Lease{}, err
	}
	return lease, nil
}
func mustRepos(v []state.SlotRepository, e error) []state.SlotRepository {
	if e != nil {
		return nil
	}
	return v
}

func (m *Manager) Release(ctx context.Context, id, token, reason string) error {
	session, err := m.store.Session(ctx, id, token)
	if err != nil {
		return err
	}
	changed, err := m.store.Release(ctx, id)
	if err != nil || !changed {
		return err
	}
	return m.enqueue("SNAPSHOT", session.WorkspaceID, session.SlotID, session.ID, func() error { m.snapshotSession(context.Background(), session); return nil })
}
func (m *Manager) snapshotSession(ctx context.Context, s state.Session) {
	_ = m.store.SetSlotState(ctx, s.SlotID, []string{"DRAINING"}, "SNAPSHOTTING", "")
	repos, err := m.store.SlotRepositories(ctx, s.SlotID)
	if err != nil {
		return
	}
	expiry := time.Now().Add(m.Config().Retention.RecoverySnapshot.Duration)
	for _, sr := range repos {
		repo, err := m.store.Repository(ctx, sr.RepositoryID)
		if err != nil {
			return
		}
		snap, err := m.archive.Snapshot(ctx, repo, sr.WorktreePath, s.ID, expiry)
		if err != nil {
			m.log.Error("snapshot failed", "session_id", s.ID, "repository_id", repo.ID, "error", err)
			_ = m.store.SetSlotState(ctx, s.SlotID, []string{"SNAPSHOTTING"}, "QUARANTINED", "SNAPSHOT_FAILED")
			return
		}
		if err := m.store.SaveSnapshot(ctx, snap); err != nil {
			return
		}
	}
	_ = m.store.MarkArchived(ctx, s.ID, s.SlotID, expiry.Format(time.RFC3339Nano))
}

func (m *Manager) GC(ctx context.Context, dry bool) (int, error) {
	before := time.Now().Add(-m.Config().Retention.EndedWorktree.Duration).UTC().Format(time.RFC3339Nano)
	items, err := m.store.GCCandidates(ctx, before)
	if err != nil {
		return 0, err
	}
	if dry {
		return len(items), nil
	}
	count := 0
	root, _ := config.ExpandHome(m.Config().Storage.WorktreeRoot)
	for _, item := range items {
		if !domain.IsWithin(root, item.Path) {
			continue
		}
		repos, err := m.store.SlotRepositories(ctx, item.SlotID)
		if err != nil {
			continue
		}
		ok := true
		for _, sr := range repos {
			repo, err := m.store.Repository(ctx, sr.RepositoryID)
			if err != nil || !domain.IsWithin(root, sr.WorktreePath) {
				ok = false
				break
			}
			if err := m.archive.RemoveWorktree(ctx, repo, root, sr.WorktreePath); err != nil {
				ok = false
				break
			}
		}
		if ok {
			_ = os.Remove(item.Path)
			if err := m.store.MarkSlotArchived(ctx, item.SlotID); err == nil {
				count++
			}
		}
	}
	return count, nil
}

func (m *Manager) Status(ctx context.Context) (map[string]any, error) {
	s, err := m.store.Status(ctx)
	if err != nil {
		return nil, err
	}
	return map[string]any{"schema_version": 1, "protocol_version": 1, "uptime_seconds": int(time.Since(m.started).Seconds()), "config_path": must(config.Path()), "workspaces": s.Workspaces, "repositories": s.Repositories, "slots": map[string]int{"ready": s.Ready, "leased": s.Leased, "failed": s.Failed, "quarantined": s.Quarantined}, "active_sessions": s.Active, "snapshots": s.Snapshots, "queued_jobs": s.Jobs}, nil
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
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	m.mu.Lock()
	m.cfg = cfg
	m.mu.Unlock()
	return nil
}
func must(v string, e error) string {
	if e != nil {
		return ""
	}
	return v
}
func JSON(v any) json.RawMessage { b, _ := json.Marshal(v); return b }
