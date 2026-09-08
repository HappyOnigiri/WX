package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/HappyOnigiri/WX/internal/config"
	"github.com/HappyOnigiri/WX/internal/daemon"
)

func TestLaunchReadinessModeAndFailureGate(t *testing.T) {
	for _, tc := range []struct {
		name, mode, method string
		hooks, ready, fail bool
	}{
		{name: "early-hooks", mode: "early", method: "WaitEarlyReady", hooks: true},
		{name: "full-hooks", mode: "full", method: "WaitReady", hooks: true},
		{name: "early-no-hooks", mode: "early", method: "WaitReady"},
		{name: "full-no-hooks", mode: "full", method: "WaitReady"},
		{name: "warm", mode: "early", hooks: true, ready: true},
		{name: "early-failure", mode: "early", method: "WaitEarlyReady", hooks: true, fail: true},
		{name: "full-failure", mode: "full", method: "WaitReady", hooks: true, fail: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			if tc.hooks {
				installReadinessHooks(t, home, "claude")
			}
			root := t.TempDir()
			record, eventLog := filepath.Join(root, "record"), filepath.Join(root, "events")
			t.Setenv("WX_TEST_LAUNCH_RECORD", record)
			t.Setenv("WX_TEST_EVENT_RECORD", eventLog)
			agent := writeLaunchRecorder(t, "claude")
			prependPath(t, filepath.Dir(agent))
			handler := &resumeLaunchHandler{lease: daemon.Lease{SessionID: "test", Token: "token", Path: root, Ready: tc.ready}, eventLog: eventLog}
			if tc.fail {
				handler.leaseErrors = map[string][]error{tc.method: {errors.New("preparation failed")}}
			}
			cfg := config.Defaults()
			cfg.Readiness.Mode = tc.mode
			client, stop := serveResumeLaunchRPCWithConfig(t, handler, cfg)
			defer stop()
			exit, retry := client.launch(context.Background(), launchPlan{agent: "claude", hooksReady: tc.hooks, cwd: root})
			if retry || (exit == 0) == tc.fail {
				t.Fatalf("exit=%d retry=%t", exit, retry)
			}
			methods := handler.methodsSnapshot()
			if tc.method != "" && !containsMethod(methods, tc.method) {
				t.Fatalf("missing %s: %v", tc.method, methods)
			}
			switch {
			case tc.fail:
				if _, err := os.Stat(record); !os.IsNotExist(err) {
					t.Fatalf("agent launched after failure: %v", err)
				}
				if !containsMethod(methods, "Release") {
					t.Fatalf("failed wait leaked lease: %v", methods)
				}
			case tc.method != "":
				if events := readEventLog(t, eventLog); !eventsBefore(events, "rpc:"+tc.method, "agent") {
					t.Fatalf("launch preceded readiness: %v", events)
				}
			case containsMethod(methods, "WaitReady") || containsMethod(methods, "WaitEarlyReady"):
				t.Fatalf("warm slot waited: %v", methods)
			}
		})
	}
}
