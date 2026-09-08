package workspace

import (
	"testing"
)

func TestEarlyPlanSelectsOnlyPlannedPathsAndSymlinkClosure(t *testing.T) {
	paths := append(append([]string{}, defaultIncludeNames...), "AGENTS.md", ".claude/settings.json", ".codex/skills/x/SKILL.md", ".github/agents/review.md", "src/AGENTS.md", "config/custom.md", "internal/rules.md", "internal/next.md", "nested/.gitignore")
	plan := earlyPlan{tracked: paths, symlinks: map[string]string{"AGENTS.md": "internal/rules.md", "internal/rules.md": "next.md"}}
	plan.split([]string{"config", "missing"})
	for _, path := range paths {
		if plan.early[path] != (path != "src/AGENTS.md") {
			t.Errorf("early[%q]=%t", path, plan.early[path])
		}
	}
	if plan.early["missing"] {
		t.Fatal("unplanned path was adopted")
	}
	plan = earlyPlan{tracked: []string{"AGENTS.md", "ordinary"}, symlinks: map[string]string{"AGENTS.md": "../../ordinary"}}
	plan.split(nil)
	if plan.early["ordinary"] {
		t.Fatal("external symlink target was adopted")
	}
}
