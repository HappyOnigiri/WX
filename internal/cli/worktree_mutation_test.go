package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunAgentWithPolicyFromMutationBoundariesRoutesLookupIntent(t *testing.T) {
	source := t.TempDir()
	record := filepath.Join(t.TempDir(), "launch-record")
	t.Setenv("WX_TEST_LAUNCH_RECORD", record)
	t.Setenv("WX_TEST_EVENT_RECORD", filepath.Join(t.TempDir(), "launch-events"))
	agent := writeLaunchRecorder(t, "claude")
	prependPath(t, filepath.Dir(agent))
	handler := &resumeLaunchHandler{
		resumeErr: errors.New("sql: no rows in result set"),
	}
	client, stop := serveResumeLaunchRPC(t, handler)
	defer stop()

	if got := client.RunAgentWithPolicyFrom(context.Background(), source, "claude", []string{"--resume", "unknown"}, nil, false, WorktreeOptions{}); got != 0 {
		t.Fatalf("exit=%d, want direct unresolved resume", got)
	}
	methods := handler.methodsSnapshot()
	for _, method := range methods {
		if method == "ResolveAndLease" || method == "Resume" {
			t.Fatalf("methods=%v, lookup intent unexpectedly requested a worktree", methods)
		}
	}
	if _, err := os.Stat(record); err != nil {
		t.Fatalf("launch record=%v, want agent launch", err)
	}
}

func TestRunAgentWithPolicyFromMutationBoundariesKeepsOrdinaryLaunchOnPolicyPath(t *testing.T) {
	client, handler, source := resumePolicyLaunchFixture(t)
	if got := client.RunAgentWithPolicyFrom(context.Background(), source, "claude", nil, nil, false, WorktreeOptions{Disable: true}); got != 0 {
		t.Fatalf("exit=%d, want direct ordinary launch", got)
	}
	for _, method := range handler.methodsSnapshot() {
		if method == "WorktreePolicy" {
			t.Fatalf("methods=%v, ordinary launch with explicit disable queried resume policy", handler.methodsSnapshot())
		}
	}
}

// fresh は resume 指定なしでは worktree policy を選ばず、引数エラーとして拒否する。
func TestRunAgentWithPolicyFromMutationBoundariesRejectsFreshWithoutResume(t *testing.T) {
	client, handler, source := resumePolicyLaunchFixture(t)
	var exit int
	stderr := captureStderrForLease(t, func() {
		exit = client.RunAgentWithPolicyFrom(context.Background(), source, "claude", nil, nil, true, WorktreeOptions{Force: true})
	})
	if exit != 2 || !strings.Contains(stderr, "fresh") {
		t.Fatalf("exit=%d stderr=%q, want fresh argument error", exit, stderr)
	}
	if methods := handler.methodsSnapshot(); len(methods) != 0 {
		t.Fatalf("methods=%v, fresh without resume must stop before daemon access", methods)
	}
}

func resumePolicyLaunchFixture(t *testing.T) (Client, *resumeLaunchHandler, string) {
	t.Helper()
	source := t.TempDir()
	t.Setenv("WX_TEST_LAUNCH_RECORD", filepath.Join(t.TempDir(), "launch-record"))
	t.Setenv("WX_TEST_EVENT_RECORD", filepath.Join(t.TempDir(), "launch-events"))
	agent := writeLaunchRecorder(t, "claude")
	prependPath(t, filepath.Dir(agent))
	handler := &resumeLaunchHandler{}
	client, stop := serveResumeLaunchRPC(t, handler)
	t.Cleanup(stop)
	return client, handler, source
}
