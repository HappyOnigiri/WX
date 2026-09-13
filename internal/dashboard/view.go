package dashboard

import (
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

// 2 カラムの下限。左は解決済みラベルの実幅から決めるので、言語で語長が変わっても
// 区切りの位置は content と一致する。右が下限を割るときは縦積みへ落とす。
const (
	minLeftColumn  = 24
	minRightColumn = 32
	columnGap      = 3
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
	lines = fitLines(lines, max(1, m.height-2), m.width)
	lines = append(lines, strings.Repeat("─", max(1, m.width)), dim+truncate(m.footer(), m.width)+reset)
	view := tea.NewView(strings.Join(lines, "\n"))
	view.AltScreen = true
	view.WindowTitle = "wx control desk"
	return view
}

// footer は現在の画面で使える操作案内を、言語を解決した断片から組み立てる。
func (m model) footer() string {
	hint := func(ids ...string) string {
		parts := make([]string, 0, len(ids))
		for _, id := range ids {
			parts = append(parts, m.t(id))
		}
		return strings.Join(parts, "  ")
	}
	switch {
	case m.tab == 0 && m.mode == modeList && m.opts.Update.Available:
		return hint("dashboard.footer.tabs_full", "dashboard.footer.refresh", "dashboard.footer.select", "dashboard.footer.enter_confirm", "dashboard.footer.esc_exit")
	case m.tab == 0 && m.mode == modeList:
		return hint("dashboard.footer.tabs_full", "dashboard.footer.refresh", "dashboard.footer.esc_exit")
	case m.tab == 2 && m.mode == modeList && m.settingsOpen:
		return hint("dashboard.footer.env_back", "dashboard.footer.tabs_right", "dashboard.footer.select", "dashboard.footer.enter_edit")
	case m.tab == 2 && m.mode == modeList:
		return hint("dashboard.footer.tabs", "dashboard.footer.select_env", "dashboard.footer.enter_open", "dashboard.footer.esc_exit")
	case m.mode == modeResult && m.maxResultOffset() > 0:
		return hint("dashboard.footer.scroll", "dashboard.footer.enter_esc_back")
	case m.mode == modeResult:
		return hint("dashboard.footer.enter_esc_back")
	}
	return hint("dashboard.footer.tabs_full", "dashboard.footer.select", "dashboard.footer.enter_confirm", "dashboard.footer.esc_exit")
}

func (m model) tabName(tab int) string { return m.t(tabIDs[tab]) }

func (m model) tabLine() string {
	parts := []string{accent + "◉ wx" + reset}
	for i := range tabIDs {
		name := m.tabName(i)
		if i == m.tab {
			parts = append(parts, activeTab+name+reset)
		} else {
			parts = append(parts, dim+name+reset)
		}
	}
	return truncate(strings.Join(parts, "  "), m.width)
}

func (m model) breadcrumb() string {
	crumb := "wx / " + m.tabName(m.tab)
	if m.tab == 2 {
		environments := m.configEnvironments()
		if m.settingsOpen && m.settingsEnv < len(environments) {
			environment := environments[m.settingsEnv]
			crumb += " / " + m.environmentTitle(environment)
			if environment.scope != "global" {
				crumb += " / " + environment.label
			}
		}
	}
	if m.mode != modeList {
		crumb += " / " + m.pendingLabel
	}
	return truncate(crumb, m.width)
}

func (m model) statusView() []string {
	lines := []string{accent + m.t("dashboard.status") + reset}
	// 動いている wx の版を見出しの直後へ出す。daemon 側の版は status 本文が持つため重ねない。
	lines = append(lines, dim+"  "+m.tf("dashboard.version", map[string]any{"Version": m.opts.Version})+reset)
	if m.loading && m.status == "" {
		// 読み込み中でも更新項目は出す。footer と itemCount が項目ありと言う間に画面から消さない。
		return append(append(lines, "", "  "+m.t("dashboard.loading")), m.updateLines()...)
	}
	if m.statusErr != "" {
		failure := m.tf("dashboard.refresh_failed", map[string]any{"Error": truncate(m.statusErr, max(1, m.width-18))})
		lines = append(lines, "", warn+"  "+failure+reset)
		if m.status != "" {
			lines = append(lines, dim+"  "+m.t("dashboard.last_response")+reset)
		}
	}
	if !m.statusAt.IsZero() {
		age := time.Since(m.statusAt).Round(time.Second)
		data := map[string]any{"Time": m.statusAt.Format("15:04:05"), "Age": age.String()}
		freshness := m.tf("dashboard.updated", data)
		if age >= 4*time.Second {
			freshness = m.tf("dashboard.updated_age", data)
		}
		lines = append(lines, dim+"  "+freshness+reset, "")
	}
	// 本文の行数は、実際に積んだ見出し行と更新項目の行から引く。
	// 固定値で引くと行を足すたびに末尾が黙って欠ける。
	update := m.updateLines()
	statusLines := strings.Split(m.status, "\n")
	end := min(len(statusLines), max(1, m.visibleRows()-len(lines)-len(update)))
	if len(statusLines) == 1 && statusLines[0] == "" {
		lines = append(lines, "  "+m.t("dashboard.no_status"))
	} else {
		lines = append(lines, statusLines[:end]...)
	}
	return append(lines, update...)
}

// updateLines は状態画面の更新項目である。menuLines を通らないため、選択行の強調はここで書く。
func (m model) updateLines() []string {
	if !m.opts.Update.Available {
		return nil
	}
	label := m.tf("dashboard.update.available", map[string]any{"Version": m.opts.Update.Version})
	marker := "  "
	if m.selected == 0 {
		marker = accent + "❯ " + reset
		label = accent + label + reset
	}
	return []string{"", truncate(marker+label, m.width), dim + truncate("  "+m.opts.Update.URL, m.width) + reset}
}

func (m model) operationView() []string {
	if m.width < 92 {
		return m.stackedOperationView()
	}
	left := m.menuLines(m.width)
	leftWidth := m.leftColumnWidth(left)
	rightWidth := m.width - leftWidth - columnGap
	if rightWidth < minRightColumn {
		return m.stackedOperationView()
	}
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

// leftColumnWidth は解決済みのメニュー行の実幅から左カラムを決める。
// 言語ごとの語長の差を幅へ反映し、余った幅は説明側へ渡す。
func (m model) leftColumnWidth(lines []string) int {
	natural := 0
	for _, line := range lines {
		natural = max(natural, xansi.StringWidth(line))
	}
	lower := max(minLeftColumn, m.width*30/100)
	upper := max(lower, m.width*55/100)
	return min(max(natural+2, lower), upper)
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
		lines = append(lines, "  "+m.t("dashboard.no_actions"))
	}
	return lines
}

func (m model) menuTitle() string {
	switch m.tab {
	case 1:
		return m.t("dashboard.choose_launch")
	case 2:
		if m.settingsOpen {
			return m.t("dashboard.editable_settings")
		}
		return m.t("dashboard.environments")
	case 3:
		return m.t("dashboard.diagnostic_mode")
	case 4:
		return m.t("dashboard.maintenance")
	default:
		return m.t("dashboard.integrations")
	}
}

func (m model) currentLabels() []string {
	if m.tab == 2 {
		if !m.settingsOpen {
			environments := m.configEnvironments()
			labels := make([]string, 0, len(environments))
			for _, environment := range environments {
				labels = append(labels, m.environmentMenuLabel(environment))
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
			labels = append(labels, m.settingDisplayName(item)+"  "+dim+value+" ("+field.Source+")"+reset)
		}
		return labels
	}
	if m.tab == 5 {
		steps := m.setupItems()
		labels := make([]string, 0, len(steps)+len(tabMenus[m.tab]))
		for _, step := range steps {
			labels = append(labels, m.msg(step.Title)+"  "+dim+m.setupStateLabel(step.State)+reset)
		}
		for _, item := range tabMenus[m.tab] {
			labels = append(labels, m.t(item.labelID))
		}
		return labels
	}
	items := tabMenus[m.tab]
	labels := make([]string, 0, len(items))
	for _, item := range items {
		labels = append(labels, m.t(item.labelID))
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
			scope := m.tf("dashboard.scope", map[string]any{"Scope": m.environmentTitle(environment)})
			lines := []string{soft + m.environmentMenuLabel(environment) + reset, dim + scope + reset}
			if environment.target != "" {
				lines = append(lines, dim+environment.target+reset)
			}
			lines = append(lines, "", warn+m.t("dashboard.effective_settings")+reset)
			for _, field := range m.environmentFields() {
				lines = append(lines, effectiveSettingLine(field, width))
			}
			return lines
		}
		meta := m.configItems()[m.selected]
		lines := []string{soft + m.settingDisplayName(meta) + reset, dim + meta.Key + " · " + string(meta.Kind) + reset, ""}
		lines = append(lines, wrap(m.settingDescription(meta), width)...)
		lines = append(lines, "", warn+m.t("dashboard.impact")+reset)
		lines = append(lines, wrap(m.settingImpact(meta), width)...)
		if len(meta.Choices) > 0 {
			choices := m.tf("dashboard.choices", map[string]any{"Choices": strings.Join(meta.Choices, " / ")})
			lines = append(lines, "", choices)
		}
		return lines
	}
	if m.tab == 5 {
		steps := m.setupItems()
		if m.selected < len(steps) {
			step := steps[m.selected]
			lines := []string{soft + m.msg(step.Title) + reset, dim + step.ID + " · " + m.setupStateLabel(step.State) + reset, ""}
			lines = append(lines, wrap(m.msg(step.Detail), width)...)
			if step.Target != "" {
				lines = append(lines, "", warn+m.t("dashboard.target_file")+reset, dim+step.Target+reset)
			}
			if len(step.Reasons) > 0 {
				lines = append(lines, "", warn+m.t("dashboard.attention")+reset)
				for _, reason := range step.Reasons {
					lines = append(lines, wrap(m.msg(reason), width)...)
				}
			}
			return lines
		}
		return m.menuDescription(tabMenus[m.tab][m.selected-len(steps)], width)
	}
	return m.menuDescription(tabMenus[m.tab][m.selected], width)
}

func (m model) menuDescription(item menuItem, width int) []string {
	lines := []string{soft + m.t(item.labelID) + reset, ""}
	lines = append(lines, wrap(m.t(item.descriptionID), width)...)
	lines = append(lines, "", warn+m.t("dashboard.behavior")+reset)
	lines = append(lines, wrap(m.t(item.impactID), width)...)
	if item.destructive {
		lines = append(lines, "", warn+m.t("dashboard.destructive_note")+reset)
	}
	return lines
}

func (m model) inputView() []string {
	return []string{
		accent + m.pendingLabel + reset,
		"",
		m.inputHint,
		soft + "> " + m.input + "█" + reset,
		"",
		dim + m.t("dashboard.footer.enter_confirm") + "  " + m.t("dashboard.footer.esc_back") + reset,
	}
}

func (m model) choiceView() []string {
	lines := []string{accent + m.pendingLabel + reset, "", m.t("dashboard.choose_value")}
	for index, option := range m.choices {
		marker := "  "
		label := option.label
		if index == m.choice {
			marker = accent + "❯ " + reset
			label = accent + label + reset
		}
		lines = append(lines, marker+label)
	}
	hint := m.t("dashboard.footer.select") + "  " + m.t("dashboard.footer.enter_confirm") + "  " + m.t("dashboard.footer.left_esc_back")
	return append(lines, "", dim+hint+reset)
}

func (m model) confirmView() []string {
	lines := []string{accent + m.pendingLabel + reset, "", m.t("dashboard.confirm")}
	if m.tab == 2 {
		meta := m.configItems()[m.selected]
		current := m.environmentValues()[meta.Key]
		if current == "" {
			current = m.t("dashboard.inherited")
		}
		change := strings.TrimSpace(m.input)
		if m.editOp == config.EditReset {
			change = m.t("dashboard.reset_default")
		}
		lines = append(lines,
			m.tf("dashboard.current", map[string]any{"Value": current}),
			m.tf("dashboard.change", map[string]any{"Value": change}))
	}
	if m.pending.workDir || m.pending.targetWorkspace || m.pending.targetInput {
		target := m.target
		if target == "" && m.pending.workDir {
			target = strings.TrimSpace(strings.SplitN(m.input, "|", 2)[0])
		}
		lines = append(lines, m.tf("dashboard.target", map[string]any{"Value": target}))
	}
	if m.input != "" {
		lines = append(lines, m.tf("dashboard.input", map[string]any{"Value": m.input}))
	}
	if m.pending.destructive {
		lines = append(lines, "", warn+m.t("dashboard.destructive_confirm")+reset)
	}
	return append(lines, "", soft+m.t("dashboard.confirm_run")+reset+"  "+m.t("dashboard.confirm_back"))
}

func (m model) runningView() []string {
	return []string{accent + m.pendingLabel + reset, "", m.t("dashboard.running"), dim + m.t("dashboard.running_note") + reset}
}

func (m model) resultView() []string {
	title := m.tf("dashboard.result_title", map[string]any{"Label": m.pendingLabel, "Code": m.resultCode})
	lines := []string{accent + title + reset, ""}
	result := m.resultLines()
	start := min(m.offset, max(0, len(result)-1))
	end := min(len(result), start+m.resultPageRows())
	return append(lines, result[start:end]...)
}

func (m model) resultLines() []string {
	if m.resultText == "" {
		return []string{m.t("dashboard.result_empty")}
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
