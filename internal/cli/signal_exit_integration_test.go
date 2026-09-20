package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/HappyOnigiri/WX/internal/config"
	"github.com/HappyOnigiri/WX/internal/daemon"
	"github.com/HappyOnigiri/WX/internal/rpc"
	"github.com/HappyOnigiri/WX/internal/testsupport"
)

// TestRunAgentSignalHelperProcess は親テストから SIGTERM を受ける agent client である。
func TestRunAgentSignalHelperProcess(t *testing.T) {
	if os.Getenv("WX_RUN_AGENT_SIGNAL_HELPER") != "1" {
		return
	}
	client := Client{
		RPC:    rpc.Client{Socket: os.Getenv("WX_HELPER_SOCKET"), Timeout: 2 * time.Second},
		Config: config.Defaults(),
	}
	os.Exit(client.runAgent(context.Background(), os.Getenv("WX_HELPER_AGENT"), nil, nil, false, ""))
}

func TestRunAgentNormalizesSignalExitAndReleasesLease(t *testing.T) {
	temp := t.TempDir()
	socket := testsupport.SocketPath(t, "wxd.sock")
	workspace := filepath.Join(temp, "workspace")
	if err := os.Mkdir(workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	handler := &launcherHandler{lease: daemon.Lease{SessionID: "signal-session", Token: "signal-token", Path: workspace, SourceWorkspace: temp, Ready: true}}
	ctx, cancel := context.WithCancel(context.Background())
	server := &rpc.Server{Socket: socket, Handler: handler}
	serverDone := make(chan error, 1)
	go func() { serverDone <- server.Serve(ctx) }()
	waitForSocket(t, socket, serverDone)
	defer func() {
		cancel()
		if err := <-serverDone; err != nil {
			t.Error(err)
		}
	}()

	agent := filepath.Join(temp, "agent")
	if err := os.WriteFile(agent, []byte("#!/bin/sh\nexec sleep 60\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	supervisor := exec.Command(os.Args[0], "-test.run=^TestRunAgentSignalHelperProcess$")
	var supervisorOutput bytes.Buffer
	supervisor.Stdout = &supervisorOutput
	supervisor.Stderr = &supervisorOutput
	supervisor.Env = append(os.Environ(),
		"WX_RUN_AGENT_SIGNAL_HELPER=1",
		"WX_HELPER_SOCKET="+socket,
		"WX_HELPER_AGENT="+agent,
	)
	if err := supervisor.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if supervisor.Process != nil {
			_ = supervisor.Process.Kill()
			_ = supervisor.Wait()
		}
	}()
	waitUntilCLI(t, 10*time.Second, func() bool {
		if supervisor.ProcessState != nil && supervisor.ProcessState.Exited() {
			t.Fatalf("signal helper exited before registering agent: %s", supervisorOutput.String())
		}
		handler.mu.Lock()
		defer handler.mu.Unlock()
		return handler.agentPID > 0
	})
	if err := supervisor.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	err := supervisor.Wait()
	supervisor.Process = nil
	if err == nil {
		t.Fatal("signal helper unexpectedly exited successfully")
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("signal helper error=%v, want an exit status", err)
	}
	wantExit := 128 + int(syscall.SIGTERM)
	if got := exitErr.ExitCode(); got != wantExit {
		t.Fatalf("signal helper exit=%d, want %d (SIGTERM)", got, wantExit)
	}
	handler.mu.Lock()
	methods := append([]string(nil), handler.methods...)
	reasons := append([]string(nil), handler.releaseReasons...)
	agentPID := handler.agentPID
	handler.mu.Unlock()
	if !containsMethod(methods, "Release") {
		t.Fatalf("methods=%v, want Release after signal exit", methods)
	}
	if len(reasons) == 0 || reasons[len(reasons)-1] != "client-exit" {
		t.Fatalf("release reasons=%v, want client-exit", reasons)
	}
	waitUntilCLI(t, 5*time.Second, func() bool { return syscall.Kill(agentPID, 0) != nil })
}
