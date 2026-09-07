package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/HappyOnigiri/WX/internal/domain"
	"github.com/HappyOnigiri/WX/internal/state"
)

// removeRegisteredSlot は DB の登録範囲だけを削除する。inode・marker・HEAD は削除権限の条件にしない。
// Git 管理情報は DB の common directory から対象 path を逆引きし、slot 内の .git が指す任意の場所には触れない。
func (m *Manager) removeRegisteredSlot(ctx context.Context, slot state.Slot) error {
	rootPath, err := m.store.RegisteredRemovalRoot(ctx, slot.ID, slot.RootID, slot.RelPath)
	if err != nil {
		return err
	}
	if !filepath.IsLocal(slot.RelPath) || slot.RelPath == "." {
		return fmt.Errorf("%w: invalid registered slot path: %s", state.ErrOwnership, slot.RelPath)
	}
	repos, err := m.store.SlotRepositories(ctx, slot.ID)
	if err != nil {
		return err
	}
	for _, sr := range repos {
		if !filepath.IsLocal(sr.DirName) || strings.ContainsAny(sr.DirName, `/\\`) || sr.DirName == "." {
			return fmt.Errorf("%w: invalid registered repository path: %s", state.ErrOwnership, sr.DirName)
		}
	}
	owner, err := os.OpenRoot(rootPath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if owner != nil {
		defer func() { _ = owner.Close() }()
		parent := filepath.Dir(slot.RelPath)
		if parent != "." {
			if _, err := domain.PhysicalPathInfo(owner, parent); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
		if err := owner.RemoveAll(slot.RelPath); err != nil {
			return err
		}
	}
	for _, sr := range repos {
		repo, err := m.store.Repository(ctx, sr.RepositoryID)
		if err != nil {
			return err
		}
		if err := m.removeGitRegistration(ctx, string(repo.CommonDir), filepath.Join(rootPath, slot.RelPath, sr.DirName)); err != nil {
			return err
		}
	}
	return nil
}

// removeGitRegistration は対象を指す backlink のある Git 管理ディレクトリだけを整理する。
// lock の理由や存在は問わず、別 worktree の登録は維持する。
func (m *Manager) removeGitRegistration(ctx context.Context, common, target string) error {
	return m.git.WithCommonDirLock(ctx, common, func(ctx context.Context) error {
		return m.removeGitRegistrationLocked(ctx, common, target)
	})
}

func (m *Manager) removeGitRegistrationLocked(ctx context.Context, common, target string) error {
	owner, err := os.OpenRoot(common)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer func() { _ = owner.Close() }()
	if _, err := domain.PhysicalPathInfo(owner, "worktrees"); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	dir, err := owner.Open("worktrees")
	if err != nil {
		return err
	}
	entries, err := dir.ReadDir(-1)
	_ = dir.Close()
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		name := filepath.Join("worktrees", entry.Name())
		info, err := owner.Lstat(filepath.Join(name, "gitdir"))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			continue
		}
		data, err := owner.ReadFile(filepath.Join(name, "gitdir"))
		if err != nil {
			return err
		}
		backlink := strings.TrimSpace(string(data))
		if !filepath.IsAbs(backlink) {
			backlink = filepath.Join(common, name, backlink)
		}
		if filepath.Clean(backlink) != filepath.Join(target, ".git") {
			continue
		}
		directory, err := owner.Open(".")
		if err != nil {
			return err
		}
		_, gitErr := m.git.RunAt(ctx, directory, nil, nil, "--git-dir=.", "worktree", "remove", "--force", "--force", target)
		_ = directory.Close()
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if gitErr == nil {
			continue
		}
		// 実体は削除済みなので、壊れた登録を Git が扱えなくても対象の管理情報だけを回収する。
		if err := owner.RemoveAll(name); err != nil {
			return err
		}
	}
	return nil
}

// removeRegisteredSnapshot は期限切れの登録済み archive を内容の一致に依存せず削除する。
func removeRegisteredSnapshot(owner *os.Root, relative string) error {
	if !filepath.IsLocal(relative) || relative == "." {
		return errors.New("invalid registered snapshot path")
	}
	if _, err := domain.PhysicalPathInfo(owner, filepath.Dir(relative)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return owner.RemoveAll(relative)
}
