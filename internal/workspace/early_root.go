package workspace

import (
	"log/slog"
	"os"
	"path/filepath"

	"github.com/HappyOnigiri/WX/internal/state"
)

// RootStagePlan は非 Git workspace root の配置予定と、実際に配置した項目の記録を持つ。
// 配置履歴は Placements から作り、規則を読み直さない。
type RootStagePlan struct {
	log    *slog.Logger
	source string
	plan   earlyPlan
}

// PlanRootStages は非 Git workspace root の通常の配置予定を二分する。
// 明示 copy の必須検査は計画時に行い、追加 early_paths だけでは配置対象を増やさない。
func PlanRootStages(log *slog.Logger, source string, rules RootRules, extra []string) (*RootStagePlan, error) {
	sourceRoot, err := OpenPhysicalRoot(source)
	if err != nil {
		return nil, err
	}
	defer func() { _ = sourceRoot.Close() }()
	names, explicit, err := workspaceRootCopyPlan(rules)
	if err != nil {
		return nil, err
	}
	if err := validateRuleConflicts(names, rules.Link); err != nil {
		return nil, err
	}
	present, err := validateWorkspaceRootCopySources(log, sourceRoot, source, names, explicit)
	if err != nil {
		return nil, err
	}
	staged := &RootStagePlan{log: log, source: source, plan: earlyPlan{sourcePath: source}}
	seen := map[string]bool{}
	for _, name := range names {
		clean := filepath.Clean(name)
		if !present[clean] || seen[clean] {
			continue
		}
		seen[clean] = true
		if err := staged.plan.collectCopies(sourceRoot, clean, nil); err != nil {
			return nil, err
		}
	}
	for _, link := range rules.Link {
		clean, err := safeRelative(link)
		if err != nil {
			return nil, err
		}
		staged.plan.links = append(staged.plan.links, linkSource{relative: clean})
	}
	staged.plan.split(extra)
	return staged, nil
}

// Materialize は計画のうち今回の段階に属する項目を destination へ配置する。
func (s *RootStagePlan) Materialize(destination *os.Root, early bool) error {
	sourceRoot, err := OpenPhysicalRoot(s.source)
	if err != nil {
		return err
	}
	defer func() { _ = sourceRoot.Close() }()
	if err := s.plan.copyAt(sourceRoot, destination, early); err != nil {
		return err
	}
	s.plan.recordCopies(early)
	var links []string
	for _, link := range s.plan.links {
		if s.plan.early[link.relative] == early {
			links = append(links, link.relative)
		}
	}
	created, err := materializeRootLinks(s.log, s.source, sourceRoot, destination, links)
	if err != nil {
		return err
	}
	s.plan.recordLinks(created)
	return nil
}

// Placements は配置し終えた項目を配置履歴の入力として返す。ContentSHA256 は配置先を読む側が埋める。
func (s *RootStagePlan) Placements() []state.Placement {
	return s.plan.placements()
}
