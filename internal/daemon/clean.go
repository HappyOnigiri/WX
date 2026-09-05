package daemon

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/HappyOnigiri/WX/internal/domain"
	"github.com/HappyOnigiri/WX/internal/state"
)

// cleanTerminationGrace は --all が使用中の session へ与える猶予である。
// 期限までに停止を確認できない対象は失敗として閉じ、強制終了はしない。
const cleanTerminationGrace = 30 * time.Second

// cleanBoundaryWait は準備・保存・削除途中の対象を安全な処理境界まで待つ上限である。
// 進行が止まった対象を無期限に待つと、clean 実行中は新規貸出を断るため workspace 全体が使えなくなる。
const cleanBoundaryWait = 5 * time.Minute

// cleanPollInterval は clean の進行監視の間隔である。既存ジョブの完了を監視するだけで worker は占有しない。
const cleanPollInterval = 200 * time.Millisecond

// cleanTargetState は clean_targets.state の値。遷移は Store の compare-and-swap で検証する。
const (
	cleanTargetPending     = "PENDING"
	cleanTargetTerminating = "TERMINATING"
	cleanTargetSaving      = "SAVING"
	cleanTargetRemoving    = "REMOVING"
	cleanTargetDone        = "DONE"
	cleanTargetFailed      = "FAILED"
	cleanTargetSkipped     = "SKIPPED"
	cleanTargetQuarantined = "QUARANTINED"
)

// sessionInUse は session が使用中かを返す。SNAPSHOTTED slot は owner を保持したまま終了済みなので、
// slot に owner が付いていることだけを使用中の根拠にしない。
func sessionInUse(sessionState string) bool {
	switch sessionState {
	case "STARTING", "ACTIVE", "RESTORING", "UNBOUND":
		return true
	default:
		return false
	}
}

// standbySlot は次の session を待つ待機用 slot（ホットスタンバイ）かを返す。補充途中の PREPARING も含める。
// owner が付いた slot は貸出済みで、世代遅れの STALE は再利用されない残骸なので、どちらも待機用としては扱わない。
func standbySlot(candidate state.CleanCandidate) bool {
	return candidate.SessionID == "" && (candidate.SlotState == "READY" || candidate.SlotState == "PREPARING")
}

// cleanMode は run へ永続化する mode を返す。--all は待機用 worktree の削除も含むため、その組み合わせは独立した mode にしない。
func cleanMode(all, standby bool) string {
	switch {
	case all:
		return "all"
	case standby:
		return "standby"
	default:
		return "normal"
	}
}

// planCleanTargets は受付時点の候補から対象と除外理由を確定する。
// 通常の clean は使用中の session と待機用 slot を対象外とし、--standby は待機用を、--all は加えて起動・復元途中も含める。
func planCleanTargets(candidates []state.CleanCandidate, all, standby bool) []state.CleanTarget {
	out := make([]state.CleanTarget, 0, len(candidates))
	for _, candidate := range candidates {
		target := state.CleanTarget{
			SlotID: candidate.SlotID, WorkspaceID: candidate.WorkspaceID,
			SessionID: candidate.SessionID, Path: candidate.Path, State: cleanTargetPending,
		}
		switch {
		case candidate.SlotState == "QUARANTINED":
			target.State, target.Reason = cleanTargetQuarantined, "slot is quarantined; wx keeps artifacts whose ownership it cannot prove"
		case standbySlot(candidate):
			if !all && !standby {
				target.State, target.Reason = cleanTargetSkipped, "standby worktree is not in use; rerun with --standby to delete it"
			}
		case !sessionInUse(candidate.SessionState):
			// 終了済み・無効化された未使用 slot はそのまま削除経路へ進む。
		case !all:
			target.State, target.Reason = cleanTargetSkipped, "session "+candidate.SessionID+" is in use; rerun with --all to ask it to stop"
		default:
			if reason := unsavedDataRisk(candidate); reason != "" {
				target.State, target.Reason = cleanTargetSkipped, reason
			}
		}
		out = append(out, target)
	}
	return out
}

// unsavedDataRisk は、使用中の slot を停止後に削除してよいと証明できない理由を返す。証明できる場合は空文字を返す。
func unsavedDataRisk(candidate state.CleanCandidate) string {
	switch candidate.SessionState {
	case "UNBOUND":
		// workspace 未確定の resume slot は bind 前で worktree を持たない。repository 行があれば証明が崩れている。
		if candidate.Repositories != 0 {
			return "unbound slot already has repositories; wx cannot prove it holds no unsaved work"
		}
		return ""
	case "RESTORING":
		// 復元途中の内容は復元元 session の snapshot が持つ。snapshot が無ければ復元可能な形式が残らない。
		if candidate.ParentSnapshots == 0 {
			return "restoring slot has no recovery snapshot to fall back on"
		}
		return ""
	default:
		return ""
	}
}

// cleanWorkspaces は補充を停止すべき workspace を、実際に削除へ進む対象から集める。
// 待機用 worktree を残すモードでは停止しない。残すと決めた以上、補充で数が戻ることは矛盾しないためである。
func cleanWorkspaces(targets []state.CleanTarget, removeStandby bool) []string {
	if !removeStandby {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	for _, target := range targets {
		if target.State != cleanTargetPending || target.WorkspaceID == "" || seen[target.WorkspaceID] {
			continue
		}
		seen[target.WorkspaceID] = true
		out = append(out, target.WorkspaceID)
	}
	return out
}

// cleanSummary は対象一覧を状態別に集計する。CLI の表示と終了コードの根拠になる。
func cleanSummary(targets []state.CleanTarget) map[string]int {
	summary := map[string]int{"total": len(targets)}
	for _, key := range []string{cleanTargetPending, cleanTargetTerminating, cleanTargetSaving, cleanTargetRemoving, cleanTargetDone, cleanTargetFailed, cleanTargetSkipped, cleanTargetQuarantined} {
		summary[key] = 0
	}
	for _, target := range targets {
		summary[target.State]++
	}
	return summary
}

func cleanReply(run state.CleanRun, targets []state.CleanTarget, dryRun bool) map[string]any {
	return map[string]any{
		"run_id": run.ID, "mode": run.Mode, "state": run.State,
		"dry_run": dryRun, "targets": targets, "summary": cleanSummary(targets),
	}
}

// Clean は保持期限を待たずに wx 管理下の worktree を削除する。
// dry-run は終了要求・補充停止・ジョブ登録・隔離を含め一切状態を変更せず、調査時点の見込みだけを返す。
func (m *Manager) Clean(ctx context.Context, all, standby, dryRun bool) (map[string]any, error) {
	candidates, err := m.store.CleanCandidates(ctx)
	if err != nil {
		return nil, err
	}
	mode := cleanMode(all, standby)
	targets := planCleanTargets(candidates, all, standby)
	if dryRun {
		return cleanReply(state.CleanRun{Mode: mode, State: "DRY_RUN"}, targets, true), nil
	}
	id, err := domain.NewShortID()
	if err != nil {
		return nil, err
	}
	runID, joined, err := m.store.BeginCleanRun(ctx, id, mode, targets, cleanWorkspaces(targets, mode != "normal"))
	if err != nil {
		return nil, err
	}
	if joined {
		m.log.Info("joined the clean already in progress", "run_id", runID, "mode", mode)
	}
	m.startCleanDriver(runID)
	run, stored, err := m.store.CleanRunByID(ctx, runID)
	if err != nil {
		return nil, err
	}
	return cleanReply(run, stored, false), nil
}

// CleanStatus は run 1 件の現在の進捗を返す。CLI は短い RPC の繰り返しで完了まで待つ。
func (m *Manager) CleanStatus(ctx context.Context, runID string) (map[string]any, error) {
	run, targets, err := m.store.CleanRunByID(ctx, runID)
	if err != nil {
		return nil, err
	}
	return cleanReply(run, targets, false), nil
}

// resumeCleanRuns は daemon 再起動後に、受付済みの run を同じ対象と期限で再開する。
func (m *Manager) resumeCleanRuns(ctx context.Context) {
	runs, err := m.store.RunningCleanRuns(ctx)
	if err != nil {
		m.log.Error("resume clean runs failed", "error", err)
		return
	}
	for _, run := range runs {
		m.startCleanDriver(run.ID)
	}
}

// startCleanDriver は run ごとに 1 つだけ進行管理を走らせる。同じ run への再実行は既存の driver に合流する。
func (m *Manager) startCleanDriver(runID string) {
	m.mu.Lock()
	if m.cleanDrivers == nil {
		m.cleanDrivers = map[string]bool{}
	}
	if m.cleanDrivers[runID] {
		m.mu.Unlock()
		return
	}
	m.cleanDrivers[runID] = true
	m.mu.Unlock()
	started := m.startBackground(func() {
		defer func() {
			m.mu.Lock()
			delete(m.cleanDrivers, runID)
			m.mu.Unlock()
		}()
		m.driveClean(runID)
	})
	if !started {
		m.mu.Lock()
		delete(m.cleanDrivers, runID)
		m.mu.Unlock()
	}
}

func (m *Manager) driveClean(runID string) {
	waiting := map[string]time.Time{}
	ticker := time.NewTicker(cleanPollInterval)
	defer ticker.Stop()
	for {
		done, err := m.advanceClean(m.ctx, runID, waiting)
		if err != nil && m.ctx.Err() == nil {
			m.log.Error("clean progress failed", "run_id", runID, "error", err)
		}
		if done {
			m.log.Info("clean finished", "run_id", runID)
			return
		}
		select {
		case <-m.ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// advanceClean は各対象を 1 段だけ進め、run 全体が閉じたかを返す。
func (m *Manager) advanceClean(ctx context.Context, runID string, waiting map[string]time.Time) (bool, error) {
	run, targets, err := m.store.CleanRunByID(ctx, runID)
	if err != nil {
		return false, err
	}
	if run.State != state.CleanRunRunning {
		return true, nil
	}
	for _, target := range targets {
		switch target.State {
		case cleanTargetDone, cleanTargetFailed, cleanTargetSkipped, cleanTargetQuarantined:
			continue
		case cleanTargetTerminating:
			m.advanceTerminating(ctx, run, target)
		case cleanTargetRemoving:
			m.advanceRemoving(ctx, run, target)
		default:
			m.advancePending(ctx, run, target, waiting)
		}
	}
	return m.store.FinishCleanRun(ctx, runID)
}

// sessionStateOf は slot を占有する session の状態を返す。session が無ければ空文字を返す。
func (m *Manager) sessionStateOf(ctx context.Context, sessionID string) string {
	if sessionID == "" {
		return ""
	}
	session, err := m.store.SessionByID(ctx, sessionID)
	if err != nil {
		return ""
	}
	return session.State
}

// advancePending は使用中なら終了要求へ、未使用なら削除ジョブへ進める。
// 準備・保存・削除途中の対象は既存ジョブと競合せず、安全な処理境界まで待つ。
func (m *Manager) advancePending(ctx context.Context, run state.CleanRun, target state.CleanTarget, waiting map[string]time.Time) {
	slot, err := m.store.Slot(ctx, target.SlotID)
	if err != nil {
		m.failCleanTarget(ctx, run.ID, target, "slot record is unreadable: "+err.Error())
		return
	}
	sessionState := m.sessionStateOf(ctx, target.SessionID)
	switch {
	case slot.State == "ARCHIVED":
		m.moveCleanTarget(ctx, run.ID, target, cleanTargetDone, "")
		return
	case slot.State == "QUARANTINED":
		m.moveCleanTarget(ctx, run.ID, target, cleanTargetQuarantined, "slot was quarantined before removal; wx kept the artifact")
		return
	case sessionInUse(sessionState):
		if run.Mode != "all" {
			m.moveCleanTarget(ctx, run.ID, target, cleanTargetSkipped, "session "+target.SessionID+" is in use")
			return
		}
		requestID, idErr := domain.NewID()
		if idErr != nil {
			return
		}
		if err := m.store.RequestSessionTermination(ctx, run.ID, target.SlotID, target.SessionID, requestID, time.Now().Add(cleanTerminationGrace)); err != nil {
			m.log.Debug("request session termination deferred", "run_id", run.ID, "slot_id", target.SlotID, "error", err)
		}
		return
	case slot.State == "REMOVING":
		// 既存の削除ジョブが動いているので、重複登録せず完了だけを監視する。
		m.moveCleanTarget(ctx, run.ID, target, cleanTargetRemoving, "")
		return
	case slot.State == "READY" || slot.State == "STALE" || slot.State == "SNAPSHOTTED":
		if m.scheduleRemovalCandidate(ctx, slot.ID, slot.Path, target.SessionID, "clean removal scheduling failed") == 1 {
			m.moveCleanTarget(ctx, run.ID, target, cleanTargetRemoving, "")
			return
		}
	case slot.State == "FAILED":
		job, changed, scheduleErr := m.store.ScheduleFailedSlotRemoval(ctx, slot.ID)
		if scheduleErr != nil {
			m.log.Error("clean failed-slot removal scheduling failed", "slot_id", slot.ID, "error", scheduleErr)
			return
		}
		if changed {
			m.schedule(job)
			m.moveCleanTarget(ctx, run.ID, target, cleanTargetRemoving, "")
			return
		}
	}
	m.waitForBoundary(ctx, run.ID, target, waiting, slot.State)
}

// waitForBoundary は進行中の対象を待ち、上限を超えたら失敗として閉じる。
func (m *Manager) waitForBoundary(ctx context.Context, runID string, target state.CleanTarget, waiting map[string]time.Time, slotState string) {
	since, seen := waiting[target.SlotID]
	if !seen {
		waiting[target.SlotID] = time.Now()
		return
	}
	if time.Since(since) < cleanBoundaryWait {
		return
	}
	m.failCleanTarget(ctx, runID, target, fmt.Sprintf("slot stayed in %s for more than %s", slotState, cleanBoundaryWait))
}

// advanceTerminating は停止の確認と期限切れを判定する。応答しない旧 client も、自然終了を確認できれば保存経路へ進む。
func (m *Manager) advanceTerminating(ctx context.Context, run state.CleanRun, target state.CleanTarget) {
	if !sessionInUse(m.sessionStateOf(ctx, target.SessionID)) {
		m.closePendingTermination(ctx, target.SessionID, "CONFIRMED")
		m.moveCleanTarget(ctx, run.ID, target, cleanTargetSaving, "")
		return
	}
	deadline, err := time.Parse(time.RFC3339Nano, target.TerminateDeadline)
	if err != nil || time.Now().Before(deadline) {
		return
	}
	// 期限切れ後に遅れて終了しても、この要求では即時削除せず通常の返却処理に戻す。
	m.closePendingTermination(ctx, target.SessionID, "TIMED_OUT")
	m.failCleanTarget(ctx, run.ID, target, fmt.Sprintf("session %s did not stop within %s", target.SessionID, cleanTerminationGrace))
}

// closePendingTermination は未応答の終了要求を、記録された要求 ID のまま閉じる。
func (m *Manager) closePendingTermination(ctx context.Context, sessionID, to string) {
	request, found, err := m.store.PendingTermination(ctx, sessionID)
	if err != nil || !found {
		return
	}
	if err := m.store.FinishTermination(ctx, sessionID, request.RequestID, to); err != nil {
		m.log.Debug("close termination request failed", "session_id", sessionID, "to", to, "error", err)
	}
}

// advanceRemoving は削除ジョブの結果を判定する。隔離された対象は実体を残したまま失敗として閉じる。
func (m *Manager) advanceRemoving(ctx context.Context, run state.CleanRun, target state.CleanTarget) {
	slot, err := m.store.Slot(ctx, target.SlotID)
	if err != nil {
		m.failCleanTarget(ctx, run.ID, target, "slot record is unreadable: "+err.Error())
		return
	}
	switch slot.State {
	case "ARCHIVED":
		m.moveCleanTarget(ctx, run.ID, target, cleanTargetDone, "")
	case "QUARANTINED":
		m.moveCleanTarget(ctx, run.ID, target, cleanTargetQuarantined, "ownership could not be proven; the artifact was quarantined instead of deleted")
	}
}

func (m *Manager) moveCleanTarget(ctx context.Context, runID string, target state.CleanTarget, to, reason string) {
	from := []string{cleanTargetPending, cleanTargetTerminating, cleanTargetSaving, cleanTargetRemoving}
	if err := m.store.SetCleanTargetState(ctx, runID, target.SlotID, from, to, reason); err != nil {
		m.log.Debug("clean target transition skipped", "run_id", runID, "slot_id", target.SlotID, "to", to, "error", err)
	}
}

func (m *Manager) failCleanTarget(ctx context.Context, runID string, target state.CleanTarget, reason string) {
	m.moveCleanTarget(ctx, runID, target, cleanTargetFailed, reason)
}

// ConfirmTermination は client からの停止確認を受け取り、通常の返却経路へ進める。
// daemon は記録された PID へ signal を送らず、停止したことは session token を持つ client の応答だけを根拠にする。
func (m *Manager) ConfirmTermination(ctx context.Context, id, token, requestID string) error {
	if _, err := m.store.Session(ctx, id, token); err != nil {
		return err
	}
	if err := m.store.FinishTermination(ctx, id, requestID, "CONFIRMED"); err != nil {
		return err
	}
	return m.Release(ctx, id, token, "clean-terminate")
}

// terminationReply は heartbeat と agent 登録の応答へ、有効な終了要求を載せる。
// 起動と要求が競合しても、登録応答で受け取った client が起動直後のプロセスを終了させられる。
func (m *Manager) terminationReply(ctx context.Context, sessionID string, base map[string]any) map[string]any {
	request, found, err := m.store.PendingTermination(ctx, sessionID)
	if err != nil || !found {
		return base
	}
	base["terminate"] = map[string]any{"request_id": request.RequestID, "deadline": request.Deadline}
	return base
}

// replenishSuspended は workspace の待機用 worktree 補充が停止中かを返す。
// 読み取りに失敗したときは補充を止める側へ倒し、clean 直後の再生成を避ける。
func (m *Manager) replenishSuspended(ctx context.Context, workspaceID string) bool {
	if workspaceID == "" {
		return false
	}
	suspended, err := m.store.ReplenishSuspended(ctx, workspaceID)
	if err != nil {
		m.log.Error("read replenish suspension failed", "workspace_id", workspaceID, "error", err)
		return true
	}
	return suspended
}

// resumeReplenish は clean 後に貸出・resume が成功した workspace の補充を再開する。
func (m *Manager) resumeReplenish(ctx context.Context, workspaceID string) {
	if workspaceID == "" {
		return
	}
	if err := m.store.ResumeReplenish(ctx, workspaceID); err != nil {
		m.log.Error("resume standby replenishment failed", "workspace_id", workspaceID, "error", err)
	}
}

// IsCleanConflict は clean 実行中に断られた要求かを返す。CLI はこの区別で再実行を案内する。
func IsCleanConflict(err error) bool {
	return errors.Is(err, state.ErrCleanInProgress) || errors.Is(err, state.ErrCleanModeConflict)
}
