package rpc

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

type echoHandler struct{}

func (echoHandler) Handle(_ context.Context, method string, raw json.RawMessage) (any, error) {
	return map[string]any{"method": method, "size": len(raw)}, nil
}

type countingHandler struct{ calls atomic.Int32 }

func (h *countingHandler) Handle(_ context.Context, _ string, _ json.RawMessage) (any, error) {
	return map[string]int32{"call": h.calls.Add(1)}, nil
}

func TestClientServerRoundTripWithoutParentDeadline(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "wxd.sock")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	server := &Server{Socket: socket, Handler: echoHandler{}}
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx) }()
	deadline := time.Now().Add(time.Second)
	for {
		if time.Now().After(deadline) {
			t.Fatal("socket was not created")
		}
		var out map[string]any
		err := (Client{Socket: socket, Timeout: time.Second}).Call(context.Background(), "Echo", map[string]string{"x": "y"}, &out)
		if err == nil {
			if out["method"] != "Echo" {
				t.Fatalf("result=%v", out)
			}
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("server did not stop")
	}
}

func TestServerRefusesNonSocket(t *testing.T) {
	path := filepath.Join(t.TempDir(), "wxd.sock")
	if err := os.WriteFile(path, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	s := Server{Socket: path, Handler: echoHandler{}}
	if err := s.Serve(context.Background()); err == nil {
		t.Fatal("Serve replaced regular file")
	}
}

func TestServerRefusesToUnlinkLiveSocket(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "wxd.sock")
	ctx, cancel := context.WithCancel(context.Background())
	first := &Server{Socket: socket, Handler: echoHandler{}}
	done := make(chan error, 1)
	go func() { done <- first.Serve(ctx) }()
	deadline := time.Now().Add(time.Second)
	for {
		if _, err := os.Lstat(socket); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("first server did not create socket")
		}
		time.Sleep(10 * time.Millisecond)
	}
	second := &Server{Socket: socket, Handler: echoHandler{}}
	if err := second.Serve(context.Background()); err == nil {
		t.Fatal("second server replaced a live socket")
	}
	var output map[string]any
	if err := (Client{Socket: socket, Timeout: time.Second}).Call(context.Background(), "still-live", nil, &output); err != nil {
		t.Fatalf("first server stopped serving: %v", err)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestIdempotencyKeyReplaysResponseWithoutRepeatingHandler(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "wxd.sock")
	ctx, cancel := context.WithCancel(context.Background())
	handler := &countingHandler{}
	server := &Server{Socket: socket, Handler: handler}
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx) }()
	client := Client{Socket: socket, Timeout: time.Second}
	deadline := time.Now().Add(time.Second)
	for {
		var ignored map[string]int32
		if err := client.Call(context.Background(), "probe", nil, &ignored); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("server did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	var first, second map[string]int32
	if err := client.CallWithKey(context.Background(), "mutate", "same-key", map[string]int{"value": 1}, &first); err != nil {
		t.Fatal(err)
	}
	if err := client.CallWithKey(context.Background(), "mutate", "same-key", map[string]int{"value": 1}, &second); err != nil {
		t.Fatal(err)
	}
	if first["call"] != second["call"] || handler.calls.Load() != 2 { // one probe and one mutation
		t.Fatalf("responses first=%v second=%v calls=%d", first, second, handler.calls.Load())
	}
	if err := client.CallWithKey(context.Background(), "mutate", "same-key", map[string]int{"value": 2}, nil); err == nil {
		t.Fatal("idempotency key reuse with different payload succeeded")
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
