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
// continueLease は失敗しても隔離しなかったことを示し、呼び出し元が貸出を続けるかの判断に使う。
func (m *Manager) prepareStagedSlot(ctx context.Context, slot state.Slot, w discovery.Workspace, resolved []pool.Resolved, preparer *workspace.Preparer) (staged map[string][]state.Placement, continueLease bool, prepareErr error) {
	ctx, release, err := preparer.LockSlot(ctx)
	if err != nil {
		return nil, false, err
	}
	defer release()
	timer := m.newPrepareTimer(slot, preparer)
	defer func() { timer.finish(prepareErr) }()
	if err := m.store.BeginStagedPreparation(ctx, slot.ID); err != nil {
		_ = m.store.SetSlotState(context.Background(), slot.ID, []string{"PREPARING", "FAILED"}, "QUARANTINED", "PREPARE_AMBIGUOUS")
		return nil, false, fmt.Errorf("%w: interrupted staged preparation: %w", state.ErrOwnership, err)
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
		} else if recordErr := m.store.RecordEarlyReadyPrepareFailure(context.Background(), slot.ID, code, detail, timer.failedPhase()); recordErr == nil {
			// early ready を過ぎた貸出はエージェントが既にこの worktree で作業している。隔離すると返却が
			// LEASED を通らず snapshot に届かないため、失敗だけを記録して貸出を続ける。
			// owner session を持たない standby の補充・更新と、所有権を証明できない失敗はここに含めない。
			continueLease = true
			return
		}
		// 一度開始した二段階準備は部分 checkout や hook の完了を推測できないため、再実行しない。
		_ = m.store.SetSlotStateWithDetail(context.Background(), slot.ID, []string{"PREPARING", "FAILED"}, "QUARANTINED", code, detail)
	}()
	var requests []workspace.Preparation
	for _, r := range resolved {
		stored, err := m.store.SlotRepository(ctx, slot.ID, string(r.Repository.ID))
		if err != nil {
			return nil, false, err
		}
		if stored.State == "COLD" {
			continue
		}
		if stored.State == "READY" {
			if err := preparer.ValidateSlotWorktreeOwnership(ctx, r.Repository, stored.WorktreePath, r.OID, slot.ID); err != nil {
				return nil, false, err
			}
			continue
		}
		if err := m.store.SetSlotRepositoryState(ctx, slot.ID, stored.RepositoryID, []string{"PREPARING"}, "PREPARE_RUNNING"); err != nil {
			return nil, false, err
		}
		requests = append(requests, workspace.Preparation{Repository: r.Repository, Target: stored.WorktreePath, OID: r.OID})
	}
	var rootStage func(bool) error
	var rootPlan *workspace.RootStagePlan
	if w.Kind == "multi_repository" {
		rootRules, err := workspace.ResolveRootRules(string(w.Root), preparer.Config.WorkspaceFor(string(w.Root)))
		if err != nil {
			return nil, false, err
		}
		// workspace root のステージはどの repository にも属さない rule を扱うため、repository defaults の early_paths を使う。
		// repository 個別値の和集合は「早期に出さない」約束を破り、積集合は空になりやすく、合成する自然な規則がない。
		plan, err := workspace.PlanRootStages(m.log, string(w.Root), rootRules, preparer.Config.RepositoryDefaults.Readiness.EarlyPaths)
		if err != nil {
			return nil, false, err
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
		return nil, false, err
	}
	for _, request := range requests {
		if err := m.store.SetSlotRepositoryState(ctx, slot.ID, string(request.Repository.ID), []string{"PREPARE_RUNNING"}, "READY"); err != nil {
			return nil, false, err
		}
	}
	if rootPlan != nil {
		placed[""] = rootPlan.Placements()
	}
	return placed, false, nil
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
