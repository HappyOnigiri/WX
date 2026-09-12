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
	if !strings.Contains(stdout, "準備完了モード") {
		t.Fatalf("stdout=%q", stdout)
	}
}
