package cli

import (
	"context"
	"fmt"

	"github.com/HappyOnigiri/WX/internal/daemon"
	"github.com/HappyOnigiri/WX/internal/diag"
)

// inspectLeasedWorkspace は貸出中の slot を変更せずに検査する。
// doctor と初回セットアップ検査で共有し、貸出・返却の判断は呼び出し側へ残す。
func (c Client) inspectLeasedWorkspace(ctx context.Context, root, sessionID, leasePath string, requireMeasurements bool) (diag.Probe, []diag.Finding) {
	probe := diag.Probe{Workspace: root, SlotID: sessionID, Path: leasePath, Usage: diag.ProbeUsageUnavailable}
	findings := []diag.Finding{}
	measurement := c.prepareMeasurement(ctx, sessionID)
	var excludedSubmodules map[string]bool
	checkSubmodules := false
	if measurement == nil {
		probe.PhasesUnavailable = true
		if requireMeasurements {
			findings = append(findings, setupMeasurementFinding(leasePath, "the daemon no longer has the preparation measurement for this lease"))
		}
	} else {
		for _, phase := range measurement.Phases {
			probe.Phases = append(probe.Phases, diag.ProbePhase{Name: phase.Name, Count: phase.Count, MS: phase.MS})
		}
		findings = append(findings, prepareNoticeFindings(root, measurement.Notices)...)
		probe.Submodules = prepareSubmoduleProbeReport(measurement.Submodules)
		findings = append(findings, prepareSubmoduleFindings(root, measurement.Submodules)...)
		excludedSubmodules = excludedSubmodulePaths(measurement.Submodules)
		checkSubmodules = true
	}
	findings = append(findings, c.probeWorktreeFindingsWithOptions(ctx, root, leasePath, excludedSubmodules, checkSubmodules)...)
	viewCtx, cancel := ctx, func() {}
	if !requireMeasurements {
		viewCtx, cancel = context.WithTimeout(ctx, probeUsageTimeout)
	}
	slot := c.probeSlotView(viewCtx, sessionID)
	cancel()
	probe.Usage, probe.Repositories = probeUsage(slot)
	findings = append(findings, prepareFailureFindings(root, slot)...)
	findings = append(findings, probeSharingFindings(root, leasePath, slot)...)
	if requireMeasurements && measurementUnavailableForSetup(slot.Measurement) {
		findings = append(findings, setupMeasurementFinding(leasePath, fmt.Sprintf("the slot usage measurement is %q", slot.Measurement)))
	}
	return probe, findings
}

func measurementUnavailableForSetup(measurement string) bool {
	switch measurement {
	case "", daemon.MeasurementPending, daemon.MeasurementUnsupported:
		return true
	default:
		return false
	}
}

func setupMeasurementFinding(target, cause string) diag.Finding {
	return diag.Finding{
		Check: diag.CheckSetupMeasurement, Severity: diag.SeverityUnchecked,
		Summary: "part of the initial worktree setup check could not be completed", Target: target, Cause: cause,
		Action: "run wx setup-check again after the daemon has finished measuring the worktree",
		Messages: diag.FindingMessages{
			Summary: message("diag.setup.measurement_unchecked"),
			Action:  message("diag.action.setup_check_again"),
		},
	}
}
