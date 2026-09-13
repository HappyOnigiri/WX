package main

import (
	"fmt"
	"io"
	"strings"

	xansi "github.com/charmbracelet/x/ansi"

	"github.com/HappyOnigiri/WX/internal/config"
	"github.com/HappyOnigiri/WX/internal/i18n"
)

func topUsage(w io.Writer) {
	topUsageLanguage(w, i18n.English)
}

func topUsageLanguage(w io.Writer, lang i18n.Language) {
	writeHelp(w, helpTopics["top"], i18n.New(string(lang)))
}

func commandUsage(w io.Writer, name string) {
	commandUsageLanguage(w, name, localizedUsageLanguage())
}

// commandUsageLanguage は 1 コマンドの help を出す。未知の名前は wx 自身の help へ落とす。
func commandUsageLanguage(w io.Writer, name string, lang i18n.Language) {
	topic, known := helpTopics[name]
	if !known {
		topic = helpTopics["top"]
	}
	writeHelp(w, topic, i18n.New(string(lang)))
}

// writeHelp はヘルプ本文を表示言語で組み立てる。
// 桁は表示幅で測るため、訳語を変えても name 列と説明列がずれない。
func writeHelp(w io.Writer, topic helpTopic, loc *i18n.Localizer) {
	label := loc.Localize("help.usage", nil)
	indent := strings.Repeat(" ", xansi.StringWidth(label)+1)
	for index, line := range topic.usage {
		if index == 0 {
			_, _ = fmt.Fprintln(w, label+" "+line)
			continue
		}
		_, _ = fmt.Fprintln(w, indent+line)
	}
	for _, block := range topic.blocks {
		if block.blank {
			_, _ = fmt.Fprintln(w)
		}
		if len(block.rows) == 0 {
			_, _ = fmt.Fprintln(w, loc.Localize(block.id, nil))
			continue
		}
		writeHelpRows(w, block, loc)
	}
}

// writeHelpRows は定義の一覧を出す。説明の折り返しは訳文が持つ改行に従い、
// 続きの行は説明列の位置へ字下げする。
func writeHelpRows(w io.Writer, block helpBlock, loc *i18n.Localizer) {
	width := 0
	for _, row := range block.rows {
		width = max(width, xansi.StringWidth(row.name))
	}
	column := 2 + width + block.gap
	continuation := strings.Repeat(" ", column)
	for _, row := range block.rows {
		head := "  " + row.name + strings.Repeat(" ", column-2-xansi.StringWidth(row.name))
		for index, line := range strings.Split(loc.Localize(row.id, nil), "\n") {
			if index > 0 {
				_, _ = fmt.Fprintln(w, continuation+line)
				continue
			}
			_, _ = fmt.Fprintln(w, head+line)
		}
	}
}

// localizedUsageLanguage is kept separate from commandLanguage so tests can render
// help for a chosen language without mutating process-wide configuration.
func localizedUsageLanguage() i18n.Language {
	return i18n.Normalize(config.LoadLanguage())
}
