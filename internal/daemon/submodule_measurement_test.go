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
