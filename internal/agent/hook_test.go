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
	params  []json.RawMessage
}

type failingHookReader struct{}

func (failingHookReader) Read([]byte) (int, error) { return 0, errors.New("read fault") }

func (h *recordingHandler) Handle(_ context.Context, method string, params json.RawMessage) (any, error) {
	h.mu.Lock()
	h.methods = append(h.methods, method)
	h.params = append(h.params, append(json.RawMessage(nil), params...))
	h.mu.Unlock()
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

func shortHookSocketPath(t *testing.T) string {
	t.Helper()
	directory, err := os.MkdirTemp("/tmp", "wx-agent-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	return filepath.Join(directory, "wxd.sock")
}

func TestHookLifecyclePayloadsAndReadinessGates(t *testing.T) {
	for _, key := range []string{"WX_NATIVE_RESUME", "WX_EXPLICIT_RESUME", "WX_FRESH", "WX_RECOVERY_DISCARDED", "WX_BRANCHES_JSON"} {
		t.Setenv(key, "")
	}
	socket := shortHookSocketPath(t)
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
	t.Setenv("WX_NATIVE_RESUME", "1")
	t.Setenv("WX_FRESH", "1")
	if err := RunHook(ctx, "session-start", strings.NewReader(`{"session_id":"agent-fresh","source":"resume"}`)); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WX_FRESH", "")
	t.Setenv("WX_NATIVE_RESUME", "")
	t.Setenv("WX_EXPLICIT_RESUME", "1")
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
	if params := handler.paramsFor("BindAgentSession"); !strings.Contains(string(params), `"source":"startup"`) {
		t.Fatalf("BindAgentSession did not forward the hook payload source: %s", params)
	}
	if params := handler.paramsFor("BindAndRestoreResume"); !strings.Contains(string(params), `"source":"resume"`) {
		t.Fatalf("BindAndRestoreResume did not forward the hook payload source: %s", params)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestHookFailsClosedForMalformedEnvironmentAndPayload(t *testing.T) {
	for _, key := range []string{"WX_NATIVE_RESUME", "WX_EXPLICIT_RESUME", "WX_FRESH", "WX_RECOVERY_DISCARDED", "WX_BRANCHES_JSON"} {
		t.Setenv(key, "")
	}
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
	t.Setenv("WX_NATIVE_RESUME", "1")
	t.Setenv("WX_BRANCHES_JSON", "{")
	if err := RunHook(context.Background(), "session-start", strings.NewReader(`{"session_id":"agent","source":"resume"}`)); err == nil || !strings.Contains(err.Error(), "branch selection") {
		t.Fatalf("invalid fresh branches error=%v", err)
	}
	t.Setenv("WX_BRANCHES_JSON", `[]`)
	if err := RunHook(context.Background(), "session-start", strings.NewReader(`{"session_id":"agent","source":"resume"}`)); err == nil {
		t.Fatal("fresh validation through missing daemon socket succeeded")
	}
}

func TestRecordedClaudeAndCodexHookPayloads(t *testing.T) {
	for _, key := range []string{"WX_NATIVE_RESUME", "WX_EXPLICIT_RESUME", "WX_FRESH", "WX_RECOVERY_DISCARDED", "WX_BRANCHES_JSON"} {
		t.Setenv(key, "")
	}
	socket := shortHookSocketPath(t)
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
			t.Setenv("WX_NATIVE_RESUME", "")
			t.Setenv("WX_EXPLICIT_RESUME", "")
			if test.method == "BindAndRestoreResume" {
				t.Setenv("WX_NATIVE_RESUME", "1")
			}
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

func TestHookRejectsContradictoryInvocationModes(t *testing.T) {
	t.Setenv("WX_SESSION_ID", "wx")
	t.Setenv("WX_SESSION_TOKEN", "token")
	t.Setenv("WX_DAEMON_SOCKET", filepath.Join(t.TempDir(), "missing.sock"))
	t.Setenv("WX_NATIVE_RESUME", "1")
	t.Setenv("WX_EXPLICIT_RESUME", "1")
	if err := RunHook(context.Background(), "session-start", strings.NewReader(`{"session_id":"agent","source":"resume"}`)); err == nil || !strings.Contains(err.Error(), "contradictory") {
		t.Fatalf("contradictory mode flags were accepted: %v", err)
	}
	t.Setenv("WX_NATIVE_RESUME", "")
	t.Setenv("WX_EXPLICIT_RESUME", "")
	t.Setenv("WX_FRESH", "1")
	if err := RunHook(context.Background(), "session-start", strings.NewReader(`{"session_id":"agent","source":"resume"}`)); err == nil || !strings.Contains(err.Error(), "contradictory") {
		t.Fatalf("fresh mode without native flag was accepted: %v", err)
	}
	for _, key := range []string{"WX_FRESH", "WX_NATIVE_RESUME", "WX_EXPLICIT_RESUME"} {
		t.Setenv(key, "")
	}
	if err := RunHook(context.Background(), "session-start", strings.NewReader(`{"session_id":"agent","source":"resume"}`)); err == nil || !strings.Contains(err.Error(), "no native") {
		t.Fatalf("resume payload without mode was accepted: %v", err)
	}
}

func TestHookRejectsInvalidModesAndReadinessTimeouts(t *testing.T) {
	t.Setenv("WX_SESSION_ID", "wx")
	t.Setenv("WX_SESSION_TOKEN", "token")
	t.Setenv("WX_DAEMON_SOCKET", filepath.Join(t.TempDir(), "missing.sock"))
	for _, key := range []string{"WX_NATIVE_RESUME", "WX_EXPLICIT_RESUME", "WX_FRESH", "WX_RECOVERY_DISCARDED"} {
		t.Setenv(key, "")
	}

	for _, key := range []string{"WX_NATIVE_RESUME", "WX_EXPLICIT_RESUME", "WX_FRESH", "WX_RECOVERY_DISCARDED"} {
		t.Run("invalid "+key, func(t *testing.T) {
			t.Setenv(key, "true")
			if _, err := readHookModes(); err == nil || !strings.Contains(err.Error(), "invalid "+key) {
				t.Fatalf("invalid mode %s was accepted: %v", key, err)
			}
			t.Setenv(key, "")
		})
	}

	for _, timeout := range []string{"not-a-duration", "0s", "-1s"} {
		t.Run("invalid readiness timeout "+timeout, func(t *testing.T) {
			t.Setenv("WX_READINESS_TIMEOUT", timeout)
			err := RunHook(context.Background(), "user-prompt-submit", strings.NewReader(""))
			if err == nil || !strings.Contains(err.Error(), "invalid WX_READINESS_TIMEOUT") {
				t.Fatalf("invalid readiness timeout %q was accepted: %v", timeout, err)
			}
			t.Setenv("WX_READINESS_TIMEOUT", "")
		})
	}

	t.Setenv("WX_NATIVE_RESUME", "1")
	if err := RunHook(context.Background(), "session-start", strings.NewReader(`{"session_id":"agent","source":"startup"}`)); err == nil || !strings.Contains(err.Error(), "does not identify a resume") {
		t.Fatalf("native hook accepted a non-resume payload: %v", err)
	}
}

func TestReadinessHookSurvivesADaemonThatIsStillRestarting(t *testing.T) {
	for _, key := range []string{"WX_NATIVE_RESUME", "WX_EXPLICIT_RESUME", "WX_FRESH", "WX_RECOVERY_DISCARDED", "WX_BRANCHES_JSON"} {
		t.Setenv(key, "")
	}
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
	handler.mu.Lock()
	methods := strings.Join(handler.methods, ",")
	handler.mu.Unlock()
	if !strings.Contains(methods, "WaitReady") {
		t.Fatalf("methods=%s, want WaitReady", methods)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
