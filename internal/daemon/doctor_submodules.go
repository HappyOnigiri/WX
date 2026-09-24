package daemon

import (
	"context"
	"strconv"
	"strings"

	"github.com/HappyOnigiri/WorktreeX/internal/diag"
	"github.com/HappyOnigiri/WorktreeX/internal/i18n"
	"github.com/HappyOnigiri/WorktreeX/internal/state"
)

// unsavedSubmoduleFindings は、snapshot に入らなかった submodule 作業のために残している slot を報告する。
// slot が保持期限を過ぎても消えない理由と、退避・削除の手順を利用者が読める場所はここだけなので Problem にする。
// Info では `--verbose` でしか出ず、対処が要る状態を隠してしまう。
func (m *Manager) unsavedSubmoduleFindings(ctx context.Context) []diag.Finding {
	slots, err := m.store.ProtectedSlots(ctx)
	if err != nil {
		return []diag.Finding{stateQueryProblem(diag.CheckUnsavedSubmodules,
			"the unsaved submodule records could not be read", i18n.Message{ID: "diag.submodule.unreadable"}, "", err)}
	}
	if len(slots) == 0 {
		return []diag.Finding{{
			Check: diag.CheckUnsavedSubmodules, Severity: diag.SeverityOK,
			Summary:  "no slot is held back by unsaved submodule work",
			Messages: diag.FindingMessages{Summary: i18n.Message{ID: "diag.submodule.none"}},
		}}
	}
	findings := make([]diag.Finding, 0, len(slots))
	for _, slot := range slots {
		findings = append(findings, unsavedSubmoduleFinding(slot))
	}
	return findings
}

func unsavedSubmoduleFinding(slot state.ProtectedSlot) diag.Finding {
	details := make([]string, 0, len(slot.Submodules))
	for _, entry := range slot.Submodules {
		details = append(details, unsavedSubmoduleDetail(entry))
	}
	return diag.Finding{
		Check: diag.CheckUnsavedSubmodules, Severity: diag.SeverityProblem,
		Summary: "a slot holds submodule work that its recovery snapshot does not contain",
		Target:  slot.Path,
		Cause:   "the recovery snapshot saves each submodule it can, and " + strconv.Itoa(len(slot.Submodules)) + " submodule(s) of this slot hold work it cannot save",
		Action:  "commit and push that work from inside the submodule, or copy it out of the slot directory yourself; wx keeps this slot out of automatic reclamation until you delete it with wx clear --discard, but the recovery snapshot still expires on its own retention, and resuming that session stops working once it does",
		Details: details,
		Messages: diag.FindingMessages{
			Summary: i18n.Message{ID: "diag.submodule.unsaved"},
			Cause:   i18n.Message{ID: "diag.submodule.unsaved_cause", Data: map[string]any{"Count": len(slot.Submodules)}},
			Action:  i18n.Message{ID: "diag.submodule.unsaved_action"},
		},
	}
}

// unsavedSubmoduleDetail は 1 submodule 分の対象と理由コードを 1 行にする。
// path と理由コードは動的な値なので置換表へは載せず、原文のまま表示する。
func unsavedSubmoduleDetail(entry state.UnsavedSubmodule) string {
	target := entry.Path
	if target == "" {
		// 列挙自体ができず、どの submodule かを特定できなかった記録である。
		target = "repository " + entry.RepositoryID
	}
	reasons := state.UnsavedSubmoduleReasons(entry.Reasons)
	if len(reasons) == 0 {
		return target
	}
	return target + " (" + strings.Join(reasons, " ") + ")"
}
