package workspace

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/HappyOnigiri/WX/internal/config"
	"github.com/HappyOnigiri/WX/internal/discovery"
	"github.com/HappyOnigiri/WX/internal/domain"
	"github.com/HappyOnigiri/WX/internal/fdexec"
	"github.com/HappyOnigiri/WX/internal/gitx"
	"github.com/HappyOnigiri/WX/internal/state"
)

type Preparer struct {
	Git       *gitx.Runner
	Config    config.Config
	Ownership state.OwnershipValidator
	SlotPath  string
	// OwnedRoot は設定済み worktree root 用に daemon が保持する。
	// 設定時は、target の作成と descriptor-bound Git 操作のすべてで、可変なパス名を開き直さずこの inode namespace を使う。
	OwnedRoot *os.Root
	RootPath  string
	// RootID と SlotRelPath は、SQLite が slot の場所を durable root 世代と root 相対パスで記録する値である。
	// 所有権検証は絶対 SlotPath の代わりにこれらを比較し、root の改名や再設定で別 directory が同じ slot に見えることを防ぐ。
	RootID      string
	SlotRelPath string
}

// markerIdentity は、repository ごとの slot 用 marker identity を作る。
func (p *Preparer) markerIdentity(repo discovery.Repository, slotID string) MarkerIdentity {
	return MarkerIdentity{SlotID: slotID, RootID: p.RootID, RepositoryID: string(repo.ID)}
}

// WorktreeDirName は worktreeDirName の exported 版であり、internal/archive が prepare と同じ方法で削除対象を表すために使う。
func (p *Preparer) WorktreeDirName(target string) (string, error) {
	return p.worktreeDirName(target)
}

// worktreeDirName は slot 内の repository directory 名、つまり SlotPath と target の間にある単一 path component を返す。
// 設定から再計算せず呼び出し元の target から読み取り、slot 作成時に記録した名前を権威として既存 slot の向き先変更を防ぐ。
func (p *Preparer) worktreeDirName(target string) (string, error) {
	if p.SlotPath == "" {
		return "", fmt.Errorf("%w: slot path is unavailable", state.ErrOwnership)
	}
	relative, err := filepath.Rel(filepath.Clean(p.SlotPath), filepath.Clean(target))
	if err != nil {
		return "", fmt.Errorf("%w: worktree is not inside its slot: %w", state.ErrOwnership, err)
	}
	if relative == "." || !filepath.IsLocal(relative) || strings.ContainsRune(relative, filepath.Separator) {
		return "", fmt.Errorf("%w: worktree %s is not a direct child of slot %s", state.ErrOwnership, target, p.SlotPath)
	}
	return relative, nil
}

func (p *Preparer) Prepare(ctx context.Context, repo discovery.Repository, target, oid, slotID string) error {
	return p.prepare(ctx, repo, target, oid, slotID, preparePhaseCreate)
}

// PrepareForRestore は worktree を RESTORING としてロックしたまま restore の clean base を作る。
// resume-phase の prepare command を実行する前に、snapshot 内容と保存済み index を配置しておく必要がある。
func (p *Preparer) PrepareForRestore(ctx context.Context, repo discovery.Repository, target, oid, slotID string) error {
	return p.prepare(ctx, repo, target, oid, slotID, preparePhaseRestore)
}

type preparePhase string

const (
	preparePhaseCreate  preparePhase = "create"
	preparePhaseRestore preparePhase = "restore"
)

func (p *Preparer) prepare(ctx context.Context, repo discovery.Repository, target, oid, slotID string, phase preparePhase) error {
	root, target, err := p.prepareTarget(target)
	if err != nil {
		return err
	}
	return p.Git.WithCommonDirLock(string(repo.CommonDir), func() error {
		return p.prepareLocked(ctx, repo, target, oid, slotID, phase, root)
	})
}

func (p *Preparer) prepareTarget(target string) (string, string, error) {
	root, err := config.ExpandHome(p.Config.Storage.WorktreeRoot)
	if err != nil {
		return "", "", err
	}
	target = filepath.Clean(target)
	if !domain.IsWithin(root, target) {
		return "", "", fmt.Errorf("target %s is outside wx worktree root", target)
	}
	if err := p.requirePinnedRoot(root); err != nil {
		return "", "", err
	}
	ownedRoot, relativeTarget, closeOwnedRoot, err := p.openOwnedRoot(root, target)
	if err != nil {
		return "", "", fmt.Errorf("open wx worktree root: %w", err)
	}
	defer closeOwnedRoot()
	if err := ownedRoot.MkdirAll(filepath.Dir(relativeTarget), 0o700); err != nil {
		return "", "", fmt.Errorf("create worktree parent safely: %w", err)
	}
	return root, target, nil
}

func (p *Preparer) prepareLocked(ctx context.Context, repo discovery.Repository, target, oid, slotID string, phase preparePhase, root string) error {
	locked, err := p.prepareLockedTarget(ctx, repo, target, oid, slotID, phase, root)
	if err != nil {
		return err
	}
	defer locked.close()
	lockedRoot := locked.root
	lockedRelativeTarget := locked.relative
	existingWorktree := locked.existing
	targetIdentity := locked.identity

	cleanup := !existingWorktree
	ownedAfterLock := false
	defer func() {
		if cleanup && ownedAfterLock {
			// 失敗した preparation は所有権を証明できる間だけ削除できる。
			// command や並行する filesystem 変更で証明が無効になった場合は、ロック済み target と marker を quarantine/reconcile 用に残す。
			if err := p.validatePreparedTarget(context.Background(), repo, target, oid, slotID, phase, lockedRoot, lockedRelativeTarget, targetIdentity, "validate worktree before cleanup"); err != nil {
				return
			}
			if _, err := p.runWorktreeAdminOwned(context.Background(), repo, lockedRoot, lockedRelativeTarget, target, targetIdentity, "unlock"); err != nil {
				return
			}
			if _, err := p.runWorktreeAdminOwned(context.Background(), repo, lockedRoot, lockedRelativeTarget, target, targetIdentity, "remove", "--force"); err != nil {
				return
			}
			_ = removeOwnershipMarkerAt(lockedRoot, root, target, string(repo.ID))
		}
	}()
	if existingWorktree {
		if _, err := p.runWorktreeAdminOwned(ctx, repo, lockedRoot, lockedRelativeTarget, target, targetIdentity, "unlock"); err != nil {
			return fmt.Errorf("unlock existing wx worktree: %w", err)
		}
	}
	lockState := "PREPARING"
	if phase == preparePhaseRestore {
		lockState = "RESTORING"
	}
	if _, err := p.runWorktreeAdminOwned(ctx, repo, lockedRoot, lockedRelativeTarget, target, targetIdentity, "lock", "--reason", "wx:"+slotID+":"+lockState); err != nil {
		return err
	}
	// この検証は意図的に新しい lock の取得後に行う。
	// file 操作の前に marker、physical path、Git registration、OID、lock reason が同じ slot を示すことを証明する。
	if err := p.validatePreparedTarget(ctx, repo, target, oid, slotID, phase, lockedRoot, lockedRelativeTarget, targetIdentity, "wx worktree ownership changed after lock"); err != nil {
		return fmt.Errorf("wx worktree ownership changed after lock: %w", err)
	}
	ownedAfterLock = true
	// worktree に書き込む、または再利用する各操作の直前に durable owner を再検証する。
	// common-directory lock は Git metadata を守り、この read-only な state の証明は slot/path の対応と state machine を独立に守る。
	if err := p.validatePreparedTarget(ctx, repo, target, oid, slotID, phase, lockedRoot, lockedRelativeTarget, targetIdentity, "wx worktree ownership changed before includes"); err != nil {
		return fmt.Errorf("wx worktree ownership changed before includes: %w", err)
	}
	if err := p.copyIncludesAt(repo, lockedRoot, lockedRelativeTarget); err != nil {
		return err
	}
	if err := p.validatePreparedTarget(ctx, repo, target, oid, slotID, phase, lockedRoot, lockedRelativeTarget, targetIdentity, "wx worktree ownership changed before links"); err != nil {
		return fmt.Errorf("wx worktree ownership changed before links: %w", err)
	}
	if err := p.createLinksAt(ctx, repo, lockedRoot, lockedRelativeTarget); err != nil {
		return err
	}
	if phase == preparePhaseCreate {
		if err := p.validatePreparedTarget(ctx, repo, target, oid, slotID, phase, lockedRoot, lockedRelativeTarget, targetIdentity, "wx worktree ownership changed before prepare command"); err != nil {
			return fmt.Errorf("wx worktree ownership changed before prepare command: %w", err)
		}
		if err := p.runPrepareWithIdentity(ctx, repo, target, ""); err != nil {
			return err
		}
		if err := p.verifyPreparedTargetIdentity(lockedRoot, lockedRelativeTarget, targetIdentity); err != nil {
			return fmt.Errorf("wx worktree ownership changed during prepare command: %w", err)
		}
	}
	if phase == preparePhaseCreate {
		if err := p.verifyPreparedTargetIdentity(lockedRoot, lockedRelativeTarget, targetIdentity); err != nil {
			return fmt.Errorf("wx worktree ownership changed before tracked status: %w", err)
		}
		if err := p.validateTrackedClean(ctx, target); err != nil {
			return err
		}
		if err := p.verifyPreparedTargetIdentity(lockedRoot, lockedRelativeTarget, targetIdentity); err != nil {
			return fmt.Errorf("wx worktree ownership changed during tracked status: %w", err)
		}
	}
	targetRoot, currentIdentity, err := domain.OpenDirectoryAt(lockedRoot, lockedRelativeTarget)
	if err != nil {
		return fmt.Errorf("open prepared worktree: %w", err)
	}
	defer func() { _ = targetRoot.Close() }()
	if targetIdentity != "" && currentIdentity != targetIdentity {
		return fmt.Errorf("%w: prepared worktree identity changed (expected %s, got %s)", state.ErrOwnership, targetIdentity, currentIdentity)
	}
	head, err := p.runGitInDirectory(ctx, targetRoot, "rev-parse", "HEAD")
	if err != nil {
		return err
	}
	if strings.TrimSpace(head.Stdout) != oid {
		return fmt.Errorf("prepared HEAD differs from requested OID")
	}
	if _, err := p.runGitInDirectory(ctx, targetRoot, "symbolic-ref", "-q", "HEAD"); err == nil {
		return errors.New("prepared worktree is not detached")
	}
	if phase == preparePhaseRestore {
		// archive.Manager が snapshot の tree/index を復元し、resume-phase command を実行するまで RESTORING lock を保持する。
		cleanup = false
		return nil
	}
	if _, err = p.runWorktreeAdminOwned(ctx, repo, lockedRoot, lockedRelativeTarget, target, targetIdentity, "unlock"); err != nil {
		return err
	}
	_, err = p.runWorktreeAdminOwned(ctx, repo, lockedRoot, lockedRelativeTarget, target, targetIdentity, "lock", "--reason", "wx:"+slotID+":READY")
	if err != nil {
		return err
	}
	if err := p.validatePreparedTarget(ctx, repo, target, oid, slotID, phase, lockedRoot, lockedRelativeTarget, targetIdentity, "wx worktree ownership changed before READY"); err != nil {
		return fmt.Errorf("wx worktree ownership changed before READY: %w", err)
	}
	cleanup = false
	return nil
}

type lockedTarget struct {
	root     *os.Root
	relative string
	identity string
	existing bool
	close    func()
}

func (p *Preparer) prepareLockedTarget(ctx context.Context, repo discovery.Repository, target, oid, slotID string, phase preparePhase, root string) (*lockedTarget, error) {
	prepareSlotStates, prepareRepositoryStates := preparationOwnershipStates(phase)
	// common-directory lock の取得後に descriptor を開き直し、physical/ownership 検査を繰り返す。
	// lock 前の検査だけでは、検証と Git 操作の間に path を置換できてしまう。
	lockedRoot, lockedRelativeTarget, closeLockedRoot, err := p.openOwnedRoot(root, target)
	if err != nil {
		return nil, fmt.Errorf("revalidate wx worktree root: %w", err)
	}
	keepOpen := false
	defer func() {
		if !keepOpen {
			closeLockedRoot()
		}
	}()
	if err := lockedRoot.MkdirAll(filepath.Dir(lockedRelativeTarget), 0o700); err != nil {
		return nil, fmt.Errorf("create worktree parent safely: %w", err)
	}
	if _, err := lockedRoot.Lstat(lockedRelativeTarget); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	existingWorktree, err := p.existingTargetState(ctx, repo, target, oid, slotID, phase, root, lockedRoot, lockedRelativeTarget)
	if err != nil {
		return nil, err
	}
	targetIdentity := ""
	if existingWorktree {
		identityDirectory, identity, identityErr := domain.OpenDirectoryAt(lockedRoot, lockedRelativeTarget)
		if identityDirectory != nil {
			_ = identityDirectory.Close()
		}
		if identityErr != nil {
			return nil, fmt.Errorf("%w: capture existing worktree identity: %w", state.ErrOwnership, identityErr)
		}
		targetIdentity = identity
	}
	if err := p.validateStateOwnership(ctx, repo, target, slotID, prepareSlotStates, prepareRepositoryStates); err != nil {
		return nil, fmt.Errorf("wx worktree ownership changed before marker: %w", err)
	}
	if err := EnsureOwnershipMarkerAt(lockedRoot, root, target, p.markerIdentity(repo, slotID), string(repo.CommonDir)); err != nil {
		return nil, fmt.Errorf("prepare wx ownership marker: %w", err)
	}
	if !existingWorktree {
		parentDirectory, _, parentErr := domain.OpenDirectoryAt(lockedRoot, filepath.Dir(lockedRelativeTarget))
		if parentErr != nil {
			return nil, fmt.Errorf("%w: worktree parent ownership changed: %w", state.ErrOwnership, parentErr)
		}
		if closeErr := parentDirectory.Close(); closeErr != nil {
			return nil, fmt.Errorf("%w: close worktree parent descriptor: %w", state.ErrOwnership, closeErr)
		}
		if err := p.validateStateOwnership(ctx, repo, target, slotID, prepareSlotStates, prepareRepositoryStates); err != nil {
			return nil, fmt.Errorf("wx worktree ownership changed before creation: %w", err)
		}
		targetIdentity, err = p.addWorktreeWithIdentity(ctx, repo, lockedRoot, target, lockedRelativeTarget, oid)
		if err != nil {
			return nil, err
		}
		if err := p.verifyPreparedTargetIdentity(lockedRoot, lockedRelativeTarget, targetIdentity); err != nil {
			return nil, fmt.Errorf("prepared worktree escaped ownership root: %w", err)
		}
	}
	keepOpen = true
	return &lockedTarget{root: lockedRoot, relative: lockedRelativeTarget, identity: targetIdentity, existing: existingWorktree, close: closeLockedRoot}, nil
}

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

// PrepareResumeWithIdentity は descriptor-bound resume phase である。
// archive.Manager が clean base 作成後に渡す identity を、resume command と最後の所有権証明まで保持する。
func (p *Preparer) PrepareResumeWithIdentity(ctx context.Context, repo discovery.Repository, target, oid, slotID, expectedIdentity string) error {
	if err := p.VerifyWorktreeIdentity(target, expectedIdentity); err != nil {
		return fmt.Errorf("validate restoring worktree identity before resume prepare: %w", err)
	}
	if err := p.validateExistingWorktreeOwnedForPhase(ctx, repo, target, oid, slotID, preparePhaseRestore); err != nil {
		return fmt.Errorf("validate restoring worktree before resume prepare: %w", err)
	}
	if err := p.runPrepareWithIdentity(ctx, repo, target, expectedIdentity); err != nil {
		return err
	}
	if err := p.VerifyWorktreeIdentity(target, expectedIdentity); err != nil {
		return fmt.Errorf("wx worktree ownership changed after resume prepare: %w", err)
	}
	if err := p.validateExistingWorktreeOwnedForPhase(ctx, repo, target, oid, slotID, preparePhaseRestore); err != nil {
		return fmt.Errorf("wx worktree ownership changed during resume prepare: %w", err)
	}
	return nil
}

// FinishRestoreWithIdentity は、二つの Git admin command に physical target identity を保持したまま復元済み worktree を READY に遷移させる。
func (p *Preparer) FinishRestoreWithIdentity(ctx context.Context, repo discovery.Repository, target, oid, slotID, expectedIdentity string) error {
	if err := p.validateExistingWorktreeOwnedForPhase(ctx, repo, target, oid, slotID, preparePhaseRestore); err != nil {
		return err
	}
	if expectedIdentity == "" {
		if identity, identityErr := p.WorktreeIdentity(target); identityErr == nil {
			expectedIdentity = identity
		}
	}
	if err := p.VerifyWorktreeIdentity(target, expectedIdentity); err != nil {
		return err
	}
	root, err := config.ExpandHome(p.Config.Storage.WorktreeRoot)
	if err != nil {
		return err
	}
	owner, relativeTarget, closeOwner, err := p.openOwnedRoot(root, filepath.Clean(target))
	if err != nil {
		return fmt.Errorf("open restored worktree namespace: %w", err)
	}
	defer closeOwner()
	if _, err := p.runWorktreeAdminOwned(ctx, repo, owner, relativeTarget, target, expectedIdentity, "unlock"); err != nil {
		return fmt.Errorf("unlock restored worktree: %w", err)
	}
	if _, err := p.runWorktreeAdminOwned(ctx, repo, owner, relativeTarget, target, expectedIdentity, "lock", "--reason", "wx:"+slotID+":READY"); err != nil {
		return err
	}
	if err := p.VerifyWorktreeIdentity(target, expectedIdentity); err != nil {
		return fmt.Errorf("validate restored READY worktree identity: %w", err)
	}
	if err := p.validateExistingWorktreeOwnedForPhase(ctx, repo, target, oid, slotID, preparePhaseRestore); err != nil {
		return fmt.Errorf("validate restored READY worktree: %w", err)
	}
	return nil
}

func (p *Preparer) existingTargetState(ctx context.Context, repo discovery.Repository, target, oid, slotID string, phase preparePhase, root string, ownedRoot *os.Root, relativeTarget string) (bool, error) {
	info, err := ownedRoot.Lstat(relativeTarget)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return false, errors.New("target is not a physical directory")
	}
	entries, err := readOwnedDirectory(ownedRoot, relativeTarget)
	if err != nil {
		return false, err
	}
	if len(entries) == 0 {
		// 中断した allocation は空の shell を残し得る。marker があれば一致が必要で、なければ下で作成する。
		markerRelative, markerErr := ownershipMarkerRelative(root, target, string(repo.ID))
		if markerErr != nil {
			return false, markerErr
		}
		if _, markerErr := ownedRoot.Lstat(markerRelative); markerErr != nil && !errors.Is(markerErr, os.ErrNotExist) {
			return false, markerErr
		}
		return false, nil
	}
	if err := p.validateExistingWorktreeOwnedForPhase(ctx, repo, target, oid, slotID, phase); err != nil {
		return false, fmt.Errorf("non-empty target is not the expected worktree: %w", err)
	}
	return true, nil
}

func readOwnedDirectory(root *os.Root, relative string) ([]string, error) {
	directory, _, err := domain.OpenDirectoryAt(root, relative)
	if err != nil {
		return nil, err
	}
	defer func() { _ = directory.Close() }()
	return directory.Readdirnames(-1)
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

func (p *Preparer) runGitInDirectory(ctx context.Context, directory *os.File, args ...string) (gitx.Result, error) {
	return p.Git.RunAt(ctx, directory, nil, nil, args...)
}

// WorktreeIdentity は configured ownership root 経由で target の device/inode identity を返す。
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

// RunGitInWorktree は WorktreeIdentity で取得した identity の target で Git command を実行する。本番の Preparer は pin 済み root と descriptor cwd を使うため、
// lexical root/target の置換では command を逸らせない。child 実行の前後で identity を検査し、変化は Git の成否にかかわらず ownership-uncertain とする。
// commentlint:allow-long -- command 実行中の path 置換に対する保証を説明する
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
	result, runErr := p.Git.RunAt(ctx, directory, env, input, args...)
	if expectedIdentity != "" {
		if identityErr := p.VerifyWorktreeIdentity(target, expectedIdentity); identityErr != nil {
			return result, fmt.Errorf("worktree target identity changed during Git: %w", identityErr)
		}
	}
	return result, runErr
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
	allOwnershipRepositoryStates = []string{"PREPARING", "PREPARE_RUNNING", "RESTORING", "RESTORE_RUNNING", "READY", "LEASED", "RETIRING"}
)

func preparationOwnershipStates(phase preparePhase) ([]string, []string) {
	if phase == preparePhaseRestore {
		return []string{"RESTORING"}, []string{"RESTORING", "RESTORE_RUNNING"}
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

func (p *Preparer) copyIncludes(repo discovery.Repository, target string) error {
	root, err := config.ExpandHome(p.Config.Storage.WorktreeRoot)
	if err != nil {
		return err
	}
	owner, relativeTarget, closeOwner, err := p.openOwnedRoot(root, filepath.Clean(target))
	if err != nil {
		return err
	}
	defer closeOwner()
	return p.copyIncludesAt(repo, owner, relativeTarget)
}

// defaultIncludeNames は慣例として version control 外に置く agent rule と tool 設定ファイルである。
// Git はこれらを worktree に持ち込まないため、そこで起動した agent は repository 固有の local rule を失う。
// その回避策は home directory から file を取り込むことである。workspace-root materializer は manifest なしで同種の file をコピーする (MaterializeRootAt)。
// repository 側も同じ動作にする。コピー対象は regular file かつ Git が追跡していないものだけである。
// tracked path は worktree 自身が checkout し、directory や symlink は明示的な共有方式なので .worktreeinclude に任せる。
// commentlint:allow-long -- 契約と安全条件を保持する説明のため
var defaultIncludeNames = []string{
	// Claude Code 用
	"CLAUDE.local.md",
	".claudeignore",
	// Codex とその他の AGENTS.md 読み取りツール用
	"AGENTS.local.md",
	"AGENTS.override.md",
	// Gemini CLI 用
	"GEMINI.local.md",
	".geminiignore",
	".aiexclude",
	// Cursor 用
	".cursorrules",
	".cursorignore",
	// Windsurf 用
	".windsurfrules",
	".codeiumignore",
	// Cline、Roo Code、Kilo Code 用
	".clinerules",
	".roorules",
	".kilocoderules",
	// Aider 用
	".aider.conf.yml",
	// 複数の agent が共有する MCP server 用
	".mcp.json",
}

// defaultWorkspaceRootCopyNames は workspace root にある通常の file を multi-repository slot へ materialize する名前である。
// tracked AGENTS.md は意図的に CLAUDE.md への symlink になり得る。Git が repository とともに checkout するため、workspace-root materializer は追従してはならない。
var defaultWorkspaceRootCopyNames = []string{"AGENTS.md", "AGENTS.local.md", "CLAUDE.md", "CLAUDE.local.md"}

func workspaceRootCopyPlan(rules config.Workspace) ([]string, map[string]bool, error) {
	copyNames := append([]string{}, defaultWorkspaceRootCopyNames...)
	copyNames = append(copyNames, rules.Copy...)
	explicit := make(map[string]bool, len(rules.Copy))
	for _, name := range rules.Copy {
		clean, err := safeRelative(name)
		if err != nil {
			return nil, nil, err
		}
		explicit[clean] = true
	}
	return copyNames, explicit, nil
}

// validateWorkspaceRootCopySources は workspace root の copy source を書き込み前に検査する。
// 既定の名前は欠落を許すが、設定で明示した名前は入力漏れとして扱い、slot 準備を成功させない。
func validateWorkspaceRootCopySources(sourceRoot *os.Root, workspaceRoot string, copyNames []string, explicit map[string]bool) (map[string]bool, error) {
	if sourceRoot == nil {
		return nil, errors.New("workspace copy source root is nil")
	}
	present := make(map[string]bool, len(copyNames))
	seen := make(map[string]bool, len(copyNames))
	for _, name := range copyNames {
		clean, err := safeRelative(name)
		if err != nil {
			return nil, err
		}
		if seen[clean] {
			continue
		}
		seen[clean] = true
		info, err := sourceRoot.Lstat(clean)
		if errors.Is(err, os.ErrNotExist) {
			if explicit[clean] {
				path := filepath.Join(workspaceRoot, clean)
				return nil, fmt.Errorf("required workspace copy source %s is missing from workspace root %s: %w", path, workspaceRoot, os.ErrNotExist)
			}
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("inspect workspace copy source %s in workspace root %s: %w", clean, workspaceRoot, err)
		}
		if info.Mode()&os.ModeSymlink != 0 && !explicit[clean] {
			continue
		}
		if _, err := domain.PhysicalPathInfo(sourceRoot, clean); err != nil {
			return nil, fmt.Errorf("workspace copy source %s in workspace root %s is not physical: %w", clean, workspaceRoot, err)
		}
		present[clean] = true
	}
	return present, nil
}

// defaultIncludeCandidates は main worktree に regular physical file として存在する default 名を返す。
// .worktreelink にある名前は明示的な link rule が所有するため除外する。存在しなくても、この一覧は全 repository に適用されるのでエラーにしない。
func defaultIncludeCandidates(mainPath string, c config.Config) ([]string, error) {
	if !c.DefaultAgentRulesEnabled(mainPath) {
		return nil, nil
	}
	linkPatterns, err := readPhysicalPatterns(mainPath, ".worktreelink")
	if err != nil {
		return nil, err
	}
	linked := map[string]bool{}
	for _, pattern := range linkPatterns {
		linked[filepath.Clean(pattern)] = true
	}
	root, err := OpenPhysicalRoot(mainPath)
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()
	out := make([]string, 0, len(defaultIncludeNames))
	for _, name := range defaultIncludeNames {
		clean, err := safeRelative(name)
		if err != nil {
			return nil, err
		}
		if linked[clean] {
			continue
		}
		info, err := domain.PhysicalPathInfo(root, clean)
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		out = append(out, clean)
	}
	return out, nil
}

// defaultIncludes は候補を Git が追跡していない名前に絞る。一度の ls-files で一覧全体を調べ、tracked name は報告せず skip する。
// これにより、repository がこれらの名前で file を commit していても prepare できる。
func (p *Preparer) defaultIncludes(mainPath string) ([]string, error) {
	candidates, err := defaultIncludeCandidates(mainPath, p.Config)
	if err != nil {
		return nil, err
	}
	if len(candidates) == 0 {
		return nil, nil
	}
	listed, err := p.Git.Run(context.Background(), mainPath, append([]string{"ls-files", "-z", "--"}, candidates...)...)
	if err != nil {
		return nil, err
	}
	tracked := map[string]bool{}
	for _, entry := range strings.Split(listed.Stdout, "\x00") {
		if entry == "" {
			continue
		}
		tracked[filepath.Clean(entry)] = true
	}
	out := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		if tracked[candidate] {
			continue
		}
		out = append(out, candidate)
	}
	return out, nil
}

// copyIncludesAt は descriptor-bound な include materializer である。destinationRoot は pin 済み owner namespace から開き、すべての書き込みをその相対 path で行う。
// destination syscall に lexical target pathname は決して使わない。
func (p *Preparer) copyIncludesAt(repo discovery.Repository, owner *os.Root, relativeTarget string) error {
	patterns, err := readPhysicalPatterns(string(repo.MainPath), ".worktreeinclude")
	if err != nil {
		return err
	}
	defaults, err := p.defaultIncludes(string(repo.MainPath))
	if err != nil {
		return err
	}
	destinationRoot, err := domain.OpenRootAt(owner, relativeTarget)
	if err != nil {
		return fmt.Errorf("open include destination: %w", err)
	}
	defer func() { _ = destinationRoot.Close() }()
	// 同じ path の最終内容を明示的な .worktreeinclude entry が決められるよう、default を先に適用する。
	for _, rel := range defaults {
		sourceRoot, sourceErr := OpenPhysicalRoot(string(repo.MainPath))
		if sourceErr != nil {
			return sourceErr
		}
		copyErr := copyPathFromOwnedRoot(sourceRoot, rel, destinationRoot, rel)
		_ = sourceRoot.Close()
		if copyErr != nil {
			return fmt.Errorf("copy default include %s: %w", rel, copyErr)
		}
	}
	for _, pattern := range patterns {
		clean := filepath.Clean(pattern)
		if filepath.IsAbs(pattern) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
			return fmt.Errorf("unsafe .worktreeinclude pattern %q", pattern)
		}
		matches, err := safeGlob(string(repo.MainPath), pattern)
		if err != nil {
			return err
		}
		for _, src := range matches {
			rel, err := filepath.Rel(string(repo.MainPath), src)
			if err != nil {
				return err
			}
			rel, err = safeRelative(rel)
			if err != nil {
				return fmt.Errorf("unsafe .worktreeinclude match %q: %w", src, err)
			}
			// tracked path は worktree 自身が checkout するため、この entry は無視する。
			// default include と同じ扱いにして、file が後から追跡下に入っても manifest の古い行が slot 準備全体を止めないようにする。
			tracked, err := p.Git.Run(context.Background(), string(repo.MainPath), "ls-files", "--error-unmatch", "--", rel)
			if err == nil && strings.TrimSpace(tracked.Stdout) != "" {
				continue
			}
			sourceRoot, sourceErr := OpenPhysicalRoot(string(repo.MainPath))
			if sourceErr != nil {
				return sourceErr
			}
			copyErr := copyPathFromOwnedRoot(sourceRoot, rel, destinationRoot, rel)
			_ = sourceRoot.Close()
			if copyErr != nil {
				return copyErr
			}
		}
	}
	return nil
}

func (p *Preparer) createLinks(ctx context.Context, repo discovery.Repository, target string) error {
	root, err := config.ExpandHome(p.Config.Storage.WorktreeRoot)
	if err != nil {
		return err
	}
	owner, relativeTarget, closeOwner, err := p.openOwnedRoot(root, filepath.Clean(target))
	if err != nil {
		return err
	}
	defer closeOwner()
	return p.createLinksAt(ctx, repo, owner, relativeTarget)
}

// createLinksAt は createLinks と同じ処理を、全検査と symlink 作成の間 destination Root を開いたまま行う。
// これにより worktree の検証から ignored link の書き込みまでに root を置換される隙間を閉じる。
func (p *Preparer) createLinksAt(ctx context.Context, repo discovery.Repository, owner *os.Root, relativeTarget string) error {
	mainPath := string(repo.MainPath)
	sourceRoot, err := openPinnedRepositoryRoot(mainPath)
	if err != nil {
		return err
	}
	defer func() { _ = sourceRoot.Close() }()
	patterns, err := readPhysicalPatternsAt(sourceRoot, ".worktreelink")
	if err != nil {
		return err
	}
	if err := validateRuleConflicts(nil, patterns); err != nil {
		return err
	}
	sources, err := inspectLinkSources(sourceRoot, patterns)
	if err != nil {
		return err
	}
	if err := verifyPinnedRepositoryPath(sourceRoot, mainPath); err != nil {
		return err
	}
	if len(sources) == 0 {
		destinationRoot, err := domain.OpenRootAt(owner, relativeTarget)
		if err != nil {
			return fmt.Errorf("open link destination: %w", err)
		}
		return destinationRoot.Close()
	}
	if !hasPresentLinkSource(sources) {
		return nil
	}
	destinationRoot, err := domain.OpenRootAt(owner, relativeTarget)
	if err != nil {
		return fmt.Errorf("open link destination: %w", err)
	}
	defer func() { _ = destinationRoot.Close() }()
	for _, link := range sources {
		if !link.present {
			continue
		}
		if err := verifyPinnedRepositoryPath(sourceRoot, mainPath); err != nil {
			return err
		}
		current, err := inspectLinkSource(sourceRoot, link.relative)
		if err != nil {
			return err
		}
		if !current.present {
			continue
		}
		if _, err := p.Git.Run(ctx, mainPath, "check-ignore", "-q", "--", link.relative); err != nil {
			return fmt.Errorf(".worktreelink path %q is not ignored", link.relative)
		}
		if err := verifyPinnedRepositoryPath(sourceRoot, mainPath); err != nil {
			return err
		}
		source := filepath.Join(mainPath, link.relative)
		destinationRelative := link.relative
		if err := ensureRootDirectory(destinationRoot, filepath.Dir(destinationRelative)); err != nil {
			return err
		}
		if info, err := destinationRoot.Lstat(destinationRelative); err == nil {
			if info.Mode()&os.ModeSymlink != 0 {
				existing, readErr := destinationRoot.Readlink(destinationRelative)
				if readErr == nil && existing == source {
					continue
				}
			}
			return fmt.Errorf(".worktreelink target collision %s", link.relative)
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err := verifyPinnedRepositoryPath(sourceRoot, mainPath); err != nil {
			return err
		}
		current, err = inspectLinkSource(sourceRoot, link.relative)
		if err != nil {
			return err
		}
		if !current.present {
			continue
		}
		if err := destinationRoot.Symlink(source, destinationRelative); err != nil {
			return err
		}
	}
	return nil
}

type linkSource struct {
	relative string
	present  bool
}

// inspectLinkSources は .worktreelink の source を pin 済み root から検査する。
// os.ErrNotExist は leaf と中間成分のどちらでも欠落として扱い、それ以外の失敗は安全側へ倒して返す。
func inspectLinkSources(sourceRoot *os.Root, patterns []string) ([]linkSource, error) {
	if sourceRoot == nil {
		return nil, errors.New("link source root is nil")
	}
	out := make([]linkSource, 0, len(patterns))
	for _, pattern := range patterns {
		link, err := inspectLinkSource(sourceRoot, pattern)
		if err != nil {
			return nil, err
		}
		out = append(out, link)
	}
	return out, nil
}

func inspectLinkSource(sourceRoot *os.Root, pattern string) (linkSource, error) {
	clean, err := safeRelative(pattern)
	if err != nil {
		return linkSource{}, fmt.Errorf("unsafe .worktreelink path %q", pattern)
	}
	_, err = domain.PhysicalPathInfo(sourceRoot, clean)
	if errors.Is(err, os.ErrNotExist) {
		return linkSource{relative: clean}, nil
	}
	if err != nil {
		return linkSource{}, fmt.Errorf(".worktreelink source %s is not physical: %w", pattern, err)
	}
	return linkSource{relative: clean, present: true}, nil
}

func hasPresentLinkSource(sources []linkSource) bool {
	for _, source := range sources {
		if source.present {
			return true
		}
	}
	return false
}

func (p *Preparer) runPrepareWithIdentity(ctx context.Context, repo discovery.Repository, target, expectedIdentity string) error {
	override, ok := p.Config.Repositories[string(repo.MainPath)]
	if !ok || len(override.Prepare.Command) == 0 {
		return nil
	}
	timeout := override.Prepare.Timeout.Duration
	if timeout <= 0 {
		timeout = p.Config.Readiness.Timeout.Duration
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	root, err := config.ExpandHome(p.Config.Storage.WorktreeRoot)
	if err != nil {
		return fmt.Errorf("open prepare command directory: %w", err)
	}
	owner, relative, closeOwner, err := p.openOwnedRoot(root, target)
	if err != nil {
		return fmt.Errorf("open prepare command directory: %w", err)
	}
	defer closeOwner()
	directory, identity, err := domain.OpenDirectoryAt(owner, relative)
	if err != nil {
		return fmt.Errorf("open prepare command directory: %w", err)
	}
	defer func() { _ = directory.Close() }()
	if expectedIdentity != "" && identity != expectedIdentity {
		return fmt.Errorf("%w: worktree target identity changed before prepare command (expected %s, got %s)", state.ErrOwnership, expectedIdentity, identity)
	}
	cmd, err := fdexec.Start(cctx, p.Git.FDHelper, directory, os.Environ(), override.Prepare.Command...)
	if err != nil {
		return err
	}
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	runErr := cmd.Run()
	if expectedIdentity != "" {
		if identityErr := p.VerifyWorktreeIdentity(target, expectedIdentity); identityErr != nil {
			return fmt.Errorf("worktree target identity changed during prepare command: %w", identityErr)
		}
	}
	return runErr
}

// Fingerprint は prepared worktree を再利用可能にするすべての情報を hash 化する。slot 内の repository directory 名は意図的に含めない。
// slot が存在すれば slot_repositories.dir_name が権威となり、既存 slot は記録済みの名前を保つ。
// 新規 slot だけが変更後の storage.repo_dir_source や repositories.<path>.dir_name を使う。
// 名前を hash 化しても reuse check は保存済みの名前から再計算するため常に自身と一致し、挙動は変わらない。
// schema=4 は .worktreelink の存在状態を含める layout 変更を示すため、以前の wx が書いた fingerprint とは一致しない。
// commentlint:allow-long -- 契約と安全条件を保持する説明のため
func Fingerprint(generation int, oid string, repo discovery.Repository, c config.Config) (string, error) {
	mainPath := string(repo.MainPath)
	sourceRoot, err := openPinnedRepositoryRoot(mainPath)
	if err != nil {
		return "", err
	}
	defer func() { _ = sourceRoot.Close() }()
	h := sha256.New()
	_, _ = fmt.Fprintf(h, "schema=4\ngeneration=%d\noid=%s\n", generation, oid)
	var linkPatterns []string
	for _, name := range []string{".worktreeinclude", ".worktreelink"} {
		data, err := readPhysicalManifestAt(sourceRoot, name)
		if err != nil {
			return "", err
		}
		_, _ = fmt.Fprintf(h, "manifest=%s:%x\n", name, sha256.Sum256(data))
		if name == ".worktreelink" && data != nil {
			linkPatterns, err = parsePhysicalPatterns(data)
			if err != nil {
				return "", err
			}
		}
	}
	if err := validateRuleConflicts(nil, linkPatterns); err != nil {
		return "", err
	}
	linkSources, err := inspectLinkSources(sourceRoot, linkPatterns)
	if err != nil {
		return "", err
	}
	sort.SliceStable(linkSources, func(i, j int) bool {
		return linkSources[i].relative < linkSources[j].relative
	})
	for _, source := range linkSources {
		_, _ = fmt.Fprintf(h, "worktreelink-source=%s:present=%t\n", source.relative, source.present)
	}
	patterns, err := readPhysicalPatternsAt(sourceRoot, ".worktreeinclude")
	if err != nil {
		return "", err
	}
	seenIncludes := map[string]bool{}
	// default include は Fingerprint が Git runner を持たないため、copyIncludesAt の tracked 検査なしで hash 化する。
	// tracked file も checkout に任せるため、main worktree の編集で再利用できた slot も cold start 時に再構築される。untracked file を除外すると古い local rule を持つ slot を渡してしまう。
	// default file がない場合の切り替えで materialized worktree は変わらないため、設定自体は意図的に hash 化しない。
	defaults, err := defaultIncludeCandidates(string(repo.MainPath), c)
	if err != nil {
		return "", err
	}
	for _, rel := range defaults {
		seenIncludes[rel] = true
		if err := fingerprintPath(h, mainPath, filepath.Join(mainPath, rel)); err != nil {
			return "", err
		}
	}
	for _, pattern := range patterns {
		clean := filepath.Clean(pattern)
		if filepath.IsAbs(pattern) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
			return "", fmt.Errorf("unsafe .worktreeinclude pattern %q", pattern)
		}
		matches, err := safeGlob(mainPath, pattern)
		if err != nil {
			return "", err
		}
		for _, match := range matches {
			rel, err := filepath.Rel(mainPath, match)
			if err != nil {
				return "", err
			}
			rel, err = safeRelative(rel)
			if err != nil {
				return "", err
			}
			if seenIncludes[rel] {
				continue
			}
			seenIncludes[rel] = true
			if err := fingerprintPath(h, string(repo.MainPath), match); err != nil {
				return "", err
			}
		}
	}
	workspaceRoot, err := repositoryWorkspaceRoot(repo)
	if err != nil {
		return "", err
	}
	rules := c.Workspaces[workspaceRoot]
	_, _ = fmt.Fprintf(h, "workspace-root=%s\ncopy-rules=%q\nlink-rules=%q\n", workspaceRoot, rules.Copy, rules.Link)
	copyNames, explicitCopies, err := workspaceRootCopyPlan(rules)
	if err != nil {
		return "", err
	}
	workspaceRootHandle, err := OpenPhysicalRoot(workspaceRoot)
	if err != nil {
		return "", err
	}
	defer func() { _ = workspaceRootHandle.Close() }()
	presentCopies, err := validateWorkspaceRootCopySources(workspaceRootHandle, workspaceRoot, copyNames, explicitCopies)
	if err != nil {
		return "", err
	}
	seenCopies := map[string]bool{}
	for _, name := range copyNames {
		clean, err := safeRelative(name)
		if err != nil {
			return "", err
		}
		if seenCopies[clean] {
			continue
		}
		seenCopies[clean] = true
		if !presentCopies[clean] {
			continue
		}
		if err := fingerprintRootPath(h, workspaceRootHandle, clean, clean); err != nil {
			return "", err
		}
	}
	for _, name := range rules.Link {
		clean, err := safeRelative(name)
		if err != nil {
			return "", err
		}
		path := filepath.Join(workspaceRoot, clean)
		if err := domain.ValidatePhysicalPath(path, false); err != nil {
			return "", err
		}
		info, err := os.Lstat(path)
		if err != nil {
			return "", err
		}
		_, _ = fmt.Fprintf(h, "workspace-link=%s:%s\n", clean, info.Mode())
	}
	if o, ok := c.Repositories[string(repo.MainPath)]; ok {
		_, _ = fmt.Fprint(h, o.Prepare.Command, o.Prepare.Version)
	}
	if err := verifyPinnedRepositoryPath(sourceRoot, mainPath); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func repositoryWorkspaceRoot(repo discovery.Repository) (string, error) {
	if repo.RelativePath == "" || filepath.Clean(repo.RelativePath) == "." {
		return string(repo.MainPath), nil
	}
	rel, err := safeRelative(repo.RelativePath)
	if err != nil {
		return "", err
	}
	root := string(repo.MainPath)
	for range strings.Split(rel, string(filepath.Separator)) {
		root = filepath.Dir(root)
	}
	return root, nil
}

func fingerprintPath(h hash.Hash, root, path string) error {
	absoluteRoot, err := filepath.Abs(filepath.Clean(root))
	if err != nil {
		return err
	}
	absolutePath, err := filepath.Abs(filepath.Clean(path))
	if err != nil {
		return err
	}
	rel, err := filepath.Rel(absoluteRoot, absolutePath)
	if err != nil {
		return err
	}
	rel, err = safeRelative(rel)
	if err != nil {
		return err
	}
	rootHandle, err := OpenPhysicalRoot(absoluteRoot)
	if err != nil {
		return err
	}
	defer func() { _ = rootHandle.Close() }()
	return fingerprintRootPath(h, rootHandle, rel, rel)
}

func fingerprintRootPath(h hash.Hash, root *os.Root, relative, display string) error {
	info, err := domain.PhysicalPathInfo(root, relative)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return errors.New("include symlinks are not followed")
	}
	_, _ = fmt.Fprintf(h, "path=%s mode=%s size=%d\n", display, info.Mode(), info.Size())
	if info.IsDir() {
		directory, err := root.OpenFile(relative, os.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
		if err != nil {
			return err
		}
		openedInfo, statErr := directory.Stat()
		if statErr != nil || !openedInfo.IsDir() || !os.SameFile(info, openedInfo) {
			_ = directory.Close()
			return fmt.Errorf("fingerprint directory %s changed while opening", display)
		}
		entries, readErr := directory.Readdirnames(-1)
		closeErr := directory.Close()
		if readErr != nil {
			return readErr
		}
		if closeErr != nil {
			return closeErr
		}
		sort.Strings(entries)
		for _, name := range entries {
			child := filepath.Join(relative, name)
			childDisplay := filepath.Join(display, name)
			if err := fingerprintRootPath(h, root, child, childDisplay); err != nil {
				return err
			}
		}
		return nil
	}
	file, err := root.OpenFile(relative, os.O_RDONLY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	openedInfo, err := file.Stat()
	if err != nil || !openedInfo.Mode().IsRegular() || !os.SameFile(info, openedInfo) {
		_ = file.Close()
		return fmt.Errorf("fingerprint file %s changed while opening", display)
	}
	_, copyErr := io.Copy(h, file)
	closeErr := file.Close()
	if copyErr != nil {
		return copyErr
	}
	return closeErr
}

func MaterializeRoot(source, target string, rules config.Workspace) error {
	var err error
	source, err = filepath.Abs(filepath.Clean(source))
	if err != nil {
		return err
	}
	target, err = filepath.Abs(filepath.Clean(target))
	if err != nil {
		return err
	}
	destinationRoot, err := domain.EnsurePhysicalDirectoryRoot(target, 0o700)
	if err != nil {
		return fmt.Errorf("workspace target is not physical: %w", err)
	}
	defer func() { _ = destinationRoot.Close() }()
	return MaterializeRootAt(source, destinationRoot, rules)
}

// MaterializeRootAt は workspace-level の copy/link rule を pin 済み destination namespace に materialize する。
// daemon が manager-held wx root descriptor から slot root を開いた後、multi-repository slot に対して使う。
func MaterializeRootAt(source string, destinationRoot *os.Root, rules config.Workspace) error {
	if destinationRoot == nil {
		return errors.New("workspace destination root is nil")
	}
	var err error
	source, err = filepath.Abs(filepath.Clean(source))
	if err != nil {
		return err
	}
	if err := domain.ValidatePhysicalPath(source, false); err != nil {
		return fmt.Errorf("workspace source is not physical: %w", err)
	}
	sourceRoot, err := OpenPhysicalRoot(source)
	if err != nil {
		return err
	}
	defer func() { _ = sourceRoot.Close() }()
	copyNames, explicitCopies, err := workspaceRootCopyPlan(rules)
	if err != nil {
		return err
	}
	if err := validateRuleConflicts(copyNames, rules.Link); err != nil {
		return err
	}
	presentCopies, err := validateWorkspaceRootCopySources(sourceRoot, source, copyNames, explicitCopies)
	if err != nil {
		return err
	}
	seenCopies := map[string]bool{}
	for _, rel := range copyNames {
		clean, err := safeRelative(rel)
		if err != nil {
			return err
		}
		if seenCopies[clean] {
			continue
		}
		seenCopies[clean] = true
		if !presentCopies[clean] {
			continue
		}
		if err := copyPathFromOwnedRoot(sourceRoot, clean, destinationRoot, clean); err != nil {
			return fmt.Errorf("copy workspace root path %s: %w", clean, err)
		}
	}
	for _, rel := range rules.Link {
		clean, err := safeRelative(rel)
		if err != nil {
			return err
		}
		src := filepath.Join(source, clean)
		if _, err := domain.PhysicalPathInfo(sourceRoot, clean); err != nil {
			return fmt.Errorf("link workspace root path %s: %w", clean, err)
		}
		if err := domain.ValidatePhysicalPath(src, false); err != nil {
			return fmt.Errorf("workspace link source %s is not physical: %w", clean, err)
		}
		if err := ensureRootDirectory(destinationRoot, filepath.Dir(clean)); err != nil {
			return err
		}
		if info, err := destinationRoot.Lstat(clean); err == nil {
			if info.Mode()&os.ModeSymlink != 0 {
				existing, readErr := destinationRoot.Readlink(clean)
				if readErr == nil && existing == src {
					continue
				}
			}
			return fmt.Errorf("workspace root link collision %s", clean)
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err := destinationRoot.Symlink(src, clean); err != nil {
			return err
		}
	}
	return nil
}

func safeRelative(path string) (string, error) {
	clean := filepath.Clean(path)
	if path == "" || filepath.IsAbs(path) || clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("unsafe workspace root path %q", path)
	}
	return clean, nil
}
