package cli

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
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

func TestForwardAgentSignalMutationBoundariesReachesTheAgentProcessGroup(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "child-terminated")
	pidPath := filepath.Join(t.TempDir(), "child-pid")
	script := `(trap 'printf terminated > "$WX_TEST_CHILD_MARKER"; exit 0' TERM; while :; do sleep 1; done) & printf '%s' "$!" > "$WX_TEST_CHILD_PID"; wait`
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
			t.Fatal("SIGTERM did not reach the child process group")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
