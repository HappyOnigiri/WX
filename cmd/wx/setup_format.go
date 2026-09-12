package main

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"

	"github.com/HappyOnigiri/WX/internal/i18n"
	"github.com/HappyOnigiri/WX/internal/setup"
)

// setupColumns は wx setup の表の列と並びで、最後の DETAIL は行末なので幅を持たない。
var setupColumns = []struct {
	title string
	min   int
}{
	{title: "ITEM", min: 14},
	{title: "STATE", min: 14},
	{title: "ACTION", min: 8},
	{title: "DETAIL"},
}

// printSetupTable は項目の状態を表で出す。
// printDisplay は配列 of map を steps[0].id のように展開するため使わない。
func printSetupTable(w io.Writer, steps []setup.Step) {
	widths := make([]int, len(setupColumns))
	titles := make([]string, len(setupColumns))
	lang := localizedUsageLanguage()
	for index, column := range setupColumns {
		titles[index] = localizeSetupColumn(column.title, lang)
		widths[index] = max(utf8.RuneCountInString(column.title), column.min)
	}
	rows := [][]string{titles}
	for _, step := range steps {
		step = localizeSetupStep(step, lang)
		rows = append(rows, []string{step.ID, string(step.State), string(step.Default), setupStepDetail(step)})
	}
	for _, row := range rows[1:] {
		for index, cell := range row {
			widths[index] = max(widths[index], utf8.RuneCountInString(cell))
		}
	}
	for _, row := range rows {
		var line strings.Builder
		for index, cell := range row {
			if index > 0 {
				line.WriteByte(' ')
			}
			if index == len(row)-1 {
				line.WriteString(cell)
				continue
			}
			_, _ = fmt.Fprintf(&line, "%-*s", widths[index], cell)
		}
		// 表示は stdout に出す。書込み失敗は対処できず、command の終了コードも変えない。
		_, _ = fmt.Fprintln(w, strings.TrimRight(line.String(), " "))
	}
}

// setupStepDetail は表の最終列に出す 1 行で、理由があれば先に見せる。
// absent だけは例外とする。未登録の理由は event ごとの欠落の列挙になり、何を書くのかの説明の方が役に立つ。
func setupStepDetail(step setup.Step) string {
	if len(step.Reasons) > 0 && step.State != setup.StateAbsent {
		return step.Reasons[0]
	}
	if step.Detail != "" {
		return step.Detail
	}
	return step.Target
}

func localizeSetupColumn(value string, lang i18n.Language) string {
	if lang != i18n.Japanese {
		return value
	}
	switch value {
	case "ITEM":
		return "項目"
	case "STATE":
		return "状態"
	case "ACTION":
		return "操作"
	case "DETAIL":
		return "詳細"
	default:
		return value
	}
}

// localizeSetupStep は表示用のラベルだけを置き換え、ID・state・action と
// path や外部エラーを含む可変値はそのまま残す。JSON 出力はこの関数を通さない。
func localizeSetupStep(step setup.Step, lang i18n.Language) setup.Step {
	if lang != i18n.Japanese {
		return step
	}
	switch step.ID {
	case "prerequisites":
		step.Title = "前提条件"
	case "worktree_root":
		step.Title = "Worktree root"
	case "shell_path":
		step.Title = "Shell の PATH"
	case "hooks.claude":
		step.Title = "Agent hook (claude)"
	case "hooks.codex":
		step.Title = "Agent hook (codex)"
	case "launch_agent":
		step.Title = "LaunchAgent"
	case "daemon":
		step.Title = "Daemon"
	}
	return step
}

// setupStepDescription は TUI の見出しの下に出す 1 行で、何が変わるのかを示す。
func setupStepDescription(step setup.Step) string {
	parts := make([]string, 0, 3)
	if step.Target != "" {
		parts = append(parts, step.Target)
	}
	parts = append(parts, "state: "+string(step.State))
	if len(step.Reasons) > 0 {
		parts = append(parts, step.Reasons[0])
	}
	return strings.Join(parts, " · ")
}

// setupActionDescription は選択肢ごとに、実際に書き込む path と内容の要約を出す。
// chezmoi などの置き換えという性質上、何が変わるか示さないと選べない。
func setupActionDescription(step setup.Step, action setup.Action) string {
	lang := localizedUsageLanguage()
	if step.ID == "language" {
		if action == setup.Action(i18n.English) {
			return "wx の表示を英語にする"
		}
		if action == setup.Action(i18n.Japanese) {
			return "wx の表示を日本語にする"
		}
	}
	if step.ID == "daemon" {
		if description := setupDaemonActionDescription(action); description != "" {
			return description
		}
	}
	target := step.Target
	if target == "" {
		target = "the wx configuration"
	}
	switch action {
	case setup.ActionInstall:
		if lang == i18n.Japanese {
			return summarizeSetupChange(step) + " を " + target + " に書き込みます"
		}
		return "write " + summarizeSetupChange(step) + " to " + target
	case setup.ActionUpdate:
		if lang == i18n.Japanese {
			return target + " の wx 部分を " + summarizeSetupChange(step) + " に更新します"
		}
		return "replace the wx part of " + target + " with " + summarizeSetupChange(step)
	case setup.ActionKeep:
		if lang == i18n.Japanese {
			return target + " をそのままにします"
		}
		return "leave " + target + " as it is"
	case setup.ActionRemove:
		if lang == i18n.Japanese {
			return target + " から wx の管理部分を削除します"
		}
		return "remove what wx manages from " + target
	case setup.ActionSkip:
		if lang == i18n.Japanese {
			return "今回は何もしません。後で wx setup を再実行できます"
		}
		return "do nothing now; wx setup can be run again later"
	// start と restart は daemon だけの操作で、文言は setupDaemonActionDescription が先に返す。
	case setup.ActionStart, setup.ActionRestart:
		return ""
	case setup.ActionDefault:
		if lang == i18n.Japanese {
			return summarizeSetupChange(step) + " を " + target + " に書き込みます"
		}
		return "write " + summarizeSetupChange(step) + " to " + target
	case setup.ActionManual:
		if lang == i18n.Japanese {
			return target + " に書き込む別の path を入力します"
		}
		return "type another path to write to " + target
	default:
		return ""
	}
}

// setupDaemonActionDescription は daemon だけの文言を返す。扱わない操作には空を返し、共通の文型に任せる。
// daemon は設定ファイルへ何も書かないため、書き込みの文型を当てると config.yaml を変えると誤解させる。
// 起動は launchd.Start（-k なしの kickstart）なので、稼働中の daemon は終了させない。
func setupDaemonActionDescription(action setup.Action) string {
	lang := localizedUsageLanguage()
	switch action {
	case setup.ActionStart:
		if lang == i18n.Japanese {
			return "wx daemon を起動し、local socket の応答を待ちます"
		}
		return "start the wx daemon and wait for the local socket to answer"
	case setup.ActionRestart:
		if lang == i18n.Japanese {
			return "daemon が idle になってから再起動し、置き換わった daemon の応答を待ちます"
		}
		return "ask the daemon to restart once it is idle, then wait for the replacement to answer"
	case setup.ActionKeep:
		if lang == i18n.Japanese {
			return "実行中の daemon をそのままにします"
		}
		return "leave the running daemon as it is"
	// install・update・remove は daemon に出ず、default と manual は値の入力を伴う項目だけのものである。
	case setup.ActionInstall, setup.ActionUpdate, setup.ActionRemove, setup.ActionSkip, setup.ActionDefault, setup.ActionManual:
		return ""
	default:
		return ""
	}
}

func summarizeSetupChange(step setup.Step) string {
	if step.Detail != "" && step.Desired == "" {
		return step.Detail
	}
	if step.Desired != "" {
		return step.Desired
	}
	return "the wx entries"
}

func printSetupSkipped(w io.Writer, step setup.Step) {
	step = localizeSetupStep(step, localizedUsageLanguage())
	line := fmt.Sprintf("%-14s %-14s %s", step.ID, step.State, setupStepDetail(step))
	_, _ = fmt.Fprintln(w, strings.TrimRight(line, " "))
	for _, reason := range step.Reasons[min(1, len(step.Reasons)):] {
		_, _ = fmt.Fprintln(w, "               "+reason)
	}
}

func printSetupApplied(w io.Writer, step setup.Step, action setup.Action) {
	_, _ = fmt.Fprintf(w, "%-14s %s\n", step.ID, action)
}

// printSetupNote は適用の補足を字下げして出す。差分から読み取れない事実だけを持つので、空なら何も出さない。
func printSetupNote(w io.Writer, note string) {
	if note == "" {
		return
	}
	_, _ = fmt.Fprintf(w, "%-14s %s\n", "", note)
}

// printSetupWarnings は適用後に期待した状態にならなかった項目を警告として残す。フローは止めない。
func printSetupWarnings(w io.Writer, id string, action setup.Action, applied setup.Step) {
	lang := localizedUsageLanguage()
	prefix := "warning: "
	if lang == i18n.Japanese {
		prefix = "警告: "
	}
	switch {
	case action == setup.ActionRemove && applied.State != setup.StateAbsent && applied.State != setup.StateNotApplicable:
		if lang == i18n.Japanese {
			_, _ = fmt.Fprintf(w, "警告: %s は remove 後も %s です\n", id, applied.State)
		} else {
			_, _ = fmt.Fprintf(w, "%s%s is still %s after remove\n", prefix, id, applied.State)
		}
	case action != setup.ActionRemove && applied.State != setup.StatePresent:
		if lang == i18n.Japanese {
			_, _ = fmt.Fprintf(w, "警告: %s は %s 後も %s です\n", id, action, applied.State)
		} else {
			_, _ = fmt.Fprintf(w, "%s%s is %s after %s\n", prefix, id, applied.State, action)
		}
	default:
		return
	}
	for _, reason := range applied.Reasons {
		_, _ = fmt.Fprintln(w, prefix+"  "+reason)
	}
}

// setupLeftoverPrefix は --remove が消さなかった path を示す行頭で、uninstall.sh が読み取る唯一の目印である。
// path には空白が入り得るため（`~/Library/Application Support/wx`）、読み手は 2 列目以降を行末まで取る。
const setupLeftoverPrefix = "leftover"

// printSetupRemoval は削除結果を項目ごとに 1 行で出し、消さなかった path を最後にまとめる。
// 失敗は stderr に出し、成功した項目の行は stdout に残す。片付けの続きを利用者が判断できるようにするためである。
func printSetupRemoval(out, errOut io.Writer, removal setup.Removal) {
	lang := localizedUsageLanguage()
	for _, result := range removal.Results {
		if result.Err != nil {
			prefix := "error: "
			if lang == i18n.Japanese {
				prefix = "エラー: "
			}
			_, _ = fmt.Fprintf(errOut, "%s%s: %v\n", prefix, result.ID, result.Err)
			continue
		}
		note := result.Note
		if note == "" {
			note = "nothing to remove"
			if lang == i18n.Japanese {
				note = "削除するものはありません"
			}
		}
		_, _ = fmt.Fprintf(out, "%-14s %s\n", result.ID, note)
	}
	if len(removal.Leftovers) == 0 {
		return
	}
	_, _ = fmt.Fprintln(out, "")
	if lang == i18n.Japanese {
		_, _ = fmt.Fprintln(out, "wx は保存済みの作業と記録を含む次の path を残しました:")
	} else {
		_, _ = fmt.Fprintln(out, "wx kept these; they hold saved work and records:")
	}
	for _, path := range removal.Leftovers {
		_, _ = fmt.Fprintf(out, "%-14s %s\n", setupLeftoverPrefix, path)
	}
}

// setupPayload は --check --json の出力形状である。
// state.JSONSchemaVersion は wx status / wx doctor の互換契約なので、ここには載せない。
type setupPayload struct {
	Pending bool            `json:"pending"`
	Steps   []setupStepJSON `json:"steps"`
}

type setupStepJSON struct {
	ID      string   `json:"id"`
	Title   string   `json:"title"`
	State   string   `json:"state"`
	Detail  string   `json:"detail,omitempty"`
	Reasons []string `json:"reasons,omitempty"`
	Target  string   `json:"target,omitempty"`
	Current string   `json:"current,omitempty"`
	Desired string   `json:"desired,omitempty"`
	Options []string `json:"options,omitempty"`
	Default string   `json:"default"`
}

func writeSetupJSON(w io.Writer, steps []setup.Step) error {
	payload := setupPayload{Pending: setup.Pending(steps)}
	for _, step := range steps {
		options := make([]string, 0, len(step.Options))
		for _, option := range step.Options {
			options = append(options, string(option))
		}
		payload.Steps = append(payload.Steps, setupStepJSON{
			ID: step.ID, Title: step.Title, State: string(step.State), Detail: step.Detail,
			Reasons: step.Reasons, Target: step.Target, Current: step.Current, Desired: step.Desired,
			Options: options, Default: string(step.Default),
		})
	}
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	return encoder.Encode(payload)
}
