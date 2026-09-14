package daemon

import (
	"testing"

	"github.com/HappyOnigiri/WX/internal/diag"
	"github.com/HappyOnigiri/WX/internal/discovery"
	"github.com/HappyOnigiri/WX/internal/domain"
	"github.com/HappyOnigiri/WX/internal/workspace"
)

func TestLFSObjectFindingReportsRepairableCandidatesAsInfo(t *testing.T) {
	repo := discovery.Repository{MainPath: domain.CanonicalPath("/source")}
	finding := lfsObjectFinding(repo, workspace.LFSObjectDiagnostics{Objects: []workspace.LFSObjectDiagnostic{{
		Object: workspace.LFSObjectInfo{OID: "sha256:abc", Size: 10}, CandidatePath: "asset.bin",
	}}})
	if finding.Check != diag.CheckLFSObjects || finding.Severity != diag.SeverityInfo {
		t.Fatalf("finding=%+v", finding)
	}
	if finding.Messages.Summary.ID == "" || finding.Messages.Cause.ID == "" || finding.Messages.Action.ID == "" {
		t.Fatalf("finding messages=%+v", finding.Messages)
	}
}

func TestLFSObjectFindingReportsUnrepairableObjectAsProblem(t *testing.T) {
	repo := discovery.Repository{MainPath: domain.CanonicalPath("/source")}
	finding := lfsObjectFinding(repo, workspace.LFSObjectDiagnostics{Objects: []workspace.LFSObjectDiagnostic{{
		Object: workspace.LFSObjectInfo{OID: "sha256:abc", Size: 10},
	}}})
	if finding.Check != diag.CheckLFSObjects || finding.Severity != diag.SeverityProblem {
		t.Fatalf("finding=%+v", finding)
	}
	if finding.Messages.Summary.ID == "" || finding.Messages.Cause.ID == "" || finding.Messages.Action.ID == "" {
		t.Fatalf("finding messages=%+v", finding.Messages)
	}
}
