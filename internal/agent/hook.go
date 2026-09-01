package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/HappyOnigiri/WX/internal/rpc"
)

type HookInput struct {
	SessionID string `json:"session_id"`
	Source    string `json:"source"`
}

func RunHook(ctx context.Context, event string, input io.Reader) error {
	wxID := os.Getenv("WX_SESSION_ID")
	token := os.Getenv("WX_SESSION_TOKEN")
	socket := os.Getenv("WX_DAEMON_SOCKET")
	if wxID == "" && token == "" {
		return nil
	}
	if wxID == "" || token == "" || socket == "" {
		return errors.New("incomplete wx hook environment")
	}
	var payload HookInput
	if event == "session-start" {
		data, err := io.ReadAll(io.LimitReader(input, 1<<20))
		if err != nil {
			return err
		}
		if len(strings.TrimSpace(string(data))) > 0 {
			if err := json.Unmarshal(data, &payload); err != nil {
				return fmt.Errorf("decode hook payload: %w", err)
			}
		}
	}
	client := rpc.Client{Socket: socket, Timeout: 3 * time.Second}
	switch event {
	case "session-start":
		if payload.SessionID == "" {
			return errors.New("hook payload does not contain session_id")
		}
		if os.Getenv("WX_FRESH") == "1" {
			return nil
		}
		method := "BindAgentSession"
		if os.Getenv("WX_NATIVE_RESUME") == "1" || payload.Source == "resume" {
			method = "BindAndRestoreResume"
		}
		return client.Call(ctx, method, map[string]any{"session_id": wxID, "token": token, "agent_session_id": payload.SessionID}, nil)
	case "user-prompt-submit", "pre-tool-use":
		timeout := 10 * time.Minute
		if raw := os.Getenv("WX_READINESS_TIMEOUT"); raw != "" {
			if d, err := time.ParseDuration(raw); err == nil {
				timeout = d
			}
		}
		waitCtx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		return client.Call(waitCtx, "WaitReady", map[string]any{"session_id": wxID, "token": token, "timeout_ms": int(timeout.Milliseconds())}, nil)
	case "session-end":
		releaseCtx, cancel := context.WithTimeout(ctx, 2500*time.Millisecond)
		defer cancel()
		return client.Call(releaseCtx, "Release", map[string]any{"session_id": wxID, "token": token, "reason": "session-end-hook"}, nil)
	default:
		return fmt.Errorf("unknown hook event %q", event)
	}
}
