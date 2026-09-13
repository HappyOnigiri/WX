package workspace

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/HappyOnigiri/WX/internal/discovery"
	"github.com/HappyOnigiri/WX/internal/state"
)

// prepareInputChanges は standby 更新で prepare.inputs に一致する変更 path を返す。
// prepare.inputs が空なら、設定していない利用者へ Git diff の追加コストを掛けない。
func (p *Preparer) prepareInputChanges(ctx context.Context, repo discovery.Repository, oldOID, newOID string, previous, desired []state.Placement) ([]string, error) {
	inputs := p.repositoryConfig(repo).Prepare.Inputs
	if len(inputs) == 0 {
		return nil, nil
	}
	diff, err := p.Git.Run(ctx, string(repo.MainPath), "diff", "--name-only", "--no-renames", "-z", oldOID, newOID)
	if err != nil {
		return nil, fmt.Errorf("diff standby prepare inputs: %w", err)
	}
	changed := make(map[string]struct{})
	for _, path := range strings.Split(diff.Stdout, "\x00") {
		if path != "" {
			changed[filepath.Clean(path)] = struct{}{}
		}
	}
	for _, path := range changedPlacementPaths(previous, desired) {
		changed[path] = struct{}{}
	}

	matched := make([]string, 0, len(changed))
	for path := range changed {
		matches, matchErr := matchesPrepareInput(inputs, path)
		if matchErr != nil {
			return nil, matchErr
		}
		if matches {
			matched = append(matched, path)
		}
	}
	sort.Strings(matched)
	return matched, nil
}

func changedPlacementPaths(previous, desired []state.Placement) []string {
	old := make(map[string]state.Placement, len(previous))
	for _, placement := range previous {
		old[placementKey(placement)] = placement
	}
	next := make(map[string]state.Placement, len(desired))
	for _, placement := range desired {
		next[placementKey(placement)] = placement
	}
	changed := make(map[string]struct{})
	for key, placement := range old {
		current, ok := next[key]
		if !ok || !placement.SameSource(current) {
			if path := filepath.Clean(placement.RelativePath); path != "." {
				changed[path] = struct{}{}
			}
		}
	}
	for key, placement := range next {
		prior, ok := old[key]
		if !ok || !prior.SameSource(placement) {
			if path := filepath.Clean(placement.RelativePath); path != "." {
				changed[path] = struct{}{}
			}
		}
	}
	out := make([]string, 0, len(changed))
	for path := range changed {
		out = append(out, path)
	}
	sort.Strings(out)
	return out
}

// matchesPrepareInput は path 自身とすべての祖先 path を pattern と照合する。
// そのため `config` は `config/db.yml` の変更を、`config/*` は直下の配置を検出できる。
func matchesPrepareInput(patterns []string, path string) (bool, error) {
	candidate := filepath.Clean(path)
	for candidate != "." && candidate != "" && candidate != string(filepath.Separator) {
		for _, pattern := range patterns {
			matched, err := matchPreparePathPattern(pattern, candidate)
			if err != nil {
				return false, fmt.Errorf("invalid prepare.inputs pattern %q: %w", pattern, err)
			}
			if matched {
				return true, nil
			}
		}
		parent := filepath.Dir(candidate)
		if parent == candidate {
			break
		}
		candidate = parent
	}
	return false, nil
}

func matchPreparePathPattern(pattern, path string) (bool, error) {
	patternParts := strings.Split(filepath.Clean(pattern), string(filepath.Separator))
	pathParts := strings.Split(filepath.Clean(path), string(filepath.Separator))
	if len(patternParts) != len(pathParts) {
		return false, nil
	}
	for index := range patternParts {
		matched, err := filepath.Match(patternParts[index], pathParts[index])
		if err != nil {
			return false, err
		}
		if !matched {
			return false, nil
		}
	}
	return true, nil
}
