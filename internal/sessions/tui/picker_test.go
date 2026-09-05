package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	xansi "github.com/charmbracelet/x/ansi"

	"github.com/HappyOnigiri/WX/internal/sessions/scanner"
)

func TestPickerFiltersToolsAndFallsBackToSessionID(t *testing.T) {
	m := newPickerModel([]scanner.Session{
		{Tool: "cursor", SessionID: "cursor-id", Title: "hidden"},
		{Tool: "claude", SessionID: "claude-id", Title: "\x1b[31mHello\x1b[0m\nunsafe"},
		{Tool: "codex", SessionID: "codex-id"},
	}, PickOptions{})
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

func TestPickerNavigationUsesViewport(t *testing.T) {
	items := make([]scanner.Session, 6)
	for i := range items {
		items[i] = scanner.Session{Tool: "claude", SessionID: "session-id", Title: "session"}
	}
	m := newPickerModel(items, PickOptions{})
	result, _ := m.Update(tea.WindowSizeMsg{Width: 24, Height: 5})
	m = result.(pickerModel)
	if got := m.visibleRows(); got != 3 {
		t.Fatalf("visible rows=%d, want 3", got)
	}

	for range 4 {
		result, _ = m.Update(keyPress("down"))
		m = result.(pickerModel)
	}
	if m.selected != 4 || m.offset != 2 {
		t.Fatalf("after down selected=%d offset=%d, want 4/2", m.selected, m.offset)
	}
	result, _ = m.Update(keyPress("pgup"))
	m = result.(pickerModel)
	if m.selected != 1 || m.offset != 1 {
		t.Fatalf("after pgup selected=%d offset=%d, want 1/1", m.selected, m.offset)
	}
	if lines := strings.Count(m.View().Content, "\n") + 1; lines > 5 {
		t.Fatalf("view lines=%d exceed height 5", lines)
	}
}

func TestPickerEnterRejectsInUseAndReturnsTarget(t *testing.T) {
	items := []scanner.Session{
		{Tool: "claude", SessionID: "busy-id", Title: "busy", StableID: "busy"},
		{Tool: "codex", SessionID: "ready-id", Title: "ready", CWD: "/workspace", StableID: "ready"},
	}
	m := newPickerModel(items, PickOptions{
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

func TestPickerCancelKeysQuit(t *testing.T) {
	for _, key := range []string{"esc", "q", "ctrl+c"} {
		t.Run(key, func(t *testing.T) {
			m := newPickerModel([]scanner.Session{{Tool: "claude", SessionID: "id"}}, PickOptions{})
			result, cmd := m.Update(keyPress(key))
			got := result.(pickerModel)
			if cmd == nil || !got.cancelled {
				t.Fatalf("key %s cancelled=%v cmd=%v", key, got.cancelled, cmd)
			}
		})
	}
}

func TestPickerViewShowsLabelAnnotationAndFitsWidth(t *testing.T) {
	m := newPickerModel([]scanner.Session{{
		Tool:      "claude",
		SessionID: "id",
		Title:     "タイトル",
		StableID:  "stable",
	}}, PickOptions{
		Label:       "workspace",
		Annotations: map[string]Annotation{"stable": {Text: "復元可"}},
	})
	result, _ := m.Update(tea.WindowSizeMsg{Width: 40, Height: 8})
	m = result.(pickerModel)
	view := m.View().Content
	if !strings.Contains(view, "workspace") || !strings.Contains(view, "復元可") {
		t.Fatalf("picker view omitted label/annotation: %q", view)
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
	case "enter":
		key.Code = tea.KeyEnter
	case "esc":
		key.Code = tea.KeyEscape
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
