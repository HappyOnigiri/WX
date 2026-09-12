package dashboard

import (
	"path/filepath"
	"sort"

	"github.com/HappyOnigiri/WX/internal/config"
	"github.com/HappyOnigiri/WX/internal/setup"
)

func (m model) setupItems() []setup.Step {
	items := make([]setup.Step, 0, len(m.opts.Setup))
	for _, step := range m.opts.Setup {
		if len(step.Options) > 0 && step.ID != "daemon" {
			items = append(items, step)
		}
	}
	return items
}

func (m model) configItems() []config.Metadata {
	scope := "global"
	if m.settingsOpen && m.settingsEnv > 0 {
		scope = "workspace"
	}
	items := make([]config.Metadata, 0, len(m.catalog))
	for _, meta := range m.catalog {
		if hasScope(meta.Scopes, scope) {
			items = append(items, meta)
		}
	}
	return items
}

func (m model) configEnvironments() []environment {
	environments := []environment{{label: "Global"}}
	paths := make([]string, 0, len(m.opts.Config.Workspaces))
	for path := range m.opts.Config.Workspaces {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	for _, path := range paths {
		label := filepath.Base(path)
		if label == "." || label == string(filepath.Separator) || label == "" {
			label = path
		}
		environments = append(environments, environment{label: label, target: path})
	}
	return environments
}

func (m *model) showConfigChoices() {
	meta := m.configMeta
	m.choices, m.choice = nil, 0
	if meta.Kind == config.KindInteger || meta.Kind == config.KindDuration {
		if current := m.environmentValues()[meta.Key]; current != "" {
			m.choices = append(m.choices, choice{label: "Keep current value: " + current, value: current, op: config.EditSet})
		}
	}
	for _, value := range meta.Choices {
		m.choices = append(m.choices, choice{label: value, value: value, op: config.EditSet})
	}
	if meta.Kind == config.KindBoolean {
		m.choices = append(m.choices,
			choice{label: "Enabled", value: "true", op: config.EditSet},
			choice{label: "Disabled", value: "false", op: config.EditSet})
	}
	if len(m.choices) == 0 || meta.Kind == config.KindInteger || meta.Kind == config.KindDuration || meta.Kind == config.KindString {
		m.choices = append(m.choices, choice{label: "Enter a custom value…", op: config.EditSet})
	}
	if meta.Kind == config.KindList {
		m.choices = append(m.choices,
			choice{label: "Add a value…", op: config.EditAdd},
			choice{label: "Remove a value…", op: config.EditRemove})
	}
	m.choices = append(m.choices, choice{label: "Reset to default", op: config.EditReset})
	m.mode, m.inputStage = modeChoice, ""
}

func hasScope(scopes []string, scope string) bool {
	for _, candidate := range scopes {
		if candidate == scope {
			return true
		}
	}
	return false
}
