package cli

import (
	"strings"
	"testing"

	"github.com/HappyOnigiri/WX/internal/config"
	"github.com/HappyOnigiri/WX/internal/daemon"
	"github.com/HappyOnigiri/WX/internal/diag"
	"github.com/HappyOnigiri/WX/internal/state"
)

// 測定前の slot を測定済みとして扱うと、0 バイトが実測値として読まれてしまう。
func TestProbeUsageSeparatesPendingFromMeasured(t *testing.T) {
	usage, repositories := probeUsage(daemon.SlotView{Measurement: daemon.MeasurementPending})
	if usage != diag.ProbeUsagePending || repositories != nil {
		t.Fatalf("pending slot = %s %+v", usage, repositories)
	}
	if usage, _ := probeUsage(daemon.SlotView{}); usage != diag.ProbeUsagePending {
		t.Fatalf("unmeasured slot = %s", usage)
	}
	measured := daemon.SlotView{
		Measurement: "log2phys_first_last",
		RepositoryUsage: []daemon.SlotRepositoryView{
			{Name: "repo", Files: 3, AllocatedBytes: 900, SharedBytes: 400, ExclusiveBytes: 500},
		},
	}
	usage, repositories = probeUsage(measured)
	if usage != diag.ProbeUsageMeasured || len(repositories) != 1 {
		t.Fatalf("measured slot = %s %+v", usage, repositories)
	}
	if repositories[0] != (diag.ProbeRepository{Name: "repo", Files: 3, AllocatedBytes: 900, SharedBytes: 400, ExclusiveBytes: 500}) {
		t.Fatalf("repository = %+v", repositories[0])
	}
}

// 共有できない platform と測定前の slot では CoW の判定そのものが成り立たない。
func TestProbeSharingFindingsSkipUndecidableSlots(t *testing.T) {
	if got := probeSharingFindings("/root", "/slot", daemon.SlotView{Measurement: daemon.MeasurementPending}); got != nil {
		t.Fatalf("pending slot = %+v", got)
	}
	if got := probeSharingFindings("/root", "/slot", daemon.SlotView{}); got != nil {
		t.Fatalf("unmeasured slot = %+v", got)
	}
	if got := probeSharingFindings("/root", "/slot", daemon.SlotView{Measurement: "unsupported"}); len(got) != 1 || got[0].Severity == diag.SeverityProblem {
		t.Fatalf("unsupported platform = %+v", got)
	}
}

// CoW が効かないのは遅くなるだけで対処の要る故障ではないため、問題にはしない。
func TestProbeSharingFindingsReportFallbackAsInfo(t *testing.T) {
	shared := probeSharingFindings("/root", "/slot", daemon.SlotView{Measurement: "log2phys_first_last", CopyMode: config.CopyModeCOW})
	if len(shared) != 1 || shared[0].Severity != diag.SeverityOK {
		t.Fatalf("shared slot = %+v", shared)
	}
	fallback := probeSharingFindings("/root", "/slot", daemon.SlotView{Measurement: "log2phys_first_last", CopyMode: config.CopyModeCopy})
	if len(fallback) != 1 || fallback[0].Severity != diag.SeverityInfo {
		t.Fatalf("fallback slot = %+v", fallback)
	}
	if fallback[0].Check != diag.CheckProbeSharing || fallback[0].Target != "/slot" {
		t.Fatalf("fallback finding = %+v", fallback[0])
	}
}

// 出力の全文は詳細ログにあるため、原因からその場所へ辿れないと診断が途切れる。
func TestPrepareNoticeFindingsCarryOutputAndDetailPath(t *testing.T) {
	notices := []daemon.PrepareNotice{{
		Target: "/slot/repo", Phase: "post-checkout", Output: "submodule update skipped",
		Truncated: true, DetailPath: "/logs/details/ABC.log",
	}}
	findings := prepareNoticeFindings("/root", notices)
	if len(findings) != 1 {
		t.Fatalf("findings = %+v, want one", findings)
	}
	finding := findings[0]
	if finding.Severity != diag.SeverityInfo || finding.Check != diag.CheckPrepareOutput || finding.Target != "/slot/repo" {
		t.Fatalf("finding = %+v", finding)
	}
	if !strings.Contains(finding.Cause, "/logs/details/ABC.log") || !strings.Contains(finding.Cause, "post-checkout") {
		t.Fatalf("cause = %q", finding.Cause)
	}
	if len(finding.Details) != 2 || finding.Details[0] != "submodule update skipped" {
		t.Fatalf("details = %+v", finding.Details)
	}
}

func TestPrepareNoticeFindingsAreEmptyWithoutNotices(t *testing.T) {
	if got := prepareNoticeFindings("/root", nil); len(got) != 0 {
		t.Fatalf("findings = %+v, want none", got)
	}
}

// 問題は cause と action が両方揃っていないと、利用者が次の一手を決められない。
func TestProbeProblemFindingsCarryCauseAndAction(t *testing.T) {
	for _, finding := range []diag.Finding{
		probeLeaseProblem("/root", "lease: refused"),
		probePrepareProblem("/root", "/slot", "full ready: timed out"),
	} {
		if finding.Severity != diag.SeverityProblem || finding.Cause == "" || finding.Action == "" {
			t.Fatalf("finding = %+v", finding)
		}
	}
	if got := probePrepareProblem("/root", "", "lease: refused"); got.Target != "/root" {
		t.Fatalf("target without a leased path = %q", got.Target)
	}
}

// 貸出の種別は `wx slots` の AGENT 列に出るため、貸出コマンドのものと取り違えられてはならない。
func TestProbeAgentKindIsNotALeaseCommand(t *testing.T) {
	if leaseAgentKind(probeAgentKind) {
		t.Fatalf("probe agent kind %q is treated as a lease command", probeAgentKind)
	}
	if state.LeaseKindPath == "" {
		t.Fatal("path lease kind is empty")
	}
}
