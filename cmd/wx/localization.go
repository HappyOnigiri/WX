package main

import (
	"context"
	"strings"

	"github.com/HappyOnigiri/WX/internal/config"
	"github.com/HappyOnigiri/WX/internal/i18n"
)

// localization.go は cmd/wx の翻訳層が共有する土台を持つ。
// 個々の翻訳は localization_help.go・localization_status.go・
// localization_message.go に層ごとに分けて置く。

// commandContext は直接サブコマンド関数を呼ぶテストと、run からの通常起動の
// どちらにも設定言語を明示する。壊れた設定は config.LoadLanguage の英語 fallback を使う。
func commandContext(ctx context.Context) context.Context {
	if i18n.HasLanguage(ctx) {
		return ctx
	}
	return i18n.WithLanguage(ctx, config.LoadLanguage())
}

// translation は英語の原文と日本語訳の対で、置換表の要素として使う。
type translation struct{ en, ja string }

// applyTranslations は表の宣言順に strings.ReplaceAll を適用する。
// 長い原文を先に当てて短い語句で補うなど、適用順が訳出の契約になる表があるため、
// 呼び出し側は表の並び順を保ったまま渡す。
func applyTranslations(text string, translations []translation) string {
	for _, replacement := range translations {
		text = strings.ReplaceAll(text, replacement.en, replacement.ja)
	}
	return text
}
