package cli

import (
	"strings"
	"testing"

	"github.com/HappyOnigiri/WX/internal/i18n"
)

func TestLocalizeCLIMessageJapanese(t *testing.T) {
	got := localizeCLIMessage("wx daemon is unavailable: /tmp/wx.sock", i18n.Japanese)
	if !strings.Contains(got, "wx daemon は利用できません") {
		t.Fatalf("localized message=%q", got)
	}
	if !strings.Contains(got, "/tmp/wx.sock") {
		t.Fatalf("opaque detail was changed: %q", got)
	}
}

func TestCLILanguageDefaultsToEnglish(t *testing.T) {
	var c Client
	if got := cliLanguage(c); got != i18n.English {
		t.Fatalf("default CLI language=%q", got)
	}
}
