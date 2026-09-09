package workspace

import (
	"testing"

	"github.com/HappyOnigiri/WX/internal/state"
)

func TestSortedPlacementsUsesRepositoryThenPath(t *testing.T) {
	placements := sortedPlacements(map[string]state.Placement{
		"b": {RepositoryID: "b", RelativePath: "z"},
		"a": {RepositoryID: "a", RelativePath: "y"},
	})
	if len(placements) != 2 || placements[0].RepositoryID != "a" || placements[1].RepositoryID != "b" {
		t.Fatalf("placements=%+v", placements)
	}
}
