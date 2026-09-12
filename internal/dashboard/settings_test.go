package dashboard

import (
	"context"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/HappyOnigiri/WX/internal/config"
)

func TestConfigEnvironmentsAreSortedAfterGlobal(t *testing.T) {
	cfg := config.Defaults()
	cfg.Workspaces["/tmp/zeta"] = config.Workspace{}
	cfg.Workspaces["/tmp/alpha"] = config.Workspace{}
	cfg.Repositories["/src/zeta"] = config.Repository{}
	cfg.Repositories["/src/beta"] = config.Repository{}
	m := newModel(context.Background(), Options{Config: cfg})
	got := m.configEnvironments()
	if len(got) != 5 || got[0].menuLabel() != "Global" || got[1].menuLabel() != "Workspace  alpha" ||
		got[2].menuLabel() != "Workspace  zeta" || got[3].menuLabel() != "Repository  beta" || got[4].menuLabel() != "Repository  zeta" {
		t.Fatalf("environments=%+v", got)
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
	if m.target != "/tmp/workspace-one" || m.inputStage != "workdir-args" {
		t.Fatalf("target=%q stage=%q", m.target, m.inputStage)
	}
}

func TestRepositoryEnvironmentBuildsRepositoryConfigAction(t *testing.T) {
	cfg := config.Defaults()
	cfg.Repositories["/tmp/repository-one"] = config.Repository{}
	m := newModel(context.Background(), Options{Config: cfg})
	m.tab, m.settingsOpen, m.settingsEnv, m.selected = 2, true, 1, 0
	m.configMeta = m.configItems()[0]
	m.pending = menuItem{label: m.configMeta.DisplayName, command: "config"}
	m.target, m.editOp, m.input = "/tmp/repository-one", config.EditSet, "changed"
	m.finishPending()
	if len(m.result.Args) != 5 || m.result.Args[0] != "config" || m.result.Args[1] != "--repository" ||
		m.result.Args[2] != "/tmp/repository-one" || m.result.Args[3] != m.configMeta.Key || m.result.Args[4] != "changed" {
		t.Fatalf("repository action=%v", m.result.Args)
	}
}
