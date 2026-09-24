package cli

import (
	"fmt"

	"github.com/HappyOnigiri/WorktreeX/internal/config"
	"github.com/HappyOnigiri/WorktreeX/internal/daemon"
	"github.com/HappyOnigiri/WorktreeX/internal/diag"
	"github.com/HappyOnigiri/WorktreeX/internal/i18n"
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
			Messages: diag.FindingMessages{Summary: i18n.Message{ID: "diag.probe.sharing_ok"}},
		}}
	}
	return []diag.Finding{{
		Check: diag.CheckProbeSharing, Severity: diag.SeverityInfo,
		Summary: "the prepared worktree shares no block with the main worktrees", Target: path,
		Cause:  fmt.Sprintf("no file of the slot prepared for %s was found to share blocks with its main worktree, so the copy took the full size", root),
		Action: "no action is required; check that the worktree root and the repositories live on the same APFS volume if you expect CoW sharing",
		Messages: diag.FindingMessages{
			Summary: i18n.Message{ID: "diag.probe.sharing_none"},
			Cause:   i18n.Message{ID: "diag.probe.sharing_none_cause", Data: map[string]any{"Root": root}},
			Action:  i18n.Message{ID: "diag.action.probe_cow_volume"},
		},
	}}
}

// prepareNoticeFindings は準備が exit 0 のまま残した出力を参考として報告する。
// 正常な hook も出力を出し得るため問題とはせず、本文と詳細ログの場所まで引き継いで判断を利用者へ渡す。
func prepareNoticeFindings(root string, notices []daemon.PrepareNotice) []diag.Finding {
	findings := make([]diag.Finding, 0, len(notices))
	for _, notice := range notices {
		cause := fmt.Sprintf("the %s phase of the preparation for %s finished without failing but wrote output", notice.Phase, root)
		data := map[string]any{"Phase": notice.Phase, "Root": root}
		causeMessage := i18n.Message{ID: "diag.probe.prepare_output_cause", Data: data}
		// notice.Output は準備 command の出力そのものなので、detail の 1 件目は訳さない。
		details := []string{notice.Output}
		detailMessages := []i18n.Message{{}}
		if notice.Truncated {
			details = append(details, "the output was truncated for this report")
			detailMessages = append(detailMessages, i18n.Message{ID: "diag.probe.output_truncated"})
		}
		if notice.DetailPath != "" {
			cause += " (full output in " + notice.DetailPath + ")"
			data["Path"] = notice.DetailPath
			causeMessage = i18n.Message{ID: "diag.probe.output_detail_path", Data: data}
		}
		findings = append(findings, diag.Finding{
			Check: diag.CheckPrepareOutput, Severity: diag.SeverityInfo,
			Summary: "the preparation wrote output without failing", Target: notice.Target, Cause: cause,
			Action:  "no action is required unless the output reports a failure the hook swallowed; run wx doctor --probe -v to read it",
			Details: details,
			Messages: diag.FindingMessages{
				Summary: i18n.Message{ID: "diag.probe.prepare_output"},
				Cause:   causeMessage,
				Action:  i18n.Message{ID: "diag.action.probe_read_output"},
				Details: detailMessages,
			},
		})
	}
	return findings
}

// prepareFailureFindings は early ready の後に準備が失敗し、それでも貸出が続いている slot を報告する。
// この経路は slot を隔離せず readiness も成功で返すため、workspace が不完全なまま使われていることは
// slot に残った失敗記録からしか分からない。
func prepareFailureFindings(root string, slot daemon.SlotView) []diag.Finding {
	if slot.PrepareFailureCode == "" {
		return nil
	}
	detailPath := slot.PrepareFailureDetailPath
	if detailPath == "" {
		detailPath = "unavailable"
	}
	return []diag.Finding{{
		Check: diag.CheckPrepareFailure, Severity: diag.SeverityProblem,
		Summary: "the prepared workspace is incomplete", Target: slot.Path,
		Cause: fmt.Sprintf("the preparation for %s failed with %s after the workspace had already been handed to the agent (details in %s)",
			root, slot.PrepareFailureCode, detailPath),
		Action: "read the detail log and fix the cause, then take a fresh workspace; files may be missing and prepare commands may not have run",
		Messages: diag.FindingMessages{
			Summary: i18n.Message{ID: "diag.probe.prepare_incomplete"},
			Cause: i18n.Message{
				ID:   "diag.probe.prepare_incomplete_cause",
				Data: map[string]any{"Root": root, "Code": slot.PrepareFailureCode, "Path": detailPath},
			},
			Action: i18n.Message{ID: "diag.action.probe_prepare_incomplete"},
		},
	}}
}

func prepareSubmoduleProbeReport(report *daemon.PrepareSubmoduleReport) *diag.ProbeSubmoduleReport {
	if report == nil {
		return nil
	}
	converted := &diag.ProbeSubmoduleReport{Truncated: report.Truncated}
	for _, summary := range report.Summaries {
		converted.Summaries = append(converted.Summaries, diag.ProbeSubmoduleSummary{
			Repository: summary.Repository, Depth: summary.Depth, Materialized: summary.Materialized,
			OutOfScope: summary.OutOfScope, Skipped: summary.Skipped, Unreachable: summary.Unreachable,
		})
	}
	for _, detail := range report.Details {
		converted.Details = append(converted.Details, diag.ProbeSubmoduleDetail{
			Repository: detail.Repository, Path: detail.Path, Depth: detail.Depth,
			Action: detail.Action, Reason: detail.Reason,
		})
	}
	return converted
}

func excludedSubmodulePaths(report *daemon.PrepareSubmoduleReport) map[string]bool {
	if report == nil || report.Truncated {
		return nil
	}
	paths := map[string]bool{}
	for _, detail := range report.Details {
		if detail.Action == "out_of_scope" {
			paths[detail.Path] = true
		}
	}
	return paths
}

func prepareSubmoduleFindings(root string, report *daemon.PrepareSubmoduleReport) []diag.Finding {
	if report == nil {
		return nil
	}
	findings := make([]diag.Finding, 0)
	for _, detail := range report.Details {
		var summaryID, causeID string
		severity := diag.SeverityInfo
		switch detail.Action {
		case "skipped":
			summaryID, causeID, severity = "diag.probe.submodule_skipped", "diag.probe.submodule_skipped_cause", diag.SeverityProblem
		case "unreachable":
			summaryID, causeID = "diag.probe.submodule_unreachable", "diag.probe.submodule_unreachable_cause"
		default:
			continue
		}
		findings = append(findings, diag.Finding{
			Check: diag.CheckProbeSubmodule, Severity: severity,
			Summary: "a prepared submodule was not available", Target: detail.Path,
			Cause:   fmt.Sprintf("the preparation for %s classified submodule %s as %s (%s)", root, detail.Path, detail.Action, detail.Reason),
			Action:  "make the submodule source and requested commit available, then run wx doctor --probe again",
			Details: []string{fmt.Sprintf("repository=%s depth=%d", detail.Repository, detail.Depth)},
			Messages: diag.FindingMessages{
				Summary: message(summaryID),
				Cause:   message(causeID, "Root", root, "Path", detail.Path, "Reason", detail.Reason),
				Action:  message("diag.action.probe_submodule_prepare"),
				Details: []i18n.Message{message("diag.probe.submodule_detail", "Repository", detail.Repository, "Depth", detail.Depth)},
			},
		})
	}
	return findings
}

// probeLeaseProblem は貸出まで辿り着けなかった失敗を、失敗した区間ごとの手順で報告する。
// RPC の error code はほぼ REQUEST_FAILED に潰れるため、分類の材料は区間名だけである。
func probeLeaseProblem(root string, stage probeStage) diag.Finding {
	action, actionMessage := probeLeaseAction(root, stage.name)
	return diag.Finding{
		Check: diag.CheckProbe, Severity: diag.SeverityProblem, Summary: "a workspace could not be leased for the probe",
		Target: root, Cause: stage.text,
		Action: action,
		Messages: diag.FindingMessages{
			Summary: i18n.Message{ID: "diag.probe.lease_failed"},
			Cause:   stage.message,
			Action:  actionMessage,
		},
	}
}

func probeLeaseAction(root, stage string) (string, i18n.Message) {
	if stage == probeStageRetireStandby {
		// standby を回収できないうちは、実地検査が測るのが cold start の重さではなくなる。
		return "run wx clear --standby to drop the standby worktrees yourself, then run wx doctor --probe again",
			i18n.Message{ID: "diag.action.probe_clear_standby"}
	}
	return fmt.Sprintf("run wx doctor and read its worktree_registration finding for %s; that check reports why wx refuses to lease this workspace", root),
		i18n.Message{ID: "diag.action.probe_read_registration", Data: map[string]any{"Root": root}}
}

func probePrepareProblem(root, path string, stage probeStage) diag.Finding {
	return diag.Finding{
		Check: diag.CheckProbe, Severity: diag.SeverityProblem, Summary: "a workspace could not be prepared for the probe",
		Target: probeTarget(root, path), Cause: stage.text,
		Action: "read the reported detail log for the failing command, fix its cause, then run wx doctor --probe again",
		Messages: diag.FindingMessages{
			Summary: i18n.Message{ID: "diag.probe.prepare_failed"},
			Cause:   stage.message,
			Action:  i18n.Message{ID: "diag.action.probe_prepare_failed"},
		},
	}
}

// 実地検査の区間名である。対処は区間ごとに違うため、報告側がこの値で手順を分ける。
const (
	probeStageRetireStandby = "retire standby"
	probeStageLease         = "lease"
	probeStageEarlyReady    = "early ready"
	probeStageFullReady     = "full ready"
)

// probeStage は実地検査が失敗した区間である。name は手順を分けるための区間名、
// text は Probe.Error と JSON へ出る英語本文で、message はそれを表示言語で解決するための ID を持つ。
type probeStage struct {
	name    string
	text    string
	message i18n.Message
}

// probeStageIDs は区間ごとの message ID である。区間名は wx が決める固定の列挙なので訳し、
// 続く原因は外部由来の本文としてそのまま埋め込む。
var probeStageIDs = map[string]string{
	probeStageRetireStandby: "diag.probe.stage.retire_standby",
	probeStageLease:         "diag.probe.stage.lease",
	probeStageEarlyReady:    "diag.probe.stage.early_ready",
	probeStageFullReady:     "diag.probe.stage.full_ready",
}

func newProbeStage(stage string, err error) probeStage {
	return probeStage{
		name:    stage,
		text:    stage + ": " + err.Error(),
		message: i18n.Message{ID: probeStageIDs[stage], Data: map[string]any{"Error": err.Error()}},
	}
}

// probeTarget は報告の対象を、貸し出せた場合はその worktree、貸し出せなかった場合は workspace root にする。
func probeTarget(root, path string) string {
	if path != "" {
		return path
	}
	return root
}
