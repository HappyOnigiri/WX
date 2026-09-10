package main

import (
	"os"
	"path/filepath"
	"testing"
)

// TestCIFlakyIssueProbe はCI上のrunner検証でだけ初回プロセスを失敗させる。
// runとattemptごとの一時ファイルを使い、追加実行では同じ条件でPASSになる。
func TestCIFlakyIssueProbe(t *testing.T) {
	if os.Getenv("GITHUB_ACTIONS") != "true" {
		return
	}
	runID := os.Getenv("GITHUB_RUN_ID")
	attempt := os.Getenv("GITHUB_RUN_ATTEMPT")
	if runID == "" || attempt == "" {
		return
	}
	marker := filepath.Join(os.TempDir(), "wx-ci-flaky-probe-"+runID+"-"+attempt)
	if _, err := os.Stat(marker); err == nil {
		return
	} else if !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if err := os.WriteFile(marker, []byte("seen"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Fatal("intentional first-run failure for flaky reporter verification")
}
