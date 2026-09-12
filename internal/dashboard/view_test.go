package dashboard

import (
	"context"
	"strings"
	"testing"
	"time"

	xansi "github.com/charmbracelet/x/ansi"

	"github.com/HappyOnigiri/WX/internal/config"
	"github.com/HappyOnigiri/WX/internal/setup"
)

func TestViewUsesStatusPaneAndResponsiveOperationLayout(t *testing.T) {
	m := newModel(context.Background(), Options{Config: config.Defaults()})
	m.loading = false
	m.status = "WORKSPACE  POLICY  READY  IN USE  LAST USED\n~/wx       hot     2      1       now"
	m.width, m.height = 100, 24
	wide := m.View().Content
	if !strings.Contains(wide, "System status") || strings.Contains(wide, " │ ") {
		t.Fatalf("status view is not a single pane: %q", wide)
	}
	m.tab, m.width = 1, 70
	narrow := m.View().Content
	if !strings.Contains(narrow, "Choose what to launch") || !strings.Contains(narrow, "selected workspace") {
		t.Fatalf("narrow operation view omitted stacked content: %q", narrow)
	}
	for _, line := range strings.Split(narrow, "\n") {
		if xansi.StringWidth(line) > 70 {
			t.Fatalf("line exceeds width: %q", line)
		}
	}
}

func TestSelectedTabUsesBackgroundInsteadOfBrackets(t *testing.T) {
	m := newModel(context.Background(), Options{Config: config.Defaults()})
	line := m.tabLine()
	if strings.Contains(line, "[Status]") {
		t.Fatalf("selected tab still uses brackets: %q", line)
	}
	if !strings.Contains(line, "\x1b[48;5;43mStatus") {
		t.Fatalf("selected tab has no background highlight: %q", line)
	}
	want := xansi.Strip(line)
	for tab := range tabNames {
		m.tab = tab
		if got := xansi.Strip(m.tabLine()); got != want {
			t.Fatalf("tab %d changed tab positions: got %q, want %q", tab, got, want)
		}
	}
}

func TestSelectedMenuItemUsesAccentColor(t *testing.T) {
	cfg := config.Defaults()
	cfg.Workspaces["/tmp/workspace-one"] = config.Workspace{}
	m := newModel(context.Background(), Options{Config: cfg})
	for _, setup := range []struct {
		name         string
		tab          int
		settingsOpen bool
		hasDetail    bool
	}{
		{name: "launch", tab: 1},
		{name: "settings environments", tab: 2},
		{name: "settings", tab: 2, settingsOpen: true, hasDetail: true},
		{name: "maintenance", tab: 4},
		{name: "system", tab: 5},
	} {
		t.Run(setup.name, func(t *testing.T) {
			m.tab, m.settingsOpen = setup.tab, setup.settingsOpen
			lines := m.menuLines(80)
			if !strings.Contains(lines[2], reset+accent) {
				t.Fatalf("selected menu item has no accent color: %q", lines[2])
			}
			if got := strings.Contains(lines[2], dim); got != setup.hasDetail {
				t.Fatalf("selected menu detail style=%v, want %v: %q", got, setup.hasDetail, lines[2])
			}
			if strings.Contains(lines[3], reset+accent) {
				t.Fatalf("unselected menu item uses accent color: %q", lines[3])
			}
		})
	}
}

func TestSelectedChoiceUsesAccentColor(t *testing.T) {
	cfg := config.Defaults()
	cfg.Workspaces["/tmp/workspace-one"] = config.Workspace{}
	m := newModel(context.Background(), Options{Config: cfg})
	m.pending = tabMenus[1][0]
	m.showWorkspaceChoices()
	lines := m.choiceView()
	if !strings.Contains(lines[3], reset+accent) {
		t.Fatalf("selected choice has no accent color: %q", lines[3])
	}
	if !strings.Contains(lines[3], accent+"workspace-one "+dim+"— /tmp/workspace-one") {
		t.Fatalf("workspace path is not styled as secondary information: %q", lines[3])
	}
}

func TestSelectedSetupItemKeepsStateSecondary(t *testing.T) {
	m := newModel(context.Background(), Options{
		Config: config.Defaults(),
		Setup: []setup.Step{{
			ID: "hooks.claude", Title: "Claude hooks", State: setup.StatePresent,
			Options: []setup.Action{setup.ActionKeep},
		}},
	})
	m.tab = 5
	line := m.menuLines(80)[2]
	if !strings.Contains(line, accent+"Claude hooks  "+dim+"present") {
		t.Fatalf("setup state is not styled as secondary information: %q", line)
	}
}

func TestEffectiveSettingLineDimsInheritedValues(t *testing.T) {
	for _, test := range []struct {
		source string
		dimmed bool
	}{
		{source: "explicit"},
		{source: "workspace"},
		{source: "repository"},
		{source: "global", dimmed: true},
		{source: "default", dimmed: true},
		{source: "unset", dimmed: true},
	} {
		line := effectiveSettingLine(config.ScopeField{Key: "warm_count", Value: "1", Source: test.source}, 80)
		if got := strings.HasPrefix(line, dim); got != test.dimmed {
			t.Errorf("source %s dimmed=%v, want %v: %q", test.source, got, test.dimmed, line)
		}
		if plain := xansi.Strip(line); plain != "warm_count = 1 ("+test.source+")" {
			t.Errorf("source %s line=%q", test.source, plain)
		}
	}
}

func testTime() time.Time { return time.Date(2026, 9, 12, 12, 0, 0, 0, time.Local) }
