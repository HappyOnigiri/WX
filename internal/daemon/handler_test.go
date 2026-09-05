package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/HappyOnigiri/WX/internal/config"
	"github.com/HappyOnigiri/WX/internal/state"
)

func TestHandlerRejectsUnknownFieldsForEveryParameterizedMethod(t *testing.T) {
	t.Parallel()
	handler := Handler{}
	methods := []string{
		"ResolveAndLease", "AllocateResumeSlot", "WaitReady", "BindAgentSession", "BindAndRestoreResume",
		"ValidateFreshResume", "Release", "Heartbeat", "RegisterAgentProcess", "Resume", "ResumeStatus", "GC", "Sessions", "Forget",
	}
	for _, method := range methods {
		if _, err := handler.Handle(context.Background(), method, json.RawMessage(`{"unexpected":true}`)); err == nil {
			t.Errorf("%s accepted unknown field", method)
		}
	}
	if _, err := handler.Handle(context.Background(), "Unknown", nil); err == nil {
		t.Fatal("unknown RPC method succeeded")
	}
	var value struct {
		Known bool `json:"known"`
	}
	if err := decode(nil, &value); err != nil {
		t.Fatal(err)
	}
}

func TestHandlerRoutesResumeAndFreshOperationsToManager(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	store, err := state.Open(filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	cfg := config.Defaults()
	cfg.Storage.WorktreeRoot = filepath.Join(root, "worktrees")
	manager := testManager(t, cfg, store)
	t.Cleanup(manager.Close)
	handler := Handler{Manager: manager}
	requests := map[string]string{
		"BindAndRestoreResume": `{"session_id":"missing","token":"token","agent_session_id":"agent"}`,
		"ValidateFreshResume":  `{"session_id":"missing","token":"token","agent_session_id":"agent","cwd":"/missing","branches":[]}`,
		"Resume":               `{"wx_session_id":"missing","agent":"codex","client_pid":1,"allow_fresh":false}`,
		"ResumeStatus":         `{"wx_session_id":"missing"}`,
		"Forget":               `{"path":"/missing"}`,
	}
	for method, raw := range requests {
		if _, err := handler.Handle(context.Background(), method, json.RawMessage(raw)); err == nil {
			t.Errorf("%s unexpectedly succeeded", method)
		}
	}
}

func TestBindAndRestoreResumeRejectsNonResumeSource(t *testing.T) {
	t.Parallel()
	handler := Handler{}
	for _, source := range []string{"start", "fresh", "resume-ish", ""} {
		raw := json.RawMessage(`{"session_id":"missing","token":"token","agent_session_id":"agent","source":"` + source + `"}`)
		if _, err := handler.Handle(context.Background(), "BindAndRestoreResume", raw); err == nil {
			t.Fatalf("source=%q was accepted for the restore-resume path", source)
		}
	}
	raw := json.RawMessage(`{"session_id":"missing","token":"token","agent_session_id":"agent"}`)
	if _, err := handler.Handle(context.Background(), "BindAndRestoreResume", raw); err == nil {
		t.Fatal("an omitted source was accepted for the restore-resume path")
	}
}

func TestDegradedHandlerAllowsOnlyReadOnlyDiagnostics(t *testing.T) {
	t.Parallel()
	handler := DegradedHandler{DatabasePath: "/state.db", OpenError: errors.New("corrupt")}
	for _, method := range []string{"Status", "Doctor"} {
		if result, err := handler.Handle(context.Background(), method, nil); err != nil || result == nil {
			t.Fatalf("%s result=%v err=%v", method, result, err)
		}
	}
	if _, err := handler.Handle(context.Background(), "GC", nil); err == nil {
		t.Fatal("degraded mutating method succeeded")
	}
}

func TestDegradedHandlerGuidanceDependsOnOpenError(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		openError  error
		want       []string
		wantAbsent []string
	}{
		{
			name: "previous worktree layout",
			openError: fmt.Errorf(
				"%w: /state.db was created by a wx release that used the previous worktree layout and cannot be migrated; stop the daemon, remove that file, and remove the old worktree root once no session needs it",
				state.ErrPreviousWorktreeLayout,
			),
			want:       []string{"previous worktree layout", "stop the daemon", "remove that file", "remove the old worktree root"},
			wantAbsent: []string{"state.db.backups", "wx doctor"},
		},
		{
			name:      "other open failure",
			openError: errors.New("corrupt"),
			want:      []string{"corrupt", "/state.db.backups", "wx doctor"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			handler := DegradedHandler{DatabasePath: "/state.db", OpenError: test.openError}
			for _, method := range []string{"Status", "Doctor"} {
				result, err := handler.Handle(context.Background(), method, nil)
				if err != nil {
					t.Fatalf("%s err=%v", method, err)
				}
				message := degradedDiagnosticMessage(t, method, result)
				assertGuidance(t, method, message, test.want, test.wantAbsent)
			}
			_, err := handler.Handle(context.Background(), "GC", nil)
			if err == nil {
				t.Fatal("default degraded method unexpectedly succeeded")
			}
			assertGuidance(t, "default", err.Error(), test.want, test.wantAbsent)
		})
	}
}

func degradedDiagnosticMessage(t *testing.T, method string, result any) string {
	t.Helper()
	payload, ok := result.(map[string]any)
	if !ok {
		t.Fatalf("%s result=%T %v, want object", method, result, result)
	}
	if method == "Status" {
		message, ok := payload["error"].(string)
		if !ok {
			t.Fatalf("status error=%T %v, want string", payload["error"], payload["error"])
		}
		return message
	}
	checks, ok := payload["checks"].(map[string]any)
	if !ok {
		t.Fatalf("doctor checks=%T %v, want object", payload["checks"], payload["checks"])
	}
	message, ok := checks["sqlite"].(string)
	if !ok {
		t.Fatalf("doctor sqlite=%T %v, want string", checks["sqlite"], checks["sqlite"])
	}
	return message
}

func assertGuidance(t *testing.T, method, message string, want, wantAbsent []string) {
	t.Helper()
	for _, fragment := range want {
		if !strings.Contains(message, fragment) {
			t.Errorf("%s message=%q, want %q", method, message, fragment)
		}
	}
	for _, fragment := range wantAbsent {
		if strings.Contains(message, fragment) {
			t.Errorf("%s message=%q, must not contain %q", method, message, fragment)
		}
	}
}

func TestDegradedHandlerStillHonoursAStop(t *testing.T) {
	t.Parallel()
	signalled := make(chan struct{})
	handler := DegradedHandler{
		DatabasePath: "/state.db",
		OpenError:    errors.New("corrupt"),
		terminate:    func() error { close(signalled); return nil },
	}
	result, err := handler.Handle(context.Background(), "RequestStop", nil)
	if err != nil {
		t.Fatalf("degraded stop err=%v", err)
	}
	reply, ok := result.(map[string]any)
	if !ok {
		t.Fatalf("degraded stop result=%v", result)
	}
	if pending, _ := reply["stop_pending"].(bool); !pending {
		t.Fatalf("degraded stop did not report the pending stop: %v", reply)
	}
	select {
	case <-signalled:
	case <-time.After(2 * time.Second):
		t.Fatal("the degraded stop never reached the signal")
	}
}

func TestHandlerRoutesAgentRegistrationAndConfigReload(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", root)
	store, err := state.Open(filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	cfg := config.Defaults()
	cfg.Storage.WorktreeRoot = filepath.Join(root, "worktrees")
	manager := testManager(t, cfg, store)
	t.Cleanup(manager.Close)
	if _, err := store.CreateSlotSession(context.Background(), testSlotRow(t, manager, "", "slot", 0, "LEASED"), nil, state.Session{ID: "session", SlotID: "slot", State: "ACTIVE", AgentKind: "codex", TokenHash: state.HashToken("token")}, ""); err != nil {
		t.Fatal(err)
	}
	handler := Handler{Manager: manager}
	registered, err := handler.Handle(context.Background(), "RegisterAgentProcess", json.RawMessage(`{"session_id":"session","token":"token","agent_pid":1}`))
	if err != nil {
		t.Fatal(err)
	}
	if result, ok := registered.(map[string]any); !ok || result["registered"] != true {
		t.Fatalf("registration result=%v", registered)
	}
	session, err := store.SessionByID(context.Background(), "session")
	if err != nil || session.AgentPID != 1 {
		t.Fatalf("registered agent PID=%d err=%v", session.AgentPID, err)
	}
	reloaded, err := handler.Handle(context.Background(), "ReloadConfig", nil)
	if err != nil {
		t.Fatal(err)
	}
	if result, ok := reloaded.(map[string]bool); !ok || !result["reloaded"] {
		t.Fatalf("reload result=%v", reloaded)
	}
	if got, want := manager.Config().Storage.WorktreeRoot, filepath.Join(root, "wx"); got != want {
		t.Fatalf("reloaded worktree root=%q, want %q", got, want)
	}
}
