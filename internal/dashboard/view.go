package dashboard

import (
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	xansi "github.com/charmbracelet/x/ansi"

	"github.com/HappyOnigiri/WX/internal/config"
)

const (
	accent    = "\x1b[38;5;43m"
	activeTab = "\x1b[38;5;16m\x1b[48;5;43m"
	soft      = "\x1b[38;5;110m"
	dim       = "\x1b[38;5;245m"
	warn      = "\x1b[38;5;214m"
	reset     = "\x1b[0m"
)

func (m model) View() tea.View {
	lines := []string{m.tabLine(), dim + m.breadcrumb() + reset, ""}
	if m.opts.Notice != "" {
		lines = append(lines, soft+truncate(m.opts.Notice, m.width)+reset, "")
	}
	switch {
	case m.mode == modeInput:
		lines = append(lines, m.inputView()...)
	case m.mode == modeChoice:
		lines = append(lines, m.choiceView()...)
	case m.mode == modeConfirm:
		lines = append(lines, m.confirmView()...)
	case m.mode == modeRunning:
		lines = append(lines, m.runningView()...)
	case m.mode == modeResult:
		lines = append(lines, m.resultView()...)
	case m.tab == 0:
		lines = append(lines, m.statusView()...)
	default:
		lines = append(lines, m.operationView()...)
	}
	footer := "←/→ or Tab/Shift+Tab tabs  ↑/↓ select  Enter confirm  Esc exit"
	switch {
	case m.tab == 0 && m.mode == modeList:
		footer = "←/→ or Tab/Shift+Tab tabs  r refresh  Esc exit"
	case m.tab == 2 && m.mode == modeList:
		if m.settingsOpen {
			footer = "←/Esc environments  → tabs  ↑/↓ select  Enter edit"
		} else {
			footer = "←/→ tabs  ↑/↓ select environment  Enter open  Esc exit"
		}
	case m.mode == modeResult:
		if m.maxResultOffset() > 0 {
			footer = "↑/↓ scroll result  Enter/Esc back"
		} else {
			footer = "Enter/Esc back"
		}
	}
	lines = fitLines(lines, max(1, m.height-2), m.width)
	lines = append(lines, strings.Repeat("─", max(1, m.width)), dim+truncate(footer, m.width)+reset)
	view := tea.NewView(strings.Join(lines, "\n"))
	view.AltScreen = true
	view.WindowTitle = "wx control desk"
	return view
}

func (m model) tabLine() string {
	parts := []string{accent + "◉ wx" + reset}
	for i, name := range tabNames {
		if i == m.tab {
			parts = append(parts, activeTab+name+reset)
		} else {
			parts = append(parts, dim+name+reset)
		}
	}
	return truncate(strings.Join(parts, "  "), m.width)
}

func (m model) breadcrumb() string {
	crumb := "wx / " + tabNames[m.tab]
	if m.tab == 2 {
		environments := m.configEnvironments()
		if m.settingsOpen && m.settingsEnv < len(environments) {
			environment := environments[m.settingsEnv]
			crumb += " / " + environment.title()
			if environment.scope != "global" {
				crumb += " / " + environment.label
			}
		}
	}
	if m.mode != modeList {
		crumb += " / " + m.pending.label
	}
	return truncate(crumb, m.width)
}

func (m model) statusView() []string {
	lines := []string{accent + "System status" + reset}
	if m.loading && m.status == "" {
		return append(lines, "", "  Loading from the daemon…")
	}
	if m.statusErr != "" {
		lines = append(lines, "", warn+"  Refresh failed: "+truncate(m.statusErr, max(1, m.width-18))+reset)
		if m.status != "" {
			lines = append(lines, dim+"  Showing the last successful response."+reset)
		}
	}
	if !m.statusAt.IsZero() {
		age := time.Since(m.statusAt).Round(time.Second)
		freshness := "Updated " + m.statusAt.Format("15:04:05")
		if age >= 4*time.Second {
			freshness += fmt.Sprintf(" (%s ago)", age)
		}
		lines = append(lines, dim+"  "+freshness+reset, "")
	}
	statusLines := strings.Split(m.status, "\n")
	start := 0
	end := min(len(statusLines), max(1, m.visibleRows()-3))
	if len(statusLines) == 1 && statusLines[0] == "" {
		lines = append(lines, "  No status is available.")
	} else {
		lines = append(lines, statusLines[start:end]...)
	}
	return lines
}

func (m model) operationView() []string {
	if m.width < 92 {
		return m.stackedOperationView()
	}
	leftWidth := max(34, m.width*45/100)
	rightWidth := max(20, m.width-leftWidth-3)
	left := m.menuLines(leftWidth)
	right := m.descriptionLines(rightWidth)
	height := max(len(left), len(right))
	lines := make([]string, 0, height)
	for i := 0; i < height; i++ {
		l, r := "", ""
		if i < len(left) {
			l = left[i]
		}
		if i < len(right) {
			r = right[i]
		}
		lines = append(lines, padANSI(l, leftWidth)+dim+" │ "+reset+r)
	}
	return lines
}

func (m model) stackedOperationView() []string {
	lines := m.menuLines(m.width)
	lines = append(lines, "", dim+strings.Repeat("─", max(1, m.width))+reset)
	lines = append(lines, m.descriptionLines(m.width)...)
	return lines
}

func (m model) menuLines(width int) []string {
	lines := []string{accent + m.menuTitle() + reset, ""}
	labels := m.currentLabels()
	end := min(len(labels), m.offset+m.visibleRows())
	for i := m.offset; i < end; i++ {
		marker := "  "
		label := labels[i]
		if i == m.selected {
			marker = accent + "❯ " + reset
			label = accent + label + reset
		}
		lines = append(lines, truncate(marker+label, width))
	}
	if len(labels) == 0 {
		lines = append(lines, "  No actions are available.")
	}
	return lines
}

func (m model) menuTitle() string {
	switch m.tab {
	case 1:
		return "Choose what to launch"
	case 2:
		if m.settingsOpen {
			return "Editable settings"
		}
		return "Environments"
	case 3:
		return "Diagnostic mode"
	case 4:
		return "Maintenance operation"
	default:
		return "Integrations and daemon"
	}
}

func (m model) currentLabels() []string {
	if m.tab == 2 {
		if !m.settingsOpen {
			environments := m.configEnvironments()
			labels := make([]string, 0, len(environments))
			for _, environment := range environments {
				labels = append(labels, environment.menuLabel())
			}
			return labels
		}
		items := m.configItems()
		fields := m.environmentFieldMap()
		labels := make([]string, 0, len(items))
		for _, item := range items {
			field := fields[item.Key]
			value := field.Value
			if value == "" {
				value = "—"
			}
			labels = append(labels, item.DisplayName+"  "+dim+value+" ("+field.Source+")"+reset)
		}
		return labels
	}
	if m.tab == 5 {
		steps := m.setupItems()
		labels := make([]string, 0, len(steps)+len(tabMenus[m.tab]))
		for _, step := range steps {
			labels = append(labels, step.Title+"  "+dim+string(step.State)+reset)
		}
		for _, item := range tabMenus[m.tab] {
			labels = append(labels, item.label)
		}
		return labels
	}
	items := tabMenus[m.tab]
	labels := make([]string, 0, len(items))
	for _, item := range items {
		labels = append(labels, item.label)
	}
	return labels
}

func (m model) descriptionLines(width int) []string {
	if m.itemCount() == 0 {
		return nil
	}
	if m.tab == 2 {
		if !m.settingsOpen {
			environments := m.configEnvironments()
			environment := environments[m.selected]
			lines := []string{soft + environment.menuLabel() + reset, dim + "Scope: " + environment.title() + reset}
			if environment.target != "" {
				lines = append(lines, dim+environment.target+reset)
			}
			lines = append(lines, "", warn+"Effective settings"+reset)
			for _, field := range m.environmentFields() {
				lines = append(lines, effectiveSettingLine(field, width))
			}
			return lines
		}
		meta := m.configItems()[m.selected]
		lines := []string{soft + meta.DisplayName + reset, dim + meta.Key + " · " + string(meta.Kind) + reset, ""}
		lines = append(lines, wrap(meta.Description, width)...)
		lines = append(lines, "", warn+"Impact"+reset)
		lines = append(lines, wrap(meta.Impact, width)...)
		if len(meta.Choices) > 0 {
			lines = append(lines, "", "Choices: "+strings.Join(meta.Choices, " / "))
		}
		return lines
	}
	if m.tab == 5 {
		steps := m.setupItems()
		if m.selected < len(steps) {
			step := steps[m.selected]
			lines := []string{soft + step.Title + reset, dim + step.ID + " · " + string(step.State) + reset, ""}
			lines = append(lines, wrap(step.Detail, width)...)
			if len(step.Reasons) > 0 {
				lines = append(lines, "", warn+"Attention"+reset)
				for _, reason := range step.Reasons {
					lines = append(lines, wrap(reason, width)...)
				}
			}
			return lines
		}
		return menuDescription(tabMenus[m.tab][m.selected-len(steps)], width)
	}
	return menuDescription(tabMenus[m.tab][m.selected], width)
}

func menuDescription(item menuItem, width int) []string {
	lines := []string{soft + item.label + reset, ""}
	lines = append(lines, wrap(item.description, width)...)
	lines = append(lines, "", warn+"Behavior"+reset)
	lines = append(lines, wrap(item.impact, width)...)
	if item.destructive {
		lines = append(lines, "", warn+"This operation may delete data."+reset)
	}
	return lines
}

func (m model) inputView() []string {
	return []string{
		accent + m.pending.label + reset,
		"",
		m.inputHint,
		soft + "> " + m.input + "█" + reset,
		"",
		dim + "Enter confirm  Esc back" + reset,
	}
}

func (m model) choiceView() []string {
	lines := []string{accent + m.pending.label + reset, "", "Choose a value or action:"}
	for index, option := range m.choices {
		marker := "  "
		label := option.label
		if index == m.choice {
			marker = accent + "❯ " + reset
			label = accent + label + reset
		}
		lines = append(lines, marker+label)
	}
	return append(lines, "", dim+"↑/↓ select  Enter confirm  ←/Esc back"+reset)
}

func (m model) confirmView() []string {
	lines := []string{accent + m.pending.label + reset, "", "Run this operation?"}
	if m.tab == 2 {
		meta := m.configItems()[m.selected]
		current := m.environmentValues()[meta.Key]
		if current == "" {
			current = "inherited / unset"
		}
		change := strings.TrimSpace(m.input)
		if m.editOp == config.EditReset {
			change = "Reset to default"
		}
		lines = append(lines, "Current: "+current, "Change: "+change)
	}
	if m.pending.workDir || m.pending.targetWorkspace || m.pending.targetInput {
		target := m.target
		if target == "" && m.pending.workDir {
			target = strings.TrimSpace(strings.SplitN(m.input, "|", 2)[0])
		}
		lines = append(lines, "Target: "+target)
	}
	if m.input != "" {
		lines = append(lines, "Input: "+m.input)
	}
	if m.pending.destructive {
		lines = append(lines, "", warn+"This may delete data. Check the target carefully."+reset)
	}
	return append(lines, "", soft+"Enter / y run"+reset+"  Esc / n back")
}

func (m model) runningView() []string {
	return []string{accent + m.pending.label + reset, "", "Running…", dim + "The result will appear here when the operation finishes." + reset}
}

func (m model) resultView() []string {
	title := fmt.Sprintf("%s — exit %d", m.pending.label, m.resultCode)
	lines := []string{accent + title + reset, ""}
	result := m.resultLines()
	start := min(m.offset, max(0, len(result)-1))
	end := min(len(result), start+m.resultPageRows())
	return append(lines, result[start:end]...)
}

func (m model) resultLines() []string {
	if m.resultText == "" {
		return []string{"The operation produced no output."}
	}
	return strings.Split(m.resultText, "\n")
}

// resultPageRows は View の tab・breadcrumb・結果見出しと footer を除いた出力行数である。
func (m model) resultPageRows() int {
	rows := m.height - 7
	if m.opts.Notice != "" {
		rows -= 2
	}
	return max(1, rows)
}

func (m model) maxResultOffset() int {
	return max(0, len(m.resultLines())-m.resultPageRows())
}

func (m model) environmentValues() map[string]string {
	values := map[string]string{}
	for _, field := range m.environmentFields() {
		values[field.Key] = field.Value
	}
	return values
}

func (m model) environmentFieldMap() map[string]config.ScopeField {
	values := map[string]config.ScopeField{}
	for _, field := range m.environmentFields() {
		values[field.Key] = field
	}
	return values
}

func (m model) environmentFields() []config.ScopeField {
	environmentIndex := m.settingsEnv
	if !m.settingsOpen {
		environmentIndex = m.selected
	}
	if environmentIndex == 0 {
		if m.opts.Config.V2() {
			return config.V2Fields(m.opts.Config, m.opts.RawConfig, config.V2ScopeSystem, "", "")
		}
		return config.GlobalFields(m.opts.Config, m.opts.RawConfig)
	}
	environments := m.configEnvironments()
	if environmentIndex >= len(environments) {
		return nil
	}
	environment := environments[environmentIndex]
	if m.opts.Config.V2() {
		fields := config.V2Fields(m.opts.Config, m.opts.RawConfig, environment.scope, environment.target, environment.repository)
		if environment.repositoryDefaults {
			filtered := make([]config.ScopeField, 0, len(fields))
			for _, field := range fields {
				if strings.HasPrefix(field.Key, "repository_defaults.") {
					filtered = append(filtered, field)
				}
			}
			return filtered
		}
		return fields
	}
	return config.ResolvedScopeFields(m.opts.Config, m.opts.RawConfig, environment.configScope(), environment.target)
}

func effectiveSettingLine(field config.ScopeField, width int) string {
	value := field.Value
	if value == "" {
		value = "—"
	}
	line := truncate(field.Key+" = "+value+" ("+field.Source+")", width)
	if field.Source == "explicit" || field.Source == "workspace" || field.Source == "repository" {
		return line
	}
	return dim + line + reset
}

func wrap(value string, width int) []string {
	if width <= 1 || xansi.StringWidth(value) <= width {
		return []string{value}
	}
	var lines []string
	for value != "" {
		line := xansi.Truncate(value, width, "")
		if line == "" {
			break
		}
		lines = append(lines, line)
		value = strings.TrimSpace(value[len(line):])
	}
	return lines
}

func truncate(value string, width int) string {
	if width <= 0 {
		return value
	}
	return xansi.Truncate(value, width, "…")
}

func labelWithDetail(label, detail string) string {
	return label + " " + dim + detail + reset
}

func padANSI(value string, width int) string {
	plainWidth := xansi.StringWidth(value)
	if plainWidth >= width {
		return truncate(value, width)
	}
	return value + strings.Repeat(" ", width-plainWidth)
}

func fitLines(lines []string, height, width int) []string {
	if len(lines) > height {
		lines = lines[:height]
	}
	for i := range lines {
		lines[i] = truncate(lines[i], width)
	}
	return lines
}
