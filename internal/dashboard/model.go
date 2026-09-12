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
	"github.com/HappyOnigiri/WX/internal/setup"
)

var ErrCancelled = errors.New("dashboard cancelled")

type (
	StatusLoader func(context.Context) (string, error)
	ActionRunner func(context.Context, Action) (string, int)
	Refresher    func(context.Context) (config.Config, []setup.Step, error)
)

type Options struct {
	Status  StatusLoader
	CWD     string
	Config  config.Config
	Setup   []setup.Step
	Notice  string
	Execute ActionRunner
	Refresh Refresher
}

type statusMsg struct {
	text string
	err  error
	at   time.Time
}
type (
	tickMsg      time.Time
	executionMsg struct {
		text   string
		code   int
		config config.Config
		setup  []setup.Step
		err    error
	}
)

type mode int

const (
	modeList mode = iota
	modeInput
	modeConfirm
	modeChoice
	modeRunning
	modeResult
)

type choice struct {
	label string
	value string
	op    config.EditOperation
}

type environment struct {
	label  string
	target string
	scope  string
}

type model struct {
	ctx          context.Context
	opts         Options
	tab          int
	selected     int
	offset       int
	width        int
	height       int
	mode         mode
	input        string
	inputHint    string
	inputStage   string
	pending      menuItem
	configMeta   config.Metadata
	target       string
	editOp       config.EditOperation
	choices      []choice
	choice       int
	status       string
	statusErr    string
	statusAt     time.Time
	loading      bool
	result       Action
	resultText   string
	resultCode   int
	cancelled    bool
	settingsEnv  int
	settingsOpen bool
	catalog      []config.Metadata
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
		if m.mode == modeResult {
			m.offset = min(m.offset, m.maxResultOffset())
		} else {
			m.keepVisible()
		}
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
	case executionMsg:
		m.loading = false
		m.resultText, m.resultCode = strings.TrimSpace(msg.text), msg.code
		if msg.err != nil {
			if m.resultText != "" {
				m.resultText += "\n"
			}
			m.resultText += "Refresh failed: " + msg.err.Error()
		} else {
			selectedScope := ""
			if environments := m.configEnvironments(); m.settingsOpen && m.settingsEnv < len(environments) {
				selectedScope = environments[m.settingsEnv].scope
			}
			m.opts.Config, m.opts.Setup = msg.config, msg.setup
			if m.target != "" {
				switch selectedScope {
				case "workspace":
					if m.opts.Config.Workspaces == nil {
						m.opts.Config.Workspaces = map[string]config.Workspace{}
					}
					if _, ok := m.opts.Config.Workspaces[m.target]; !ok {
						m.opts.Config.Workspaces[m.target] = config.Workspace{}
					}
				case "repository":
					if m.opts.Config.Repositories == nil {
						m.opts.Config.Repositories = map[string]config.Repository{}
					}
					if _, ok := m.opts.Config.Repositories[m.target]; !ok {
						m.opts.Config.Repositories[m.target] = config.Repository{}
					}
				}
				for index, environment := range m.configEnvironments() {
					if environment.scope == selectedScope && environment.target == m.target {
						m.settingsEnv = index
						break
					}
				}
			}
		}
		m.mode, m.offset = modeResult, 0
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
		if m.mode == modeRunning {
			return m, nil
		}
		m.cancelled = true
		return m, tea.Quit
	}
	if m.mode == modeInput {
		return m.updateInput(key)
	}
	if m.mode == modeChoice {
		switch key.String() {
		case "up", "ctrl+p":
			m.choice = max(0, m.choice-1)
		case "down", "ctrl+n":
			m.choice = min(len(m.choices)-1, m.choice+1)
		case "enter":
			return m.choose()
		case "esc":
			m.mode = modeList
		}
		return m, nil
	}
	if m.mode == modeRunning {
		return m, nil
	}
	if m.mode == modeResult {
		switch key.String() {
		case "up", "ctrl+p":
			m.offset = max(0, m.offset-1)
		case "down", "ctrl+n":
			m.offset = min(m.maxResultOffset(), m.offset+1)
		case "enter", "esc":
			m.mode, m.offset = modeList, 0
		}
		return m, nil
	}
	if m.mode == modeConfirm {
		switch key.String() {
		case "enter", "y":
			m.finishPending()
			if m.pending.external || m.opts.Execute == nil {
				return m, tea.Quit
			}
			action := m.result
			m.result = Action{}
			m.mode, m.loading = modeRunning, true
			return m, m.execute(action)
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
		m.changeTab(-1)
	case "right":
		m.changeTab(1)
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
		if m.tab == 2 && m.settingsOpen {
			m.settingsOpen = false
			m.selected, m.offset = m.settingsEnv, 0
			return m, nil
		}
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
		required := m.pending.inputNeeded
		if m.inputStage == "workdir" || (m.inputStage != "workdir-args" && m.pending.workDir) {
			required = true
		}
		if strings.TrimSpace(m.input) == "" && required {
			return m, nil
		}
		switch m.inputStage {
		case "config-target":
			m.target, m.input = strings.TrimSpace(m.input), ""
			m.showConfigChoices()
		case "config-value", "setup-value":
			m.mode = modeConfirm
		case "workdir":
			m.target, m.input = strings.TrimSpace(m.input), ""
			if m.pending.inputLabel != "" {
				m.inputHint, m.inputStage = m.pending.inputLabel, "workdir-args"
			} else {
				m.mode = modeConfirm
			}
		case "workdir-args":
			m.mode = modeConfirm
		default:
			m.mode = modeConfirm
		}
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
	m.selected, m.offset, m.mode, m.input, m.settingsOpen = 0, 0, modeList, "", false
}

func (m *model) move(delta int) {
	if m.tab == 0 {
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
		if !m.settingsOpen {
			return len(m.configEnvironments())
		}
		return len(m.configItems())
	}
	if m.tab == 5 {
		return len(m.setupItems()) + len(tabMenus[m.tab])
	}
	return len(tabMenus[m.tab])
}

func (m model) activate() (tea.Model, tea.Cmd) {
	if m.tab == 0 {
		return m, nil
	}
	if m.tab == 2 {
		if !m.settingsOpen {
			environments := m.configEnvironments()
			if len(environments) == 0 {
				return m, nil
			}
			m.settingsEnv, m.settingsOpen = m.selected, true
			m.selected, m.offset = 0, 0
			return m, nil
		}
		items := m.configItems()
		if len(items) == 0 {
			return m, nil
		}
		meta := items[m.selected]
		m.pending = menuItem{label: meta.DisplayName, command: "config"}
		m.configMeta, m.target = meta, ""
		if environment := m.configEnvironments()[m.settingsEnv]; environment.scope != "global" {
			m.target = environment.target
		}
		m.showConfigChoices()
		return m, nil
	}
	if m.tab == 5 {
		setupItems := m.setupItems()
		if m.selected < len(setupItems) {
			step := setupItems[m.selected]
			m.pending = menuItem{label: step.Title, command: "setup", defaultArgs: []string{"--item", step.ID}}
			m.choices = make([]choice, 0, len(step.Options))
			for _, option := range step.Options {
				m.choices = append(m.choices, choice{label: strings.ToUpper(string(option)), value: string(option)})
				if option == step.Default {
					m.choice = len(m.choices) - 1
				}
			}
			m.mode = modeChoice
			return m, nil
		}
		m.pending = tabMenus[m.tab][m.selected-len(setupItems)]
	} else {
		items := tabMenus[m.tab]
		if len(items) == 0 {
			return m, nil
		}
		m.pending = items[m.selected]
	}
	if m.pending.workDir {
		m.showWorkspaceChoices()
		return m, nil
	}
	if m.pending.inputLabel != "" {
		m.inputHint = m.pending.inputLabel
		m.mode, m.input = modeInput, ""
		return m, nil
	}
	m.mode = modeConfirm
	return m, nil
}

func (m model) choose() (tea.Model, tea.Cmd) {
	if len(m.choices) == 0 {
		return m, nil
	}
	selected := m.choices[m.choice]
	if m.tab == 2 {
		m.editOp = selected.op
		m.input = selected.value
		if selected.op != config.EditReset && selected.value == "" {
			m.inputHint, m.inputStage = "Value", "config-value"
			m.mode = modeInput
		} else {
			m.mode = modeConfirm
		}
		return m, nil
	}
	if m.tab == 5 && m.pending.command == "setup" {
		m.pending.defaultArgs = append(m.pending.defaultArgs, "--action", selected.value)
		if selected.value == string(setup.ActionManual) {
			m.inputHint, m.inputStage, m.input = "Value", "setup-value", ""
			m.mode = modeInput
		} else {
			m.mode = modeConfirm
		}
		return m, nil
	}
	if m.pending.workDir {
		m.target, m.input = selected.value, ""
		switch {
		case selected.value == "":
			m.inputHint, m.inputStage = "Workspace path", "workdir"
			m.mode = modeInput
		case m.pending.inputLabel != "":
			m.inputHint, m.inputStage = m.pending.inputLabel, "workdir-args"
			m.mode = modeInput
		default:
			m.mode = modeConfirm
		}
	}
	return m, nil
}

func (m *model) finishPending() {
	args := []string{m.pending.command}
	args = append(args, m.pending.defaultArgs...)
	workDir := ""
	input := strings.TrimSpace(m.input)
	switch {
	case m.tab == 2:
		meta := m.configItems()[m.selected]
		scope := m.configEnvironments()[m.settingsEnv].scope
		if scope != "global" {
			workDir = m.target
			args = append(args, "--"+scope, workDir)
		}
		args = append(args, meta.Key)
		switch m.editOp {
		case config.EditReset:
			args = append(args, "--reset")
		case config.EditAdd:
			args = append(args, "--add", input)
		case config.EditRemove:
			args = append(args, "--remove", input)
		case config.EditSet:
			args = append(args, input)
		default:
			args = append(args, input)
		}
	case m.tab == 5 && m.pending.command == "setup":
		if m.inputStage == "setup-value" {
			args = append(args, "--value", input)
		}
	default:
		if m.pending.workDir {
			workDir = m.target
			if workDir == "" {
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
		}
		args = append(args, splitArgs(input)...)
	}
	m.result = Action{Args: args, WorkDir: workDir}
}

func (m model) execute(action Action) tea.Cmd {
	runner, refresh, ctx := m.opts.Execute, m.opts.Refresh, m.ctx
	return func() tea.Msg {
		text, code := runner(ctx, action)
		msg := executionMsg{text: text, code: code}
		if refresh != nil {
			msg.config, msg.setup, msg.err = refresh(ctx)
		}
		return msg
	}
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
