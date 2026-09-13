package main

import (
	"errors"
	"strings"
	"testing"

	"github.com/HappyOnigiri/WX/internal/i18n"
)

func TestLocalizeErrorTextJapaneseKeepsDynamicDetails(t *testing.T) {
	got := localizeErrorText("language must be en or ja: /tmp/config.yaml", i18n.Japanese)
	if !strings.Contains(got, "language は en または ja で指定してください") || !strings.Contains(got, "/tmp/config.yaml") {
		t.Fatalf("localized error=%q", got)
	}
}

// localizeError は nil を空文字にする。呼び出し元はこの判定に依存している。
func TestLocalizeErrorNilYieldsEmptyString(t *testing.T) {
	if got := localizeError(nil, i18n.Japanese); got != "" {
		t.Fatalf("nil error=%q", got)
	}
	if got := localizeError(errors.New("language must be en or ja"), i18n.Japanese); got == "" || strings.Contains(got, "must be") {
		t.Fatalf("localized error=%q", got)
	}
}

// localizedMessage は ID を持たないとき行ごと省けるよう空文字を返す。
func TestLocalizedMessageResolvesPerLanguage(t *testing.T) {
	msg := localizedMessage{id: "daemon.no_answer", data: map[string]any{"Label": "com.example.wx", "Socket": "/tmp/wx.sock", "Timeout": "60s"}}
	english := msg.text(i18n.New(string(i18n.English)))
	if english != "launchd was asked to start com.example.wx but no daemon answered /tmp/wx.sock within 60s" {
		t.Fatalf("english=%q", english)
	}
	japanese := msg.text(i18n.New(string(i18n.Japanese)))
	// Label・Socket・Timeout は payload 由来なので、日本語でも原文のまま残る。
	for _, want := range []string{"com.example.wx", "/tmp/wx.sock", "60s", "起動を依頼しました"} {
		if !strings.Contains(japanese, want) {
			t.Fatalf("japanese=%q missing %q", japanese, want)
		}
	}
	if empty := (localizedMessage{}); !empty.empty() || empty.text(i18n.New(string(i18n.Japanese))) != "" {
		t.Fatalf("empty message resolved to %q", empty.text(i18n.New(string(i18n.Japanese))))
	}
}
