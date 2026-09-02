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
	modes, err := readHookModes()
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
	client := rpc.Client{Socket: socket, Timeout: 3 * time.Second}
	switch event {
	case "session-start":
		if payload.SessionID == "" {
			return errors.New("hook payload does not contain session_id")
		}
		if modes.native && payload.Source != "resume" {
			return errors.New("native resume hook payload does not identify a resume source")
		}
		if modes.fresh {
			var branches []string
			if raw := os.Getenv("WX_BRANCHES_JSON"); raw != "" {
				if err := json.Unmarshal([]byte(raw), &branches); err != nil {
					return fmt.Errorf("decode fresh resume branch selection: %w", err)
				}
			}
			if err := client.CallWithKey(ctx, "ValidateFreshResume", "fresh:"+wxID+":"+payload.SessionID, map[string]any{"session_id": wxID, "token": token, "agent_session_id": payload.SessionID, "cwd": os.Getenv("WX_SOURCE_CWD"), "branches": branches}, nil); err != nil {
				return err
			}
			writeRecoveryDiscardedNotice()
			return nil
		}
		method := "BindAgentSession"
		if modes.native && !modes.explicit {
			method = "BindAndRestoreResume"
		} else if payload.Source == "resume" && !modes.explicit {
			return errors.New("resume hook payload has no native resume mode")
		}
		if err := client.CallWithKey(ctx, method, "bind:"+wxID+":"+payload.SessionID, map[string]any{"session_id": wxID, "token": token, "agent_session_id": payload.SessionID}, nil); err != nil {
			return err
		}
		if modes.recoveryDiscarded {
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

type hookModes struct {
	native            bool
	explicit          bool
	fresh             bool
	recoveryDiscarded bool
}

func readHookModes() (hookModes, error) {
	native, err := modeFlag("WX_NATIVE_RESUME")
	if err != nil {
		return hookModes{}, err
	}
	explicit, err := modeFlag("WX_EXPLICIT_RESUME")
	if err != nil {
		return hookModes{}, err
	}
	fresh, err := modeFlag("WX_FRESH")
	if err != nil {
		return hookModes{}, err
	}
	recoveryDiscarded, err := modeFlag("WX_RECOVERY_DISCARDED")
	if err != nil {
		return hookModes{}, err
	}
	if explicit && native || explicit && fresh || fresh && !native || recoveryDiscarded && !explicit || recoveryDiscarded && fresh {
		return hookModes{}, errors.New("contradictory wx hook mode flags")
	}
	return hookModes{native: native, explicit: explicit, fresh: fresh, recoveryDiscarded: recoveryDiscarded}, nil
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
