package workspace

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"

	"github.com/HappyOnigiri/WorktreeX/internal/discovery"
	"github.com/HappyOnigiri/WorktreeX/internal/domain"
	"github.com/HappyOnigiri/WorktreeX/internal/state"
)

// ErrUpdateIneligible はREADY standbyを要求OIDへ更新できない構造的な条件を示す。
// 呼び出し元はslotを回収して補充へ回し、Cold Startで作り直す合図に使う。
var ErrUpdateIneligible = errors.New("standby cannot be updated to the requested state")

func placementPathSet(placements []state.Placement) map[string]bool {
	out := make(map[string]bool, len(placements))
	for _, placement := range placements {
		out[filepath.Clean(placement.RelativePath)] = true
	}
	return out
}

func (p *Preparer) destinationRoot(target string) (*os.Root, error) {
	root, target, err := p.prepareTarget(target)
	if err != nil {
		return nil, err
	}
	owner, relative, closeOwner, err := p.openOwnedRoot(root, target)
	if err != nil {
		return nil, err
	}
	destination, err := domain.OpenRootAt(owner, relative)
	closeOwner()
	return destination, err
}

// UpdateLocked はmulti-repository全体でslot lockを保持する呼び出し元向けの更新処理である。
// 区間名に update- 接頭辞を付けるのは、cold startの同名区間と合算されないようにするためである。
func (p *Preparer) UpdateLocked(ctx context.Context, repo discovery.Repository, target, oldOID, newOID, slotID string, previous, desired []state.Placement) ([]state.Placement, error) {
	stage, err := p.UpdateCheckoutLocked(ctx, repo, target, oldOID, newOID, slotID, previous, desired)
	if err != nil {
		return nil, err
	}
	return p.UpdateFinishLocked(ctx, repo, target, oldOID, newOID, slotID, previous, stage)
}

// StandbyUpdateStage は更新の前半を終えたrepository 1件の状態で、後半へそのまま渡す。
type StandbyUpdateStage struct {
	identity string
	desired  []state.Placement
}

// Placements は前半で実際に置いた配置で、更新先OIDのignore規則で見送ったlinkを含まない。
func (s StandbyUpdateStage) Placements() []state.Placement {
	return s.desired
}

// UpdateCheckoutLocked は更新の前半で、旧HEADの検証から要求OIDへのcheckoutと配置までを行う。
// 前半を終えたworktreeはエージェントの起動に必要なfileが揃っており、呼び出し側はここでEarly Readyを出してよい。
// prepare commandの再実行・CoW・最終の検証はUpdateFinishLockedが行う。
func (p *Preparer) UpdateCheckoutLocked(ctx context.Context, repo discovery.Repository, target, oldOID, newOID, slotID string, previous, desired []state.Placement) (StandbyUpdateStage, error) {
	identity, err := p.WorktreeIdentity(target)
	if err != nil {
		return StandbyUpdateStage{}, err
	}
	if err := p.timePhase("update-validate", func() error {
		return p.validateUpdating(ctx, repo, target, oldOID, slotID, identity)
	}); err != nil {
		return StandbyUpdateStage{}, err
	}
	destination, err := p.destinationRoot(target)
	if err != nil {
		return StandbyUpdateStage{}, err
	}
	defer func() { _ = destination.Close() }()
	if err := p.timePhase("update-place", func() error {
		if err := validateRecordedPlacements(destination, previous); err != nil {
			return err
		}
		return removeChangedPlacements(destination, previous, desired)
	}); err != nil {
		return StandbyUpdateStage{}, err
	}
	if err := p.timePhase("update-checkout", func() error {
		return p.checkoutUpdate(ctx, repo, target, identity, oldOID, newOID, destination)
	}); err != nil {
		return StandbyUpdateStage{}, err
	}
	retainedPrevious := unchangedPlacements(previous, desired)
	if err := p.timePhase("update-place", func() error {
		desired, err = p.filterUpdateLinks(ctx, destination, desired)
		if err != nil {
			return err
		}
		if err := removeChangedPlacements(destination, retainedPrevious, desired); err != nil {
			return err
		}
		return materializeChangedPlacements(destination, previous, desired)
	}); err != nil {
		return StandbyUpdateStage{}, err
	}
	return StandbyUpdateStage{identity: identity, desired: desired}, nil
}

// UpdateFinishLocked は更新の後半で、入力の変わったprepare commandの再実行・CoW・最終の検証を行う。
// Early Readyの後に走っても、エージェントは最初のプロンプトでFull Readyを待つため、作業と重ならない。
func (p *Preparer) UpdateFinishLocked(ctx context.Context, repo discovery.Repository, target, oldOID, newOID, slotID string, previous []state.Placement, stage StandbyUpdateStage) ([]state.Placement, error) {
	identity, desired := stage.identity, stage.desired
	changedInputs, err := p.prepareInputChanges(ctx, repo, oldOID, newOID, previous, desired)
	if err != nil {
		return nil, err
	}
	if len(changedInputs) > 0 {
		if p.Log != nil {
			for _, path := range changedInputs {
				p.Log.Info("standby update rerunning prepare command", "repository", string(repo.MainPath), "input_path", path, "old_oid", oldOID, "new_oid", newOID)
			}
		}
		if err := p.timePhase("update-prepare-command", func() error {
			return p.runPrepareWithIdentity(ctx, repo, target, identity)
		}); err != nil {
			return nil, err
		}
	}
	// CoWの直前の検証は、共有しない回は最終の検証と完全に重なるので省く。
	// 共有する回はcompactionの事前検証がtracked clean以外を重ねて確かめるが、
	// tracked cleanは最終の検証が共有後に確かめるので、ここでは所有権だけを見る。
	// prepare commandを再実行した回だけは、その汚れを共有の前に止めるため完全な検証を残す。
	// commentlint:allow-long -- 3通りの省略の根拠はどれも検証を削る判断の安全性に要る
	rerunPrepare := len(changedInputs) > 0
	if rerunPrepare || p.compactsWorktree(repo) {
		if err := p.timePhase("update-validate", func() error {
			if rerunPrepare {
				return p.validateUpdating(ctx, repo, target, newOID, slotID, identity)
			}
			return p.validateUpdatingOwnership(ctx, repo, target, newOID, slotID, identity)
		}); err != nil {
			return nil, err
		}
	}
	if err := p.timePhase("update-cow", func() error {
		scope, err := p.updateCOWScope(ctx, repo, oldOID, newOID, previous, desired, rerunPrepare)
		if err != nil {
			return err
		}
		return p.compactWorktree(ctx, repo, target, newOID, slotID, preparePhaseUpdate, identity, scope)
	}); err != nil {
		return nil, err
	}
	if err := p.timePhase("update-validate", func() error {
		return p.validateUpdating(ctx, repo, target, newOID, slotID, identity)
	}); err != nil {
		return nil, err
	}
	return desired, nil
}

// updateCOWScope は今回の更新が宛先へ書き直した path の集合を返す。
// `checkout --detach --force` は旧OIDとの差分しか書かず、配置の更新は previous と desired に挙がった path だけを触る。
// 集合の外は前回の準備が残した実体のままなので、候補から外しても宛先のbytesは変わらず、共有済みなら共有が続く。
// 逆に前回共有できなかったpathを更新で共有し直すことは諦める。共有の水準は準備時に決まり、更新では増えない。
// commentlint:allow-long -- 候補限定の根拠（bytesが変わらないこと）と代償（共有が増えないこと）はどちらも保守に要る
// rerunPrepareはprepare commandを再実行したかで、再実行した回は集合の外にも一時ファイル名が作られ得るため、残骸の探索を全体へ戻す。
func (p *Preparer) updateCOWScope(ctx context.Context, repo discovery.Repository, oldOID, newOID string, previous, desired []state.Placement, rerunPrepare bool) (*cowScope, error) {
	// rename検出は報告を減らす方向にしか働かない（旧名が落ちる）ため切る。集合は多めに見積もる側へ倒す。
	diff, err := p.Git.Run(ctx, string(repo.MainPath), "diff", "--name-only", "--no-renames", "-z", oldOID, newOID)
	if err != nil {
		return nil, err
	}
	rewritten := map[string]bool{}
	for _, name := range strings.Split(diff.Stdout, "\x00") {
		if name != "" {
			rewritten[filepath.Clean(name)] = true
		}
	}
	// 配置はignore対象に限るのでindexのentryとしては現れないが、集合の定義を「実際に触ったpath」へ揃えておく。
	for _, placements := range [][]state.Placement{previous, desired} {
		for _, placement := range placements {
			rewritten[filepath.Clean(placement.RelativePath)] = true
		}
	}
	return &cowScope{rewritten: rewritten, scopedLeftovers: !rerunPrepare}, nil
}

func unchangedPlacements(previous, desired []state.Placement) []state.Placement {
	wanted := make(map[string]state.Placement, len(desired))
	for _, placement := range desired {
		wanted[placementKey(placement)] = placement
	}
	var out []state.Placement
	for _, placement := range previous {
		if next, ok := wanted[placementKey(placement)]; ok && placement.SameSource(next) {
			out = append(out, placement)
		}
	}
	return out
}

// filterUpdateLinks は更新先OIDのignore規則を適用し、配置履歴へ残すlinkを確定する。
func (p *Preparer) filterUpdateLinks(ctx context.Context, root *os.Root, desired []state.Placement) ([]state.Placement, error) {
	directory, err := root.Open(".")
	if err != nil {
		return nil, err
	}
	defer directory.Close()
	out := make([]state.Placement, 0, len(desired))
	for _, placement := range desired {
		if placement.Kind == "link" {
			ignored, err := checkIgnoredAt(ctx, p.Git, directory, placement.RelativePath)
			if err != nil {
				return nil, err
			}
			if !ignored {
				continue
			}
		}
		out = append(out, placement)
	}
	return out, nil
}

func (p *Preparer) validateUpdating(ctx context.Context, repo discovery.Repository, target, oid, slotID, identity string) error {
	if err := p.validateUpdatingOwnership(ctx, repo, target, oid, slotID, identity); err != nil {
		return err
	}
	return p.validateTrackedClean(ctx, target)
}

// validateUpdatingOwnership は更新中のworktreeの所有権と、HEADが要求OIDのdetachedであることを確かめる。
// detachedの確認はvalidateExistingWorktreeが`symbolic-ref`で行うので、ここで重ねない。
func (p *Preparer) validateUpdatingOwnership(ctx context.Context, repo discovery.Repository, target, oid, slotID, identity string) error {
	if err := p.validateExistingWorktree(ctx, repo, target, oid); err != nil {
		return err
	}
	return p.validateStateOwnershipWithIdentity(ctx, repo, target, slotID, identity, []string{"PREPARING"}, []string{"UPDATE_RUNNING"})
}

func validateRecordedPlacements(root *os.Root, placements []state.Placement) error {
	for _, placement := range placements {
		info, err := root.Lstat(placement.RelativePath)
		if err != nil {
			return fmt.Errorf("recorded placement %s is missing: %w", placement.RelativePath, err)
		}
		switch placement.Kind {
		case "link":
			if info.Mode()&os.ModeSymlink == 0 {
				return fmt.Errorf("recorded link %s changed shape", placement.RelativePath)
			}
			target, err := root.Readlink(placement.RelativePath)
			if err != nil || target != placement.SourcePath {
				return fmt.Errorf("recorded link %s changed target", placement.RelativePath)
			}
		case "copy":
			if !info.Mode().IsRegular() {
				return fmt.Errorf("recorded copy %s changed shape", placement.RelativePath)
			}
			hash, err := hashRootFile(root, placement.RelativePath)
			if err != nil || hash != placement.ContentSHA256 {
				return fmt.Errorf("recorded copy %s changed content", placement.RelativePath)
			}
		default:
			return fmt.Errorf("unknown placement kind %q", placement.Kind)
		}
	}
	return nil
}

func removeChangedPlacements(root *os.Root, previous, desired []state.Placement) error {
	wanted := map[string]state.Placement{}
	for _, placement := range desired {
		wanted[placementKey(placement)] = placement
	}
	for _, old := range previous {
		if next, ok := wanted[placementKey(old)]; ok && old.SameSource(next) {
			continue
		}
		if err := root.Remove(old.RelativePath); err != nil {
			return fmt.Errorf("remove obsolete placement %s: %w", old.RelativePath, err)
		}
	}
	var directories []string
	seen := map[string]bool{}
	for _, old := range previous {
		for directory := filepath.Dir(old.RelativePath); directory != "." && directory != string(filepath.Separator); directory = filepath.Dir(directory) {
			if !seen[directory] {
				seen[directory] = true
				directories = append(directories, directory)
			}
		}
	}
	sort.Slice(directories, func(i, j int) bool { return len(directories[i]) > len(directories[j]) })
	for _, directory := range directories {
		if err := root.Remove(directory); err != nil && !errors.Is(err, os.ErrNotExist) && !errors.Is(err, syscall.ENOTEMPTY) {
			return fmt.Errorf("remove empty placement directory %s: %w", directory, err)
		}
	}
	return nil
}

func materializeChangedPlacements(root *os.Root, previous, desired []state.Placement) error {
	old := map[string]state.Placement{}
	for _, placement := range previous {
		old[placementKey(placement)] = placement
	}
	for _, placement := range desired {
		if prior, ok := old[placementKey(placement)]; ok && prior.SameSource(placement) {
			if placement.Kind == "copy" {
				if err := syncRootCopyMode(root, placement); err != nil {
					return err
				}
			}
			continue
		}
		if err := ensureRootDirectory(root, filepath.Dir(placement.RelativePath)); err != nil {
			return err
		}
		switch placement.Kind {
		case "link":
			if err := domain.ValidatePhysicalLeaf(placement.SourcePath); err != nil {
				return err
			}
			if err := root.Symlink(placement.SourcePath, placement.RelativePath); err != nil {
				return err
			}
		case "copy":
			sourceRoot, err := OpenPhysicalRoot(filepath.Dir(placement.SourcePath))
			if err != nil {
				return err
			}
			name := filepath.Base(placement.SourcePath)
			hash, hashErr := hashRootFile(sourceRoot, name)
			if hashErr != nil || hash != placement.ContentSHA256 {
				_ = sourceRoot.Close()
				return fmt.Errorf("copy source %s changed after update reservation", placement.SourcePath)
			}
			copyErr := copyPathFromOwnedRoot(sourceRoot, name, root, placement.RelativePath)
			_ = sourceRoot.Close()
			if copyErr != nil {
				return copyErr
			}
		default:
			return fmt.Errorf("unknown placement kind %q", placement.Kind)
		}
	}
	return validateRecordedPlacements(root, desired)
}

// syncRootCopyMode は内容と source path が同じ copy でも、source の permission mode が
// 変わっていれば既存 destination へ反映する。OpenFile の作成 mode は既存 file に効かない
// ため、同一 placement を再利用する更新経路だけ明示的に同期する。
func syncRootCopyMode(destination *os.Root, placement state.Placement) error {
	sourceRoot, err := OpenPhysicalRoot(filepath.Dir(placement.SourcePath))
	if err != nil {
		return err
	}
	defer func() { _ = sourceRoot.Close() }()
	sourceName := filepath.Base(placement.SourcePath)
	sourceInfo, err := sourceRoot.Lstat(sourceName)
	if err != nil {
		return fmt.Errorf("inspect root copy source %s: %w", placement.SourcePath, err)
	}
	if sourceInfo.Mode()&os.ModeSymlink != 0 || !sourceInfo.Mode().IsRegular() {
		return fmt.Errorf("root copy source %s is not a regular file", placement.SourcePath)
	}
	sourceFile, err := sourceRoot.OpenFile(sourceName, os.O_RDONLY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return fmt.Errorf("open root copy source %s: %w", placement.SourcePath, err)
	}
	openedInfo, statErr := sourceFile.Stat()
	closeErr := sourceFile.Close()
	if statErr != nil {
		return fmt.Errorf("stat root copy source %s: %w", placement.SourcePath, statErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close root copy source %s: %w", placement.SourcePath, closeErr)
	}
	if !openedInfo.Mode().IsRegular() || !os.SameFile(sourceInfo, openedInfo) {
		return fmt.Errorf("root copy source %s changed while opening", placement.SourcePath)
	}

	destinationInfo, err := destination.Lstat(placement.RelativePath)
	if err != nil {
		return fmt.Errorf("inspect materialized root copy %s: %w", placement.RelativePath, err)
	}
	if destinationInfo.Mode()&os.ModeSymlink != 0 || !destinationInfo.Mode().IsRegular() {
		return fmt.Errorf("materialized root copy %s is not a regular file", placement.RelativePath)
	}
	wantMode := sourceInfo.Mode().Perm()
	if destinationInfo.Mode().Perm() == wantMode {
		return nil
	}
	destinationFile, err := destination.OpenFile(placement.RelativePath, os.O_RDONLY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return fmt.Errorf("open materialized root copy %s: %w", placement.RelativePath, err)
	}
	defer func() { _ = destinationFile.Close() }()
	if err := destinationFile.Chmod(wantMode); err != nil {
		return fmt.Errorf("update mode of materialized root copy %s: %w", placement.RelativePath, err)
	}
	return nil
}

// ValidateAndSyncRootPlacements は記録済みのworkspace root配置だけを更新する。
func ValidateAndSyncRootPlacements(destination *os.Root, previous, desired []state.Placement) error {
	if destination == nil {
		return errors.New("workspace root destination is nil")
	}
	if err := ValidateRootPlacements(destination, previous, desired); err != nil {
		return err
	}
	if err := removeChangedPlacements(destination, previous, desired); err != nil {
		return err
	}
	return materializeChangedPlacements(destination, previous, desired)
}

// ValidateRootPlacements はstandby slotを書き換えずにworkspace rootのcopy/link変更を検査する。
func ValidateRootPlacements(destination *os.Root, previous, desired []state.Placement) error {
	if destination == nil {
		return errors.New("workspace root destination is nil")
	}
	if err := validateRecordedPlacements(destination, previous); err != nil {
		return err
	}
	for _, placement := range desired {
		if info, err := destination.Lstat(placement.RelativePath); err == nil {
			found := false
			for _, old := range previous {
				if placementKey(old) == placementKey(placement) {
					found = true
					break
				}
			}
			oldDescendant := false
			for _, old := range previous {
				if strings.HasPrefix(filepath.Clean(old.RelativePath), filepath.Clean(placement.RelativePath)+string(filepath.Separator)) {
					oldDescendant = true
					break
				}
			}
			covered := false
			if info.IsDir() && oldDescendant {
				covered, err = directoryCoveredByPlacements(destination, placement.RelativePath, previous)
				if err != nil {
					return err
				}
			}
			if !found && !covered {
				return fmt.Errorf("%w: workspace placement collision %s", ErrUpdateIneligible, placement.RelativePath)
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

func directoryCoveredByPlacements(root *os.Root, relative string, placements []state.Placement) (bool, error) {
	recorded := placementPathSet(placements)
	covered := true
	err := fs.WalkDir(root.FS(), filepath.ToSlash(relative), func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == filepath.ToSlash(relative) || entry.IsDir() {
			return nil
		}
		if !recorded[filepath.Clean(filepath.FromSlash(path))] {
			covered = false
			return fs.SkipAll
		}
		return nil
	})
	return covered, err
}
