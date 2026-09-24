package daemon

import (
	"context"
	"errors"
	"fmt"

	"github.com/HappyOnigiri/WorktreeX/internal/discovery"
	"github.com/HappyOnigiri/WorktreeX/internal/domain"
	"github.com/HappyOnigiri/WorktreeX/internal/pool"
	"github.com/HappyOnigiri/WorktreeX/internal/state"
	"github.com/HappyOnigiri/WorktreeX/internal/workspace"
)

func (m *Manager) leaseMatchingReady(ctx context.Context, w discovery.Workspace, ready state.Slot, agent string, pid int, attrs leaseAttrs) (Lease, bool, error) {
	releaseRoot, err := m.holdRootForPath(ready.Path)
	if err != nil {
		return Lease{}, false, err
	}
	defer releaseRoot()
	repositories, err := m.store.SlotRepositories(ctx, ready.ID)
	if err != nil {
		return Lease{}, false, err
	}
	leasePathValue := leasePath(ready.Path, w.Kind, repositories)
	rootIdentity, err := m.ensureLeaseRoot(ready.Path, leasePathValue)
	if err != nil {
		_ = m.store.SetSlotState(context.Background(), ready.ID, []string{"READY"}, "QUARANTINED", "LEASE_ROOT_OWNERSHIP_UNCERTAIN")
		return Lease{}, false, fmt.Errorf("pin ready lease root: %w", err)
	}
	token, err := state.TokenHex()
	if err != nil {
		return Lease{}, false, err
	}
	hasCold := false
	for _, repository := range repositories {
		hasCold = hasCold || repository.State == "COLD"
	}
	sessionState := "ACTIVE"
	if hasCold {
		sessionState = "STARTING"
	}
	session := state.Session{ID: ready.ID, WorkspaceID: string(w.ID), SlotID: ready.ID, State: sessionState, AgentKind: agent, ClientPID: pid, TokenHash: state.HashToken(token)}
	m.applyLeaseAttrs(&session, attrs)
	if err := m.retainLease(session.ID, leasePathValue); err != nil {
		return Lease{}, false, err
	}
	if hasCold {
		job, err := m.store.LeaseReadyWithCold(ctx, ready.ID, session)
		if err != nil {
			m.releaseLease(session.ID)
			return Lease{}, false, err
		}
		m.schedule(job)
		return m.withReadiness(Lease{SessionID: session.ID, Token: token, Path: leasePathValue, RootIdentity: rootIdentity, SourceWorkspace: string(w.Root), Ready: false, RepositoryDirs: leaseRepositoryDirs(ready.Path, leasePathValue, repositories), Route: RouteColdStart}, w), true, nil
	}
	job, replenished, err := m.store.LeaseReadyWithReplenishment(ctx, ready.ID, session)
	if err != nil {
		m.releaseLease(session.ID)
		return Lease{}, false, err
	}
	m.handleNormalSessionSuccess(ctx, w, job, replenished)
	return m.withReadiness(Lease{SessionID: session.ID, Token: token, Path: leasePathValue, RootIdentity: rootIdentity, SourceWorkspace: string(w.Root), Ready: true, RepositoryDirs: leaseRepositoryDirs(ready.Path, leasePathValue, repositories), Route: RouteReady}, w), true, nil
}

// standbyUpdatePlan は READY standby を要求内容へ更新するための、予約前に確定した入力一式である。
// mismatch は予約後に再計算できないため、判定に使った値から組み立てて持ち回る。
type standbyUpdatePlan struct {
	repositories []state.SlotRepository
	targets      []state.SlotRepository
	desired      []state.Placement
	mismatch     readyMismatch
}

// planStandbyUpdate は READY standby の更新可否を検証し、予約に渡す target と配置を組み立てる。
// 実体には触れず、適合しない場合は workspace.ErrUpdateIneligible などを返す。
// 呼び出し側は root を保持してから呼ぶこと。貸出予約と idle 更新で判定をずらさないために共有する。
func (m *Manager) planStandbyUpdate(ctx context.Context, w discovery.Workspace, slot state.Slot, resolved []pool.Resolved) (standbyUpdatePlan, error) {
	if !slot.PlacementHistoryComplete || slot.OwnerSessionID != "" || slot.Generation == 0 {
		return standbyUpdatePlan{}, fmt.Errorf("%w: standby has no complete placement history", workspace.ErrUpdateIneligible)
	}
	repositories, err := m.store.SlotRepositories(ctx, slot.ID)
	if err != nil {
		return standbyUpdatePlan{}, err
	}
	if len(repositories) != len(resolved) {
		return standbyUpdatePlan{}, fmt.Errorf("%w: workspace repository set changed", workspace.ErrUpdateIneligible)
	}
	storedByID := make(map[string]state.SlotRepository, len(repositories))
	for _, repository := range repositories {
		if repository.State != "READY" || repository.CompatibilityFingerprint == "" {
			return standbyUpdatePlan{}, fmt.Errorf("%w: standby contains an unmaterialized or legacy repository", workspace.ErrUpdateIneligible)
		}
		storedByID[repository.RepositoryID] = repository
	}
	previous, err := m.store.Placements(ctx, slot.ID)
	if err != nil {
		return standbyUpdatePlan{}, err
	}
	preparer := m.newPreparer(m.Config(), slot)
	plan := standbyUpdatePlan{repositories: repositories}
	for _, requested := range resolved {
		stored, ok := storedByID[string(requested.Repository.ID)]
		if !ok {
			return standbyUpdatePlan{}, fmt.Errorf("%w: workspace repository set changed", workspace.ErrUpdateIneligible)
		}
		compatibility, err := workspace.UpdateCompatibilityFingerprintWithGit(ctx, m.git, slot.Generation, requested.Repository, m.Config())
		if err != nil {
			return standbyUpdatePlan{}, err
		}
		if compatibility != stored.CompatibilityFingerprint {
			return standbyUpdatePlan{}, fmt.Errorf("%w: standby preparation conditions changed", workspace.ErrUpdateIneligible)
		}
		fingerprint, err := workspace.FingerprintWithGit(ctx, m.git, slot.Generation, requested.OID, requested.Repository, m.Config())
		if err != nil {
			return standbyUpdatePlan{}, err
		}
		planned, err := preparer.RepositoryPlacements(ctx, requested.Repository, requested.OID)
		if err != nil {
			return standbyUpdatePlan{}, err
		}
		oldRepositoryPlacements := placementsFor(previous, stored.RepositoryID)
		if err := preparer.ValidateUpdateCandidate(ctx, requested.Repository, stored.WorktreePath, stored.BaseOID, requested.OID, oldRepositoryPlacements, planned); err != nil {
			return standbyUpdatePlan{}, err
		}
		if plan.mismatch.reason == "" {
			plan.mismatch = updateMismatch(stored, requested, fingerprint, oldRepositoryPlacements, planned)
		}
		plan.desired = append(plan.desired, planned...)
		plan.targets = append(plan.targets, state.SlotRepository{RepositoryID: stored.RepositoryID, RequestedRef: requested.RequestedRef, BaseOID: requested.OID, Fingerprint: fingerprint, CompatibilityFingerprint: compatibility, UpdateBaseOID: stored.BaseOID, UpdateFingerprint: stored.Fingerprint})
	}
	if w.Kind == "multi_repository" {
		rootRules, err := m.rootRules(w)
		if err != nil {
			return standbyUpdatePlan{}, err
		}
		planned, err := workspace.RootPlacements(string(w.Root), rootRules)
		if err != nil {
			return standbyUpdatePlan{}, err
		}
		destination, err := domain.OpenRootAt(preparer.OwnedRoot, slot.RelPath)
		if err != nil {
			return standbyUpdatePlan{}, err
		}
		validateErr := workspace.ValidateRootPlacements(destination, placementsFor(previous, ""), planned)
		_ = destination.Close()
		if validateErr != nil {
			return standbyUpdatePlan{}, validateErr
		}
		plan.desired = append(plan.desired, planned...)
	}
	return plan, nil
}

func (m *Manager) leaseUpdatingStandby(ctx context.Context, w discovery.Workspace, slot state.Slot, resolved []pool.Resolved, agent string, pid int, attrs leaseAttrs) (Lease, bool, error) {
	releaseRoot, err := m.holdRootForPath(slot.Path)
	if err != nil {
		return Lease{}, false, err
	}
	defer releaseRoot()
	plan, err := m.planStandbyUpdate(ctx, w, slot, resolved)
	if err != nil {
		return Lease{}, false, err
	}
	repositories, targets, desired, mismatch := plan.repositories, plan.targets, plan.desired, plan.mismatch
	leasePathValue := leasePath(slot.Path, w.Kind, repositories)
	rootIdentity, err := m.ensureLeaseRoot(slot.Path, leasePathValue)
	if err != nil {
		return Lease{}, false, err
	}
	token, err := state.TokenHex()
	if err != nil {
		return Lease{}, false, err
	}
	session := state.Session{ID: slot.ID, WorkspaceID: string(w.ID), SlotID: slot.ID, State: "STARTING", AgentKind: agent, ClientPID: pid, TokenHash: state.HashToken(token)}
	m.applyLeaseAttrs(&session, attrs)
	if err := m.retainLease(session.ID, leasePathValue); err != nil {
		return Lease{}, false, err
	}
	job, err := m.store.ReserveStandbyUpdate(ctx, slot.ID, session, targets, desired, m.Config().RepositoryDefaults.Storage.CopyMode)
	if err != nil {
		m.releaseLease(session.ID)
		if standbyStateRace(err) {
			return Lease{}, false, nil
		}
		return Lease{}, false, err
	}
	m.log.Info("standby update reserved", append([]any{"workspace_id", w.ID, "slot_id", slot.ID}, mismatch.logArgs()...)...)
	m.schedule(job)
	return m.withReadiness(Lease{SessionID: session.ID, Token: token, Path: leasePathValue, RootIdentity: rootIdentity, SourceWorkspace: string(w.Root), Ready: false, RepositoryDirs: leaseRepositoryDirs(slot.Path, leasePathValue, repositories), Route: RouteUpdate}, w), true, nil
}

// standbyStateRace は候補を奪われただけの一時的な失敗かを返す。slotの状態は変えず次の候補へ回す。
func standbyStateRace(err error) bool {
	return err != nil && (errors.Is(err, state.ErrSlotStateIneligible) || errors.Is(err, state.ErrStandbyNotUpdateable))
}

func placementsFor(placements []state.Placement, repositoryID string) []state.Placement {
	var out []state.Placement
	for _, placement := range placements {
		if placement.RepositoryID == repositoryID {
			out = append(out, placement)
		}
	}
	return out
}

func (m *Manager) standbyStoredStateValid(ctx context.Context, slot state.Slot, w discovery.Workspace) (bool, error) {
	repositories, err := m.store.SlotRepositories(ctx, slot.ID)
	if err != nil || len(repositories) != len(w.Repositories) {
		return false, err
	}
	byID := make(map[string]discovery.Repository, len(w.Repositories))
	for _, repository := range w.Repositories {
		byID[string(repository.ID)] = repository
	}
	preparer := m.newPreparer(m.Config(), slot)
	placements, err := m.store.Placements(ctx, slot.ID)
	if err != nil {
		return false, err
	}
	for _, stored := range repositories {
		repository, ok := byID[stored.RepositoryID]
		if !ok || stored.State != "READY" {
			return false, nil
		}
		// 更新互換fingerprintを持たないrepositoryは更新に使えないため、保存済み状態では維持しない。
		if stored.CompatibilityFingerprint == "" {
			return false, nil
		}
		compatibility, err := workspace.UpdateCompatibilityFingerprintWithGit(ctx, m.git, slot.Generation, repository, m.Config())
		if err != nil || compatibility != stored.CompatibilityFingerprint {
			return false, err
		}
		if err := preparer.ValidateReady(ctx, repository, stored.WorktreePath, stored.BaseOID); err != nil {
			return false, err
		}
		if _, err := preparer.MaterializedPlacements(stored.WorktreePath, placementsFor(placements, stored.RepositoryID)); err != nil {
			return false, err
		}
	}
	if w.Kind == "multi_repository" {
		destination, err := domain.OpenRootAt(preparer.OwnedRoot, slot.RelPath)
		if err != nil {
			return false, err
		}
		_, validateErr := workspace.ExistingPlacements(destination, placementsFor(placements, ""), false)
		_ = destination.Close()
		if validateErr != nil {
			return false, validateErr
		}
	}
	return true, nil
}

// standbyReadyUsable はREADY slotを維持できるかを返す。reconcileと`wx doctor`で判定がずれないよう共有する。
// 配置履歴を持たないslotは更新に使えないため、現在のmainと完全一致するときだけ維持する。
func (m *Manager) standbyReadyUsable(ctx context.Context, slot state.Slot, w discovery.Workspace, resolved []pool.Resolved, reuse bool) (bool, error) {
	valid, err := m.readyMatches(ctx, slot, resolved)
	if err == nil && valid {
		return true, nil
	}
	if !reuse || !slot.PlacementHistoryComplete {
		return valid, err
	}
	return m.standbyStoredStateValid(ctx, slot, w)
}

func (m *Manager) runStandbyUpdate(ctx context.Context, job state.Job) error {
	slot, err := m.store.Slot(ctx, job.SlotID)
	if err != nil {
		return err
	}
	// 完了済みの更新を job の再配送で二度走らせない。貸出付きは LEASED、idle 更新は READY へ戻っている。
	// Early Ready 後に失敗して貸出を続けた更新も、update_completed_at を記録して LEASED へ進めている。
	if slot.UpdateCompletedAt != "" && (slot.State == "LEASED" || (job.SessionID == "" && slot.State == "READY")) {
		return nil
	}
	if slot.State != "PREPARING" {
		return fmt.Errorf("slot %s cannot update from %s", slot.ID, slot.State)
	}
	if slot.UpdateStartedAt != "" {
		_ = m.store.SetSlotState(ctx, slot.ID, []string{"PREPARING"}, "QUARANTINED", "UPDATE_AMBIGUOUS")
		return fmt.Errorf("%w: standby update was interrupted; automatic replay is disabled", state.ErrOwnership)
	}
	releaseRoot, err := m.holdRootForPath(slot.Path)
	if err != nil {
		return err
	}
	defer releaseRoot()
	w, continueLease, err := m.updateStandbySlot(ctx, job, slot)
	if err == nil || !continueLease {
		return err
	}
	m.log.Error("standby update failed after early readiness", "job_id", job.ID, "session_id", job.SessionID, "slot_id", slot.ID, "continue_lease", continueLease, "error", err)
	return m.leaseAfterStandbyUpdateFailure(ctx, w, slot.ID)
}

// updateStandbySlot は全repositoryの前半（checkoutと配置）を終えてから、貸出付きの更新に限りEarly Readyを出し、後半を実行する。
// continueLease は Early Ready の後の失敗を隔離せずに記録したことを示し、呼び出し側は貸出を続けて LEASED へ進める。
// エージェントが起動した後に隔離すると、返却が DRAINING を通らず作業が snapshot へ届かないためである。
// commentlint:allow-long -- Early Ready を出す境界と、その後の失敗を隔離しない理由を doc comment にまとめる
func (m *Manager) updateStandbySlot(ctx context.Context, job state.Job, slot state.Slot) (w discovery.Workspace, continueLease bool, updateErr error) {
	updateConfig := m.Config()
	if slot.UpdateCopyMode != "" {
		updateConfig.RepositoryDefaults.Storage.CopyMode = slot.UpdateCopyMode
	}
	preparer := m.newPreparer(updateConfig, slot)
	ctx, releaseSlot, err := preparer.LockSlot(ctx)
	if err != nil {
		return w, false, err
	}
	defer releaseSlot()
	// 更新も cold start と同じ器で測る。`wx bench` から更新の所要時間が見えるようにし、
	// 待機中の client が読む実行中区間の表へもこの経路を載せるためである。
	timer := m.newPrepareTimer(slot, preparer)
	defer func() { timer.finish(updateErr) }()
	if err := m.store.BeginStandbyUpdate(ctx, slot.ID); err != nil {
		return w, false, err
	}
	early := false
	defer func() {
		if updateErr == nil {
			return
		}
		code := "UPDATE_FAILED"
		if errors.Is(updateErr, state.ErrOwnership) {
			code = "WORKTREE_OWNERSHIP_UNCERTAIN"
		} else if early {
			// owner session が既に終わっていれば記録は拒否され、cold start の Early 後の失敗と同じく隔離へ倒れる。
			failureCode, detail := m.preparationFailure(code, updateErr)
			if recordErr := m.store.RecordEarlyReadyPrepareFailure(context.Background(), slot.ID, failureCode, detail, timer.failedPhase()); recordErr == nil {
				continueLease = true
				return
			}
		}
		_ = m.store.SetSlotState(context.Background(), slot.ID, []string{"PREPARING"}, "QUARANTINED", code)
	}()
	w, err = m.store.Workspace(ctx, job.WorkspaceID)
	if err != nil {
		return w, false, err
	}
	repositories, err := m.store.SlotRepositories(ctx, slot.ID)
	if err != nil {
		return w, false, err
	}
	previous, err := m.store.Placements(ctx, slot.ID)
	if err != nil {
		return w, false, err
	}
	desired, err := m.store.UpdatePlacements(ctx, slot.ID)
	if err != nil {
		return w, false, err
	}
	byID := make(map[string]discovery.Repository, len(w.Repositories))
	actualDesired := append([]state.Placement(nil), placementsFor(desired, "")...)
	for _, repository := range w.Repositories {
		byID[string(repository.ID)] = repository
	}
	targets := make([]discovery.Repository, len(repositories))
	stages := make([]workspace.StandbyUpdateStage, len(repositories))
	for index, stored := range repositories {
		repository, ok := byID[stored.RepositoryID]
		if !ok || stored.UpdateBaseOID == "" {
			return w, false, errors.New("standby update metadata no longer matches the workspace")
		}
		// 更新の区間名も repository ごとに繰り返すため、実行中の表示が何件目かを読めるようにする。
		// 前半と後半の2周で同じ repository に同じ番号を付ける。
		preparer.Phases.Scope(workspace.RepositoryScope(repository, index+1, len(repositories)))
		if err := m.store.MarkRepositoryUpdateRunning(ctx, slot.ID, stored.RepositoryID); err != nil {
			return w, false, err
		}
		stage, err := preparer.UpdateCheckoutLocked(ctx, repository, stored.WorktreePath, stored.BaseOID, stored.UpdateBaseOID, slot.ID, placementsFor(previous, stored.RepositoryID), placementsFor(desired, stored.RepositoryID))
		if err != nil {
			return w, false, err
		}
		targets[index], stages[index] = repository, stage
		actualDesired = append(actualDesired, stage.Placements()...)
	}
	if w.Kind == "multi_repository" {
		destination, err := domain.OpenRootAt(preparer.OwnedRoot, slot.RelPath)
		if err != nil {
			return w, false, err
		}
		syncErr := workspace.ValidateAndSyncRootPlacements(destination, placementsFor(previous, ""), placementsFor(desired, ""))
		_ = destination.Close()
		if syncErr != nil {
			return w, false, syncErr
		}
	}
	// 配置は前半で全て置き終えているため、Early Ready の前に実際の配置を staging へ確定させる。
	// Early Ready の後に失敗して貸出を続ける場合も、この staging を配置履歴として公開する。
	if err := m.store.ReplaceUpdatePlacements(ctx, slot.ID, actualDesired); err != nil {
		return w, false, err
	}
	if job.SessionID != "" {
		for index, stored := range repositories {
			identity, err := preparer.WorktreeIdentity(stored.WorktreePath)
			if err != nil {
				return w, false, err
			}
			if err := m.store.RecordSlotRepositoryIdentity(ctx, slot.ID, string(targets[index].ID), identity); err != nil {
				return w, false, err
			}
		}
		if err := m.store.MarkStandbyUpdateEarlyReady(ctx, slot.ID); err != nil {
			return w, false, err
		}
		early = true
		timer.markEarly()
	}
	for index, stored := range repositories {
		preparer.Phases.Scope(workspace.RepositoryScope(targets[index], index+1, len(repositories)))
		if _, err := preparer.UpdateFinishLocked(ctx, targets[index], stored.WorktreePath, stored.BaseOID, stored.UpdateBaseOID, slot.ID, placementsFor(previous, stored.RepositoryID), stages[index]); err != nil {
			return w, false, err
		}
	}
	if job.SessionID == "" {
		if err := m.store.FinishIdleStandbyUpdate(ctx, slot.ID); err != nil {
			return w, false, err
		}
		m.log.Info("standby idle update completed", "workspace_id", w.ID, "slot_id", slot.ID)
		m.scheduleSlotUsageMeasurement(slot.ID)
		return w, false, nil
	}
	releaseJob, released, replenishJob, replenished, err := m.store.FinishStandbyUpdate(ctx, slot.ID)
	if err != nil {
		return w, false, err
	}
	m.log.Info("standby update completed", "workspace_id", w.ID, "slot_id", slot.ID)
	m.finishStandbyUpdateLease(ctx, w, slot.ID, releaseJob, released, replenishJob, replenished)
	return w, false, nil
}

// leaseAfterStandbyUpdateFailure は Early Ready の後に後半が失敗した更新を LEASED まで進める。
// cold start の leaseAfterPrepareFailure に相当し、失敗自体は failure_code に残って最初のプロンプトで伝わる。
// 返却が先着して owner session が RELEASING の場合は、同じ遷移が DRAINING と SNAPSHOT を選ぶ。
func (m *Manager) leaseAfterStandbyUpdateFailure(ctx context.Context, w discovery.Workspace, slotID string) error {
	releaseJob, released, replenishJob, replenished, err := m.store.FinishStandbyUpdateAfterFailure(ctx, slotID)
	if err != nil {
		m.log.Error("finish standby update after failure failed", "slot_id", slotID, "error", err)
		_ = m.store.SetSlotState(context.Background(), slotID, []string{"PREPARING"}, "QUARANTINED", "UPDATE_AMBIGUOUS")
		return err
	}
	m.finishStandbyUpdateLease(ctx, w, slotID, releaseJob, released, replenishJob, replenished)
	return nil
}

func (m *Manager) finishStandbyUpdateLease(ctx context.Context, w discovery.Workspace, slotID string, releaseJob state.Job, released bool, replenishJob state.Job, replenished bool) {
	m.scheduleSlotUsageMeasurement(slotID)
	if released {
		m.schedule(releaseJob)
		return
	}
	m.handleNormalSessionSuccess(ctx, w, replenishJob, replenished)
}
