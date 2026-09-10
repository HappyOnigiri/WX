package tui

import (
	"context"
	"errors"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	xansi "github.com/charmbracelet/x/ansi"

	"github.com/HappyOnigiri/WX/internal/sessions/scanner"
	"github.com/HappyOnigiri/WX/internal/sessions/termtext"
	"github.com/HappyOnigiri/WX/internal/textfmt"
)

// ErrCancelled は選択を確定せずに picker を終了したことを示す。
var ErrCancelled = errors.New("session selection cancelled")

// Annotation は一覧に付ける注記と、選択を禁止する使用中フラグを持つ。
type Annotation struct {
	Text  string
	InUse bool
}

// ScopeFilter は「この workspace のみ」で絞る対象を StableID で表す。
// PickOptions.Scope が nil のときは scope を判定できないため、絞り込みも scope の表示も行わない。
type ScopeFilter struct {
	InScope map[string]bool
}

func (s *ScopeFilter) contains(stableID string) bool {
	if s == nil {
		return true
	}
	return s.InScope[stableID]
}

// PickOptions は picker の見出しとセッション注記、scope 判定を指定する。
type PickOptions struct {
	Label       string
	Annotations map[string]Annotation
	Scope       *ScopeFilter
	// Now は相対時刻の基準時刻で、ゼロ値なら time.Now() を使う。テストが表示を固定するための差し替え点である。
	Now time.Time
}

type pickerItem struct {
	session    scanner.Session
	title      string
	age        string // 相対時刻。2 行目の先頭に出す
	cwd        string // ホームを短縮した作業ディレクトリ
	size       string // JSONL のバイト数
	annotation Annotation
	inScope    bool
	haystack   string // 検索用にタイトルと cwd を小文字で連結したもの
}

// メタ行の組み立て規則。path が長いときに時刻とサイズを残すため、path へ回す幅の下限を決めておく。
const (
	metaIndent    = "    "
	metaSeparator = " · "
	minPathWidth  = 8
)

func metaJoin(age, path, size string) string {
	parts := make([]string, 0, 3)
	for _, part := range []string{age, path, size} {
		if part != "" {
			parts = append(parts, part)
		}
	}
	return metaIndent + strings.Join(parts, metaSeparator)
}

// metaLine は 2 行目を width に収める。幅が足りないときは path を先に削り、
// 識別に効く末尾を残して先頭を省略する。それでも足りなければ path ごと落とす。
func (i pickerItem) metaLine(width int) string {
	line := metaJoin(i.age, i.cwd, i.size)
	if i.cwd == "" || width <= 0 || xansi.StringWidth(line) <= width {
		return truncateLine(line, width)
	}
	kept := metaJoin(i.age, "", i.size)
	budget := width - xansi.StringWidth(kept) - xansi.StringWidth(metaSeparator)
	if budget < minPathWidth {
		return truncateLine(kept, width)
	}
	path := xansi.TruncateLeft(i.cwd, xansi.StringWidth(i.cwd)-budget+1, "…")
	return truncateLine(metaJoin(i.age, path, i.size), width)
}

type pickerModel struct {
	items []pickerItem
	// visible は表示対象の items 添字。scope 状態と検索語から導出し、selected と offset はこの列に対する位置である。
	visible    []int
	label      string
	query      string
	scoped     bool // この workspace のみに絞っているか
	scopeAware bool // scope を判定できるか。false なら Ctrl-A も scope 表示も出さない
	selected   int
	offset     int
	width      int
	height     int
	status     string
	result     scanner.ResumeTarget
	cancelled  bool
}

func newPickerModel(items []scanner.Session, opts PickOptions) pickerModel {
	now := opts.Now
	if now.IsZero() {
		now = time.Now()
	}
	m := pickerModel{
		label:      sanitizeLine(opts.Label),
		scopeAware: opts.Scope != nil,
		scoped:     opts.Scope != nil,
		width:      80,
		height:     24,
	}
	for _, session := range items {
		tool := strings.ToLower(strings.TrimSpace(session.Tool))
		if tool != "claude" && tool != "codex" {
			continue
		}
		session.Tool = tool
		annotation := opts.Annotations[session.StableID]
		annotation.Text = sanitizeLine(annotation.Text)
		title := sessionTitle(session)
		cwd := sanitizeLine(textfmt.HomePath(session.CWD))
		m.items = append(m.items, pickerItem{
			session:    session,
			title:      title,
			age:        sessionAge(session, now),
			cwd:        cwd,
			size:       sessionSize(session),
			annotation: annotation,
			inScope:    opts.Scope.contains(session.StableID),
			haystack:   strings.ToLower(title + "\x00" + cwd),
		})
	}
	m.refresh()
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
		return m.updateKey(msg)
	default:
		return m, nil
	}
}

// updateKey は移動・検索・scope 切替を処理する。
// 素の文字は検索語として消費するため、j / k / q / space は移動やキャンセルに割り当てない。
func (m pickerModel) updateKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "up", "ctrl+p":
		m.moveBy(-1)
	case "down", "ctrl+n":
		m.moveBy(1)
	case "pgup":
		m.moveBy(-m.visibleRows())
	case "pgdown":
		m.moveBy(m.visibleRows())
	case "home":
		m.moveTo(0)
	case "end":
		m.moveTo(len(m.visible) - 1)
	case "enter":
		return m.confirm()
	case "ctrl+a":
		if m.scopeAware {
			m.scoped = !m.scoped
			m.status = ""
			m.resetSelection()
		}
	case "backspace":
		if m.query != "" {
			runes := []rune(m.query)
			m.query = string(runes[:len(runes)-1])
			m.status = ""
			m.resetSelection()
		}
	case "ctrl+u":
		if m.query != "" {
			m.query = ""
			m.status = ""
			m.resetSelection()
		}
	case "esc":
		// 検索語があるときはまずそれを消し、空のときだけ選択を諦める。
		if m.query != "" {
			m.query = ""
			m.status = ""
			m.resetSelection()
			return m, nil
		}
		m.cancelled = true
		return m, tea.Quit
	case "ctrl+c":
		m.cancelled = true
		return m, tea.Quit
	default:
		if text := searchText(msg); text != "" {
			m.query += text
			m.status = ""
			m.resetSelection()
		}
	}
	return m, nil
}

// searchText は検索語へ足す印字可能文字を取り出す。ctrl や alt が付いた入力は検索語にしない。
// Caps Lock・Num Lock は Kitty keyboard protocol の端末で印字可能文字にも付くため、除いてから判定する。
func searchText(msg tea.KeyPressMsg) string {
	if msg.Mod&^(tea.ModShift|tea.ModCapsLock|tea.ModNumLock) != 0 {
		return ""
	}
	return termtext.Sanitize(msg.Text, "")
}

func (m *pickerModel) moveBy(delta int) {
	m.moveTo(m.selected + delta)
}

func (m *pickerModel) moveTo(index int) {
	m.selected = min(max(0, index), max(0, len(m.visible)-1))
	m.status = ""
	m.ensureVisible()
}

// resetSelection は表示対象が変わったときに選択位置を先頭へ戻す。
func (m *pickerModel) resetSelection() {
	m.refresh()
	m.selected = 0
	m.offset = 0
	m.ensureVisible()
}

func (m pickerModel) confirm() (tea.Model, tea.Cmd) {
	if len(m.visible) == 0 {
		m.status = m.emptyMessage()
		return m, nil
	}
	item := m.items[m.visible[m.selected]]
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
}

// refresh は scope 状態と検索語から表示対象を作り直す。1 回の走査結果だけを使い、再走査はしない。
func (m *pickerModel) refresh() {
	query := strings.ToLower(m.query)
	m.visible = make([]int, 0, len(m.items))
	for i, item := range m.items {
		if m.scoped && !item.inScope {
			continue
		}
		if query != "" && !strings.Contains(item.haystack, query) {
			continue
		}
		m.visible = append(m.visible, i)
	}
	m.ensureVisible()
}

func (m *pickerModel) ensureVisible() {
	rows := m.visibleRows()
	if len(m.visible) == 0 {
		m.selected = 0
		m.offset = 0
		return
	}
	m.selected = min(max(0, m.selected), len(m.visible)-1)
	if m.selected < m.offset {
		m.offset = m.selected
	}
	if m.selected >= m.offset+rows {
		m.offset = m.selected - rows + 1
	}
	m.offset = min(max(0, m.offset), max(0, len(m.visible)-rows))
}

// visibleRows は同時に出せる会話の件数を返す。1 件がタイトルとメタの 2 行を占め、
// ヘッダ・検索行・フッタ（と status 行）は常に残す。狭い端末でも 1 件は出す。
func (m pickerModel) visibleRows() int {
	height := m.height
	if height <= 0 {
		height = 24
	}
	reserved := 3
	if m.status != "" {
		reserved++
	}
	return max(1, (height-reserved)/2)
}

// emptyMessage は表示対象が無い理由を、会話そのものが無い場合と絞り込みで消えた場合で分ける。
func (m pickerModel) emptyMessage() string {
	if len(m.items) == 0 {
		return "セッションがありません"
	}
	return "条件に一致する会話がありません"
}

func (m pickerModel) View() tea.View {
	m.ensureVisible()
	lines := make([]string, 0, m.height)
	lines = append(lines, truncateLine(m.headerLine(), m.width))
	lines = append(lines, truncateLine("検索: "+sanitizeLine(m.query), m.width))
	if len(m.visible) == 0 {
		lines = append(lines, truncateLine("  "+m.emptyMessage(), m.width))
	} else {
		end := min(len(m.visible), m.offset+m.visibleRows())
		for i := m.offset; i < end; i++ {
			item := m.items[m.visible[i]]
			marker := "  "
			if i == m.selected {
				marker = "❯ "
			}
			line := marker + item.title
			if note := itemNote(item.annotation); note != "" {
				line += "  [" + note + "]"
			}
			lines = append(lines, truncateLine(line, m.width))
			lines = append(lines, item.metaLine(m.width))
		}
	}
	if m.status != "" {
		lines = append(lines, truncateLine("! "+m.status, m.width))
	}
	lines = append(lines, truncateLine(m.footerLine(), m.width))
	if height := max(1, m.height); len(lines) > height {
		lines = lines[:height]
	}
	view := tea.NewView(strings.Join(lines, "\n"))
	view.AltScreen = true
	return view
}

func (m pickerModel) headerLine() string {
	header := "wx resume"
	if m.label != "" {
		header += " [" + m.label + "]"
	}
	if !m.scopeAware {
		return header
	}
	if m.scoped {
		return header + "  (この workspace)"
	}
	return header + "  (全 workspace)"
}

func (m pickerModel) footerLine() string {
	footer := "↑↓/Ctrl-N/Ctrl-P/PgUp/PgDn 移動  Enter 選択  文字入力 検索"
	if m.scopeAware {
		footer += "  Ctrl-A workspace切替"
	}
	return footer + "  Esc キャンセル"
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

// sessionAge は mtime を相対表記へ直す。mtime を持たない会話では空を返し、起点不明の時刻を出さない。
func sessionAge(session scanner.Session, now time.Time) string {
	if session.Mtime <= 0 {
		return ""
	}
	seconds := int64(session.Mtime)
	nanos := int64((session.Mtime - float64(seconds)) * 1e9)
	return termtext.RelativeTime(time.Unix(seconds, nanos), now)
}

// sessionSize は空ファイルと未取得を区別できないため、0 バイトでは何も出さない。
func sessionSize(session scanner.Session) string {
	if session.Size <= 0 {
		return ""
	}
	return textfmt.HumanBytes(session.Size)
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
