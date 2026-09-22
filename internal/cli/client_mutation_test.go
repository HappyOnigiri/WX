package cli

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/HappyOnigiri/WX/internal/config"
	"github.com/HappyOnigiri/WX/internal/daemon"
)

func TestClientNewMutationBoundariesKeepRPCTimeouts(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	client, err := New(config.Defaults())
	if err != nil {
		t.Fatal(err)
	}
	if client.RPC.Timeout != 5*time.Second || client.RPC.ConnectRetry != 2*time.Second {
		t.Fatalf("RPC=%+v, want five-second calls and two-second connect retry", client.RPC)
	}
	if client.daemonGate == nil {
		t.Fatal("New returned a client without a shared daemon gate")
	}
}

func TestInterruptedDuringSetupMutationBoundariesDistinguishSignalAndCancel(t *testing.T) {
	parent, cancelParent := context.WithCancel(context.Background())
	setup, cancelSetup := context.WithCancel(parent)
	if interruptedDuringSetup(parent, setup) {
		t.Fatal("an active setup context was reported as interrupted")
	}
	cancelSetup()
	if !interruptedDuringSetup(parent, setup) {
		t.Fatal("setup cancellation was not reported as an interruption")
	}
	cancelParent()
	if interruptedDuringSetup(parent, setup) {
		t.Fatal("caller cancellation was reported as a signal interruption")
	}
}

func TestChildEnvironmentMutationBoundariesKeepOnlyTheLatestWXValues(t *testing.T) {
	base := []string{"PATH=/bin", "WX_SESSION_ID=old", "KEEP=one"}
	overrides := []string{"WX_SESSION_ID=new", "WX_SOURCE_CWD=/source"}
	got := childEnvironment(base, overrides)
	want := []string{"PATH=/bin", "KEEP=one", "WX_SESSION_ID=new", "WX_SOURCE_CWD=/source"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("childEnvironment()=%v, want %v", got, want)
	}
}

func TestLaunchMutationBoundariesUseDiscoveryBudgetForUnboundedResumeReadiness(t *testing.T) {
	root := t.TempDir()
	cfg := config.Defaults()
	cfg.RepositoryDefaults.Readiness.Timeout = &config.Duration{}
	handler := &resumeLaunchHandler{lease: daemon.Lease{SessionID: "resume-session", Token: "resume-token", Path: root, Ready: true}}
	client, stop := serveResumeLaunchRPCWithConfig(t, handler, cfg)
	defer stop()
	if exit, relaunch := client.launch(context.Background(), launchPlan{
		agent: "true", cwd: root, resuming: true, target: resumeTarget{WXSessionID: "wx-session"},
	}); exit != 0 || relaunch != nil {
		t.Fatalf("launch with zero readiness timeout: exit=%d relaunch=%v", exit, relaunch)
	}
	if methods := handler.methodsSnapshot(); !containsMethod(methods, "Resume") {
		t.Fatalf("methods=%v, want Resume to reach the daemon", methods)
	}
}

func TestLaunchMutationBoundariesUsesReadinessBudgetForResumeLease(t *testing.T) {
	root := t.TempDir()
	cfg := config.Defaults()
	cfg.RepositoryDefaults.Readiness.Timeout = &config.Duration{Duration: 500 * time.Millisecond}
	handler := &resumeLaunchHandler{lease: daemon.Lease{SessionID: "resume-session", Token: "resume-token", Path: root, Ready: true}}
	client, stop := serveResumeLaunchRPCWithConfig(t, handler, cfg)
	defer stop()
	if exit, relaunch := client.launch(context.Background(), launchPlan{
		agent: "true", cwd: root, resuming: true, target: resumeTarget{WXSessionID: "wx-session"},
	}); exit != 0 || relaunch != nil {
		t.Fatalf("launch with bounded readiness timeout: exit=%d relaunch=%v", exit, relaunch)
	}
	deadlines := handler.deadlinesFor("Resume")
	if len(deadlines) != 1 {
		t.Fatalf("Resume deadlines=%v, want one request deadline", deadlines)
	}
	remaining := time.Until(deadlines[0])
	if remaining <= 0 || remaining > time.Second {
		t.Fatalf("Resume deadline remaining=%s, want the configured readiness budget", remaining)
	}
}

func TestLaunchMutationBoundariesPreserveExplicitCodexArguments(t *testing.T) {
	root := t.TempDir()
	record := filepath.Join(t.TempDir(), "launch-record")
	t.Setenv("WX_TEST_LAUNCH_RECORD", record)
	t.Setenv("WX_TEST_EVENT_RECORD", filepath.Join(t.TempDir(), "launch-events"))
	agent := writeLaunchRecorder(t, "codex")
	prependPath(t, filepath.Dir(agent))
	handler := &resumeLaunchHandler{lease: daemon.Lease{SessionID: "resume-session", Token: "resume-token", Path: root, Ready: true}}
	client, stop := serveResumeLaunchRPC(t, handler)
	defer stop()
	if exit, relaunch := client.launch(context.Background(), launchPlan{
		agent: "codex", args: []string{"--model", "gpt-5.6-sol"}, cwd: root,
		resuming: true, explicitResume: "wx-session", intentKind: resumeIntentNone,
		target: resumeTarget{WXSessionID: "wx-session", AgentSessionID: "native-session"},
	}); exit != 0 || relaunch != nil {
		t.Fatalf("codex launch: exit=%d relaunch=%v", exit, relaunch)
	}
	launch := readLaunchRecord(t, record)
	if got := launch["args"]; got != "resume --cd "+root+" native-session --model gpt-5.6-sol" {
		t.Fatalf("Codex args=%q, want explicit arguments preserved", got)
	}
}

func TestLaunchMutationBoundariesDoNotRenderAnEmptySetupFailureReport(t *testing.T) {
	root := t.TempDir()
	handler := &resumeLaunchHandler{lease: daemon.Lease{SessionID: "session", Token: "token", Path: root, Ready: false}, waitReadyErrors: []error{errors.New("injected readiness failure")}}
	client, stop := serveResumeLaunchRPC(t, handler)
	defer stop()
	stderr := captureStderrForLease(t, func() {
		if exit, relaunch := client.launch(context.Background(), launchPlan{agent: "true", cwd: root, setupResolved: true}); exit != 1 || relaunch != nil {
			t.Fatalf("launch exit=%d relaunch=%v", exit, relaunch)
		}
	})
	if strings.Contains(stderr, "a workspace could not be prepared for the probe") {
		t.Fatalf("stderr=%q, empty setup check unexpectedly rendered a probe report", stderr)
	}
}

func TestLaunchMutationBoundariesRenderASetupFailureReportForRepositories(t *testing.T) {
	root := t.TempDir()
	handler := &resumeLaunchHandler{lease: daemon.Lease{SessionID: "session", Token: "token", Path: root, SourceWorkspace: root, Ready: false}, waitReadyErrors: []error{errors.New("injected readiness failure")}}
	client, stop := serveResumeLaunchRPC(t, handler)
	defer stop()
	stderr := captureStderrForLease(t, func() {
		if exit, relaunch := client.launch(context.Background(), launchPlan{
			agent: "true", cwd: root, setupResolved: true,
			setupCheckRepositories: []daemon.SetupCheckRepository{{RelativePath: ".", MainPath: root, DirName: "repo"}},
		}); exit != 1 || relaunch != nil {
			t.Fatalf("launch exit=%d relaunch=%v", exit, relaunch)
		}
	})
	if !strings.Contains(stderr, "a workspace could not be prepared for the probe") {
		t.Fatalf("stderr=%q, want setup failure report", stderr)
	}
}

func TestForwardAgentSignalMutationBoundariesReachesTheAgentProcessGroup(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "child-terminated")
	pidPath := filepath.Join(t.TempDir(), "child-pid")
	// shell 実装が外部 sleep の終了を待ってから trap を処理しても、期限内に観測できる周期にする。
	script := `(trap 'printf terminated > "$WX_TEST_CHILD_MARKER"; exit 0' TERM; printf '%s' "$$" > "$WX_TEST_CHILD_PID"; while :; do sleep 0.1; done) & wait`
	cmd := exec.Command("/bin/sh", "-c", script)
	cmd.Env = append(os.Environ(), "WX_TEST_CHILD_MARKER="+marker, "WX_TEST_CHILD_PID="+pidPath)
	configureAgentProcess(cmd, -1)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	cleanupChild := func() {
		if data, err := os.ReadFile(pidPath); err == nil {
			if pid, parseErr := strconv.Atoi(string(data)); parseErr == nil {
				_ = syscall.Kill(pid, syscall.SIGKILL)
			}
		}
	}
	t.Cleanup(cleanupChild)
	deadline := time.Now().Add(time.Second)
	for {
		if _, err := os.Stat(pidPath); err == nil {
			break
		}
		if time.Now().After(deadline) {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			t.Fatal("child process did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	forwardAgentSignal(cmd, syscall.SIGTERM)
	if err := cmd.Wait(); err == nil {
		t.Fatal("agent process unexpectedly exited successfully")
	}
	deadline = time.Now().Add(time.Second)
	for {
		if _, err := os.Stat(marker); err == nil {
			return
		}
		if time.Now().After(deadline) {
			// 最初の stat と期限判定の間に marker が作られることがあるため、
			// timeout を報告する前にもう一度確認する。
			if _, err := os.Stat(marker); err == nil {
				return
			}
			t.Fatal("SIGTERM did not reach the child process group")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
