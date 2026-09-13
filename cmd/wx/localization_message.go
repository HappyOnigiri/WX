package main

import (
	"strings"

	"github.com/HappyOnigiri/WX/internal/i18n"
)

// localization_message.go は固定文を描画の直前まで未解決のまま持ち回る道具と、
// 設定検証の固定エラーだけを訳す変換をまとめる。
// path・key・外部コマンドのエラー文字列は原文のまま残す。

// localizedMessage は固定文を message ID と template データの組で保持する。
// 値を組み立てた層が訳語を選ばずに済み、表示側が言語を 1 度だけ解決できる。
type localizedMessage struct {
	id   string
	data map[string]any
}

// text は message ID を解決する。ID が空なら空文字を返し、呼び出し側が行ごと省ける。
func (m localizedMessage) text(loc *i18n.Localizer) string {
	if m.id == "" {
		return ""
	}
	return loc.Localize(m.id, m.data)
}

// empty は解決すべき固定文を持たないことを示す。
func (m localizedMessage) empty() bool { return m.id == "" }

// localizeErrorText は設定検証などの固定エラーだけを翻訳し、path・key・外部エラーの値は残す。
func localizeErrorText(text string, lang i18n.Language) string {
	if lang != i18n.Japanese {
		return text
	}
	return strings.ReplaceAll(text, "language must be en or ja", i18n.New(string(lang)).Localize("config.language.invalid", nil))
}

func localizeError(err error, lang i18n.Language) string {
	if err == nil {
		return ""
	}
	return localizeErrorText(err.Error(), lang)
}
