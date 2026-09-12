package dashboard

import (
	"strings"
	"testing"

	"github.com/HappyOnigiri/WX/internal/i18n"
)

func TestTranslateDashboardJapanese(t *testing.T) {
	got := translateDashboard("System status\nChoose what to launch\n/tmp/Database\n/tmp/System", i18n.Japanese)
	if !strings.Contains(got, "システム状態") || !strings.Contains(got, "起動するものを選択") {
		t.Fatalf("Japanese dashboard=%q", got)
	}
	if !strings.Contains(got, "/tmp/Database") || !strings.Contains(got, "/tmp/System") {
		t.Fatalf("opaque dashboard value changed: %q", got)
	}
}
