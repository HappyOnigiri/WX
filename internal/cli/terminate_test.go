package cli

import (
	"context"
	"encoding/json"
	"os/exec"
	"testing"
	"time"

	"github.com/HappyOnigiri/WX/internal/daemon"
	"github.com/HappyOnigiri/WX/internal/rpc"
)

func TestAgentTerminatorSignalsOnceAndOnlyAfterAdoption(t *testing.T) {
	terminator := &agentTerminator{}
	terminator.request(nil)
	terminator.request(&terminationRequest{})
	if terminator.requested() {
		t.Fatal("an empty request was treated as a termination request")
	}
	// process 登録前に届いた要求は記録だけされ、signal は登録後に一度だけ送る。
	terminator.request(&terminationRequest{RequestID: "first", Deadline: "2026-01-01T00:00:00Z"})
	if !terminator.requested() {
		t.Fatal("request before adoption was dropped")
	}
	if terminator.signaled {
		t.Fatal("a signal was sent before the agent process existed")
	}
	cmd := exec.Command("sleep", "30")
	cmd.SysProcAttr = nil
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	terminator.adopt(cmd)
	if !terminator.signaled {
		t.Fatal("adoption did not deliver the pending termination request")
	}
	terminator.request(&terminationRequest{RequestID: "second"})
	terminator.mu.Lock()
	requestID := terminator.requestID
	terminator.mu.Unlock()
	if requestID != "first" {
		t.Fatalf("a later request replaced the first one: %s", requestID)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the agent process did not stop after SIGTERM")
	}
}

func TestTerminationEnvelopeTreatsMissingFieldAsNoRequest(t *testing.T) {
	var reply terminationEnvelope
	if err := json.Unmarshal([]byte(`{"ok":true}`), &reply); err != nil {
		t.Fatal(err)
	}
	if reply.Terminate != nil {
		t.Fatalf("terminate=%+v", reply.Terminate)
	}
	if err := json.Unmarshal([]byte(`{"ok":true,"terminate":{"request_id":"abc","deadline":"2026-01-01T00:00:00Z"}}`), &reply); err != nil {
		t.Fatal(err)
	}
	if reply.Terminate == nil || reply.Terminate.RequestID != "abc" {
		t.Fatalf("terminate=%+v", reply.Terminate)
	}
}

func TestConfirmSkipsWhenNoTerminationWasRequested(t *testing.T) {
	// socket を持たない client でも、要求が無ければ RPC を送らないので失敗しない。
	client := Client{RPC: rpc.Client{Socket: "/nonexistent/wx.sock", Timeout: 10 * time.Millisecond}}
	(&agentTerminator{}).confirm(client, daemon.Lease{SessionID: "session", Token: "token"})
}

func TestWatchTerminationStopsWhenTheSessionEnds(t *testing.T) {
	client := Client{RPC: rpc.Client{Socket: "/nonexistent/wx.sock", Timeout: 10 * time.Millisecond}}
	done := make(chan struct{})
	stopped := make(chan struct{})
	go func() {
		client.watchTermination(context.Background(), daemon.Lease{SessionID: "session"}, &agentTerminator{}, done)
		close(stopped)
	}()
	close(done)
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("the heartbeat watcher outlived the session")
	}
}
