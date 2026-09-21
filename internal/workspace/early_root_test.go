package workspace

import (
	"maps"
	"os"
	"path/filepath"
	"testing"

	"github.com/HappyOnigiri/WX/internal/config"
	"github.com/HappyOnigiri/WX/internal/state"
)

func TestRootStagesSelectWithinCopyAndLinkPlan(t *testing.T) {
	t.Parallel()
	source, target := t.TempDir(), t.TempDir()
	for path, content := range map[string]string{"AGENTS.md": "root rules", "configs/early": "early", "configs/late": "late", "unplanned": "skip", "shared/file": "linked"} {
		path = filepath.Join(source, path)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	rules := RootRulesFromConfig(config.Workspace{Copy: []string{"configs"}, Link: []string{"shared"}})
	stage, err := PlanRootStages(nil, source, rules, []string{"configs/early", "shared/file", "unplanned"})
	if err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(target)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if err := stage.Materialize(root, true); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"AGENTS.md", "configs/early", "shared/file"} {
		if _, err := root.Stat(path); err != nil && path != "shared/file" {
			t.Fatalf("early %s: %v", path, err)
		}
	}
	if _, err := os.Stat(filepath.Join(target, "shared", "file")); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"configs/late", "unplanned", ".git"} {
		if _, err := root.Lstat(path); !os.IsNotExist(err) {
			t.Fatalf("unexpected path %s: %v", path, err)
		}
	}
	if err := root.WriteFile("configs/early", []byte("keep changes"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := stage.Materialize(root, false); err != nil {
		t.Fatal(err)
	}
	if data, err := root.ReadFile("configs/early"); err != nil || string(data) != "keep changes" {
		t.Fatalf("early rewritten: %q, %v", data, err)
	}
	if _, err := root.Stat("configs/late"); err != nil {
		t.Fatal(err)
	}
	if _, err := PlanRootStages(nil, source, RootRulesFromConfig(config.Workspace{Copy: []string{"missing"}}), nil); err == nil {
		t.Fatal("missing required copy accepted")
	}
}

// TestRootStagesSkipNestedSymlinksLikeStandbyPlacementsは、copy配下のnested symlinkを
// 段階準備とstandbyの配置計画が同じに扱うことを確認する。
// 片方だけが失敗すると、同じsourceでも貸出経路で成否が分かれる。
func TestRootStagesSkipNestedSymlinksLikeStandbyPlacements(t *testing.T) {
	t.Parallel()
	source, target := t.TempDir(), t.TempDir()
	if err := os.Mkdir(filepath.Join(source, "configs"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "configs", "value.txt"), []byte("value"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/outside", filepath.Join(source, "configs", "link")); err != nil {
		t.Fatal(err)
	}
	rules := RootRulesFromConfig(config.Workspace{Copy: []string{"configs"}})
	stage, err := PlanRootStages(nil, source, rules, nil)
	if err != nil {
		t.Fatalf("nested copy symlink was rejected: %v", err)
	}
	root, err := os.OpenRoot(target)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	for _, early := range []bool{true, false} {
		if err := stage.Materialize(root, early); err != nil {
			t.Fatalf("materialize early=%t: %v", early, err)
		}
	}
	if _, err := root.Stat(filepath.Join("configs", "value.txt")); err != nil {
		t.Fatal(err)
	}
	if _, err := root.Lstat(filepath.Join("configs", "link")); !os.IsNotExist(err) {
		t.Fatalf("nested symlink was materialized: %v", err)
	}
	standby, err := RootPlacements(source, rules)
	if err != nil {
		t.Fatal(err)
	}
	if staged, want := placementKinds(stage.Placements()), placementKinds(standby); !maps.Equal(staged, want) {
		t.Fatalf("staged placements %v differ from standby placements %v", staged, want)
	}
}

func placementKinds(placements []state.Placement) map[string]string {
	kinds := map[string]string{}
	for _, placement := range placements {
		kinds[placement.RelativePath] = placement.Kind
	}
	return kinds
}

func TestRootStagesRejectChangedCopyTypes(t *testing.T) {
	t.Parallel()
	source, target := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(source, "leaf"), []byte("planned file"), 0o600); err != nil {
		t.Fatal(err)
	}
	stage, err := PlanRootStages(nil, source, RootRulesFromConfig(config.Workspace{Copy: []string{"leaf"}}), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(source, "leaf")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(source, "leaf"), 0o700); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(target)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if err := stage.Materialize(root, false); err == nil {
		t.Fatal("changed source type was copied recursively")
	}
}

// workspace root の link 先衝突は計画の実体化失敗として返し、成功した配置履歴へ進めない。
func TestRootStagesPropagateLinkCollision(t *testing.T) {
	t.Parallel()
	source, target := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(source, "linked"), []byte("source\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	stage, err := PlanRootStages(nil, source, RootRulesFromConfig(config.Workspace{Link: []string{"linked"}}), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "linked"), []byte("occupied\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(target)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	if err := stage.Materialize(root, false); err == nil {
		t.Fatal("root link collision was ignored")
	}
}
