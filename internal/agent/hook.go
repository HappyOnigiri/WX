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
	CWD       string `json:"cwd"`
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
	recoveryDiscarded, err := modeFlag("WX_RECOVERY_DISCARDED")
	if err != nil {
		return err
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
	// hook の失敗は agent 操作を止めるため、binary 置換後の再起動中も接続を再試行する。
	// 空の DB では最短 22ms だが、launchd の遅延、migration、復旧 job、root descriptor を考慮して予算は 2 秒とする。
	client := rpc.Client{Socket: socket, Timeout: 3 * time.Second, ConnectRetry: 2 * time.Second}
	switch event {
	case "session-start":
		if payload.SessionID == "" {
			return errors.New("hook payload does not contain session_id")
		}
		var response struct {
			PreviousWorktree string `json:"previous_worktree"`
		}
		if err := client.CallWithKey(ctx, "BindAgentSession", "bind:"+wxID+":"+payload.SessionID, map[string]any{"session_id": wxID, "token": token, "agent_session_id": payload.SessionID, "source": payload.Source}, &response); err != nil {
			return err
		}
		if response.PreviousWorktree != "" && payload.Source == "resume" {
			previous := strings.NewReplacer("\n", " ", "\r", " ").Replace(response.PreviousWorktree)
			cwd := strings.NewReplacer("\n", " ", "\r", " ").Replace(payload.CWD)
			_, _ = fmt.Fprintf(os.Stdout, "wx notice: this conversation previously ran in %s; the workspace is now %s. Do not use the old path.\n", previous, cwd)
		}
		if recoveryDiscarded {
			writeRecoveryDiscardedNotice()
		}
		return nil
	case "user-prompt-submit", "pre-tool-use":
		timeout := 10 * time.Minute
		if raw := os.Getenv("WX_READINESS_TIMEOUT"); raw != "" {
			d, err := time.ParseDuration(raw)
			if err != nil || d <= 0 {
				return errors.New("invalid WX_READINESS_TIMEOUT")
			}
			timeout = d
		}
		waitCtx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		return client.Call(waitCtx, "WaitReady", map[string]any{"session_id": wxID, "token": token, "timeout_ms": int(timeout.Milliseconds())}, nil)
	case "session-end":
		releaseCtx, cancel := context.WithTimeout(ctx, 2500*time.Millisecond)
		defer cancel()
		return client.CallWithKey(releaseCtx, "Release", "release:"+wxID+":session-end-hook", map[string]any{"session_id": wxID, "token": token, "reason": "session-end-hook"}, nil)
	default:
		return fmt.Errorf("unknown hook event %q", event)
	}
}

func modeFlag(name string) (bool, error) {
	value := os.Getenv(name)
	switch value {
	case "":
		return false, nil
	case "1":
		return true, nil
	default:
		return false, fmt.Errorf("invalid %s hook mode flag", name)
	}
}

func writeRecoveryDiscardedNotice() {
	_, _ = fmt.Fprintln(os.Stdout, "wx recovery notice: this conversation is running from the current base branch; prior uncommitted local workspace state was not restored.")
}
