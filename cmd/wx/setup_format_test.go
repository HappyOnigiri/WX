package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/HappyOnigiri/WX/internal/setup"
)

func TestSetupTableKeepsColumnsAlignedAndShowsReasons(t *testing.T) {
	steps := []setup.Step{
		{ID: "worktree_root", State: setup.StatePresent, Default: setup.ActionKeep, Detail: "/home/user/wx"},
		{ID: "hooks.claude", State: setup.StateDivergent, Default: setup.ActionUpdate, Detail: "4 wx entries", Reasons: []string{"command_other_binary"}},
		{ID: "a-very-long-item-name", State: setup.StateNotApplicable, Default: setup.ActionKeep, Target: "/some/path"},
	}
	var out bytes.Buffer
	printSetupTable(&out, steps)
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

func TestSetupActionDescriptionsNameWhatChanges(t *testing.T) {
	step := setup.Step{ID: "hooks.codex", Target: "/home/user/.codex/hooks.json", Desired: "/home/user/.local/bin/wx", State: setup.StateDivergent}
	for action, want := range map[setup.Action]string{
		setup.ActionInstall: "/home/user/.codex/hooks.json",
		setup.ActionUpdate:  "/home/user/.local/bin/wx",
		setup.ActionKeep:    "leave",
		setup.ActionRemove:  "remove what wx manages",
		setup.ActionSkip:    "do nothing now",
		setup.ActionDefault: "use /home/user/.local/bin/wx",
		setup.ActionManual:  "type a value instead of /home/user/.local/bin/wx",
	} {
		if got := setupActionDescription(step, action); !strings.Contains(got, want) {
			t.Fatalf("%s description=%q, want it to mention %q", action, got, want)
		}
	}
	if got := setupActionDescription(setup.Step{Detail: "the daemon"}, setup.ActionInstall); !strings.Contains(got, "the wx configuration") {
		t.Fatalf("a step without a target=%q", got)
	}
	if got := setupActionDescription(step, setup.Action("nonsense")); got != "" {
		t.Fatalf("an unknown action described itself: %q", got)
	}
	if got := setupStepDescription(setup.Step{State: setup.StateUnknown, Reasons: []string{"why"}}); !strings.Contains(got, "why") {
		t.Fatalf("description=%q", got)
	}
	if got := summarizeSetupChange(setup.Step{}); got != "the wx entries" {
		t.Fatalf("summary=%q", got)
	}
}

func TestSetupWarningsReportStatesThatDidNotSettle(t *testing.T) {
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
