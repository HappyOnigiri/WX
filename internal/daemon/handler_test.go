package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"

	"github.com/HappyOnigiri/WX/internal/config"
	"github.com/HappyOnigiri/WX/internal/state"
)

func TestHandlerRejectsUnknownFieldsForEveryParameterizedMethod(t *testing.T) {
	handler := Handler{}
	methods := []string{
		"ResolveAndLease", "AllocateResumeSlot", "WaitReady", "BindAgentSession", "BindAndRestoreResume",
		"ValidateFreshResume", "Release", "Heartbeat", "Resume", "ResumeStatus", "GC", "Sessions", "Forget",
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

func TestDegradedHandlerAllowsOnlyReadOnlyDiagnostics(t *testing.T) {
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
