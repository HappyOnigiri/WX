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
			footer = "←/→ tabs  ↑/↓ select  Enter edit  Esc environments"
		} else {
			footer = "←/→ tabs  ↑/↓ select environment  Enter open  Esc exit"
		}
	case m.mode == modeResult:
		footer = "↑/↓ scroll result  Enter/Esc back"
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
			parts = append(parts, activeTab+" "+name+" "+reset)
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
			crumb += " / " + environments[m.settingsEnv].label
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
		if i == m.selected {
			marker = accent + "❯ " + reset
		}
		lines = append(lines, truncate(marker+labels[i], width))
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
				labels = append(labels, environment.label)
			}
			return labels
		}
		items := m.configItems()
		values := m.environmentValues()
		labels := make([]string, 0, len(items))
		for _, item := range items {
			value := values[item.Key]
			if value == "" {
				value = "inherited / unset"
			}
			labels = append(labels, item.DisplayName+"  "+dim+value+reset)
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
			lines := []string{soft + environment.label + reset}
			if environment.target != "" {
				lines = append(lines, dim+environment.target+reset)
			}
			lines = append(lines, "", warn+"Effective settings"+reset)
			if environment.target == "" {
				for _, field := range append(config.Fields(m.opts.Config), config.Lists(m.opts.Config)...) {
					lines = append(lines, truncate(field.Key+" = "+field.Value, width))
				}
			} else {
				for _, field := range config.ScopeFields(m.opts.Config, config.ScopeWorkspace, environment.target) {
					lines = append(lines, truncate(field.Key+" = "+field.Value+" ("+field.Source+")", width))
				}
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
		dim + "For workspace operations, use: workspace path | additional arguments" + reset,
		dim + "Enter confirm  Esc back" + reset,
	}
}

func (m model) choiceView() []string {
	lines := []string{accent + m.pending.label + reset, "", "Choose a value or action:"}
	for index, option := range m.choices {
		marker := "  "
		if index == m.choice {
			marker = accent + "❯ " + reset
		}
		lines = append(lines, marker+option.label)
	}
	return append(lines, "", dim+"↑/↓ select  Enter confirm  Esc back"+reset)
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
	if m.pending.workDir {
		lines = append(lines, "Target: "+strings.TrimSpace(strings.SplitN(m.input, "|", 2)[0]))
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
	result := strings.Split(m.resultText, "\n")
	if len(result) == 1 && result[0] == "" {
		result[0] = "The operation produced no output."
	}
	start := min(m.offset, max(0, len(result)-1))
	end := min(len(result), start+max(1, m.visibleRows()-2))
	return append(lines, result[start:end]...)
}

func configValues(cfg config.Config) map[string]string {
	values := map[string]string{}
	for _, field := range append(config.Fields(cfg), config.Lists(cfg)...) {
		values[field.Key] = field.Value
	}
	return values
}

func (m model) environmentValues() map[string]string {
	if m.settingsEnv == 0 {
		return configValues(m.opts.Config)
	}
	environments := m.configEnvironments()
	if m.settingsEnv >= len(environments) {
		return map[string]string{}
	}
	values := map[string]string{}
	for _, field := range config.ScopeFields(m.opts.Config, config.ScopeWorkspace, environments[m.settingsEnv].target) {
		values[field.Key] = field.Value
	}
	return values
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
