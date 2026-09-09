package daemon

import (
	"context"
	"errors"
	"fmt"

	"github.com/HappyOnigiri/WX/internal/discovery"
	"github.com/HappyOnigiri/WX/internal/domain"
	"github.com/HappyOnigiri/WX/internal/pool"
	"github.com/HappyOnigiri/WX/internal/state"
	"github.com/HappyOnigiri/WX/internal/workspace"
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
		return Lease{SessionID: session.ID, Token: token, Path: leasePathValue, RootIdentity: rootIdentity, SourceWorkspace: string(w.Root), Ready: false}, true, nil
	}
	job, replenished, err := m.store.LeaseReadyWithReplenishment(ctx, ready.ID, session)
	if err != nil {
		m.releaseLease(session.ID)
		return Lease{}, false, err
	}
	m.handleNormalSessionSuccess(ctx, w, job, replenished)
	return Lease{SessionID: session.ID, Token: token, Path: leasePathValue, RootIdentity: rootIdentity, SourceWorkspace: string(w.Root), Ready: true}, true, nil
}

func (m *Manager) leaseUpdatingStandby(ctx context.Context, w discovery.Workspace, slot state.Slot, resolved []pool.Resolved, agent string, pid int, attrs leaseAttrs) (Lease, bool, error) {
	if !slot.PlacementHistoryComplete || slot.OwnerSessionID != "" || slot.Generation == 0 {
		return Lease{}, false, errors.New("standby has no complete placement history")
	}
	releaseRoot, err := m.holdRootForPath(slot.Path)
	if err != nil {
		return Lease{}, false, err
	}
	defer releaseRoot()
	repositories, err := m.store.SlotRepositories(ctx, slot.ID)
	if err != nil {
		return Lease{}, false, err
	}
	if len(repositories) != len(resolved) {
		return Lease{}, false, errors.New("workspace repository set changed")
	}
	storedByID := make(map[string]state.SlotRepository, len(repositories))
	for _, repository := range repositories {
		if repository.State != "READY" || repository.CompatibilityFingerprint == "" {
			return Lease{}, false, errors.New("standby contains an unmaterialized or legacy repository")
		}
		storedByID[repository.RepositoryID] = repository
	}
	previous, err := m.store.Placements(ctx, slot.ID)
	if err != nil {
		return Lease{}, false, err
	}
	preparer := m.newPreparer(m.Config(), slot)
	var desired []state.Placement
	var targets []state.SlotRepository
	for _, requested := range resolved {
		stored, ok := storedByID[string(requested.Repository.ID)]
		if !ok {
			return Lease{}, false, errors.New("workspace repository set changed")
		}
		compatibility, err := workspace.UpdateCompatibilityFingerprint(slot.Generation, requested.Repository, m.Config())
		if err != nil {
			return Lease{}, false, err
		}
		if compatibility != stored.CompatibilityFingerprint {
			return Lease{}, false, errors.New("standby preparation conditions changed")
		}
		fingerprint, err := workspace.Fingerprint(slot.Generation, requested.OID, requested.Repository, m.Config())
		if err != nil {
			return Lease{}, false, err
		}
		planned, err := preparer.RepositoryPlacements(ctx, requested.Repository, requested.OID)
		if err != nil {
			return Lease{}, false, err
		}
		oldRepositoryPlacements := placementsFor(previous, stored.RepositoryID)
		if err := preparer.ValidateUpdateCandidate(ctx, requested.Repository, stored.WorktreePath, stored.BaseOID, requested.OID, oldRepositoryPlacements, planned); err != nil {
			return Lease{}, false, err
		}
		desired = append(desired, planned...)
		targets = append(targets, state.SlotRepository{RepositoryID: stored.RepositoryID, RequestedRef: requested.RequestedRef, BaseOID: requested.OID, Fingerprint: fingerprint, CompatibilityFingerprint: compatibility, UpdateBaseOID: stored.BaseOID, UpdateFingerprint: stored.Fingerprint})
	}
	if w.Kind == "multi_repository" {
		planned, err := workspace.RootPlacements(string(w.Root), m.Config().Workspaces[string(w.Root)])
		if err != nil {
			return Lease{}, false, err
		}
		destination, err := domain.OpenRootAt(preparer.OwnedRoot, slot.RelPath)
		if err != nil {
			return Lease{}, false, err
		}
		validateErr := workspace.ValidateRootPlacements(destination, placementsFor(previous, ""), planned)
		_ = destination.Close()
		if validateErr != nil {
			return Lease{}, false, validateErr
		}
		desired = append(desired, planned...)
	}
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
	job, err := m.store.ReserveStandbyUpdate(ctx, slot.ID, session, targets, desired, m.Config().Storage.CopyMode)
	if err != nil {
		m.releaseLease(session.ID)
		if stringsContainStateRace(err) {
			return Lease{}, false, nil
		}
		return Lease{}, false, err
	}
	m.log.Info("standby update reserved", "workspace_id", w.ID, "slot_id", slot.ID)
	m.schedule(job)
	return Lease{SessionID: session.ID, Token: token, Path: leasePathValue, RootIdentity: rootIdentity, SourceWorkspace: string(w.Root), Ready: false}, true, nil
}

func stringsContainStateRace(err error) bool {
	return err != nil && (errors.Is(err, state.ErrSlotStateIneligible) || err.Error() == "slot is no longer an updateable READY standby")
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
		if stored.CompatibilityFingerprint == "" {
			return m.readyMatches(ctx, slot, mustResolveStored(w, repositories))
		}
		compatibility, err := workspace.UpdateCompatibilityFingerprint(slot.Generation, repository, m.Config())
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

func mustResolveStored(w discovery.Workspace, repositories []state.SlotRepository) []pool.Resolved {
	byID := make(map[string]discovery.Repository, len(w.Repositories))
	for _, repository := range w.Repositories {
		byID[string(repository.ID)] = repository
	}
	resolved := make([]pool.Resolved, 0, len(repositories))
	for _, stored := range repositories {
		resolved = append(resolved, pool.Resolved{Repository: byID[stored.RepositoryID], RequestedRef: stored.RequestedRef, OID: stored.BaseOID})
	}
	return resolved
}

func (m *Manager) runStandbyUpdate(ctx context.Context, job state.Job) (updateErr error) {
	slot, err := m.store.Slot(ctx, job.SlotID)
	if err != nil {
		return err
	}
	if slot.State == "LEASED" && slot.UpdateCompletedAt != "" {
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
	updateConfig := m.Config()
	if slot.UpdateCopyMode != "" {
		updateConfig.Storage.CopyMode = slot.UpdateCopyMode
	}
	preparer := m.newPreparer(updateConfig, slot)
	ctx, releaseSlot, err := preparer.LockSlot(ctx)
	if err != nil {
		return err
	}
	defer releaseSlot()
	if err := m.store.BeginStandbyUpdate(ctx, slot.ID); err != nil {
		return err
	}
	defer func() {
		if updateErr == nil {
			return
		}
		code := "UPDATE_FAILED"
		if errors.Is(updateErr, state.ErrOwnership) {
			code = "WORKTREE_OWNERSHIP_UNCERTAIN"
		}
		_ = m.store.SetSlotState(context.Background(), slot.ID, []string{"PREPARING"}, "QUARANTINED", code)
	}()
	w, err := m.store.Workspace(ctx, job.WorkspaceID)
	if err != nil {
		return err
	}
	repositories, err := m.store.SlotRepositories(ctx, slot.ID)
	if err != nil {
		return err
	}
	previous, err := m.store.Placements(ctx, slot.ID)
	if err != nil {
		return err
	}
	desired, err := m.store.UpdatePlacements(ctx, slot.ID)
	if err != nil {
		return err
	}
	byID := make(map[string]discovery.Repository, len(w.Repositories))
	actualDesired := append([]state.Placement(nil), placementsFor(desired, "")...)
	for _, repository := range w.Repositories {
		byID[string(repository.ID)] = repository
	}
	for _, stored := range repositories {
		repository, ok := byID[stored.RepositoryID]
		if !ok || stored.UpdateBaseOID == "" {
			return errors.New("standby update metadata no longer matches the workspace")
		}
		if err := m.store.MarkRepositoryUpdateRunning(ctx, slot.ID, stored.RepositoryID); err != nil {
			return err
		}
		materialized, err := preparer.UpdateLocked(ctx, repository, stored.WorktreePath, stored.BaseOID, stored.UpdateBaseOID, slot.ID, placementsFor(previous, stored.RepositoryID), placementsFor(desired, stored.RepositoryID))
		if err != nil {
			return err
		}
		actualDesired = append(actualDesired, materialized...)
	}
	if w.Kind == "multi_repository" {
		destination, err := domain.OpenRootAt(preparer.OwnedRoot, slot.RelPath)
		if err != nil {
			return err
		}
		syncErr := workspace.ValidateAndSyncRootPlacements(destination, placementsFor(previous, ""), placementsFor(desired, ""))
		_ = destination.Close()
		if syncErr != nil {
			return syncErr
		}
	}
	if err := m.store.ReplaceUpdatePlacements(ctx, slot.ID, actualDesired); err != nil {
		return err
	}
	releaseJob, released, replenishJob, replenished, err := m.store.FinishStandbyUpdate(ctx, slot.ID)
	if err != nil {
		return err
	}
	m.log.Info("standby update completed", "workspace_id", w.ID, "slot_id", slot.ID)
	m.scheduleSlotUsageMeasurement(slot.ID)
	if released {
		m.schedule(releaseJob)
		return nil
	}
	m.handleNormalSessionSuccess(ctx, w, replenishJob, replenished)
	return nil
}
