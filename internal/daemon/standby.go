package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/HappyOnigiri/WX/internal/config"
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
	// 進行中の貸出はまだ last_leased_at を書いていないことがある。その workspace の repository は hot として扱い、
	// 使用中の workspace へ COLD の待機枠を作らないようにする。
	// 問い合わせの前後で確かめるのは、その間に貸出が終わると書き込みも進行中も見えなくなるためである。
	leaseInFlight := m.workspaceLeaseInFlight(string(w.ID))
	// HotRepositoryIDs は workspace を跨いで引くが、参照するのはこの workspace の repository だけなので、
	// cutoff もこの workspace の実効保持期間で作れば足りる。
	hotStandby, _ := cfg.HotStandbyForWorkspace(string(w.Root))
	hotBefore := state.FormatTime(time.Now().UTC().Add(-hotStandby))
	hot, err := m.store.HotRepositoryIDs(ctx, hotBefore)
	if err != nil {
		return err
	}
	leaseInFlight = leaseInFlight || m.workspaceLeaseInFlight(string(w.ID))
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

// RetryStandby は環境修復を利用者が確認した後、workspace の補充停止を停止理由によらず解除し、補充を一度だけ予約する。
// 準備に失敗した FAILED slot は待機枠に数えるため、残すと補充の不足が 0 になる。停止解除と併せて削除予約へ載せる。
// QUARANTINED slot の状態・実体は変更しない。
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
	// 停止解除を先に通す。clean 中はここで断られるので、拒否されたときは slot を触らない。
	retry, err := m.store.RetryStandbyReplenishment(ctx, string(w.ID))
	if err != nil {
		return nil, err
	}
	m.clearStandbySuspensionWarned(string(w.ID))
	// ENSURE_STANDBY の実行時点で FAILED が REMOVING になっているよう、補充の予約より先に回収を流す。
	removed := m.scheduleFailedStandbyRemoval(ctx, string(w.ID))
	scheduled := retry.Job.ID != "" && retry.Job.State == "PENDING"
	if scheduled {
		m.schedule(retry.Job)
	}
	return map[string]any{
		"workspace_id": w.ID, "root": w.Root, "generation": retry.Generation,
		"resumed": retry.Suspended, "job_id": retry.Job.ID, "scheduled": scheduled,
		"removed_failed": removed,
	}, nil
}

// RetryStandbyFailure は `--all` で 1 つの workspace の解除に失敗した理由を表す。
type RetryStandbyFailure struct {
	Root  string `json:"root"`
	Error string `json:"error"`
}

// RetryStandbyAllResult は `--all` の結果で、workspace ごとの応答と失敗の一覧を持つ。
// Workspaces の要素は RetryStandby の応答と同じ形である。
type RetryStandbyAllResult struct {
	Workspaces []map[string]any      `json:"workspaces"`
	Failures   []RetryStandbyFailure `json:"failures"`
}

// RetryStandbyAll は補充停止の記録があり、設定上まだ補充が有効な workspace を全て解除して、それぞれで補充を予約する。
// `wx clear` は補充の有無を問わず停止を記録するため、絞らないと RetryStandby が拒否する workspace を対象にしてしまう。
// 停止行を持たない補充計画の失敗は解除するものが無いので含めない。1 件の失敗では止めず、残りを処理して理由を集める。
func (m *Manager) RetryStandbyAll(ctx context.Context) (RetryStandbyAllResult, error) {
	suspended, err := m.store.StandbyReplenishmentDiagnostics(ctx)
	if err != nil {
		return RetryStandbyAllResult{}, err
	}
	result := RetryStandbyAllResult{Workspaces: []map[string]any{}, Failures: []RetryStandbyFailure{}}
	for _, item := range suspended {
		if !m.standbyReplenishmentEnabledForRoot(item.Root) {
			continue
		}
		reply, retryErr := m.RetryStandby(ctx, item.Root)
		if retryErr != nil {
			if errors.Is(retryErr, state.ErrCleanInProgress) {
				return result, retryErr
			}
			m.log.Error("standby replenishment retry failed", "root", item.Root, "error", retryErr)
			result.Failures = append(result.Failures, RetryStandbyFailure{Root: item.Root, Error: retryErr.Error()})
			continue
		}
		result.Workspaces = append(result.Workspaces, reply)
	}
	return result, nil
}

// scheduleFailedStandbyRemoval は workspace に残った FAILED slot を削除予約し、予約できた件数を返す。
// REMOVING は待機枠に数えないため、削除の完了を待たずに ENSURE_STANDBY が新しい slot を作れる。
// ScheduleQuarantinedRemoval は RUNNING job を持つ slot を弾く。準備が進行中で枠に数えるのが正しい状態なので、黙って残す。
func (m *Manager) scheduleFailedStandbyRemoval(ctx context.Context, workspaceID string) int {
	ids, err := m.store.FailedSlotIDs(ctx, workspaceID)
	if err != nil {
		m.log.Error("list failed standby slots failed", "workspace_id", workspaceID, "error", err)
		return 0
	}
	removed := 0
	for _, slotID := range ids {
		job, changed, err := m.store.ScheduleQuarantinedRemoval(ctx, slotID)
		if err != nil {
			// 停止解除は済んでいるので、残りと補充の予約まで進める。取りこぼしは retry の再実行で直せる。
			m.log.Error("failed standby removal scheduling failed", "slot_id", slotID, "error", err)
			continue
		}
		if !changed {
			continue
		}
		m.schedule(job)
		removed++
	}
	return removed
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
	// 保持期間も workspace の実効値で見る。global だけで判定すると、個別指定が 0 の workspace で
	// 補充だけが回り GC が即座に回収する往復になる。
	hotStandby, _ := cfg.HotStandbyForWorkspace(root)
	return cfg.WorktreeMode(root) == "hot" && warmCount >= 1 && hotStandby > 0
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
		repos, err := m.slotRepos(slotPath, w, resolved, generation, hot, config.PrepareOverride{})
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
