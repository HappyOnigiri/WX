package dashboard

import (
	"context"
	"strings"
	"testing"
	"time"

	xansi "github.com/charmbracelet/x/ansi"

	"github.com/HappyOnigiri/WX/internal/config"
	"github.com/HappyOnigiri/WX/internal/i18n"
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
	if !strings.Contains(narrow, "Choose what to run in a borrowed worktree") || !strings.Contains(narrow, "selected workspace") {
		t.Fatalf("narrow operation view omitted stacked content: %q", narrow)
	}
	for _, line := range strings.Split(narrow, "\n") {
		if xansi.StringWidth(line) > 70 {
			t.Fatalf("line exceeds width: %q", line)
		}
	}
}

// TestJapaneseOperationViewKeepsColumnsAligned は、英語の幅で padding を決めた
// 後に翻訳して区切りがずれる退行を防ぐ。全角を含む行でも区切りは同じ列に並ぶ。
func TestJapaneseOperationViewKeepsColumnsAligned(t *testing.T) {
	cfg := config.Defaults()
	cfg.Language = config.LanguageJapanese
	m := newModel(context.Background(), Options{Config: cfg})
	m.tab, m.width, m.height = 4, 145, 30
	lines := m.operationView()
	column := -1
	for _, line := range lines {
		plain := xansi.Strip(line)
		index := strings.Index(plain, "│")
		if index < 0 {
			t.Fatalf("two column line has no separator: %q", plain)
		}
		width := xansi.StringWidth(plain[:index])
		if column < 0 {
			column = width
		}
		if width != column {
			t.Fatalf("separator column=%d, want %d: %q", width, column, plain)
		}
	}
	frame := strings.Join(lines, "\n")
	if !strings.Contains(frame, "保持期間を過ぎたデータを削除") {
		t.Fatalf("menu label is not localized: %q", frame)
	}
	// 英語の説明が残っていないことを確かめる。label にも含まれない英語の一文を選ぶ。
	if strings.Contains(frame, "wx keeps for a fixed period") {
		t.Fatalf("description is not localized: %q", frame)
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
	for tab := range tabIDs {
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
		Setup:  []setup.Step{hooksStep()},
	})
	m.tab = 5
	line := m.menuLines(80)[2]
	if !strings.Contains(line, accent+"Agent hooks (claude)  "+dim+"configured as wx expects") {
		t.Fatalf("setup state is not styled as secondary information: %q", line)
	}
}

// hooksStep は setup が返す形の項目で、見出し・説明・理由をすべて解決前の message で持つ。
func hooksStep() setup.Step {
	return setup.Step{
		ID:      "hooks.claude",
		Title:   i18n.Message{ID: "wx.setup.item.hooks", Data: map[string]any{"Agent": "claude"}},
		State:   setup.StatePresent,
		Detail:  i18n.Message{ID: "setup.detail.launch_agent"},
		Target:  "/home/user/.claude/settings.json",
		Reasons: []i18n.Message{{ID: "setup.reason.plist_stale"}},
		Options: []setup.Action{setup.ActionKeep},
	}
}

// TestSetupItemsRenderInTheConfiguredLanguage は、システムタブの見出し・状態・説明・理由が
// 表示言語で解決されることを検査する。setup は message ID だけを返すので、
// 解決を落とすと画面に ID がそのまま出る。
func TestSetupItemsRenderInTheConfiguredLanguage(t *testing.T) {
	japanese := config.Defaults()
	japanese.Language = config.LanguageJapanese
	m := newModel(context.Background(), Options{Config: japanese, Setup: []setup.Step{hooksStep()}})
	m.tab = 5
	menu := strings.Join(m.menuLines(80), "\n")
	if !strings.Contains(menu, "Agent hook (claude)") || !strings.Contains(menu, "設定済み") {
		t.Fatalf("menu is not localized: %q", menu)
	}
	description := strings.Join(m.descriptionLines(80), "\n")
	for _, want := range []string{"hooks.claude", "LaunchAgent", "/home/user/.claude/settings.json", "登録済みの plist"} {
		if !strings.Contains(description, want) {
			t.Fatalf("description %q is missing from %q", want, description)
		}
	}
	// 解決できなかった message は ID が画面へ出る。項目 ID（hooks.claude）以外に
	// message ID の形をした語が残っていないことを確かめる。
	for _, prefix := range []string{"setup.", "wx.setup.", "dashboard.", "hook.finding."} {
		if strings.Contains(description, prefix) || strings.Contains(menu, prefix) {
			t.Fatalf("an unresolved message id (%s…) reached the screen: %q", prefix, menu+description)
		}
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
