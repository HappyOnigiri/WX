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

func TestLocalizeDaemonAndSetupTextJapanese(t *testing.T) {
	got := localizeDaemonText("the daemon is not managed by launchd", i18n.Japanese)
	if got != "daemon は launchd に管理されていません" {
		t.Fatalf("daemon text=%q", got)
	}
	if got := localizeDaemonText("the daemon is not managed by launchd", i18n.English); got != "the daemon is not managed by launchd" {
		t.Fatalf("English daemon text=%q", got)
	}
	setup := localizeSetupError("setup item hooks.claude action manual requires --value", i18n.Japanese)
	if !strings.Contains(setup, "setup 項目 hooks.claude") || !strings.Contains(setup, "--value が必要です") {
		t.Fatalf("setup error=%q", setup)
	}
}
