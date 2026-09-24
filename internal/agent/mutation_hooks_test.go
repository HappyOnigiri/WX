package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/HappyOnigiri/WorktreeX/internal/rpc"
	"github.com/HappyOnigiri/WorktreeX/internal/testsupport"
)

func TestMutationHookClientKeepsIndependentTimeoutBudgets(t *testing.T) {
	client := newHookClient("socket")
	if client.Timeout != 3*time.Second {
		t.Fatalf("hook client timeout=%s, want 3s", client.Timeout)
	}
	if client.ConnectRetry != 2*time.Second {
		t.Fatalf("hook client connect retry=%s, want 2s", client.ConnectRetry)
	}
}

// Codex が同じ session_id と source を再利用しても、fork 親が違えば
// transcript metadata の矛盾として通常 bind へ戻す。
func TestMutationCodexForkParentRejectsConflictingSessionMetadata(t *testing.T) {
	path := filepath.Join(t.TempDir(), "conflicting.jsonl")
	data := `{"type":"session_meta","payload":{"id":"child","session_id":"different","forked_from_id":"parent"}}
`
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := codexForkParent(path, "child"); got != "" {
		t.Fatalf("conflicting transcript metadata returned parent %q", got)
	}
}

// delayedHookHandler は daemon の応答を遅らせ、hook の既定 readiness timeout が
// 0 へ変化した場合も呼び出し側がエラーとして観測できるようにする。
type delayedHookHandler struct{ delay time.Duration }

func (h delayedHookHandler) Handle(ctx context.Context, _ string, _ json.RawMessage) (any, error) {
	select {
	case <-time.After(h.delay):
		return map[string]bool{"ok": true}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func TestMutationHookDefaultReadinessTimeoutRemainsPositive(t *testing.T) {
	clearHookEnvironment(t)
	ctx := startHookServer(t, delayedHookHandler{delay: 20 * time.Millisecond})
	t.Setenv("WX_SESSION_ID", "wx-timeout")
	t.Setenv("WX_SESSION_TOKEN", "token")
	if err := RunHook(ctx, "user-prompt-submit", strings.NewReader("")); err != nil {
		t.Fatalf("default readiness timeout rejected a delayed response: %v", err)
	}
}

func TestMutationHookReadinessRetryBridgesDaemonRestart(t *testing.T) {
	clearHookEnvironment(t)
	socket := testsupport.SocketPath(t, "delayed-wxd.sock")
	serverContext, cancel := context.WithCancel(context.Background())
	defer cancel()
	server := &rpc.Server{Socket: socket, Handler: &recordingHandler{}}
	serverErr := make(chan error, 1)
	go func() {
		time.Sleep(250 * time.Millisecond)
		serverErr <- server.Serve(serverContext)
	}()
	t.Setenv("WX_DAEMON_SOCKET", socket)
	t.Setenv("WX_SESSION_ID", "wx-retry")
	t.Setenv("WX_SESSION_TOKEN", "token")
	t.Setenv("WX_READINESS_TIMEOUT", "5s")
	if err := RunHook(context.Background(), "pre-tool-use", strings.NewReader("")); err != nil {
		t.Fatalf("hook did not bridge the daemon restart gap: %v", err)
	}
	cancel()
	if err := <-serverErr; err != nil {
		t.Fatal(err)
	}
}

func TestMutationHookForkParentChangesBindIdempotencyKey(t *testing.T) {
	clearHookEnvironment(t)
	handler := &recordingHandler{}
	ctx := startHookServer(t, handler)
	t.Setenv("WX_SESSION_ID", "wx-fork-key")
	t.Setenv("WX_SESSION_TOKEN", "token")
	t.Setenv("WX_READINESS_TIMEOUT", "1s")
	transcriptDir := t.TempDir()
	for index, parent := range []string{"parent-one", "parent-two"} {
		path := filepath.Join(transcriptDir, parent+".jsonl")
		data := `{"type":"session_meta","payload":{"id":"child","forked_from_id":"` + parent + `"}}
`
		if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
		payload := `{"session_id":"child","source":"compact","transcript_path":"` + path + `"}`
		if err := RunHook(ctx, "session-start", strings.NewReader(payload)); err != nil {
			t.Fatalf("fork parent %q (case %d) reused the bind key: %v", parent, index, err)
		}
	}
}

func TestMutationHookNoticeOnlyBelongsToUserPrompt(t *testing.T) {
	clearHookEnvironment(t)
	handler := &recordingHandler{}
	ctx := startHookServer(t, handler)
	t.Setenv("WX_SESSION_ID", "wx-notice")
	t.Setenv("WX_SESSION_TOKEN", "token")
	t.Setenv("WX_READINESS_TIMEOUT", "1s")
	if err := RunHook(ctx, "user-prompt-submit", strings.NewReader("")); err != nil {
		t.Fatal(err)
	}
	var promptParams map[string]any
	if err := json.Unmarshal(handler.paramsFor("WaitReady"), &promptParams); err != nil {
		t.Fatal(err)
	}
	if notice, ok := promptParams["notice"].(bool); !ok || !notice {
		t.Fatalf("user prompt notice=%v, want true", promptParams["notice"])
	}

	handler.mu.Lock()
	handler.methods = nil
	handler.params = nil
	handler.mu.Unlock()
	if err := RunHook(ctx, "pre-tool-use", strings.NewReader("")); err != nil {
		t.Fatal(err)
	}
	var toolParams map[string]any
	if err := json.Unmarshal(handler.paramsFor("WaitReady"), &toolParams); err != nil {
		t.Fatal(err)
	}
	if _, exists := toolParams["notice"]; exists {
		t.Fatalf("pre-tool-use unexpectedly requested notice: %v", toolParams["notice"])
	}
}
