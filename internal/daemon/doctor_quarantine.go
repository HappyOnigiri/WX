package daemon

import (
	"context"

	"github.com/HappyOnigiri/WorktreeX/internal/diag"
	"github.com/HappyOnigiri/WorktreeX/internal/i18n"
	"github.com/HappyOnigiri/WorktreeX/internal/state"
)

// quarantinedSlotFindings は隔離された slot を 1 件ずつ報告する。
// 隔離した実体には自動で触らないので、報告しないと待機枠が黙って減り、
// 起動が cold start へ落ちて済んでしまう分だけ失敗そのものが見えなくなる。
func (m *Manager) quarantinedSlotFindings(ctx context.Context) []diag.Finding {
	slots, err := m.store.QuarantinedSlots(ctx)
	if err != nil {
		return []diag.Finding{stateQueryProblem(diag.CheckQuarantinedSlots,
			"the quarantined slots could not be read", message("diag.quarantine.slots_unreadable"), "", err)}
	}
	if len(slots) == 0 {
		return []diag.Finding{{
			Check: diag.CheckQuarantinedSlots, Severity: diag.SeverityOK,
			Summary: "no slot is quarantined", Details: []string{"0 quarantined slot(s)"},
			Messages: diag.FindingMessages{
				Summary: message("diag.quarantine.no_slot"),
				Details: []i18n.Message{message("diag.detail.no_quarantined_slots")},
			},
		}}
	}
	findings := make([]diag.Finding, 0, len(slots))
	for _, slot := range slots {
		findings = append(findings, quarantinedSlotFinding(slot))
	}
	return findings
}

// quarantinedSlotFinding は隔離 1 件を、失敗 code と詳細ログの場所まで辿れる形にする。
func quarantinedSlotFinding(slot state.QuarantinedSlot) diag.Finding {
	cause, causeMessage := quarantinedSlotCause(slot)
	lead, leadMessage := jobFailureLead(slot.FailureDetailPath)
	details := []string{"slot " + slot.SlotID}
	detailMessages := []i18n.Message{message("diag.detail.slot", "SlotID", slot.SlotID)}
	if slot.UpdatedAt != "" {
		details = append(details, "quarantined at "+slot.UpdatedAt)
		detailMessages = append(detailMessages, message("diag.detail.quarantined_at", "Time", slot.UpdatedAt))
	}
	return diag.Finding{
		Check: diag.CheckQuarantinedSlots, Severity: diag.SeverityProblem,
		Summary: "a slot is quarantined, so its worktree stays out of service",
		Target:  slot.Path, Cause: cause, Details: details,
		Action: lead + ", then release the quarantined capacity with wx clear; wx keeps the worktree until you do or until the quarantine retention elapses",
		Messages: diag.FindingMessages{
			Summary: message("diag.quarantine.slot"),
			Cause:   causeMessage,
			Action:  message("diag.action.clear_quarantined_slot", "Lead", leadMessage),
			Details: detailMessages,
		},
	}
}

// quarantinedSlotCause は隔離の原因を「失敗 code・詳細ログの場所」の順で 1 行にまとめる。
// どちらも記録されていないことがあり、その場合は無いことを明示して原因を言い換えない。
// 失敗 code と path は値そのものなので、訳さず不透明値として message へ渡す。
func quarantinedSlotCause(slot state.QuarantinedSlot) (string, i18n.Message) {
	cause := "the slot was quarantined without a recorded failure code"
	result := message("diag.quarantine.cause")
	if slot.FailureCode != "" {
		cause = "the slot was quarantined with " + slot.FailureCode
		result = message("diag.quarantine.cause_code", "Code", slot.FailureCode)
	}
	if slot.FailureDetailPath == "" {
		return cause + "; no command output was recorded for it", message("diag.quarantine.cause_no_detail_path", "Cause", result)
	}
	return cause + " (command output in " + slot.FailureDetailPath + ")",
		message("diag.job.failed_detail_path", "Cause", result, "Path", slot.FailureDetailPath)
}
