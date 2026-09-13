package main

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/HappyOnigiri/WX/internal/update"
)

// updateTestAdapters は実網と実 process へ出さずに update の経路を通すための土台である。
// 既定は配布用ビルドで最新が1つ新しい状態にし、各 test は必要な項目だけを上書きする。
func updateTestAdapters(t *testing.T) (*updateAdapters, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	adapters := updateAdapters{
		releaseBuild: func() bool { return true },
		current:      func() string { return "v1.0.0" },
		latest: func(context.Context) (update.Release, error) {
			return update.Release{Tag: "v1.1.0", URL: "https://example.test/v1.1.0"}, nil
		},
		supported:   func() bool { return true },
		fetchScript: func(context.Context, string) ([]byte, error) { return []byte("#!/usr/bin/env bash\n"), nil },
		runScript:   func(context.Context, string) error { return nil },
		stdout:      &stdout,
		stderr:      &stderr,
	}
	previous := updateCommand
	t.Cleanup(func() { updateCommand = previous })
	updateCommand = adapters
	return &updateCommand, &stdout, &stderr
}

// TestUpdateReportsWithoutInstalling は、--apply のない実行が案内だけで終えることを守る。
// 確認のつもりの実行が置き換えまで進むと、利用者の知らないうちにバイナリが変わる。
func TestUpdateReportsWithoutInstalling(t *testing.T) {
	adapters, stdout, _ := updateTestAdapters(t)
	installed := false
	adapters.runScript = func(context.Context, string) error {
		installed = true
		return nil
	}
	if code := runUpdate(context.Background(), nil); code != 0 {
		t.Fatalf("exit=%d", code)
	}
	if installed {
		t.Fatal("a check without --apply ran install.sh")
	}
	if !strings.Contains(stdout.String(), "v1.1.0") || !strings.Contains(stdout.String(), "https://example.test/v1.1.0") {
		t.Fatalf("stdout=%q, want the new release and its URL", stdout.String())
	}
}

// TestUpdateKeepsQuietOnTheLatestRelease は、最新版では更新を促さないことを守る。
func TestUpdateKeepsQuietOnTheLatestRelease(t *testing.T) {
	adapters, stdout, _ := updateTestAdapters(t)
	adapters.current = func() string { return "v1.1.0" }
	if code := runUpdate(context.Background(), []string{"--apply"}); code != 0 {
		t.Fatalf("exit=%d", code)
	}
	if strings.Contains(stdout.String(), "--apply") {
		t.Fatalf("stdout=%q, want no upgrade hint on the latest release", stdout.String())
	}
}

// TestUpdateStopsWhenTheReleaseCannotBeRead は、確認に失敗した実行が置き換えへ進まないことを守る。
func TestUpdateStopsWhenTheReleaseCannotBeRead(t *testing.T) {
	adapters, _, stderr := updateTestAdapters(t)
	adapters.latest = func(context.Context) (update.Release, error) {
		return update.Release{}, errors.New("dial tcp: no route to host")
	}
	installed := false
	adapters.runScript = func(context.Context, string) error {
		installed = true
		return nil
	}
	if code := runUpdate(context.Background(), []string{"--apply"}); code != 1 {
		t.Fatalf("exit=%d", code)
	}
	if installed {
		t.Fatal("install.sh ran although the release could not be read")
	}
	if !strings.Contains(stderr.String(), "no route to host") {
		t.Fatalf("stderr=%q, want the failure reported", stderr.String())
	}
}

// TestUpdateApplyRunsTheDownloadedScript は、--apply が取得した install.sh を実行することを守る。
func TestUpdateApplyRunsTheDownloadedScript(t *testing.T) {
	adapters, stdout, _ := updateTestAdapters(t)
	adapters.fetchScript = func(_ context.Context, tag string) ([]byte, error) {
		if tag != "v1.1.0" {
			t.Fatalf("tag=%q, want the latest release", tag)
		}
		return []byte("#!/usr/bin/env bash\necho installed\n"), nil
	}
	executed := ""
	adapters.runScript = func(_ context.Context, path string) error {
		content, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		executed = string(content)
		return nil
	}
	if code := runUpdate(context.Background(), []string{"--apply"}); code != 0 {
		t.Fatalf("exit=%d", code)
	}
	if !strings.Contains(executed, "echo installed") {
		t.Fatalf("executed=%q, want the downloaded script", executed)
	}
	if !strings.Contains(stdout.String(), "v1.1.0") {
		t.Fatalf("stdout=%q, want the installed release named", stdout.String())
	}
}

// TestUpdateApplyReportsFailures は、適用の各段の失敗が終了コード1として出ることを守る。
// 0 で終えると、状態画面が置き換えていないのに再起動の案内を出す。
func TestUpdateApplyReportsFailures(t *testing.T) {
	cases := []struct {
		name    string
		arrange func(*updateAdapters)
		want    string
	}{
		{
			name:    "unsupported platform",
			arrange: func(a *updateAdapters) { a.supported = func() bool { return false } },
			want:    "macOS arm64",
		},
		{
			name: "download failure",
			arrange: func(a *updateAdapters) {
				a.fetchScript = func(context.Context, string) ([]byte, error) {
					return nil, errors.New("install.sh download failed: 404 Not Found")
				}
			},
			want: "404 Not Found",
		},
		{
			name: "install failure",
			arrange: func(a *updateAdapters) {
				a.runScript = func(context.Context, string) error { return errors.New("exit status 1") }
			},
			want: "exit status 1",
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			adapters, _, stderr := updateTestAdapters(t)
			test.arrange(adapters)
			if code := runUpdate(context.Background(), []string{"--apply"}); code != 1 {
				t.Fatalf("exit=%d", code)
			}
			if !strings.Contains(stderr.String(), test.want) {
				t.Fatalf("stderr=%q, want %q", stderr.String(), test.want)
			}
		})
	}
}

// TestUpdateSkipsDevelopmentBuilds は、開発ビルドが GitHub を見に行かないことを守る。
// 埋め込み版はリリースタグと比較できず、install.sh の置き換え先とも一致しない。
func TestUpdateSkipsDevelopmentBuilds(t *testing.T) {
	adapters, stdout, _ := updateTestAdapters(t)
	adapters.releaseBuild = func() bool { return false }
	asked := false
	adapters.latest = func(context.Context) (update.Release, error) {
		asked = true
		return update.Release{}, nil
	}
	if code := runUpdate(context.Background(), nil); code != 0 {
		t.Fatalf("exit=%d", code)
	}
	if asked {
		t.Fatal("a development build asked GitHub for the latest release")
	}
	if !strings.Contains(stdout.String(), "v1.0.0") {
		t.Fatalf("stdout=%q, want the development build reported", stdout.String())
	}
}

// TestFetchInstallScriptRejectsAnOversizedBody は、上限を超えた応答を error にすることを守る。
// 切り詰めた script を返すと、失敗が内容の破損ではなく bash の構文 error にしか見えない。
func TestFetchInstallScriptRejectsAnOversizedBody(t *testing.T) {
	oversized := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(bytes.Repeat([]byte("#"), installScriptLimit+1))
	}))
	t.Cleanup(oversized.Close)
	if _, err := fetchScriptFrom(context.Background(), oversized.URL); err == nil {
		t.Fatal("an oversized install.sh was accepted")
	}
	small := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("#!/usr/bin/env bash\n"))
	}))
	t.Cleanup(small.Close)
	script, err := fetchScriptFrom(context.Background(), small.URL)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(script), "#!") {
		t.Fatalf("script=%q, want the body returned as is", script)
	}
	missing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(missing.Close)
	if _, err := fetchScriptFrom(context.Background(), missing.URL); err == nil {
		t.Fatal("a missing install.sh was accepted")
	}
}
