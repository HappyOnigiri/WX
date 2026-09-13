package cli

import (
	"strings"
	"testing"

	"github.com/HappyOnigiri/WX/internal/daemon"
	"github.com/HappyOnigiri/WX/internal/i18n"
	"github.com/HappyOnigiri/WX/internal/update"
)

// TestUpdateNoticeIsWrittenOnlyForAClaimedNewerRelease は、案内を出す条件を守る。
// 案内権を持たない応答や、呼び出し側より新しくない版で出すと、起動のたびに同じ行が並ぶ。
func TestUpdateNoticeIsWrittenOnlyForAClaimedNewerRelease(t *testing.T) {
	t.Parallel()
	localizer := i18n.New(string(i18n.English))
	for _, test := range []struct {
		name    string
		current string
		status  daemon.UpdateStatus
		want    bool
	}{
		{name: "claimed newer release", current: "v1.0.0", status: daemon.UpdateStatus{Announce: true, LatestVersion: "v1.1.0", ReleaseURL: "https://example.test/v1.1.0"}, want: true},
		{name: "announcement already taken", current: "v1.0.0", status: daemon.UpdateStatus{LatestVersion: "v1.1.0"}},
		{name: "caller is already current", current: "v1.1.0", status: daemon.UpdateStatus{Announce: true, LatestVersion: "v1.1.0"}},
		{name: "caller is newer than the daemon saw", current: "v1.2.0", status: daemon.UpdateStatus{Announce: true, LatestVersion: "v1.1.0"}},
		{name: "development build", current: "v1.0.0-dev", status: daemon.UpdateStatus{Announce: true, LatestVersion: "v1.1.0"}},
		{name: "nothing recorded yet", current: "v1.0.0", status: daemon.UpdateStatus{Announce: true}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			lines := updateNoticeLines(localizer, test.current, test.status)
			if !test.want {
				if lines != nil {
					t.Fatalf("notice=%v, want nothing", lines)
				}
				return
			}
			if len(lines) != 2 {
				t.Fatalf("notice=%v, want one line and its URL", lines)
			}
			if !strings.Contains(lines[0], "v1.1.0") || !strings.Contains(lines[0], test.current) {
				t.Fatalf("notice line=%q, want both versions", lines[0])
			}
			if lines[1] != test.status.ReleaseURL {
				t.Fatalf("notice URL=%q, want %q", lines[1], test.status.ReleaseURL)
			}
		})
	}
}

// TestUpdateNoticeFallsBackToTheReleasesPage は、daemon が URL を持たない記録でも行先が空にならないことを守る。
func TestUpdateNoticeFallsBackToTheReleasesPage(t *testing.T) {
	t.Parallel()
	lines := updateNoticeLines(i18n.New(string(i18n.English)), "v1.0.0", daemon.UpdateStatus{Announce: true, LatestVersion: "v1.1.0"})
	if len(lines) != 2 || lines[1] != update.ReleasesPage {
		t.Fatalf("notice=%v, want the releases page as the destination", lines)
	}
}

// TestUpdateNoticeIsSilentForADevelopmentBuild は、テストバイナリが案内経路で RPC を出さないことを守る。
// 開発ビルドの判定はテストの保護でもあり、ここが通ると起動のたびに daemon へ問い合わせる。
func TestUpdateNoticeIsSilentForADevelopmentBuild(t *testing.T) {
	t.Parallel()
	if update.ReleaseBuild() {
		t.Fatal("the test binary is treated as a release build")
	}
	called := false
	previous := noticeIsTerminal
	noticeIsTerminal = func(int) bool { called = true; return true }
	t.Cleanup(func() { noticeIsTerminal = previous })
	Client{}.announceUpdate(t.Context())
	if called {
		t.Fatal("a development build reached the terminal check instead of stopping first")
	}
}
