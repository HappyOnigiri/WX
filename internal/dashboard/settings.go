package dashboard

import (
	"path/filepath"
	"sort"
	"strings"

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
	repositoryDefaults := false
	if environments := m.configEnvironments(); m.settingsOpen && m.settingsEnv < len(environments) {
		scope = environments[m.settingsEnv].scope
		repositoryDefaults = environments[m.settingsEnv].repositoryDefaults
	}
	items := make([]config.Metadata, 0, len(m.catalog))
	for _, meta := range m.catalog {
		if !hasScope(meta.Scopes, scope) {
			continue
		}
		if m.opts.Config.V2() && scope == config.V2ScopeWorkspace {
			if meta.Key == "submodules" {
				continue
			}
			isNested := strings.HasPrefix(meta.Key, "repository_defaults.")
			if isNested != repositoryDefaults {
				continue
			}
		}
		items = append(items, meta)
	}
	return items
}

func (m model) configEnvironments() []environment {
	if m.opts.Config.V2() {
		return m.configV2Environments()
	}
	environments := []environment{{label: "Global", scope: "global"}}
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
		environments = append(environments, environment{label: label, target: path, scope: "workspace"})
	}
	paths = paths[:0]
	for path := range m.opts.Config.Repositories {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	for _, path := range paths {
		label := filepath.Base(path)
		if label == "." || label == string(filepath.Separator) || label == "" {
			label = path
		}
		environments = append(environments, environment{label: label, target: path, scope: "repository"})
	}
	return environments
}

func (m model) configV2Environments() []environment {
	environments := []environment{
		{label: "System", scope: config.V2ScopeSystem},
		{label: "Workspace", scope: config.V2ScopeWorkspaceDefaults},
		{label: "Repository", scope: config.V2ScopeRepositoryDefaults},
	}
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
		environments = append(environments, environment{label: label, target: path, scope: config.V2ScopeWorkspace, missing: !m.opts.Config.Workspaces[path].Discovered})
		// single-repository workspace には repository 個別編集階層を作らない。
		// その repository defaults は workspace の子として編集し、multi-repository
		// workspace だけ membership をさらに表示する。
		environments = append(environments, environment{label: "Repository defaults", target: path, scope: config.V2ScopeWorkspace, repositoryDefaults: true})
		members := make([]string, 0, len(m.opts.Config.Workspaces[path].Repositories))
		for relative := range m.opts.Config.Workspaces[path].Repositories {
			members = append(members, relative)
		}
		sort.Strings(members)
		if len(members) > 1 {
			for _, relative := range members {
				missing := !m.opts.Config.Workspaces[path].Repositories[relative].Discovered
				environments = append(environments, environment{label: relative, target: path, repository: relative, scope: config.V2ScopeRepository, missing: missing})
			}
		}
	}
	return environments
}

func (m model) environmentTitle(e environment) string {
	switch e.scope {
	case config.V2ScopeWorkspace:
		return m.t("dashboard.workspace")
	case config.V2ScopeRepository:
		return m.t("dashboard.repository")
	case config.V2ScopeSystem:
		return m.t("dashboard.system")
	case config.V2ScopeWorkspaceDefaults:
		return m.t("dashboard.workspace_defaults")
	case config.V2ScopeRepositoryDefaults:
		return m.t("dashboard.repository_defaults")
	default:
		return m.t("dashboard.global")
	}
}

func (m model) environmentMenuLabel(e environment) string {
	if e.scope == "global" || e.scope == config.V2ScopeSystem || e.scope == config.V2ScopeWorkspaceDefaults || e.scope == config.V2ScopeRepositoryDefaults {
		return m.environmentTitle(e)
	}
	label := e.label
	if e.missing {
		label += " " + m.t("dashboard.not_discovered")
	}
	if e.repository != "" {
		return "  " + m.environmentTitle(e) + "  " + label
	}
	if e.repositoryDefaults {
		return "  " + m.t("dashboard.repository_defaults")
	}
	return m.environmentTitle(e) + "  " + label
}

// settingDisplayName / settingDescription / settingImpact は config catalog の
// 英語のまま持つ説明を、TUI で訳がある key だけ差し替える。
func (m model) settingDisplayName(meta config.Metadata) string {
	return m.settingText(meta.Key, "name", meta.DisplayName)
}

func (m model) settingDescription(meta config.Metadata) string {
	return m.settingText(meta.Key, "description", meta.Description)
}

func (m model) settingImpact(meta config.Metadata) string {
	return m.settingText(meta.Key, "impact", meta.Impact)
}

// settingText は設定キーの散文を動的 ID で引く。ID の組み立て方は wx config describe と同じで、
// カタログに無いキーは config が持つ英語の原文へ落とす。
func (m model) settingText(key, kind, fallback string) string {
	return m.messages.LocalizeOr("config."+key+"."+kind, fallback)
}

func (e environment) configScope() config.Scope {
	if e.scope == "repository" {
		return config.ScopeRepository
	}
	return config.ScopeWorkspace
}

func (m *model) showConfigChoices() {
	meta := m.configMeta
	m.choices, m.choice = nil, 0
	if meta.Kind == config.KindInteger || meta.Kind == config.KindDuration {
		if current := m.environmentValues()[meta.Key]; current != "" {
			label := m.tf("dashboard.keep_current", map[string]any{"Value": current})
			m.choices = append(m.choices, choice{label: label, value: current, op: config.EditSet})
		}
	}
	for _, value := range meta.Choices {
		m.choices = append(m.choices, choice{label: value, value: value, op: config.EditSet})
	}
	if meta.Kind == config.KindBoolean {
		m.choices = append(m.choices,
			choice{label: m.t("dashboard.enabled"), value: "true", op: config.EditSet},
			choice{label: m.t("dashboard.disabled"), value: "false", op: config.EditSet})
	}
	if meta.Kind == config.KindInteger || meta.Kind == config.KindDuration || (len(m.choices) == 0 && meta.Kind != config.KindList) {
		m.choices = append(m.choices, choice{label: m.t("dashboard.enter_custom"), op: config.EditSet, input: true})
	}
	if meta.Kind == config.KindList {
		m.choices = append(m.choices,
			choice{label: m.t("dashboard.add_value"), op: config.EditAdd, input: true},
			choice{label: m.t("dashboard.remove_value"), op: config.EditRemove, input: true})
	}
	m.choices = append(m.choices, choice{label: m.t("dashboard.reset_default"), op: config.EditReset})
	m.mode, m.inputStage = modeChoice, ""
}

func (m *model) showWorkspaceChoices() {
	m.choices, m.choice, m.inputStage = nil, 0, "workdir-choice"
	for _, environment := range m.configEnvironments() {
		if environment.scope == "workspace" {
			m.choices = append(m.choices, choice{label: labelWithDetail(environment.label, "— "+environment.target), value: environment.target})
		}
	}
	m.choices = append(m.choices, choice{label: m.t("dashboard.another_path"), input: true})
	m.mode = modeChoice
}

func (m *model) showTargetChoices() {
	m.choices, m.choice, m.inputStage = nil, 0, "target-choice"
	if m.pending.targetAll {
		m.choices = append(m.choices, choice{label: m.t("dashboard.all_workspaces"), value: "--all"})
	}
	for _, environment := range m.configEnvironments() {
		if environment.scope == "workspace" {
			m.choices = append(m.choices, choice{label: labelWithDetail(environment.label, "— "+environment.target), value: environment.target})
		}
	}
	m.choices = append(m.choices, choice{label: m.t("dashboard.another_path"), input: true})
	m.mode = modeChoice
}

func (m *model) showArgumentChoices() {
	m.choices = append(m.choices[:0], m.pending.argumentChoices(m.messages)...)
	m.choice, m.inputStage, m.mode = 0, "arguments-choice", modeChoice
}

func (m *model) afterWorkdirChoice() {
	switch {
	case m.pending.argumentChoices != nil:
		m.showArgumentChoices()
	case m.pending.inputLabelID != "":
		m.inputHint, m.inputStage, m.mode = m.t(m.pending.inputLabelID), "workdir-args", modeInput
	default:
		m.mode = modeConfirm
	}
}

func (m *model) afterTargetChoice() {
	if m.pending.argumentChoices != nil {
		m.showArgumentChoices()
	} else {
		m.mode = modeConfirm
	}
}

func hasScope(scopes []string, scope string) bool {
	for _, candidate := range scopes {
		if candidate == scope {
			return true
		}
	}
	return false
}
