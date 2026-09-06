package workspace

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"

	"github.com/HappyOnigiri/WX/internal/config"
	"github.com/HappyOnigiri/WX/internal/discovery"
	"github.com/HappyOnigiri/WX/internal/domain"
	"github.com/HappyOnigiri/WX/internal/gitx"
	"github.com/HappyOnigiri/WX/internal/state"
)

// requirePinnedRoot は daemon が保持する root descriptor が存在し、root に pin されていない限り fail closed する。
// production では常にこの方法で Preparer を構築し、可変な path 名への fallback はない。
func (p *Preparer) requirePinnedRoot(root string) error {
	if p.OwnedRoot == nil || filepath.Clean(p.RootPath) != filepath.Clean(root) {
		return errors.New("wx worktree root descriptor is unavailable")
	}
	return nil
}

func (p *Preparer) verifyPreparedTargetIdentity(lockedRoot *os.Root, relative, expected string) error {
	if expected == "" {
		return nil
	}
	currentDirectory, currentIdentity, identityErr := domain.OpenDirectoryAt(lockedRoot, relative)
	if currentDirectory != nil {
		_ = currentDirectory.Close()
	}
	if identityErr != nil {
		return fmt.Errorf("%w: worktree target identity is unavailable: %w", state.ErrOwnership, identityErr)
	}
	if currentIdentity != expected {
		return fmt.Errorf("%w: worktree target identity changed (expected %s, got %s)", state.ErrOwnership, expected, currentIdentity)
	}
	return nil
}

func (p *Preparer) openOwnedRoot(root, target string) (*os.Root, string, func(), error) {
	if err := p.requirePinnedRoot(root); err != nil {
		return nil, "", func() {}, err
	}
	relative, err := filepath.Rel(filepath.Clean(root), filepath.Clean(target))
	if err != nil {
		return nil, "", func() {}, err
	}
	return p.OwnedRoot, relative, func() {}, nil
}

// addWorktree は pin 済み parent descriptor から最終 directory を作成して target namespace を予約し、その descriptor を cwd、`.` を target にして Git を起動する。
// 以後 lexical root/ancestor/target が置換されても、Git の checkout と worktree registration を所有 inode の外へ向けられない。
func (p *Preparer) addWorktree(ctx context.Context, repo discovery.Repository, owner *os.Root, target, relativeTarget, oid string) error {
	_, err := p.addWorktreeWithIdentity(ctx, repo, owner, target, relativeTarget, oid)
	return err
}

func (p *Preparer) addWorktreeWithIdentity(ctx context.Context, repo discovery.Repository, owner *os.Root, target, relativeTarget, oid string) (string, error) {
	parentRelative := filepath.Dir(relativeTarget)
	parent, _, err := domain.OpenDirectoryAt(owner, parentRelative)
	if err != nil {
		return "", fmt.Errorf("open worktree target namespace: %w", err)
	}
	defer func() { _ = parent.Close() }()
	if info, statErr := parent.Stat(); statErr != nil || !info.IsDir() {
		if statErr != nil {
			return "", fmt.Errorf("validate worktree target namespace: %w", statErr)
		}
		return "", errors.New("worktree target namespace is not a directory")
	}
	name := filepath.Base(relativeTarget)
	if name == "." || name == string(filepath.Separator) || name == "" {
		return "", errors.New("worktree target has no relative leaf")
	}
	// 最終 leaf は mkdirat で予約する。ここで owner.Mkdir(relativeTarget) を使うと、descriptor barrier 後に parent path を開き直してしまう。
	if err := unix.Mkdirat(int(parent.Fd()), name, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return "", fmt.Errorf("reserve worktree target namespace: %w", err)
	}
	targetDirectory, targetIdentity, err := domain.OpenDirectoryAt(owner, relativeTarget)
	if err != nil {
		return "", fmt.Errorf("open reserved worktree target: %w", err)
	}
	defer func() { _ = targetDirectory.Close() }()
	// `--git-dir` は cwd を変えず source repository を特定する。`-C` では target を repo.MainPath 基準に解決し、上で確立した descriptor-bound namespace を失う。
	_, err = p.Git.RunAt(ctx, targetDirectory, nil, nil, "--git-dir", string(repo.CommonDir), "worktree", "add", "--detach", ".", oid)
	if err == nil {
		return targetIdentity, nil
	}
	// Git は error 報告前に common-directory registration を更新し得る。child 中断も含め、descriptor namespace または registration の clean を証明できなければ、
	// 曖昧な add を通常の FAILED slot にせず、daemon の ownership quarantine 用に target を残す。
	entries, readErr := targetDirectory.Readdirnames(-1)
	if readErr != nil {
		return "", fmt.Errorf("%w: inspect git worktree add target: %w", state.ErrOwnership, readErr)
	}
	if len(entries) > 0 {
		return "", fmt.Errorf("%w: git worktree add outcome is uncertain: %w", state.ErrOwnership, err)
	}
	_, _, found, inspectErr := RegisteredWorktreeLockStatusAt(ctx, p.Git, string(repo.MainPath), owner, p.RootPath, relativeTarget, targetIdentity)
	if inspectErr != nil || found {
		if inspectErr == nil {
			inspectErr = errors.New("Git registration remains after failed worktree add")
		}
		return "", fmt.Errorf("%w: git worktree add registration is uncertain: %w", state.ErrOwnership, inspectErr)
	}
	var gitErr *gitx.Error
	if errors.As(err, &gitErr) && gitErr.Result.ExitCode < 0 {
		return "", fmt.Errorf("%w: git worktree add process outcome is uncertain: %w", state.ErrOwnership, err)
	}
	return "", err
}

func (p *Preparer) runWorktreeAdmin(ctx context.Context, repo discovery.Repository, owner *os.Root, relativeTarget, target string, args ...string) (gitx.Result, error) {
	return p.runWorktreeAdminOwned(ctx, repo, owner, relativeTarget, target, "", args...)
}

// RemoveWorktreeAt は descriptor-bound target inode から破壊的な worktree 削除を行う。
// 呼び出し元は Git common-directory lock 中に所有権検査を完了していなければならない。
// 検査後の失敗は Git 起動中の rename/置換を示し得るため ownership-uncertain とし、ErrOwnership として返す。
func (p *Preparer) RemoveWorktreeAt(ctx context.Context, repo discovery.Repository, root, target, expectedIdentity string) error {
	if err := p.requirePinnedRoot(root); err != nil {
		return fmt.Errorf("%w: descriptor-bound worktree removal requires the pinned root", state.ErrOwnership)
	}
	relativeTarget, err := domain.RelativeWithin(filepath.Clean(root), filepath.Clean(target))
	if err != nil {
		return fmt.Errorf("%w: worktree target is outside the pinned root: %w", state.ErrOwnership, err)
	}
	if _, err := p.runWorktreeAdminOwned(ctx, repo, p.OwnedRoot, relativeTarget, target, expectedIdentity, "remove", "--force"); err != nil {
		return fmt.Errorf("%w: descriptor-bound worktree removal is uncertain: %w", state.ErrOwnership, err)
	}
	return nil
}

func (p *Preparer) runWorktreeAdminOwned(ctx context.Context, repo discovery.Repository, owner *os.Root, relativeTarget, target, expectedIdentity string, args ...string) (gitx.Result, error) {
	if p.OwnedRoot == nil {
		return gitx.Result{}, errors.New("wx worktree root descriptor is unavailable")
	}
	targetDirectory, identity, err := domain.OpenDirectoryAt(owner, relativeTarget)
	if err != nil {
		return gitx.Result{}, fmt.Errorf("open worktree target namespace: %w", err)
	}
	defer func() { _ = targetDirectory.Close() }()
	if expectedIdentity != "" && identity != expectedIdentity {
		return gitx.Result{}, fmt.Errorf("%w: worktree target identity changed (expected %s, got %s)", state.ErrOwnership, expectedIdentity, identity)
	}
	// cwd は descriptor-bound target に保つ。`--git-dir` で source repository を選び、`.` で予約済み worktree inode を指定する。
	command := append([]string{"--git-dir", string(repo.CommonDir), "worktree"}, args...)
	command = append(command, ".")
	result, runErr := p.Git.RunAt(ctx, targetDirectory, nil, nil, command...)
	if runErr != nil && expectedIdentity != "" {
		currentDirectory, currentIdentity, identityErr := domain.OpenDirectoryAt(owner, relativeTarget)
		if currentDirectory != nil {
			_ = currentDirectory.Close()
		}
		if identityErr != nil {
			return result, fmt.Errorf("%w: worktree target identity became unavailable during Git operation: %w", state.ErrOwnership, identityErr)
		}
		if currentIdentity != expectedIdentity {
			return result, fmt.Errorf("%w: worktree target identity changed during Git operation (expected %s, got %s)", state.ErrOwnership, expectedIdentity, currentIdentity)
		}
	}
	return result, runErr
}

func (p *Preparer) runGitInDirectory(ctx context.Context, directory *os.File, args ...string) (gitx.Result, error) {
	return p.Git.RunAt(ctx, directory, nil, nil, args...)
}

// WorktreeIdentity は configured ownership root 経由で target の volume/inode identity を返す。
// 復元済みまたは lease 中 worktree に複数操作をする呼び出し元はこれを保持し、置換 target を元の slot と取り違えない。
func (p *Preparer) WorktreeIdentity(target string) (string, error) {
	root, err := config.ExpandHome(p.Config.Storage.WorktreeRoot)
	if err != nil {
		return "", err
	}
	target = filepath.Clean(target)
	if !domain.IsWithin(root, target) {
		return "", fmt.Errorf("worktree target is outside wx ownership root")
	}
	owner, relative, closeOwner, err := p.openOwnedRoot(root, target)
	if err != nil {
		return "", err
	}
	defer closeOwner()
	directory, identity, err := domain.OpenDirectoryAt(owner, relative)
	if err != nil {
		return "", err
	}
	if err := directory.Close(); err != nil {
		return "", err
	}
	return identity, nil
}

// VerifyWorktreeIdentity は target が呼び出し元の取得した physical directory を指さなくなった場合に拒否する。
// expected identity が空なら、以前の in-process caller 用の互換経路を維持する。
func (p *Preparer) VerifyWorktreeIdentity(target, expectedIdentity string) error {
	if expectedIdentity == "" {
		return nil
	}
	actual, err := p.WorktreeIdentity(target)
	if err != nil {
		return fmt.Errorf("%w: worktree target identity is unavailable: %w", state.ErrOwnership, err)
	}
	if actual != expectedIdentity {
		return fmt.Errorf("%w: worktree target identity changed (expected %s, got %s)", state.ErrOwnership, expectedIdentity, actual)
	}
	return nil
}

// RunGitInWorktree は WorktreeIdentity で取得した identity の target で Git command を実行する。
// 実行前に identity を検査したうえで、pin 済み directory descriptor を internal/fdexec 経由の fchdir で
// 子プロセスの cwd に束縛してから Git を起動するため、実行中に pathname が rename・置換されても子は
// pin した inode を見続ける。したがって実行後の再検証は行わない。
// commentlint:allow-long -- fchdir 束縛により実行後再検証が不要になる根拠を保守時に確認できるようにする
func (p *Preparer) RunGitInWorktree(ctx context.Context, target, expectedIdentity string, env []string, input []byte, args ...string) (gitx.Result, error) {
	root, err := config.ExpandHome(p.Config.Storage.WorktreeRoot)
	if err != nil {
		return gitx.Result{}, err
	}
	owner, relative, closeOwner, err := p.openOwnedRoot(root, filepath.Clean(target))
	if err != nil {
		return gitx.Result{}, fmt.Errorf("open worktree command root: %w", err)
	}
	defer closeOwner()
	directory, identity, err := domain.OpenDirectoryAt(owner, relative)
	if err != nil {
		return gitx.Result{}, fmt.Errorf("open worktree command directory: %w", err)
	}
	defer func() { _ = directory.Close() }()
	if expectedIdentity != "" && identity != expectedIdentity {
		return gitx.Result{}, fmt.Errorf("%w: worktree target identity changed before Git (expected %s, got %s)", state.ErrOwnership, expectedIdentity, identity)
	}
	return p.Git.RunAt(ctx, directory, env, input, args...)
}
