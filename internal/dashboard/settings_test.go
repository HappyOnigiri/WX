package dashboard

import (
	"context"
	"slices"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/HappyOnigiri/WX/internal/config"
	"github.com/HappyOnigiri/WX/internal/setup"
)

func TestConfigEnvironmentsAreSortedAfterGlobal(t *testing.T) {
	cfg := config.Defaults()
	cfg.Workspaces["/tmp/zeta"] = config.Workspace{}
	cfg.Workspaces["/tmp/alpha"] = config.Workspace{}
	cfg.Repositories["/src/zeta"] = config.Repository{}
	cfg.Repositories["/src/beta"] = config.Repository{}
	m := newModel(context.Background(), Options{Config: cfg})
	got := m.configEnvironments()
	if len(got) != 5 || m.environmentMenuLabel(got[0]) != "Global" || m.environmentMenuLabel(got[1]) != "Workspace  alpha" ||
		m.environmentMenuLabel(got[2]) != "Workspace  zeta" || m.environmentMenuLabel(got[3]) != "Repository  beta" || m.environmentMenuLabel(got[4]) != "Repository  zeta" {
		t.Fatalf("environments=%+v", got)
	}
}

func TestV2ConfigEnvironmentsShowWorkspaceMembershipHierarchy(t *testing.T) {
	cfg := config.DefaultsV2()
	cfg.Workspaces["/tmp/product"] = config.Workspace{Discovered: true, Repositories: map[string]config.Repository{
		"backend":  {Discovered: true},
		"frontend": {},
	}}
	m := newModel(context.Background(), Options{Config: cfg})
	got := m.configEnvironments()
	labels := make([]string, 0, len(got))
	for _, environment := range got {
		labels = append(labels, m.environmentMenuLabel(environment))
	}
	want := []string{"System", "Workspace defaults", "Repository defaults", "Workspace  product", "  Repository defaults", "  Repository  backend", "  Repository  frontend (not discovered)"}
	if !slices.Equal(labels, want) {
		t.Fatalf("v2 environments=%v, want %v", labels, want)
	}
}

func TestV2SystemEnvironmentUsesV2Fields(t *testing.T) {
	cfg := config.DefaultsV2()
	m := newModel(context.Background(), Options{Config: cfg})
	m.tab = 2
	fields := scopeFieldMap(m.environmentFields())
	if _, ok := fields["storage.worktree_root"]; !ok {
		t.Fatalf("system fields=%v, want system.storage.worktree_root", fields)
	}
	if _, ok := fields["worktree"]; ok {
		t.Fatalf("system fields include workspace key: %v", fields)
	}
}

func TestLaunchOffersRegisteredWorkspacesAndCustomInput(t *testing.T) {
	cfg := config.Defaults()
	cfg.Workspaces["/tmp/workspace-one"] = config.Workspace{}
	m := newModel(context.Background(), Options{Config: cfg})
	m.tab = 1
	updated, _ := m.Update(key(tea.KeyEnter))
	m = updated.(model)
	if m.mode != modeChoice || len(m.choices) != 2 || !strings.Contains(m.choices[0].label, "/tmp/workspace-one") || m.choices[1].value != "" {
		t.Fatalf("workspace choices=%+v mode=%v", m.choices, m.mode)
	}
	updated, _ = m.Update(key(tea.KeyEnter))
	m = updated.(model)
	if m.target != "/tmp/workspace-one" || m.mode != modeChoice || m.inputStage != "arguments-choice" {
		t.Fatalf("target=%q stage=%q", m.target, m.inputStage)
	}
}

func TestRepositoryEnvironmentBuildsRepositoryConfigAction(t *testing.T) {
	cfg := config.Defaults()
	cfg.Repositories["/tmp/repository-one"] = config.Repository{}
	m := newModel(context.Background(), Options{Config: cfg})
	m.tab, m.settingsOpen, m.settingsEnv, m.selected = 2, true, 1, 0
	m.configMeta = m.configItems()[0]
	m.pending, m.pendingLabel = menuItem{command: "config"}, m.configMeta.DisplayName
	m.target, m.editOp, m.input = "/tmp/repository-one", config.EditSet, "changed"
	m.finishPending()
	if len(m.result.Args) != 5 || m.result.Args[0] != "config" || m.result.Args[1] != "--repository" ||
		m.result.Args[2] != "/tmp/repository-one" || m.result.Args[3] != m.configMeta.Key || m.result.Args[4] != "changed" {
		t.Fatalf("repository action=%v", m.result.Args)
	}
}

func TestConfigChoicesLimitCustomInputToOpenEndedKinds(t *testing.T) {
	m := newModel(context.Background(), Options{Config: config.Defaults()})
	for _, meta := range m.catalog {
		m.configMeta = meta
		m.showConfigChoices()
		hasCustomSet := false
		for _, option := range m.choices {
			hasCustomSet = hasCustomSet || option.label == "Enter a custom value…"
		}
		wantCustomSet := meta.Kind == config.KindInteger || meta.Kind == config.KindDuration || (meta.Kind == config.KindString && len(meta.Choices) == 0)
		if hasCustomSet != wantCustomSet {
			t.Errorf("%s custom set=%v, want %v for kind=%s choices=%v", meta.Key, hasCustomSet, wantCustomSet, meta.Kind, meta.Choices)
		}
	}
}

func TestEnvironmentFieldsDistinguishExplicitAndInheritedSources(t *testing.T) {
	raw := config.Config{}
	if err := config.SetField(&raw, "pool.warm_per_workspace", "1"); err != nil {
		t.Fatal(err)
	}
	if err := config.SetScopeField(&raw, config.ScopeWorkspace, "/repo", "worktree", "hot"); err != nil {
		t.Fatal(err)
	}
	effective := config.Merge(config.Defaults(), raw)
	m := newModel(context.Background(), Options{Config: effective, RawConfig: raw})
	m.tab = 2
	global := scopeFieldMap(m.environmentFields())
	if got := global["pool.warm_per_workspace"]; got.Value != "1" || got.Source != "explicit" {
		t.Fatalf("explicit global=%+v", got)
	}
	if got := global["storage.cow_min_size_kib"]; got.Value != "16" || got.Source != "default" {
		t.Fatalf("default global=%+v", got)
	}

	m.selected = 1
	workspace := scopeFieldMap(m.environmentFields())
	if got := workspace["worktree"]; got.Value != "hot" || got.Source != "workspace" {
		t.Fatalf("workspace override=%+v", got)
	}
	if got := workspace["warm_count"]; got.Value != "1" || got.Source != "global" {
		t.Fatalf("global inheritance=%+v", got)
	}
	if got := workspace["reuse_standby"]; got.Value != "true" || got.Source != "default" {
		t.Fatalf("default inheritance=%+v", got)
	}
}

// TestConfigItemsIgnoresOutOfRangeSettingsEnvironment は、環境一覧の末尾を越えた選択位置を
// グローバル設定として扱い、設定項目の構築で panic しないことを守る。
func TestConfigItemsIgnoresOutOfRangeSettingsEnvironment(t *testing.T) {
	m := newModel(context.Background(), Options{Config: config.Defaults()})
	m.settingsOpen = true
	m.settingsEnv = len(m.configEnvironments())

	got := m.configItems()
	m.settingsOpen = false
	want := m.configItems()
	if len(got) != len(want) {
		t.Fatalf("out-of-range items=%d, global items=%d", len(got), len(want))
	}
	for index := range want {
		if got[index].Key != want[index].Key {
			t.Fatalf("item %d key=%q, want global key=%q", index, got[index].Key, want[index].Key)
		}
	}
}

func TestV2ConfigEnvironmentsSkipRepositoryMenuForSingleMember(t *testing.T) {
	cfg := config.DefaultsV2()
	cfg.Workspaces["/tmp/single"] = config.Workspace{Discovered: true, Repositories: map[string]config.Repository{
		"only": {},
	}}
	m := newModel(context.Background(), Options{Config: cfg})
	got := m.configEnvironments()
	labels := make([]string, 0, len(got))
	for _, environment := range got {
		labels = append(labels, m.environmentMenuLabel(environment))
	}
	want := []string{"System", "Workspace defaults", "Repository defaults", "Workspace  single", "  Repository defaults"}
	if !slices.Equal(labels, want) {
		t.Fatalf("v2 environments=%v, want %v", labels, want)
	}
}

func TestSetupItemsExcludeStepsWithoutChoices(t *testing.T) {
	m := newModel(context.Background(), Options{
		Config: config.Defaults(),
		Setup: []setup.Step{
			{ID: "informational"},
			{ID: "actionable", Options: []setup.Action{setup.ActionInstall}},
			{ID: "daemon", Options: []setup.Action{setup.ActionStart}},
		},
	})
	got := m.setupItems()
	if len(got) != 1 || got[0].ID != "actionable" {
		t.Fatalf("setup items=%+v, want only the actionable non-daemon step", got)
	}
}

func scopeFieldMap(fields []config.ScopeField) map[string]config.ScopeField {
	values := make(map[string]config.ScopeField, len(fields))
	for _, field := range fields {
		values[field.Key] = field
	}
	return values
}
