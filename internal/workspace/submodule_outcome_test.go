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
