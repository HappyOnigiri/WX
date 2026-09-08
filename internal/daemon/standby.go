package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/HappyOnigiri/WX/internal/discovery"
	"github.com/HappyOnigiri/WX/internal/domain"
	"github.com/HappyOnigiri/WX/internal/pool"
	"github.com/HappyOnigiri/WX/internal/state"
)

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

func (m *Manager) ensureStandby(ctx context.Context, w discovery.Workspace) error {
	cfg := m.Config()
	if !m.standbyReplenishmentEnabled(w) {
		return nil
	}
	// clean 後の補充停止は永続化してあるため、定期 reconcile と補充ジョブの双方でここを通る。
	if m.replenishSuspended(ctx, string(w.ID)) {
		return nil
	}
	warmCount, _ := cfg.WarmCountForWorkspace(string(w.Root))
	needed := warmCount - m.store.StandbyCount(ctx, string(w.ID))
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
	// 進行中の貸出はまだ last_leased_at を書いていないことがある。その workspace の repository は hot として扱い、
	// 使用中の workspace へ COLD の待機枠を作らないようにする。
	leaseInFlight := m.workspaceLeaseInFlight(string(w.ID))
	cold := make([]string, 0, len(resolved))
	for _, r := range resolved {
		if leaseInFlight {
			hot[string(r.Repository.ID)] = true
			continue
		}
		if !hot[string(r.Repository.ID)] {
			cold = append(cold, string(r.Repository.ID))
		}
	}
	// COLD の待機枠は貸出時に cold start となるため、どの repository をどの基準で cold と判断したかを残す。
	m.log.Debug("standby replenishment decides hot or cold", "workspace_id", w.ID, "needed", needed, "hot_before", hotBefore, "cold_repositories", cold)
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

// RetryStandby は環境修復を利用者が確認した後、workspace の補充停止を解除して補充を一度だけ予約する。
// 停止理由（clean 由来か standby 失敗か）では区別せず、隔離 slot の状態・実体も変更しない。
func (m *Manager) RetryStandby(ctx context.Context, root string) (map[string]any, error) {
	canonical, err := domain.Canonicalize(root)
	if err != nil {
		return nil, err
	}
	w, err := m.store.WorkspaceByRoot(ctx, string(canonical))
	if err != nil {
		return nil, fmt.Errorf("find registered workspace %s: %w", canonical, err)
	}
	if !m.standbyReplenishmentEnabled(w) {
		return nil, errors.New("standby replenishment is disabled for this workspace")
	}
	retry, err := m.store.RetryStandbyReplenishment(ctx, string(w.ID))
	if err != nil {
		return nil, err
	}
	m.clearStandbySuspensionWarned(string(w.ID))
	scheduled := retry.Job.ID != "" && retry.Job.State == "PENDING"
	if scheduled {
		m.schedule(retry.Job)
	}
	return map[string]any{
		"workspace_id": w.ID, "root": w.Root, "generation": retry.Generation,
		"resumed": retry.Suspended, "job_id": retry.Job.ID, "scheduled": scheduled,
	}, nil
}

// markStandbySuspensionWarned は補充停止の警告をまだ出していない workspace で true を返し、以降は false を返す。
func (m *Manager) markStandbySuspensionWarned(workspaceID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.standbySuspensionWarned == nil {
		m.standbySuspensionWarned = map[string]bool{}
	}
	if m.standbySuspensionWarned[workspaceID] {
		return false
	}
	m.standbySuspensionWarned[workspaceID] = true
	return true
}

// clearStandbySuspensionWarned は停止が解除された workspace の記録を消し、再発時に再び警告できるようにする。
func (m *Manager) clearStandbySuspensionWarned(workspaceID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.standbySuspensionWarned, workspaceID)
}

func (m *Manager) standbyReplenishmentEnabled(w discovery.Workspace) bool {
	return m.standbyReplenishmentEnabledForRoot(string(w.Root))
}

// standbyReplenishmentEnabledForRoot は root path だけから補充の有無を判定する。診断は workspace 行しか持たないため、root で引ける形を分けている。
func (m *Manager) standbyReplenishmentEnabledForRoot(root string) bool {
	cfg := m.Config()
	warmCount, _ := cfg.WarmCountForWorkspace(root)
	return cfg.WorktreeMode(root) == "hot" && warmCount >= 1 && cfg.Retention.HotStandby.Duration > 0
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

// reserveStandbySlot は予約から登録までを1回分だけ行う。retry が true のときは ID 衝突なので、別の ID で呼び直せる。
// 予約中は reconcile の回収対象から外し、進行中の確保を中断扱いで隔離されないようにする。
func (m *Manager) reserveStandbySlot(ctx context.Context, id, rootID, relPath, slotPath, workspaceID string, generation, warmCount int, repos []state.SlotRepository) (state.Job, bool, error) {
	endReservation := m.beginReservation(id)
	defer endReservation()
	reserved, err := m.store.ReserveStandbyIfNeeded(ctx, state.Slot{ID: id, WorkspaceID: workspaceID, Generation: generation, RootID: rootID, RelPath: relPath}, warmCount)
	if err == nil && !reserved {
		return state.Job{}, false, nil
	}
	if state.IsIDCollision(err) {
		return state.Job{}, true, err
	}
	if err != nil {
		return state.Job{}, false, err
	}
	quarantineReservation := func() {
		if quarantineErr := m.store.QuarantineReservedSlot(context.Background(), id, "STANDBY_ALLOCATION_FAILED"); quarantineErr != nil {
			m.log.Error("quarantine failed standby reservation failed", "slot_id", id, "error", quarantineErr)
		}
	}
	slotIdentity, _, err := m.createSlotRoot(slotPath, slotPath)
	if err != nil {
		if errors.Is(err, errSlotPathExists) {
			return state.Job{}, false, errors.Join(err, m.store.AbandonSlotReservation(ctx, id))
		}
		quarantineReservation()
		return state.Job{}, false, err
	}
	if err := m.store.ConfirmSlotCreation(ctx, id, slotIdentity); err != nil {
		quarantineReservation()
		return state.Job{}, false, err
	}
	job, err := m.store.RegisterReservedStandby(ctx, id, repos)
	if err != nil {
		quarantineReservation()
		return state.Job{}, false, err
	}
	return job, false, nil
}

func (m *Manager) createStandbySlot(ctx context.Context, rootPath, rootID string, w discovery.Workspace, resolved []pool.Resolved, generation int, hot map[string]bool) (state.Job, error) {
	warmCount, _ := m.Config().WarmCountForWorkspace(string(w.Root))
	var lastErr error
	for range idAllocationAttempts {
		id, err := newSlotID()
		if err != nil {
			return state.Job{}, err
		}
		relPath, err := slotRelPath(string(w.ID), id)
		if err != nil {
			return state.Job{}, err
		}
		slotPath := filepath.Join(rootPath, relPath)
		repos, err := m.slotRepos(slotPath, w, resolved, generation, hot)
		if err != nil {
			return state.Job{}, err
		}
		if _, err := os.Lstat(slotPath); err == nil {
			lastErr = fmt.Errorf("%w: %s", errSlotPathExists, slotPath)
			continue
		} else if !errors.Is(err, os.ErrNotExist) {
			return state.Job{}, err
		}
		job, retry, err := m.reserveStandbySlot(ctx, id, rootID, relPath, slotPath, string(w.ID), generation, warmCount, repos)
		if retry {
			lastErr = err
			continue
		}
		return job, err
	}
	return state.Job{}, fmt.Errorf("create standby slot: %w", lastErr)
}
