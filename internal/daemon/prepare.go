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
	if slot.State != "PREPARING" {
		return errors.New("restore preparation must use the restore job")
	}
	staged, err := m.prepareStagedSlot(ctx, slot, w, resolved, preparer)
	if err != nil {
		m.log.Error("slot preparation failed", "job_id", job.ID, "session_id", job.SessionID, "slot_id", id, "error", err)
		return err
	}
	placements, err := m.capturePlacements(ctx, slot, w, resolved, preparer, staged)
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

// capturePlacements は staged preparation が実際に使った計画から配置履歴を作る。
// 記録のために include/link の規則を読み直さない。1 つの job の中で規則を 2 度読むと、その間の規則変更で配置済みの実体と記録が食い違い、slot ごと隔離される。
// staged に無い repository はこの job で配置していないので、記録済みの配置履歴をそのまま残す。
func (m *Manager) capturePlacements(ctx context.Context, slot state.Slot, w discovery.Workspace, resolved []pool.Resolved, preparer *workspace.Preparer, staged map[string][]state.Placement) ([]state.Placement, error) {
	var placements []state.Placement
	repositories, err := m.store.SlotRepositories(ctx, slot.ID)
	if err != nil {
		return nil, err
	}
	byID := make(map[string]state.SlotRepository, len(repositories))
	for _, repository := range repositories {
		byID[repository.RepositoryID] = repository
	}
	var recorded []state.Placement
	for _, resolvedRepository := range resolved {
		stored := byID[string(resolvedRepository.Repository.ID)]
		if stored.State != "READY" {
			continue
		}
		placed, ok := staged[stored.RepositoryID]
		if !ok {
			if recorded == nil {
				if recorded, err = m.store.Placements(ctx, slot.ID); err != nil {
					return nil, err
				}
			}
			placements = append(placements, placementsFor(recorded, stored.RepositoryID)...)
			continue
		}
		materialized, err := preparer.RecordMaterializedPlacements(stored.WorktreePath, placed)
		if err != nil {
			return nil, err
		}
		placements = append(placements, materialized...)
	}
	if w.Kind == "multi_repository" {
		destination, err := domain.OpenRootAt(preparer.OwnedRoot, slot.RelPath)
		if err != nil {
			return nil, err
		}
		materialized, materializeErr := workspace.RecordPlacements(destination, staged[""])
		_ = destination.Close()
		if materializeErr != nil {
			return nil, materializeErr
		}
		placements = append(placements, materialized...)
	}
	return placements, nil
}

// rootRules は非Git workspace rootの配置ruleを、configとroot直下のmanifestから解決する。
// 1つのjobでは解決を1回に保ち、計画・配置・記録・除外が同じ結果を見るようにする。
func (m *Manager) rootRules(w discovery.Workspace) (workspace.RootRules, error) {
	return workspace.ResolveRootRules(string(w.Root), m.Config().Workspaces[string(w.Root)])
}

func (m *Manager) materializeWorkspaceRoot(source, slotPath string, rules workspace.RootRules) error {
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
