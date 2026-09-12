package dashboard

import (
	"context"
	"strings"
	"testing"
	"time"

	xansi "github.com/charmbracelet/x/ansi"

	"github.com/HappyOnigiri/WX/internal/config"
)

func TestViewUsesStatusPaneAndResponsiveOperationLayout(t *testing.T) {
	m := newModel(context.Background(), Options{Config: config.Defaults()})
	m.loading = false
	m.status = "WORKSPACE  POLICY  READY  IN USE  LAST USED\n~/wx       hot     2      1       now"
	m.width, m.height = 100, 24
	wide := m.View().Content
	if !strings.Contains(wide, "System status") || strings.Contains(wide, " │ ") {
		t.Fatalf("status view is not a single pane: %q", wide)
	}
	m.tab, m.width = 1, 70
	narrow := m.View().Content
	if !strings.Contains(narrow, "Choose what to launch") || !strings.Contains(narrow, "selected workspace") {
		t.Fatalf("narrow operation view omitted stacked content: %q", narrow)
	}
	for _, line := range strings.Split(narrow, "\n") {
		if xansi.StringWidth(line) > 70 {
			t.Fatalf("line exceeds width: %q", line)
		}
	}
}

func TestSelectedTabUsesBackgroundInsteadOfBrackets(t *testing.T) {
	m := newModel(context.Background(), Options{Config: config.Defaults()})
	line := m.tabLine()
	if strings.Contains(line, "[Status]") {
		t.Fatalf("selected tab still uses brackets: %q", line)
	}
	if !strings.Contains(line, "\x1b[48;5;43mStatus") {
		t.Fatalf("selected tab has no background highlight: %q", line)
	}
	want := xansi.Strip(line)
	for tab := range tabNames {
		m.tab = tab
		if got := xansi.Strip(m.tabLine()); got != want {
			t.Fatalf("tab %d changed tab positions: got %q, want %q", tab, got, want)
		}
	}
}

func testTime() time.Time { return time.Date(2026, 9, 12, 12, 0, 0, 0, time.Local) }
