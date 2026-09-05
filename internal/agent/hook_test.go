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
	mu       sync.Mutex
	methods  []string
	params   []json.RawMessage
	response any
}

type failingHookReader struct{}

func (failingHookReader) Read([]byte) (int, error) { return 0, errors.New("read fault") }

func (h *recordingHandler) Handle(_ context.Context, method string, params json.RawMessage) (any, error) {
	h.mu.Lock()
	h.methods = append(h.methods, method)
	h.params = append(h.params, append(json.RawMessage(nil), params...))
	response := h.response
	h.mu.Unlock()
	if response != nil {
		return response, nil
	}
	return map[string]bool{"ok": true}, nil
}

// paramsFor は method の最初の記録済み呼び出しの request body を返す。未呼び出しなら nil。
func (h *recordingHandler) paramsFor(method string) json.RawMessage {
	h.mu.Lock()
	defer h.mu.Unlock()
	for i, m := range h.methods {
		if m == method {
			return h.params[i]
		}
	}
	return nil
}

func (h *recordingHandler) methodsSnapshot() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.methods...)
}

func shortHookSocketPath(t *testing.T) string {
	t.Helper()
	directory, err := os.MkdirTemp("/tmp", "wx-agent-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	return filepath.Join(directory, "wxd.sock")
}

func startHookServer(t *testing.T, handler rpc.Handler) context.Context {
	t.Helper()
	socket := shortHookSocketPath(t)
	ctx, cancel := context.WithCancel(context.Background())
	server := &rpc.Server{Socket: socket, Handler: handler}
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx) }()
	for deadline := time.Now().Add(time.Second); ; {
		if _, err := os.Lstat(socket); err == nil {
			break
		}
		if time.Now().After(deadline) {
			cancel()
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
	return ctx
}

func clearHookEnvironment(t *testing.T) {
	t.Helper()
	for _, key := range []string{
		"WX_SESSION_ID",
		"WX_SESSION_TOKEN",
		"WX_DAEMON_SOCKET",
		"WX_READINESS_TIMEOUT",
		"WX_RECOVERY_DISCARDED",
	} {
		t.Setenv(key, "")
	}
}

func TestHookLifecyclePayloadsAndReadinessGates(t *testing.T) {
	clearHookEnvironment(t)
	handler := &recordingHandler{}
	ctx := startHookServer(t, handler)
	t.Setenv("WX_SESSION_TOKEN", "token")
	t.Setenv("WX_READINESS_TIMEOUT", "2s")
	t.Setenv("WX_SESSION_ID", "wx-normal")
	if err := RunHook(ctx, "session-start", strings.NewReader(`{"session_id":"agent-normal","source":"startup","cwd":"/workspace"}`)); err != nil {
		t.Fatal(err)
	}
	for _, event := range []string{"user-prompt-submit", "pre-tool-use"} {
		if err := RunHook(ctx, event, strings.NewReader("")); err != nil {
			t.Fatalf("%s: %v", event, err)
		}
	}
	if err := RunHook(ctx, "session-end", strings.NewReader("")); err != nil {
		t.Fatal(err)
	}

	wantMethods := "BindAgentSession,WaitReady,WaitReady,Release"
	if got := strings.Join(handler.methodsSnapshot(), ","); got != wantMethods {
		t.Fatalf("methods=%s, want %s", got, wantMethods)
	}
	if params := handler.paramsFor("BindAgentSession"); !strings.Contains(string(params), `"agent_session_id":"agent-normal"`) || !strings.Contains(string(params), `"source":"startup"`) {
		t.Fatalf("BindAgentSession params=%s", params)
	}
	if params := handler.paramsFor("WaitReady"); !strings.Contains(string(params), `"timeout_ms":2000`) {
		t.Fatalf("WaitReady params=%s", params)
	}
	if params := handler.paramsFor("Release"); !strings.Contains(string(params), `"reason":"session-end-hook"`) {
		t.Fatalf("Release params=%s", params)
	}
}

func TestHookFailsClosedForMalformedEnvironmentAndPayload(t *testing.T) {
	clearHookEnvironment(t)
	if err := RunHook(context.Background(), "unknown", strings.NewReader("")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WX_SESSION_ID", "wx")
	if err := RunHook(context.Background(), "session-start", strings.NewReader("")); err == nil || !strings.Contains(err.Error(), "incomplete wx hook environment") {
		t.Fatalf("incomplete environment error=%v", err)
	}
	t.Setenv("WX_SESSION_TOKEN", "token")
	t.Setenv("WX_DAEMON_SOCKET", filepath.Join(t.TempDir(), "missing.sock"))
	if err := RunHook(context.Background(), "session-start", strings.NewReader("{")); err == nil || !strings.Contains(err.Error(), "decode hook payload") {
		t.Fatalf("malformed payload error=%v", err)
	}
	if err := RunHook(context.Background(), "session-start", strings.NewReader(`{}`)); err == nil || !strings.Contains(err.Error(), "does not contain session_id") {
		t.Fatalf("missing session ID error=%v", err)
	}
	if err := RunHook(context.Background(), "session-start", failingHookReader{}); err == nil || !strings.Contains(err.Error(), "read fault") {
		t.Fatalf("input read error=%v", err)
	}
	if err := RunHook(context.Background(), "session-start", strings.NewReader(`{"session_id":"agent"}`)); err == nil {
		t.Fatal("binding through missing daemon socket succeeded")
	}
	if err := RunHook(context.Background(), "unknown", strings.NewReader("")); err == nil || !strings.Contains(err.Error(), `unknown hook event "unknown"`) {
		t.Fatalf("unknown event error=%v", err)
	}
	t.Setenv("WX_RECOVERY_DISCARDED", "true")
	if err := RunHook(context.Background(), "unknown", strings.NewReader("")); err == nil || !strings.Contains(err.Error(), "invalid WX_RECOVERY_DISCARDED hook mode flag") {
		t.Fatalf("invalid recovery flag error=%v", err)
	}
}

func TestRecoveryDiscardedModeFlag(t *testing.T) {
	t.Setenv("WX_RECOVERY_DISCARDED", "")
	got, err := modeFlag("WX_RECOVERY_DISCARDED")
	if err != nil || got {
		t.Fatalf("empty recovery flag=(%v, %v)", got, err)
	}
	t.Setenv("WX_RECOVERY_DISCARDED", "1")
	got, err = modeFlag("WX_RECOVERY_DISCARDED")
	if err != nil || !got {
		t.Fatalf("enabled recovery flag=(%v, %v)", got, err)
	}
}

func TestRecordedClaudeAndCodexHookPayloads(t *testing.T) {
	clearHookEnvironment(t)
	handler := &recordingHandler{}
	ctx := startHookServer(t, handler)
	t.Setenv("WX_SESSION_TOKEN", "token")
	t.Setenv("WX_READINESS_TIMEOUT", "2s")

	tests := []struct {
		name, fixture, event, method string
	}{
		{name: "claude startup", fixture: "claude-2.1.258-session-start-startup.json", event: "session-start", method: "BindAgentSession"},
		{name: "claude resume", fixture: "claude-2.1.258-session-start-resume.json", event: "session-start", method: "BindAgentSession"},
		{name: "claude prompt", fixture: "claude-2.1.258-user-prompt-submit.json", event: "user-prompt-submit", method: "WaitReady"},
		{name: "claude tool", fixture: "claude-2.1.258-pre-tool-use.json", event: "pre-tool-use", method: "WaitReady"},
		{name: "claude end", fixture: "claude-2.1.258-session-end.json", event: "session-end", method: "Release"},
		{name: "codex resume", fixture: "codex-0.151.0-session-start-resume.json", event: "session-start", method: "BindAgentSession"},
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
			handler.params = nil
			handler.mu.Unlock()
			t.Setenv("WX_SESSION_ID", "wx-recorded-"+string(rune('a'+index)))
			if err := RunHook(ctx, test.event, strings.NewReader(string(data))); err != nil {
				t.Fatal(err)
			}
			methods := handler.methodsSnapshot()
			if len(methods) != 1 || methods[0] != test.method {
				t.Fatalf("recorded payload routed to %v, want [%s]", methods, test.method)
			}
			if test.event == "session-start" {
				params := handler.paramsFor("BindAgentSession")
				if !strings.Contains(string(params), `"source":"`+recorded["source"].(string)+`"`) {
					t.Fatalf("source was not forwarded: %s", params)
				}
			}
		})
	}
}

func TestHookRejectsInvalidReadinessTimeouts(t *testing.T) {
	clearHookEnvironment(t)
	t.Setenv("WX_SESSION_ID", "wx")
	t.Setenv("WX_SESSION_TOKEN", "token")
	t.Setenv("WX_DAEMON_SOCKET", filepath.Join(t.TempDir(), "missing.sock"))
	for _, timeout := range []string{"not-a-duration", "0s", "-1s"} {
		t.Run("invalid readiness timeout "+timeout, func(t *testing.T) {
			t.Setenv("WX_READINESS_TIMEOUT", timeout)
			err := RunHook(context.Background(), "user-prompt-submit", strings.NewReader(""))
			if err == nil || !strings.Contains(err.Error(), "invalid WX_READINESS_TIMEOUT") {
				t.Fatalf("invalid readiness timeout %q was accepted: %v", timeout, err)
			}
		})
	}
}

func TestReadinessHookSurvivesADaemonThatIsStillRestarting(t *testing.T) {
	clearHookEnvironment(t)
	socket := shortHookSocketPath(t)
	handler := &recordingHandler{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	server := &rpc.Server{Socket: socket, Handler: handler}
	done := make(chan error, 1)
	go func() {
		// hook 開始時は未待受で、kickstart -k から置換 process の最初の accept までの隙間に当たる。
		time.Sleep(200 * time.Millisecond)
		done <- server.Serve(ctx)
	}()
	t.Setenv("WX_DAEMON_SOCKET", socket)
	t.Setenv("WX_SESSION_TOKEN", "token")
	t.Setenv("WX_SESSION_ID", "wx-restarting")
	t.Setenv("WX_READINESS_TIMEOUT", "5s")
	if err := RunHook(ctx, "pre-tool-use", strings.NewReader("")); err != nil {
		t.Fatalf("pre-tool-use hook failed across a daemon restart: %v", err)
	}
	methods := strings.Join(handler.methodsSnapshot(), ",")
	if !strings.Contains(methods, "WaitReady") {
		t.Fatalf("methods=%s, want WaitReady", methods)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
