package i18n

import (
	"errors"
	"strings"
	"testing"
)

// message ID を持つ error は、表示では言語ごとに解決され、Error() では英語のままである。
// ログ・テスト・外部へ渡る文字列が表示言語で揺れると、機械可読な契約が壊れる。
func TestMessageErrorKeepsEnglishAndLocalizesOnDisplay(t *testing.T) {
	err := NewError("config.language.invalid", nil)
	if got := err.Error(); got != catalog["config.language.invalid"].EN {
		t.Fatalf("Error()=%q, want the English text", got)
	}
	if got := LocalizeError(err, Japanese); got != catalog["config.language.invalid"].JA {
		t.Fatalf("localized=%q, want the Japanese text", got)
	}
}

// 外部 error は message ID を持たないため、表示でも原文のまま出す。
func TestLocalizeErrorLeavesExternalErrorsAlone(t *testing.T) {
	err := errors.New("dial unix /tmp/wx.sock: connection refused")
	if got := LocalizeError(err, Japanese); got != err.Error() {
		t.Fatalf("external error was rewritten: %q", got)
	}
	if got := LocalizeError(nil, Japanese); got != "" {
		t.Fatalf("nil error produced %q", got)
	}
}

// WrapError は下位 error を errors.Is に残したまま、表示だけを message ID で決める。
func TestWrapErrorKeepsTheWrappedError(t *testing.T) {
	cause := errors.New("boom")
	err := WrapError(cause, "cli.daemon.recovery_no_cause", map[string]any{
		"Reason": Message{ID: "cli.daemon.unavailable"}, "Hint": Message{ID: "cli.daemon.hint_doctor"},
	})
	if !errors.Is(err, cause) {
		t.Fatalf("wrapped error lost: %v", err)
	}
	// Data の入れ子 Message は表示言語で解決する。断片を英語のまま連結しない。
	got := LocalizeError(err, Japanese)
	if !strings.Contains(got, catalog["cli.daemon.unavailable"].JA) || !strings.Contains(got, catalog["cli.daemon.hint_doctor"].JA) {
		t.Fatalf("nested messages were not localized: %q", got)
	}
}

// ErrorValue は template のデータとして渡せる形を返す。
func TestErrorValueDistinguishesMessageErrors(t *testing.T) {
	message, ok := ErrorValue(NewError("cli.daemon.unavailable", nil)).(Message)
	if !ok || message.ID != "cli.daemon.unavailable" {
		t.Fatalf("ErrorValue of a message error=%#v", message)
	}
	if got := ErrorValue(errors.New("boom")); got != "boom" {
		t.Fatalf("ErrorValue of an external error=%#v", got)
	}
}

// 未知 ID は欠落が分かるよう ID 自体を返し、空の Message は空文字を返す。
func TestMessageResolutionOfEmptyAndUnknownIDs(t *testing.T) {
	localizer := New(string(English))
	if got := localizer.Message(Message{}); got != "" {
		t.Fatalf("empty message=%q", got)
	}
	if got := localizer.Message(Message{ID: "no.such.id"}); got != "no.such.id" {
		t.Fatalf("unknown message=%q", got)
	}
}

// HasMessage は可変 ID を引く描画側が fallback を選べるようにする。
func TestHasMessage(t *testing.T) {
	if !HasMessage("help.top") || HasMessage("help.command.no-such-command") {
		t.Fatal("HasMessage did not distinguish a known ID from an unknown one")
	}
}
