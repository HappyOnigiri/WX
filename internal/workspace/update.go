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

	"github.com/HappyOnigiri/WX/internal/discovery"
	"github.com/HappyOnigiri/WX/internal/domain"
	"github.com/HappyOnigiri/WX/internal/state"
)

// ErrUpdateIneligible はREADY standbyを要求OIDへ更新できない構造的な条件を示す。
// 呼び出し元はslotを回収して補充へ回し、Cold Startで作り直す合図に使う。
var ErrUpdateIneligible = errors.New("standby cannot be updated to the requested state")

// ValidateUpdateCandidate は既知の更新不能条件をREADY slotの予約前に検査する。
func (p *Preparer) ValidateUpdateCandidate(ctx context.Context, repo discovery.Repository, target, oldOID, newOID string, previous, desired []state.Placement) error {
	if err := p.ValidateReady(ctx, repo, target, oldOID); err != nil {
		return err
	}
	if err := p.rejectChangedGitlinks(ctx, repo, oldOID, newOID); err != nil {
		return err
	}
	if err := p.rejectChangedAttributes(ctx, repo, oldOID, newOID); err != nil {
		return err
	}
	root, err := p.destinationRoot(target)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	if err := validateRecordedPlacements(root, previous); err != nil {
		return fmt.Errorf("%w: %w", ErrUpdateIneligible, err)
	}
	tracked, err := p.gitPaths(ctx, target, "ls-tree", "-r", "--name-only", "-z", newOID)
	if err != nil {
		return err
	}
	untracked, err := p.gitPaths(ctx, target, "ls-files", "--others", "--exclude-standard", "-z")
	if err != nil {
		return err
	}
	ignored, err := p.gitPaths(ctx, target, "ls-files", "--others", "--ignored", "--exclude-standard", "-z")
	if err != nil {
		return err
	}
	for path := range ignored {
		untracked[path] = true
	}
	old := placementPathSet(previous)
	desiredPaths := placementPathSet(desired)
	for path := range untracked {
		if old[path] {
			continue
		}
		if pathsConflictAny(path, tracked) || pathsConflictAny(path, desiredPaths) {
			return fmt.Errorf("%w: untracked or ignored path %s conflicts with standby update", ErrUpdateIneligible, path)
		}
	}
	for path := range desiredPaths {
		if pathsConflictAny(path, tracked) {
			return fmt.Errorf("%w: placement path %s becomes tracked at requested OID", ErrUpdateIneligible, path)
		}
	}
	return nil
}

// rejectChangedAttributes は.gitattributesに差のある更新を不適格として扱う。
// 更新の再展開は`git checkout-index`では済まず、内容が同じでstat cacheの一致するfileだけが旧属性のまま残る。
func (p *Preparer) rejectChangedAttributes(ctx context.Context, repo discovery.Repository, oldOID, newOID string) error {
	diff, err := p.Git.Run(ctx, string(repo.MainPath), "diff", "--name-only", "-z", oldOID, newOID, "--", ".gitattributes", ":(glob)**/.gitattributes")
	if err != nil {
		return err
	}
	if diff.Stdout != "" {
		return fmt.Errorf("%w: .gitattributes changed between the standby and the requested OID", ErrUpdateIneligible)
	}
	return nil
}

func (p *Preparer) rejectChangedGitlinks(ctx context.Context, repo discovery.Repository, oldOID, newOID string) error {
	modules, err := p.Git.Run(ctx, string(repo.MainPath), "diff", "--name-only", "-z", oldOID, newOID, "--", ".gitmodules")
	if err != nil {
		return err
	}
	if modules.Stdout != "" {
		return fmt.Errorf("%w: submodule configuration changed", ErrUpdateIneligible)
	}
	oldLinks, err := p.Git.Run(ctx, string(repo.MainPath), "ls-tree", "-r", oldOID)
	if err != nil {
		return err
	}
	newLinks, err := p.Git.Run(ctx, string(repo.MainPath), "ls-tree", "-r", newOID)
	if err != nil {
		return err
	}
	filter := func(output string) string {
		var lines []string
		for _, line := range strings.Split(output, "\n") {
			if strings.HasPrefix(line, "160000 ") {
				lines = append(lines, line)
			}
		}
		return strings.Join(lines, "\n")
	}
	if filter(oldLinks.Stdout) != filter(newLinks.Stdout) {
		return fmt.Errorf("%w: submodule configuration or gitlink OID changed", ErrUpdateIneligible)
	}
	return nil
}

func (p *Preparer) gitPaths(ctx context.Context, target string, args ...string) (map[string]bool, error) {
	identity, err := p.WorktreeIdentity(target)
	if err != nil {
		return nil, err
	}
	result, err := p.RunGitInWorktree(ctx, target, identity, nil, nil, args...)
	if err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for _, entry := range strings.Split(result.Stdout, "\x00") {
		if entry != "" {
			out[filepath.Clean(entry)] = true
		}
	}
	return out, nil
}

func pathsConflictAny(path string, candidates map[string]bool) bool {
	path = filepath.Clean(path)
	for candidate := range candidates {
		candidate = filepath.Clean(candidate)
		if path == candidate || strings.HasPrefix(path, candidate+string(filepath.Separator)) || strings.HasPrefix(candidate, path+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

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
	identity, err := p.WorktreeIdentity(target)
	if err != nil {
		return nil, err
	}
	if err := p.timePhase("update-validate", func() error {
		return p.validateUpdating(ctx, repo, target, oldOID, slotID, identity)
	}); err != nil {
		return nil, err
	}
	destination, err := p.destinationRoot(target)
	if err != nil {
		return nil, err
	}
	defer func() { _ = destination.Close() }()
	if err := p.timePhase("update-place", func() error {
		if err := validateRecordedPlacements(destination, previous); err != nil {
			return err
		}
		return removeChangedPlacements(destination, previous, desired)
	}); err != nil {
		return nil, err
	}
	if err := p.timePhase("update-checkout", func() error {
		_, err := p.RunGitInWorktree(ctx, target, identity, nil, nil, "-c", "core.hooksPath=/dev/null", "checkout", "--detach", "--force", newOID)
		return err
	}); err != nil {
		return nil, err
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
		return nil, err
	}
	if err := p.timePhase("update-validate", func() error {
		return p.validateUpdating(ctx, repo, target, newOID, slotID, identity)
	}); err != nil {
		return nil, err
	}
	if err := p.timePhase("update-cow", func() error {
		scope, err := p.updateCOWScope(ctx, repo, oldOID, newOID, previous, desired)
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
func (p *Preparer) updateCOWScope(ctx context.Context, repo discovery.Repository, oldOID, newOID string, previous, desired []state.Placement) (*cowScope, error) {
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
	return &cowScope{rewritten: rewritten}, nil
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
	if err := p.validateExistingWorktree(ctx, repo, target, oid); err != nil {
		return err
	}
	if err := p.validateStateOwnershipWithIdentity(ctx, repo, target, slotID, identity, []string{"PREPARING"}, []string{"UPDATE_RUNNING"}); err != nil {
		return err
	}
	if err := p.validateTrackedClean(ctx, target); err != nil {
		return err
	}
	result, err := p.RunGitInWorktree(ctx, target, identity, nil, nil, "symbolic-ref", "-q", "HEAD")
	if err == nil || strings.TrimSpace(result.Stdout) != "" {
		return errors.New("updated worktree is not detached")
	}
	return nil
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
