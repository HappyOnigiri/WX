package rpc

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func FuzzReadFrame(f *testing.F) {
	f.Add([]byte{0, 0, 0, 2, '{', '}'})
	f.Add([]byte{0, 0, 0, 0})
	f.Fuzz(func(t *testing.T, data []byte) {
		var value any
		_ = readFrame(bytes.NewReader(data), &value)
	})
}

type echoHandler struct{}

func (echoHandler) Handle(_ context.Context, method string, raw json.RawMessage) (any, error) {
	return map[string]any{"method": method, "size": len(raw)}, nil
}

type countingHandler struct{ calls atomic.Int32 }

func (h *countingHandler) Handle(_ context.Context, _ string, _ json.RawMessage) (any, error) {
	return map[string]int32{"call": h.calls.Add(1)}, nil
}

type errorHandler struct{ result any }

func (h errorHandler) Handle(_ context.Context, method string, _ json.RawMessage) (any, error) {
	if method == "error" {
		return nil, errors.New("handler failed")
	}
	return h.result, nil
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

type headerOnlyWriter struct{ writes int }

func (w *headerOnlyWriter) Write(p []byte) (int, error) {
	w.writes++
	if w.writes > 1 {
		return 0, io.ErrClosedPipe
	}
	return len(p), nil
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
	if err := os.WriteFile(path, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := Server{Socket: path, Handler: echoHandler{}}
	if err := s.Serve(context.Background()); err == nil {
		t.Fatal("Serve replaced regular file")
	}
}

func TestServerCloseWithoutListenerIsSafe(t *testing.T) {
	if err := (&Server{}).Close(); err != nil {
		t.Fatal(err)
	}
}

func TestFrameValidationAndWriteFailures(t *testing.T) {
	for _, data := range [][]byte{
		nil,
		{0, 0, 0, 0},
		{0, 128, 0, 1},
		{0, 0, 0, 2, '{', 'x'},
		{0, 0, 0, 4, '{'},
	} {
		var value any
		if err := readFrame(bytes.NewReader(data), &value); err == nil {
			t.Errorf("readFrame(%v) succeeded", data)
		}
	}
	if err := writeFrame(io.Discard, strings.Repeat("x", maxFrame+1)); err == nil {
		t.Fatal("oversized frame succeeded")
	}
	if err := writeFrame(failingWriter{}, map[string]bool{"ok": true}); err == nil {
		t.Fatal("frame write failure succeeded")
	}
	if err := writeFrame(&headerOnlyWriter{}, map[string]bool{"ok": true}); err == nil {
		t.Fatal("frame body write failure succeeded")
	}
	if err := writeFrame(io.Discard, make(chan int)); err == nil {
		t.Fatal("unencodable frame succeeded")
	}
}

func TestClientValidatesEveryResponseBoundary(t *testing.T) {
	tests := []struct {
		name   string
		params any
		reply  func(Request) any
		result any
	}{
		{name: "request marshal", params: make(chan int)},
		{name: "protocol", reply: func(req Request) any { return Response{Version: ProtocolVersion + 1, ID: req.ID} }},
		{name: "id", reply: func(Request) any { return Response{Version: ProtocolVersion, ID: "different"} }},
		{name: "rpc error", reply: func(req Request) any {
			return Response{Version: ProtocolVersion, ID: req.ID, Error: &RPCError{Code: "TEST", Message: "failed"}}
		}},
		{name: "invalid result", result: &map[string]any{}, reply: func(req Request) any {
			return Response{Version: ProtocolVersion, ID: req.ID, Result: json.RawMessage("{")}
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			socket := filepath.Join(t.TempDir(), "server.sock")
			listener, err := net.Listen("unix", socket)
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			done := make(chan struct{})
			go func() {
				defer close(done)
				connection, acceptErr := listener.Accept()
				if acceptErr != nil {
					return
				}
				defer connection.Close()
				var request Request
				if readFrame(bufio.NewReader(connection), &request) != nil || test.reply == nil {
					return
				}
				_ = writeFrame(connection, test.reply(request))
			}()
			err = (Client{Socket: socket}).Call(context.Background(), "test", test.params, test.result)
			if err == nil {
				t.Fatal("invalid boundary response succeeded")
			}
			_ = listener.Close()
			<-done
		})
	}
}

func TestInvokeReportsHandlerAndEncodingErrors(t *testing.T) {
	server := &Server{Handler: errorHandler{result: make(chan int)}}
	if _, rpcErr := server.invoke(context.Background(), Request{Method: "error"}); rpcErr == nil || rpcErr.Code != "REQUEST_FAILED" {
		t.Fatalf("handler RPC error=%+v", rpcErr)
	}
	if _, rpcErr := server.invoke(context.Background(), Request{Method: "encode"}); rpcErr == nil || rpcErr.Code != "ENCODE_FAILED" {
		t.Fatalf("encoding RPC error=%+v", rpcErr)
	}
}

func TestServeConnRejectsProtocolVersion(t *testing.T) {
	serverSide, clientSide := net.Pipe()
	server := &Server{Handler: echoHandler{}}
	done := make(chan struct{})
	go func() {
		server.serveConn(context.Background(), serverSide)
		close(done)
	}()
	if err := writeFrame(clientSide, Request{Version: ProtocolVersion + 1, ID: "request"}); err != nil {
		t.Fatal(err)
	}
	var response Response
	if err := readFrame(clientSide, &response); err != nil {
		t.Fatal(err)
	}
	if response.Error == nil || response.Error.Code != "PROTOCOL_VERSION" {
		t.Fatalf("response=%+v", response)
	}
	_ = clientSide.Close()
	<-done
}

func TestClientReportsMissingSocket(t *testing.T) {
	client := Client{Socket: filepath.Join(t.TempDir(), "missing.sock"), Timeout: time.Millisecond}
	if err := client.Call(context.Background(), "missing", nil, nil); err == nil {
		t.Fatal("missing server call succeeded")
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
