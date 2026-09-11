package workspace

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/HappyOnigiri/WX/internal/config"
)

func TestRootStagesSelectWithinCopyAndLinkPlan(t *testing.T) {
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

func TestRootStagesRejectNestedSymlinksAndChangedCopyTypes(t *testing.T) {
	source, target := t.TempDir(), t.TempDir()
	if err := os.Mkdir(filepath.Join(source, "configs"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/outside", filepath.Join(source, "configs", "link")); err != nil {
		t.Fatal(err)
	}
	if _, err := PlanRootStages(nil, source, RootRulesFromConfig(config.Workspace{Copy: []string{"configs"}}), nil); err == nil {
		t.Fatal("nested copy symlink was accepted")
	}
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
