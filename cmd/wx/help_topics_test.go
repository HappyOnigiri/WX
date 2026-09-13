package main

import (
	"bytes"
	"strings"
	"testing"

	xansi "github.com/charmbracelet/x/ansi"

	"github.com/HappyOnigiri/WX/internal/i18n"
)

// help のすべての ID はカタログに実在し、両言語で本文を持つ。
// 未知 ID は Localize が ID 自体を返すため、表示に ID が漏れていないかで確かめる。
func TestHelpTopicsResolveEveryMessageID(t *testing.T) {
	for _, lang := range []i18n.Language{i18n.English, i18n.Japanese} {
		loc := i18n.New(string(lang))
		for name, topic := range helpTopics {
			var out bytes.Buffer
			writeHelp(&out, topic, loc)
			if strings.Contains(out.String(), "help."+strings.ReplaceAll(name, "-", "_")+".") {
				t.Fatalf("%s help in %s leaked a message id:\n%s", name, lang, out.String())
			}
		}
	}
}

// name 列は機械識別子なので、日本語表示でもコマンド名・オプション名・引数の形は原文のまま残る。
func TestHelpJapaneseKeepsCommandSyntaxVerbatim(t *testing.T) {
	var out bytes.Buffer
	writeHelp(&out, helpTopics["top"], i18n.New(string(i18n.Japanese)))
	text := out.String()
	for _, want := range []string{"使い方:", "wx <command> [options]", "--branch <branch|repo=branch>", "retry-standby [--all] [<path>]"} {
		if !strings.Contains(text, want) {
			t.Fatalf("Japanese help lacks %q:\n%s", want, text)
		}
	}
}

// 桁は表示幅で測る。訳語を入れても name 列と説明列の境目は 1 つに揃う。
func TestHelpRowsAlignByDisplayWidth(t *testing.T) {
	japanese := i18n.New(string(i18n.Japanese))
	block := helpBlock{gap: 2, rows: []helpRow{
		{name: "--json", id: "help.top.json"},
		{name: "--branch <branch|repo=branch>", id: "help.top.branch"},
	}}
	var out bytes.Buffer
	writeHelpRows(&out, block, japanese)
	lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	if len(lines) != len(block.rows) {
		t.Fatalf("rendered %d line(s) for %d row(s)", len(lines), len(block.rows))
	}
	column := -1
	for index, line := range lines {
		description := japanese.Localize(block.rows[index].id, nil)
		if !strings.HasSuffix(line, description) {
			t.Fatalf("row %q does not end with its description %q", line, description)
		}
		got := xansi.StringWidth(line) - xansi.StringWidth(description)
		if column == -1 {
			column = got
			continue
		}
		if got != column {
			t.Fatalf("row %q starts its description at %d, want %d", line, got, column)
		}
	}
}
