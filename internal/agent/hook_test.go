package agent

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

	"github.com/HappyOnigiri/WX/internal/rpc"
)

type recordingHandler struct {
	mu      sync.Mutex
	methods []string
}

type failingHookReader struct{}

func (failingHookReader) Read([]byte) (int, error) { return 0, errors.New("read fault") }

func (h *recordingHandler) Handle(_ context.Context, method string, _ json.RawMessage) (any, error) {
	h.mu.Lock()
	h.methods = append(h.methods, method)
	h.mu.Unlock()
	return map[string]bool{"ok": true}, nil
}

func TestHookLifecyclePayloadsAndReadinessGates(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "wxd.sock")
	handler := &recordingHandler{}
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
	t.Setenv("WX_DAEMON_SOCKET", socket)
	t.Setenv("WX_SESSION_TOKEN", "token")
	t.Setenv("WX_READINESS_TIMEOUT", "2s")
	t.Setenv("WX_SESSION_ID", "wx-normal")
	if err := RunHook(ctx, "session-start", strings.NewReader(`{"session_id":"agent-normal","source":"startup"}`)); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WX_SESSION_ID", "wx-native")
	t.Setenv("WX_NATIVE_RESUME", "1")
	if err := RunHook(ctx, "session-start", strings.NewReader(`{"session_id":"agent-native","source":"resume"}`)); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WX_NATIVE_RESUME", "")
	t.Setenv("WX_SESSION_ID", "wx-fresh")
	t.Setenv("WX_FRESH", "1")
	if err := RunHook(ctx, "session-start", strings.NewReader(`{"session_id":"agent-fresh","source":"resume"}`)); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WX_FRESH", "")
	t.Setenv("WX_SESSION_ID", "wx-recovery-notice")
	t.Setenv("WX_RECOVERY_DISCARDED", "1")
	if err := RunHook(ctx, "session-start", strings.NewReader(`{"session_id":"agent-notice","source":"startup"}`)); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WX_RECOVERY_DISCARDED", "")
	for _, event := range []string{"user-prompt-submit", "pre-tool-use", "session-end"} {
		if err := RunHook(ctx, event, strings.NewReader("")); err != nil {
			t.Fatalf("%s: %v", event, err)
		}
	}
	handler.mu.Lock()
	got := strings.Join(handler.methods, ",")
	handler.mu.Unlock()
	for _, method := range []string{"BindAgentSession", "BindAndRestoreResume", "ValidateFreshResume", "WaitReady", "Release"} {
		if !strings.Contains(got, method) {
			t.Fatalf("methods=%s, missing %s", got, method)
		}
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestHookFailsClosedForMalformedEnvironmentAndPayload(t *testing.T) {
	t.Setenv("WX_SESSION_ID", "")
	t.Setenv("WX_SESSION_TOKEN", "")
	if err := RunHook(context.Background(), "unknown", strings.NewReader("")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WX_SESSION_ID", "wx")
	if err := RunHook(context.Background(), "session-start", strings.NewReader("")); err == nil {
		t.Fatal("incomplete environment succeeded")
	}
	t.Setenv("WX_SESSION_TOKEN", "token")
	t.Setenv("WX_DAEMON_SOCKET", filepath.Join(t.TempDir(), "missing.sock"))
	if err := RunHook(context.Background(), "session-start", strings.NewReader("{")); err == nil {
		t.Fatal("malformed payload succeeded")
	}
	if err := RunHook(context.Background(), "session-start", strings.NewReader(`{}`)); err == nil {
		t.Fatal("payload without agent session ID succeeded")
	}
	if err := RunHook(context.Background(), "unknown", strings.NewReader("")); err == nil {
		t.Fatal("unknown event succeeded")
	}
	if err := RunHook(context.Background(), "session-start", failingHookReader{}); err == nil || !strings.Contains(err.Error(), "read fault") {
		t.Fatalf("input read error=%v", err)
	}
	if err := RunHook(context.Background(), "session-start", strings.NewReader(`{"session_id":"agent"}`)); err == nil {
		t.Fatal("binding through missing daemon socket succeeded")
	}
	t.Setenv("WX_FRESH", "1")
	t.Setenv("WX_BRANCHES_JSON", "{")
	if err := RunHook(context.Background(), "session-start", strings.NewReader(`{"session_id":"agent"}`)); err == nil || !strings.Contains(err.Error(), "branch selection") {
		t.Fatalf("invalid fresh branches error=%v", err)
	}
	t.Setenv("WX_BRANCHES_JSON", `[]`)
	if err := RunHook(context.Background(), "session-start", strings.NewReader(`{"session_id":"agent"}`)); err == nil {
		t.Fatal("fresh validation through missing daemon socket succeeded")
	}
}

func TestRecordedClaudeAndCodexHookPayloads(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "wxd.sock")
	handler := &recordingHandler{}
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
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	})
	t.Setenv("WX_DAEMON_SOCKET", socket)
	t.Setenv("WX_SESSION_TOKEN", "token")
	t.Setenv("WX_READINESS_TIMEOUT", "2s")

	tests := []struct {
		name, fixture, event, method string
	}{
		{name: "claude startup", fixture: "claude-2.1.258-session-start-startup.json", event: "session-start", method: "BindAgentSession"},
		{name: "claude resume", fixture: "claude-2.1.258-session-start-resume.json", event: "session-start", method: "BindAndRestoreResume"},
		{name: "claude prompt", fixture: "claude-2.1.258-user-prompt-submit.json", event: "user-prompt-submit", method: "WaitReady"},
		{name: "claude tool", fixture: "claude-2.1.258-pre-tool-use.json", event: "pre-tool-use", method: "WaitReady"},
		{name: "claude end", fixture: "claude-2.1.258-session-end.json", event: "session-end", method: "Release"},
		{name: "codex native resume", fixture: "codex-0.151.0-session-start-resume.json", event: "session-start", method: "BindAndRestoreResume"},
		{name: "codex exec", fixture: "codex-0.151.0-pre-tool-use-exec.json", event: "pre-tool-use", method: "WaitReady"},
	}
	for index, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join("testdata", test.fixture))
			if err != nil {
				t.Fatal(err)
			}
			var recorded map[string]any
			if err := json.Unmarshal(data, &recorded); err != nil {
				t.Fatalf("invalid recorded payload: %v", err)
			}
			if recorded["hook_event_name"] == "" || recorded["session_id"] == "" {
				t.Fatalf("recorded payload lacks lifecycle identity: %s", data)
			}
			if test.fixture == "codex-0.151.0-pre-tool-use-exec.json" {
				if _, exists := recorded["workdir"]; exists {
					t.Fatal("Codex 0.151.0 fixture unexpectedly exposes exec workdir")
				}
				if recorded["cwd"] != "/tmp/wx-session/root" {
					t.Fatalf("Codex exec fixture cwd=%v", recorded["cwd"])
				}
			}
			handler.mu.Lock()
			handler.methods = nil
			handler.mu.Unlock()
			t.Setenv("WX_SESSION_ID", "wx-recorded-"+string(rune('a'+index)))
			if err := RunHook(ctx, test.event, strings.NewReader(string(data))); err != nil {
				t.Fatal(err)
			}
			handler.mu.Lock()
			methods := append([]string(nil), handler.methods...)
			handler.mu.Unlock()
			if len(methods) != 1 || methods[0] != test.method {
				t.Fatalf("recorded payload routed to %v, want [%s]", methods, test.method)
			}
		})
	}
}
