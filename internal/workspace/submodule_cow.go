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

// submoduleCOWChild は親 worktree から見た、実体化済み submodule の path である。
// 共有元・宛先の root を子ごとに開き直さず、path を親 root 相対のまま共有機構へ渡す。
type submoduleCOWChild struct {
	path string
}

// compactSubmoduleWorktree は実体化済み submodule の checkout を、親の CoW と同じ置換方式で共有する。
// known が非 nil の場合は、直前の submodule 区間が対象を調べた結果を使い、submodule が無い回の Git 起動を省く。
// known が nil の復元経路では宛先 index から対象を列挙する。
func (p *Preparer) compactSubmoduleWorktree(ctx context.Context, repo discovery.Repository, target, oid, slotID string, phase preparePhase, identity string, known *submodulePhaseResult, checkLeftovers bool) error {
	workspaceRoot := p.workspaceRootForRepository(repo)
	mode := p.Config.CopyModeForWorkspaceRepository(workspaceRoot, repo.RelativePath, string(repo.MainPath))
	ready, err := submoduleCOWPreparationReady(mode, known, func() (bool, error) {
		return p.submodulesEnabled(repo)
	})
	if err != nil {
		return err
	}
	if !ready {
		return nil
	}
	if !submoduleCOWEnabled(mode, cowAvailable()) {
		return p.cowFallback(ctx, mode, target, errors.New("CoW is unavailable on this platform"))
	}
	err = p.compactOwnedSubmoduleWorktree(ctx, repo, target, oid, slotID, phase, identity, checkLeftovers)
	return p.cowFallback(ctx, mode, target, err)
}

func submoduleCOWPreparationReady(mode string, known *submodulePhaseResult, resolve func() (bool, error)) (bool, error) {
	if !submoduleCOWModeEnabled(mode) {
		return false, nil
	}
	if known != nil && (!known.enabled || len(known.declared) == 0) {
		return false, nil
	}
	if known == nil {
		enabled, err := resolve()
		if err != nil {
			return false, err
		}
		if !enabled {
			return false, nil
		}
	}
	return true, nil
}

func submoduleCOWEnabled(mode string, available bool) bool {
	return available && submoduleCOWModeEnabled(mode)
}

func submoduleCOWModeEnabled(mode string) bool {
	return mode != config.CopyModeCopy
}

func (p *Preparer) compactOwnedSubmoduleWorktree(ctx context.Context, repo discovery.Repository, target, oid, slotID string, phase preparePhase, identity string, checkLeftovers bool) error {
	workspaceRoot := p.workspaceRootForRepository(repo)
	owner, relative, _, err := p.openOwnedRoot(p.RootPath, target)
	if err != nil {
		return err
	}
	validate := func() error {
		return p.validatePreparedTarget(ctx, repo, target, oid, slotID, phase, owner, relative, identity, "validate submodule CoW target")
	}
	if err := validate(); err != nil {
		return err
	}
	destination, err := domain.OpenRootAt(owner, relative)
	if err != nil {
		return fmt.Errorf("%w: open submodule CoW target: %w", state.ErrOwnership, err)
	}
	defer func() { _ = destination.Close() }()
	source, err := openPinnedRepositoryRoot(string(repo.MainPath))
	if err != nil {
		return err
	}
	defer func() { _ = source.Close() }()
	directory, _, err := domain.OpenDirectoryAt(owner, relative)
	if err != nil {
		return fmt.Errorf("%w: open submodule CoW Git directory: %w", state.ErrOwnership, err)
	}
	defer func() { _ = directory.Close() }()

	staged, err := p.runGitInDirectory(ctx, directory, "ls-files", "--stage", "-z")
	if err != nil {
		return err
	}
	gitlinkPaths, err := parseCOWGitlinkPaths(staged.Stdout)
	if err != nil {
		return err
	}
	children, err := submoduleCOWChildren(destination, gitlinkPaths)
	if err != nil {
		return err
	}
	if len(children) == 0 {
		return validate()
	}
	if checkLeftovers {
		if err := p.rejectSubmoduleCOWTemporaries(ctx, target, identity, children); err != nil {
			return err
		}
	}

	targetOutput, err := p.submoduleCOWIndex(ctx, directory, children, false)
	if err != nil {
		return err
	}
	parsed, err := parseCOWIndexEntries(targetOutput)
	if err != nil {
		return err
	}
	parsed = filterSubmoduleCOWEntries(parsed, children)
	stats := &cowStats{}
	stats.entries.Store(int64(len(parsed)))
	if len(parsed) == 0 {
		p.logCOWStats(target, stats)
		stats.recordCOWPhases(p.Phases, "submodule-cow")
		return validate()
	}

	sourceDirectory, err := source.Open(".")
	if err != nil {
		return err
	}
	defer func() { _ = sourceDirectory.Close() }()
	sourceOutput, err := p.submoduleCOWIndex(ctx, sourceDirectory, children, true)
	if err != nil {
		return err
	}
	sourceOIDs := filterSubmoduleCOWSourceOIDs(parseCOWSourceIndexOIDs(sourceOutput), children)
	candidates := selectCOWCandidates(parsed, sourceOIDs)
	stats.candidates.Store(int64(len(candidates)))
	slotStates, repoStates := preparationOwnershipStates(phase)
	sharer := &cowSharer{
		source:      source,
		destination: destination,
		proof:       func() error { return p.verifyPreparedTargetIdentity(owner, relative, identity) },
		minSize:     int64(p.Config.COWMinSizeKiBForWorkspaceRepository(workspaceRoot, repo.RelativePath, string(repo.MainPath))) * 1024,
		stats:       stats,
	}
	batches := batchCOWRuns(splitCOWRuns(candidates), cowBatchSize)
	shareErr := runCOWBatches(ctx, p.cowWorkers(), batches, func(ctx context.Context, batch []cowRun) error {
		// 中断後も隔離判断に必要な所有権証明を成立させるため、検査側の context は打ち切らない。
		if err := p.validateStateOwnership(context.WithoutCancel(ctx), repo, target, slotID, slotStates, repoStates); err != nil {
			return err
		}
		if err := sharer.verifyProof(); err != nil {
			return fmt.Errorf("%w: submodule CoW replacement ownership: %w", state.ErrOwnership, err)
		}
		scratch := newCOWScratch()
		for _, run := range batch {
			if err := sharer.shareRun(ctx, scratch, run.directory, run.leaves); err != nil {
				return err
			}
		}
		if err := sharer.verifyProof(); err != nil {
			return fmt.Errorf("%w: submodule CoW cleanup ownership: %w", state.ErrOwnership, err)
		}
		return nil
	})
	p.logCOWStats(target, stats)
	stats.recordCOWPhases(p.Phases, "submodule-cow")
	if shareErr != nil {
		return shareErr
	}
	return validate()
}

// submoduleCOWChildren は gitlink path のうち、実際に checkout 済みの子だけを残す。
// 空の gitlink directoryや、子の .gitdir が作られなかった省略対象は共有しない。
func submoduleCOWChildren(destination *os.Root, paths []string) ([]submoduleCOWChild, error) {
	children := make([]submoduleCOWChild, 0, len(paths))
	for _, path := range paths {
		info, err := domain.PhysicalPathInfo(destination, path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("%w: inspect submodule checkout %s: %w", state.ErrOwnership, path, err)
		}
		if !info.IsDir() {
			return nil, fmt.Errorf("%w: submodule checkout %s is not a directory", state.ErrOwnership, path)
		}
		gitdir, err := domain.PhysicalPathInfo(destination, filepath.Join(path, ".git"))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("%w: inspect submodule gitdir %s: %w", state.ErrOwnership, filepath.Join(path, ".git"), err)
		}
		if gitdir.IsDir() || gitdir.Mode().IsRegular() {
			children = append(children, submoduleCOWChild{path: path})
		}
	}
	return children, nil
}

func (p *Preparer) rejectSubmoduleCOWTemporaries(ctx context.Context, target, identity string, children []submoduleCOWChild) error {
	batches := make([][]submoduleCOWChild, 0, len(children))
	for _, child := range children {
		batches = append(batches, []submoduleCOWChild{child})
	}
	err := runCOWBatches(ctx, p.submoduleWorkers(), batches, func(ctx context.Context, batch []submoduleCOWChild) error {
		child := batch[0]
		args := append([]string{"-C", child.path}, cowLeftoverArgs()...)
		result, err := p.RunGitInWorktree(ctx, target, identity, nil, nil, args...)
		if err != nil {
			return fmt.Errorf("%w: inspect submodule CoW leftovers %s: %w", state.ErrOwnership, child.path, err)
		}
		if err := cowLeftoverResult(result.Stdout); err != nil {
			return fmt.Errorf("%w in submodule %s", err, child.path)
		}
		return nil
	})
	return err
}

// submoduleCOWIndex は active path を一時設定して、親から全子の index をまとめて読む。
// source 側だけ --no-optional-locks を付け、main worktree の index を変更しない。
func (p *Preparer) submoduleCOWIndex(ctx context.Context, directory *os.File, children []submoduleCOWChild, source bool) (string, error) {
	base := []string{"ls-files", "--stage", "-z", "--recurse-submodules", "--"}
	if source {
		base = append([]string{"--no-optional-locks"}, base...)
	}
	var output strings.Builder
	for _, batch := range batchSubmoduleArgs(children, base, func(child submoduleCOWChild) []string {
		return []string{"-c", "submodule.active=:(top,literal)" + child.path, child.path}
	}) {
		args := append([]string(nil), base...)
		for _, child := range batch {
			args = append(args, "-c", "submodule.active=:(top,literal)"+child.path)
		}
		args = append(args, base...)
		for _, child := range batch {
			args = append(args, child.path)
		}
		result, err := p.runGitInDirectory(ctx, directory, args...)
		if err != nil {
			return "", err
		}
		output.WriteString(result.Stdout)
	}
	return output.String(), nil
}

func filterSubmoduleCOWEntries(entries []cowIndexEntry, children []submoduleCOWChild) []cowIndexEntry {
	filtered := make([]cowIndexEntry, 0, len(entries))
	for _, entry := range entries {
		if submoduleCOWPathIncluded(entry.name, children) {
			filtered = append(filtered, entry)
		}
	}
	return filtered
}

func filterSubmoduleCOWSourceOIDs(oids map[string]string, children []submoduleCOWChild) map[string]string {
	filtered := make(map[string]string, len(oids))
	for path, oid := range oids {
		if submoduleCOWPathIncluded(path, children) {
			filtered[path] = oid
		}
	}
	return filtered
}

func submoduleCOWPathIncluded(path string, children []submoduleCOWChild) bool {
	path = filepath.ToSlash(path)
	for _, child := range children {
		prefix := strings.TrimSuffix(filepath.ToSlash(child.path), "/") + "/"
		if path == strings.TrimSuffix(prefix, "/") || strings.HasPrefix(path, prefix) {
			return true
		}
	}
	return false
}
