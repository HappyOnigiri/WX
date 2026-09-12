package dashboard

import (
	"context"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/HappyOnigiri/WX/internal/config"
)

func TestModelBuildsActionWithExplicitWorkspace(t *testing.T) {
	m := newModel(context.Background(), Options{CWD: "/fallback", Config: config.Defaults()})
	m.tab = 1
	updated, _ := m.Update(key(tea.KeyEnter))
	m = updated.(model)
	if m.mode != modeChoice {
		t.Fatalf("mode=%v, want workspace choice", m.mode)
	}
	updated, _ = m.Update(key(tea.KeyEnter))
	m = updated.(model)
	if m.mode != modeInput {
		t.Fatalf("mode=%v, want input", m.mode)
	}
	m.input = "/tmp/project"
	updated, _ = m.Update(key(tea.KeyEnter))
	m = updated.(model)
	if m.mode != modeChoice || m.inputStage != "arguments-choice" {
		t.Fatalf("mode=%v stage=%q, want argument choice", m.mode, m.inputStage)
	}
	updated, _ = m.Update(key(tea.KeyDown))
	m = updated.(model)
	updated, _ = m.Update(key(tea.KeyEnter))
	m = updated.(model)
	if m.mode != modeInput || m.inputStage != "arguments" {
		t.Fatalf("mode=%v stage=%q, want custom argument input", m.mode, m.inputStage)
	}
	m.input = "--dangerously-skip-permissions"
	updated, _ = m.Update(key(tea.KeyEnter))
	m = updated.(model)
	updated, _ = m.Update(key(tea.KeyEnter))
	m = updated.(model)
	if m.result.WorkDir != "/tmp/project" {
		t.Fatalf("workdir=%q", m.result.WorkDir)
	}
	if got := m.result.Args; len(got) != 2 || got[0] != "claude" || got[1] != "--dangerously-skip-permissions" {
		t.Fatalf("args=%v", got)
	}
}

func TestOptionalArgumentsAlwaysStartWithChoices(t *testing.T) {
	for tab, items := range tabMenus {
		for _, item := range items {
			if item.inputLabel != "" && !item.inputNeeded && len(item.argumentChoices) == 0 {
				t.Errorf("tab %d item %q opens unrestricted optional input", tab, item.label)
			}
		}
	}
}

func TestReleaseChoosesWhetherToDiscardAfterSessionInput(t *testing.T) {
	m := newModel(context.Background(), Options{Config: config.Defaults()})
	m.tab, m.selected = 4, 4
	updated, _ := m.Update(key(tea.KeyEnter))
	m = updated.(model)
	if m.mode != modeInput || m.inputStage != "target-value" {
		t.Fatalf("release mode=%v stage=%q, want session input", m.mode, m.inputStage)
	}
	m.input = "session-1"
	updated, _ = m.Update(key(tea.KeyEnter))
	m = updated.(model)
	if m.mode != modeChoice || m.inputStage != "arguments-choice" || len(m.choices) != 2 {
		t.Fatalf("release choices=%+v mode=%v stage=%q", m.choices, m.mode, m.inputStage)
	}
	m.choice = 1
	updated, _ = m.Update(key(tea.KeyEnter))
	m = updated.(model)
	m.finishPending()
	if got := m.result.Args; len(got) != 3 || got[0] != "release" || got[1] != "session-1" || got[2] != "--discard" {
		t.Fatalf("release action=%v", got)
	}
}

func TestMaintenanceTargetsAndModesStartWithChoices(t *testing.T) {
	tests := []struct {
		selected int
		stage    string
	}{
		{selected: 0, stage: "arguments-choice"},
		{selected: 3, stage: "target-choice"},
		{selected: 5, stage: "target-choice"},
	}
	for _, test := range tests {
		m := newModel(context.Background(), Options{Config: config.Defaults()})
		m.tab, m.selected = 4, test.selected
		updated, _ := m.Update(key(tea.KeyEnter))
		m = updated.(model)
		if m.mode != modeChoice || m.inputStage != test.stage {
			t.Errorf("item %d mode=%v stage=%q, want choice stage %q", test.selected, m.mode, m.inputStage, test.stage)
		}
	}
}

func TestModelKeepsLastStatusWhenRefreshFails(t *testing.T) {
	m := newModel(context.Background(), Options{Config: config.Defaults()})
	updated, _ := m.Update(statusMsg{text: "Daemon running", at: testTime()})
	m = updated.(model)
	updated, _ = m.Update(statusMsg{err: context.DeadlineExceeded, at: testTime()})
	m = updated.(model)
	if m.status != "Daemon running" || m.statusErr == "" {
		t.Fatalf("status=%q err=%q", m.status, m.statusErr)
	}
}

func TestStatusArrowKeysDoNotScroll(t *testing.T) {
	m := newModel(context.Background(), Options{Config: config.Defaults()})
	m.status = strings.Repeat("line\n", 30)
	m.offset = 4
	updated, _ := m.Update(key(tea.KeyDown))
	if got := updated.(model).offset; got != 4 {
		t.Fatalf("status offset=%d, want unchanged", got)
	}
}

func TestLeftAndRightChangeTabs(t *testing.T) {
	m := newModel(context.Background(), Options{Config: config.Defaults()})
	updated, _ := m.Update(key(tea.KeyRight))
	m = updated.(model)
	if m.tab != 1 {
		t.Fatalf("right tab=%d, want 1", m.tab)
	}
	updated, _ = m.Update(key(tea.KeyLeft))
	if got := updated.(model).tab; got != 0 {
		t.Fatalf("left tab=%d, want 0", got)
	}
}

func TestSettingsNavigateFromEnvironmentToChoice(t *testing.T) {
	cfg := config.Defaults()
	cfg.Workspaces["/tmp/workspace-one"] = config.Workspace{}
	m := newModel(context.Background(), Options{Config: cfg})
	m.tab = 2
	if got := m.currentLabels(); len(got) != 2 || got[0] != "Global" || got[1] != "Workspace  workspace-one" {
		t.Fatalf("environments=%v", got)
	}
	m.selected = 1
	updated, _ := m.Update(key(tea.KeyEnter))
	m = updated.(model)
	if !m.settingsOpen || m.settingsEnv != 1 || len(m.configItems()) == 0 {
		t.Fatalf("settings state=%+v", m)
	}
	updated, _ = m.Update(key(tea.KeyEnter))
	m = updated.(model)
	if m.mode != modeChoice {
		t.Fatalf("mode=%v, want choice", m.mode)
	}
	foundReset := false
	for _, option := range m.choices {
		foundReset = foundReset || option.op == config.EditReset
	}
	if !foundReset {
		t.Fatal("configuration choices omitted Reset to default")
	}
}

func TestInlineOperationKeepsTheDashboardOpenAndShowsResult(t *testing.T) {
	m := newModel(context.Background(), Options{
		Config:  config.Defaults(),
		Execute: func(context.Context, Action) (string, int) { return "diagnostics complete", 1 },
	})
	m.tab = 3
	updated, _ := m.Update(key(tea.KeyEnter))
	m = updated.(model)
	updated, cmd := m.Update(key(tea.KeyEnter))
	m = updated.(model)
	if cmd == nil || m.mode != modeRunning {
		t.Fatalf("mode=%v cmd=%v", m.mode, cmd)
	}
	updated, _ = m.Update(cmd())
	m = updated.(model)
	if m.mode != modeResult || m.resultText != "diagnostics complete" || m.resultCode != 1 {
		t.Fatalf("result state=%+v", m)
	}
}

func TestResultScrollsOnlyWhenOutputExceedsTheScreen(t *testing.T) {
	m := newModel(context.Background(), Options{Config: config.Defaults()})
	m.mode, m.height, m.resultText = modeResult, 20, "first\nsecond"
	updated, _ := m.Update(key(tea.KeyDown))
	m = updated.(model)
	if m.offset != 0 {
		t.Fatalf("short result offset=%d, want 0", m.offset)
	}
	m.resultText = strings.Repeat("line\n", 20) + "last"
	updated, _ = m.Update(key(tea.KeyDown))
	m = updated.(model)
	if m.offset != 1 {
		t.Fatalf("long result offset=%d, want 1", m.offset)
	}
	for range 30 {
		updated, _ = m.Update(key(tea.KeyDown))
		m = updated.(model)
	}
	if m.offset != m.maxResultOffset() {
		t.Fatalf("result offset=%d, max=%d", m.offset, m.maxResultOffset())
	}
}

func key(code rune) tea.KeyPressMsg { return tea.KeyPressMsg{Code: code} }
