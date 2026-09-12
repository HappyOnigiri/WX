package workspace

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/HappyOnigiri/WX/internal/config"
	"github.com/HappyOnigiri/WX/internal/state"
)

func TestSortedPlacementsUsesRepositoryThenPath(t *testing.T) {
	t.Parallel()
	placements := sortedPlacements(map[string]state.Placement{
		"b": {RepositoryID: "b", RelativePath: "z"},
		"a": {RepositoryID: "a", RelativePath: "y"},
	})
	if len(placements) != 2 || placements[0].RepositoryID != "a" || placements[1].RepositoryID != "b" {
		t.Fatalf("placements=%+v", placements)
	}
}

// TestRootPlacementsListsCopiesAndLinks は workspace root の配置計画が、copy を file 単位に開き
// link を1件として返すことを確認する。standby の UPDATE はこの計画を旧配置履歴と比較する。
func TestRootPlacementsListsCopiesAndLinks(t *testing.T) {
	t.Parallel()
	source := t.TempDir()
	for path, content := range map[string]string{"AGENTS.md": "root rules", "configs/app.yml": "config", "shared/file": "linked"} {
		full := filepath.Join(source, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	placements, err := RootPlacements(source, RootRulesFromConfig(config.Workspace{Copy: []string{"configs"}, Link: []string{"shared"}}))
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[string]string{}
	for _, placement := range placements {
		if placement.RepositoryID != "" {
			t.Fatalf("workspace root placement carries a repository: %+v", placement)
		}
		kinds[placement.RelativePath] = placement.Kind
	}
	if kinds["AGENTS.md"] != "copy" || kinds["configs/app.yml"] != "copy" || kinds["shared"] != "link" {
		t.Fatalf("placements=%+v", placements)
	}
	if _, ok := kinds["configs"]; ok {
		t.Fatalf("copied directory recorded as a placement: %+v", placements)
	}
}
