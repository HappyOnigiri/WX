package cli

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/HappyOnigiri/WX/internal/config"
	"github.com/HappyOnigiri/WX/internal/daemon"
)

// restoreFailure は daemon の WaitReady が復元不能なときに返す形の失敗である。
func restoreFailure() error {
	return errors.New("workspace readiness failed: state=QUARANTINED failure_id=RESTORE_FAILED " + daemon.RecoveryUnavailableMarker + " detail_path=unavailable exit_code=unknown timed_out=false canceled=false")
}

// newResumeLaunchFixture は agent の起動を記録する resume 用の client を組み立てる。
func newResumeLaunchFixture(t *testing.T, handler *resumeLaunchHandler, cfg config.Config) (Client, string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	workspace := filepath.Join(t.TempDir(), "slot")
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	handler.lease.Path = workspace
	record := filepath.Join(t.TempDir(), "launch-record")
	t.Setenv("WX_TEST_LAUNCH_RECORD", record)
	t.Setenv("WX_TEST_EVENT_RECORD", filepath.Join(t.TempDir(), "launch-events"))
	agent := writeLaunchRecorder(t, "claude")
	prependPath(t, filepath.Dir(agent))
	client, _ := serveResumeLaunchRPCWithConfig(t, handler, cfg)
	return client, record
}

func resumeFreshFlags(t *testing.T, handler *resumeLaunchHandler) []bool {
	t.Helper()
	var flags []bool
	for _, raw := range handler.historyFor("Resume") {
		var params struct {
			Fresh bool `json:"fresh"`
		}
		if err := json.Unmarshal(raw, &params); err != nil {
			t.Fatal(err)
		}
		flags = append(flags, params.Fresh)
	}
	return flags
}

func TestRunResumeRetriesInFreshWorkspaceWhenRecoveryIsUnavailable(t *testing.T) {
	handler := &resumeLaunchHandler{
		lease:           daemon.Lease{SessionID: "restore-session", Token: "restore-token", Ready: false},
		status:          resumeStatus{WXSessionID: "old-session", Agent: "claude", AgentSessionID: "native-claude"},
		waitReadyErrors: []error{restoreFailure()},
	}
	client, record := newResumeLaunchFixture(t, handler, config.Defaults())

	if exit := client.RunResume(context.Background(), "old-session", "", nil, nil, false); exit != 0 {
		t.Fatalf("RunResume exit=%d, want 0", exit)
	}
	if flags := resumeFreshFlags(t, handler); len(flags) != 2 || flags[0] || !flags[1] {
		t.Fatalf("Resume fresh flags=%v, want [false true]", flags)
	}
	launch := readLaunchRecord(t, record)
	if got := launch["WX_RECOVERY_DISCARDED"]; got != "1" {
		t.Fatalf("WX_RECOVERY_DISCARDED=%q, want 1; record=%v", got, launch)
	}
	if got := launch["args"]; got != "--resume native-claude" {
		t.Fatalf("resume args=%q, want --resume native-claude", got)
	}
}

func TestRunResumeKeepsFailureWhenPreparationIsNotARestoreFailure(t *testing.T) {
	handler := &resumeLaunchHandler{
		lease:           daemon.Lease{SessionID: "prepare-session", Token: "prepare-token", Ready: false},
		status:          resumeStatus{WXSessionID: "old-session", Agent: "claude", AgentSessionID: "native-claude"},
		waitReadyErrors: []error{errors.New("workspace readiness failed: state=FAILED failure_id=PREPARE_FAILED")},
	}
	client, _ := newResumeLaunchFixture(t, handler, config.Defaults())

	if exit := client.RunResume(context.Background(), "old-session", "", nil, nil, false); exit != 1 {
		t.Fatalf("RunResume exit=%d, want 1", exit)
	}
	if flags := resumeFreshFlags(t, handler); len(flags) != 1 {
		t.Fatalf("Resume calls=%v, want a single attempt", flags)
	}
}

func TestRunResumeStopsAfterOneFreshRetry(t *testing.T) {
	handler := &resumeLaunchHandler{
		lease:           daemon.Lease{SessionID: "looping-session", Token: "looping-token", Ready: false},
		status:          resumeStatus{WXSessionID: "old-session", Agent: "claude", AgentSessionID: "native-claude"},
		waitReadyErrors: []error{restoreFailure(), restoreFailure()},
	}
	client, _ := newResumeLaunchFixture(t, handler, config.Defaults())

	if exit := client.RunResume(context.Background(), "old-session", "", nil, nil, false); exit != 1 {
		t.Fatalf("RunResume exit=%d, want 1", exit)
	}
	if flags := resumeFreshFlags(t, handler); len(flags) != 2 || flags[0] || !flags[1] {
		t.Fatalf("Resume fresh flags=%v, want exactly [false true]", flags)
	}
}

func TestRunResumeRetriesWhenTheDaemonRefusesAnExpiredSnapshot(t *testing.T) {
	handler := &resumeLaunchHandler{
		lease:  daemon.Lease{SessionID: "expired-session", Token: "expired-token", Ready: true},
		status: resumeStatus{WXSessionID: "old-session", Agent: "claude", AgentSessionID: "native-claude"},
		leaseErrors: map[string][]error{"Resume": {
			errors.New("session snapshot is EXPIRED; confirmation is required before creating a workspace from the current base " + daemon.RecoveryUnavailableMarker),
		}},
	}
	client, record := newResumeLaunchFixture(t, handler, config.Defaults())

	if exit := client.RunResume(context.Background(), "old-session", "", nil, nil, false); exit != 0 {
		t.Fatalf("RunResume exit=%d, want 0", exit)
	}
	if flags := resumeFreshFlags(t, handler); len(flags) != 2 || flags[0] || !flags[1] {
		t.Fatalf("Resume fresh flags=%v, want [false true]", flags)
	}
	if launch := readLaunchRecord(t, record); launch["WX_RECOVERY_DISCARDED"] != "1" {
		t.Fatalf("WX_RECOVERY_DISCARDED=%q, want 1", launch["WX_RECOVERY_DISCARDED"])
	}
}

func TestConfirmFreshResumeAcceptsWithoutATerminal(t *testing.T) {
	client := Client{Config: config.Defaults()}
	if !client.confirmFreshResume(context.Background(), "session", "no recovery snapshot is available") {
		t.Fatal("a non-interactive confirmation must fall back to a fresh workspace")
	}
	auto := Client{Config: config.Defaults()}
	auto.Config.Resume.AutoFresh = true
	if !auto.confirmFreshResume(context.Background(), "session", "no recovery snapshot is available") {
		t.Fatal("resume.auto_fresh must skip the confirmation")
	}
}

func TestAcceptsFreshWorkspaceIgnoresUnrelatedLaunches(t *testing.T) {
	client := Client{Config: config.Defaults()}
	recovery := restoreFailure()
	resuming := launchPlan{resuming: true, target: resumeTarget{WXSessionID: "old-session"}}
	for name, plan := range map[string]launchPlan{
		"already fresh": {resuming: true, fresh: true, target: resumeTarget{WXSessionID: "old-session"}},
		"not resuming":  {},
		"unmanaged":     {resuming: true},
	} {
		if client.acceptsFreshWorkspace(context.Background(), plan, recovery) {
			t.Fatalf("%s must not be retried in a fresh workspace", name)
		}
	}
	if client.acceptsFreshWorkspace(context.Background(), resuming, errors.New("unrelated failure")) {
		t.Fatal("a failure without the recovery marker must not be retried")
	}
	if !client.acceptsFreshWorkspace(context.Background(), resuming, recovery) {
		t.Fatal("a recovery failure on a managed resume must be retried")
	}
}
