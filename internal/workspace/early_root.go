package workspace

import (
	"log/slog"
	"os"
	"path/filepath"

	"github.com/HappyOnigiri/WX/internal/config"
)

// PlanRootStages は非 Git workspace root の通常の配置予定を二分する。
// 明示 copy の必須検査は計画時に行い、追加 early_paths だけでは配置対象を増やさない。
func PlanRootStages(log *slog.Logger, source string, rules config.Workspace, extra []string) (func(*os.Root, bool) error, error) {
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
	plan := earlyPlan{}
	seen := map[string]bool{}
	for _, name := range names {
		clean := filepath.Clean(name)
		if !present[clean] || seen[clean] {
			continue
		}
		seen[clean] = true
		if err := plan.collectCopies(sourceRoot, clean, nil); err != nil {
			return nil, err
		}
	}
	for _, link := range rules.Link {
		clean, err := safeRelative(link)
		if err != nil {
			return nil, err
		}
		plan.links = append(plan.links, linkSource{relative: clean})
	}
	plan.split(extra)
	return func(destination *os.Root, early bool) error {
		sourceRoot, err := OpenPhysicalRoot(source)
		if err != nil {
			return err
		}
		defer func() { _ = sourceRoot.Close() }()
		if err := plan.copyAt(sourceRoot, destination, early); err != nil {
			return err
		}
		var links []string
		for _, link := range plan.links {
			if plan.early[link.relative] == early {
				links = append(links, link.relative)
			}
		}
		return materializeRootLinks(log, source, sourceRoot, destination, links)
	}, nil
}
