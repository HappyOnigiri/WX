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
		SlotLocks: &m.slotLocks,
	}
}

// slotPrepareConfig は slot に記録された準備設定の上書きを実効設定へ載せて返す。
// 準備 job は貸出要求とは別のタイミングで走るため、上書きは要求からではなく slot 行から読む。
// 記録が壊れていた場合は既定設定で準備し直さず失敗させる。fingerprint は上書き前提で計算されている。
func (m *Manager) slotPrepareConfig(slot state.Slot) (config.Config, error) {
	override, err := config.DecodePrepareOverride(slot.PrepareOverride)
	if err != nil {
		return config.Config{}, fmt.Errorf("slot %s: %w", slot.ID, err)
	}
	return override.Apply(m.Config()), nil
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
	prepareConfig, err := m.slotPrepareConfig(slot)
	if err != nil {
		return err
	}
	preparer := m.newPreparer(prepareConfig, slot)
	if len(repos) != len(resolved) {
		return errors.New("slot repository metadata does not match resolved workspace")
	}
	if slot.State == "PREPARING" {
		if err := m.prepareStagedSlot(ctx, slot, w, resolved, preparer); err != nil {
			m.log.Error("slot preparation failed", "job_id", job.ID, "session_id", job.SessionID, "slot_id", id, "error", err)
			return err
		}
	} else {
		return errors.New("restore preparation must use the restore job")
	}
	placements, err := m.capturePlacements(ctx, slot, w, resolved, preparer)
	if err != nil {
		return err
	}
	if err := m.store.ReplacePlacements(ctx, id, placements); err != nil {
		return err
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
		_ = m.store.SetSlotState(context.Background(), id, []string{"PREPARING"}, "QUARANTINED", "PREPARE_AMBIGUOUS")
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

func (m *Manager) capturePlacements(ctx context.Context, slot state.Slot, w discovery.Workspace, resolved []pool.Resolved, preparer *workspace.Preparer) ([]state.Placement, error) {
	var placements []state.Placement
	repositories, err := m.store.SlotRepositories(ctx, slot.ID)
	if err != nil {
		return nil, err
	}
	byID := make(map[string]state.SlotRepository, len(repositories))
	for _, repository := range repositories {
		byID[repository.RepositoryID] = repository
	}
	for _, resolvedRepository := range resolved {
		stored := byID[string(resolvedRepository.Repository.ID)]
		if stored.State != "READY" {
			continue
		}
		planned, err := preparer.RepositoryPlacements(ctx, resolvedRepository.Repository, resolvedRepository.OID)
		if err != nil {
			return nil, err
		}
		materialized, err := preparer.MaterializedPlacements(stored.WorktreePath, planned)
		if err != nil {
			return nil, err
		}
		placements = append(placements, materialized...)
	}
	if w.Kind == "multi_repository" {
		rootPlacements, err := workspace.RootPlacements(string(w.Root), preparer.Config.Workspaces[string(w.Root)])
		if err != nil {
			return nil, err
		}
		destination, err := domain.OpenRootAt(preparer.OwnedRoot, slot.RelPath)
		if err != nil {
			return nil, err
		}
		materialized, materializeErr := workspace.ExistingPlacements(destination, rootPlacements, false)
		_ = destination.Close()
		if materializeErr != nil {
			return nil, materializeErr
		}
		placements = append(placements, materialized...)
	}
	return placements, nil
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
