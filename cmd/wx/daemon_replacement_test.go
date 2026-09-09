package main

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/HappyOnigiri/WX/internal/rpc"
	"github.com/HappyOnigiri/WX/internal/testsupport"
)

type replacementProbeHandler struct {
	pingPID     int
	statusPID   int
	pingErr     error
	pingCalls   atomic.Int64
	statusCalls atomic.Int64
}

func (h *replacementProbeHandler) Handle(_ context.Context, method string, _ json.RawMessage) (any, error) {
	switch method {
	case "Ping":
		h.pingCalls.Add(1)
		if h.pingErr != nil {
			return nil, h.pingErr
		}
		if h.pingPID == 0 {
			return map[string]any{"protocol_version": rpc.ProtocolVersion, "degraded": false}, nil
		}
		return map[string]any{"protocol_version": rpc.ProtocolVersion, "degraded": false, "pid": h.pingPID}, nil
	case "Status":
		h.statusCalls.Add(1)
		return map[string]any{"pid": h.statusPID}, nil
	default:
		return map[string]any{"ok": true}, nil
	}
}

func TestWaitForDaemonReplacementUsesPingPIDWithoutSocketOutage(t *testing.T) {
	socket := testsupport.SocketPath(t, "daemon.sock")
	handler := &replacementProbeHandler{pingPID: 2002, statusPID: 2002}
	cancel, done := serveUntilCanceled(t, socket, handler)
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}()
	restore := daemonWaitTimeout
	daemonWaitTimeout = time.Second
	t.Cleanup(func() { daemonWaitTimeout = restore })

	started := time.Now()
	if !waitForDaemonReplacement(context.Background(), socket, 1001) {
		t.Fatal("replacement was not detected from Ping")
	}
	if elapsed := time.Since(started); elapsed >= daemonWaitTimeout/2 {
		t.Fatalf("replacement detection waited %s for the first probe", elapsed)
	}
	if handler.pingCalls.Load() == 0 {
		t.Fatal("wait did not issue a Ping probe")
	}
	if handler.statusCalls.Load() != 0 {
		t.Fatalf("Status fallback was used for a PID-bearing Ping: %d calls", handler.statusCalls.Load())
	}
}

func TestWaitForDaemonReplacementFallsBackToStatusForLegacyPing(t *testing.T) {
	socket := testsupport.SocketPath(t, "daemon.sock")
	handler := &replacementProbeHandler{statusPID: 3003}
	cancel, done := serveUntilCanceled(t, socket, handler)
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}()
	previousCalls := handler.statusCalls.Load()
	if !waitForDaemonReplacement(context.Background(), socket, 1001) {
		t.Fatal("replacement was not detected from the Status fallback")
	}
	if handler.pingCalls.Load() == 0 || handler.statusCalls.Load() <= previousCalls {
		t.Fatalf("legacy Ping fallback calls: ping=%d status=%d", handler.pingCalls.Load(), handler.statusCalls.Load())
	}
}

func TestWaitForDaemonReplacementFallsBackToStatusWhenPingFails(t *testing.T) {
	socket := testsupport.SocketPath(t, "daemon.sock")
	handler := &replacementProbeHandler{pingErr: errors.New("unknown method"), statusPID: 4004}
	cancel, done := serveUntilCanceled(t, socket, handler)
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}()
	if !waitForDaemonReplacement(context.Background(), socket, 1001) {
		t.Fatal("replacement was not detected after Ping failed")
	}
	if handler.pingCalls.Load() == 0 || handler.statusCalls.Load() == 0 {
		t.Fatalf("failed Ping did not use Status fallback: ping=%d status=%d", handler.pingCalls.Load(), handler.statusCalls.Load())
	}
}

func TestWaitForDaemonReplacementStopsAtTheDeadlineForTheSamePID(t *testing.T) {
	socket := testsupport.SocketPath(t, "daemon.sock")
	handler := &replacementProbeHandler{pingPID: 5005, statusPID: 5005}
	cancel, done := serveUntilCanceled(t, socket, handler)
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}()
	restore := daemonWaitTimeout
	daemonWaitTimeout = 80 * time.Millisecond
	t.Cleanup(func() { daemonWaitTimeout = restore })
	if waitForDaemonReplacement(context.Background(), socket, 5005) {
		t.Fatal("same PID was reported as a replacement")
	}
}

func TestWaitForDaemonReplacementStopsWhenTheCallerCancels(t *testing.T) {
	socket := testsupport.SocketPath(t, "daemon.sock")
	handler := &replacementProbeHandler{pingPID: 6006}
	cancelServer, done := serveUntilCanceled(t, socket, handler)
	defer func() {
		cancelServer()
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if waitForDaemonReplacement(ctx, socket, 5005) {
		t.Fatal("a canceled caller reported a replacement")
	}
}
