package update

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestLatestReadsThePublishedTagAndPage は、公開済みリリースの応答から案内に必要な 2 つの値を取り出すことを守る。
func TestLatestReadsThePublishedTagAndPage(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Accept"); got != "application/vnd.github+json" {
			t.Errorf("Accept=%q", got)
		}
		_, _ = w.Write([]byte(`{"tag_name":"v1.4.0","html_url":"https://example.test/releases/tag/v1.4.0"}`))
	}))
	defer server.Close()
	release, err := (Checker{Endpoint: server.URL}).Latest(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if release.Tag != "v1.4.0" || release.URL != "https://example.test/releases/tag/v1.4.0" {
		t.Fatalf("release=%+v", release)
	}
}

// TestLatestFallsBackToTheReleasesPageWhenTheReplyHasNoURL は、案内の行先が空にならないことを守る。
func TestLatestFallsBackToTheReleasesPageWhenTheReplyHasNoURL(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"tag_name":"v2.0.0"}`))
	}))
	defer server.Close()
	release, err := (Checker{Endpoint: server.URL}).Latest(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if release.URL != ReleasesPage {
		t.Fatalf("release URL=%q, want the releases page", release.URL)
	}
}

// TestLatestClassifiesTheFailuresItMustTellApart は、レート制限を他の失敗と区別することを守る。
// 区別しないと、制限に触れた確認とネットワーク障害へ同じ扱いしかできない。
func TestLatestClassifiesTheFailuresItMustTellApart(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name    string
		status  int
		body    string
		want    error
		message string
	}{
		{name: "rate limited", status: http.StatusForbidden, body: `{}`, want: ErrRateLimited},
		{name: "too many requests", status: http.StatusTooManyRequests, body: `{}`, want: ErrRateLimited},
		{name: "no release yet", status: http.StatusNotFound, body: `{}`, want: ErrUnavailable},
		{name: "tag is not a release tag", status: http.StatusOK, body: `{"tag_name":"nightly"}`, want: ErrUnavailable},
		{name: "server failure", status: http.StatusBadGateway, body: `{}`, message: "github responded"},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(test.status)
				_, _ = w.Write([]byte(test.body))
			}))
			defer server.Close()
			_, err := (Checker{Endpoint: server.URL}).Latest(context.Background())
			switch {
			case test.want != nil && !errors.Is(err, test.want):
				t.Fatalf("error=%v, want %v", err, test.want)
			case test.want == nil && (err == nil || !strings.Contains(err.Error(), test.message)):
				t.Fatalf("error=%v, want one mentioning %q", err, test.message)
			}
		})
	}
}

// TestNewerComparesOnlyReleaseTags は版比較の境界を守る。
// 開発ビルドの表示や git describe 由来の abbrev では案内を出さない。
func TestNewerComparesOnlyReleaseTags(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		current, candidate string
		want               bool
	}{
		{"v1.2.3", "v1.2.4", true},
		{"v1.2.3", "v1.3.0", true},
		{"v1.2.3", "v2.0.0", true},
		{"v1.2.3", "v1.2.3", false},
		{"v1.2.3", "v1.2.2", false},
		{"v2.0.0", "v1.9.9", false},
		{"v0.4.0-dev", "v0.5.0", false},
		{"v0.4.0", "v0.5.0-rc.1", false},
		{"abc1234", "v0.5.0", false},
		{"v1.2", "v1.3.0", false},
		{"v1.02.3", "v1.3.0", false},
	} {
		if got := Newer(test.current, test.candidate); got != test.want {
			t.Errorf("Newer(%q, %q)=%v, want %v", test.current, test.candidate, got, test.want)
		}
	}
}

// TestReleaseBuildIsFalseUnderTest は、開発ビルドで機能が無効になることがテストの保護でもあることを固定する。
// BuildMeta の既定値が dev であるため、テストバイナリは必ずこちら側へ落ち、実ネットワークへ出る経路を持たない。
func TestReleaseBuildIsFalseUnderTest(t *testing.T) {
	t.Parallel()
	if ReleaseBuild() {
		t.Fatal("the test binary is treated as a release build; the automatic check would reach GitHub")
	}
	if Newer(CurrentVersion(), "v999.0.0") {
		t.Fatalf("a development version %q was compared against a release tag", CurrentVersion())
	}
}

// TestInstallScriptURLPointsAtTheAssetOfThatTag は、更新の実行が要求したタグの資産だけを取ることを守る。
func TestInstallScriptURLPointsAtTheAssetOfThatTag(t *testing.T) {
	t.Parallel()
	got := InstallScriptURL("v1.4.0")
	if !strings.HasSuffix(got, "/releases/download/v1.4.0/install.sh") || !strings.Contains(got, ownerRepository) {
		t.Fatalf("install script URL=%q", got)
	}
}
