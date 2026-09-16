package daemon

import (
	"testing"

	"github.com/HappyOnigiri/WX/internal/workspace"
)

func TestRecordPrepareSubmodulesAggregatesByRepositoryAndDepth(t *testing.T) {
	m := &Manager{}
	outcomes := &workspace.SubmoduleOutcomes{}
	outcomes.BeginRepository("repo")
	outcomes.Add(workspace.SubmoduleOutcome{Repository: "repo", Path: "child", Depth: 1, Action: workspace.SubmoduleActionMaterialized})
	outcomes.Add(workspace.SubmoduleOutcome{Repository: "repo", Path: "skipped", Depth: 1, Action: workspace.SubmoduleActionSkipped, Reason: workspace.SubmoduleReasonURLMissing})
	outcomes.Add(workspace.SubmoduleOutcome{Repository: "repo", Path: "grandchild", Depth: 2, Action: workspace.SubmoduleActionUnreachable, Reason: workspace.SubmoduleReasonAncestorSkipped})

	report := m.recordPrepareSubmodules(outcomes)
	if report == nil || len(report.Summaries) != 2 || len(report.Details) != 3 {
		t.Fatalf("report=%+v, want two summaries and three details", report)
	}
	if got := report.Summaries[0]; got.Materialized != 1 || got.Skipped != 1 || got.Depth != 1 {
		t.Fatalf("depth-1 summary=%+v", got)
	}
	if got := report.Summaries[1]; got.Unreachable != 1 || got.Depth != 2 {
		t.Fatalf("depth-2 summary=%+v", got)
	}
}

func TestRecordPrepareSubmodulesUsesOnlyRegisteredRepositoryForLegacyOutcomes(t *testing.T) {
	m := &Manager{}
	outcomes := &workspace.SubmoduleOutcomes{}
	outcomes.BeginRepository("repo")
	outcomes.Add(workspace.SubmoduleOutcome{Path: "child", Depth: 1, Action: workspace.SubmoduleActionMaterialized})
	report := m.recordPrepareSubmodules(outcomes)
	if report == nil || len(report.Summaries) != 1 || report.Summaries[0].Repository != "repo" || len(report.Details) != 1 || report.Details[0].Repository != "repo" {
		t.Fatalf("report=%+v, want the registered repository", report)
	}
}

func TestRecordPrepareSubmodulesCapsDetailsWithoutChangingCounts(t *testing.T) {
	m := &Manager{}
	outcomes := &workspace.SubmoduleOutcomes{}
	for index := 0; index < prepareSubmoduleDetailLimit+1; index++ {
		outcomes.Add(workspace.SubmoduleOutcome{Repository: "repo", Path: "child/" + string(rune('a'+index%26)), Depth: 1, Action: workspace.SubmoduleActionSkipped, Reason: workspace.SubmoduleReasonObjectMissing})
	}
	report := m.recordPrepareSubmodules(outcomes)
	if report == nil || !report.Truncated || len(report.Details) != prepareSubmoduleDetailLimit {
		t.Fatalf("report=%+v, want truncated details=%d", report, prepareSubmoduleDetailLimit)
	}
	if got := report.Summaries[0].Skipped; got != prepareSubmoduleDetailLimit+1 {
		t.Fatalf("skipped=%d, want %d", got, prepareSubmoduleDetailLimit+1)
	}
}

// repository・depth・action の境界では、集計と明細の表示順をそれぞれ固定する。
func TestRecordPrepareSubmodulesOrdersSummariesAndDetailsAtBoundaries(t *testing.T) {
	t.Parallel()
	m := &Manager{}
	outcomes := &workspace.SubmoduleOutcomes{}
	outcomes.Add(workspace.SubmoduleOutcome{Repository: "repo-b", Path: "same", Depth: 1, Action: workspace.SubmoduleActionOutOfScope})
	outcomes.Add(workspace.SubmoduleOutcome{Repository: "repo-a", Path: "z", Depth: 2, Action: workspace.SubmoduleActionMaterialized})
	outcomes.Add(workspace.SubmoduleOutcome{Repository: "repo-a", Path: "same", Depth: 1, Action: workspace.SubmoduleActionSkipped})

	report := m.recordPrepareSubmodules(outcomes)
	if report == nil || len(report.Summaries) != 3 || len(report.Details) != 3 {
		t.Fatalf("report=%+v, want three summaries and details", report)
	}
	wantSummaries := []struct {
		repository string
		depth      int
	}{{"repo-a", 1}, {"repo-a", 2}, {"repo-b", 1}}
	for index, want := range wantSummaries {
		got := report.Summaries[index]
		if got.Repository != want.repository || got.Depth != want.depth {
			t.Fatalf("summary[%d]=%+v, want repository=%q depth=%d", index, got, want.repository, want.depth)
		}
	}
	if got := report.Details; got[0].Action != workspace.SubmoduleActionSkipped || got[1].Action != workspace.SubmoduleActionOutOfScope || got[2].Action != workspace.SubmoduleActionMaterialized {
		t.Fatalf("detail order=%+v, want skipped, out-of-scope, materialized", got)
	}
}

// outcomeLess は sort の strict ordering を満たし、同一要素を自分自身より前とは判定しない。
func TestOutcomeLessRejectsAnIdenticalOutcome(t *testing.T) {
	t.Parallel()
	outcome := workspace.SubmoduleOutcome{Repository: "repo", Path: "child", Depth: 1, Action: workspace.SubmoduleActionSkipped, Reason: workspace.SubmoduleReasonObjectMissing}
	if outcomeLess(outcome, outcome) {
		t.Fatal("an outcome sorted before itself")
	}
}
