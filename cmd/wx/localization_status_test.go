package main

import (
	"strings"
	"testing"

	"github.com/HappyOnigiri/WX/internal/i18n"
)

func TestTranslateHumanOutputJapaneseKeepsOpaqueValues(t *testing.T) {
	text := "Path: /tmp/Database\nError: permission denied\n"
	got := translateHumanOutput(text, i18n.Japanese)
	if !strings.Contains(got, "/tmp/Database") || !strings.Contains(got, "エラー") {
		t.Fatalf("Japanese output=%q", got)
	}
}

// localizeStatusHeader は見出しの先頭語だけを訳し、括弧付きの残りは原文で連結する。
func TestLocalizeStatusHeaderJapaneseKeepsSuffix(t *testing.T) {
	if got := localizeStatusHeader("LAST USED (JST)", i18n.Japanese); got != "最終使用 (JST)" {
		t.Fatalf("Japanese header=%q", got)
	}
	if got := localizeStatusHeader("LAST USED (JST)", i18n.English); got != "LAST USED (JST)" {
		t.Fatalf("English header=%q", got)
	}
	if got := localizeStatusHeader("SIZE(MB)", i18n.Japanese); got != "SIZE(MB)" {
		t.Fatalf("unknown header=%q", got)
	}
}
