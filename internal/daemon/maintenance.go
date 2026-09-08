package daemon

import (
	"context"
	"fmt"
	"time"

	"github.com/HappyOnigiri/WX/internal/discovery"
	"github.com/HappyOnigiri/WX/internal/domain"
	"github.com/HappyOnigiri/WX/internal/pool"
	"github.com/HappyOnigiri/WX/internal/state"
)

const lifecycleCheckInterval = time.Second

func (m *Manager) maintainJobs() {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	lifecycle := time.NewTimer(lifecycleCheckInterval)
	lifecycle.Stop()
	defer lifecycle.Stop()
	armed := false
	rearm := func() {
		delay, pending := m.lifecycleCheckDelay()
		if !pending {
			if armed {
				if !lifecycle.Stop() {
					select {
					case <-lifecycle.C:
					default:
					}
				}
				armed = false
			}
			return
		}
		if armed {
			if !lifecycle.Stop() {
				select {
				case <-lifecycle.C:
				default:
				}
			}
		}
		lifecycle.Reset(delay)
		armed = true
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
			m.reconcileExpiredLeases(m.ctx)
			m.detectExecutableReplacement()
			m.runPendingLifecycle()
			rearm()
		}
	}
}

func (m *Manager) maintainLifecycle() {
	// 起動直後の Status を pending のままにしないため、重い reconcile より先に一度測る。
	m.measureRootUsage(m.ctx)
	m.resumeCleanRuns(m.ctx)
	m.reconcileStandbyReplenishments(m.ctx)
	m.reconcileArtifacts(m.ctx)
	m.reconcileOrphans(m.ctx)
	m.reconcileExpiredLeases(m.ctx)
	m.maybeBackup(m.ctx)
	m.runMaintenance()
	m.measureRootUsage(m.ctx)
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
			m.retryRootGeneration(m.ctx)
			m.reconcileStandbyReplenishments(m.ctx)
			m.reconcileArtifacts(m.ctx)
			m.reconcileOrphans(m.ctx)
			m.maybeBackup(m.ctx)
			m.runMaintenance()
			m.measureRootUsage(m.ctx)
		}
	}
}

// runMaintenance は registry reconcile と GC の一巡を実行する、定期保守と明示 reload に共通の経路である。
// 一巡は同時に1本しか走らせず、実行中に届いた要求は落とさずに畳んで、最新設定で追加の一巡を行う。
func (m *Manager) runMaintenance() {
	if !m.claimMaintenance() {
		return
	}
	for {
		m.mu.RLock()
		barrier := m.beforeMaintenanceSweep
		m.mu.RUnlock()
		if barrier != nil {
			barrier()
		}
		m.reconcileRegistry(m.ctx)
		m.runBackgroundGC()
		if !m.nextMaintenanceSweep() {
			return
		}
	}
}

// claimMaintenance は一巡の実行権を取る。既に走っていれば dirty を立てて false を返す。
func (m *Manager) claimMaintenance() bool {
	m.maintenanceMu.Lock()
	defer m.maintenanceMu.Unlock()
	if m.maintenanceRunning {
		m.maintenanceDirty = true
		return false
	}
	m.maintenanceRunning = true
	return true
}

// nextMaintenanceSweep は畳まれた要求が残っていれば実行権を保ったまま true を返し、なければ手放す。
func (m *Manager) nextMaintenanceSweep() bool {
	m.maintenanceMu.Lock()
	defer m.maintenanceMu.Unlock()
	if m.maintenanceDirty && m.ctx.Err() == nil {
		m.maintenanceDirty = false
		return true
	}
	m.maintenanceRunning = false
	m.maintenanceDirty = false
	return false
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

// backupDeadline は1回の online backup に与える総時間である。約24時間ごとの周期に対し十分短く取る。
const backupDeadline = 60 * time.Second

func (m *Manager) maybeBackup(ctx context.Context) {
	m.mu.RLock()
	last := m.lastBackup
	cfg := m.cfg
	m.mu.RUnlock()
	if !last.IsZero() && time.Since(last) < 24*time.Hour {
		return
	}
	// backup は writer を止めないため、並行書き込みで複製が繰り返し再走査されうる。
	// 無期限に居座らせず、期限内に終わらなければ失敗として次の周期へ回す。
	backupCtx, cancel := context.WithTimeout(ctx, backupDeadline)
	defer cancel()
	_, err := m.store.Backup(backupCtx, cfg.Storage.BackupGenerations, cfg.Storage.BackupRetention.Duration)
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
		if leaseCandidateRunning(candidate) {
			continue
		}
		if err := m.releaseLeaseWithoutToken(ctx, candidate, "orphan-reconcile"); err != nil {
			m.log.Error("lease release failed", "session_id", candidate.ID, "error", err)
		}
	}
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
