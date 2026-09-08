package hookconfig

import (
	"encoding/json"
	"fmt"
)

// documentReport は 1 つの設定ファイルに対する検査結果である。
// blocked はドキュメント全体が却下される事情（parse 不能、disableAllHooks）を示す。
type documentReport struct {
	blocked  bool
	matched  map[string]bool
	findings []Finding
}

// inspectDocument は readinessHookDocumentMatches と同じ受理規則で、event ごとの一致と理由を集める。
// 受理判定を二重管理しないため、bool 版はこの関数のラッパーとして残す。
func inspectDocument(data []byte, events map[string]string, executable string) documentReport {
	report := documentReport{matched: map[string]bool{}}
	var document map[string]json.RawMessage
	if err := decodeJSON(data, &document); err != nil {
		report.blocked = true
		report.findings = append(report.findings, Finding{Code: FindingTargetUnparsable, Detail: err.Error(), Blocking: true})
		return report
	}
	if disabled, valid := boolOption(document["disableAllHooks"]); !valid || disabled {
		report.blocked = true
		report.findings = append(report.findings, Finding{Code: FindingAllHooksDisabled, Detail: "disableAllHooks rejects every hook in this file", Blocking: true})
		return report
	}
	hooksRaw, ok := document["hooks"]
	if !ok {
		report.findings = append(report.findings, Finding{Code: FindingHooksMissing, Detail: "the file has no hooks object"})
		return report
	}
	var hooks map[string]json.RawMessage
	if err := decodeJSON(hooksRaw, &hooks); err != nil {
		report.blocked = true
		report.findings = append(report.findings, Finding{Code: FindingTargetUnparsable, Detail: "hooks: " + err.Error(), Blocking: true})
		return report
	}
	for event, command := range events {
		eventRaw, ok := hooks[event]
		if !ok {
			report.findings = append(report.findings, Finding{Code: FindingEventMissing, Event: event})
			continue
		}
		var groups []readinessHookGroup
		if err := decodeStrictJSON(eventRaw, &groups); err != nil {
			// 必須 event の配列だけが strict decode の対象で、未知フィールド 1 つで event 全体が却下される。
			// 原因のフィールド名を残さないと、agent 側の追加項目で壊れたときに手掛かりが無くなる。
			report.findings = append(report.findings, Finding{Code: FindingEventUnknownField, Event: event, Detail: err.Error()})
			continue
		}
		matched, findings := inspectEvent(groups, command, event, executable)
		report.matched[event] = matched
		if !matched && len(findings) == 0 {
			// 4 token の command のように、wx のエントリとして識別すらされない形は他の finding に現れない。
			// 理由なく divergent と表示されるのを防ぐため、ここで欠落そのものを残す。
			findings = []Finding{{Code: FindingCommandMissing, Event: event, Detail: "no hook runs exactly `<wx> hook " + command + "`"}}
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
			if skipped, reason := hookSkipReason(hook); skipped {
				if wxHookSubcommand(hook.Command) == command {
					findings = append(findings, Finding{Code: FindingCommandSkipped, Event: event, Detail: reason})
				}
				continue
			}
			if isExactWXHookCommandForExecutable(hook.Command, command, executable) {
				return true, findings
			}
			if wxHookSubcommand(hook.Command) == command {
				findings = append(findings, Finding{Code: FindingCommandOtherBinary, Event: event, Detail: otherBinaryDetail(hook.Command, executable)})
			}
		}
	}
	return false, findings
}

// hookSkipReason は hook 単位で読み側が読み飛ばす条件（disabled・async・once）を返す。
func hookSkipReason(hook readinessHookCommand) (bool, string) {
	if disabled, _ := boolOption(hook.Disabled); disabled {
		return true, "the hook is disabled"
	}
	if async, _ := boolOption(hook.Async); async {
		return true, "async hooks do not gate the agent, so readiness is not enforced"
	}
	if once, _ := boolOption(hook.Once); once {
		return true, "once hooks do not run for every event"
	}
	return false, ""
}

// otherBinaryDetail は wx の hook command が別の実体へ解決したときに、両方の path を残す。
func otherBinaryDetail(command, executable string) string {
	fields, ok := splitHookCommand(command)
	if !ok || len(fields) == 0 {
		return "the command does not name a wx executable; expected " + executable
	}
	resolved, ok := resolveHookExecutable(fields[0].value)
	if !ok {
		return fmt.Sprintf("%s cannot be resolved to an executable; expected %s", fields[0].value, executable)
	}
	return fmt.Sprintf("%s resolves to %s, not the running wx at %s", fields[0].value, resolved, executable)
}

// inspectGroup は group が読み側に受理される形かを、理由付きで返す。
func inspectGroup(group readinessHookGroup) (bool, []Finding) {
	if disabled, valid := boolOption(group.Disabled); !valid || disabled {
		return false, []Finding{{Code: FindingGroupRejected, Detail: "the group sets disabled"}}
	}
	if !matcherAppliesToEveryEvent(group.Matcher) {
		return false, []Finding{{Code: FindingGroupRejected, Detail: "the group matcher does not apply to every event; omit matcher or use \"*\""}}
	}
	if len(group.Hooks) == 0 {
		return false, []Finding{{Code: FindingGroupRejected, Detail: "the group has no hooks"}}
	}
	for _, hook := range group.Hooks {
		if !inspectCommand(hook) {
			// 同一 group 内に 1 つでも形の不正な hook があると group 丸ごと却下される。
			// wx が常に専用 group を書くのはこのためである。
			return false, []Finding{{Code: FindingGroupRejected, Detail: "another hook in the same group is malformed, so the whole group is rejected"}}
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

func readinessHookDocumentMatches(data []byte, required map[string]string, executable string) bool {
	report := inspectDocument(data, required, executable)
	if report.blocked {
		return false
	}
	for event := range required {
		if !report.matched[event] {
			return false
		}
	}
	return true
}

func readinessHookGroupsMatch(groups []readinessHookGroup, command, event, executable string) bool {
	matched, _ := inspectEvent(groups, command, event, executable)
	return matched
}

func readinessHookGroupValid(group readinessHookGroup) bool {
	valid, _ := inspectGroup(group)
	return valid
}

func readinessHookCommandValid(hook readinessHookCommand) bool {
	return inspectCommand(hook)
}
