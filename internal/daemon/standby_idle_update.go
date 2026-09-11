package daemon

import (
	"context"
	"errors"
	"time"

	"github.com/HappyOnigiri/WX/internal/discovery"
	"github.com/HappyOnigiri/WX/internal/pool"
	"github.com/HappyOnigiri/WX/internal/state"
	"github.com/HappyOnigiri/WX/internal/workspace"
)

// idleStandbyRefreshCooldown は同じ workspace で idle 更新を始める最短間隔である。
// include 対象の書き換えのように短時間で何度も fingerprint が動く相手に対し、更新が連鎖しないための歯止めとして置く。
// 保守の周期（既定10分）より短くしてあるのは、reload 由来の追加の一巡でだけこの下限が効けばよいためである。
const idleStandbyRefreshCooldown = time.Minute

// refreshIdleStandbys は完全一致しないが更新適合な READY standby を、貸出を待たずに現在の要求へ合わせる。
// 貸出時の UPDATE 待ちが無くなり、`wx status` の READY が実態と一致する。
// 更新中は PREPARING で READY から外れるため、1巡1件・待機枠が全て READY・cooldown の3点で貸出を遅らせない。
func (m *Manager) refreshIdleStandbys(ctx context.Context, w discovery.Workspace, resolved []pool.Resolved) {
	if reuse, _ := m.Config().ReuseStandbyForWorkspace(string(w.Root)); !reuse {
		return
	}
	if !m.idleStandbyRefreshDue(string(w.ID)) || m.workspaceLeaseInFlight(string(w.ID)) {
		return
	}
	ready, err := m.store.ReadySlotCount(ctx, string(w.ID))
	if err != nil {
		m.log.Debug("idle standby refresh skipped", "workspace_id", w.ID, "error", err)
		return
	}
	// 補充途中（ALLOCATING・PREPARING・FAILED）の枠が残る間は更新を始めない。補充の完了を遅らせないためである。
	if ready == 0 || ready != m.store.StandbyCount(ctx, string(w.ID)) {
		return
	}
	candidates, err := m.store.ReadySlots(ctx, string(w.ID))
	if err != nil {
		m.log.Debug("idle standby refresh skipped", "workspace_id", w.ID, "error", err)
		return
	}
	for _, candidate := range candidates {
		matched, matchErr := m.readyMatches(ctx, candidate, resolved)
		// 一致しているものと、実体の検査に失敗したものは対象にしない。
		// 後者の回収は reconcile の standbyReadyUsable が判断する。
		if matchErr != nil || matched {
			continue
		}
		started, startErr := m.startIdleStandbyUpdate(ctx, w, candidate, resolved)
		if startErr != nil {
			// 更新に使えない候補は READY のまま残す。回収するかは貸出時の判断に委ねる。
			level := m.log.Debug
			if !errors.Is(startErr, workspace.ErrUpdateIneligible) {
				level = m.log.Info
			}
			level("idle standby update rejected before writes", "workspace_id", w.ID, "slot_id", candidate.ID, "reason", startErr)
			continue
		}
		if started {
			return
		}
	}
}

// startIdleStandbyUpdate は候補1件の更新を予約し、UPDATE job を積んだかを返す。
// 予約は READY slot の compare-and-swap なので、併走する貸出に先を越された場合は何もせず false を返す。
func (m *Manager) startIdleStandbyUpdate(ctx context.Context, w discovery.Workspace, slot state.Slot, resolved []pool.Resolved) (bool, error) {
	releaseRoot, err := m.holdRootForPath(slot.Path)
	if err != nil {
		return false, err
	}
	defer releaseRoot()
	plan, err := m.planStandbyUpdate(ctx, w, slot, resolved)
	if err != nil {
		return false, err
	}
	job, err := m.store.ReserveIdleStandbyUpdate(ctx, slot.ID, string(w.ID), plan.targets, plan.desired, m.Config().Storage.CopyMode)
	if err != nil {
		if standbyStateRace(err) {
			return false, nil
		}
		return false, err
	}
	m.markIdleStandbyRefresh(string(w.ID))
	m.log.Info("standby idle update reserved", append([]any{"workspace_id", w.ID, "slot_id", slot.ID}, plan.mismatch.logArgs()...)...)
	m.schedule(job)
	return true, nil
}

// idleStandbyRefreshDue は workspace の idle 更新を始めてよい時刻に達したかを返す。
func (m *Manager) idleStandbyRefreshDue(workspaceID string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	last, ok := m.idleStandbyRefreshes[workspaceID]
	return !ok || time.Since(last) >= idleStandbyRefreshCooldown
}

func (m *Manager) markIdleStandbyRefresh(workspaceID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.idleStandbyRefreshes == nil {
		m.idleStandbyRefreshes = map[string]time.Time{}
	}
	m.idleStandbyRefreshes[workspaceID] = time.Now()
}
