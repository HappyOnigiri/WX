package main

import (
	"fmt"
	"io"

	"github.com/HappyOnigiri/WX/internal/config"
	"github.com/HappyOnigiri/WX/internal/i18n"
)

// help 本文は internal/i18n の catalog がコマンド単位のブロックとして言語ごとに持つ。
// ここは message ID を選んで書き出すだけにし、桁揃えと改行位置の契約を各言語の中で閉じる。

func topUsage(w io.Writer) {
	topUsageLanguage(w, i18n.English)
}

func topUsageLanguage(w io.Writer, lang i18n.Language) {
	writeUsage(w, lang, topUsageID)
}

func commandUsage(w io.Writer, name string) {
	commandUsageLanguage(w, name, localizedUsageLanguage())
}

// commandUsageLanguage は未知のコマンド名では全体 help へ落とす。
func commandUsageLanguage(w io.Writer, name string, lang i18n.Language) {
	id := commandUsageID + name
	if !i18n.HasMessage(id) {
		id = topUsageID
	}
	writeUsage(w, lang, id)
}

const (
	topUsageID     = "help.top"
	commandUsageID = "help.command."
)

func writeUsage(w io.Writer, lang i18n.Language, id string) {
	_, _ = fmt.Fprintln(w, i18n.New(string(lang)).Localize(id, nil))
}

// localizedUsageLanguage is kept separate from commandLanguage so tests can render
// help for a chosen language without mutating process-wide configuration.
func localizedUsageLanguage() i18n.Language {
	return i18n.Normalize(config.LoadLanguage())
}
