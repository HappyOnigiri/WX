package workspace

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/HappyOnigiri/WX/internal/config"
	"github.com/HappyOnigiri/WX/internal/discovery"
	"github.com/HappyOnigiri/WX/internal/domain"
	"github.com/HappyOnigiri/WX/internal/state"
)

func (p *Preparer) validatePreparedTarget(ctx context.Context, repo discovery.Repository, target, oid, slotID string, phase preparePhase, lockedRoot *os.Root, relative, expected, stage string) error {
	if identityErr := p.verifyPreparedTargetIdentity(lockedRoot, relative, expected); identityErr != nil {
		return fmt.Errorf("%s: %w", stage, identityErr)
	}
	if validationErr := p.validateExistingWorktreeOwnedForPhase(ctx, repo, target, oid, slotID, phase); validationErr != nil {
		return validationErr
	}
	if identityErr := p.verifyPreparedTargetIdentity(lockedRoot, relative, expected); identityErr != nil {
		return fmt.Errorf("%s after validation: %w", stage, identityErr)
	}
	return nil
}

func (p *Preparer) validateExistingWorktree(ctx context.Context, repo discovery.Repository, target, oid string) error {
	return p.validateExistingWorktreeOwnedForPhase(ctx, repo, target, oid, "", preparePhaseCreate)
}

func (p *Preparer) validateExistingWorktreeOwnedForPhase(ctx context.Context, repo discovery.Repository, target, oid, slotID string, phase preparePhase) error {
	slotStates, repositoryStates := preparationOwnershipStates(phase)
	err := p.validateExistingWorktreeOwnedForStates(ctx, repo, target, oid, slotID, slotStates, repositoryStates)
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, state.ErrOwnership) {
		return err
	}
	return fmt.Errorf("%w: %w", state.ErrOwnership, err)
}

// ValidateSlotWorktreeOwnership は、prepare/restore job が slot state を commit する前に repository level だけ READY になった worktree を検証する。
// daemon crash でこの中間状態が残るため、slot 昇格前に READY row だけで済ませず、正確な slot/path/registration を証明する。
func (p *Preparer) ValidateSlotWorktreeOwnership(ctx context.Context, repo discovery.Repository, target, oid, slotID string) error {
	return p.validateSlotWorktreeOwnershipForPhase(ctx, repo, target, oid, slotID, preparePhaseCreate)
}

// ValidateRestoringSlotWorktreeOwnership は restore 中に durable に READY と記録された repository の replay 検査である。
// 関係する slot は RESTORING のままにし、無関係な lifecycle state まで証明範囲を広げない。
func (p *Preparer) ValidateRestoringSlotWorktreeOwnership(ctx context.Context, repo discovery.Repository, target, oid, slotID string) error {
	return p.validateSlotWorktreeOwnershipForPhase(ctx, repo, target, oid, slotID, preparePhaseRestore)
}

func (p *Preparer) validateSlotWorktreeOwnershipForPhase(ctx context.Context, repo discovery.Repository, target, oid, slotID string, phase preparePhase) error {
	if slotID == "" {
		return fmt.Errorf("%w: slot ID is required for replay validation", state.ErrOwnership)
	}
	slotStates, repositoryStates := preparationOwnershipStates(phase)
	// この crash-replay 境界では repository row はすでに READY だが、関係する slot はまだ phase state にある。
	// その durable な中間 state だけを許可し、無関係な lifecycle state は受け入れない。
	repositoryStates = append(repositoryStates, "READY")
	if err := p.validateExistingWorktreeOwnedForStates(ctx, repo, target, oid, slotID, slotStates, repositoryStates); err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return err
		}
		return fmt.Errorf("%w: validate replayed slot worktree: %w", state.ErrOwnership, err)
	}
	return nil
}

func (p *Preparer) validateExistingWorktreeOwnedForStates(ctx context.Context, repo discovery.Repository, target, oid, slotID string, slotStates, repositoryStates []string) error {
	root, err := config.ExpandHome(p.Config.Storage.WorktreeRoot)
	if err != nil {
		return err
	}
	target = filepath.Clean(target)
	if !domain.IsWithin(root, target) {
		return errors.New("worktree target is outside wx ownership root")
	}
	owner, relativeTarget, closeOwner, err := p.openOwnedRoot(root, target)
	if err != nil {
		return fmt.Errorf("open worktree ownership root: %w", err)
	}
	defer closeOwner()
	if err := ValidateOwnershipMarkerAt(owner, root, target, p.markerIdentity(repo, slotID), string(repo.CommonDir)); err != nil {
		return err
	}
	ownedTarget, targetIdentity, err := domain.OpenDirectoryAt(owner, relativeTarget)
	if err != nil {
		return fmt.Errorf("worktree target is not physical: %w", err)
	}
	targetRoot := ownedTarget
	defer func() { _ = targetRoot.Close() }()
	gitMarker, err := owner.Lstat(filepath.Join(relativeTarget, ".git"))
	if err != nil || gitMarker.Mode()&os.ModeSymlink != 0 {
		return errors.New("missing or unsafe .git marker")
	}
	common, err := p.runGitInDirectory(ctx, targetRoot, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return err
	}
	expectedCommon, err := filepath.EvalSymlinks(string(repo.CommonDir))
	if err != nil {
		return err
	}
	actualCommon, err := filepath.EvalSymlinks(strings.TrimSpace(common.Stdout))
	if err != nil || actualCommon != expectedCommon {
		return errors.New("common Git directory does not match")
	}
	head, err := p.runGitInDirectory(ctx, targetRoot, "rev-parse", "HEAD")
	detached := false
	if err == nil {
		_, detachedErr := p.runGitInDirectory(ctx, targetRoot, "symbolic-ref", "-q", "HEAD")
		detached = detachedErr != nil
	}
	if err != nil || strings.TrimSpace(head.Stdout) != oid || !detached {
		return errors.New("HEAD is not the expected detached commit")
	}
	if err := ValidateRegisteredWorktreeAt(ctx, p.Git, string(repo.MainPath), owner, root, relativeTarget, targetIdentity, slotID, slotID != ""); err != nil {
		return err
	}
	if slotID == "" {
		return nil
	}
	proof, err := p.stateOwnershipProof(ctx, repo, target, slotID, slotStates, repositoryStates)
	if err != nil {
		return err
	}
	// 記録済み identity は実際に open した directory と一致しなければならない。空 record は完了前に中断した run を示すため retry で収束できる。
	// 異なる record は marker と Git metadata が再現されていても wx が prepare した directory ではないことを示す。
	if proof.DirIdentity != "" && proof.DirIdentity != targetIdentity {
		return fmt.Errorf("%w: worktree directory identity does not match the SQLite record", state.ErrOwnership)
	}
	return nil
}

// ValidateReady は、保存済み READY worktree を安全に lease できる physical および Git-administrative invariant を検証する。
func (p *Preparer) ValidateReady(ctx context.Context, repo discovery.Repository, target, oid string) error {
	if err := p.ValidateOwnership(ctx, repo, target, oid); err != nil {
		return err
	}
	root, err := config.ExpandHome(p.Config.Storage.WorktreeRoot)
	if err != nil {
		return err
	}
	common, err := filepath.EvalSymlinks(string(repo.CommonDir))
	if err != nil {
		return err
	}
	slotID, err := p.validateRemovalOwnership(repo, root, target, common)
	if err != nil {
		return err
	}
	if err := p.validateStateOwnership(ctx, repo, target, slotID, []string{"READY"}, []string{"READY"}); err != nil {
		return err
	}
	return p.validateTrackedClean(ctx, target)
}

// ValidateOwnership は index や working tree の clean を要求せず、physical および Git-administrative ownership invariant を検証する。
// 復元した leased session には archived user の tracked changes が意図的に含まれる。
func (p *Preparer) ValidateOwnership(ctx context.Context, repo discovery.Repository, target, oid string) error {
	if err := p.validateExistingWorktree(ctx, repo, target, oid); err != nil {
		return err
	}
	root, err := config.ExpandHome(p.Config.Storage.WorktreeRoot)
	if err != nil {
		return err
	}
	common, err := filepath.EvalSymlinks(string(repo.CommonDir))
	if err != nil {
		return err
	}
	slotID, err := p.validateRemovalOwnership(repo, root, target, common)
	if err != nil {
		return err
	}
	owner, relativeTarget, closeOwner, openErr := p.openOwnedRoot(root, target)
	if openErr != nil {
		return openErr
	}
	defer closeOwner()
	directory, targetIdentity, openErr := domain.OpenDirectoryAt(owner, relativeTarget)
	if openErr != nil {
		return openErr
	}
	if closeErr := directory.Close(); closeErr != nil {
		return closeErr
	}
	if err := ValidateRegisteredWorktreeAt(ctx, p.Git, string(repo.MainPath), owner, root, relativeTarget, targetIdentity, slotID, true); err != nil {
		return err
	}
	return p.validateStateOwnershipWithIdentity(ctx, repo, target, slotID, targetIdentity, allOwnershipSlotStates, allOwnershipRepositoryStates)
}

func (p *Preparer) validateRemovalOwnership(repo discovery.Repository, root, target, common string) (string, error) {
	owner, _, closeOwner, err := p.openOwnedRoot(root, target)
	if err != nil {
		return "", err
	}
	defer closeOwner()
	return ValidateRemovalOwnershipAt(owner, root, target, p.markerIdentity(repo, ""), common)
}

// ValidateRestoringOwnership は restore lock の保持中に使う slot-bound ownership 検査である。
// この検査で slot ID を保持し、同じ path にある別の wx lock reason を restore handoff が受け入れることを防ぐ。
func (p *Preparer) ValidateRestoringOwnership(ctx context.Context, repo discovery.Repository, target, oid, slotID string) error {
	return p.validateExistingWorktreeOwnedForPhase(ctx, repo, target, oid, slotID, preparePhaseRestore)
}

var (
	allOwnershipSlotStates       = []string{"PREPARING", "RESTORING", "READY", "LEASED", "DRAINING", "SNAPSHOTTING", "SNAPSHOTTED", "ARCHIVED", "REMOVING", "RETIRING"}
	allOwnershipRepositoryStates = []string{"PREPARING", "PREPARE_RUNNING", "UPDATE_PENDING", "UPDATE_RUNNING", "RESTORING", "RESTORE_RUNNING", "READY", "LEASED", "RETIRING"}
)

func preparationOwnershipStates(phase preparePhase) ([]string, []string) {
	if phase == preparePhaseRestore {
		return []string{"RESTORING"}, []string{"RESTORING", "RESTORE_RUNNING"}
	}
	if phase == preparePhaseUpdate {
		return []string{"PREPARING"}, []string{"UPDATE_RUNNING"}
	}
	return []string{"PREPARING"}, []string{"PREPARING", "PREPARE_RUNNING"}
}

func (p *Preparer) validateStateOwnership(ctx context.Context, repo discovery.Repository, target, slotID string, slotStates, repositoryStates []string) error {
	_, err := p.stateOwnershipProof(ctx, repo, target, slotID, slotStates, repositoryStates)
	return err
}

// stateOwnershipProof は proof を返す validateStateOwnership であり、記録済み identity を自分で比較する caller が使う。
func (p *Preparer) stateOwnershipProof(ctx context.Context, repo discovery.Repository, target, slotID string, slotStates, repositoryStates []string) (state.WorktreeOwnership, error) {
	if slotID == "" {
		return state.WorktreeOwnership{}, nil
	}
	if p.Ownership == nil {
		return state.WorktreeOwnership{}, fmt.Errorf("%w: state-backed worktree ownership validator is required", state.ErrOwnership)
	}
	dirName, err := p.worktreeDirName(target)
	if err != nil {
		return state.WorktreeOwnership{}, err
	}
	return p.Ownership.ValidateWorktreeOwnership(ctx, state.WorktreeOwnershipRequest{
		SlotID:       slotID,
		RepositoryID: string(repo.ID),
		WorkspaceID:  "",
		RootID:       p.RootID,
		SlotRelPath:  p.SlotRelPath,
		DirName:      dirName,
		// DirIdentity は意図的に空のままにする。この helper は worktree directory の作成前後で実行されるため identity を保証できない。
		// descriptor を保持する caller は validateStateOwnershipWithIdentity で渡し、record だけを比較する caller は返された proof から読む。
		CommonDir:               string(repo.CommonDir),
		AllowedSlotStates:       slotStates,
		AllowedRepositoryStates: repositoryStates,
	})
}

// validateStateOwnershipWithIdentity は caller が worktree directory を open した後に使う fail-closed 形式である。
// identity を提示することで、SQLite record の欠落を暗黙の成功ではなく失敗にする。
func (p *Preparer) validateStateOwnershipWithIdentity(ctx context.Context, repo discovery.Repository, target, slotID, dirIdentity string, slotStates, repositoryStates []string) error {
	if slotID == "" {
		return nil
	}
	if p.Ownership == nil {
		return fmt.Errorf("%w: state-backed worktree ownership validator is required", state.ErrOwnership)
	}
	if dirIdentity == "" {
		return fmt.Errorf("%w: worktree directory identity is unavailable", state.ErrOwnership)
	}
	dirName, err := p.worktreeDirName(target)
	if err != nil {
		return err
	}
	_, err = p.Ownership.ValidateWorktreeOwnership(ctx, state.WorktreeOwnershipRequest{
		SlotID:                  slotID,
		RepositoryID:            string(repo.ID),
		RootID:                  p.RootID,
		SlotRelPath:             p.SlotRelPath,
		DirName:                 dirName,
		DirIdentity:             dirIdentity,
		CommonDir:               string(repo.CommonDir),
		AllowedSlotStates:       slotStates,
		AllowedRepositoryStates: repositoryStates,
	})
	return err
}

// validateTrackedCleanOwned は tracked status の前後で worktree の所有権を確認し、stage を失敗の文脈として使う。
func (p *Preparer) validateTrackedCleanOwned(ctx context.Context, target string, lockedRoot *os.Root, relative, identity, stage string) error {
	if err := p.verifyPreparedTargetIdentity(lockedRoot, relative, identity); err != nil {
		return fmt.Errorf("wx worktree ownership changed before %s: %w", stage, err)
	}
	if err := p.validateTrackedClean(ctx, target); err != nil {
		return err
	}
	if err := p.verifyPreparedTargetIdentity(lockedRoot, relative, identity); err != nil {
		return fmt.Errorf("wx worktree ownership changed during %s: %w", stage, err)
	}
	return nil
}

func (p *Preparer) validateTrackedClean(ctx context.Context, target string) error {
	root, err := config.ExpandHome(p.Config.Storage.WorktreeRoot)
	if err != nil {
		return err
	}
	owner, relative, closeOwner, err := p.openOwnedRoot(root, target)
	if err != nil {
		return err
	}
	defer closeOwner()
	directory, _, err := domain.OpenDirectoryAt(owner, relative)
	if err != nil {
		return err
	}
	defer func() { _ = directory.Close() }()
	status, err := p.Git.RunAt(ctx, directory, nil, nil, "status", "--porcelain=v1", "--untracked-files=no")
	if err != nil {
		return err
	}
	if strings.TrimSpace(status.Stdout) != "" {
		return errors.New("prepared worktree has tracked changes")
	}
	return nil
}
