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

// TestTranslateDashboardKeepsLongerLabelsIntact は、短いラベルが長いラベルの
// 一部を先に置き換えて長い方が一致しなくなる退行を防ぐ。
func TestTranslateDashboardKeepsLongerLabelsIntact(t *testing.T) {
	for source, want := range map[string]string{
		"Maintenance operation": "保守操作",
		"Enter/Esc back":        "Enter/Esc で戻る",
		"Editable settings":     "編集できる設定",
	} {
		if got := translateDashboard(source, i18n.Japanese); got != want {
			t.Fatalf("translate(%q)=%q, want %q", source, got, want)
		}
	}
}

// TestTranslateDashboardFooterHints は、語中の `/` を path として退避していた
// ために英語のまま残っていた footer の操作案内を守る。
func TestTranslateDashboardFooterHints(t *testing.T) {
	got := translateDashboard("↑/↓ select  Enter confirm  ←/Esc back  /tmp/wx/slot", i18n.Japanese)
	if got != "↑/↓ で選択  Enter で確定  ←/Esc で戻る  /tmp/wx/slot" {
		t.Fatalf("footer=%q", got)
	}
}
