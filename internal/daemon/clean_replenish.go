package daemon

import (
	"context"
	"encoding/json"
)

// CleanReplenishResult は `--replenish` が再開した workspace と、再開できなかった理由の一覧である。
// Workspaces の要素は RetryStandby の応答と同じ形で、CLI は retry-standby と同じ表示を使う。
type CleanReplenishResult struct {
	Workspaces []map[string]any      `json:"workspaces"`
	Failures   []RetryStandbyFailure `json:"failures"`
}

// runCleanReplenish は閉じた run の補充停止を解除し、workspace ごとに補充を予約する。
// 引き受けは DB の compare-and-swap で 1 度に限るため、driver を何度起こしても再開は 1 回だけ行われる。
// 対象はこの run が記録した停止行から決める。target から作り直すと、受付時点の判定を再現できないためである。
// 1 件の失敗では止めず、理由を集めて結果に残す。黙って落とすと、停止が残ったまま戻ったと誤解される。
// commentlint:allow-long -- 再開が 1 回に限られる根拠と、対象と失敗の扱いを呼び出し側へ残す
func (m *Manager) runCleanReplenish(ctx context.Context, runID string, fromRunning bool) {
	claimed, err := m.store.ClaimCleanReplenish(ctx, runID, fromRunning)
	if err != nil {
		m.log.Error("claim clean replenishment failed", "run_id", runID, "error", err)
		return
	}
	if !claimed {
		return
	}
	result := m.cleanReplenishResult(ctx, runID)
	encoded, err := json.Marshal(result)
	if err != nil {
		m.log.Error("encode clean replenishment result failed", "run_id", runID, "error", err)
		encoded = []byte(`{"workspaces":[],"failures":[]}`)
	}
	if err := m.store.FinishCleanReplenish(ctx, runID, string(encoded)); err != nil {
		m.log.Error("finish clean replenishment failed", "run_id", runID, "error", err)
	}
}

// cleanReplenishResult は再開の対象を選び、workspace ごとに RetryStandby を通す。
// 失敗・隔離が残った workspace は戻さない。停止は「作って即消す往復」を防ぐためにあり、
// 環境の回復確認は `wx retry-standby` に委ねるのが既存の契約である。
// 補充が無効な workspace は RetryStandby がエラーにするので、事前に外して失敗として並べない。
// commentlint:allow-long -- 除外する 2 つの条件とその根拠を対にして残す
func (m *Manager) cleanReplenishResult(ctx context.Context, runID string) CleanReplenishResult {
	result := CleanReplenishResult{Workspaces: []map[string]any{}, Failures: []RetryStandbyFailure{}}
	workspaceIDs, err := m.store.CleanSuspendedWorkspaces(ctx, runID)
	if err != nil {
		m.log.Error("list clean replenishment targets failed", "run_id", runID, "error", err)
		return result
	}
	unhealthy := m.unhealthyCleanWorkspaces(ctx, runID)
	for _, workspaceID := range workspaceIDs {
		if unhealthy[workspaceID] {
			m.log.Info("standby replenishment stays stopped after a failed clear target", "run_id", runID, "workspace_id", workspaceID)
			continue
		}
		w, err := m.store.Workspace(ctx, workspaceID)
		if err != nil {
			result.Failures = append(result.Failures, RetryStandbyFailure{Root: workspaceID, Error: err.Error()})
			continue
		}
		if !m.standbyReplenishmentEnabledForRoot(string(w.Root)) {
			continue
		}
		reply, err := m.RetryStandby(ctx, string(w.Root))
		if err != nil {
			m.log.Error("clean replenishment retry failed", "root", w.Root, "error", err)
			result.Failures = append(result.Failures, RetryStandbyFailure{Root: string(w.Root), Error: err.Error()})
			continue
		}
		result.Workspaces = append(result.Workspaces, reply)
	}
	return result
}

// unhealthyCleanWorkspaces は失敗・隔離で終わった対象を持つ workspace を返す。SKIPPED は除外の理由にしない。
func (m *Manager) unhealthyCleanWorkspaces(ctx context.Context, runID string) map[string]bool {
	targets, err := m.store.CleanTargets(ctx, runID)
	if err != nil {
		m.log.Error("read clean targets for replenishment failed", "run_id", runID, "error", err)
		return nil
	}
	out := map[string]bool{}
	for _, target := range targets {
		if target.State == cleanTargetFailed || target.State == cleanTargetQuarantined {
			out[target.WorkspaceID] = true
		}
	}
	return out
}
