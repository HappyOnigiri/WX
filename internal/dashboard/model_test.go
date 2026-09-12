package dashboard

import (
	"context"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/HappyOnigiri/WX/internal/config"
)

func TestModelBuildsActionWithExplicitWorkspace(t *testing.T) {
	m := newModel(context.Background(), Options{CWD: "/fallback", Config: config.Defaults()})
	m.tab = 1
	updated, _ := m.Update(key(tea.KeyEnter))
	m = updated.(model)
	if m.mode != modeInput {
		t.Fatalf("mode=%v, want input", m.mode)
	}
	m.input = "/tmp/project | --dangerously-skip-permissions"
	updated, _ = m.Update(key(tea.KeyEnter))
	m = updated.(model)
	updated, _ = m.Update(key(tea.KeyEnter))
	m = updated.(model)
	if m.result.WorkDir != "/tmp/project" {
		t.Fatalf("workdir=%q", m.result.WorkDir)
	}
	if got := m.result.Args; len(got) != 2 || got[0] != "claude" || got[1] != "--dangerously-skip-permissions" {
		t.Fatalf("args=%v", got)
	}
}

func TestModelKeepsLastStatusWhenRefreshFails(t *testing.T) {
	m := newModel(context.Background(), Options{Config: config.Defaults()})
	updated, _ := m.Update(statusMsg{text: "Daemon running", at: testTime()})
	m = updated.(model)
	updated, _ = m.Update(statusMsg{err: context.DeadlineExceeded, at: testTime()})
	m = updated.(model)
	if m.status != "Daemon running" || m.statusErr == "" {
		t.Fatalf("status=%q err=%q", m.status, m.statusErr)
	}
}

func key(code rune) tea.KeyPressMsg { return tea.KeyPressMsg{Code: code} }
