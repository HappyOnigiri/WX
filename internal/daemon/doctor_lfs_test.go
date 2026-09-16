package daemon

import (
	"strings"
	"testing"

	"github.com/HappyOnigiri/WX/internal/diag"
	"github.com/HappyOnigiri/WX/internal/discovery"
	"github.com/HappyOnigiri/WX/internal/domain"
	"github.com/HappyOnigiri/WX/internal/workspace"
)

// size が一致する候補を数えた結果は、候補の数とともに利用者へ示す。
// 1 件の候補で負数にならないことを境界として固定する。
func TestLFSObjectFindingCountsRepairableCandidates(t *testing.T) {
	t.Parallel()
	repo := discovery.Repository{MainPath: domain.CanonicalPath("/source")}
	finding := lfsObjectFinding(repo, workspace.LFSObjectDiagnostics{Objects: []workspace.LFSObjectDiagnostic{{
		Object: workspace.LFSObjectInfo{OID: "sha256:abc", Size: 10}, CandidatePath: "asset.bin",
	}}})
	if finding.Severity != diag.SeverityInfo {
		t.Fatalf("finding=%+v, want a repairable informational finding", finding)
	}
	if !strings.HasPrefix(finding.Cause, "1 LFS object(s),") {
		t.Fatalf("cause=%q, want one repairable object", finding.Cause)
	}
}

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
