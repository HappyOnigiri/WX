package workspace

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/HappyOnigiri/WX/internal/discovery"
)

// repositoryRules は1つのrepositoryについて解決済みのcopy rule / link ruleである。
// defaults と includes はどちらも main worktree 相対の copy rule だが、
// 「defaultを先に置き、明示した .worktreeinclude entry が同じpathの最終内容を決める」順序を配置側が保てるよう区別する。
type repositoryRules struct {
	defaults []string
	includes []string
	links    []string
}

// copyRules は衝突検査と配置計画が見る copy rule 全体を、配置と同じ順序で返す。
func (r repositoryRules) copyRules() []string {
	out := make([]string, 0, len(r.defaults)+len(r.includes))
	out = append(out, r.defaults...)
	return append(out, r.includes...)
}

// resolveRepositoryRules は .worktreeinclude と .worktreelink を1回ずつだけ読み、両者が同じpathを指す設定矛盾を実体を作る前に拒否する。
// sourceRoot は呼び出し側が開いた main worktree で、同じPREPARE jobでruleを読み直さないために受け取る。
// 読み直すと、その間のrule変更で配置済みの実体と配置履歴が食い違う。
func (p *Preparer) resolveRepositoryRules(repo discovery.Repository, sourceRoot *os.Root) (repositoryRules, error) {
	mainPath := string(repo.MainPath)
	includePatterns, err := readPhysicalPatternsAt(sourceRoot, ".worktreeinclude")
	if err != nil {
		return repositoryRules{}, err
	}
	linkPatterns, err := readPhysicalPatternsAt(sourceRoot, ".worktreelink")
	if err != nil {
		return repositoryRules{}, err
	}
	defaults, err := p.defaultIncludesForRepository(repo, linkPatterns)
	if err != nil {
		return repositoryRules{}, err
	}
	includes, err := expandIncludePatterns(mainPath, includePatterns)
	if err != nil {
		return repositoryRules{}, err
	}
	rules := repositoryRules{defaults: defaults, includes: includes, links: linkPatterns}
	if err := validateRuleConflicts(rules.copyRules(), linkPatterns); err != nil {
		// validateRuleConflicts は workspace root の config 由来 rule とも共有するため manifest 名を持たない。
		// 利用者がどのrepositoryのどのmanifestを直せばよいか読めるよう、ここで名指しする。
		return repositoryRules{}, fmt.Errorf("%s: .worktreeinclude and .worktreelink rules conflict: %w", mainPath, err)
	}
	return rules, nil
}

// expandIncludePatterns は .worktreeinclude の pattern を main worktree 相対の copy rule へ展開する。
func expandIncludePatterns(mainPath string, patterns []string) ([]string, error) {
	out := make([]string, 0, len(patterns))
	for _, pattern := range patterns {
		clean := filepath.Clean(pattern)
		if filepath.IsAbs(pattern) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
			return nil, fmt.Errorf("unsafe .worktreeinclude pattern %q", pattern)
		}
		matches, err := safeGlob(mainPath, pattern)
		if err != nil {
			return nil, err
		}
		for _, match := range matches {
			rel, err := filepath.Rel(mainPath, match)
			if err != nil {
				return nil, err
			}
			rel, err = safeRelative(rel)
			if err != nil {
				return nil, fmt.Errorf("unsafe .worktreeinclude match %q: %w", match, err)
			}
			out = append(out, rel)
		}
	}
	return out, nil
}
