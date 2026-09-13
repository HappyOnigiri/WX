package main

import (
	"fmt"
	"io"
	"strings"
	"unicode/utf8"

	"github.com/HappyOnigiri/WX/internal/i18n"
)

// textRenderer は人間向け出力を描画時にローカライズする。
// ラベル・見出し・案内文は message ID で解決し、path・ID・状態値・時刻・外部エラーは訳を通さない。
// 固定文を受ける line/field と、値をそのまま受ける raw/dataField を分けて型で区別する。
type textRenderer struct {
	w         io.Writer
	localizer *i18n.Localizer
}

// newTextRenderer は言語を 1 度だけ解決する。
// i18n.T と i18n.New は呼ぶたびに go-i18n の bundle を組み直すため、行ごとに呼ばない。
func newTextRenderer(w io.Writer, lang i18n.Language) *textRenderer {
	return &textRenderer{w: w, localizer: i18n.New(string(lang))}
}

// Localize は message ID を展開する。未知 ID は ID 自体を返すため、
// 静的検査 tools/checkcatalog が ID の実在を照合できる書き方（literal 引数）を守る。
func (r *textRenderer) Localize(id string, data map[string]any) string {
	return r.localizer.Localize(id, data)
}

// LocalizeOr は組み立てた可変 ID を引き、カタログに無い ID は fallback を返す。
func (r *textRenderer) LocalizeOr(id, fallback string) string {
	return r.localizer.LocalizeOr(id, fallback)
}

// line は固定文だけの 1 行を書く。
func (r *textRenderer) line(id string, data map[string]any) {
	r.raw(r.Localize(id, data))
}

// indentLine は字下げ付きの固定文 1 行を書く。
func (r *textRenderer) indentLine(indent int, id string, data map[string]any) {
	r.raw(strings.Repeat(" ", indent) + r.Localize(id, data))
}

// raw は組み立て済みの行や payload 由来のデータ行をそのまま書く。
func (r *textRenderer) raw(text string) {
	_, _ = fmt.Fprintln(r.w, text)
}

// field はラベルを message ID で解決し、値は不透明なまま書く。
func (r *textRenderer) field(indent int, id, value string) {
	r.dataField(indent, r.Localize(id, nil), value)
}

// dataField はラベルも payload 由来のときに使う。
// キーや path をラベル位置へ流す経路をここへ閉じ込め、field と区別できるようにする。
func (r *textRenderer) dataField(indent int, key, value string) {
	if value == "" {
		value = "(empty)"
	}
	prefix := strings.Repeat(" ", indent)
	// 長い error・path・opaque ID は値を失わないよう継続行へ送り、短い値は 1 行に収める。
	if strings.Contains(value, "\n") || utf8.RuneCountInString(value) > 120 {
		r.raw(prefix + key + ":")
		for line := range strings.SplitSeq(value, "\n") {
			r.raw("    " + line)
		}
		return
	}
	r.raw(prefix + key + ": " + value)
}
