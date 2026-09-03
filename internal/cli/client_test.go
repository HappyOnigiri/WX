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
	"github.com/HappyOnigiri/WX/internal/domain"
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

func TestChildEnvironmentScrubsInheritedWXInvocationState(t *testing.T) {
	base := []string{
		"PATH=/bin",
		"WX_SESSION_ID=parent",
		"WX_SESSION_TOKEN=parent-token",
		"WX_NATIVE_RESUME=1",
		"WX_EXPLICIT_RESUME=1",
		"WX_FRESH=1",
		"WX_RECOVERY_DISCARDED=1",
		"KEEP=present",
	}
	child := childEnvironment(base, []string{"WX_SESSION_ID=child", "WX_SESSION_TOKEN=child-token"})
	values := make(map[string]string)
	for _, entry := range child {
		key, value, ok := strings.Cut(entry, "=")
		if ok {
			if _, exists := values[key]; exists {
				t.Fatalf("environment contains duplicate key %q: %v", key, child)
			}
			values[key] = value
		}
	}
	if values["WX_SESSION_ID"] != "child" || values["WX_SESSION_TOKEN"] != "child-token" || values["KEEP"] != "present" {
		t.Fatalf("current child environment was not applied: %v", values)
	}
	for _, key := range []string{"WX_NATIVE_RESUME", "WX_EXPLICIT_RESUME", "WX_FRESH", "WX_RECOVERY_DISCARDED"} {
		if _, exists := values[key]; exists {
			t.Fatalf("parent invocation mode %s leaked into child: %v", key, values)
		}
	}
}

func TestRunAgentUsesForegroundReadyFallbackWhenHooksAreUnavailable(t *testing.T) {
	temp, err := os.MkdirTemp("/tmp", "wx-cli-fallback-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(temp) })
	t.Setenv("HOME", filepath.Join(temp, "home"))
	for _, key := range []string{"WX_NATIVE_RESUME", "WX_EXPLICIT_RESUME", "WX_FRESH", "WX_RECOVERY_DISCARDED"} {
		t.Setenv(key, "1")
	}
	socket := filepath.Join(temp, "wxd.sock")
	workspace := filepath.Join(temp, "workspace")
	if err := os.Mkdir(workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	handler := &launcherHandler{lease: daemon.Lease{SessionID: "fallback-session", Token: "fallback-token", Path: workspace, Ready: false}}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	server := &rpc.Server{Socket: socket, Handler: handler}
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx) }()
	waitUntilCLI(t, 3*time.Second, func() bool {
		if _, err := os.Lstat(socket); err == nil {
			return true
		}
		select {
		case err := <-done:
			t.Fatalf("fallback test server stopped before listening: %v", err)
			return false
		default:
			return false
		}
	})
	agentScript := filepath.Join(temp, "agent")
	if err := os.WriteFile(agentScript, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	client := Client{RPC: rpc.Client{Socket: socket, Timeout: time.Second}, Config: config.Defaults()}
	if exit := client.RunAgent(ctx, agentScript, nil, nil, false, ""); exit != 0 {
		t.Fatalf("RunAgent exit=%d", exit)
	}
	handler.mu.Lock()
	methods := append([]string(nil), handler.methods...)
	handler.mu.Unlock()
	waitIndex, registerIndex := -1, -1
	for index, method := range methods {
		if method == "WaitReady" {
			waitIndex = index
		}
		if method == "RegisterAgentProcess" {
			registerIndex = index
		}
	}
	if waitIndex < 0 || registerIndex < 0 || waitIndex > registerIndex {
		t.Fatalf("unavailable hooks did not gate child startup: methods=%v", methods)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
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

// NEW-1: replacing the configured root after the lease directory is opened
// must not redirect the agent CWD to the replacement directory.
func TestRunAgentKeepsDescriptorBoundCWDAcrossRootReplacement(t *testing.T) {
	base, err := os.MkdirTemp("", "wx-cwd-")
	if err != nil {
		t.Fatal(err)
	}
	base, err = filepath.EvalSymlinks(base)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })
	root := filepath.Join(base, "worktrees")
	workspace := filepath.Join(root, "workspace")
	outside := filepath.Join(base, "outside")
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(outside, "workspace"), 0o700); err != nil {
		t.Fatal(err)
	}
	directory, identity, err := domain.OpenOwnedDirectory(root, workspace)
	if err != nil {
		t.Fatal(err)
	}
	if err := directory.Close(); err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(base, "wxd.sock")
	handler := &launcherHandler{lease: daemon.Lease{SessionID: "session", Token: "token", Path: workspace, RootIdentity: identity, Ready: true}}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	server := &rpc.Server{Socket: socket, Handler: handler}
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx) }()
	waitForPath(t, socket)
	agent := filepath.Join(base, "agent")
	result := filepath.Join(base, "agent-pwd")
	if err := os.WriteFile(agent, []byte("#!/bin/sh\npwd -P > \"$1\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	oldRoot := root + "-old"
	cfg := config.Defaults()
	cfg.Storage.WorktreeRoot = root
	client := Client{
		RPC:    rpc.Client{Socket: socket, Timeout: time.Second},
		Config: cfg,
		beforeAgentStart: func() {
			if err := os.Rename(root, oldRoot); err != nil {
				t.Fatalf("replace configured root: %v", err)
			}
			if err := os.Symlink(outside, root); err != nil {
				t.Fatalf("install replacement root: %v", err)
			}
		},
	}
	if exit := client.RunAgent(ctx, agent, []string{result}, nil, false, ""); exit != 0 {
		t.Fatalf("RunAgent exit=%d", exit)
	}
	data, err := os.ReadFile(result)
	if err != nil {
		t.Fatal(err)
	}
	want, err := filepath.EvalSymlinks(filepath.Join(oldRoot, "workspace"))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(data)); got != want {
		t.Fatalf("agent CWD=%q want pinned %q", got, want)
	}
	if _, err := os.Stat(filepath.Join(outside, "workspace", "agent-pwd")); !os.IsNotExist(err) {
		t.Fatalf("replacement directory received agent output: %v", err)
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
