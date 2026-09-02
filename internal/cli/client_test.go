package cli

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/HappyOnigiri/WX/internal/config"
	"github.com/HappyOnigiri/WX/internal/daemon"
	"github.com/HappyOnigiri/WX/internal/rpc"
)

type launcherHandler struct {
	mu           sync.Mutex
	methods      []string
	lease        daemon.Lease
	agentPID     int
	failRegister bool
}

func (h *launcherHandler) Handle(_ context.Context, method string, raw json.RawMessage) (any, error) {
	h.mu.Lock()
	h.methods = append(h.methods, method)
	if method == "RegisterAgentProcess" {
		var params struct {
			AgentPID int `json:"agent_pid"`
		}
		if json.Unmarshal(raw, &params) == nil {
			h.agentPID = params.AgentPID
		}
	}
	h.mu.Unlock()
	if method == "RegisterAgentProcess" && h.failRegister {
		return nil, errors.New("injected registration failure")
	}
	switch method {
	case "ResolveAndLease", "Resume", "AllocateResumeSlot":
		return h.lease, nil
	case "ResumeStatus":
		return map[string]bool{"expired": false}, nil
	default:
		return map[string]bool{"ok": true}, nil
	}
}

func TestRunAgentFailsClosedWhenAgentRegistrationFails(t *testing.T) {
	temp, err := os.MkdirTemp("/tmp", "wx-cli-register-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(temp) })
	socket := filepath.Join(temp, "wxd.sock")
	workspace := filepath.Join(temp, "workspace")
	if err := os.Mkdir(workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	handler := &launcherHandler{lease: daemon.Lease{SessionID: "session", Token: "token", Path: workspace, Ready: true}, failRegister: true}
	ctx, cancel := context.WithCancel(context.Background())
	server := &rpc.Server{Socket: socket, Handler: handler}
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx) }()
	waitForPath(t, socket)
	agentScript := filepath.Join(temp, "agent")
	if err := os.WriteFile(agentScript, []byte("#!/bin/sh\nexec </dev/null >/dev/null 2>&1\nwhile kill -0 \"$1\" 2>/dev/null; do sleep 0.05; done\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	client := Client{RPC: rpc.Client{Socket: socket, Timeout: time.Second}, Config: config.Defaults()}
	if exit := client.RunAgent(ctx, agentScript, []string{strconv.Itoa(os.Getpid())}, nil, false, ""); exit != 1 {
		t.Fatalf("RunAgent exit=%d", exit)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestRunAgentHelperProcess(t *testing.T) {
	if os.Getenv("WX_RUN_AGENT_HELPER") != "1" {
		return
	}
	client := Client{RPC: rpc.Client{Socket: os.Getenv("WX_HELPER_SOCKET"), Timeout: time.Second}, Config: config.Defaults()}
	os.Exit(client.RunAgent(context.Background(), os.Getenv("WX_HELPER_AGENT"), []string{os.Getenv("WX_HELPER_PID_FILE"), os.Getenv("WX_HELPER_GUARDIAN_PID")}, nil, false, ""))
}

func TestSupervisorKillLeavesRegisteredAgentProtected(t *testing.T) {
	temp, err := os.MkdirTemp("/tmp", "wx-cli-kill-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(temp) })
	socket := filepath.Join(temp, "wxd.sock")
	workspace := filepath.Join(temp, "workspace")
	if err := os.Mkdir(workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	handler := &launcherHandler{lease: daemon.Lease{SessionID: "session", Token: "token", Path: workspace, SourceWorkspace: temp, Ready: true}}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	server := &rpc.Server{Socket: socket, Handler: handler}
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx) }()
	waitForPath(t, socket)
	agentScript := filepath.Join(temp, "agent")
	pidFile := filepath.Join(temp, "agent.pid")
	if err := os.WriteFile(agentScript, []byte("#!/bin/sh\nexec </dev/null >/dev/null 2>&1\nprintf '%s' $$ > \"$1\"\nwhile kill -0 \"$2\" 2>/dev/null; do sleep 0.05; done\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	supervisor := exec.Command(os.Args[0], "-test.run=^TestRunAgentHelperProcess$")
	supervisor.Env = append(os.Environ(), "WX_RUN_AGENT_HELPER=1", "WX_HELPER_SOCKET="+socket, "WX_HELPER_AGENT="+agentScript, "WX_HELPER_PID_FILE="+pidFile, "WX_HELPER_GUARDIAN_PID="+strconv.Itoa(os.Getpid()))
	if err := supervisor.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if supervisor.Process != nil {
			_ = supervisor.Process.Kill()
		}
		_ = supervisor.Wait()
	})
	waitForPathWithin(t, pidFile, 10*time.Second)
	var agentPID int
	// Coverage, race, and mutation jobs execute this helper while the machine is
	// saturated. The agent may already be running before the helper process gets
	// enough CPU to complete its registration RPC, so allow the same bounded
	// startup window used by the other process-level tests.
	waitUntilCLI(t, 10*time.Second, func() bool {
		handler.mu.Lock()
		defer handler.mu.Unlock()
		agentPID = handler.agentPID
		return agentPID > 0
	})
	if err := supervisor.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := supervisor.Wait(); err == nil {
		t.Fatal("killed supervisor exited successfully")
	}
	supervisor.Process = nil
	if err := syscall.Kill(agentPID, 0); err != nil {
		t.Fatalf("registered agent did not survive supervisor kill: %v", err)
	}
	if err := syscall.Kill(agentPID, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func waitForPath(t *testing.T, path string) {
	t.Helper()
	waitForPathWithin(t, path, 3*time.Second)
}

func waitForPathWithin(t *testing.T, path string, timeout time.Duration) {
	t.Helper()
	waitUntilCLI(t, timeout, func() bool {
		_, err := os.Lstat(path)
		return err == nil
	})
}

func waitUntilCLI(t *testing.T, timeout time.Duration, predicate func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !predicate() {
		if time.Now().After(deadline) {
			t.Fatal("condition did not become true")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestReadinessHooksRequireCommandsUnderMatchingEvents(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	path := filepath.Join(home, ".codex", "hooks.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	valid := `{"hooks":{"SessionStart":[{"hooks":[{"type":"command","command":"/usr/local/bin/wx hook session-start"}]}],"UserPromptSubmit":[{"hooks":[{"type":"command","command":"wx hook user-prompt-submit"}]}],"PreToolUse":[{"hooks":[{"type":"command","command":"wx hook pre-tool-use"}]}]}}`
	if err := os.WriteFile(path, []byte(valid), 0o600); err != nil {
		t.Fatal(err)
	}
	if !readinessHooksAvailable("codex") {
		t.Fatal("structurally valid hooks were not detected")
	}

	wrongEvent := `{"hooks":{"SessionStart":[{"hooks":[{"type":"command","command":"wx hook session-start"},{"type":"command","command":"wx hook user-prompt-submit"},{"type":"command","command":"wx hook pre-tool-use"}]}]}}`
	if err := os.WriteFile(path, []byte(wrongEvent), 0o600); err != nil {
		t.Fatal(err)
	}
	if readinessHooksAvailable("codex") {
		t.Fatal("commands under the wrong events enabled asynchronous startup")
	}
}

func TestReadinessHooksRejectDisabledSubstringAndMalformedConfigurations(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	path := filepath.Join(home, ".codex", "hooks.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	cases := []string{
		`{"description":"wx hook session-start wx hook user-prompt-submit wx hook pre-tool-use"}`,
		`{"hooks":{"SessionStart":[{"disabled":true,"hooks":[{"type":"command","command":"wx hook session-start"}]}],"UserPromptSubmit":[{"hooks":[{"type":"command","command":"wx hook user-prompt-submit"}]}],"PreToolUse":[{"hooks":[{"type":"command","command":"wx hook pre-tool-use"}]}]}}`,
		`{"hooks":`,
		`{"hooks":{"SessionStart":[{"hooks":[{"type":"command","command":"wx hook session-start && false"}]}],"UserPromptSubmit":[],"PreToolUse":[]}}`,
	}
	for _, document := range cases {
		if err := os.WriteFile(path, []byte(document), 0o600); err != nil {
			t.Fatal(err)
		}
		if readinessHooksAvailable("codex") {
			t.Fatalf("unsafe hook configuration was accepted: %s", document)
		}
	}
}

func TestRunAgentSupervisesChildAndReleasesLease(t *testing.T) {
	temp, err := os.MkdirTemp("/tmp", "wx-cli-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(temp) })
	socket := filepath.Join(temp, "wxd.sock")
	workspace := filepath.Join(temp, "workspace")
	if err := os.Mkdir(workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	handler := &launcherHandler{lease: daemon.Lease{SessionID: "session", Token: "token", Path: workspace, SourceWorkspace: temp, Ready: true}}
	ctx, cancel := context.WithCancel(context.Background())
	server := &rpc.Server{Socket: socket, Handler: handler}
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx) }()
	for deadline := time.Now().Add(time.Second); ; {
		if _, err := os.Lstat(socket); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("RPC server did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	script := filepath.Join(temp, "agent")
	result := filepath.Join(temp, "result")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nprintf '%s\\n%s\\n%s\\n%s\\n' \"$PWD\" \"$WX_SESSION_ID\" \"$$\" \"$(ps -o pgid= -p $$)\" > \"$1\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	client := Client{RPC: rpc.Client{Socket: socket, Timeout: time.Second}, Config: config.Defaults()}
	if exit := client.RunAgent(ctx, script, []string{result}, nil, false, ""); exit != 0 {
		t.Fatalf("RunAgent exit=%d", exit)
	}
	data, err := os.ReadFile(result)
	canonicalWorkspace, _ := filepath.EvalSymlinks(workspace)
	fields := strings.Fields(string(data))
	if err != nil || len(fields) != 4 || fields[0] != canonicalWorkspace || fields[1] != "session" {
		t.Fatalf("child environment=%q err=%v", data, err)
	}
	pid, pidErr := strconv.Atoi(fields[2])
	pgid, pgidErr := strconv.Atoi(fields[3])
	if pidErr != nil || pgidErr != nil || pgid != pid || pgid == syscall.Getpgrp() {
		t.Fatalf("child process group: pid=%d pgid=%d parent_pgid=%d pid_err=%v pgid_err=%v", pid, pgid, syscall.Getpgrp(), pidErr, pgidErr)
	}
	handler.mu.Lock()
	methods := append([]string(nil), handler.methods...)
	handler.mu.Unlock()
	for _, required := range []string{"Status", "ResolveAndLease", "RegisterAgentProcess", "Release"} {
		found := false
		for _, method := range methods {
			found = found || method == required
		}
		if !found {
			t.Fatalf("methods=%v missing %s", methods, required)
		}
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestAgentArgumentAdapters(t *testing.T) {
	if !isNativeResume("codex", []string{"resume"}) || !isNativeResume("claude", []string{"--resume=id"}) || isNativeResume("codex", []string{"exec"}) {
		t.Fatal("native resume detection mismatch")
	}
	if got := codexResumeArgs([]string{"resume"}); len(got) != 2 || got[1] != "--all" {
		t.Fatalf("picker args=%v", got)
	}
	for _, args := range [][]string{{"resume", "--last"}, {"resume", "session"}, {"resume", "--all"}} {
		if got := codexResumeArgs(args); len(got) != len(args) {
			t.Fatalf("targeted resume changed: %v -> %v", args, got)
		}
	}
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	_ = write.Close()
	originalStdin := os.Stdin
	os.Stdin = read
	t.Cleanup(func() { os.Stdin = originalStdin; _ = read.Close() })
	if confirmExpiredResume("session") {
		t.Fatal("non-interactive expired resume was confirmed")
	}
}
