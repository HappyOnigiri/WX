package agent

import (
	"context"
	"encoding/json"
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
}
