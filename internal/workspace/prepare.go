package workspace

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/HappyOnigiri/WX/internal/config"
	"github.com/HappyOnigiri/WX/internal/discovery"
	"github.com/HappyOnigiri/WX/internal/domain"
	"github.com/HappyOnigiri/WX/internal/gitx"
	"github.com/HappyOnigiri/WX/internal/state"
)

type Preparer struct {
	Git       *gitx.Runner
	Config    config.Config
	Ownership state.OwnershipValidator
	SlotPath  string
	// Log は copy/link source の skip など、準備結果を変えない出来事だけを daemon log へ残す。
	// nil でも準備は同じ結果になり、記録だけが落ちる。
	Log *slog.Logger
	// DetailDir は prepare command の失敗診断を保存する daemon 管理ディレクトリである。
	// 空の場合も command の出力を無制限に保持せず破棄し、診断保存の失敗で準備結果を変えない。
	DetailDir string
	// OwnedRoot は設定済み worktree root 用に daemon が保持する。
	// 設定時は、target の作成と descriptor-bound Git 操作のすべてで、可変なパス名を開き直さずこの inode namespace を使う。
	OwnedRoot *os.Root
	RootPath  string
	// RootID と SlotRelPath は、SQLite が slot の場所を durable root 世代と root 相対パスで記録する値である。
	// 所有権検証は絶対 SlotPath の代わりにこれらを比較し、root の改名や再設定で別 directory が同じ slot に見えることを防ぐ。
	RootID      string
	SlotRelPath string
	// Phases は準備の区間ごとの所要時間を集計する診断用の器である。
	// nil でも準備は同じ結果になり、記録だけが落ちる。`wx bench` がこの内訳を読む。
	Phases *PhaseTimings
	// Notices は成功したまま出力を残した区間を集める診断用の器である。
	// nil でも準備は同じ結果になり、記録だけが落ちる。`wx doctor --probe` がこの記録を読む。
	Notices *PrepareNotices
	// SlotLocks は同じ slot へ書く操作を直列化する共有の lock 表である。
	// prepare が common-directory lock を手放す区間の排他をこれが引き受けるため、daemon は全 Preparer と archive.Manager へ同じ表を渡す。
	SlotLocks  *gitx.KeyedLocks
	noCheckout bool
	// sharedPlaced は共有できる tracked file を checkout の前に clone で置き切ったことを表す。
	// この回は置き換え方式の共有を行わない。置けなかった候補が残る回は、それを共有できる方式が他に無いため省かない。
	sharedPlaced bool
	// cowWorkerCount は CoW 共有の並列度をテストから固定する内部フックである。
	// 0 のままなら cowWorkers が既定値を決める。1 にすると共有順序が index の並び順で決定的になる。
	cowWorkerCount int
}

// logSkip は prepare が copy/link source を使わずに進んだ事実と理由を warn として残す。
// logger を持たない経路（fingerprint 計算やテスト）から呼べるよう nil を許す。
func logSkip(log *slog.Logger, message string, args ...any) {
	if log == nil {
		return
	}
	log.Warn("prepare skipped source: "+message, args...)
}

func (p *Preparer) logSkip(message string, args ...any) {
	logSkip(p.Log, message, args...)
}

// markerIdentity は、repository ごとの slot 用 marker identity を作る。
func (p *Preparer) markerIdentity(repo discovery.Repository, slotID string) MarkerIdentity {
	return MarkerIdentity{SlotID: slotID, RootID: p.RootID, RepositoryID: string(repo.ID)}
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
	preparePhaseUpdate  preparePhase = "update"
)

func (p *Preparer) prepare(ctx context.Context, repo discovery.Repository, target, oid, slotID string, phase preparePhase) error {
	root, target, err := p.prepareTarget(target)
	if err != nil {
		return err
	}
	// slot 排他は最上位で一度だけ取る。以降の common-directory lock は slot lock の内側で取り、逆順にしない。
	slotCtx, releaseSlot, err := p.LockSlot(ctx)
	if err != nil {
		return err
	}
	defer releaseSlot()
	return p.prepareOwned(slotCtx, repo, target, oid, slotID, phase, root)
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

// prepareOwned は slot 排他の下で準備を三つの区間に分ける。
// Git 共有情報を作る区間と READY へ移す区間だけ common-directory lock を保持し、その間のコピー・link・prepare command は保持せずに行う。
// 保持しない区間で同じ slot を触れるのは slot lock を持つこの経路だけなので、区間の境目では所有権を証明し直す。
func (p *Preparer) prepareOwned(ctx context.Context, repo discovery.Repository, target, oid, slotID string, phase preparePhase, root string) (prepareErr error) {
	var locked *lockedTarget
	if err := p.Git.WithCommonDirLock(ctx, string(repo.CommonDir), func(lockCtx context.Context) error {
		var beginErr error
		locked, beginErr = p.beginPrepare(lockCtx, repo, target, oid, slotID, phase, root)
		return beginErr
	}); err != nil {
		return err
	}
	defer locked.close()
	lockedRoot := locked.root
	lockedRelativeTarget := locked.relative
	existingWorktree := locked.existing
	targetIdentity := locked.identity

	cleanup := !existingWorktree
	defer func() {
		if cleanup && !errors.Is(prepareErr, state.ErrOwnership) {
			// 失敗した preparation は所有権を証明できる間だけ削除できる。
			// command や並行する filesystem 変更で証明が無効になった場合は、ロック済み target と marker を quarantine/reconcile 用に残す。
			// Git 登録を消すため common-directory lock を取り直す。要求の context は既に終わっていることがあるので待機は打ち切らない。
			cleanupCtx, releaseCommon, lockErr := p.Git.AcquireCommonDirLock(context.Background(), string(repo.CommonDir))
			if lockErr != nil {
				return
			}
			defer releaseCommon()
			if err := p.validatePreparedTarget(cleanupCtx, repo, target, oid, slotID, phase, lockedRoot, lockedRelativeTarget, targetIdentity, "validate worktree before cleanup"); err != nil {
				return
			}
			if _, err := p.runWorktreeAdminOwned(cleanupCtx, repo, lockedRoot, lockedRelativeTarget, target, targetIdentity, "unlock"); err != nil {
				return
			}
			if _, err := p.runWorktreeAdminOwned(cleanupCtx, repo, lockedRoot, lockedRelativeTarget, target, targetIdentity, "remove", "--force"); err != nil {
				return
			}
			_ = removeOwnershipMarkerAt(lockedRoot, root, target, string(repo.ID))
		}
	}()
	if err := p.completePrepare(ctx, repo, target, oid, slotID, phase, locked,
		func() error { return p.submodulePhase(ctx, repo, target, oid, targetIdentity) },
		func() error { return p.copyIncludesAt(repo, lockedRoot, lockedRelativeTarget) },
		func() error { return p.createLinksAt(ctx, repo, lockedRoot, lockedRelativeTarget, true) }); err != nil {
		return err
	}
	cleanup = false
	return nil
}

// completePrepare は配置後の command・CoW・最終検証を通常準備と二段階準備で共有する。
// submodules は二段階準備では既に済んでいるため、その経路からは何もしない callback を受ける。
func (p *Preparer) completePrepare(ctx context.Context, repo discovery.Repository, target, oid, slotID string, phase preparePhase, locked *lockedTarget, submodules, includes, links func() error) error {
	lockedRoot, lockedRelativeTarget, targetIdentity := locked.root, locked.relative, locked.identity
	if locked.existing {
		if err := p.rejectCOWTemporaries(ctx, target, targetIdentity); err != nil {
			return err
		}
	}
	// worktree に書き込む、または再利用する各操作の直前に durable owner を再検証する。
	// common-directory lock は Git metadata を守り、この read-only な state の証明は slot/path の対応と state machine を独立に守る。
	if err := p.validatePreparedTarget(ctx, repo, target, oid, slotID, phase, lockedRoot, lockedRelativeTarget, targetIdentity, "wx worktree ownership changed before includes"); err != nil {
		return fmt.Errorf("wx worktree ownership changed before includes: %w", err)
	}
	// include・link・prepare command が submodule 配下を前提にできるよう、配置より前に実体化する。
	if err := p.timePhase("submodule", submodules); err != nil {
		return err
	}
	if err := p.timePhase("place", includes); err != nil {
		return err
	}
	if err := p.validatePreparedTarget(ctx, repo, target, oid, slotID, phase, lockedRoot, lockedRelativeTarget, targetIdentity, "wx worktree ownership changed before links"); err != nil {
		return fmt.Errorf("wx worktree ownership changed before links: %w", err)
	}
	// snapshot と現在の main で ignore 規則が異なるため、復元先で symlink 形を無視できる場合だけ link を作る。
	// source 側だけを確認すると、古い `/.tools/` のような directory-only 規則で復元後の tree が変わる。
	if err := p.timePhase("link", links); err != nil {
		return err
	}
	if phase == preparePhaseCreate {
		if err := p.validatePreparedTarget(ctx, repo, target, oid, slotID, phase, lockedRoot, lockedRelativeTarget, targetIdentity, "wx worktree ownership changed before prepare command"); err != nil {
			return fmt.Errorf("wx worktree ownership changed before prepare command: %w", err)
		}
		if err := p.timePhase("prepare-command", func() error { return p.runPrepareWithIdentity(ctx, repo, target, "") }); err != nil {
			return err
		}
		if err := p.verifyPreparedTargetIdentity(lockedRoot, lockedRelativeTarget, targetIdentity); err != nil {
			return fmt.Errorf("wx worktree ownership changed during prepare command: %w", err)
		}
	}
	if phase == preparePhaseCreate {
		if err := p.timePhase("tracked-status", func() error {
			return p.validateTrackedCleanOwned(ctx, target, lockedRoot, lockedRelativeTarget, targetIdentity, "tracked status")
		}); err != nil {
			return err
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
		return nil
	}
	if !p.sharedPlaced {
		if err := p.timePhase("cow", func() error {
			return p.compactWorktree(ctx, repo, target, oid, slotID, phase, targetIdentity, nil)
		}); err != nil {
			return err
		}
	}
	// inode 交換で index の stat cache が陳腐化するため、貸出前に refresh して再ハッシュを PREPARING 側で払う。
	// tracked 内容が変わっていないことの独立検証も兼ねる。先行配置した回も、配置した path 以外の検証はここだけが行う。
	// 配置方式の照合が index を refresh 済みなので、その回のこの status は stat の確認だけで済む。
	if phase == preparePhaseCreate {
		if err := p.timePhase("tracked-status-refresh", func() error {
			return p.validateTrackedCleanOwned(ctx, target, lockedRoot, lockedRelativeTarget, targetIdentity, "tracked status refresh")
		}); err != nil {
			return err
		}
	}
	if err := p.timePhase("ready-lock", func() error {
		return p.Git.WithCommonDirLock(ctx, string(repo.CommonDir), func(lockCtx context.Context) error {
			return p.finishPrepare(lockCtx, repo, target, oid, slotID, phase, lockedRoot, lockedRelativeTarget, targetIdentity)
		})
	}); err != nil {
		return err
	}
	return nil
}

// beginPrepare は common-directory lock を保持する最初の区間である。
// marker と Git 登録を作り、slot の lock reason を立ててから、以降の file 操作が同じ slot に向くことを証明する。
// 失敗時は所有権証明を持てないまま target を残さないよう descriptor を閉じて返し、呼び出し側の cleanup を始めない。
func (p *Preparer) beginPrepare(ctx context.Context, repo discovery.Repository, target, oid, slotID string, phase preparePhase, root string) (*lockedTarget, error) {
	prepared, err := p.prepareLockedTarget(ctx, repo, target, oid, slotID, phase, root)
	if err != nil {
		return nil, err
	}
	begun := false
	defer func() {
		if !begun {
			prepared.close()
		}
	}()
	if prepared.existing {
		if _, err := p.runWorktreeAdminOwned(ctx, repo, prepared.root, prepared.relative, target, prepared.identity, "unlock"); err != nil {
			return nil, fmt.Errorf("unlock existing wx worktree: %w", err)
		}
	}
	lockState := "PREPARING"
	if phase == preparePhaseRestore {
		lockState = "RESTORING"
	}
	if _, err := p.runWorktreeAdminOwned(ctx, repo, prepared.root, prepared.relative, target, prepared.identity, "lock", "--reason", "wx:"+slotID+":"+lockState); err != nil {
		return nil, err
	}
	// この検証は意図的に新しい lock の取得後に行う。
	// file 操作の前に marker、physical path、Git registration、OID、lock reason が同じ slot を示すことを証明する。
	if err := p.validatePreparedTarget(ctx, repo, target, oid, slotID, phase, prepared.root, prepared.relative, prepared.identity, "wx worktree ownership changed after lock"); err != nil {
		return nil, fmt.Errorf("wx worktree ownership changed after lock: %w", err)
	}
	begun = true
	return prepared, nil
}

// finishPrepare は common-directory lock を取り直して worktree を READY へ移す最後の区間である。
// lock を手放している間に slot の実体や DB 上の位置が入れ替わり得るため、Git 管理操作の前後で所有権を証明し直す。
func (p *Preparer) finishPrepare(ctx context.Context, repo discovery.Repository, target, oid, slotID string, phase preparePhase, lockedRoot *os.Root, lockedRelativeTarget, targetIdentity string) error {
	if err := p.validatePreparedTarget(ctx, repo, target, oid, slotID, phase, lockedRoot, lockedRelativeTarget, targetIdentity, "wx worktree ownership changed before READY lock"); err != nil {
		return fmt.Errorf("wx worktree ownership changed before READY lock: %w", err)
	}
	if _, err := p.runWorktreeAdminOwned(ctx, repo, lockedRoot, lockedRelativeTarget, target, targetIdentity, "unlock"); err != nil {
		return err
	}
	if _, err := p.runWorktreeAdminOwned(ctx, repo, lockedRoot, lockedRelativeTarget, target, targetIdentity, "lock", "--reason", "wx:"+slotID+":READY"); err != nil {
		return err
	}
	if err := p.validatePreparedTarget(ctx, repo, target, oid, slotID, phase, lockedRoot, lockedRelativeTarget, targetIdentity, "wx worktree ownership changed before READY"); err != nil {
		return fmt.Errorf("wx worktree ownership changed before READY: %w", err)
	}
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

// PrepareResumeWithIdentity は descriptor-bound resume phase である。
// archive.Manager が clean base 作成後に渡す identity を、resume command と最後の所有権証明まで保持する。
func (p *Preparer) PrepareResumeWithIdentity(ctx context.Context, repo discovery.Repository, target, oid, slotID, expectedIdentity string) error {
	if err := p.VerifyWorktreeIdentity(target, expectedIdentity); err != nil {
		return fmt.Errorf("validate restoring worktree identity before resume prepare: %w", err)
	}
	if err := p.validateExistingWorktreeOwnedForPhase(ctx, repo, target, oid, slotID, preparePhaseRestore); err != nil {
		return fmt.Errorf("validate restoring worktree before resume prepare: %w", err)
	}
	if err := p.rejectCOWTemporaries(ctx, target, expectedIdentity); err != nil {
		return err
	}
	if err := p.runPrepareWithIdentity(ctx, repo, target, expectedIdentity); err != nil {
		return err
	}
	if err := p.compactWorktree(ctx, repo, target, oid, slotID, preparePhaseRestore, expectedIdentity, nil); err != nil {
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
