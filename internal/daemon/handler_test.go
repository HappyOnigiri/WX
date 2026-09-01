package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
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
