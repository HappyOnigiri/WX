package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/HappyOnigiri/WX/internal/i18n"
)

func TestTextRendererLineAndRawTerminateEveryLine(t *testing.T) {
	var output bytes.Buffer
	r := newTextRenderer(&output, i18n.English)
	r.line("status.section.workspaces", nil)
	r.raw("")
	if got := output.String(); got != "Workspaces\n\n" {
		t.Fatalf("lines=%q", got)
	}
}

// TestTextRendererFieldSendsLongAndMultilineValuesToContinuationLines は、値を切らずに
// 全文を残す折り返しを固定する。error・path・opaque ID は途中で失うと診断に使えない。
func TestTextRendererFieldSendsLongAndMultilineValuesToContinuationLines(t *testing.T) {
	for _, testCase := range []struct{ name, value, want string }{
		{"short", "value", "Path: value\n"},
		{"empty", "", "Path: (empty)\n"},
		{"multiline", "first\nsecond", "Path:\n    first\n    second\n"},
		// 120 rune を超える値は折り返して全文を残す。境界の 120 rune は 1 行に収める。
		{"at limit", strings.Repeat("あ", 120), "Path: " + strings.Repeat("あ", 120) + "\n"},
		{"over limit", strings.Repeat("あ", 121), "Path:\n    " + strings.Repeat("あ", 121) + "\n"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			var output bytes.Buffer
			newTextRenderer(&output, i18n.English).field(0, "status.field.path", testCase.value)
			if got := output.String(); got != testCase.want {
				t.Fatalf("field=%q, want %q", got, testCase.want)
			}
		})
	}
	var indented bytes.Buffer
	newTextRenderer(&indented, i18n.English).field(4, "status.field.path", "value")
	if got := indented.String(); got != "    Path: value\n" {
		t.Fatalf("indented field=%q", got)
	}
}

// TestTextRendererDataFieldKeepsTheLabelUntranslated は、payload のキーをラベル位置へ
// 流す経路が訳を通らないことを固定する。訳すと payload のキーが書き換わって読めなくなる。
func TestTextRendererDataFieldKeepsTheLabelUntranslated(t *testing.T) {
	var output bytes.Buffer
	r := newTextRenderer(&output, i18n.Japanese)
	r.dataField(2, "status.field.path", "/tmp/Action")
	r.dataField(2, "worktree_roots[0].error", "permission denied")
	want := "  status.field.path: /tmp/Action\n  worktree_roots[0].error: permission denied\n"
	if got := output.String(); got != want {
		t.Fatalf("data fields=%q, want %q", got, want)
	}
}

// TestTextRendererUnknownIDShowsTheIDItself は、タイポした ID が静かに消えず
// 目に見える形で残ることを固定する。
func TestTextRendererUnknownIDShowsTheIDItself(t *testing.T) {
	var output bytes.Buffer
	r := newTextRenderer(&output, i18n.Japanese)
	// ID は変数で渡す。tools/checkcatalog は literal の ID をカタログと照合するため、
	// 意図的に存在しない ID を literal で書くと静的検査が落ちる。
	unknown := "status.section." + "does_not_exist"
	if got := r.Localize(unknown, nil); got != unknown {
		t.Fatalf("unknown ID=%q", got)
	}
	r.line(unknown, nil)
	if got := output.String(); got != unknown+"\n" {
		t.Fatalf("unknown ID line=%q", got)
	}
}

// TestTextRendererIndentLineKeepsTheIndentOutOfTheCatalog は、字下げが訳文ではなく
// 呼び出し側の引数で決まることを固定する。訳文へ空白を埋めると訳語の変更で桁がずれる。
func TestTextRendererIndentLineKeepsTheIndentOutOfTheCatalog(t *testing.T) {
	var output bytes.Buffer
	newTextRenderer(&output, i18n.English).indentLine(2, "status.section.root", map[string]any{"Index": 3})
	if got := output.String(); got != "  Root 3\n" {
		t.Fatalf("indented line=%q", got)
	}
}
