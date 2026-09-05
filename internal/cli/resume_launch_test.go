package cli

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/HappyOnigiri/WX/internal/config"
	"github.com/HappyOnigiri/WX/internal/daemon"
	"github.com/HappyOnigiri/WX/internal/hookconfig"
	"github.com/HappyOnigiri/WX/internal/rpc"
)

func TestRunResumeAgentOmittedFreshSetsRecoveryModeAndWaitsBeforeLaunch(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	installReadinessHooks(t, home, "claude")
	if !hookconfig.Available("claude") {
		t.Fatal("Claude readiness hook fixture was not accepted")
	}

	workspace := filepath.Join(t.TempDir(), "restored-slot")
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	record := filepath.Join(t.TempDir(), "launch-record")
	eventLog := filepath.Join(t.TempDir(), "launch-events")
	t.Setenv("WX_TEST_LAUNCH_RECORD", record)
	t.Setenv("WX_TEST_EVENT_RECORD", eventLog)
	agent := writeLaunchRecorder(t, "claude")
	prependPath(t, filepath.Dir(agent))
	handler := &resumeLaunchHandler{
		lease:    daemon.Lease{SessionID: "new-session", Token: "new-token", Path: workspace, Ready: false},
		status:   resumeStatus{WXSessionID: "old-session", Agent: "claude", AgentSessionID: "native-claude"},
		eventLog: eventLog,
	}
	client, stop := serveResumeLaunchRPC(t, handler)

	if exit := client.RunResume(context.Background(), "old-session", "", nil, nil, true); exit != 0 {
		t.Fatalf("RunResume exit=%d", exit)
	}

	params := handler.paramsFor("Resume")
	var resumeParams map[string]any
	if err := json.Unmarshal(params, &resumeParams); err != nil {
		t.Fatal(err)
	}
	if got, ok := resumeParams["fresh"].(bool); !ok || !got {
		t.Fatalf("Resume fresh parameter=%v, want true", resumeParams["fresh"])
	}
	launch := readLaunchRecord(t, record)
	if got := launch["WX_RECOVERY_DISCARDED"]; got != "1" {
		t.Fatalf("WX_RECOVERY_DISCARDED=%q, want 1; record=%v", got, launch)
	}
	if got := launch["args"]; got != "--resume native-claude" {
		t.Fatalf("Claude resume args=%q, want --resume native-claude", got)
	}
	if events := readEventLog(t, eventLog); !eventsBefore(events, "rpc:WaitReady", "agent") {
		t.Fatalf("WaitReady was not completed before agent launch: %v", events)
	}
	stop()
}

func TestRunResumeCodexAddsResumeCDAndNativeID(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	workspace := filepath.Join(t.TempDir(), "new-codex-slot")
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	record := filepath.Join(t.TempDir(), "launch-record")
	eventLog := filepath.Join(t.TempDir(), "launch-events")
	t.Setenv("WX_TEST_LAUNCH_RECORD", record)
	t.Setenv("WX_TEST_EVENT_RECORD", eventLog)
	agent := writeLaunchRecorder(t, "codex")
	prependPath(t, filepath.Dir(agent))
	handler := &resumeLaunchHandler{
		lease:  daemon.Lease{SessionID: "codex-session", Token: "codex-token", Path: workspace, Ready: true},
		status: resumeStatus{WXSessionID: "old-codex", Agent: "codex", AgentSessionID: "native-codex"},
	}
	client, stop := serveResumeLaunchRPC(t, handler)

	if exit := client.RunResume(context.Background(), "old-codex", "codex", []string{"--model", "o3"}, nil, false); exit != 0 {
		t.Fatalf("RunResume exit=%d", exit)
	}
	launch := readLaunchRecord(t, record)
	want := "resume --cd " + workspace + " native-codex --model o3"
	if got := launch["args"]; got != want {
		t.Fatalf("Codex resume args=%q, want %q; record=%v", got, want, launch)
	}
	if got := launch["WX_RECOVERY_DISCARDED"]; got != "" {
		t.Fatalf("unexpected recovery marker for normal resume: %q", got)
	}
	stop()
}

func TestRunAgentUnknownResumeIDPassesOriginalArgumentsThrough(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	record := filepath.Join(t.TempDir(), "launch-record")
	t.Setenv("WX_TEST_LAUNCH_RECORD", record)
	t.Setenv("WX_TEST_EVENT_RECORD", filepath.Join(t.TempDir(), "launch-events"))
	agent := writeLaunchRecorder(t, "claude")
	prependPath(t, filepath.Dir(agent))
	cfg := config.Defaults()
	cfg.Sessions.Paths.Claude.Sessions = []string{filepath.Join(home, "no-session-history")}
	workspace := filepath.Join(t.TempDir(), "ordinary-slot")
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	handler := &resumeLaunchHandler{
		lease:     daemon.Lease{SessionID: "ordinary-session", Token: "ordinary-token", Path: workspace, Ready: true},
		resumeErr: errors.New("no rows for agent session"),
	}
	client, stop := serveResumeLaunchRPCWithConfig(t, handler, cfg)

	wantArgs := []string{"--resume", "unknown-native", "--flag", "value"}
	if exit := client.RunAgent(context.Background(), "claude", wantArgs, nil, false); exit != 0 {
		t.Fatalf("RunAgent exit=%d", exit)
	}
	launch := readLaunchRecord(t, record)
	if got := launch["args"]; got != strings.Join(wantArgs, " ") {
		t.Fatalf("unknown ID args=%q, want %q", got, strings.Join(wantArgs, " "))
	}
	if methods := handler.methodsSnapshot(); !containsMethod(methods, "ResolveAndLease") || containsMethod(methods, "Resume") {
		t.Fatalf("unknown ID RPC methods=%v, want ResolveAndLease without Resume", methods)
	}
	stop()
}

type resumeLaunchHandler struct {
	mu        sync.Mutex
	events    []string
	params    map[string]json.RawMessage
	lease     daemon.Lease
	status    resumeStatus
	resumeErr error
	eventLog  string
}

func (h *resumeLaunchHandler) Handle(_ context.Context, method string, raw json.RawMessage) (any, error) {
	h.mu.Lock()
	h.events = append(h.events, method)
	if h.params == nil {
		h.params = map[string]json.RawMessage{}
	}
	h.params[method] = append(json.RawMessage(nil), raw...)
	h.mu.Unlock()
	if h.eventLog != "" {
		if err := appendEvent(h.eventLog, "rpc:"+method); err != nil {
			return nil, err
		}
	}

	switch method {
	case "Status":
		return map[string]bool{"ok": true}, nil
	case "ResumeStatus":
		if h.resumeErr != nil {
			return nil, h.resumeErr
		}
		return h.status, nil
	case "Resume", "ResolveAndLease":
		return h.lease, nil
	case "WaitReady":
		return map[string]bool{"ok": true}, nil
	case "RegisterAgentProcess", "Release":
		return map[string]bool{"ok": true}, nil
	default:
		return map[string]bool{"ok": true}, nil
	}
}

func (h *resumeLaunchHandler) eventsSnapshot() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.events...)
}

func (h *resumeLaunchHandler) methodsSnapshot() []string { return h.eventsSnapshot() }

func (h *resumeLaunchHandler) paramsFor(method string) json.RawMessage {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append(json.RawMessage(nil), h.params[method]...)
}

func serveResumeLaunchRPC(t *testing.T, handler *resumeLaunchHandler) (Client, func()) {
	return serveResumeLaunchRPCWithConfig(t, handler, config.Defaults())
}

func serveResumeLaunchRPCWithConfig(t *testing.T, handler *resumeLaunchHandler, cfg config.Config) (Client, func()) {
	t.Helper()
	socket := filepath.Join(t.TempDir(), "wxd.sock")
	ctx, cancel := context.WithCancel(context.Background())
	server := &rpc.Server{Socket: socket, Handler: handler}
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx) }()
	waitForPath(t, socket)
	client := Client{RPC: rpc.Client{Socket: socket, Timeout: time.Second}, Config: cfg}
	var stopOnce sync.Once
	stop := func() {
		stopOnce.Do(func() {
			cancel()
			if err := <-done; err != nil {
				t.Errorf("resume launch RPC server: %v", err)
			}
		})
	}
	t.Cleanup(stop)
	return client, stop
}

func writeLaunchRecorder(t *testing.T, name string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	script := `#!/bin/sh
printf 'agent\n' >> "$WX_TEST_EVENT_RECORD"
{
  printf 'pwd=%s\n' "$PWD"
  printf 'WX_RECOVERY_DISCARDED=%s\n' "${WX_RECOVERY_DISCARDED-}"
  printf 'args='
  printf '%s ' "$@"
  printf '\n'
} > "$WX_TEST_LAUNCH_RECORD"
exit 0
`
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func prependPath(t *testing.T, directory string) {
	t.Helper()
	path := directory
	if existing := os.Getenv("PATH"); existing != "" {
		path += string(os.PathListSeparator) + existing
	}
	t.Setenv("PATH", path)
}

func readLaunchRecord(t *testing.T, path string) map[string]string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read launch record: %v", err)
	}
	values := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		key, value, ok := strings.Cut(line, "=")
		if ok {
			values[key] = strings.TrimSpace(value)
		}
	}
	return values
}

func eventsBefore(events []string, first, second string) bool {
	firstIndex, secondIndex := -1, -1
	for i, event := range events {
		if event == first && firstIndex < 0 {
			firstIndex = i
		}
		if event == second && secondIndex < 0 {
			secondIndex = i
		}
	}
	return firstIndex >= 0 && secondIndex >= 0 && firstIndex < secondIndex
}

func containsMethod(methods []string, want string) bool {
	return slicesContains(methods, want)
}

func appendEvent(path, event string) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer file.Close()
	_, err = file.WriteString(event + "\n")
	return err
}

func readEventLog(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read event log: %v", err)
	}
	var events []string
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line != "" {
			events = append(events, line)
		}
	}
	return events
}

func slicesContains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func installReadinessHooks(t *testing.T, home, agent string) {
	t.Helper()
	executable, err := hookconfig.CurrentExecutable()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(home, ".claude", "settings.json")
	if agent == "codex" {
		path = filepath.Join(home, ".codex", "hooks.json")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	document := map[string]any{"disableAllHooks": false, "hooks": map[string]any{}}
	hooks := document["hooks"].(map[string]any)
	for event, command := range map[string]string{
		"SessionStart":     "session-start",
		"UserPromptSubmit": "user-prompt-submit",
		"PreToolUse":       "pre-tool-use",
	} {
		hooks[event] = []any{map[string]any{"matcher": "*", "hooks": []any{map[string]any{"type": "command", "command": executable + " hook " + command}}}}
	}
	data, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}
