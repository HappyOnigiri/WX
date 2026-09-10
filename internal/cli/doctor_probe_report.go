package cli

import (
	"fmt"

	"github.com/HappyOnigiri/WX/internal/config"
	"github.com/HappyOnigiri/WX/internal/daemon"
	"github.com/HappyOnigiri/WX/internal/diag"
)

// probeUsage は slot の測定結果を probe の内訳へ写す。
// 測定を待てなかった回と測れない platform を区別し、0 を実測値として読ませない。
func probeUsage(slot daemon.SlotView) (string, []diag.ProbeRepository) {
	if slot.Measurement == "" || slot.Measurement == daemon.MeasurementPending {
		return diag.ProbeUsagePending, nil
	}
	repositories := make([]diag.ProbeRepository, 0, len(slot.RepositoryUsage))
	for _, repository := range slot.RepositoryUsage {
		repositories = append(repositories, diag.ProbeRepository{
			Name: repository.Name, Files: repository.Files, AllocatedBytes: repository.AllocatedBytes,
			SharedBytes: repository.SharedBytes, ExclusiveBytes: repository.ExclusiveBytes,
		})
	}
	return diag.ProbeUsageMeasured, repositories
}

// probeSharingFindings は CoW 共有が効かなかった slot を参考として報告する。
// 測定前と共有を判定できない platform では判定そのものが成り立たないため、finding を出さない。
// その区別は slot の measurement だけで行い、報告する側の platform を条件にしない。
func probeSharingFindings(root, path string, slot daemon.SlotView) []diag.Finding {
	switch slot.Measurement {
	case "", daemon.MeasurementPending, daemon.MeasurementUnsupported:
		return nil
	}
	if slot.CopyMode == config.CopyModeCOW {
		return []diag.Finding{{
			Check: diag.CheckProbeSharing, Severity: diag.SeverityOK,
			Summary: "the prepared worktree shares blocks with the main worktrees", Target: path,
		}}
	}
	return []diag.Finding{{
		Check: diag.CheckProbeSharing, Severity: diag.SeverityInfo,
		Summary: "the prepared worktree shares no block with the main worktrees", Target: path,
		Cause:  fmt.Sprintf("no file of the slot prepared for %s was found to share blocks with its main worktree, so the copy took the full size", root),
		Action: "no action is required; check that the worktree root and the repositories live on the same APFS volume if you expect CoW sharing",
	}}
}

// prepareNoticeFindings は準備が exit 0 のまま残した出力を参考として報告する。
// 正常な hook も出力を出し得るため問題とはせず、本文と詳細ログの場所まで引き継いで判断を利用者へ渡す。
func prepareNoticeFindings(root string, notices []daemon.PrepareNotice) []diag.Finding {
	findings := make([]diag.Finding, 0, len(notices))
	for _, notice := range notices {
		cause := fmt.Sprintf("the %s phase of the preparation for %s finished without failing but wrote output", notice.Phase, root)
		details := []string{notice.Output}
		if notice.Truncated {
			details = append(details, "the output was truncated for this report")
		}
		if notice.DetailPath != "" {
			cause += " (full output in " + notice.DetailPath + ")"
		}
		findings = append(findings, diag.Finding{
			Check: diag.CheckPrepareOutput, Severity: diag.SeverityInfo,
			Summary: "the preparation wrote output without failing", Target: notice.Target, Cause: cause,
			Action:  "no action is required unless the output reports a failure the hook swallowed; run wx doctor --probe -v to read it",
			Details: details,
		})
	}
	return findings
}

func probeLeaseProblem(root, cause string) diag.Finding {
	return diag.Finding{
		Check: diag.CheckProbe, Severity: diag.SeverityProblem, Summary: "a workspace could not be leased for the probe",
		Target: root, Cause: cause,
		Action: "fix the reported cause; wx cannot hand this workspace to an agent until then",
	}
}

func probePrepareProblem(root, path, cause string) diag.Finding {
	return diag.Finding{
		Check: diag.CheckProbe, Severity: diag.SeverityProblem, Summary: "a workspace could not be prepared for the probe",
		Target: probeTarget(root, path), Cause: cause,
		Action: "read the reported detail log for the failing command, fix its cause, then run wx doctor --probe again",
	}
}

// probeTarget は報告の対象を、貸し出せた場合はその worktree、貸し出せなかった場合は workspace root にする。
func probeTarget(root, path string) string {
	if path != "" {
		return path
	}
	return root
}
