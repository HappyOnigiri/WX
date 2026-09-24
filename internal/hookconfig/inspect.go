package hookconfig

import (
	"encoding/json"
	"fmt"

	"github.com/HappyOnigiri/WorktreeX/internal/i18n"
)

// documentReport は 1 つの設定ファイルに対する検査結果である。
// blocked はドキュメント全体が却下される事情（parse 不能、disableAllHooks）を示す。
type documentReport struct {
	blocked  bool
	matched  map[string]bool
	findings []Finding
}

// inspectDocument は読み側の受理規則で、event ごとの一致と理由を集める。path は表示する理由の対象として使うだけで、判定には関わらない。
// 受理判定はこの関数だけが持ち、Available も Inspect 経由でここへ辿り着く。
func inspectDocument(path string, data []byte, events map[string]string, executable string) documentReport {
	report := documentReport{matched: map[string]bool{}}
	var document map[string]json.RawMessage
	if err := decodeJSON(data, &document); err != nil {
		report.blocked = true
		report.findings = append(report.findings, Finding{
			Code: FindingTargetUnparsable, Detail: err.Error(), Blocking: true,
			Message: i18n.Message{ID: "hook.finding.target_unparsable", Data: map[string]any{"Path": path, "Detail": err.Error()}},
		})
		return report
	}
	if disabled, valid := boolOption(document["disableAllHooks"]); !valid || disabled {
		report.blocked = true
		report.findings = append(report.findings, Finding{
			Code: FindingAllHooksDisabled, Detail: "disableAllHooks rejects every hook in this file", Blocking: true,
			Message: i18n.Message{ID: "hook.finding.all_hooks_disabled"},
		})
		return report
	}
	hooksRaw, ok := document["hooks"]
	if !ok {
		report.findings = append(report.findings, Finding{
			Code: FindingHooksMissing, Detail: "the file has no hooks object",
			Message: i18n.Message{ID: "hook.finding.hooks_missing"},
		})
		return report
	}
	var hooks map[string]json.RawMessage
	if err := decodeJSON(hooksRaw, &hooks); err != nil {
		report.blocked = true
		report.findings = append(report.findings, Finding{
			Code: FindingTargetUnparsable, Detail: "hooks: " + err.Error(), Blocking: true,
			Message: i18n.Message{ID: "hook.finding.target_unparsable", Data: map[string]any{"Path": path, "Detail": "hooks: " + err.Error()}},
		})
		return report
	}
	for event, command := range events {
		eventRaw, ok := hooks[event]
		if !ok {
			report.findings = append(report.findings, Finding{
				Code: FindingEventMissing, Event: event,
				Message: i18n.Message{ID: "hook.finding.event_missing", Data: map[string]any{"Event": event}},
			})
			continue
		}
		var groups []readinessHookGroup
		if err := decodeStrictJSON(eventRaw, &groups); err != nil {
			// 必須 event の配列だけが strict decode の対象で、未知フィールド 1 つで event 全体が却下される。
			// 原因のフィールド名を残さないと、agent 側の追加項目で壊れたときに手掛かりが無くなる。
			report.findings = append(report.findings, Finding{
				Code: FindingEventUnknownField, Event: event, Detail: err.Error(),
				Message: i18n.Message{ID: "hook.finding.event_unknown_field", Data: map[string]any{"Event": event, "Detail": err.Error()}},
			})
			continue
		}
		matched, findings := inspectEvent(groups, command, event, executable)
		report.matched[event] = matched
		if !matched && len(findings) == 0 {
			// 4 token の command のように、wx のエントリとして識別すらされない形は他の finding に現れない。
			// 理由なく divergent と表示されるのを防ぐため、ここで欠落そのものを残す。
			findings = []Finding{{
				Code: FindingCommandMissing, Event: event, Detail: "no hook runs exactly `<wx> hook " + command + "`",
				Message: i18n.Message{ID: "hook.finding.command_missing", Data: map[string]any{"Event": event, "Command": "<wx> hook " + command}},
			}}
		}
		report.findings = append(report.findings, findings...)
	}
	return report
}

// inspectEvent は 1 つの event の group 列を調べ、受理できる wx hook があるかと理由を返す。
// 不正な group を early-continue で読み飛ばす挙動は読み側と同一で、その理由は非 blocking の診断にとどめる。
func inspectEvent(groups []readinessHookGroup, command, event, executable string) (bool, []Finding) {
	var findings []Finding
	for _, group := range groups {
		valid, groupFindings := inspectGroup(group)
		if !valid {
			findings = append(findings, groupFindings...)
			continue
		}
		for _, hook := range group.Hooks {
			if hook.Type != "command" {
				continue
			}
			if skipped, reason, messageID := hookSkipReason(hook); skipped {
				if wxHookSubcommand(hook.Command) == command {
					findings = append(findings, Finding{
						Code: FindingCommandSkipped, Event: event, Detail: reason,
						Message: i18n.Message{ID: messageID, Data: map[string]any{"Event": event}},
					})
				}
				continue
			}
			if isExactWXHookCommandForExecutable(hook.Command, command, executable) {
				return true, findings
			}
			if wxHookSubcommand(hook.Command) == command {
				detail, message := otherBinaryReason(hook.Command, executable, event)
				findings = append(findings, Finding{Code: FindingCommandOtherBinary, Event: event, Detail: detail, Message: message})
			}
		}
	}
	return false, findings
}

// hookSkipReason は hook 単位で読み側が読み飛ばす条件（disabled・async・once）を、
// 機械向けの理由と表示用の message ID の両方で返す。
func hookSkipReason(hook readinessHookCommand) (bool, string, string) {
	if disabled, _ := boolOption(hook.Disabled); disabled {
		return true, "the hook is disabled", "hook.finding.command_skipped_disabled"
	}
	if async, _ := boolOption(hook.Async); async {
		return true, "async hooks do not gate the agent, so readiness is not enforced", "hook.finding.command_skipped_async"
	}
	if once, _ := boolOption(hook.Once); once {
		return true, "once hooks do not run for every event", "hook.finding.command_skipped_once"
	}
	return false, "", ""
}

// otherBinaryReason は wx の hook command が別の実体へ解決したときに、両方の path を残す。
func otherBinaryReason(command, executable, event string) (string, i18n.Message) {
	fields, ok := splitHookCommand(command)
	if !ok || len(fields) == 0 {
		return "the command does not name a wx executable; expected " + executable,
			i18n.Message{ID: "hook.finding.command_not_wx", Data: map[string]any{"Event": event, "Expected": executable}}
	}
	resolved, ok := resolveHookExecutable(fields[0].value)
	if !ok {
		return fmt.Sprintf("%s cannot be resolved to an executable; expected %s", fields[0].value, executable),
			i18n.Message{ID: "hook.finding.command_unresolvable", Data: map[string]any{"Event": event, "Actual": fields[0].value, "Expected": executable}}
	}
	return fmt.Sprintf("%s resolves to %s, not the running wx at %s", fields[0].value, resolved, executable),
		i18n.Message{ID: "hook.finding.command_other_binary", Data: map[string]any{"Event": event, "Actual": resolved, "Expected": executable}}
}

// inspectGroup は group が読み側に受理される形かを、理由付きで返す。
func inspectGroup(group readinessHookGroup) (bool, []Finding) {
	if disabled, valid := boolOption(group.Disabled); !valid || disabled {
		return false, []Finding{{
			Code: FindingGroupRejected, Detail: "the group sets disabled",
			Message: i18n.Message{ID: "hook.finding.group_disabled"},
		}}
	}
	if !matcherAppliesToEveryEvent(group.Matcher) {
		return false, []Finding{{
			Code: FindingGroupRejected, Detail: "the group matcher does not apply to every event; omit matcher or use \"*\"",
			Message: i18n.Message{ID: "hook.finding.group_matcher"},
		}}
	}
	if len(group.Hooks) == 0 {
		return false, []Finding{{
			Code: FindingGroupRejected, Detail: "the group has no hooks",
			Message: i18n.Message{ID: "hook.finding.group_empty"},
		}}
	}
	for _, hook := range group.Hooks {
		if !inspectCommand(hook) {
			// 同一 group 内に 1 つでも形の不正な hook があると group 丸ごと却下される。
			// wx が常に専用 group を書くのはこのためである。
			return false, []Finding{{
				Code: FindingGroupRejected, Detail: "another hook in the same group is malformed, so the whole group is rejected",
				Message: i18n.Message{ID: "hook.finding.group_malformed_peer"},
			}}
		}
	}
	return true, nil
}

// inspectCommand は hook 1 件が読み側の形式要件を満たすかを返す。
func inspectCommand(hook readinessHookCommand) bool {
	if hook.Type != "command" && hook.Type != "prompt" && hook.Type != "agent" {
		return false
	}
	if hook.Type == "command" && hook.Command == "" {
		return false
	}
	for _, raw := range []json.RawMessage{hook.Disabled, hook.Async, hook.Once} {
		if _, valid := boolOption(raw); !valid {
			return false
		}
	}
	return optionalNumberValid(hook.Timeout) && optionalStringValid(hook.StatusMessage) && optionalIntegerValid(hook.AdditionalContextLimit)
}

// wxHookSubcommand は command が `<executable> hook <subcommand>` の形なら subcommand を返す。
// wx の登録済みエントリを見分けるための構文判定で、実体の解決は行わない。
func wxHookSubcommand(command string) string {
	fields, ok := splitHookCommand(command)
	if !ok || len(fields) != 3 || fields[1].value != "hook" {
		return ""
	}
	for _, event := range Events() {
		if fields[2].value == event.Subcommand {
			return event.Subcommand
		}
	}
	return ""
}
