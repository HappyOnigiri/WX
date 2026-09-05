package tui

import (
	"context"
	"errors"
	"strings"

	tea "charm.land/bubbletea/v2"
	xansi "github.com/charmbracelet/x/ansi"

	"github.com/HappyOnigiri/WX/internal/sessions/scanner"
	"github.com/HappyOnigiri/WX/internal/sessions/termtext"
)

// ErrCancelled は選択を確定せずに picker を終了したことを示す。
var ErrCancelled = errors.New("session selection cancelled")

// Annotation は一覧に付ける注記と、選択を禁止する使用中フラグを持つ。
type Annotation struct {
	Text  string
	InUse bool
}

// PickOptions は picker の見出しとセッション注記を指定する。
type PickOptions struct {
	Label       string
	Annotations map[string]Annotation
}

type pickerItem struct {
	session    scanner.Session
	title      string
	annotation Annotation
}

type pickerModel struct {
	items     []pickerItem
	label     string
	selected  int
	offset    int
	width     int
	height    int
	status    string
	result    scanner.ResumeTarget
	cancelled bool
}

func newPickerModel(items []scanner.Session, opts PickOptions) pickerModel {
	m := pickerModel{
		label:  sanitizeLine(opts.Label),
		width:  80,
		height: 24,
	}
	for _, session := range items {
		tool := strings.ToLower(strings.TrimSpace(session.Tool))
		if tool != "claude" && tool != "codex" {
			continue
		}
		session.Tool = tool
		annotation := opts.Annotations[session.StableID]
		annotation.Text = sanitizeLine(annotation.Text)
		m.items = append(m.items, pickerItem{
			session:    session,
			title:      sessionTitle(session),
			annotation: annotation,
		})
	}
	m.ensureVisible()
	return m
}

// Pick は渡されたセッションだけを表示し、選択された再開先を返す。
func Pick(ctx context.Context, items []scanner.Session, opts PickOptions) (scanner.ResumeTarget, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	m := newPickerModel(items, opts)
	final, err := tea.NewProgram(m, tea.WithContext(ctx)).Run()
	if err != nil {
		if errors.Is(err, tea.ErrInterrupted) || errors.Is(err, tea.ErrProgramKilled) {
			return scanner.ResumeTarget{}, ErrCancelled
		}
		return scanner.ResumeTarget{}, err
	}
	got, ok := final.(pickerModel)
	if !ok || got.cancelled || !got.result.Resumable() {
		return scanner.ResumeTarget{}, ErrCancelled
	}
	return got.result, nil
}

func (m pickerModel) Init() tea.Cmd {
	return nil
}

func (m pickerModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = max(1, msg.Width)
		m.height = max(1, msg.Height)
		m.ensureVisible()
		return m, nil
	case tea.KeyPressMsg:
		return m.updateKey(msg.String())
	default:
		return m, nil
	}
}

func (m pickerModel) updateKey(key string) (tea.Model, tea.Cmd) {
	switch key {
	case "up", "k":
		m.selected = max(0, m.selected-1)
		m.status = ""
		m.ensureVisible()
	case "down", "j":
		m.selected = min(max(0, len(m.items)-1), m.selected+1)
		m.status = ""
		m.ensureVisible()
	case "pgup", "b":
		m.selected = max(0, m.selected-m.visibleRows())
		m.status = ""
		m.ensureVisible()
	case "pgdown", "f", "space":
		m.selected = min(max(0, len(m.items)-1), m.selected+m.visibleRows())
		m.status = ""
		m.ensureVisible()
	case "home":
		m.selected = 0
		m.status = ""
		m.ensureVisible()
	case "end":
		m.selected = max(0, len(m.items)-1)
		m.status = ""
		m.ensureVisible()
	case "enter":
		if len(m.items) == 0 {
			m.status = "セッションがありません"
			return m, nil
		}
		item := m.items[m.selected]
		if item.annotation.InUse {
			m.status = "使用中のセッションは選択できません"
			return m, nil
		}
		target := item.session.Target()
		if !target.Resumable() {
			m.status = "このセッションは再開できません"
			return m, nil
		}
		m.result = target
		return m, tea.Quit
	case "esc", "q", "ctrl+c":
		m.cancelled = true
		return m, tea.Quit
	}
	return m, nil
}

func (m *pickerModel) ensureVisible() {
	rows := m.visibleRows()
	if len(m.items) == 0 {
		m.selected = 0
		m.offset = 0
		return
	}
	m.selected = min(max(0, m.selected), len(m.items)-1)
	if m.selected < m.offset {
		m.offset = m.selected
	}
	if m.selected >= m.offset+rows {
		m.offset = m.selected - rows + 1
	}
	m.offset = min(max(0, m.offset), max(0, len(m.items)-rows))
}

func (m pickerModel) visibleRows() int {
	height := m.height
	if height <= 0 {
		height = 24
	}
	reserved := 2
	if m.status != "" {
		reserved++
	}
	return max(1, height-reserved)
}

func (m pickerModel) View() tea.View {
	m.ensureVisible()
	lines := make([]string, 0, m.height)
	header := "wx resume"
	if m.label != "" {
		header += " [" + m.label + "]"
	}
	lines = append(lines, truncateLine(header, m.width))

	rows := m.visibleRows()
	if len(m.items) == 0 {
		lines = append(lines, "  セッションがありません")
	} else {
		end := min(len(m.items), m.offset+rows)
		for i := m.offset; i < end; i++ {
			item := m.items[i]
			marker := "  "
			if i == m.selected {
				marker = "> "
			}
			line := marker + item.title
			if note := itemNote(item.annotation); note != "" {
				line += "  [" + note + "]"
			}
			lines = append(lines, truncateLine(line, m.width))
		}
	}
	if m.status != "" {
		lines = append(lines, truncateLine("! "+m.status, m.width))
	}
	lines = append(lines, truncateLine("↑↓/PgUp/PgDn 移動  Enter 選択  Esc/q キャンセル", m.width))
	if height := max(1, m.height); len(lines) > height {
		lines = lines[:height]
	}
	view := tea.NewView(strings.Join(lines, "\n"))
	view.AltScreen = true
	return view
}

func sessionTitle(session scanner.Session) string {
	title := sanitizeLine(session.Title)
	if title == "" {
		title = sanitizeLine(session.SessionID)
	}
	if title == "" {
		return "(untitled session)"
	}
	return title
}

func itemNote(annotation Annotation) string {
	note := sanitizeLine(annotation.Text)
	if annotation.InUse {
		if note == "" {
			return "使用中"
		}
		if note != "使用中" {
			note += "・使用中"
		}
	}
	return note
}

func sanitizeLine(value string) string {
	return termtext.Sanitize(value, termtext.KeepLine)
}

func truncateLine(value string, width int) string {
	if width <= 0 {
		return value
	}
	return xansi.Truncate(value, width, "…")
}
