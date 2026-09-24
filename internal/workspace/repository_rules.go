package workspace

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/HappyOnigiri/WorktreeX/internal/discovery"
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
	// 展開は rule を読んだ直後に 1 回だけ行い、以降の既定 include の譲り判定・衝突検査・配置へ同じ集合を流す。
	links, err := expandLinkPatternsAt(sourceRoot, linkPatterns)
	if err != nil {
		return repositoryRules{}, err
	}
	defaults, err := p.defaultIncludesForRepository(repo, links)
	if err != nil {
		return repositoryRules{}, err
	}
	includes, err := expandIncludePatterns(mainPath, includePatterns)
	if err != nil {
		return repositoryRules{}, err
	}
	rules := repositoryRules{defaults: defaults, includes: includes, links: links}
	if err := validateRuleConflicts(rules.copyRules(), links); err != nil {
		// validateRuleConflicts は workspace root の config 由来 rule とも共有するため manifest 名を持たない。
		// 利用者がどのrepositoryのどのmanifestを直せばよいか読めるよう、ここで名指しする。
		return repositoryRules{}, fmt.Errorf("%s: .worktreeinclude and .worktreelink rules conflict: %w", mainPath, err)
	}
	return rules, nil
}

// expandIncludePatterns は .worktreeinclude の pattern を main worktree 相対の copy rule へ展開する。
func expandIncludePatterns(mainPath string, patterns []string) ([]string, error) {
	owner, err := OpenPhysicalRoot(filepath.Clean(mainPath))
	if err != nil {
		return nil, err
	}
	defer func() { _ = owner.Close() }()
	return expandPatternsAt(owner, ".worktreeinclude", patterns)
}

// expandPatternsAt は manifest の pattern を root 相対 path へ glob 展開する。
// 安全検査は glob より先に行い、root の外を指す pattern が filesystem に触れる前に落とす。
// kind は失敗を利用者が直せるよう、名指しする manifest 名である。
func expandPatternsAt(root *os.Root, kind string, patterns []string) ([]string, error) {
	out := make([]string, 0, len(patterns))
	for _, pattern := range patterns {
		clean := filepath.Clean(pattern)
		if filepath.IsAbs(pattern) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
			return nil, fmt.Errorf("unsafe %s pattern %q", kind, pattern)
		}
		matches, err := safeGlobAt(root, pattern)
		if err != nil {
			return nil, err
		}
		for _, match := range matches {
			rel, err := safeRelative(match)
			if err != nil {
				return nil, fmt.Errorf("unsafe %s match %q: %w", kind, match, err)
			}
			out = append(out, rel)
		}
	}
	return out, nil
}

// expandLinkPatternsAt は .worktreelink の行を配置対象の relative path へ解決する。
// メタ文字 (*?[) を含まない行は filesystem に触れず素通しし、欠落時の挙動（repository は skip、root は準備失敗）を維持する。
// メタ文字を含む行だけ glob 展開し、.worktreeinclude と同じく 0 件マッチを許す。
// 配置ループには重複防御が無いため、ここで初出だけを残す。
// commentlint:allow-long -- literal と glob で契約が非対称な理由を保守時に読めるようにする
func expandLinkPatternsAt(root *os.Root, patterns []string) ([]string, error) {
	out := make([]string, 0, len(patterns))
	seen := map[string]bool{}
	appendPath := func(path string) {
		if seen[path] {
			return
		}
		seen[path] = true
		out = append(out, path)
	}
	for _, pattern := range patterns {
		if !strings.ContainsAny(pattern, "*?[") {
			clean, err := safeRelative(pattern)
			if err != nil {
				return nil, fmt.Errorf("unsafe .worktreelink path %q", pattern)
			}
			appendPath(clean)
			continue
		}
		matches, err := expandPatternsAt(root, ".worktreelink", []string{pattern})
		if err != nil {
			return nil, err
		}
		for _, match := range matches {
			appendPath(match)
		}
	}
	return out, nil
}
