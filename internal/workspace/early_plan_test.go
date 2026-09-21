package workspace

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestEarlyPlanSelectsOnlyPlannedPathsAndSymlinkClosure(t *testing.T) {
	t.Parallel()
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

// 先行配置の link 衝突は placement 記録へ進めず、呼び出し側へ返す。
func TestMaterializePlanPropagatesLinkCollision(t *testing.T) {
	t.Parallel()
	source, repo, preparer, _, target := prepareEdgesFixture(t)
	if err := os.WriteFile(filepath.Join(source, ".gitignore"), []byte("linked\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "linked"), []byte("source\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "linked"), []byte("occupied\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	relative, err := filepath.Rel(preparer.RootPath, target)
	if err != nil {
		t.Fatal(err)
	}
	plan := &earlyPlan{
		repositoryID: string(repo.ID),
		sourcePath:   string(repo.MainPath),
		links:        []linkSource{{relative: "linked", present: true}},
		early:        map[string]bool{"linked": false},
	}
	locked := &lockedTarget{root: preparer.OwnedRoot, relative: relative}
	if err := preparer.materializePlan(context.Background(), repo, locked, plan, false); err == nil {
		t.Fatal("early plan link collision was ignored")
	}
}
