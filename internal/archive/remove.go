package archive

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/HappyOnigiri/WX/internal/discovery"
	"github.com/HappyOnigiri/WX/internal/domain"
	"github.com/HappyOnigiri/WX/internal/state"
	"github.com/HappyOnigiri/WX/internal/workspace"
)

func (m *Manager) RemoveWorktree(ctx context.Context, repo discovery.Repository, root, path, expectedHead string) error {
	// 削除も slot 排他を先に取る。prepare が common-directory lock を手放している区間の実体を消さないためである。
	ctx, releaseSlot, err := m.lockSlot(ctx)
	if err != nil {
		return err
	}
	defer releaseSlot()
	return m.Git.WithCommonDirLock(ctx, string(repo.CommonDir), func(ctx context.Context) error {
		// 何かを解決する前に SQLite に記録されたパスをそのまま検査する。
		// 先に解決すると、別の登録済み worktree へ向ける symlink を見落とす。
		absoluteRoot, err := filepath.Abs(root)
		if err != nil {
			return err
		}
		absolutePath, err := filepath.Abs(path)
		if err != nil {
			return err
		}
		absoluteRoot, absolutePath = filepath.Clean(absoluteRoot), filepath.Clean(absolutePath)
		if !domain.IsWithin(absoluteRoot, absolutePath) {
			return removalOwnershipFailure(errors.New("worktree path is outside wx root"))
		}
		// 本番の Preparer は root descriptor を必ず持つ。記述子がない場合や別 root に pin されている場合は、
		// 記述子を取得できなかったか config/root 置換に問題がある。パス名だけの削除経路へ進めず、フェイルクローズする。
		if m.Preparer != nil && (m.Preparer.OwnedRoot == nil || filepath.Clean(m.Preparer.RootPath) != absoluteRoot) {
			return removalOwnershipFailure(errors.New("descriptor-bound worktree removal is unavailable"))
		}
		relative, err := filepath.Rel(absoluteRoot, absolutePath)
		if err != nil {
			return removalOwnershipFailure(err)
		}
		if err := validateRemovalPathComponents(absoluteRoot, relative); err != nil {
			return removalOwnershipFailure(err)
		}
		lockReason, registered, err := workspace.RegisteredWorktreeLockReason(ctx, m.Git, string(repo.MainPath), absolutePath)
		if err != nil {
			return err
		}
		_, statErr := os.Lstat(absolutePath)
		if errors.Is(statErr, os.ErrNotExist) {
			if !registered {
				return nil // A prior attempt completed physical and Git metadata removal.
			}
			common, err := filepath.EvalSymlinks(string(repo.CommonDir))
			if err != nil {
				return removalOwnershipFailure(err)
			}
			slotID, err := workspace.ValidateRemovalOwnership(absoluteRoot, absolutePath, m.markerIdentity(repo), common)
			if err != nil {
				return fmt.Errorf("validate missing worktree ownership: %w", err)
			}
			if err := validateWxLockReason(lockReason, slotID); err != nil {
				return removalOwnershipFailure(err)
			}
			if err := m.validateStateOwnership(ctx, repo, absolutePath, slotID, "", []string{"REMOVING", "RETIRING"}, []string{"READY", "RETIRING"}); err != nil {
				return fmt.Errorf("validate missing worktree SQLite ownership: %w", err)
			}
			if err := validateRemovalPathComponents(absoluteRoot, relative); err != nil {
				return removalOwnershipFailure(err)
			}
			if _, err := m.Git.Run(ctx, string(repo.MainPath), "worktree", "unlock", absolutePath); err != nil {
				return err
			}
			postReason, postLocked, found, err := workspace.RegisteredWorktreeLockStatus(ctx, m.Git, string(repo.MainPath), absolutePath)
			if err != nil {
				return err
			}
			if !found {
				return nil
			}
			if postLocked || postReason != "" {
				return removalOwnershipFailure(errors.New("worktree lock changed before removal"))
			}
			if _, err := workspace.ValidateRemovalOwnership(absoluteRoot, absolutePath, m.markerIdentity(repo), common); err != nil {
				return fmt.Errorf("revalidate missing worktree ownership: %w", err)
			}
			if err := m.validateStateOwnership(ctx, repo, absolutePath, slotID, "", []string{"REMOVING", "RETIRING"}, []string{"READY", "RETIRING"}); err != nil {
				return fmt.Errorf("revalidate missing worktree SQLite ownership: %w", err)
			}
			if err := validateRemovalPathComponents(absoluteRoot, relative); err != nil {
				return removalOwnershipFailure(err)
			}
			_, removeErr := m.Git.Run(ctx, string(repo.MainPath), "worktree", "remove", "--force", absolutePath)
			return removeErr
		}
		if statErr != nil {
			return removalOwnershipFailure(statErr)
		}
		return m.removeExistingWorktree(ctx, repo, absoluteRoot, absolutePath, relative, expectedHead, lockReason, registered)
	})
}

func (m *Manager) removeExistingWorktree(ctx context.Context, repo discovery.Repository, absoluteRoot, absolutePath, relative, expectedHead, lockReason string, registered bool) error {
	targetIdentity := ""
	if m.Preparer != nil && m.Preparer.OwnedRoot != nil && filepath.Clean(m.Preparer.RootPath) == absoluteRoot {
		directory, identity, identityErr := domain.OpenDirectoryAt(m.Preparer.OwnedRoot, relative)
		if identityErr != nil {
			return fmt.Errorf("%w: capture worktree identity before removal: %w", state.ErrOwnership, identityErr)
		}
		targetIdentity = identity
		if closeErr := directory.Close(); closeErr != nil {
			return fmt.Errorf("%w: close worktree identity descriptor: %w", state.ErrOwnership, closeErr)
		}
	}
	common, err := filepath.EvalSymlinks(string(repo.CommonDir))
	if err != nil {
		return removalOwnershipFailure(err)
	}
	slotID, err := workspace.ValidateRemovalOwnership(absoluteRoot, absolutePath, m.markerIdentity(repo), common)
	if err != nil {
		return fmt.Errorf("validate worktree ownership: %w", err)
	}
	if err := validateWxLockReason(lockReason, slotID); err != nil {
		return removalOwnershipFailure(err)
	}
	if err := m.validateStateOwnership(ctx, repo, absolutePath, slotID, targetIdentity, []string{"REMOVING", "RETIRING"}, []string{"READY", "RETIRING"}); err != nil {
		return fmt.Errorf("validate worktree SQLite ownership: %w", err)
	}
	canonicalRoot, err := filepath.EvalSymlinks(absoluteRoot)
	if err != nil {
		return removalOwnershipFailure(err)
	}
	canonicalPath, err := filepath.EvalSymlinks(absolutePath)
	if err != nil {
		return removalOwnershipFailure(err)
	}
	if !domain.IsWithin(canonicalRoot, canonicalPath) {
		return removalOwnershipFailure(errors.New("worktree path is outside canonical wx root"))
	}
	commonOutput, err := m.gitValue(ctx, absolutePath, nil, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return err
	}
	actual, err := filepath.EvalSymlinks(commonOutput)
	if err != nil {
		return removalOwnershipFailure(err)
	}
	if actual != common {
		return removalOwnershipFailure(errors.New("worktree common directory does not match repository ownership"))
	}
	if expectedHead != "" {
		head, err := m.gitValue(ctx, absolutePath, nil, "rev-parse", "HEAD")
		if err != nil || head != expectedHead {
			return removalOwnershipFailure(errors.New("worktree HEAD does not match SQLite ownership metadata"))
		}
	}
	if !registered {
		return removalOwnershipFailure(errors.New("worktree is not registered at expected path"))
	}
	if err := validateRemovalPathComponents(absoluteRoot, relative); err != nil {
		return removalOwnershipFailure(err)
	}
	if _, err := m.Git.Run(ctx, string(repo.MainPath), "worktree", "unlock", absolutePath); err != nil {
		return err
	}
	postReason, postLocked, found, err := workspace.RegisteredWorktreeLockStatus(ctx, m.Git, string(repo.MainPath), absolutePath)
	if err != nil {
		return err
	}
	if !found {
		return nil
	}
	if postLocked || postReason != "" {
		return removalOwnershipFailure(errors.New("worktree lock changed before removal"))
	}
	if _, err := workspace.ValidateRemovalOwnership(absoluteRoot, absolutePath, m.markerIdentity(repo), common); err != nil {
		return fmt.Errorf("revalidate worktree ownership: %w", err)
	}
	if err := m.validateStateOwnership(ctx, repo, absolutePath, slotID, targetIdentity, []string{"REMOVING", "RETIRING"}, []string{"READY", "RETIRING"}); err != nil {
		return fmt.Errorf("revalidate worktree SQLite ownership: %w", err)
	}
	if err := validateRemovalPathComponents(absoluteRoot, relative); err != nil {
		return removalOwnershipFailure(err)
	}
	commonOutput, err = m.gitValue(ctx, absolutePath, nil, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return err
	}
	actual, err = filepath.EvalSymlinks(commonOutput)
	if err != nil || actual != common {
		return removalOwnershipFailure(errors.New("worktree common directory changed before removal"))
	}
	if expectedHead != "" {
		head, err := m.gitValue(ctx, absolutePath, nil, "rev-parse", "HEAD")
		if err != nil || head != expectedHead {
			return removalOwnershipFailure(errors.New("worktree HEAD changed before removal"))
		}
	}
	// 破壊的な Git 操作の直前に行う最後の永続的な所有権検査である。
	// common-directory lock を物理/Git 検査と状態読取りの間も保持するため、偽造した marker/lock だけでは削除を許可できない。
	if err := m.validateStateOwnership(ctx, repo, absolutePath, slotID, targetIdentity, []string{"REMOVING", "RETIRING"}, []string{"READY", "RETIRING"}); err != nil {
		return fmt.Errorf("worktree SQLite ownership changed before removal: %w", err)
	}
	if targetIdentity != "" {
		return m.Preparer.RemoveWorktreeAt(ctx, repo, absoluteRoot, absolutePath, targetIdentity)
	}
	_, err = m.Git.Run(ctx, string(repo.MainPath), "worktree", "remove", "--force", absolutePath)
	return err
}

func removalOwnershipFailure(err error) error {
	if err == nil || errors.Is(err, state.ErrOwnership) {
		return err
	}
	return fmt.Errorf("%w: %w", state.ErrOwnership, err)
}

// validateStateOwnership は SQLite に対して削除対象を証明する。dirIdentity は呼び出し元が開いている directory の inode identity である。
// これにより SQLite record の欠落を失敗にするため、descriptor を持つ呼び出し元は必ず渡す。空にできるのは対象 directory が既にない場合だけである。
func (m *Manager) validateStateOwnership(ctx context.Context, repo discovery.Repository, target, slotID, dirIdentity string, slotStates, repositoryStates []string) error {
	validator := m.Ownership
	if validator == nil && m.Preparer != nil {
		validator = m.Preparer.Ownership
	}
	if validator == nil {
		return fmt.Errorf("%w: state-backed worktree ownership validator is required", state.ErrOwnership)
	}
	rootID, slotRel, dirName, err := m.worktreeLocation(target)
	if err != nil {
		return err
	}
	_, err = validator.ValidateWorktreeOwnership(ctx, state.WorktreeOwnershipRequest{
		SlotID:                  slotID,
		RepositoryID:            string(repo.ID),
		RootID:                  rootID,
		SlotRelPath:             slotRel,
		DirName:                 dirName,
		DirIdentity:             dirIdentity,
		CommonDir:               string(repo.CommonDir),
		AllowedSlotStates:       slotStates,
		AllowedRepositoryStates: repositoryStates,
	})
	return err
}

// markerIdentity はこの manager が repository に要求する marker の同一性を返す。
// root generation は Preparer から取得する。daemon が削除対象の pin 済み root を記録する場所はここだけである。
func (m *Manager) markerIdentity(repo discovery.Repository) workspace.MarkerIdentity {
	rootID := ""
	if m.Preparer != nil {
		rootID = m.Preparer.RootID
	}
	return workspace.MarkerIdentity{RootID: rootID, RepositoryID: string(repo.ID)}
}

// worktreeLocation は SQLite に記録した target の場所、すなわち root generation、slot の root 相対パス、repository directory 名を返す。
// 本番ではこれらを持つ Preparer で manager を構築する。比較値がない場合はパス名比較へ戻さず、削除をフェイルクローズする。
func (m *Manager) worktreeLocation(target string) (rootID, slotRel, dirName string, err error) {
	if m.Preparer == nil || m.Preparer.RootID == "" || m.Preparer.SlotRelPath == "" {
		return "", "", "", fmt.Errorf("%w: worktree slot location is unavailable", state.ErrOwnership)
	}
	dirName, err = m.Preparer.WorktreeDirName(target)
	if err != nil {
		return "", "", "", err
	}
	return m.Preparer.RootID, m.Preparer.SlotRelPath, dirName, nil
}

func validateWxLockReason(reason, slotID string) error {
	if !domain.ValidWxLockReason(reason, slotID) {
		return fmt.Errorf("worktree lock reason does not belong to wx slot %s", slotID)
	}
	return nil
}

// validateRemovalPathComponents は削除対象までの全 symlink を拒否する。どの階層でも成分がなければ、leaf もなく削除を逸らす symlink は残らない。
// worktree は <workspace-id>/<slot-id>/<RepoName> にあり、removeSlotWorktrees は worktree 後に slot directory も削除する。
// RemoveAll 後で ARCHIVED commit 前に中断すると中間成分がないため、leaf だけで ENOENT を許容すると完了済み slot を誤って quarantine する。
// commentlint:allow-long -- 中断した削除を再実行する際の ENOENT の許容範囲を説明する
func validateRemovalPathComponents(root, relative string) error {
	current := root
	for _, component := range strings.Split(relative, string(filepath.Separator)) {
		if component == "." {
			continue
		}
		current = filepath.Join(current, component)
		info, err := os.Lstat(current)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("symlink component in removal path %s", current)
		}
	}
	return nil
}
