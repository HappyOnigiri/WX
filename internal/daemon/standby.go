package daemon

import (
	"context"
	"errors"
	"fmt"
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
	return cfg.WorktreeMode(root) == "hot" && cfg.Pool.WarmPerWorkspace >= 1 && cfg.Retention.HotStandby.Duration > 0
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
		relPath, err := slotRelPath(string(w.ID), id)
		if err != nil {
			return state.Job{}, err
		}
		slotPath := filepath.Join(rootPath, relPath)
		repos, err := m.slotRepos(slotPath, w, resolved, generation, hot)
		if err != nil {
			return state.Job{}, err
		}
		reserved, err := m.store.ReserveStandbyIfNeeded(ctx, state.Slot{ID: id, WorkspaceID: string(w.ID), Generation: generation, RootID: rootID, RelPath: relPath}, m.Config().Pool.WarmPerWorkspace)
		if err == nil && !reserved {
			return state.Job{}, nil
		}
		if state.IsIDCollision(err) {
			lastErr = err
			continue
		}
		if err != nil {
			return state.Job{}, err
		}
		quarantineReservation := func() {
			if quarantineErr := m.store.QuarantineReservedSlot(context.Background(), id, "STANDBY_ALLOCATION_FAILED"); quarantineErr != nil {
				m.log.Error("quarantine failed standby reservation failed", "slot_id", id, "error", quarantineErr)
			}
		}
		slotIdentity, _, err := m.createSlotRoot(slotPath, slotPath)
		if err != nil {
			quarantineReservation()
			return state.Job{}, err
		}
		if err := m.store.ConfirmSlotCreation(ctx, id, slotIdentity); err != nil {
			quarantineReservation()
			return state.Job{}, err
		}
		job, err := m.store.RegisterReservedStandby(ctx, id, repos)
		if err != nil {
			quarantineReservation()
			return state.Job{}, err
		}
		return job, nil
	}
	return state.Job{}, fmt.Errorf("create standby slot: %w", lastErr)
}
