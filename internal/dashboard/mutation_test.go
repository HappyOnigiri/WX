package dashboard

import (
	"context"
	"slices"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	xansi "github.com/charmbracelet/x/ansi"

	"github.com/HappyOnigiri/WorktreeX/internal/config"
	"github.com/HappyOnigiri/WorktreeX/internal/i18n"
	"github.com/HappyOnigiri/WorktreeX/internal/setup"
)

// TestMutationMoveKeepsSelectionAndVisibleWindowAtBothEnds は、選択移動の加減算と
// 表示窓の上下端を同時に検査する。selected+delta の符号を変えるだけの変異も、
// 行の追加だけを検査するテストでは見逃す。
func TestMutationMoveKeepsSelectionAndVisibleWindowAtBothEnds(t *testing.T) {
	m := newModel(context.Background(), Options{Config: config.Defaults()})
	m.tab, m.height = 1, 12
	m.selected, m.offset = 0, 2
	m.move(-1)
	if m.selected != 0 || m.offset != 0 {
		t.Fatalf("move above first item selected=%d offset=%d, want 0,0", m.selected, m.offset)
	}

	m.selected, m.offset = 0, 0
	m.move(m.visibleRows())
	if m.selected != m.visibleRows() || m.offset != 1 {
		t.Fatalf("move below first page selected=%d offset=%d, want %d,1", m.selected, m.offset, m.visibleRows())
	}

	m.selected, m.offset = m.itemCount()-1, m.offset
	m.move(1)
	if m.selected != m.itemCount()-1 {
		t.Fatalf("move past last item selected=%d, want %d", m.selected, m.itemCount()-1)
	}

	// keepVisible は上へ寄せた直後でも、rows 分下の範囲を再度ずらさない。
	m.selected, m.offset = 4, 6
	m.keepVisible()
	if m.offset != 4 {
		t.Fatalf("keepVisible offset=%d, want 4", m.offset)
	}
}

// TestMutationActivateDistinguishesV2EnvironmentScopes は、workspace を対象にする環境と
// machine/global defaults を対象にする環境を、activate の target へ反映する契約で比較する。
func TestMutationActivateDistinguishesV2EnvironmentScopes(t *testing.T) {
	cfg := config.DefaultsV2()
	cfg.Workspaces["/tmp/project"] = config.Workspace{Repositories: map[string]config.Repository{
		"backend":  {},
		"frontend": {},
	}}

	base := newModel(context.Background(), Options{Config: cfg})
	seen := map[string]bool{}
	for index, environment := range base.configEnvironments() {
		m := newModel(context.Background(), Options{Config: cfg})
		m.tab, m.settingsOpen, m.settingsEnv, m.selected = 2, true, index, 0
		if len(m.configItems()) == 0 {
			t.Fatalf("environment %q has no editable items", environment.label)
		}
		updated, _ := m.activate()
		got := updated.(model).target
		want := ""
		if environment.scope == config.V2ScopeWorkspace || environment.scope == config.V2ScopeRepository {
			want = environment.target
		}
		if got != want {
			t.Errorf("scope=%q repositoryDefaults=%v target=%q, want %q", environment.scope, environment.repositoryDefaults, got, want)
		}
		seen[environment.scope] = true
	}
	for _, scope := range []string{config.V2ScopeSystem, config.V2ScopeWorkspaceDefaults, config.V2ScopeRepositoryDefaults, config.V2ScopeWorkspace, config.V2ScopeRepository} {
		if !seen[scope] {
			t.Errorf("scope %q was not exercised", scope)
		}
	}
}

// TestMutationExecutionRefreshRestoresARepositorySelection は、設定の再読込後も選択中の
// repository membership と breadcrumb の対象を失わないことを守る。
func TestMutationExecutionRefreshRestoresARepositorySelection(t *testing.T) {
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
	m.tab, m.settingsOpen, m.settingsEnv, m.target = 2, true, repositoryIndex, "/tmp/project"
	refreshed := config.DefaultsV2()
	refreshed.Workspaces["/tmp/project"] = config.Workspace{Repositories: map[string]config.Repository{
		"backend":  {},
		"frontend": {},
	}}
	updated, _ := m.Update(executionMsg{config: refreshed})
	got := updated.(model)
	selected := got.configEnvironments()[got.settingsEnv]
	if selected.scope != config.V2ScopeRepository || selected.repository != "backend" || selected.target != "/tmp/project" {
		t.Fatalf("selection after refresh=%+v, want backend repository", selected)
	}
}

func TestMutationExecutionRefreshCreatesSelectedWorkspaceScope(t *testing.T) {
	const workspace = "/tmp/project"
	cfg := config.DefaultsV2()
	cfg.Workspaces[workspace] = config.Workspace{}
	m := newModel(context.Background(), Options{Config: cfg})
	workspaceIndex := -1
	for index, environment := range m.configEnvironments() {
		if environment.scope == config.V2ScopeWorkspace && environment.target == workspace && !environment.repositoryDefaults {
			workspaceIndex = index
			break
		}
	}
	if workspaceIndex < 0 {
		t.Fatal("workspace environment is missing")
	}
	m.tab, m.settingsOpen, m.settingsEnv, m.target = 2, true, workspaceIndex, workspace
	updated, _ := m.Update(executionMsg{config: config.Config{Version: 2}})
	got := updated.(model)
	if _, ok := got.opts.Config.Workspaces[workspace]; !ok {
		t.Fatalf("refreshed workspace map=%v, want selected workspace initialized", got.opts.Config.Workspaces)
	}
}

func TestMutationExecutionRefreshCreatesSelectedRepositoryScope(t *testing.T) {
	const workspace = "/tmp/project"
	cfg := config.DefaultsV2()
	cfg.Workspaces[workspace] = config.Workspace{Repositories: map[string]config.Repository{"backend": {}, "frontend": {}}}
	m := newModel(context.Background(), Options{Config: cfg})
	repositoryIndex := -1
	for index, environment := range m.configEnvironments() {
		if environment.scope == config.V2ScopeRepository && environment.target == workspace && environment.repository == "backend" {
			repositoryIndex = index
			break
		}
	}
	if repositoryIndex < 0 {
		t.Fatal("repository environment is missing")
	}
	m.tab, m.settingsOpen, m.settingsEnv, m.target = 2, true, repositoryIndex, workspace
	refreshed := config.Config{Version: 2, Workspaces: map[string]config.Workspace{
		workspace:    {},
		"/tmp/other": {},
	}}
	updated, _ := m.Update(executionMsg{config: refreshed})
	got := updated.(model)
	workspaceConfig := got.opts.Config.Workspaces[workspace]
	if _, ok := workspaceConfig.Repositories[""]; ok {
		t.Fatalf("refreshed repositories=%v, did not want an empty repository from workspace environment", workspaceConfig.Repositories)
	}
}

func TestMutationExecutionRefreshMatchesEveryEnvironmentQualifier(t *testing.T) {
	cfg := config.DefaultsV2()
	cfg.Workspaces["/tmp/alpha"] = config.Workspace{Repositories: map[string]config.Repository{"backend": {}, "frontend": {}}}
	cfg.Workspaces["/tmp/zeta"] = config.Workspace{Repositories: map[string]config.Repository{"backend": {}, "frontend": {}}}
	base := newModel(context.Background(), Options{Config: cfg})
	for index, want := range base.configEnvironments() {
		m := newModel(context.Background(), Options{Config: cfg})
		m.tab, m.settingsOpen, m.settingsEnv, m.target = 2, true, index, want.target
		refreshed := config.DefaultsV2()
		for path, workspace := range cfg.Workspaces {
			refreshed.Workspaces[path] = workspace
		}
		// 先行workspaceの追加後も同じ環境へ戻る。
		refreshed.Workspaces["/tmp/beta"] = config.Workspace{}
		updated, _ := m.Update(executionMsg{config: refreshed})
		got := updated.(model)
		if got.settingsEnv >= len(got.configEnvironments()) {
			t.Fatalf("scope=%q target=%q repository=%q settingsEnv=%d out of range", want.scope, want.target, want.repository, got.settingsEnv)
		}
		selected := got.configEnvironments()[got.settingsEnv]
		if selected.scope != want.scope || selected.target != want.target || selected.repository != want.repository || selected.repositoryDefaults != want.repositoryDefaults {
			t.Fatalf("selected=%+v, want exact environment %+v", selected, want)
		}
	}
}

// TestMutationItemCountCoversEmptyAndSetupMenus は、tab と settingsOpen の分岐、および
// setup item と固定メニューの加算を、実際の件数で比較する。
func TestMutationItemCountCoversEmptyAndSetupMenus(t *testing.T) {
	cfg := config.DefaultsV2()
	cfg.Workspaces["/tmp/project"] = config.Workspace{}
	m := newModel(context.Background(), Options{
		Config: cfg,
		Setup:  []setup.Step{{ID: "hooks", Options: []setup.Action{setup.ActionKeep}}},
	})

	m.tab, m.opts.Update = 0, UpdateInfo{}
	if got := m.itemCount(); got != 0 {
		t.Fatalf("status without update count=%d, want 0", got)
	}
	m.opts.Update.Available = true
	if got := m.itemCount(); got != 1 {
		t.Fatalf("status with update count=%d, want 1", got)
	}
	m.tab, m.settingsOpen = 2, false
	if got, want := m.itemCount(), len(m.configEnvironments()); got != want || got != 5 {
		t.Fatalf("environment count=%d, want %d", got, want)
	}
	m.settingsOpen = true
	if got, want := m.itemCount(), len(m.configItems()); got != want || got == 0 {
		t.Fatalf("settings count=%d, want %d", got, want)
	}
	m.tab, m.settingsOpen = 5, false
	if got, want := m.itemCount(), len(m.setupItems())+len(tabMenus[5]); got != want || got != 4 {
		t.Fatalf("setup count=%d, want %d", got, want)
	}
	m.tab = 3
	if got, want := m.itemCount(), len(tabMenus[3]); got != want || got != 3 {
		t.Fatalf("doctor count=%d, want %d", got, want)
	}
}

// TestMutationUpdateInputUsesStageSpecificRequiredRules は、空入力が許される workdir-args
// と、空入力を拒否する各 stage を分けて検査する。
func TestMutationUpdateInputUsesStageSpecificRequiredRules(t *testing.T) {
	tests := []struct {
		name        string
		stage       string
		workDir     bool
		inputNeeded bool
		wantMode    mode
	}{
		{name: "workdir", stage: "workdir", wantMode: modeInput},
		{name: "target", stage: "target-value", wantMode: modeInput},
		{name: "other workdir", stage: "other", workDir: true, wantMode: modeInput},
		{name: "required argument", stage: "arguments", inputNeeded: true, wantMode: modeInput},
		{name: "optional workdir arguments", stage: "workdir-args", workDir: true, wantMode: modeConfirm},
		{name: "optional input", stage: "arguments", wantMode: modeConfirm},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			m := newModel(context.Background(), Options{Config: config.Defaults()})
			m.mode, m.inputStage = modeInput, test.stage
			m.pending.workDir, m.pending.inputNeeded = test.workDir, test.inputNeeded
			updated, _ := m.updateInput(key(tea.KeyEnter))
			if got := updated.(model).mode; got != test.wantMode {
				t.Fatalf("stage=%q workDir=%v inputNeeded=%v mode=%v, want %v", test.stage, test.workDir, test.inputNeeded, got, test.wantMode)
			}
		})
	}
}

// TestMutationChooseSeparatesSetupAndArgumentChoices は、同じ tab でも setup command の
// choice と通常の argument choice が異なる状態遷移になることを検査する。
func TestMutationChooseSeparatesSetupAndArgumentChoices(t *testing.T) {
	m := newModel(context.Background(), Options{Config: config.Defaults()})
	m.tab, m.mode, m.pending = 5, modeChoice, menuItem{command: "setup"}
	m.choices = []choice{{label: "keep", value: string(setup.ActionKeep)}, {label: "manual", value: string(setup.ActionManual)}}
	m.choice = 1
	updated, _ := m.choose()
	got := updated.(model)
	if got.mode != modeInput || got.inputStage != "setup-value" || !slices.Equal(got.pending.defaultArgs, []string{"--action", string(setup.ActionManual)}) {
		t.Fatalf("manual setup choice state=%+v, want setup-value input", got)
	}

	m = newModel(context.Background(), Options{Config: config.Defaults()})
	m.tab, m.mode, m.pending = 5, modeChoice, menuItem{command: "other"}
	m.inputStage = "arguments-choice"
	m.choices = []choice{{label: "value", value: "--value"}}
	updated, _ = m.choose()
	got = updated.(model)
	if got.mode != modeConfirm || got.input != "--value" || len(got.pending.defaultArgs) != 0 {
		t.Fatalf("non-setup choice state=%+v, want argument confirmation without setup args", got)
	}
}

func TestMutationActivateUsesTheSetupBoundaryBeforeFixedMenu(t *testing.T) {
	setupStep := setup.Step{ID: "hooks", Title: i18n.Message{ID: "wx.setup.item.hooks"}, Default: setup.ActionManual, Options: []setup.Action{setup.ActionKeep, setup.ActionManual}}
	m := newModel(context.Background(), Options{Config: config.Defaults(), Setup: []setup.Step{setupStep}})
	m.tab, m.selected = 5, 0
	updated, _ := m.activate()
	got := updated.(model)
	if got.mode != modeChoice || got.pending.command != "setup" || got.pending.defaultArgs[1] != "hooks" {
		t.Fatalf("setup item at boundary state=%+v, want setup choice", got)
	}
	if got.choice != 1 || got.choices[got.choice].value != string(setup.ActionManual) {
		t.Fatalf("setup default choice=%d choices=%+v, want manual at index 1", got.choice, got.choices)
	}

	m.selected = len(m.setupItems())
	updated, _ = m.activate()
	got = updated.(model)
	if got.pending.command != tabMenus[5][0].command || got.mode != modeConfirm {
		t.Fatalf("fixed menu after setup state=%+v, want daemon confirmation", got)
	}
	got.finishPending()
	if !slices.Equal(got.result.Args, []string{"daemon", "start"}) {
		t.Fatalf("fixed menu action=%v, want daemon start", got.result.Args)
	}
}

// TestMutationChoiceDownMovesByOne は、choice の下移動が +1 であることと、末尾で
// clamp されることを出力状態で比較する。
func TestMutationChoiceDownMovesByOne(t *testing.T) {
	m := newModel(context.Background(), Options{Config: config.Defaults()})
	m.mode, m.choices, m.choice = modeChoice, []choice{{}, {}, {}}, 0
	updated, _ := m.Update(key(tea.KeyDown))
	m = updated.(model)
	if m.choice != 1 {
		t.Fatalf("choice after down=%d, want 1", m.choice)
	}
	updated, _ = m.Update(key(tea.KeyDown))
	m = updated.(model)
	if m.choice != 2 {
		t.Fatalf("choice after second down=%d, want 2", m.choice)
	}
	updated, _ = m.Update(key(tea.KeyDown))
	if got := updated.(model).choice; got != 2 {
		t.Fatalf("choice past end=%d, want 2", got)
	}
}

// TestMutationConfigChoicesCoversCurrentAndListBoundaries は、現在値の有無と list の add/remove
// を別々に比較し、choice の先頭順を契約にする。
func TestMutationConfigChoicesCoversCurrentAndListBoundaries(t *testing.T) {
	zero := newModel(context.Background(), Options{Config: config.Config{Version: 2}})
	zero.tab, zero.settingsOpen, zero.settingsEnv = 2, true, 1
	zero.configMeta = metadataByKey(t, zero, "warm_count")
	zero.showConfigChoices()
	if len(zero.choices) == 0 || zero.choices[0].value != "" {
		t.Fatalf("empty current choices=%+v, want custom/reset first", zero.choices)
	}

	defaults := newModel(context.Background(), Options{Config: config.DefaultsV2()})
	defaults.tab, defaults.settingsOpen, defaults.settingsEnv = 2, true, 1
	defaults.configMeta = metadataByKey(t, defaults, "warm_count")
	defaults.showConfigChoices()
	if len(defaults.choices) == 0 || defaults.choices[0].value == "" || !strings.Contains(defaults.choices[0].label, "Keep current") {
		t.Fatalf("non-empty current choices=%+v, want Keep current first", defaults.choices)
	}

	list := newModel(context.Background(), Options{Config: config.DefaultsV2()})
	list.configMeta = metadataByKind(t, list, config.KindList)
	list.showConfigChoices()
	if len(list.choices) != 3 || list.choices[0].op != config.EditAdd || !list.choices[0].input || list.choices[1].op != config.EditRemove || !list.choices[1].input || list.choices[2].op != config.EditReset {
		t.Fatalf("list choices=%+v, want add/remove/reset", list.choices)
	}
}

// TestMutationTargetChoicesFilterToWorkspaces は、--all の有無を含めて workspace scope の
// target だけを選択肢に出し、system/repository を混ぜないことを比較する。
func TestMutationTargetChoicesFilterToWorkspaces(t *testing.T) {
	cfg := config.DefaultsV2()
	cfg.Workspaces["/tmp/project"] = config.Workspace{Repositories: map[string]config.Repository{"backend": {}, "frontend": {}}}
	m := newModel(context.Background(), Options{Config: cfg})
	m.pending = menuItem{targetWorkspace: true, targetAll: true}
	m.showTargetChoices()
	values := make([]string, 0, len(m.choices))
	for _, option := range m.choices {
		values = append(values, option.value)
	}
	want := []string{"--all", "/tmp/project", "/tmp/project", ""}
	if !slices.Equal(values, want) {
		t.Fatalf("target values=%v, want %v", values, want)
	}

	m.pending.targetAll = false
	m.showTargetChoices()
	values = values[:0]
	for _, option := range m.choices {
		values = append(values, option.value)
	}
	if slices.Contains(values, "--all") || len(values) != 3 {
		t.Fatalf("target values without --all=%v, want two workspace entries and custom", values)
	}
}

func TestMutationHasScopeRequiresExactMatch(t *testing.T) {
	if !hasScope([]string{"system", "workspace"}, "workspace") {
		t.Fatal("hasScope rejected an exact scope")
	}
	if hasScope([]string{"workspace-defaults"}, "workspace") {
		t.Fatal("hasScope accepted a non-exact scope")
	}
	if hasScope(nil, "workspace") {
		t.Fatal("hasScope accepted an empty scope list")
	}
}

// TestMutationBreadcrumbIncludesOnlyValidSettingsContext は、設定階層・pending label・幅 0
// の各境界を、切り詰め前の breadcrumb として比較する。
func TestMutationBreadcrumbIncludesOnlyValidSettingsContext(t *testing.T) {
	cfg := config.DefaultsV2()
	cfg.Workspaces["/tmp/project"] = config.Workspace{}
	m := newModel(context.Background(), Options{Config: cfg})
	m.tab = 1
	if got, want := m.breadcrumb(), "wx / "+m.tabName(1); got != want {
		t.Fatalf("launch breadcrumb=%q, want %q", got, want)
	}

	m.tab, m.settingsOpen, m.settingsEnv = 2, true, 3
	environment := m.configEnvironments()[m.settingsEnv]
	want := "wx / " + m.tabName(2) + " / " + m.environmentTitle(environment) + " / " + environment.label
	if got := m.breadcrumb(); got != want {
		t.Fatalf("settings breadcrumb=%q, want %q", got, want)
	}
	m.mode, m.pendingLabel = modeChoice, "Choose"
	if got := m.breadcrumb(); got != want+" / Choose" {
		t.Fatalf("choice breadcrumb=%q, want %q", got, want+" / Choose")
	}

	m.settingsEnv = len(m.configEnvironments())
	m.mode = modeList
	if got, want := m.breadcrumb(), "wx / "+m.tabName(2); got != want {
		t.Fatalf("invalid settings breadcrumb=%q, want %q", got, want)
	}
}

// TestMutationCurrentLabelsRendersEmptyAndNonEmptyValues は、空の設定値だけを em dash へ
// 置き換え、実値は保持することを ANSI 除去後の行で比較する。
func TestMutationCurrentLabelsRendersEmptyAndNonEmptyValues(t *testing.T) {
	for _, cfg := range []config.Config{config.DefaultsV2(), {Version: 2}} {
		m := newModel(context.Background(), Options{Config: cfg})
		m.tab, m.settingsOpen, m.settingsEnv = 2, true, 0
		items := m.configItems()
		fields := m.environmentFieldMap()
		labels := m.currentLabels()
		if len(items) == 0 || len(labels) != len(items) {
			t.Fatalf("items=%d labels=%d", len(items), len(labels))
		}
		field := fields[items[0].Key]
		value := field.Value
		if value == "" {
			value = "—"
		}
		want := m.settingDisplayName(items[0]) + "  " + value + " (" + field.Source + ")"
		if got := xansi.Strip(labels[0]); got != want {
			t.Fatalf("config=%+v first label=%q, want %q", cfg, got, want)
		}
	}
}

// TestMutationLeftColumnWidthUsesLowerUpperAndNaturalBounds は、幅率の下限・上限と
// natural width の三つの境界を比較する。slice の capacity は契約に含めない。
func TestMutationLeftColumnWidthUsesLowerUpperAndNaturalBounds(t *testing.T) {
	for _, test := range []struct {
		name  string
		width int
		line  string
		want  int
	}{
		{name: "minimum ratio", width: 100, line: "x", want: 30},
		{name: "minimum absolute", width: 50, line: "x", want: 24},
		{name: "natural width", width: 100, line: strings.Repeat("x", 40), want: 42},
		{name: "maximum ratio", width: 100, line: strings.Repeat("x", 100), want: 55},
	} {
		t.Run(test.name, func(t *testing.T) {
			m := newModel(context.Background(), Options{Config: config.Defaults()})
			m.width = test.width
			if got := m.leftColumnWidth([]string{test.line}); got != test.want {
				t.Fatalf("width=%d natural=%d column=%d, want %d", test.width, len(test.line), got, test.want)
			}
		})
	}
}

func TestMutationMenuLinesRendersTheEmptyState(t *testing.T) {
	m := newModel(context.Background(), Options{Config: config.Defaults()})
	m.tab, m.settingsOpen, m.catalog = 2, true, nil
	got := m.menuLines(80)
	want := []string{accent + m.menuTitle() + reset, "", "  " + m.t("dashboard.no_actions")}
	if !slices.Equal(got, want) {
		t.Fatalf("empty menu lines=%q, want %q", got, want)
	}
}

func TestMutationResultPageRowsHonorsNoticeAndMinimum(t *testing.T) {
	for _, test := range []struct {
		height int
		notice string
		want   int
	}{
		{height: 10, want: 3},
		{height: 10, notice: "notice", want: 1},
		{height: 7, want: 1},
		{height: 7, notice: "notice", want: 1},
	} {
		m := newModel(context.Background(), Options{Config: config.Defaults(), Notice: test.notice})
		m.height = test.height
		if got := m.resultPageRows(); got != test.want {
			t.Errorf("height=%d notice=%q rows=%d, want %d", test.height, test.notice, got, test.want)
		}
	}
}

// TestMutationResultViewClampsAnOutOfRangeOffset は、再表示前に offset が古い結果の
// 行数を越えても、結果の最後の行だけを表示する契約を確認する。
func TestMutationResultViewClampsAnOutOfRangeOffset(t *testing.T) {
	m := newModel(context.Background(), Options{Config: config.Defaults()})
	m.pendingLabel, m.resultText, m.height, m.offset = "result", "first\nsecond\nlast", 20, 99
	lines := m.resultView()
	if got := xansi.Strip(lines[len(lines)-1]); got != "last" {
		t.Fatalf("clamped result last line=%q, want last; lines=%q", got, lines)
	}
}

func TestMutationFooterDistinguishesScrollableResults(t *testing.T) {
	m := newModel(context.Background(), Options{Config: config.Defaults()})
	m.mode, m.height, m.resultText = modeResult, 10, strings.Repeat("line\n", 12)
	if got, want := m.footer(), m.t("dashboard.footer.scroll")+"  "+m.t("dashboard.footer.enter_esc_back"); got != want {
		t.Fatalf("scrollable result footer=%q, want %q", got, want)
	}
	if !strings.Contains(xansi.Strip(m.View().Content), m.t("dashboard.footer.scroll")) {
		t.Fatal("scrollable result view omitted the scroll footer")
	}
	m.resultText = "one"
	if got, want := m.footer(), m.t("dashboard.footer.enter_esc_back"); got != want {
		t.Fatalf("short result footer=%q, want %q", got, want)
	}
	if strings.Contains(xansi.Strip(m.View().Content), m.t("dashboard.footer.scroll")) {
		t.Fatal("short result view included the scroll footer")
	}
}

func TestMutationUpdateLinesHighlightsOnlySelectedUpdate(t *testing.T) {
	m := newModel(context.Background(), Options{Config: config.Defaults()})
	m.width = 100
	m.opts.Update = UpdateInfo{Available: true, Version: "v1.2.3", URL: "https://example.test/update"}
	label := m.tf("dashboard.update.available", map[string]any{"Version": "v1.2.3"})
	wantSelected := []string{"", accent + "❯ " + reset + accent + label + reset, dim + "  https://example.test/update" + reset}
	if got := m.updateLines(); !slices.Equal(got, wantSelected) {
		t.Fatalf("selected update lines=%q, want %q", got, wantSelected)
	}
	m.selected = 1
	wantUnselected := []string{"", "  " + label, dim + "  https://example.test/update" + reset}
	if got := m.updateLines(); !slices.Equal(got, wantUnselected) {
		t.Fatalf("unselected update lines=%q, want %q", got, wantUnselected)
	}
}

// TestMutationViewNoticeAndHeightBoundaries は、notice の空文字境界と View の body 行数を
// 同時に出力比較する。内部 slice の capacity や allocation は比較しない。
func TestMutationViewNoticeAndHeightBoundaries(t *testing.T) {
	m := newModel(context.Background(), Options{Config: config.Defaults()})
	m.mode, m.pendingLabel, m.width, m.height = modeChoice, "Choose", 80, 8
	m.choices = make([]choice, 8)
	for index := range m.choices {
		m.choices[index] = choice{label: "choice"}
	}
	withoutNotice := m.View().Content
	if lines := strings.Count(withoutNotice, "\n") + 1; lines != 8 {
		t.Fatalf("view without notice has %d lines, want 8: %q", lines, withoutNotice)
	}
	m.opts.Notice = "notice"
	withNotice := m.View().Content
	if !strings.Contains(withNotice, "notice") || strings.Contains(withoutNotice, "notice") {
		t.Fatalf("notice boundary without=%q with=%q", withoutNotice, withNotice)
	}
}

func TestMutationWrapSplitsAValueAcrossLines(t *testing.T) {
	got := wrap("abcdef", 3)
	want := []string{"abc", "def"}
	if !slices.Equal(got, want) {
		t.Fatalf("wrap split=%q, want %q", got, want)
	}
}

func TestMutationDescriptionShowsChoicesOnlyForChoiceMetadata(t *testing.T) {
	m := newModel(context.Background(), Options{Config: config.DefaultsV2()})
	m.tab, m.settingsOpen, m.settingsEnv = 2, true, 0
	items := m.configItems()
	withChoices, withoutChoices := -1, -1
	for index, item := range items {
		if len(item.Choices) > 0 && withChoices < 0 {
			withChoices = index
		}
		if len(item.Choices) == 0 && withoutChoices < 0 {
			withoutChoices = index
		}
	}
	if withChoices < 0 || withoutChoices < 0 {
		t.Fatalf("system settings do not provide both choice kinds: %+v", items)
	}
	m.selected = withChoices
	with := xansi.Strip(strings.Join(m.descriptionLines(80), "\n"))
	choiceLine := m.tf("dashboard.choices", map[string]any{"Choices": strings.Join(items[withChoices].Choices, " / ")})
	if !strings.Contains(with, choiceLine) {
		t.Fatalf("choice setting description=%q, want choices line", with)
	}
	m.selected = withoutChoices
	without := xansi.Strip(strings.Join(m.descriptionLines(80), "\n"))
	emptyChoiceLine := m.tf("dashboard.choices", map[string]any{"Choices": ""})
	if strings.Contains(without, emptyChoiceLine) {
		t.Fatalf("scalar setting description=%q, did not want choices line", without)
	}
}

func metadataByKey(t *testing.T, m model, key string) config.Metadata {
	t.Helper()
	for _, meta := range m.catalog {
		if meta.Key == key {
			return meta
		}
	}
	t.Fatalf("catalog key %q is missing", key)
	return config.Metadata{}
}

func metadataByKind(t *testing.T, m model, kind config.ValueKind) config.Metadata {
	t.Helper()
	for _, meta := range m.catalog {
		if meta.Kind == kind {
			return meta
		}
	}
	t.Fatalf("catalog kind %q is missing", kind)
	return config.Metadata{}
}
