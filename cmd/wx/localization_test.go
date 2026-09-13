package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/HappyOnigiri/WX/internal/i18n"
)

func TestTranslateHelpJapaneseKeepsCommandSyntax(t *testing.T) {
	text := "Usage: wx status [--json]\nshow detailed status instead of the summary\n"
	got := translateHelp(text, i18n.Japanese)
	if !strings.Contains(got, "使い方:") || !strings.Contains(got, "wx status [--json]") {
		t.Fatalf("Japanese help=%q", got)
	}
}

func TestTranslateHelpJapaneseTranslatesLongOptionProse(t *testing.T) {
	text := "Agent directories (agent.add_dir):\n  always     pass every repository directory under the agent's working directory to --add-dir (default)\n  worktree   pass them only when the agent runs in a wx worktree\n  off        never pass them\nA workspace with several repositories puts the agent's working directory at the\nparent of those repositories, so their .claude/skills and other agent assets are\nonly loaded when the directories are passed with --add-dir. A workspace that is a\nsingle repository has nothing to pass. Directories you pass yourself are kept.\n"
	got := translateHelp(text, i18n.Japanese)
	if strings.Contains(got, "pass every repository") || strings.Contains(got, "pass them only") || strings.Contains(got, "never pass them") || strings.Contains(got, "A workspace with several") {
		t.Fatalf("long Japanese help retained English prose=%q", got)
	}
}

func TestTranslateHelpJapaneseTranslatesBenchProse(t *testing.T) {
	text := "Measure how long the current workspace takes to become usable, and where that\ntime goes. Each run leases a workspace the way wx new does, waits for EARLY\nREADY and then for FULL READY, prints the breakdown the daemon recorded for that\npreparation, and returns the lease without saving it.\n"
	got := translateHelp(text, i18n.Japanese)
	if strings.Contains(got, "time goes") || strings.Contains(got, "Each run leases") {
		t.Fatalf("bench help retained English prose=%q", got)
	}
}

func TestCommandConfigHelpJapaneseTranslatesLongParagraphs(t *testing.T) {
	var raw bytes.Buffer
	commandUsageEnglish(&raw, "config")
	got := translateHelp(raw.String(), i18n.Japanese)
	if strings.Contains(got, "A workspace with several repositories") || strings.Contains(got, "pass every repository") {
		t.Fatalf("config help retained English prose=%q", got)
	}
}

func TestTranslateHumanOutputJapaneseKeepsOpaqueValues(t *testing.T) {
	text := "Path: /tmp/Database\nError: permission denied\n"
	got := translateHumanOutput(text, i18n.Japanese)
	if !strings.Contains(got, "/tmp/Database") || !strings.Contains(got, "エラー") {
		t.Fatalf("Japanese output=%q", got)
	}
}

func TestLocalizeErrorTextJapaneseKeepsDynamicDetails(t *testing.T) {
	got := localizeErrorText("language must be en or ja: /tmp/config.yaml", i18n.Japanese)
	if !strings.Contains(got, "language は en または ja で指定してください") || !strings.Contains(got, "/tmp/config.yaml") {
		t.Fatalf("localized error=%q", got)
	}
}
