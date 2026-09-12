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
	accent = "\x1b[38;5;43m"
	soft   = "\x1b[38;5;110m"
	dim    = "\x1b[38;5;245m"
	warn   = "\x1b[38;5;214m"
	reset  = "\x1b[0m"
)

func (m model) View() tea.View {
	lines := []string{m.tabLine(), dim + m.breadcrumb() + reset, ""}
	if m.opts.Notice != "" {
		lines = append(lines, soft+truncate(m.opts.Notice, m.width)+reset, "")
	}
	switch {
	case m.mode == modeInput:
		lines = append(lines, m.inputView()...)
	case m.mode == modeConfirm:
		lines = append(lines, m.confirmView()...)
	case m.tab == 0:
		lines = append(lines, m.statusView()...)
	default:
		lines = append(lines, m.operationView()...)
	}
	footer := "Tab/Shift+Tab タブ  ↑↓ 選択  Enter 決定  Esc 終了"
	if m.tab == 0 && m.mode == modeList {
		footer = "Tab/Shift+Tab タブ  r 更新  ↑↓ スクロール  Esc 終了"
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
			parts = append(parts, accent+"["+name+"]"+reset)
		} else {
			parts = append(parts, dim+name+reset)
		}
	}
	return truncate(strings.Join(parts, "  "), m.width)
}

func (m model) breadcrumb() string {
	crumb := "wx / " + tabNames[m.tab]
	if m.tab == 2 {
		crumb += " / " + []string{"Global", "Workspace", "Repository"}[m.scope]
	}
	if m.mode != modeList {
		crumb += " / " + m.pending.label
	}
	return truncate(crumb, m.width)
}

func (m model) statusView() []string {
	lines := []string{accent + "稼働状況" + reset}
	if m.loading && m.status == "" {
		return append(lines, "", "  daemon から取得しています…")
	}
	if m.statusErr != "" {
		lines = append(lines, "", warn+"  更新失敗: "+truncate(m.statusErr, max(1, m.width-10))+reset)
		if m.status != "" {
			lines = append(lines, dim+"  最後に取得した値を表示しています。"+reset)
		}
	}
	if !m.statusAt.IsZero() {
		age := time.Since(m.statusAt).Round(time.Second)
		freshness := "更新 " + m.statusAt.Format("15:04:05")
		if age >= 4*time.Second {
			freshness += fmt.Sprintf("（%s 前）", age)
		}
		lines = append(lines, dim+"  "+freshness+reset, "")
	}
	statusLines := strings.Split(m.status, "\n")
	start := min(m.offset, max(0, len(statusLines)-1))
	end := min(len(statusLines), start+max(1, m.visibleRows()-3))
	if len(statusLines) == 1 && statusLines[0] == "" {
		lines = append(lines, "  表示できる状態はありません。")
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
	lines := []string{accent + m.menuTitle() + reset}
	if m.tab == 2 {
		scopes := []string{"Global", "Workspace", "Repository"}
		parts := make([]string, len(scopes))
		for i, scope := range scopes {
			if i == m.scope {
				parts[i] = accent + "[" + scope + "]" + reset
			} else {
				parts[i] = dim + scope + reset
			}
		}
		lines = append(lines, strings.Join(parts, " "), "")
	}
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
		lines = append(lines, "  操作はありません。")
	}
	return lines
}

func (m model) menuTitle() string {
	switch m.tab {
	case 1:
		return "起動するものを選ぶ"
	case 2:
		return "設定項目"
	case 3:
		return "診断方法"
	case 4:
		return "保守操作"
	default:
		return "セットアップと daemon"
	}
}

func (m model) currentLabels() []string {
	if m.tab == 2 {
		items := m.configItems()
		values := configValues(m.opts.Config)
		labels := make([]string, 0, len(items))
		for _, item := range items {
			value := values[item.Key]
			if value == "" {
				value = "継承 / 未設定"
			}
			labels = append(labels, item.DisplayName+"  "+dim+value+reset)
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
		meta := m.configItems()[m.selected]
		lines := []string{soft + meta.DisplayName + reset, dim + meta.Key + " · " + string(meta.Kind) + reset, ""}
		lines = append(lines, wrap(meta.Description, width)...)
		lines = append(lines, "", warn+"変更の影響"+reset)
		lines = append(lines, wrap(meta.Impact, width)...)
		if len(meta.Choices) > 0 {
			lines = append(lines, "", "選択肢: "+strings.Join(meta.Choices, " / "))
		}
		return lines
	}
	item := tabMenus[m.tab][m.selected]
	lines := []string{soft + item.label + reset, ""}
	lines = append(lines, wrap(item.description, width)...)
	lines = append(lines, "", warn+"実行時の扱い"+reset)
	lines = append(lines, wrap(item.impact, width)...)
	if item.destructive {
		lines = append(lines, "", warn+"この操作はデータを削除する場合があります。"+reset)
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
		dim + "複数の値が必要な操作は workspace path | 追加引数 の形で入力します。" + reset,
		dim + "Enter 確認  Esc 戻る" + reset,
	}
}

func (m model) confirmView() []string {
	lines := []string{accent + m.pending.label + reset, "", "この操作を実行しますか？"}
	if m.tab == 2 {
		meta := m.configItems()[m.selected]
		current := configValues(m.opts.Config)[meta.Key]
		if current == "" {
			current = "継承 / 未設定"
		}
		lines = append(lines, "現在: "+current, "変更: "+strings.TrimSpace(m.input))
	}
	if m.pending.workDir {
		lines = append(lines, "対象: "+strings.TrimSpace(strings.SplitN(m.input, "|", 2)[0]))
	}
	if m.input != "" {
		lines = append(lines, "入力: "+m.input)
	}
	if m.pending.destructive {
		lines = append(lines, "", warn+"削除を伴う可能性があります。対象を確認してください。"+reset)
	}
	return append(lines, "", soft+"Enter / y 実行"+reset+"  Esc / n 戻る")
}

func configValues(cfg config.Config) map[string]string {
	values := map[string]string{}
	for _, field := range append(config.Fields(cfg), config.Lists(cfg)...) {
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
