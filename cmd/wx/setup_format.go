package main

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	xansi "github.com/charmbracelet/x/ansi"

	"github.com/HappyOnigiri/WX/internal/i18n"
	"github.com/HappyOnigiri/WX/internal/setup"
)

// setupColumns は wx setup の表の列と並びで、最後の DETAIL は行末なので幅を持たない。
var setupColumns = []struct {
	id  string
	min int
}{
	{id: "setup.column.item", min: 14},
	{id: "setup.column.state", min: 14},
	{id: "setup.column.action", min: 8},
	{id: "setup.column.detail"},
}

// printSetupTable は項目の状態を表で出す。
// printDisplay は配列 of map を steps[0].id のように展開するため使わない。
// 幅は訳した見出しと値の表示幅から決める。英語の幅で桁を決めてから訳を入れると列がずれる。
func printSetupTable(w io.Writer, lang i18n.Language, steps []setup.Step) {
	loc := i18n.New(string(lang))
	widths := make([]int, len(setupColumns))
	titles := make([]string, len(setupColumns))
	for index, column := range setupColumns {
		titles[index] = loc.Localize(column.id, nil)
		widths[index] = max(xansi.StringWidth(titles[index]), column.min)
	}
	rows := [][]string{titles}
	for _, step := range steps {
		step = localizeSetupStep(step, loc)
		rows = append(rows, []string{step.ID, string(step.State), string(step.Default), setupStepDetail(step)})
	}
	for _, row := range rows[1:] {
		for index, cell := range row {
			widths[index] = max(widths[index], xansi.StringWidth(cell))
		}
	}
	for _, row := range rows {
		var line strings.Builder
		for index, cell := range row {
			if index > 0 {
				line.WriteByte(' ')
			}
			line.WriteString(cell)
			if index == len(row)-1 {
				continue
			}
			if pad := widths[index] - xansi.StringWidth(cell); pad > 0 {
				line.WriteString(strings.Repeat(" ", pad))
			}
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

// localizeSetupStep は表示用のラベルだけを置き換え、ID・state・action と
// path や外部エラーを含む可変値はそのまま残す。JSON 出力はこの関数を通さない。
// 表に出ない ID は setup package が付けた Title のまま残す。
func localizeSetupStep(step setup.Step, loc *i18n.Localizer) setup.Step {
	switch step.ID {
	case "prerequisites":
		step.Title = loc.Localize("setup.prerequisites", nil)
	case "worktree_root":
		step.Title = loc.Localize("setup.worktree_root", nil)
	case "shell_path":
		step.Title = loc.Localize("setup.item.shell_path", nil)
	case "hooks.claude":
		step.Title = loc.Localize("setup.item.hooks_claude", nil)
	case "hooks.codex":
		step.Title = loc.Localize("setup.item.hooks_codex", nil)
	case "launch_agent":
		step.Title = loc.Localize("setup.launch_agent", nil)
	case "daemon":
		step.Title = loc.Localize("setup.daemon", nil)
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
	loc := i18n.New(string(localizedUsageLanguage()))
	if step.ID == "language" {
		if action == setup.Action(i18n.English) {
			return loc.Localize("setup.language.english", nil)
		}
		if action == setup.Action(i18n.Japanese) {
			return loc.Localize("setup.language.japanese", nil)
		}
	}
	if step.ID == "daemon" {
		if description := setupDaemonActionDescription(loc, action); description != "" {
			return description
		}
	}
	target := step.Target
	if target == "" {
		target = "the wx configuration"
	}
	data := map[string]any{"Target": target, "Change": summarizeSetupChange(step)}
	switch action {
	// install と default はどちらも新規の書き込みなので、同じ文型で説明する。
	case setup.ActionInstall, setup.ActionDefault:
		return loc.Localize("setup.action.write", data)
	case setup.ActionUpdate:
		return loc.Localize("setup.action.replace", data)
	case setup.ActionKeep:
		return loc.Localize("setup.action.keep", data)
	case setup.ActionRemove:
		return loc.Localize("setup.action.remove", data)
	case setup.ActionSkip:
		return loc.Localize("setup.action.skip", data)
	// start と restart は daemon だけの操作で、文言は setupDaemonActionDescription が先に返す。
	case setup.ActionStart, setup.ActionRestart:
		return ""
	case setup.ActionManual:
		return loc.Localize("setup.action.manual", data)
	default:
		return ""
	}
}

// setupDaemonActionDescription は daemon だけの文言を返す。扱わない操作には空を返し、共通の文型に任せる。
// daemon は設定ファイルへ何も書かないため、書き込みの文型を当てると config.yaml を変えると誤解させる。
// 起動は launchd.Start（-k なしの kickstart）なので、稼働中の daemon は終了させない。
func setupDaemonActionDescription(loc *i18n.Localizer, action setup.Action) string {
	switch action {
	case setup.ActionStart:
		return loc.Localize("setup.daemon.start", nil)
	case setup.ActionRestart:
		return loc.Localize("setup.daemon.restart", nil)
	case setup.ActionKeep:
		return loc.Localize("setup.daemon.keep", nil)
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
	step = localizeSetupStep(step, i18n.New(string(localizedUsageLanguage())))
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
	loc := i18n.New(string(localizedUsageLanguage()))
	prefix := loc.Localize("setup.warning_prefix", nil)
	switch {
	case action == setup.ActionRemove && applied.State != setup.StateAbsent && applied.State != setup.StateNotApplicable:
		_, _ = fmt.Fprintln(w, prefix+loc.Localize("setup.still_present", map[string]any{"Item": id, "State": string(applied.State)}))
	case action != setup.ActionRemove && applied.State != setup.StatePresent:
		_, _ = fmt.Fprintln(w, prefix+loc.Localize("setup.unexpected_state", map[string]any{"Item": id, "State": string(applied.State), "Action": string(action)}))
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
	loc := i18n.New(string(localizedUsageLanguage()))
	for _, result := range removal.Results {
		if result.Err != nil {
			_, _ = fmt.Fprintf(errOut, "%s: %s: %v\n", loc.Localize("common.error", nil), result.ID, result.Err)
			continue
		}
		note := result.Note
		if note == "" {
			note = loc.Localize("setup.nothing_to_remove", nil)
		}
		_, _ = fmt.Fprintf(out, "%-14s %s\n", result.ID, note)
	}
	if len(removal.Leftovers) == 0 {
		return
	}
	_, _ = fmt.Fprintln(out, "")
	_, _ = fmt.Fprintln(out, loc.Localize("setup.kept_paths", nil))
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
