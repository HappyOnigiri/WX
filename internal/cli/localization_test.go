package cli

import (
	"errors"
	"strings"
	"testing"

	"github.com/HappyOnigiri/WX/internal/daemon"
	"github.com/HappyOnigiri/WX/internal/i18n"
)

// wx が作った固定文のエラーだけを訳し、payload と外部エラーは原文のまま残す。
func TestLocalizeCLIErrorTranslatesOnlyFixedMessages(t *testing.T) {
	japanese := i18n.New(string(i18n.Japanese))
	fixed := newLocalizedError("cli.daemon_unavailable_doctor", map[string]any{"Error": "dial unix /tmp/wx.sock: connect: no such file"}, nil)
	got := localizeCLIError(japanese, fixed)
	if !strings.Contains(got, "wx daemon は利用できません") {
		t.Fatalf("localized message=%q", got)
	}
	if !strings.Contains(got, "/tmp/wx.sock") {
		t.Fatalf("opaque detail was changed: %q", got)
	}
	// 英語の Error は機械経路と wrap 済みの文脈のために保つ。
	if english := fixed.Error(); !strings.Contains(english, "wx daemon is unavailable") {
		t.Fatalf("english error=%q", english)
	}
	external := errors.New("git for-each-ref failed with exit -1")
	if got := localizeCLIError(japanese, external); got != external.Error() {
		t.Fatalf("external error was rewritten: %q", got)
	}
}

// worktree を使わない設定のエラーは、訳した後も終了コードの判定を保つ。
func TestLocalizedWorktreeDisabledErrorKeepsExitCode(t *testing.T) {
	err := newLocalizedError("cli.worktree_disabled", map[string]any{"Root": "/repos/app", "Marker": daemon.WorktreeDisabledMarker}, nil)
	if !daemon.IsWorktreeDisabled(err) {
		t.Fatalf("worktree-disabled error was not recognized: %q", err.Error())
	}
	got := localizeCLIError(i18n.New(string(i18n.Japanese)), err)
	if !strings.Contains(got, "/repos/app") || !strings.Contains(got, "worktree を使わない設定です") {
		t.Fatalf("localized message=%q", got)
	}
}

func TestCLILanguageDefaultsToEnglish(t *testing.T) {
	var c Client
	if got := cliLanguage(c); got != i18n.English {
		t.Fatalf("default CLI language=%q", got)
	}
}
