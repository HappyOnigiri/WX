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
	cfg.System.Language = config.LanguageJapanese
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
	japanese.System.Language = config.LanguageJapanese
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

// TestStatusViewShowsTheRunningVersionAndTheUpdateItem は、状態画面が版を示し、
// 更新があるときだけ選べる項目を出すことを守る。状態タブは 2 カラム化しないので区切りも出さない。
func TestStatusViewShowsTheRunningVersionAndTheUpdateItem(t *testing.T) {
	m := newModel(context.Background(), Options{Config: config.Defaults(), Version: "v1.0.0"})
	m.loading, m.status, m.statusAt = false, "DAEMON  running", testTime()
	m.width, m.height = 100, 24
	plain := xansi.Strip(m.View().Content)
	if !strings.Contains(plain, "Running wx v1.0.0") {
		t.Fatalf("status view has no running version: %q", plain)
	}
	if strings.Contains(plain, "A newer wx is available") {
		t.Fatalf("an update item appeared without an update: %q", plain)
	}
	m.opts.Update = UpdateInfo{Available: true, Version: "v1.1.0", URL: "https://example.test/v1.1.0"}
	withUpdate := m.View().Content
	plain = xansi.Strip(withUpdate)
	if !strings.Contains(plain, "A newer wx is available: v1.1.0") || !strings.Contains(plain, "https://example.test/v1.1.0") {
		t.Fatalf("status view omits the update item: %q", plain)
	}
	if strings.Contains(withUpdate, " │ ") {
		t.Fatalf("status view became a two column layout: %q", withUpdate)
	}
	if !strings.Contains(m.footer(), "Enter") {
		t.Fatalf("footer has no enter hint while an update item is selectable: %q", m.footer())
	}
}

// TestStatusViewKeepsTheUpdateItemVisibleOnALongStatus は、本文の行数を固定値で引くと
// 末尾が黙って欠ける退行を防ぐ。更新項目は画面に収まり、全体は端末の高さを超えない。
func TestStatusViewKeepsTheUpdateItemVisibleOnALongStatus(t *testing.T) {
	m := newModel(context.Background(), Options{Config: config.Defaults(), Version: "v1.0.0"})
	m.loading, m.statusAt = false, testTime()
	m.status = strings.TrimSuffix(strings.Repeat("status line\n", 60), "\n")
	m.opts.Update = UpdateInfo{Available: true, Version: "v1.1.0", URL: "https://example.test/v1.1.0"}
	for _, height := range []int{14, 24, 40} {
		m.width, m.height = 100, height
		content := m.View().Content
		if lines := strings.Count(content, "\n") + 1; lines > height {
			t.Fatalf("height=%d produced %d lines", height, lines)
		}
		if !strings.Contains(xansi.Strip(content), "A newer wx is available: v1.1.0") {
			t.Fatalf("height=%d dropped the update item: %q", height, content)
		}
	}
}

// TestStatusViewMarksTheFourSecondBoundary は、4 秒ちょうどの応答を古い表示へ分類する
// 境界を守る。丸め後の時刻を使うため、実時間の端数は 100ms だけ手前に置く。
func TestStatusViewMarksTheFourSecondBoundary(t *testing.T) {
	m := newModel(context.Background(), Options{Config: config.Defaults()})
	m.loading, m.status = false, "DAEMON  running"
	m.statusAt = time.Now().Add(-4*time.Second - 100*time.Millisecond)
	plain := xansi.Strip(strings.Join(m.statusView(), "\n"))
	if !strings.Contains(plain, "(4s ago)") {
		t.Fatalf("four-second response is not marked old: %q", plain)
	}
}

func TestStatusViewDistinguishesLoadingWithAndWithoutCachedStatus(t *testing.T) {
	m := newModel(context.Background(), Options{Config: config.Defaults()})
	m.loading, m.status = true, "cached status"
	withStatus := xansi.Strip(strings.Join(m.statusView(), "\n"))
	if strings.Contains(withStatus, m.t("dashboard.loading")) || !strings.Contains(withStatus, "cached status") {
		t.Fatalf("loading view with cached status=%q, want cached status without loading placeholder", withStatus)
	}
	m.status = ""
	withoutStatus := xansi.Strip(strings.Join(m.statusView(), "\n"))
	if !strings.Contains(withoutStatus, m.t("dashboard.loading")) {
		t.Fatalf("loading view without cached status=%q, want loading placeholder", withoutStatus)
	}
}

func TestStatusViewUsesFreshLabelBeforeFourSeconds(t *testing.T) {
	m := newModel(context.Background(), Options{Config: config.Defaults()})
	m.loading, m.status = false, "DAEMON running"
	m.statusAt = time.Now().Add(-time.Second)
	plain := xansi.Strip(strings.Join(m.statusView(), "\n"))
	if !strings.Contains(plain, "Updated ") || strings.Contains(plain, "ago)") {
		t.Fatalf("fresh response label=%q, want an absolute updated-at label", plain)
	}
}

func TestStatusViewShowsNoStatusForEmptyResponse(t *testing.T) {
	m := newModel(context.Background(), Options{Config: config.Defaults()})
	m.loading = false
	plain := xansi.Strip(strings.Join(m.statusView(), "\n"))
	if !strings.Contains(plain, m.t("dashboard.no_status")) {
		t.Fatalf("empty response=%q, want no-status message", plain)
	}
}

// TestOperationViewUsesTwoColumnsAtTheWidthBoundary は、幅 92 の画面を縦積みにせず
// 2 カラムで描画する境界を守る。
func TestOperationViewUsesTwoColumnsAtTheWidthBoundary(t *testing.T) {
	m := newModel(context.Background(), Options{Config: config.Defaults()})
	m.tab, m.width = 1, 92
	if got := strings.Join(m.operationView(), "\n"); !strings.Contains(got, " │ ") {
		t.Fatalf("width-boundary operation view is not two columns: %q", got)
	}
}

// TestOperationViewDoesNotAppendAHeightBoundaryRow は、左右ペインの最大行数ちょうどで
// 描画を止め、空の余分な区切り行を追加しないことを守る。
func TestOperationViewDoesNotAppendAHeightBoundaryRow(t *testing.T) {
	m := newModel(context.Background(), Options{Config: config.Defaults()})
	m.tab, m.width = 1, 92
	left := m.menuLines(m.width)
	leftWidth := m.leftColumnWidth(left)
	right := m.descriptionLines(m.width - leftWidth - columnGap)
	want := max(len(left), len(right))
	got := m.operationView()
	if len(got) != want {
		t.Fatalf("operation rows=%d, want %d (left=%d right=%d)", len(got), want, len(left), len(right))
	}
}

// TestOperationViewHandlesATallerDescriptionPane は、説明側がメニュー側より長い場合も
// 左ペインの末尾を越えて参照しないことを守る。
func TestOperationViewHandlesATallerDescriptionPane(t *testing.T) {
	cases := []struct {
		language string
		tab      int
	}{
		{language: config.LanguageEnglish, tab: 4},
		{language: config.LanguageJapanese, tab: 4},
		{language: config.LanguageEnglish, tab: 1},
		{language: config.LanguageJapanese, tab: 1},
	}
	for _, test := range cases {
		cfg := config.Defaults()
		cfg.System.Language = test.language
		m := newModel(context.Background(), Options{Config: cfg})
		m.tab, m.width = test.tab, 92
		left := m.menuLines(m.width)
		leftWidth := m.leftColumnWidth(left)
		rightWidth := m.width - leftWidth - columnGap
		if rightWidth < minRightColumn {
			continue
		}
		right := m.descriptionLines(rightWidth)
		if len(right) <= len(left) {
			continue
		}
		if got := m.operationView(); len(got) != len(right) {
			t.Fatalf("tab=%d language=%s rows=%d, want %d", test.tab, test.language, len(got), len(right))
		}
		return
	}
	t.Fatal("no operation view case has a taller description pane")
}

// TestDescriptionLinesUsesTheMenuAfterTheLastSetupStep は、setup 項目の末尾から通常メニュー
// へ切り替わる位置を、setup 配列の範囲外として扱わないことを守る。
func TestDescriptionLinesUsesTheMenuAfterTheLastSetupStep(t *testing.T) {
	m := newModel(context.Background(), Options{
		Config: config.Defaults(),
		Setup:  []setup.Step{hooksStep()},
	})
	m.tab, m.selected = 5, 1
	plain := xansi.Strip(strings.Join(m.descriptionLines(80), "\n"))
	if !strings.Contains(plain, m.t(tabMenus[5][0].labelID)) {
		t.Fatalf("first system menu description is missing: %q", plain)
	}
}

// TestDescriptionLinesOmitsAttentionWithoutReasons は、理由のない setup 項目に注意見出しを
// 追加しないことを守る。
func TestDescriptionLinesOmitsAttentionWithoutReasons(t *testing.T) {
	step := hooksStep()
	step.Reasons = nil
	m := newModel(context.Background(), Options{Config: config.Defaults(), Setup: []setup.Step{step}})
	m.tab = 5
	plain := xansi.Strip(strings.Join(m.descriptionLines(80), "\n"))
	if strings.Contains(plain, m.t("dashboard.attention")) {
		t.Fatalf("attention heading appeared without reasons: %q", plain)
	}
}

func TestDescriptionLinesHandlesExactSelectionBoundaries(t *testing.T) {
	cfg := config.DefaultsV2()
	const workspace = "/tmp/project"
	cfg.Workspaces[workspace] = config.Workspace{}
	m := newModel(context.Background(), Options{Config: cfg})
	m.tab, m.settingsOpen = 2, false
	m.selected = len(m.configEnvironments())
	if got := m.descriptionLines(80); got != nil {
		t.Fatalf("out-of-range environment description=%q, want nil", got)
	}
	workspaceIndex, defaultsIndex := -1, -1
	for index, environment := range m.configEnvironments() {
		if environment.target == workspace && environment.scope == config.V2ScopeWorkspace {
			if environment.repositoryDefaults {
				defaultsIndex = index
			} else {
				workspaceIndex = index
			}
		}
	}
	if workspaceIndex < 0 || defaultsIndex < 0 {
		t.Fatalf("workspace environments missing: %d/%d", workspaceIndex, defaultsIndex)
	}
	m.selected = workspaceIndex
	if got := strings.Join(m.descriptionLines(80), "\n"); !strings.Contains(got, workspace) {
		t.Fatalf("workspace description=%q, want target path", got)
	}

	m.settingsOpen, m.settingsEnv = true, 0
	items := m.configItems()
	if len(items) == 0 {
		t.Fatal("system settings are empty, want an exact end-of-list selection")
	}
	m.selected = len(items)
	if got := m.itemCount(); got != len(items) {
		t.Fatalf("settings item count=%d, want %d", got, len(items))
	}
	if got := m.descriptionLines(80); got != nil {
		t.Fatalf("out-of-range setting description=%q, want nil", got)
	}
}

// TestEnvironmentFieldsReturnsNilAtTheEnd は、環境一覧の直後を選択したときに一覧外へ
// アクセスせず、表示項目なしとして扱うことを守る。
func TestEnvironmentFieldsReturnsNilAtTheEnd(t *testing.T) {
	m := newModel(context.Background(), Options{Config: config.Defaults()})
	m.settingsOpen = true
	m.settingsEnv = len(m.configEnvironments())
	if got := m.environmentFields(); got != nil {
		t.Fatalf("out-of-range environment returned %d fields, want nil", len(got))
	}
}

// TestWrapKeepsAValueWhenWidthIsOne は、幅 1 以下では折り返し不能として元の値を返す
// 契約を守る。
func TestWrapKeepsAValueWhenWidthIsOne(t *testing.T) {
	got := wrap("abc", 1)
	if len(got) != 1 || got[0] != "abc" {
		t.Fatalf("wrap width=1 got %#v, want []string{\"abc\"}", got)
	}
}

// TestWrapKeepsAnExactWidthValue は、表示幅がちょうど収まる値を複数行へ分割しない
// 契約を記録する。同じ結果になる比較変異は exclusion で理由を明示する。
func TestWrapKeepsAnExactWidthValue(t *testing.T) {
	got := wrap("abc", 3)
	if len(got) != 1 || got[0] != "abc" {
		t.Fatalf("wrap exact width got %#v, want []string{\"abc\"}", got)
	}
}

// TestTruncateKeepsAValueAtZeroWidth は、幅 0 を切り詰め不能として元の値を返す境界を
// 守る。
func TestTruncateKeepsAValueAtZeroWidth(t *testing.T) {
	if got := truncate("abc", 0); got != "abc" {
		t.Fatalf("truncate width=0 got %q, want original value", got)
	}
}

// TestPadANSIKeepsAnExactWidthValue は、既に収まる行へ padding を追加しない契約を記録
// する。比較変異自体は同じ文字列を返すため exclusion で理由を明示する。
func TestPadANSIKeepsAnExactWidthValue(t *testing.T) {
	if got := padANSI("abc", 3); got != "abc" {
		t.Fatalf("padANSI exact width got %q, want original value", got)
	}
}

// TestFitLinesKeepsExactlyHeightLines は、行数が高さと等しいときに行を削らない契約を
// 記録する。比較変異自体は同じ slice を返すため exclusion で理由を明示する。
func TestFitLinesKeepsExactlyHeightLines(t *testing.T) {
	got := fitLines([]string{"one", "two"}, 2, 10)
	if len(got) != 2 || got[0] != "one" || got[1] != "two" {
		t.Fatalf("fitLines exact height got %#v, want both lines", got)
	}
}
