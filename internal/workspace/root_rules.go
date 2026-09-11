package workspace

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/HappyOnigiri/WX/internal/config"
	"github.com/HappyOnigiri/WX/internal/domain"
)

// defaultRootAgentAssetNames は非 Git workspace root 直下から slot へ持ち込む agent 資産である。
// root には checkout が無いため、ここに並べた名前と設定した rule だけが agent の CWD から読める。
// 実行中に書き換わる settings.local.json や mailbox/ まで入れると内容 hash が動き続けて standby の更新が止まらないため、読み取り資産のサブパスに限る。
// commentlint:allow-long -- 追加してよい名前の条件を保守時に判断できるようにする
var defaultRootAgentAssetNames = []string{
	".claude/skills",
	".claude/agents",
	".claude/commands",
	".claude/hooks",
	".codex/prompts",
}

// RootRules は workspace root の配置規則を解決した結果である。
// Copy は欠落を準備失敗として扱う明示指定、OptionalCopy は欠落を許す既定名と manifest 由来の一覧である。
// 準備・fingerprint・配置履歴・復元は同じ値を見なければ配置履歴と実体が食い違うため、1 つの job では解決を 1 回だけ行って共有する。
type RootRules struct {
	Copy         []string
	OptionalCopy []string
	Link         []string
}

// RootRulesFromConfig は manifest を読まずに config の rule だけを解決する。
// workspace root が repository の main worktree と同じ場合に使う。root 直下の実体は Git が checkout するため、manifest も agent 資産の既定も効かせない。
func RootRulesFromConfig(rules config.Workspace) RootRules {
	return RootRules{Copy: rules.Copy, OptionalCopy: defaultWorkspaceRootCopyNames, Link: rules.Link}
}

// ResolveRootRules は非 Git workspace root の rule を、config と root 直下の manifest から合成する。
// `.worktreeinclude` は copy、`.worktreelink` は link として加算する。include は glob を展開し、0 件マッチを許す。
// 既定名（root 直下の copy 名と agent 資産）は link rule が所有する path を避ける。link した path を copy 予定に残すと rule 衝突で準備が失敗するためである。
// root 自体が無い場合は config の rule だけを返し、ここでは失敗させない。
// commentlint:allow-long -- 合成の入力と、既定を落とす条件を 1 箇所にまとめて説明する
func ResolveRootRules(root string, rules config.Workspace) (RootRules, error) {
	root = filepath.Clean(root)
	resolved := RootRules{Copy: rules.Copy, Link: rules.Link}
	owner, err := OpenPhysicalRoot(root)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			resolved.OptionalCopy = defaultWorkspaceRootCopyNames
			return resolved, nil
		}
		return RootRules{}, err
	}
	defer func() { _ = owner.Close() }()
	linkPatterns, err := readPhysicalPatternsAt(owner, ".worktreelink")
	if err != nil {
		return RootRules{}, fmt.Errorf("read workspace root .worktreelink in %s: %w", root, err)
	}
	for _, pattern := range linkPatterns {
		clean, err := safeRelative(pattern)
		if err != nil {
			return RootRules{}, fmt.Errorf("unsafe workspace root .worktreelink path %q in %s", pattern, root)
		}
		resolved.Link = append(resolved.Link, clean)
	}
	// 既定名は利用者が書いていない暗黙の追加なので、`.worktreelink` の明示に譲って衝突させない。
	// 以降の include の glob 由来は、利用者が copy 側も明示した矛盾なので落とさず衝突検査へ渡す。
	optional := make([]string, 0, len(defaultWorkspaceRootCopyNames)+len(defaultRootAgentAssetNames))
	for _, name := range slices.Concat(defaultWorkspaceRootCopyNames, defaultRootAgentAssetNames) {
		if linkOwnsPath(resolved.Link, name) {
			continue
		}
		optional = append(optional, name)
	}
	includePatterns, err := readPhysicalPatternsAt(owner, ".worktreeinclude")
	if err != nil {
		return RootRules{}, fmt.Errorf("read workspace root .worktreeinclude in %s: %w", root, err)
	}
	for _, pattern := range includePatterns {
		clean := filepath.Clean(pattern)
		if filepath.IsAbs(pattern) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
			return RootRules{}, fmt.Errorf("unsafe workspace root .worktreeinclude pattern %q in %s", pattern, root)
		}
		matches, err := safeGlob(root, pattern)
		if err != nil {
			return RootRules{}, err
		}
		for _, match := range matches {
			relative, err := filepath.Rel(root, match)
			if err != nil {
				return RootRules{}, err
			}
			relative, err = safeRelative(relative)
			if err != nil {
				return RootRules{}, err
			}
			optional = append(optional, relative)
		}
	}
	resolved.OptionalCopy = optional
	if err := validateRuleConflicts(append(append([]string{}, resolved.OptionalCopy...), resolved.Copy...), resolved.Link); err != nil {
		return RootRules{}, fmt.Errorf("workspace root %s rules from config and .worktreeinclude/.worktreelink conflict: %w", root, err)
	}
	return resolved, nil
}

// linkOwnsPath は path が link rule そのものか、その祖先・子孫かを返す。
func linkOwnsPath(links []string, path string) bool {
	clean := filepath.Clean(path)
	for _, link := range links {
		link = filepath.Clean(link)
		if link == clean || domain.IsWithin(link, clean) || domain.IsWithin(clean, link) {
			return true
		}
	}
	return false
}
