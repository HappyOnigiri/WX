package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/HappyOnigiri/WX/internal/config"
	"github.com/HappyOnigiri/WX/internal/discovery"
	"github.com/HappyOnigiri/WX/internal/domain"
	"github.com/HappyOnigiri/WX/internal/pool"
	"github.com/HappyOnigiri/WX/internal/state"
	"github.com/HappyOnigiri/WX/internal/workspace"
)

func (m *Manager) newPreparer(cfg config.Config, slot state.Slot) *workspace.Preparer {
	// worktreeを再利用・削除する操作には必ずStoreによる所有権証明を渡す。
	slotPath := slot.Path
	if root, ok := m.rootForPath(slotPath); ok {
		cfg.Storage.WorktreeRoot = root
	}
	root, err := config.ExpandHome(cfg.Storage.WorktreeRoot)
	var ownedRoot *os.Root
	if err == nil {
		ownedRoot = m.rootHandleForPath(slotPath)
		if ownedRoot == nil && slotPath == "" {
			ownedRoot = m.rootHandleForPath(root)
		}
	}
	return &workspace.Preparer{
		Git: m.git, Config: cfg, Ownership: m.store, SlotPath: slotPath, Log: m.log,
		DetailDir: m.prepareDetailDir,
		OwnedRoot: ownedRoot, RootPath: filepath.Clean(root),
		RootID: slot.RootID, SlotRelPath: slot.RelPath,
	}
}

func (m *Manager) resolvedFromStored(ctx context.Context, w discovery.Workspace, repos []state.SlotRepository) ([]pool.Resolved, error) {
	by := map[string]discovery.Repository{}
	for _, r := range w.Repositories {
		by[string(r.ID)] = r
	}
	out := make([]pool.Resolved, 0, len(repos))
	for _, sr := range repos {
		repo, ok := by[sr.RepositoryID]
		if !ok {
			return nil, fmt.Errorf("repository %s left workspace", sr.RepositoryID)
		}
		out = append(out, pool.Resolved{Repository: repo, RequestedRef: sr.RequestedRef, OID: sr.BaseOID})
	}
	return out, nil
}

func (m *Manager) prepareSlot(ctx context.Context, id string, w discovery.Workspace, resolved []pool.Resolved, repos []state.SlotRepository) error {
	return m.prepareSlotWithJob(ctx, id, w, resolved, repos, state.Job{})
}

func (m *Manager) prepareSlotWithJob(ctx context.Context, id string, w discovery.Workspace, resolved []pool.Resolved, repos []state.SlotRepository, job state.Job) error {
	slot, err := m.store.Slot(ctx, id)
	if err != nil {
		return err
	}
	if slot.State == "READY" || slot.State == "LEASED" {
		return nil
	}
	if slot.State != "PREPARING" && slot.State != "RESTORING" {
		return fmt.Errorf("slot %s cannot be prepared from %s", id, slot.State)
	}
	releaseRoot, err := m.holdRootForPath(slot.Path)
	if err != nil {
		m.quarantineOwnershipFailure(id, []string{"PREPARING", "RESTORING"}, err)
		return err
	}
	defer releaseRoot()
	preparer := m.newPreparer(m.Config(), slot)
	if len(repos) != len(resolved) {
		return errors.New("slot repository metadata does not match resolved workspace")
	}
	for _, r := range resolved {
		stored, err := m.store.SlotRepository(ctx, id, string(r.Repository.ID))
		if err != nil {
			return err
		}
		if stored.State == "COLD" {
			continue
		}
		if stored.State == "READY" {
			if err := preparer.ValidateSlotWorktreeOwnership(ctx, r.Repository, stored.WorktreePath, r.OID, id); err != nil {
				m.quarantineOwnershipFailure(id, []string{"PREPARING", "RESTORING"}, err)
				return err
			}
			continue
		}
		if stored.State == "PREPARE_RUNNING" {
			if override := m.Config().Repositories[string(r.Repository.MainPath)]; len(override.Prepare.Command) > 0 {
				err := errors.New("prepare command completion is ambiguous after interruption")
				_ = m.store.SetSlotState(ctx, id, []string{"PREPARING", "RESTORING"}, "QUARANTINED", "PREPARE_AMBIGUOUS")
				return err
			}
		} else if err := m.store.SetSlotRepositoryState(ctx, id, string(r.Repository.ID), []string{"PREPARING", "RESTORING"}, "PREPARE_RUNNING"); err != nil {
			return err
		}
		if err := preparer.Prepare(ctx, r.Repository, stored.WorktreePath, r.OID, id); err != nil {
			m.log.Error("slot preparation failed", "job_id", job.ID, "session_id", job.SessionID, "slot_id", id, "repository_id", r.Repository.ID, "error", err)
			if errors.Is(err, state.ErrOwnership) {
				_ = m.store.SetSlotState(context.Background(), id, []string{"PREPARING", "RESTORING"}, "QUARANTINED", "WORKTREE_OWNERSHIP_UNCERTAIN")
			} else {
				failureCode, detailPath := "PREPARE_FAILED", ""
				var prepareErr *workspace.PrepareCommandError
				if errors.As(err, &prepareErr) {
					detailPath = prepareErr.DetailPath
					if prepareErr.FailureID != "" {
						failureCode += ":" + prepareErr.FailureID
					}
				}
				_ = m.store.SetSlotStateWithDetail(ctx, id, []string{"PREPARING", "RESTORING"}, "FAILED", failureCode, detailPath)
			}
			return err
		}
		identity, identityErr := preparer.WorktreeIdentity(stored.WorktreePath)
		if identityErr != nil {
			m.quarantineOwnershipFailure(id, []string{"PREPARING", "RESTORING"}, fmt.Errorf("%w: capture prepared worktree identity: %w", state.ErrOwnership, identityErr))
			return identityErr
		}
		if err := m.store.RecordSlotRepositoryIdentity(ctx, id, string(r.Repository.ID), identity); err != nil {
			return err
		}
		if err := m.store.SetSlotRepositoryState(ctx, id, string(r.Repository.ID), []string{"PREPARE_RUNNING"}, "READY"); err != nil {
			return err
		}
	}
	if w.Kind == "multi_repository" {
		slot, err := m.store.Slot(ctx, id)
		if err != nil {
			return err
		}
		dirIdentity, identityErr := m.ownedDirectoryIdentity(slot.Path)
		if identityErr != nil {
			m.quarantineOwnershipFailure(id, []string{"PREPARING", "RESTORING"}, fmt.Errorf("%w: read slot directory identity: %w", state.ErrOwnership, identityErr))
			return identityErr
		}
		if err := m.store.ValidateSlotOwnership(context.Background(), state.SlotOwnershipRequest{SlotID: id, WorkspaceID: slot.WorkspaceID, RootID: slot.RootID, RelPath: slot.RelPath, DirIdentity: dirIdentity, AllowedSlotStates: []string{"PREPARING", "RESTORING"}}); err != nil {
			if errors.Is(err, state.ErrOwnership) {
				_ = m.store.SetSlotState(context.Background(), id, []string{"PREPARING", "RESTORING"}, "QUARANTINED", "WORKTREE_OWNERSHIP_UNCERTAIN")
			}
			return err
		}
		if err := m.materializeWorkspaceRoot(string(w.Root), slot.Path, m.Config().Workspaces[string(w.Root)]); err != nil {
			m.log.Error("workspace root materialization failed", "slot_id", id, "error", err)
			if errors.Is(err, state.ErrOwnership) {
				_ = m.store.SetSlotState(context.Background(), id, []string{"PREPARING", "RESTORING"}, "QUARANTINED", "WORKTREE_OWNERSHIP_UNCERTAIN")
			} else {
				_ = m.store.SetSlotState(ctx, id, []string{"PREPARING", "RESTORING"}, "FAILED", "ROOT_MATERIALIZATION_FAILED")
			}
			return err
		}
	}
	normalPreparation := false
	if slot.OwnerSessionID != "" {
		if owner, ownerErr := m.store.SessionByID(ctx, slot.OwnerSessionID); ownerErr == nil {
			normalPreparation = owner.State != "RESTORING"
		}
	}
	releaseJob, released, replenishJob, replenished, err := m.store.FinishPreparationWithReplenishment(ctx, id)
	if err != nil {
		m.log.Error("finish preparation failed", "slot_id", id, "error", err)
		return err
	}
	m.scheduleSlotUsageMeasurement(id)
	if released {
		m.schedule(releaseJob)
		return nil
	}
	if normalPreparation && m.standbyReplenishmentEnabled(w) {
		m.handleNormalSessionSuccess(ctx, w, replenishJob, replenished)
	}
	return nil
}

func (m *Manager) materializeWorkspaceRoot(source, slotPath string, rules config.Workspace) error {
	// SQLiteの所有権確認後もroot置換の窓を作らないよう、pathベースでmaterializeしない。
	root, ok := m.rootForPath(slotPath)
	if !ok {
		return fmt.Errorf("%w: slot path is outside known wx roots", state.ErrOwnership)
	}
	owner, closeOwner, err := m.existingRootDescriptor(root)
	if err != nil {
		return fmt.Errorf("%w: open slot root namespace: %w", state.ErrOwnership, err)
	}
	defer closeOwner()
	if err := verifyRootDescriptorPath(root, owner); err != nil {
		return err
	}
	relative, ok := relativeWithinRoot(root, slotPath)
	if !ok {
		return fmt.Errorf("%w: slot path is outside wx root", state.ErrOwnership)
	}
	destination, err := domain.OpenRootAt(owner, relative)
	if err != nil {
		return fmt.Errorf("%w: open slot root namespace: %w", state.ErrOwnership, err)
	}
	defer func() { _ = destination.Close() }()
	if err := workspace.MaterializeRootAt(m.log, source, destination, rules); err != nil {
		return err
	}
	if err := verifyRootDescriptorPath(root, owner); err != nil {
		return err
	}
	return nil
}
