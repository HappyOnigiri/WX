package cli

import (
	"context"
	"encoding/json"
	"os"
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
	mu      sync.Mutex
	methods []string
	lease   daemon.Lease
}

func (h *launcherHandler) Handle(_ context.Context, method string, _ json.RawMessage) (any, error) {
	h.mu.Lock()
	h.methods = append(h.methods, method)
	h.mu.Unlock()
	switch method {
	case "ResolveAndLease", "Resume", "AllocateResumeSlot":
		return h.lease, nil
	case "ResumeStatus":
		return map[string]bool{"expired": false}, nil
	default:
		return map[string]bool{"ok": true}, nil
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
	if err := os.WriteFile(script, []byte("#!/bin/sh\nprintf '%s\\n%s\\n%s\\n' \"$PWD\" \"$WX_SESSION_ID\" \"$(ps -o pgid= -p $$)\" > \"$1\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	client := Client{RPC: rpc.Client{Socket: socket, Timeout: time.Second}, Config: config.Defaults()}
	if exit := client.RunAgent(ctx, script, []string{result}, nil, false, ""); exit != 0 {
		t.Fatalf("RunAgent exit=%d", exit)
	}
	data, err := os.ReadFile(result)
	canonicalWorkspace, _ := filepath.EvalSymlinks(workspace)
	want := canonicalWorkspace + "\nsession\n" + strconv.Itoa(syscall.Getpgrp()) + "\n"
	if err != nil || strings.Join(strings.Fields(string(data)), "\n")+"\n" != want {
		t.Fatalf("child environment=%q err=%v", data, err)
	}
	handler.mu.Lock()
	methods := append([]string(nil), handler.methods...)
	handler.mu.Unlock()
	for _, required := range []string{"Status", "ResolveAndLease", "Release"} {
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
