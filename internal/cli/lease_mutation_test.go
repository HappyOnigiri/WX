package cli

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestLeasePlanMutationBoundariesKeepResumeState(t *testing.T) {
	for _, tt := range []struct {
		name     string
		resume   string
		resuming bool
		target   resumeTarget
	}{
		{name: "new lease"},
		{name: "explicit resume", resume: "wx-session", resuming: true, target: resumeTarget{Agent: leaseAgentKindCommand, WXSessionID: "wx-session"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			client := Client{}
			got := client.leasePlan("command", leaseAgentKindCommand, "/bin/true", []string{"arg"}, []string{"main"}, tt.resume, "/source")
			if got.resuming != tt.resuming || !reflect.DeepEqual(got.target, tt.target) {
				t.Fatalf("leasePlan=%+v, want resuming=%t target=%+v", got, tt.resuming, tt.target)
			}
		})
	}
}

func TestLeaseCommandMutationBoundariesPreserveExplicitAndDefaultCWD(t *testing.T) {
	client, handler, base, ctx := leaseFixture(t)
	if got := client.RunLeaseCommandFrom(ctx, "", []string{"true"}, nil, ""); got != 0 {
		t.Fatalf("default-cwd command exit=%d", got)
	}
	if got := client.RunLeaseCommandFrom(ctx, base, []string{"true"}, nil, ""); got != 0 {
		t.Fatalf("explicit-cwd command exit=%d", got)
	}
	requests := leaseRequests(t, handler)
	if len(requests) != 2 || requests[0].CWD == "" || requests[1].CWD != base {
		t.Fatalf("lease requests=%+v, want non-empty process cwd then explicit %q", requests, base)
	}
}

// 明示した session の再開は、呼び出し元 workspace の worktree policy を検査しない。
func TestRunLeaseFromMutationBoundariesSkipsPolicyForResume(t *testing.T) {
	client, handler, _, ctx := leaseFixture(t)
	client.Config.WorkspaceDefaults.Worktree = "off"
	source := probeWorktreeFixture(t)
	if got := client.RunLeaseCommandFrom(ctx, source, []string{"true"}, nil, "wx-session"); got != 0 {
		t.Fatalf("resume command exit=%d, want 0", got)
	}
	handler.mu.Lock()
	methods := append([]string(nil), handler.methods...)
	handler.mu.Unlock()
	if !containsMethod(methods, "Resume") {
		t.Fatalf("methods=%v, want Resume for an explicit session", methods)
	}
	if containsMethod(methods, "ResolveAndLease") {
		t.Fatalf("methods=%v, explicit resume unexpectedly used a new lease", methods)
	}
}

func TestLeaseNewMutationBoundariesKeepCWDAndReadinessNotice(t *testing.T) {
	client, handler, base, ctx := leaseFixture(t)
	stderr := captureStderrForLease(t, func() {
		if got := client.RunLeaseNewFrom(ctx, "", nil, true); got != 0 {
			t.Fatalf("new --json exit=%d", got)
		}
	})
	if strings.Contains(stderr, "wx setup") {
		t.Fatalf("stderr=%q, path lease must not claim missing agent hooks", stderr)
	}
	requests := leaseRequests(t, handler)
	if len(requests) != 1 || requests[0].CWD == "" {
		t.Fatalf("lease request=%+v, want the process cwd %q", requests, base)
	}
}

func TestLeaseNewMutationBoundariesDoNotRenderSetupForAnEmptyCheck(t *testing.T) {
	client, handler, _, ctx := leaseFixture(t)
	// 準備検査の対象がない失敗では、setup report を追加すると診断が二重になる。
	handler.lease.Ready = false
	handler.waitReadyErr = errors.New("injected readiness failure")
	stderr := captureStderrForLease(t, func() {
		if got := client.RunLeaseNew(ctx, nil, true); got != 1 {
			t.Fatalf("new failure exit=%d", got)
		}
	})
	if !strings.Contains(stderr, "injected readiness failure") {
		t.Fatalf("stderr=%q, want the readiness failure", stderr)
	}
	if strings.Contains(stderr, "a workspace could not be prepared for the probe") {
		t.Fatalf("stderr=%q, empty setup check unexpectedly rendered a probe report", stderr)
	}
}

func TestRunLeaseReleaseMutationBoundariesFillsMissingSessionID(t *testing.T) {
	client, handler, _, ctx := leaseFixture(t)
	handler.releaseLeaseReply = map[string]any{"released": true, "discarded": false}
	stdout := captureLeaseStdout(t, func() {
		if got := client.RunLeaseRelease(ctx, "session", false, false, true); got != 0 {
			t.Fatalf("release --json exit=%d", got)
		}
	})
	var reply releaseReply
	if err := json.Unmarshal([]byte(stdout), &reply); err != nil {
		t.Fatalf("stdout=%q err=%v", stdout, err)
	}
	if reply.SessionID != "session" {
		t.Fatalf("reply=%+v, want the requested session ID", reply)
	}
}

func TestRunLeaseReleaseMutationBoundariesPrintsCompletionAfterWaiting(t *testing.T) {
	client, handler, _, ctx := leaseFixture(t)
	handler.releaseLeaseReply = map[string]any{"released": true, "discarded": false, "job_id": "job-1", "job_kind": "SNAPSHOT"}
	stdout := captureLeaseStdout(t, func() {
		if got := client.RunLeaseRelease(ctx, "session", false, true, false); got != 0 {
			t.Fatalf("release --wait exit=%d", got)
		}
	})
	if !strings.Contains(stdout, "completed") {
		t.Fatalf("stdout=%q, want completed release report", stdout)
	}
}

func TestMergeReleaseStatusMutationBoundariesKeepNonEmptyFields(t *testing.T) {
	base := releaseReply{SessionID: "old-session", JobID: "old-job", JobKind: "OLD", State: "PENDING", SlotState: "DRAINING", SessionState: "RELEASING", FailureCode: "old-code", FailureMessage: "old-message", DetailPath: "/old"}
	status := releaseReply{SessionID: "new-session", JobID: "new-job", JobKind: "SNAPSHOT", State: "SUCCEEDED", SlotState: "SNAPSHOTTED", SessionState: "ARCHIVED", FailureCode: "new-code", FailureMessage: "new-message", DetailPath: "/new"}
	mergeReleaseStatus(&base, status)
	if !reflect.DeepEqual(base, status) {
		t.Fatalf("merged=%+v, want all non-empty fields from status %+v", base, status)
	}

	empty := base
	mergeReleaseStatus(&empty, releaseReply{})
	if !reflect.DeepEqual(empty, base) {
		t.Fatalf("empty status changed reply from %+v to %+v", base, empty)
	}
	stateOnly := base
	mergeReleaseStatus(&stateOnly, releaseReply{State: "SUCCEEDED"})
	if stateOnly.State != "SUCCEEDED" {
		t.Fatalf("state-only status=%+v, want the terminal state", stateOnly)
	}
}

func TestRunLeaseReleaseMutationBoundariesPollOnlyWhenAJobExists(t *testing.T) {
	client, handler, _, ctx := leaseFixture(t)
	handler.releaseLeaseReply = map[string]any{"released": true, "discarded": false}
	if got := client.RunLeaseRelease(ctx, "session", false, true, false); got != 0 {
		t.Fatalf("release --wait without job exit=%d", got)
	}
	handler.mu.Lock()
	calls := handler.releaseStatusCalls
	handler.mu.Unlock()
	if calls != 0 {
		t.Fatalf("ReleaseStatus calls=%d, want zero without a job", calls)
	}

	handler.releaseLeaseReply = map[string]any{"released": true, "discarded": false, "job_id": "job-1", "job_kind": "SNAPSHOT"}
	if got := client.RunLeaseRelease(ctx, "session", false, true, false); got != 0 {
		t.Fatalf("release --wait with a job exit=%d", got)
	}
	handler.mu.Lock()
	calls = handler.releaseStatusCalls
	handler.mu.Unlock()
	if calls == 0 {
		t.Fatal("ReleaseStatus was not called for a job")
	}
}
