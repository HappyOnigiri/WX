package daemon

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func JSON(v any) json.RawMessage { b, _ := json.Marshal(v); return b }

func TestHandlerPublicLifecycleSurface(t *testing.T) {
	t.Parallel()
	requireDaemonIntegration(t)
	f := runningManagerFixture(t, func(s *managerFixtureSetup) {
		s.Config.Pool.WarmPerWorkspace = 0
		s.Config.Discovery.ReconcileInterval.Duration = time.Hour
	})
	repository := filepath.Join(f.Root, "repo")
	initGitRepo(t, repository)
	handler := Handler{Manager: f.Manager}
	ctx := context.Background()
	result, err := handler.Handle(ctx, "ResolveAndLease", JSON(map[string]any{"force_worktree": true, "cwd": repository, "agent": "codex", "client_pid": os.Getpid()}))
	if err != nil {
		t.Fatal(err)
	}
	lease := result.(Lease)
	if _, err := handler.Handle(ctx, "WaitReady", JSON(map[string]any{"session_id": lease.SessionID, "token": lease.Token, "timeout_ms": 10000})); err != nil {
		t.Fatal(err)
	}
	for method, params := range map[string]any{
		"BindAgentSession": map[string]any{"session_id": lease.SessionID, "token": lease.Token, "agent_session_id": "agent-session"},
		"Heartbeat":        map[string]any{"session_id": lease.SessionID, "token": lease.Token},
		"ResumeStatus":     map[string]any{"wx_session_id": lease.SessionID},
		"GC":               map[string]any{"dry_run": true},
		"Slots":            map[string]any{"all": true},
	} {
		if _, err := handler.Handle(ctx, method, JSON(params)); err != nil {
			t.Fatalf("%s: %v", method, err)
		}
	}
	for _, method := range []string{"Status", "Doctor"} {
		if _, err := handler.Handle(ctx, method, nil); err != nil {
			t.Fatalf("%s: %v", method, err)
		}
	}
	if _, err := handler.Handle(ctx, "Release", JSON(map[string]any{"session_id": lease.SessionID, "token": lease.Token, "reason": "test"})); err != nil {
		t.Fatal(err)
	}
}
