package daemon

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/HappyOnigiri/WX/internal/archive"
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
	m.maybeCheckUpdate(m.ctx)
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
			m.maybeCheckUpdate(m.ctx)
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
		reuse, _ := m.Config().ReuseStandbyForWorkspace(string(workspaceRecord.Root))
		for _, slot := range readySlots {
			valid, validationErr := m.standbyReadyUsable(ctx, slot, workspaceRecord, resolved, reuse)
			if validationErr != nil || !valid {
				_ = m.store.SetSlotState(ctx, slot.ID, []string{"READY"}, "STALE", "READY_RECONCILE_FAILED")
				m.log.Warn("READY slot failed startup reconciliation", "slot_id", slot.ID, "error", validationErr)
			}
		}
		if err := m.ensureStandby(ctx, workspaceRecord); err != nil {
			m.log.Error("workspace standby reconcile failed", "workspace_id", workspaceRecord.ID, "error", err)
		}
		m.refreshIdleStandbys(ctx, workspaceRecord, resolved)
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

// ForgetResult は wx forget が登録を解除するまでに回収・破棄した件数である。
type ForgetResult struct {
	Root                        string `json:"root"`
	ReclaimedSlots              int    `json:"reclaimed_slots"`
	DiscardedSessions           int    `json:"discarded_sessions"`
	DiscardedSnapshots          int    `json:"discarded_snapshots"`
	DiscardedWorkspaceSnapshots int    `json:"discarded_workspace_snapshots"`
}

// Forget は workspace 登録を解除し、その前に自分で回収できる slot を同期的に回収する。
// 貸出中の実体が残る場合はフラグによらず断る。復元資産が残る場合は discardRecovery のときだけ破棄して進む。
func (m *Manager) Forget(ctx context.Context, path string, discardRecovery bool) (ForgetResult, error) {
	canonical, err := m.forgetRoot(ctx, path)
	if err != nil {
		return ForgetResult{}, err
	}
	result := ForgetResult{Root: string(canonical)}
	w, lookupErr := m.store.WorkspaceByRoot(ctx, string(canonical))
	if lookupErr != nil {
		// 登録を読めない場合は実体に触れず、同じ理由で失敗する ForgetWorkspace の検査へ委ねる。
		return result, m.store.ForgetWorkspace(ctx, string(canonical))
	}
	blockers, err := m.store.WorkspaceForgetBlockers(ctx, string(canonical))
	if err != nil {
		return result, err
	}
	// 断る理由は実体を1つも消す前に判定する。回収してから断ると、待機枠と補充だけが失われる。
	if err := blockers.Err(); err != nil && (!discardRecovery || errors.Is(err, state.ErrWorkspaceInUse)) {
		return result, err
	}
	// FinishRemoval は削除のたびに補充を予約するため、回収の前に止める。
	// 止めずに消すと hot な workspace では補充が走り、解除が実行中の job で落ちるか slot が復活する。
	if err := m.store.SuspendReplenish(ctx, string(w.ID), state.SuspendReplenishReasonForget, string(canonical)); err != nil {
		return result, err
	}
	if discardRecovery {
		if err := m.discardWorkspaceRecovery(ctx, string(w.ID), &result); err != nil {
			return result, err
		}
	}
	if err := m.reclaimSlotsForForget(ctx, string(w.ID), discardRecovery, &result); err != nil {
		return result, err
	}
	return result, m.store.ForgetWorkspace(ctx, string(canonical))
}

// reclaimSlotsForForget は forget が自分で消してよい slot の worktree を今すぐ回収する。
// 状態ごとに予約の経路が違うのは、検査すべき競合が違うためである。
func (m *Manager) reclaimSlotsForForget(ctx context.Context, workspaceID string, discardRecovery bool, result *ForgetResult) error {
	slots, err := m.store.ReclaimableSlots(ctx, workspaceID)
	if err != nil {
		return err
	}
	for _, slot := range slots {
		var job state.Job
		var changed bool
		switch slot.State {
		case "FAILED":
			job, changed, err = m.store.ScheduleFailedSlotRemoval(ctx, slot.ID)
		case "READY", "STALE":
			job, changed, err = m.store.ScheduleRemoval(ctx, slot.ID, "")
		case "SNAPSHOTTED":
			if !discardRecovery {
				continue
			}
			job, changed, err = m.store.ScheduleDiscardRemoval(ctx, slot.ID)
		case "QUARANTINED":
			if !discardRecovery {
				continue
			}
			job, changed, err = m.store.ScheduleQuarantinedRemoval(ctx, slot.ID)
		default:
			continue
		}
		if err != nil {
			return fmt.Errorf("reclaim %s slot %s before forgetting workspace: %w", slot.State, slot.ID, err)
		}
		reclaimed, err := m.runForgetRemoval(ctx, job, changed)
		if err != nil {
			return fmt.Errorf("reclaim %s slot %s before forgetting workspace: %w", slot.State, slot.ID, err)
		}
		if reclaimed {
			result.ReclaimedSlots++
		}
	}
	return nil
}

// runForgetRemoval は予約した REMOVE job をその場で実行する。
// 即時に消せなくても REMOVING job を残し、通常の recovery が完了するまで Forget を収束させない。
// 予約が取れなかった場合は他の回収が進んでいるので、失敗にせず false を返す。
func (m *Manager) runForgetRemoval(ctx context.Context, job state.Job, changed bool) (bool, error) {
	if !changed {
		return false, nil
	}
	claimed, err := m.store.ClaimJob(ctx, job.ID, "wx-forget")
	if err != nil {
		return false, err
	}
	if runErr := m.runRecoveredJob(ctx, claimed); runErr != nil {
		return false, runErr
	}
	return true, m.store.FinishJob(ctx, claimed.ID, "wx-forget", nil)
}

// discardWorkspaceRecovery は workspace に残る復元資産を破棄する。
// GC の期限切れ処理と違い、ソースリポジトリや保存先を開けなくても解除を止めず、消し残しは警告に出す。
// root ごと消えた workspace ではこの削除が必ず失敗し、止めると登録を永久に消せないためである。
func (m *Manager) discardWorkspaceRecovery(ctx context.Context, workspaceID string, result *ForgetResult) error {
	sessions, err := m.store.RecoveryStateSessions(ctx, workspaceID)
	if err != nil {
		return err
	}
	archiveManager := m.newArchiveManager(m.Config(), state.Slot{})
	for _, session := range sessions {
		snapshots, err := m.store.Snapshots(ctx, session.ID)
		if err != nil {
			return err
		}
		for _, snapshot := range snapshots {
			m.deleteForgottenSnapshotRefs(ctx, &archiveManager, snapshot)
			result.DiscardedSnapshots++
		}
		if m.removeForgottenWorkspaceSnapshot(ctx, session.ID) {
			result.DiscardedWorkspaceSnapshots++
		}
		if session.State == "QUARANTINED" {
			// 隔離した session は ARCHIVED を経ずに終端させる専用経路でしか行を消せない。
			if err := m.store.DiscardQuarantinedRecovery(ctx, session.ID); err != nil {
				return fmt.Errorf("discard quarantined recovery state of session %s: %w", session.ID, err)
			}
		} else if err := m.store.ExpireSessionSnapshots(ctx, session.ID); err != nil {
			return fmt.Errorf("discard recovery state of session %s: %w", session.ID, err)
		}
		result.DiscardedSessions++
	}
	return nil
}

// deleteForgottenSnapshotRefs はソースリポジトリ側の recovery ref を消す。失敗しても解除は続ける。
func (m *Manager) deleteForgottenSnapshotRefs(ctx context.Context, archiveManager *archive.Manager, snapshot state.Snapshot) {
	repo, err := m.store.Repository(ctx, snapshot.RepositoryID)
	if err != nil {
		m.log.Warn("forget could not read the repository of a discarded snapshot", "session_id", snapshot.SessionID, "repository_id", snapshot.RepositoryID, "error", err)
		return
	}
	submodules, submodulesErr := m.store.SubmoduleSnapshots(ctx, snapshot.SessionID, snapshot.RepositoryID)
	if submodulesErr != nil {
		m.log.Warn("forget could not read the submodule snapshots of a discarded snapshot", "session_id", snapshot.SessionID, "repository_id", snapshot.RepositoryID, "error", submodulesErr)
		return
	}
	if err := archiveManager.DeleteSnapshotRefs(ctx, repo, snapshot, submodules); err != nil {
		m.log.Warn("forget left recovery refs behind", "session_id", snapshot.SessionID, "repository_id", snapshot.RepositoryID, "error", err)
	}
}

// removeForgottenWorkspaceSnapshot は保存済みの workspace snapshot を消し、対象があったかを返す。
func (m *Manager) removeForgottenWorkspaceSnapshot(ctx context.Context, sessionID string) bool {
	snapshot, found, err := m.store.WorkspaceSnapshot(ctx, sessionID)
	if err != nil {
		m.log.Warn("forget could not read a workspace snapshot record", "session_id", sessionID, "error", err)
		return false
	}
	if !found {
		return false
	}
	owner := strings.TrimSuffix(snapshot.ArchivePath, string(filepath.Separator)+snapshot.RelPath)
	ownerHandle, err := os.OpenRoot(owner)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			m.log.Warn("forget left a workspace snapshot behind", "session_id", sessionID, "path", snapshot.ArchivePath, "error", err)
		}
		return true
	}
	defer func() { _ = ownerHandle.Close() }()
	if err := removeRegisteredSnapshot(ownerHandle, snapshot.RelPath); err != nil {
		m.log.Warn("forget left a workspace snapshot behind", "session_id", sessionID, "path", snapshot.ArchivePath, "error", err)
	}
	return true
}

// forgetRoot は forget 対象にする登録済み root path を決める。
// root ごと消えた登録はcanonical化できず、登録を消す経路はforgetしかないため、その場合だけ
// 絶対pathが`workspaces.root_path`と完全一致する登録を受け付ける。実体には触れない登録の削除に限る。
func (m *Manager) forgetRoot(ctx context.Context, path string) (domain.CanonicalPath, error) {
	canonical, err := domain.Canonicalize(path)
	if err == nil {
		return canonical, nil
	}
	absolute, absErr := filepath.Abs(path)
	if absErr != nil {
		return "", err
	}
	// 照合は登録時にcanonical化して保存した文字列との完全一致にし、打ち間違えたpathを黙って受け入れない。
	if _, lookupErr := m.store.WorkspaceByRoot(ctx, absolute); lookupErr != nil {
		return "", err
	}
	return domain.CanonicalPath(absolute), nil
}

// DiscardRecoveryTarget は破棄対象の session 1 件と、その session が使っていた slot である。
type DiscardRecoveryTarget struct {
	SessionID          string `json:"session_id"`
	SlotID             string `json:"slot_id"`
	SlotState          string `json:"slot_state"`
	SlotPath           string `json:"slot_path"`
	Snapshots          int    `json:"snapshots"`
	WorkspaceSnapshots int    `json:"workspace_snapshots"`
	Retired            bool   `json:"retired"`
}

// DiscardRecoveryResult は wx discard-recovery の結果である。DryRun のときは Targets だけを埋める。
type DiscardRecoveryResult struct {
	Root      string                  `json:"root"`
	DryRun    bool                    `json:"dry_run"`
	Targets   []DiscardRecoveryTarget `json:"targets"`
	Discarded int                     `json:"discarded"`
	Retired   int                     `json:"retired"`
}

// DiscardRecovery は workspace の QUARANTINED な復元資産を破棄し、その slot を ARCHIVED まで回収する。
// 対象は recovery ref を失って復元不能になった session だけなので、他の session の snapshot には触れない。
// 破棄後は wx forget の前提（session は EXPIRED、snapshot 行は無い、slot は ARCHIVED）が満たせる。
func (m *Manager) DiscardRecovery(ctx context.Context, path string, dryRun bool) (DiscardRecoveryResult, error) {
	canonical, err := domain.Canonicalize(path)
	if err != nil {
		return DiscardRecoveryResult{}, err
	}
	result := DiscardRecoveryResult{Root: string(canonical), DryRun: dryRun, Targets: []DiscardRecoveryTarget{}}
	if _, err := m.store.WorkspaceByRoot(ctx, string(canonical)); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return result, fmt.Errorf("%s is not a registered workspace", canonical)
		}
		return result, fmt.Errorf("look up workspace %s: %w", canonical, err)
	}
	sessions, err := m.store.QuarantinedRecoverySessions(ctx, string(canonical))
	if err != nil {
		return result, err
	}
	for _, session := range sessions {
		target := DiscardRecoveryTarget{
			SessionID: session.SessionID, SlotID: session.SlotID, SlotState: session.SlotState, SlotPath: session.SlotPath,
			Snapshots: session.Snapshots, WorkspaceSnapshots: session.WorkspaceSnapshots,
		}
		if dryRun {
			result.Targets = append(result.Targets, target)
			continue
		}
		if err := m.store.DiscardQuarantinedRecovery(ctx, session.SessionID); err != nil {
			return result, fmt.Errorf("discard quarantined recovery state of session %s: %w", session.SessionID, err)
		}
		result.Discarded++
		if session.SlotState == "QUARANTINED" {
			retired, err := m.retireQuarantinedSlot(ctx, session.SlotID)
			if err != nil {
				return result, fmt.Errorf("retire quarantined slot %s: %w", session.SlotID, err)
			}
			target.Retired = retired
			if retired {
				result.Retired++
			}
		}
		result.Targets = append(result.Targets, target)
	}
	return result, nil
}

// retireQuarantinedSlot は隔離 slot の worktree を今すぐ回収し、row を ARCHIVED へ進める。
// 予約が取れなかった場合は他の回収が進んでいるので、失敗にせず retired=false を返す。
func (m *Manager) retireQuarantinedSlot(ctx context.Context, slotID string) (bool, error) {
	job, changed, err := m.store.ScheduleQuarantinedRemoval(ctx, slotID)
	if err != nil {
		return false, err
	}
	if !changed {
		return false, nil
	}
	claimed, err := m.store.ClaimJob(ctx, job.ID, "wx-discard-recovery")
	if err != nil {
		return false, err
	}
	if runErr := m.runRecoveredJob(ctx, claimed); runErr != nil {
		return false, runErr
	}
	return true, m.store.FinishJob(ctx, claimed.ID, "wx-discard-recovery", nil)
}
