package cli

import (
	"errors"
	"strings"
	"testing"

	"github.com/HappyOnigiri/WorktreeX/internal/daemon"
	"github.com/HappyOnigiri/WorktreeX/internal/i18n"
)

func TestCLILanguageDefaultsToEnglish(t *testing.T) {
	var c Client
	if got := cliLanguage(c); got != i18n.English {
		t.Fatalf("default CLI language=%q", got)
	}
}

// message ID を持つ error だけを訳し、外部 error の本文は原文のまま出す。
func TestLeaseErrorTextLocalizesOnlyKnownMessages(t *testing.T) {
	localizer := i18n.New(string(i18n.Japanese))
	got := leaseErrorText(localizer, i18n.NewError("cli.daemon.unavailable", nil))
	if !strings.Contains(got, "wx daemon は利用できません") {
		t.Fatalf("localized message=%q", got)
	}
	if got := leaseErrorText(localizer, errors.New("dial unix /tmp/wx.sock: refused")); got != "dial unix /tmp/wx.sock: refused" {
		t.Fatalf("external error was rewritten: %q", got)
	}
}

// worktree を使わない workspace の拒否は RPC 越しに文字列で届くため、
// 表示時に root を取り出して訳し直す。root そのものは訳さない。
func TestLeaseErrorTextLocalizesWorktreeDisabledFromTheDaemon(t *testing.T) {
	got := leaseErrorText(i18n.New(string(i18n.Japanese)), daemon.WorktreeDisabledError("/repos/app"))
	if !strings.Contains(got, "worktree を使わない設定です") {
		t.Fatalf("localized message=%q", got)
	}
	if !strings.Contains(got, "/repos/app") || !strings.Contains(got, daemon.WorktreeDisabledMarker) {
		t.Fatalf("opaque values were changed: %q", got)
	}
}
