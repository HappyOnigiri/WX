package dashboard

import (
	"context"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/HappyOnigiri/WorktreeX/internal/config"
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
			if item.inputLabelID != "" && !item.inputNeeded && item.argumentChoices == nil {
				t.Errorf("tab %d item %q opens unrestricted optional input", tab, item.labelID)
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

func TestStatusScrollKeysAndResize(t *testing.T) {
	m := newModel(context.Background(), Options{Config: config.Defaults()})
	m.loading = false
	m.status = strings.TrimSuffix(strings.Repeat("line\n", 30), "\n")
	m.height = 14
	m.offset = 4
	updated, _ := m.Update(key(tea.KeyDown))
	m = updated.(model)
	if m.statusOffset != 1 || m.offset != 4 {
		t.Fatalf("status offset=%d menu offset=%d, want 1 and 4", m.statusOffset, m.offset)
	}
	updated, _ = m.Update(key(tea.KeyPgDown))
	m = updated.(model)
	if m.statusOffset != 1+m.statusPageRows() {
		t.Fatalf("page down offset=%d, page rows=%d", m.statusOffset, m.statusPageRows())
	}
	updated, _ = m.Update(tea.WindowSizeMsg{Width: 100, Height: 50})
	m = updated.(model)
	if m.statusOffset != 0 {
		t.Fatalf("resize did not clamp status offset: %d", m.statusOffset)
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

func TestLeftReturnsFromNestedMenus(t *testing.T) {
	m := newModel(context.Background(), Options{Config: config.Defaults()})
	m.tab = 1
	updated, _ := m.Update(key(tea.KeyEnter))
	m = updated.(model)
	updated, _ = m.Update(key(tea.KeyLeft))
	m = updated.(model)
	if m.tab != 1 || m.mode != modeList {
		t.Fatalf("workspace choice left returned tab=%d mode=%v", m.tab, m.mode)
	}

	m.tab, m.settingsOpen, m.settingsEnv, m.selected = 2, true, 0, 0
	updated, _ = m.Update(key(tea.KeyEnter))
	m = updated.(model)
	updated, _ = m.Update(key(tea.KeyLeft))
	m = updated.(model)
	if m.tab != 2 || m.mode != modeList || !m.settingsOpen {
		t.Fatalf("setting choice left returned tab=%d mode=%v open=%v", m.tab, m.mode, m.settingsOpen)
	}

	m.selected = 1
	updated, _ = m.Update(key(tea.KeyLeft))
	m = updated.(model)
	if m.tab != 2 || m.settingsOpen || m.selected != 0 {
		t.Fatalf("settings left returned tab=%d open=%v selected=%d", m.tab, m.settingsOpen, m.selected)
	}
}

func TestSettingsNavigateFromEnvironmentToChoice(t *testing.T) {
	cfg := config.Defaults()
	cfg.Workspaces["/tmp/workspace-one"] = config.Workspace{}
	m := newModel(context.Background(), Options{Config: cfg})
	m.tab = 2
	if got := m.currentLabels(); len(got) != 5 || got[0] != "System" || got[1] != "Workspace defaults" || got[2] != "Repository defaults" || got[3] != "Workspace  workspace-one (not discovered)" || got[4] != "  Repository defaults" {
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

// TestStatusTabActivatesTheUpdateItemOnlyWhenAnUpdateExists は、状態タブのカーソルと Enter の
// 出し分けを守る。更新が無いときは従来どおり選択も offset も動かず、Enter も何もしない。
func TestStatusTabActivatesTheUpdateItemOnlyWhenAnUpdateExists(t *testing.T) {
	m := newModel(context.Background(), Options{Config: config.Defaults(), Version: "v1.0.0"})
	m.offset = 3
	updated, _ := m.Update(key(tea.KeyDown))
	m = updated.(model)
	if m.itemCount() != 0 || m.selected != 0 || m.offset != 3 {
		t.Fatalf("count=%d selected=%d offset=%d, want an inert status tab", m.itemCount(), m.selected, m.offset)
	}
	updated, _ = m.Update(key(tea.KeyEnter))
	m = updated.(model)
	if m.mode != modeList {
		t.Fatalf("mode=%v, want the status tab to ignore enter", m.mode)
	}

	m.opts.Update = UpdateInfo{Available: true, Version: "v1.1.0", URL: "https://example.test/v1.1.0"}
	updated, _ = m.Update(key(tea.KeyDown))
	m = updated.(model)
	if m.itemCount() != 1 || m.selected != 0 || m.offset != 3 {
		t.Fatalf("count=%d selected=%d offset=%d, want the single item selected and the offset untouched", m.itemCount(), m.selected, m.offset)
	}
	updated, _ = m.Update(key(tea.KeyEnter))
	m = updated.(model)
	if m.mode != modeConfirm || m.pending.command != "update" {
		t.Fatalf("mode=%v pending=%q, want the update confirmation", m.mode, m.pending.command)
	}
	m.finishPending()
	if got := strings.Join(m.result.Args, " "); got != "update --apply" || m.result.WorkDir != "" {
		t.Fatalf("action=%q workdir=%q, want an update --apply with no working directory", got, m.result.WorkDir)
	}
	if !m.pending.external {
		t.Fatal("the update runs inside the dashboard process, which is the binary being replaced")
	}
}

// TestStatusTabSelectionStaysInRangeWhenTheUpdateItemDisappears は、項目が消えた瞬間に
// 選択が範囲外を指さないことを守る。
func TestStatusTabSelectionStaysInRangeWhenTheUpdateItemDisappears(t *testing.T) {
	m := newModel(context.Background(), Options{Config: config.Defaults(), Version: "v1.0.0"})
	m.opts.Update = UpdateInfo{Available: true, Version: "v1.1.0"}
	updated, _ := m.Update(key(tea.KeyDown))
	m = updated.(model)
	m.opts.Update = UpdateInfo{}
	if m.selected >= max(1, m.itemCount()) && m.itemCount() != 0 {
		t.Fatalf("selected=%d count=%d", m.selected, m.itemCount())
	}
	updated, _ = m.Update(key(tea.KeyEnter))
	if got := updated.(model).mode; got != modeList {
		t.Fatalf("mode=%v, want enter ignored once the update item is gone", got)
	}
}

// TestStatusTabStillRefreshesWithTheUpdateItemPresent は、r キーの再読込が更新項目と衝突しないことを守る。
func TestStatusTabStillRefreshesWithTheUpdateItemPresent(t *testing.T) {
	m := newModel(context.Background(), Options{Config: config.Defaults(), Version: "v1.0.0"})
	m.loading = false
	m.opts.Update = UpdateInfo{Available: true, Version: "v1.1.0"}
	updated, cmd := m.Update(key('r'))
	if cmd == nil || !updated.(model).loading {
		t.Fatal("r did not start a status refresh while the update item is present")
	}
}

// TestExecutionRefreshIgnoresOutOfRangeSettingsEnvironment は、設定画面の環境一覧が
// 更新前後で変わっても、存在しない選択位置を参照しないことを守る。
func TestExecutionRefreshIgnoresOutOfRangeSettingsEnvironment(t *testing.T) {
	m := newModel(context.Background(), Options{Config: config.Defaults()})
	m.settingsOpen = true
	m.settingsEnv = len(m.configEnvironments())
	m.target = "/tmp/target"

	updated, _ := m.Update(executionMsg{config: config.Defaults()})
	if got := updated.(model).mode; got != modeResult {
		t.Fatalf("mode=%v, want result after a refresh with an out-of-range environment", got)
	}
}

// TestExecutionRefreshSkipsAnOutOfRangeRepositoryEnvironment は、再読込後に環境数が
// 減っても repository の選択位置を参照しないことを守る。
func TestExecutionRefreshSkipsAnOutOfRangeRepositoryEnvironment(t *testing.T) {
	cfg := config.DefaultsV2()
	cfg.Workspaces["/tmp/project"] = config.Workspace{Repositories: map[string]config.Repository{
		"backend":  {},
		"frontend": {},
	}}
	m := newModel(context.Background(), Options{Config: cfg})
	repositoryIndex := -1
	for index, environment := range m.configEnvironments() {
		if environment.scope == config.V2ScopeRepository && environment.repository == "backend" {
			repositoryIndex = index
			break
		}
	}
	if repositoryIndex < 0 {
		t.Fatal("backend repository environment is missing")
	}
	m.settingsOpen, m.settingsEnv, m.target = true, repositoryIndex, "/tmp/project"
	refreshed := config.DefaultsV2()
	refreshed.Workspaces["/tmp/project"] = config.Workspace{Repositories: map[string]config.Repository{"other": {}}}
	updated, _ := m.Update(executionMsg{config: refreshed})
	if got := updated.(model).mode; got != modeResult {
		t.Fatalf("mode=%v, want result after the repository environment disappeared", got)
	}
}

// TestExecutionRefreshKeepsRepositoryDefaultsEnvironment は、nested な「Repository defaults」で
// 編集した後の再読込が、同じ scope と target を持つ親 workspace へ滑らないことを守る。
// 滑ると項目一覧が短い親へ入れ替わり、末尾を選んでいた描画が範囲外を引いて panic する。
func TestExecutionRefreshKeepsRepositoryDefaultsEnvironment(t *testing.T) {
	const workspace = "/tmp/project"
	cfg := config.DefaultsV2()
	cfg.Workspaces[workspace] = config.Workspace{}
	m := newModel(context.Background(), Options{Config: cfg})
	defaultsIndex := -1
	for index, environment := range m.configEnvironments() {
		if environment.scope == config.V2ScopeWorkspace && environment.target == workspace && environment.repositoryDefaults {
			defaultsIndex = index
			break
		}
	}
	if defaultsIndex < 0 {
		t.Fatal("repository defaults environment is missing")
	}
	m.tab, m.settingsOpen, m.settingsEnv, m.target = 2, true, defaultsIndex, workspace
	nested := len(m.configItems())
	m.selected = nested - 1

	refreshed := config.DefaultsV2()
	refreshed.Workspaces[workspace] = config.Workspace{}
	updated, _ := m.Update(executionMsg{config: refreshed, rawConfig: refreshed})
	m = updated.(model)
	if m.settingsEnv != defaultsIndex {
		t.Fatalf("settingsEnv=%d, want the repository defaults environment %d", m.settingsEnv, defaultsIndex)
	}
	if got := len(m.configItems()); got != nested {
		t.Fatalf("items=%d, want the nested list of %d", got, nested)
	}
	updated, _ = m.Update(key(tea.KeyEnter))
	m = updated.(model)
	if m.mode != modeList {
		t.Fatalf("mode=%v, want list after leaving the result screen", m.mode)
	}
	m.View()
}

// TestExecutionRefreshClampsSelectionToShorterList は、再読込で項目が減ったときに選択位置が
// 一覧の範囲へ戻ることを守る。再読込は選択を動かさずに一覧だけを入れ替える。
func TestExecutionRefreshClampsSelectionToShorterList(t *testing.T) {
	cfg := config.DefaultsV2()
	cfg.Workspaces["/tmp/project"] = config.Workspace{}
	m := newModel(context.Background(), Options{Config: cfg})
	m.tab = 2
	m.selected = m.itemCount() - 1

	refreshed := config.DefaultsV2()
	updated, _ := m.Update(executionMsg{config: refreshed, rawConfig: refreshed})
	m = updated.(model)
	if want := m.itemCount() - 1; m.selected != want {
		t.Fatalf("selected=%d, want %d after the workspace environments disappeared", m.selected, want)
	}
	updated, _ = m.Update(key(tea.KeyEnter))
	m = updated.(model)
	m.View()
}
