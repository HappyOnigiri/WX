package workspace

import (
	"testing"
)

func TestSubmoduleOutcomesSnapshotSortsResults(t *testing.T) {
	t.Parallel()
	results := &SubmoduleOutcomes{}
	results.BeginRepository("repo-b")
	results.BeginRepository("repo-a")
	results.Add(SubmoduleOutcome{Repository: "repo-b", Path: "nested/z", Depth: 2, Action: SubmoduleActionSkipped, Reason: SubmoduleReasonObjectMissing})
	results.Add(SubmoduleOutcome{Repository: "repo-a", Path: "child", Depth: 1, Action: SubmoduleActionMaterialized})
	results.Add(SubmoduleOutcome{Repository: "repo-b", Path: "child", Depth: 1, Action: SubmoduleActionOutOfScope})

	repositories, items := results.Snapshot()
	if got, want := len(repositories), 2; got != want || repositories[0] != "repo-a" || repositories[1] != "repo-b" {
		t.Fatalf("repositories=%v, want sorted %v", repositories, []string{"repo-a", "repo-b"})
	}
	if len(items) != 3 || items[0].Path != "child" || items[1].Path != "child" || items[2].Path != "nested/z" {
		t.Fatalf("items=%+v, want depth/path order", items)
	}
	items[0].Path = "changed"
	_, again := results.Snapshot()
	if again[0].Path == "changed" {
		t.Fatal("Snapshot returned mutable internal storage")
	}
	if got := results.Outcomes(); len(got) != 3 {
		t.Fatalf("Outcomes() returned %d items, want 3", len(got))
	}
}

// 同じ repository の比較は depth、同じ depth の比較は path へ進み、全比較を strict order に保つ。
func TestSubmoduleOutcomesSnapshotUsesStrictTieBreakers(t *testing.T) {
	t.Parallel()
	results := &SubmoduleOutcomes{}
	results.Add(SubmoduleOutcome{Repository: "repo-depth", Path: "shallow", Depth: 1, Action: SubmoduleActionSkipped})
	results.Add(SubmoduleOutcome{Repository: "repo-depth", Path: "deep", Depth: 2, Action: SubmoduleActionSkipped})
	results.Add(SubmoduleOutcome{Repository: "repo-path", Path: "alpha", Depth: 1, Action: SubmoduleActionSkipped})
	results.Add(SubmoduleOutcome{Repository: "repo-path", Path: "beta", Depth: 1, Action: SubmoduleActionSkipped})
	_, items := results.Snapshot()
	want := []struct {
		repository string
		path       string
		depth      int
	}{
		{repository: "repo-depth", path: "shallow", depth: 1},
		{repository: "repo-depth", path: "deep", depth: 2},
		{repository: "repo-path", path: "alpha", depth: 1},
		{repository: "repo-path", path: "beta", depth: 1},
	}
	if len(items) != len(want) {
		t.Fatalf("items=%+v, want %d items", items, len(want))
	}
	for index, expected := range want {
		got := items[index]
		if got.Repository != expected.repository || got.Path != expected.path || got.Depth != expected.depth {
			t.Fatalf("items[%d]=%+v, want repository=%q path=%q depth=%d", index, got, expected.repository, expected.path, expected.depth)
		}
	}
}

func TestSubmoduleOutcomesNilIsSafe(t *testing.T) {
	t.Parallel()
	var results *SubmoduleOutcomes
	results.BeginRepository("repo")
	results.Add(SubmoduleOutcome{Repository: "repo", Path: "child", Depth: 1, Action: SubmoduleActionSkipped})
	repositories, items := results.Snapshot()
	if repositories != nil || items != nil {
		t.Fatalf("nil snapshot=%v/%v, want nil", repositories, items)
	}
}

func TestSubmoduleOutcomesRegisterRepositoriesFromAddedResults(t *testing.T) {
	t.Parallel()
	results := &SubmoduleOutcomes{}
	results.Add(SubmoduleOutcome{Repository: "repo", Path: "child", Depth: 1, Action: SubmoduleActionSkipped})
	repositories, items := results.Snapshot()
	if len(repositories) != 1 || repositories[0] != "repo" || len(items) != 1 {
		t.Fatalf("repositories=%v items=%+v, want repo and one item", repositories, items)
	}
}

func TestSubmoduleOutcomesBeginRepositoryKeepsEmptyNamesOut(t *testing.T) {
	t.Parallel()
	results := &SubmoduleOutcomes{}
	results.BeginRepository("")
	results.BeginRepository("repo-only")
	repositories, items := results.Snapshot()
	if len(repositories) != 1 || repositories[0] != "repo-only" || len(items) != 0 {
		t.Fatalf("repositories=%v items=%+v, want only repo-only", repositories, items)
	}
}
