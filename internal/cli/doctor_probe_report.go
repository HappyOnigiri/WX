package cli

import (
	"fmt"

	"github.com/HappyOnigiri/WX/internal/config"
	"github.com/HappyOnigiri/WX/internal/daemon"
	"github.com/HappyOnigiri/WX/internal/diag"
	"github.com/HappyOnigiri/WX/internal/i18n"
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
