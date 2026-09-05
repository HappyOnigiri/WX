// Package termtext は元データに残った端末制御列を、表示の直前で無害化する。
// 履歴に貼り付けられた ANSI エスケープ列やキャリッジリターンを除去し、
// 制御列による画面消去や幅計算のずれを防ぐ。
package termtext

import (
	"strings"

	xansi "github.com/charmbracelet/x/ansi"
)

// 元データは変更せず、scanner が保持する履歴を表示側で無害化する。

// Keep* は Sanitize で残す制御文字の集合。用途ごとに使い分ける。
const (
	// KeepLine はタイトルやパスなど 1 行に収める値向け。
	KeepLine = "\t"
)

// StripANSI は ANSI エスケープシーケンスをシーケンス単位で除去する。
// ESC だけを落とすと後続の `[2K` が画面にそのまま出るため、必ず列全体で落とす。
func StripANSI(s string) string {
	if !strings.ContainsRune(s, '\x1b') {
		return s
	}
	return xansi.Strip(s)
}

// Sanitize はエスケープシーケンスと、keep に挙げていない制御文字を除去する。
func Sanitize(s, keep string) string {
	s = StripANSI(s)
	return strings.Map(func(r rune) rune {
		if r >= 0x20 && r != 0x7f {
			return r
		}
		if strings.ContainsRune(keep, r) {
			return r
		}
		return -1
	}, s)
}
