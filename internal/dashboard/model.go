package dashboard

import (
	"context"
	"errors"
	"os"
	"strings"
	"time"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"

	"github.com/HappyOnigiri/WX/internal/config"
)

var ErrCancelled = errors.New("dashboard cancelled")

type StatusLoader func(context.Context) (string, error)

type Options struct {
	Status StatusLoader
	CWD    string
	Config config.Config
	Notice string
}

type statusMsg struct {
	text string
	err  error
	at   time.Time
}
type tickMsg time.Time

type mode int

const (
	modeList mode = iota
	modeInput
	modeConfirm
)

type model struct {
	ctx       context.Context
	opts      Options
	tab       int
	selected  int
	offset    int
	width     int
	height    int
	mode      mode
	input     string
	inputHint string
	pending   menuItem
	status    string
	statusErr string
	statusAt  time.Time
	loading   bool
	result    Action
	cancelled bool
	scope     int
	catalog   []config.Metadata
}

func Run(ctx context.Context, opts Options) (Action, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if opts.CWD == "" {
		opts.CWD, _ = os.Getwd()
	}
	m := newModel(ctx, opts)
	final, err := tea.NewProgram(m, tea.WithContext(ctx)).Run()
	if err != nil {
		return Action{}, err
	}
	got, ok := final.(model)
	if !ok || got.cancelled || len(got.result.Args) == 0 {
		return Action{}, ErrCancelled
	}
	return got.result, nil
}

func newModel(ctx context.Context, opts Options) model {
	return model{ctx: ctx, opts: opts, width: 100, height: 28, loading: true, catalog: config.Catalog()}
}

func (m model) Init() tea.Cmd {
	return tea.Batch(m.loadStatus(), tick())
}

func tick() tea.Cmd {
	return tea.Tick(2*time.Second, func(t time.Time) tea.Msg { return tickMsg(t) })
}

func (m model) loadStatus() tea.Cmd {
	loader, ctx := m.opts.Status, m.ctx
	return func() tea.Msg {
		if loader == nil {
			return statusMsg{err: errors.New("status provider is unavailable"), at: time.Now()}
		}
		text, err := loader(ctx)
		return statusMsg{text: text, err: err, at: time.Now()}
	}
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = max(1, msg.Width), max(1, msg.Height)
		m.keepVisible()
	case statusMsg:
		m.loading = false
		m.statusAt = msg.at
		if msg.err != nil {
			m.statusErr = msg.err.Error()
		} else {
			m.status, m.statusErr = strings.TrimSpace(msg.text), ""
		}
	case tickMsg:
		if m.tab == 0 && !m.loading {
			m.loading = true
			return m, tea.Batch(m.loadStatus(), tick())
		}
		return m, tick()
	case tea.PasteMsg:
		if m.mode == modeInput {
			m.input += cleanInput(msg.Content)
		}
	case tea.KeyPressMsg:
		return m.updateKey(msg)
	}
	return m, nil
}

func (m model) updateKey(key tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if key.String() == "ctrl+c" {
		m.cancelled = true
		return m, tea.Quit
	}
	if m.mode == modeInput {
		return m.updateInput(key)
	}
	if m.mode == modeConfirm {
		switch key.String() {
		case "enter", "y":
			m.finishPending()
			return m, tea.Quit
		case "esc", "n":
			m.mode = modeList
		}
		return m, nil
	}
	switch key.String() {
	case "tab":
		m.changeTab(1)
	case "shift+tab":
		m.changeTab(-1)
	case "left":
		if m.tab == 2 {
			m.scope = (m.scope + 2) % 3
			m.selected, m.offset = 0, 0
		}
	case "right":
		if m.tab == 2 {
			m.scope = (m.scope + 1) % 3
			m.selected, m.offset = 0, 0
		}
	case "up", "ctrl+p":
		m.move(-1)
	case "down", "ctrl+n":
		m.move(1)
	case "pgup":
		m.move(-m.visibleRows())
	case "pgdown":
		m.move(m.visibleRows())
	case "r":
		if m.tab == 0 && !m.loading {
			m.loading = true
			return m, m.loadStatus()
		}
	case "enter":
		return m.activate()
	case "esc", "q":
		m.cancelled = true
		return m, tea.Quit
	}
	return m, nil
}

func (m model) updateInput(key tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch key.String() {
	case "esc":
		m.mode, m.input = modeList, ""
	case "enter":
		if strings.TrimSpace(m.input) == "" && (m.pending.inputNeeded || m.pending.workDir || m.tab == 2) {
			return m, nil
		}
		m.mode = modeConfirm
	case "backspace":
		if m.input != "" {
			_, size := utf8.DecodeLastRuneInString(m.input)
			m.input = m.input[:len(m.input)-size]
		}
	default:
		if key.Mod&^(tea.ModShift|tea.ModCapsLock|tea.ModNumLock) == 0 {
			m.input += cleanInput(key.Text)
		}
	}
	return m, nil
}

func (m *model) changeTab(delta int) {
	m.tab = (m.tab + delta + len(tabNames)) % len(tabNames)
	m.selected, m.offset, m.mode, m.input = 0, 0, modeList, ""
}

func (m *model) move(delta int) {
	if m.tab == 0 {
		lineCount := len(strings.Split(m.status, "\n"))
		m.offset = min(max(0, m.offset+delta), max(0, lineCount-1))
		return
	}
	count := m.itemCount()
	if count == 0 {
		return
	}
	m.selected = min(max(0, m.selected+delta), count-1)
	m.keepVisible()
}

func (m *model) keepVisible() {
	rows := m.visibleRows()
	if m.selected < m.offset {
		m.offset = m.selected
	}
	if m.selected >= m.offset+rows {
		m.offset = m.selected - rows + 1
	}
	m.offset = max(0, m.offset)
}

func (m model) visibleRows() int { return max(2, m.height-9) }

func (m model) itemCount() int {
	if m.tab == 0 {
		return 0
	}
	if m.tab == 2 {
		return len(m.configItems())
	}
	return len(tabMenus[m.tab])
}

func (m model) configItems() []config.Metadata {
	scope := []string{"global", "workspace", "repository"}[m.scope]
	items := make([]config.Metadata, 0, len(m.catalog))
	for _, meta := range m.catalog {
		if hasScope(meta.Scopes, scope) {
			items = append(items, meta)
		}
	}
	return items
}

func (m model) activate() (tea.Model, tea.Cmd) {
	if m.tab == 0 {
		return m, nil
	}
	if m.tab == 2 {
		items := m.configItems()
		if len(items) == 0 {
			return m, nil
		}
		meta := items[m.selected]
		m.pending = menuItem{label: meta.DisplayName, command: "config", inputLabel: "新しい値（reset は --reset）", inputNeeded: true}
		m.inputHint = meta.Key
		if meta.Kind == config.KindList {
			m.inputHint += " — + value / - value / --reset"
		}
		if m.scope > 0 {
			m.inputHint += " — target path | 新しい値（reset は --reset）"
		}
		m.mode, m.input = modeInput, ""
		return m, nil
	}
	items := tabMenus[m.tab]
	if len(items) == 0 {
		return m, nil
	}
	m.pending = items[m.selected]
	if m.pending.inputLabel != "" || m.pending.workDir {
		m.inputHint = m.pending.inputLabel
		if m.pending.workDir {
			m.inputHint = "workspace path"
			if m.pending.inputLabel != "" {
				m.inputHint += " | " + m.pending.inputLabel
			}
		}
		m.mode, m.input = modeInput, ""
		return m, nil
	}
	m.mode = modeConfirm
	return m, nil
}

func (m *model) finishPending() {
	args := []string{m.pending.command}
	args = append(args, m.pending.defaultArgs...)
	workDir := ""
	input := strings.TrimSpace(m.input)
	if m.tab == 2 {
		meta := m.configItems()[m.selected]
		scope := []string{"global", "workspace", "repository"}[m.scope]
		if scope != "global" {
			parts := strings.SplitN(input, "|", 2)
			workDir = strings.TrimSpace(parts[0])
			if len(parts) == 2 {
				input = strings.TrimSpace(parts[1])
			} else {
				input = ""
			}
			args = append(args, "--"+scope, workDir)
		}
		args = append(args, meta.Key)
		switch {
		case input == "--reset":
			args = append(args, "--reset")
		case meta.Kind == config.KindList && strings.HasPrefix(input, "+ "):
			args = append(args, "--add", strings.TrimSpace(strings.TrimPrefix(input, "+")))
		case meta.Kind == config.KindList && strings.HasPrefix(input, "- "):
			args = append(args, "--remove", strings.TrimSpace(strings.TrimPrefix(input, "-")))
		default:
			args = append(args, input)
		}
	} else {
		if m.pending.workDir {
			parts := strings.SplitN(input, "|", 2)
			workDir = strings.TrimSpace(parts[0])
			if workDir == "" {
				workDir = m.opts.CWD
			}
			if len(parts) == 2 {
				input = strings.TrimSpace(parts[1])
			} else {
				input = ""
			}
		}
		args = append(args, splitArgs(input)...)
	}
	m.result = Action{Args: args, WorkDir: workDir}
}

func splitArgs(value string) []string { return strings.Fields(value) }

func cleanInput(value string) string {
	return strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == 0 || r == 0x1b {
			return -1
		}
		return r
	}, value)
}

func hasScope(scopes []string, scope string) bool {
	for _, candidate := range scopes {
		if candidate == scope {
			return true
		}
	}
	return false
}
