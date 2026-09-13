package main

import (
	"bytes"
	"strings"
	"testing"

	xansi "github.com/charmbracelet/x/ansi"

	"github.com/HappyOnigiri/WX/internal/i18n"
	"github.com/HappyOnigiri/WX/internal/setup"
)

func TestSetupTableKeepsColumnsAlignedAndShowsReasons(t *testing.T) {
	steps := []setup.Step{
		{ID: "worktree_root", State: setup.StatePresent, Default: setup.ActionKeep, Detail: "/home/user/wx"},
		{ID: "hooks.claude", State: setup.StateDivergent, Default: setup.ActionUpdate, Detail: "4 wx entries", Reasons: []string{"command_other_binary"}},
		{ID: "a-very-long-item-name", State: setup.StateNotApplicable, Default: setup.ActionKeep, Target: "/some/path"},
	}
	var out bytes.Buffer
	printSetupTable(&out, i18n.English, steps)
	lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	if len(lines) != len(steps)+1 {
		t.Fatalf("table rows=%d:\n%s", len(lines), out.String())
	}
	column := strings.Index(lines[0], "STATE")
	for _, line := range lines[1:] {
		if !strings.HasPrefix(line[column:], string(steps[0].State)) && line[column-1] != ' ' {
			t.Fatalf("the STATE column is not aligned:\n%s", out.String())
		}
	}
	if !strings.Contains(out.String(), "command_other_binary") {
		t.Fatalf("the reason is not shown:\n%s", out.String())
	}
	if !strings.Contains(lines[3], "/some/path") {
		t.Fatalf("the target is not used as a fallback detail:\n%s", out.String())
	}
}

// TestSetupTableKeepsColumnsAlignedInJapanese は、英語の見出し幅で桁を決めてから
// 訳を入れて列がずれる退行を防ぐ。全角の見出しでも各列の開始位置は揃う。
func TestSetupTableKeepsColumnsAlignedInJapanese(t *testing.T) {
	steps := []setup.Step{
		{ID: "worktree_root", State: setup.StatePresent, Default: setup.ActionKeep, Detail: "/home/user/wx"},
		{ID: "hooks.claude", State: setup.StateDivergent, Default: setup.ActionUpdate, Detail: "4 wx entries"},
	}
	var out bytes.Buffer
	printSetupTable(&out, i18n.Japanese, steps)
	lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	if !strings.HasPrefix(lines[0], "項目") {
		t.Fatalf("the header is not localized:\n%s", out.String())
	}
	header := strings.Index(lines[0], "状態")
	if header < 0 {
		t.Fatalf("the state column is missing:\n%s", out.String())
	}
	column := xansi.StringWidth(lines[0][:header])
	for index, line := range lines[1:] {
		at := strings.Index(line, string(steps[index].State))
		if at < 0 {
			t.Fatalf("row %d has no state:\n%s", index, out.String())
		}
		if got := xansi.StringWidth(line[:at]); got != column {
			t.Fatalf("the state column starts at %d, want %d:\n%s", got, column, out.String())
		}
	}
}

func TestSetupActionDescriptionsNameWhatChanges(t *testing.T) {
	// 表示言語は設定から読むため、英語の表示を検査するテストは空のホームを見る。
	t.Setenv("HOME", t.TempDir())
	step := setup.Step{ID: "hooks.codex", Target: "/home/user/.codex/hooks.json", Desired: "/home/user/.local/bin/wx", State: setup.StateDivergent}
	for action, want := range map[setup.Action]string{
		setup.ActionInstall: "/home/user/.codex/hooks.json",
		setup.ActionUpdate:  "/home/user/.local/bin/wx",
		setup.ActionKeep:    "leave",
		setup.ActionRemove:  "remove what wx manages",
		setup.ActionSkip:    "do nothing now",
		setup.ActionDefault: "write /home/user/.local/bin/wx to /home/user/.codex/hooks.json",
		setup.ActionManual:  "type another path to write to /home/user/.codex/hooks.json",
	} {
		if got := setupActionDescription(step, action); !strings.Contains(got, want) {
			t.Fatalf("%s description=%q, want it to mention %q", action, got, want)
		}
	}
	if got := setupActionDescription(setup.Step{Detail: "the daemon"}, setup.ActionInstall); !strings.Contains(got, "the wx configuration") {
		t.Fatalf("a step without a target=%q", got)
	}
	// daemon は設定を書かないので、書き込みの文型を当てない。
	daemon := setup.Step{ID: "daemon", Detail: "wx daemon answers the local socket", State: setup.StateAbsent}
	for action, want := range map[setup.Action]string{
		setup.ActionStart:   "start the wx daemon and wait",
		setup.ActionRestart: "ask the daemon to restart once it is idle",
		setup.ActionKeep:    "leave the running daemon as it is",
		setup.ActionSkip:    "do nothing now",
	} {
		got := setupActionDescription(daemon, action)
		if !strings.Contains(got, want) {
			t.Fatalf("daemon %s description=%q, want it to mention %q", action, got, want)
		}
		if strings.Contains(got, "the wx configuration") {
			t.Fatalf("daemon %s description reads as a write to the configuration: %q", action, got)
		}
	}
	if got := setupActionDescription(step, setup.Action("nonsense")); got != "" {
		t.Fatalf("an unknown action described itself: %q", got)
	}
	if got := setupStepDescription(setup.Step{State: setup.StateUnknown, Reasons: []string{"why"}}); !strings.Contains(got, "why") {
		t.Fatalf("description=%q", got)
	}
	if got := summarizeSetupChange(i18n.New(string(i18n.English)), setup.Step{}); got != "the wx entries" {
		t.Fatalf("summary=%q", got)
	}
}

func TestSetupWarningsReportStatesThatDidNotSettle(t *testing.T) {
	// 表示言語は設定から読むため、英語の表示を検査するテストは空のホームを見る。
	t.Setenv("HOME", t.TempDir())
	var out bytes.Buffer
	printSetupWarnings(&out, "hooks.claude", setup.ActionInstall, setup.Step{State: setup.StateDivergent, Reasons: []string{"command_other_binary"}})
	if !strings.Contains(out.String(), "warning: hooks.claude is divergent after install") || !strings.Contains(out.String(), "command_other_binary") {
		t.Fatalf("warning=%q", out.String())
	}
	out.Reset()
	printSetupWarnings(&out, "hooks.claude", setup.ActionRemove, setup.Step{State: setup.StatePresent})
	if !strings.Contains(out.String(), "still present after remove") {
		t.Fatalf("remove warning=%q", out.String())
	}
	out.Reset()
	printSetupWarnings(&out, "hooks.claude", setup.ActionInstall, setup.Step{State: setup.StatePresent})
	printSetupWarnings(&out, "hooks.claude", setup.ActionRemove, setup.Step{State: setup.StateAbsent})
	if out.Len() != 0 {
		t.Fatalf("a settled step produced a warning: %q", out.String())
	}
	out.Reset()
	printSetupSkipped(&out, setup.Step{ID: "prerequisites", State: setup.StatePresent, Reasons: []string{"first", "second"}})
	if !strings.Contains(out.String(), "first") || !strings.Contains(out.String(), "second") {
		t.Fatalf("skipped step=%q", out.String())
	}
	out.Reset()
	printSetupApplied(&out, setup.Step{ID: "daemon"}, setup.ActionInstall)
	if !strings.Contains(out.String(), "daemon") || !strings.Contains(out.String(), "install") {
		t.Fatalf("applied line=%q", out.String())
	}
	out.Reset()
	printSetupNote(&out, "wrote /home/x/.claude/settings.json; backup at /home/x/backups/claude-settings.json")
	if !strings.Contains(out.String(), "backup at /home/x/backups/claude-settings.json") {
		t.Fatalf("note line=%q", out.String())
	}
	out.Reset()
	printSetupNote(&out, "")
	if out.String() != "" {
		t.Fatalf("an empty note produced output=%q", out.String())
	}
}
