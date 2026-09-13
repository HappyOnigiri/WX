package main

import (
	"context"
	"strings"
	"testing"

	"github.com/HappyOnigiri/WX/internal/dashboard"
)

func TestDashboardActionRejectsMissingTarget(t *testing.T) {
	stderr := captureStderr(t, func() {
		if code := runDashboardAction(context.Background(), dashboard.Action{Args: []string{"new"}, WorkDir: "/definitely/missing/wx-target"}); code != 1 {
			t.Fatalf("exit=%d", code)
		}
	})
	if !strings.Contains(stderr, "not an accessible directory") {
		t.Fatalf("stderr=%q", stderr)
	}
}

func TestDashboardActionReusesCommandDispatch(t *testing.T) {
	stdout := captureStdout(t, func() {
		if code := runDashboardAction(context.Background(), dashboard.Action{Args: []string{"config", "--describe", "readiness.mode"}}); code != 0 {
			t.Fatalf("exit=%d", code)
		}
	})
	// 表示名は表示言語で変わるため、照合は設定キーと選択肢で行う。
	if !strings.Contains(stdout, "readiness.mode") || !strings.Contains(stdout, "early, full") {
		t.Fatalf("stdout=%q", stdout)
	}
}

// TestUpdateReplacedBinaryTracksTheExecutable は、置き換えが起きた実行だけを置き換えと数えることを守る。
// 失敗した実行や、すでに最新で何もしなかった実行まで置き換えと数えると、
// 状態画面が失敗の直後に再起動の案内を出し、更新済みだと誤解させる。
func TestUpdateReplacedBinaryTracksTheExecutable(t *testing.T) {
	before, ok := executableFingerprint()
	if !ok {
		t.Skip("the test binary cannot be stat'ed")
	}
	if updateReplacedBinary(before, true, 1) {
		t.Fatal("a failed update was counted as a replacement")
	}
	if updateReplacedBinary(before, true, 0) {
		t.Fatal("an unchanged executable was counted as a replacement")
	}
	if !updateReplacedBinary("stale-fingerprint", true, 0) {
		t.Fatal("a changed executable was not counted as a replacement")
	}
	// 実行ファイルを辿れない場合は比較材料がないので、終了コードだけで判断する。
	if !updateReplacedBinary("", false, 0) {
		t.Fatal("a successful update without a fingerprint was not counted as a replacement")
	}
}
