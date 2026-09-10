package tui

import (
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	xansi "github.com/charmbracelet/x/ansi"

	"github.com/HappyOnigiri/WX/internal/sessions/scanner"
)

// fixedNow は相対時刻の表示を固定するための基準時刻である。
var fixedNow = time.Date(2026, 2, 3, 12, 0, 0, 0, time.UTC)

func TestPickerFiltersToolsAndFallsBackToSessionID(t *testing.T) {
	m := newPickerModel([]scanner.Session{
		{Tool: "cursor", SessionID: "cursor-id", Title: "hidden"},
		{Tool: "claude", SessionID: "claude-id", Title: "\x1b[31mHello\x1b[0m\nunsafe"},
		{Tool: "codex", SessionID: "codex-id"},
	}, PickOptions{Now: fixedNow})
	if len(m.items) != 2 {
		t.Fatalf("picker items=%d, want 2", len(m.items))
	}
	if m.items[0].title != "Hellounsafe" {
		t.Errorf("sanitized title=%q", m.items[0].title)
	}
	if m.items[1].title != "codex-id" {
		t.Errorf("empty title fallback=%q", m.items[1].title)
	}
}

// TestPickerMetaLineShowsAgeHomePathAndSize は 2 行目の項目と、値が無い項目を落とすことを確かめる。
func TestPickerMetaLineShowsAgeHomePathAndSize(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	m := newPickerModel([]scanner.Session{
		{Tool: "claude", SessionID: "full", Title: "full", CWD: home + "/wx/repo", Mtime: float64(fixedNow.Add(-7 * time.Hour).Unix()), Size: 1468006},
		{Tool: "claude", SessionID: "bare", Title: "bare"},
	}, PickOptions{Now: fixedNow})
	if got, want := m.items[0].metaLine(80), "    7時間前 · ~/wx/repo · 1.4 MiB"; got != want {
		t.Errorf("meta=%q, want %q", got, want)
	}
	if got := m.items[1].metaLine(80); got != metaIndent {
		t.Errorf("meta without mtime/cwd/size=%q, want indent only", got)
	}

	// 幅が足りないときは path の先頭だけを省き、相対時刻とサイズは残す。
	narrow := m.items[0].metaLine(32)
	if !strings.Contains(narrow, "7時間前") || !strings.Contains(narrow, "1.4 MiB") || !strings.Contains(narrow, "…") {
		t.Errorf("narrow meta=%q", narrow)
	}
	if got := displayWidth(narrow); got > 32 {
		t.Errorf("narrow meta width=%d: %q", got, narrow)
	}
	// path へ回す幅が下限を割ると path ごと落ちる。
	if got := m.items[0].metaLine(28); strings.Contains(got, "~") || !strings.Contains(got, "1.4 MiB") {
		t.Errorf("path dropped meta=%q", got)
	}
}

func TestPickerNavigationUsesTwoLineRows(t *testing.T) {
	items := make([]scanner.Session, 6)
	for i := range items {
		items[i] = scanner.Session{Tool: "claude", SessionID: "session-id", Title: "session"}
	}
	m := newPickerModel(items, PickOptions{Now: fixedNow})
	result, _ := m.Update(tea.WindowSizeMsg{Width: 24, Height: 11})
	m = result.(pickerModel)
	if got := m.visibleRows(); got != 4 {
		t.Fatalf("visible rows=%d, want 4", got)
	}

	for range 4 {
		result, _ = m.Update(keyPress("down"))
		m = result.(pickerModel)
	}
	if m.selected != 4 || m.offset != 1 {
		t.Fatalf("after down selected=%d offset=%d, want 4/1", m.selected, m.offset)
	}
	result, _ = m.Update(keyPress("ctrl+p"))
	m = result.(pickerModel)
	if m.selected != 3 {
		t.Fatalf("after ctrl+p selected=%d, want 3", m.selected)
	}
	result, _ = m.Update(keyPress("pgup"))
	m = result.(pickerModel)
	if m.selected != 0 || m.offset != 0 {
		t.Fatalf("after pgup selected=%d offset=%d, want 0/0", m.selected, m.offset)
	}
	result, _ = m.Update(keyPress("end"))
	m = result.(pickerModel)
	if m.selected != 5 {
		t.Fatalf("after end selected=%d, want 5", m.selected)
	}
	if lines := strings.Count(m.View().Content, "\n") + 1; lines > 11 {
		t.Fatalf("view lines=%d exceed height 11", lines)
	}

	// 狭い端末でも 1 件（2 行）は残す。
	result, _ = m.Update(tea.WindowSizeMsg{Width: 24, Height: 2})
	m = result.(pickerModel)
	if got := m.visibleRows(); got != 1 {
		t.Fatalf("narrow visible rows=%d, want 1", got)
	}
}

func TestPickerEnterRejectsInUseAndReturnsTarget(t *testing.T) {
	items := []scanner.Session{
		{Tool: "claude", SessionID: "busy-id", Title: "busy", StableID: "busy"},
		{Tool: "codex", SessionID: "ready-id", Title: "ready", CWD: "/workspace", StableID: "ready"},
	}
	m := newPickerModel(items, PickOptions{
		Now:         fixedNow,
		Annotations: map[string]Annotation{"busy": {Text: "作業中", InUse: true}},
	})
	result, cmd := m.Update(keyPress("enter"))
	m = result.(pickerModel)
	if cmd != nil || m.status == "" || m.result.Resumable() {
		t.Fatalf("in-use enter status=%q target=%+v cmd=%v", m.status, m.result, cmd)
	}

	result, _ = m.Update(keyPress("down"))
	m = result.(pickerModel)
	result, cmd = m.Update(keyPress("enter"))
	m = result.(pickerModel)
	if cmd == nil || !m.result.Resumable() || m.result.SessionID != "ready-id" {
		t.Fatalf("selected target=%+v cmd=%v", m.result, cmd)
	}
}

// TestPickerSearchNarrowsTitleAndPath は文字入力での絞り込みと、0 件・backspace・esc の扱いを確かめる。
func TestPickerSearchNarrowsTitleAndPath(t *testing.T) {
	items := []scanner.Session{
		{Tool: "claude", SessionID: "a", Title: "Worktree の調査", CWD: "/one", StableID: "a"},
		{Tool: "claude", SessionID: "b", Title: "ルール追加", CWD: "/two/worktree-x", StableID: "b"},
		{Tool: "claude", SessionID: "c", Title: "無関係", CWD: "/three", StableID: "c"},
	}
	m := newPickerModel(items, PickOptions{Now: fixedNow})
	for _, key := range []string{"w", "o", "r", "k"} {
		result, _ := m.Update(keyPress(key))
		m = result.(pickerModel)
	}
	if m.query != "work" || len(m.visible) != 2 {
		t.Fatalf("query=%q visible=%v, want work/2 items", m.query, m.visible)
	}

	result, _ := m.Update(keyPress("z"))
	m = result.(pickerModel)
	if len(m.visible) != 0 {
		t.Fatalf("no-match visible=%v", m.visible)
	}
	view := m.View().Content
	if !strings.Contains(view, "条件に一致する会話がありません") || strings.Contains(view, "セッションがありません") {
		t.Fatalf("no-match view=%q", view)
	}
	result, cmd := m.Update(keyPress("enter"))
	m = result.(pickerModel)
	if cmd != nil || m.result.Resumable() || m.status == "" {
		t.Fatalf("no-match enter status=%q cmd=%v", m.status, cmd)
	}

	result, _ = m.Update(keyPress("backspace"))
	m = result.(pickerModel)
	if m.query != "work" || len(m.visible) != 2 || m.selected != 0 {
		t.Fatalf("after backspace query=%q visible=%v selected=%d", m.query, m.visible, m.selected)
	}

	// 検索語があるうちの esc は語を消すだけで、選択は諦めない。
	result, cmd = m.Update(keyPress("esc"))
	m = result.(pickerModel)
	if cmd != nil || m.cancelled || m.query != "" || len(m.visible) != 3 {
		t.Fatalf("esc with query cancelled=%v query=%q visible=%v", m.cancelled, m.query, m.visible)
	}
}

// TestPickerCtrlUClearsQuery は Ctrl-U が検索語を一度に消し、表示を全件へ戻すことを確かめる。
func TestPickerCtrlUClearsQuery(t *testing.T) {
	items := []scanner.Session{
		{Tool: "claude", SessionID: "a", Title: "Worktree の調査", CWD: "/one", StableID: "a"},
		{Tool: "claude", SessionID: "b", Title: "無関係", CWD: "/other", StableID: "b"},
	}
	m := newPickerModel(items, PickOptions{Now: fixedNow})
	for _, key := range []string{"w", "o", "r"} {
		result, _ := m.Update(keyPress(key))
		m = result.(pickerModel)
	}
	if m.query != "wor" || len(m.visible) != 1 {
		t.Fatalf("before ctrl+u query=%q visible=%v", m.query, m.visible)
	}
	result, cmd := m.Update(keyPress("ctrl+u"))
	m = result.(pickerModel)
	if cmd != nil || m.cancelled || m.query != "" || len(m.visible) != 2 || m.selected != 0 {
		t.Fatalf("after ctrl+u query=%q visible=%v selected=%d cancelled=%v", m.query, m.visible, m.selected, m.cancelled)
	}
}

// TestPickerSearchAcceptsLockModifiers は Caps Lock 中の文字入力が検索語になり、
// ctrl や alt が付いた入力は検索語にならないことを確かめる。
func TestPickerSearchAcceptsLockModifiers(t *testing.T) {
	items := []scanner.Session{
		{Tool: "claude", SessionID: "a", Title: "WORKTREE", CWD: "/one", StableID: "a"},
		{Tool: "claude", SessionID: "b", Title: "無関係", CWD: "/other", StableID: "b"},
	}
	m := newPickerModel(items, PickOptions{Now: fixedNow})
	result, _ := m.Update(tea.KeyPressMsg{Code: 'w', Text: "W", Mod: tea.ModCapsLock | tea.ModNumLock})
	m = result.(pickerModel)
	if m.query != "W" || len(m.visible) != 1 {
		t.Fatalf("caps lock input query=%q visible=%v", m.query, m.visible)
	}
	result, _ = m.Update(tea.KeyPressMsg{Code: 'x', Text: "x", Mod: tea.ModAlt})
	m = result.(pickerModel)
	if m.query != "W" {
		t.Fatalf("alt input query=%q, want it ignored", m.query)
	}
}

// TestPickerScopeToggleReusesOneScan は Ctrl-A が走査結果のフラグだけで表示範囲を切り替えることを確かめる。
func TestPickerScopeToggleReusesOneScan(t *testing.T) {
	items := []scanner.Session{
		{Tool: "claude", SessionID: "here", Title: "here", CWD: "/one", StableID: "here"},
		{Tool: "claude", SessionID: "elsewhere", Title: "elsewhere", CWD: "/two", StableID: "elsewhere"},
	}
	m := newPickerModel(items, PickOptions{Now: fixedNow, Scope: &ScopeFilter{InScope: map[string]bool{"here": true}}})
	if !m.scoped || len(m.visible) != 1 {
		t.Fatalf("initial scoped=%v visible=%v", m.scoped, m.visible)
	}
	if view := m.View().Content; !strings.Contains(view, "(この workspace)") {
		t.Fatalf("scoped header missing: %q", view)
	}

	result, _ := m.Update(keyPress("ctrl+a"))
	m = result.(pickerModel)
	if m.scoped || len(m.visible) != 2 || m.selected != 0 {
		t.Fatalf("widened scoped=%v visible=%v selected=%d", m.scoped, m.visible, m.selected)
	}
	if view := m.View().Content; !strings.Contains(view, "(全 workspace") || !strings.Contains(view, "elsewhere") {
		t.Fatalf("widened view=%q", view)
	}

	// scope を判定できないときは切替も表示もしない。
	unaware := newPickerModel(items, PickOptions{Now: fixedNow})
	if unaware.scoped || len(unaware.visible) != 2 {
		t.Fatalf("scope-unaware scoped=%v visible=%v", unaware.scoped, unaware.visible)
	}
	result, _ = unaware.Update(keyPress("ctrl+a"))
	unaware = result.(pickerModel)
	if unaware.scoped {
		t.Fatal("ctrl+a toggled scope without scope information")
	}
	if view := unaware.View().Content; strings.Contains(view, "workspace") {
		t.Fatalf("scope-unaware view mentions workspace: %q", view)
	}
}

// TestPickerWidenedViewMarksUnknownUsage は scope 外の会話に使用状況の未判定が付くことを確かめる。
func TestPickerWidenedViewMarksUnknownUsage(t *testing.T) {
	items := []scanner.Session{
		{Tool: "claude", SessionID: "here", Title: "here", CWD: "/one", StableID: "here"},
		{Tool: "claude", SessionID: "elsewhere", Title: "elsewhere", CWD: "/two", StableID: "elsewhere"},
	}
	m := newPickerModel(items, PickOptions{
		Now:         fixedNow,
		Scope:       &ScopeFilter{InScope: map[string]bool{"here": true}},
		Annotations: map[string]Annotation{"here": {Text: "復元可"}},
	})
	if got := m.itemNote(m.items[0]); got != "復元可" {
		t.Fatalf("in-scope note=%q, want the annotation only", got)
	}
	if got := m.itemNote(m.items[1]); got != "使用状況不明" {
		t.Fatalf("out-of-scope note=%q", got)
	}

	result, _ := m.Update(keyPress("ctrl+a"))
	m = result.(pickerModel)
	if view := m.View().Content; !strings.Contains(view, "使用状況不明") || !strings.Contains(view, "使用状況は未判定") {
		t.Fatalf("widened view=%q", view)
	}

	// scope を判定できないときは、判定していないことも表示しない。
	unaware := newPickerModel(items, PickOptions{Now: fixedNow})
	if got := unaware.itemNote(unaware.items[1]); got != "" {
		t.Fatalf("scope-unaware note=%q", got)
	}
}

// TestPickerCancelKeysQuit は esc と ctrl+c だけがキャンセルで、q は検索語になることを確かめる。
func TestPickerCancelKeysQuit(t *testing.T) {
	for _, key := range []string{"esc", "ctrl+c"} {
		t.Run(key, func(t *testing.T) {
			m := newPickerModel([]scanner.Session{{Tool: "claude", SessionID: "id"}}, PickOptions{Now: fixedNow})
			result, cmd := m.Update(keyPress(key))
			got := result.(pickerModel)
			if cmd == nil || !got.cancelled {
				t.Fatalf("key %s cancelled=%v cmd=%v", key, got.cancelled, cmd)
			}
		})
	}
	m := newPickerModel([]scanner.Session{{Tool: "claude", SessionID: "id", Title: "q-title"}}, PickOptions{Now: fixedNow})
	result, cmd := m.Update(keyPress("q"))
	got := result.(pickerModel)
	if cmd != nil || got.cancelled || got.query != "q" {
		t.Fatalf("q cancelled=%v query=%q cmd=%v", got.cancelled, got.query, cmd)
	}
}

// TestPickerFooterListsEveryBinding は案内に実装したキーが揃っていることを確かめる。
func TestPickerFooterListsEveryBinding(t *testing.T) {
	items := []scanner.Session{{Tool: "claude", SessionID: "id", Title: "title", StableID: "id"}}
	scoped := newPickerModel(items, PickOptions{Now: fixedNow, Scope: &ScopeFilter{InScope: map[string]bool{"id": true}}})
	footer := scoped.footerLine()
	for _, want := range []string{"↑↓", "Ctrl-N", "Ctrl-P", "PgUp", "PgDn", "Home/End", "Enter", "文字入力", "Ctrl-U", "Ctrl-A", "Esc 検索語クリア→キャンセル"} {
		if !strings.Contains(footer, want) {
			t.Fatalf("footer omitted %q: %q", want, footer)
		}
	}
	// scope を判定できないときだけ Ctrl-A を出さない。
	if got := newPickerModel(items, PickOptions{Now: fixedNow}).footerLine(); strings.Contains(got, "Ctrl-A") {
		t.Fatalf("scope-unaware footer=%q", got)
	}
}

func TestPickerViewShowsLabelAnnotationAndFitsWidth(t *testing.T) {
	m := newPickerModel([]scanner.Session{{
		Tool:      "claude",
		SessionID: "id",
		Title:     "タイトル",
		CWD:       "/very/long/workspace/path/that/never/fits",
		Mtime:     float64(fixedNow.Add(-2 * time.Hour).Unix()),
		Size:      254464,
		StableID:  "stable",
	}}, PickOptions{
		Now:         fixedNow,
		Label:       "workspace",
		Annotations: map[string]Annotation{"stable": {Text: "復元可"}},
	})
	result, _ := m.Update(tea.WindowSizeMsg{Width: 40, Height: 10})
	m = result.(pickerModel)
	view := m.View().Content
	for _, want := range []string{"workspace", "復元可", "2時間前", "248.5 KiB", "検索: "} {
		if !strings.Contains(view, want) {
			t.Fatalf("picker view omitted %q: %q", want, view)
		}
	}
	for _, line := range strings.Split(view, "\n") {
		if got := displayWidth(line); got > 40 {
			t.Fatalf("line width=%d: %q", got, line)
		}
	}
}

func keyPress(name string) tea.KeyPressMsg {
	key := tea.KeyPressMsg{}
	switch name {
	case "up":
		key.Code = tea.KeyUp
	case "down":
		key.Code = tea.KeyDown
	case "pgup":
		key.Code = tea.KeyPgUp
	case "pgdown":
		key.Code = tea.KeyPgDown
	case "home":
		key.Code = tea.KeyHome
	case "end":
		key.Code = tea.KeyEnd
	case "enter":
		key.Code = tea.KeyEnter
	case "esc":
		key.Code = tea.KeyEscape
	case "backspace":
		key.Code = tea.KeyBackspace
	default:
		if strings.HasPrefix(name, "ctrl+") {
			key.Code = rune(name[len("ctrl+")])
			key.Mod = tea.ModCtrl
		} else {
			key.Text = name
			if name != "" {
				key.Code = rune(name[0])
			}
		}
	}
	return key
}

func displayWidth(s string) int {
	return xansi.StringWidth(s)
}
