package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/HappyOnigiri/WX/internal/discovery"
	"github.com/HappyOnigiri/WX/internal/domain"
	"github.com/HappyOnigiri/WX/internal/gitx"
	"github.com/HappyOnigiri/WX/internal/pool"
	"github.com/HappyOnigiri/WX/internal/state"
	"github.com/HappyOnigiri/WX/internal/workspace"
)

// prepareStagedSlot は staged preparation を実行し、この呼び出しで実際に配置した include/link を
// repository ID ごとに返す。workspace root の分は空 key に入れる。
func (m *Manager) prepareStagedSlot(ctx context.Context, slot state.Slot, w discovery.Workspace, resolved []pool.Resolved, preparer *workspace.Preparer) (staged map[string][]state.Placement, prepareErr error) {
	ctx, release, err := preparer.LockSlot(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	timer := m.newPrepareTimer(slot, preparer)
	defer func() { timer.finish(prepareErr) }()
	if err := m.store.BeginStagedPreparation(ctx, slot.ID); err != nil {
		_ = m.store.SetSlotState(context.Background(), slot.ID, []string{"PREPARING", "FAILED"}, "QUARANTINED", "PREPARE_AMBIGUOUS")
		return nil, fmt.Errorf("%w: interrupted staged preparation: %w", state.ErrOwnership, err)
	}
	defer func() {
		if prepareErr == nil {
			return
		}
		code, detail := "PREPARE_FAILED", ""
		var commandErr *workspace.PrepareCommandError
		var gitErr *gitx.Error
		switch {
		case errors.As(prepareErr, &commandErr):
			detail = commandErr.DetailPath
			if commandErr.FailureID != "" {
				code += ":" + commandErr.FailureID
			}
		case errors.As(prepareErr, &gitErr):
			// Git は失敗の stderr を failure ID のログへ既に書いている。
			// path を残さないと `git hook failed with exit N` だけが伝わり、hook が何を言って落ちたかへ辿れない。
			detail = gitx.DetailPath(m.prepareDetailDir, gitErr.FailureID)
		}
		if errors.Is(prepareErr, state.ErrOwnership) {
			code = "WORKTREE_OWNERSHIP_UNCERTAIN"
		}
		// 一度開始した二段階準備は部分 checkout や hook の完了を推測できないため、再実行しない。
		_ = m.store.SetSlotStateWithDetail(context.Background(), slot.ID, []string{"PREPARING", "FAILED"}, "QUARANTINED", code, detail)
	}()
	var requests []workspace.Preparation
	for _, r := range resolved {
		stored, err := m.store.SlotRepository(ctx, slot.ID, string(r.Repository.ID))
		if err != nil {
			return nil, err
		}
		if stored.State == "COLD" {
			continue
		}
		if stored.State == "READY" {
			if err := preparer.ValidateSlotWorktreeOwnership(ctx, r.Repository, stored.WorktreePath, r.OID, slot.ID); err != nil {
				return nil, err
			}
			continue
		}
		if err := m.store.SetSlotRepositoryState(ctx, slot.ID, stored.RepositoryID, []string{"PREPARING"}, "PREPARE_RUNNING"); err != nil {
			return nil, err
		}
		requests = append(requests, workspace.Preparation{Repository: r.Repository, Target: stored.WorktreePath, OID: r.OID})
	}
	var rootStage func(bool) error
	var rootPlan *workspace.RootStagePlan
	if w.Kind == "multi_repository" {
		plan, err := workspace.PlanRootStages(m.log, string(w.Root), preparer.Config.Workspaces[string(w.Root)], preparer.Config.Readiness.EarlyPaths)
		if err != nil {
			return nil, err
		}
		rootPlan = plan
		rootStage = func(early bool) error { return m.materializeStagedRoot(ctx, slot, plan.Materialize, early) }
	}
	markEarly := func() error {
		for _, request := range requests {
			identity, err := preparer.WorktreeIdentity(request.Target)
			if err != nil {
				return err
			}
			if err := m.store.RecordSlotRepositoryIdentity(ctx, slot.ID, string(request.Repository.ID), identity); err != nil {
				return err
			}
		}
		if err := m.store.MarkEarlyReady(ctx, slot.ID); err != nil {
			return err
		}
		timer.markEarly()
		return nil
	}
	placed, err := preparer.PrepareStaged(ctx, slot.ID, requests, rootStage, markEarly)
	if err != nil {
		return nil, err
	}
	for _, request := range requests {
		if err := m.store.SetSlotRepositoryState(ctx, slot.ID, string(request.Repository.ID), []string{"PREPARE_RUNNING"}, "READY"); err != nil {
			return nil, err
		}
	}
	if rootPlan != nil {
		placed[""] = rootPlan.Placements()
	}
	return placed, nil
}

func (m *Manager) materializeStagedRoot(ctx context.Context, slot state.Slot, materialize func(*os.Root, bool) error, early bool) error {
	identity, err := m.ownedDirectoryIdentity(slot.Path)
	if err != nil {
		return err
	}
	if err := m.store.ValidateSlotOwnership(ctx, state.SlotOwnershipRequest{SlotID: slot.ID, WorkspaceID: slot.WorkspaceID, RootID: slot.RootID, RelPath: slot.RelPath, DirIdentity: identity, AllowedSlotStates: []string{"PREPARING"}}); err != nil {
		return err
	}
	preparer := m.newPreparer(m.Config(), slot)
	if preparer.OwnedRoot == nil {
		return fmt.Errorf("%w: workspace root descriptor unavailable", state.ErrOwnership)
	}
	destination, err := domain.OpenRootAt(preparer.OwnedRoot, slot.RelPath)
	if err != nil {
		return err
	}
	defer func() { _ = destination.Close() }()
	if err := materialize(destination, early); err != nil {
		return err
	}
	current, err := m.ownedDirectoryIdentity(slot.Path)
	if err != nil {
		return err
	}
	if current != identity {
		return fmt.Errorf("%w: workspace root identity changed", state.ErrOwnership)
	}
	return nil
}
