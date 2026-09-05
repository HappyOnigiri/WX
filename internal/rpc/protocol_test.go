package rpc

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestIdempotentCallStopsRetryingWhenContextIsCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	client := Client{Socket: shortSocketPath(t, "missing.sock"), Timeout: time.Second}
	if err := client.CallWithKey(ctx, "mutate", "stable-key", map[string]int{"value": 1}, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled idempotent call error=%v", err)
	}
	if !transientTransportError(io.EOF) || !transientTransportError(io.ErrUnexpectedEOF) || transientTransportError(errors.New("permanent")) {
		t.Fatal("transient transport error classification is inconsistent")
	}
}

func TestIdempotentCallStopsRetryingWhenContextExpiresDuringBackoff(t *testing.T) {
	// missing socket への dial はすぐ *net.OpError となり、最初の 25ms backoff と短い deadline が競合する。
	// 5ms の予算なら backoff より十分短い。
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
	defer cancel()
	client := Client{Socket: shortSocketPath(t, "missing.sock"), Timeout: time.Second}
	if err := client.CallWithKey(ctx, "mutate", "stable-key", map[string]int{"value": 1}, nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("idempotent call error during backoff=%v", err)
	}
}

type echoHandler struct{}

func shortSocketPath(t *testing.T, name string) string {
	t.Helper()
	directory, err := os.MkdirTemp("", "wx-rpc-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	return filepath.Join(directory, name)
}

func (echoHandler) Handle(_ context.Context, method string, raw json.RawMessage) (any, error) {
	return map[string]any{"method": method, "size": len(raw)}, nil
}

type countingHandler struct{ calls atomic.Int32 }

func (h *countingHandler) Handle(_ context.Context, _ string, _ json.RawMessage) (any, error) {
	return map[string]int32{"call": h.calls.Add(1)}, nil
}

type memoryDurableStore struct {
	mu      sync.Mutex
	records map[string]durableRecord
}

type durableRecord struct {
	method, params, code, message, state string
	result                               []byte
}

type failingDurableStore struct{ failBegin bool }

type interruptedDurableStore struct {
	memoryDurableStore
	failComplete atomic.Bool
}

func (s failingDurableStore) BeginRPCRequest(context.Context, string, string, string, time.Time) ([]byte, string, string, bool, error) {
	if s.failBegin {
		return nil, "", "", false, errors.New("begin fault")
	}
	return nil, "", "", true, nil
}

func (s failingDurableStore) CompleteRPCRequest(context.Context, string, string, string, []byte, string, string, time.Time) error {
	if !s.failBegin {
		return errors.New("complete fault")
	}
	return nil
}

func (s *memoryDurableStore) BeginRPCRequest(_ context.Context, key, method, params string, _ time.Time) ([]byte, string, string, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.records == nil {
		s.records = map[string]durableRecord{}
	}
	record, ok := s.records[key]
	if !ok {
		s.records[key] = durableRecord{method: method, params: params, state: "PENDING"}
		return nil, "", "", true, nil
	}
	if record.method != method || record.params != params {
		return nil, "IDEMPOTENCY_KEY_REUSE", "mismatch", false, nil
	}
	if record.state == "PENDING" {
		return nil, "IDEMPOTENCY_INDETERMINATE", "pending", false, nil
	}
	return append([]byte(nil), record.result...), record.code, record.message, false, nil
}

func (s *memoryDurableStore) CompleteRPCRequest(_ context.Context, key, method, params string, result []byte, code, message string, _ time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	record, ok := s.records[key]
	if !ok || record.method != method || record.params != params || record.state != "PENDING" {
		return errors.New("reservation is not pending")
	}
	s.records[key] = durableRecord{method: method, params: params, result: append([]byte(nil), result...), code: code, message: message, state: "COMPLETED"}
	return nil
}

func (s *interruptedDurableStore) CompleteRPCRequest(ctx context.Context, key, method, params string, result []byte, code, message string, expires time.Time) error {
	if s.failComplete.Swap(false) {
		return errors.New("simulated crash before durable response commit")
	}
	return s.memoryDurableStore.CompleteRPCRequest(ctx, key, method, params, result, code, message, expires)
}

type errorHandler struct{ result any }

func (h errorHandler) Handle(_ context.Context, method string, _ json.RawMessage) (any, error) {
	if method == "error" {
		return nil, errors.New("handler failed")
	}
	return h.result, nil
}

type countingErrorHandler struct{ calls atomic.Int32 }

func (h *countingErrorHandler) Handle(_ context.Context, _ string, _ json.RawMessage) (any, error) {
	h.calls.Add(1)
	return nil, errors.New("handler failed")
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

type blockingRPCHandler struct{}

func (blockingRPCHandler) Handle(ctx context.Context, _ string, _ json.RawMessage) (any, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

type delayedRPCHandler struct{ delay time.Duration }

func (h delayedRPCHandler) Handle(ctx context.Context, _ string, _ json.RawMessage) (any, error) {
	timer := time.NewTimer(h.delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return map[string]string{"status": "completed"}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func TestClientBoundsConnectedPeerReadWithDefaultTimeout(t *testing.T) {
	socket := shortSocketPath(t, "unresponsive.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		connection, acceptErr := listener.Accept()
		if acceptErr == nil {
			accepted <- connection
		}
	}()
	started := time.Now()
	err = (Client{Socket: socket, Timeout: 40 * time.Millisecond}).Call(context.Background(), "blocked", nil, nil)
	if err == nil {
		t.Fatal("unresponsive peer call succeeded")
	}
	if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
		t.Fatalf("connected peer was not bounded: %v", elapsed)
	}
	select {
	case connection := <-accepted:
		_ = connection.Close()
	case <-time.After(time.Second):
		t.Fatal("client did not connect to test peer")
	}
}

func TestClientCancellationClosesConnectedPeer(t *testing.T) {
	socket := shortSocketPath(t, "cancel.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		connection, acceptErr := listener.Accept()
		if acceptErr == nil {
			accepted <- connection
		}
	}()
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { result <- (Client{Socket: socket, Timeout: time.Second}).Call(ctx, "blocked", nil, nil) }()
	var connection net.Conn
	select {
	case connection = <-accepted:
	case <-time.After(time.Second):
		t.Fatal("client did not connect to test peer")
	}
	started := time.Now()
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled call error=%v", err)
		}
		if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
			t.Fatalf("cancellation did not close connected peer: %v", elapsed)
		}
	case <-time.After(time.Second):
		t.Fatal("canceled connected call remained blocked")
	}
	_ = connection.Close()
}

func TestServerBoundsUnframedConnection(t *testing.T) {
	serverSide, clientSide := net.Pipe()
	server := &Server{FrameTimeout: 30 * time.Millisecond, Handler: echoHandler{}}
	done := make(chan struct{})
	go func() {
		server.serveConn(context.Background(), serverSide)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("server retained an unframed connection past its deadline")
	}
	_ = clientSide.Close()
}

func TestServerBoundsHandlerWithIndependentDeadline(t *testing.T) {
	serverSide, clientSide := net.Pipe()
	server := &Server{HandlerTimeout: 30 * time.Millisecond, Handler: blockingRPCHandler{}}
	done := make(chan struct{})
	go func() {
		server.serveConn(context.Background(), serverSide)
		close(done)
	}()
	if err := writeFrame(clientSide, Request{Version: ProtocolVersion, ID: "request", Method: "blocked", Params: json.RawMessage(`{}`)}); err != nil {
		t.Fatal(err)
	}
	var response Response
	if err := readFrame(clientSide, &response); err != nil {
		t.Fatal(err)
	}
	if response.Error == nil || response.Error.Code != "REQUEST_FAILED" {
		t.Fatalf("bounded handler response=%+v", response)
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("server handler connection did not finish")
	}
	_ = clientSide.Close()
}

func TestServerHonorsExplicitDeadlineForLongHandler(t *testing.T) {
	serverSide, clientSide := net.Pipe()
	server := &Server{HandlerTimeout: 30 * time.Millisecond, Handler: delayedRPCHandler{delay: 70 * time.Millisecond}}
	done := make(chan struct{})
	go func() {
		server.serveConn(context.Background(), serverSide)
		close(done)
	}()
	request := Request{
		Version:  ProtocolVersion,
		ID:       "request",
		Method:   "delayed",
		Deadline: time.Now().Add(250 * time.Millisecond).UTC().Format(time.RFC3339Nano),
		Params:   json.RawMessage(`{}`),
	}
	if err := writeFrame(clientSide, request); err != nil {
		t.Fatal(err)
	}
	var response Response
	if err := readFrame(clientSide, &response); err != nil {
		t.Fatal(err)
	}
	if response.Error != nil || string(response.Result) != `{"status":"completed"}` {
		t.Fatalf("explicit long-handler response=%+v", response)
	}
	_ = clientSide.Close()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("server did not finish explicit long-handler request")
	}
}

func TestServerRejectsInvalidRequestDeadline(t *testing.T) {
	serverSide, clientSide := net.Pipe()
	server := &Server{FrameTimeout: 100 * time.Millisecond, Handler: echoHandler{}}
	done := make(chan struct{})
	go func() {
		server.serveConn(context.Background(), serverSide)
		close(done)
	}()
	if err := writeFrame(clientSide, Request{Version: ProtocolVersion, ID: "request", Method: "echo", Deadline: "not-a-timestamp", Params: json.RawMessage(`{}`)}); err != nil {
		t.Fatal(err)
	}
	var response Response
	if err := readFrame(clientSide, &response); err != nil {
		t.Fatal(err)
	}
	if response.Error == nil || response.Error.Code != "INVALID_DEADLINE" {
		t.Fatalf("invalid deadline response=%+v", response)
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("server did not finish after rejecting invalid deadline")
	}
	_ = clientSide.Close()
}

func TestServerBoundsResponseWriteToUnresponsivePeer(t *testing.T) {
	serverSide, clientSide := net.Pipe()
	server := &Server{HandlerTimeout: 30 * time.Millisecond, Handler: echoHandler{}}
	done := make(chan struct{})
	go func() {
		server.serveConn(context.Background(), serverSide)
		close(done)
	}()
	if err := writeFrame(clientSide, Request{Version: ProtocolVersion, ID: "request", Method: "echo", Params: json.RawMessage(`{}`)}); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	select {
	case <-done:
		if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
			t.Fatalf("response write was not bounded: %v", elapsed)
		}
	case <-time.After(time.Second):
		t.Fatal("server retained an unresponsive response peer")
	}
	_ = clientSide.Close()
}

func TestClientServerRoundTripWithoutParentDeadline(t *testing.T) {
	socket := shortSocketPath(t, "wxd.sock")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	server := &Server{Socket: socket, Handler: echoHandler{}}
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx) }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if time.Now().After(deadline) {
			t.Fatal("socket was not created")
		}
		info, err := os.Lstat(socket)
		if err == nil && info.Mode()&os.ModeSocket != 0 {
			break
		}
		select {
		case err := <-done:
			t.Fatalf("server stopped before creating socket: %v", err)
		default:
		}
		time.Sleep(10 * time.Millisecond)
	}
	var out map[string]any
	if err := (Client{Socket: socket, Timeout: time.Second}).Call(context.Background(), "Echo", map[string]string{"x": "y"}, &out); err != nil {
		t.Fatal(err)
	}
	if out["method"] != "Echo" {
		t.Fatalf("result=%v", out)
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

func TestDurableIdempotencySurvivesServerRestart(t *testing.T) {
	durable := &memoryDurableStore{}
	handler := &countingHandler{}
	call := func(socket string) map[string]int32 {
		ctx, cancel := context.WithCancel(context.Background())
		server := &Server{Socket: socket, Handler: handler, Durable: durable}
		done := make(chan error, 1)
		go func() { done <- server.Serve(ctx) }()
		client := Client{Socket: socket, Timeout: time.Second}
		deadline := time.Now().Add(5 * time.Second)
		var result map[string]int32
		for {
			if err := client.CallWithKey(context.Background(), "mutate", "stable-key", map[string]string{"value": "same"}, &result); err == nil {
				break
			} else if time.Now().After(deadline) {
				t.Fatal(err)
			}
			time.Sleep(10 * time.Millisecond)
		}
		cancel()
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		return result
	}
	first := call(shortSocketPath(t, "first.sock"))
	second := call(shortSocketPath(t, "second.sock"))
	if first["call"] != 1 || second["call"] != 1 || handler.calls.Load() != 1 {
		t.Fatalf("first=%v second=%v handler_calls=%d", first, second, handler.calls.Load())
	}
}

func TestDurableIdempotencyStorageFailuresFailClosed(t *testing.T) {
	for _, test := range []struct {
		name     string
		durable  DurableIdempotency
		wantCode string
	}{
		{name: "begin", durable: failingDurableStore{failBegin: true}, wantCode: "IDEMPOTENCY_STORE"},
		{name: "complete", durable: failingDurableStore{}, wantCode: "IDEMPOTENCY_STORE"},
	} {
		t.Run(test.name, func(t *testing.T) {
			serverSide, clientSide := net.Pipe()
			server := &Server{Handler: echoHandler{}, Durable: test.durable}
			done := make(chan struct{})
			go func() {
				server.serveConn(context.Background(), serverSide)
				close(done)
			}()
			request := Request{Version: ProtocolVersion, ID: "request", Method: "mutate", IdempotencyKey: "key", Params: json.RawMessage(`{}`)}
			if err := writeFrame(clientSide, request); err != nil {
				t.Fatal(err)
			}
			var response Response
			if err := readFrame(clientSide, &response); err != nil {
				t.Fatal(err)
			}
			if response.Error == nil || response.Error.Code != test.wantCode || len(response.Result) != 0 {
				t.Fatalf("response=%+v", response)
			}
			_ = clientSide.Close()
			<-done
		})
	}
}

func TestDurableReservationPreventsMutationReplayAfterResponseCommitGap(t *testing.T) {
	durable := &interruptedDurableStore{}
	durable.failComplete.Store(true)
	handler := &countingHandler{}
	call := func() Response {
		serverSide, clientSide := net.Pipe()
		server := &Server{Handler: handler, Durable: durable}
		done := make(chan struct{})
		go func() {
			server.serveConn(context.Background(), serverSide)
			close(done)
		}()
		request := Request{Version: ProtocolVersion, ID: "request", Method: "mutate", IdempotencyKey: "crash-gap", Params: json.RawMessage(`{"token":"secret"}`)}
		if err := writeFrame(clientSide, request); err != nil {
			t.Fatal(err)
		}
		var response Response
		if err := readFrame(clientSide, &response); err != nil {
			t.Fatal(err)
		}
		_ = clientSide.Close()
		<-done
		return response
	}
	first := call()
	if first.Error == nil || first.Error.Code != "IDEMPOTENCY_STORE" || handler.calls.Load() != 1 {
		t.Fatalf("first response=%+v calls=%d", first, handler.calls.Load())
	}
	second := call()
	if second.Error == nil || second.Error.Code != "IDEMPOTENCY_INDETERMINATE" || handler.calls.Load() != 1 {
		t.Fatalf("second response=%+v calls=%d", second, handler.calls.Load())
	}
}

func TestServerRefusesNonSocket(t *testing.T) {
	path := shortSocketPath(t, "wxd.sock")
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
	listener, err := net.Listen("unix", shortSocketPath(t, "close.sock"))
	if err != nil {
		t.Fatal(err)
	}
	if err := (&Server{listener: listener}).Close(); err != nil {
		t.Fatal(err)
	}
}

func TestServerReplacesOnlyAStaleUnixSocket(t *testing.T) {
	socket := shortSocketPath(t, "stale.sock")
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: socket, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	listener.SetUnlinkOnClose(false)
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	server := &Server{Socket: socket, Handler: echoHandler{}}
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx) }()
	client := Client{Socket: socket, Timeout: time.Second}
	deadline := time.Now().Add(time.Second)
	for {
		var output map[string]any
		if err := client.Call(context.Background(), "ready", nil, &output); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("server did not replace stale socket")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestServeReturnsAcceptErrorWhenClosedWithoutContextCancellation(t *testing.T) {
	socket := shortSocketPath(t, "closed-without-cancel.sock")
	ctx := context.Background()
	server := &Server{Socket: socket, Handler: echoHandler{}}
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx) }()
	client := Client{Socket: socket, Timeout: time.Second}
	deadline := time.Now().Add(time.Second)
	for {
		var output map[string]any
		if err := client.Call(context.Background(), "ready", nil, &output); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("server did not start listening")
		}
		time.Sleep(10 * time.Millisecond)
	}
	// ctx を中断せず listener を直接閉じ、ctx が有効なまま Accept が失敗する経路を確認する。
	// Serve は通常の shutdown と扱わず、Accept error を返さなければならない。
	if err := server.Close(); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err == nil {
		t.Fatal("Serve returned nil after an externally closed listener")
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
		{name: "missing response", params: map[string]int{"value": 1}},
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
			socket := shortSocketPath(t, "server.sock")
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

func TestServerPropagatesSocketSetupAndAddressFailures(t *testing.T) {
	blockingParent := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(blockingParent, []byte("file"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := (&Server{Socket: filepath.Join(blockingParent, "wxd.sock"), Handler: echoHandler{}}).Serve(context.Background()); err == nil {
		t.Fatal("server created a socket beneath a regular file")
	}

	directory, err := os.MkdirTemp("/tmp", "wx-rpc-long-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	tooLong := filepath.Join(directory, strings.Repeat("s", 180))
	if err := (&Server{Socket: tooLong, Handler: echoHandler{}}).Serve(context.Background()); err == nil {
		t.Fatal("server accepted a Unix socket path beyond platform limits")
	}
}

func TestDurableIdempotencyPersistsHandlerErrors(t *testing.T) {
	durable := &memoryDurableStore{}
	handler := &countingErrorHandler{}
	call := func() Response {
		server := &Server{Handler: handler, Durable: durable}
		serverSide, clientSide := net.Pipe()
		done := make(chan struct{})
		go func() {
			server.serveConn(context.Background(), serverSide)
			close(done)
		}()
		request := Request{Version: ProtocolVersion, ID: "request", Method: "error", IdempotencyKey: "error-key", Params: json.RawMessage(`{}`)}
		if err := writeFrame(clientSide, request); err != nil {
			t.Fatal(err)
		}
		var response Response
		if err := readFrame(clientSide, &response); err != nil {
			t.Fatal(err)
		}
		_ = clientSide.Close()
		<-done
		return response
	}
	for range 2 {
		response := call()
		if response.Error == nil || response.Error.Code != "REQUEST_FAILED" {
			t.Fatalf("durable error response=%+v", response)
		}
	}
	if calls := handler.calls.Load(); calls != 1 {
		t.Fatalf("durable replay invoked handler %d times, want 1", calls)
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
	client := Client{Socket: shortSocketPath(t, "missing.sock"), Timeout: time.Millisecond}
	err := client.Call(context.Background(), "missing", nil, nil)
	if err == nil {
		t.Fatal("missing server call succeeded")
	}
	if !IsConnectError(err) {
		t.Fatalf("a missing socket was not classified as a connect error: %v", err)
	}
}

type alwaysFailingHandler struct{}

func (alwaysFailingHandler) Handle(context.Context, string, json.RawMessage) (any, error) {
	return nil, errors.New("handler intentionally failed")
}

func TestIsConnectErrorRejectsFailuresAfterConnectionEstablished(t *testing.T) {
	socket := shortSocketPath(t, "connected-failure.sock")
	server := &Server{Socket: socket, Handler: alwaysFailingHandler{}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx) }()
	deadline := time.Now().Add(time.Second)
	for {
		if _, err := os.Lstat(socket); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("server did not create socket")
		}
		time.Sleep(5 * time.Millisecond)
	}
	client := Client{Socket: socket, Timeout: time.Second}
	// handler の application error でも接続自体は成功しており、daemon は応答している。
	// cli.Client.ensureDaemon が -k の安全性を判定する際、未待受と誤認してはならない。
	err := client.Call(ctx, "any-method", struct{}{}, nil)
	if err == nil {
		t.Fatal("call against an always-failing handler unexpectedly succeeded")
	}
	if IsConnectError(err) {
		t.Fatalf("a response from a live server was classified as a connect error: %v", err)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestServerRefusesToUnlinkLiveSocket(t *testing.T) {
	socket := shortSocketPath(t, "wxd.sock")
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
	socket := shortSocketPath(t, "wxd.sock")
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

func TestIdempotencyCacheIsBoundedAndExpiresCompletedEntries(t *testing.T) {
	server := &Server{idem: map[string]*idempotentEntry{}}
	now := time.Now()
	for i := range maxIdempotency {
		server.idem[fmt.Sprintf("key-%d", i)] = &idempotentEntry{done: closedChannel(), ended: now.Add(-time.Duration(i) * time.Second)}
	}
	server.idem["expired"] = &idempotentEntry{done: closedChannel(), ended: now.Add(-idempotencyTTL)}
	server.pruneIdempotencyLocked(now)
	if len(server.idem) >= maxIdempotency {
		t.Fatalf("idempotency cache was not bounded: %d", len(server.idem))
	}
	if _, ok := server.idem["expired"]; ok {
		t.Fatal("expired idempotency entry was retained")
	}
	entry, owner := server.idempotencyEntry(Request{Method: "test", IdempotencyKey: "replacement", Params: json.RawMessage(`{}`)})
	if entry == nil || !owner {
		t.Fatal("bounded cache did not accept a replacement entry")
	}
}

func TestIdempotencyWaitHonorsDeadlineAndRejectsInflightOverflow(t *testing.T) {
	server := &Server{Handler: echoHandler{}, idem: map[string]*idempotentEntry{}}
	params := json.RawMessage(`{"value":1}`)
	server.idem["waiting"] = &idempotentEntry{method: "mutate", params: string(params), done: make(chan struct{})}
	serverSide, clientSide := net.Pipe()
	done := make(chan struct{})
	go func() {
		server.serveConn(context.Background(), serverSide)
		close(done)
	}()
	request := Request{Version: ProtocolVersion, ID: "request", Method: "mutate", IdempotencyKey: "waiting", Params: params, Deadline: time.Now().Add(10 * time.Millisecond).UTC().Format(time.RFC3339Nano)}
	if err := writeFrame(clientSide, request); err != nil {
		t.Fatal(err)
	}
	var response Response
	if err := readFrame(clientSide, &response); err != nil {
		t.Fatal(err)
	}
	if response.Error == nil || response.Error.Code != "DEADLINE" {
		t.Fatalf("deadline response=%+v", response)
	}
	_ = clientSide.Close()
	<-done

	server.idem = map[string]*idempotentEntry{}
	for i := range maxIdempotency {
		server.idem[fmt.Sprintf("inflight-%d", i)] = &idempotentEntry{done: make(chan struct{})}
	}
	if entry, _ := server.idempotencyEntry(Request{Method: "overflow", IdempotencyKey: "overflow"}); entry != nil {
		t.Fatal("in-flight idempotency cache exceeded its bound")
	}
}

func closedChannel() chan struct{} {
	done := make(chan struct{})
	close(done)
	return done
}
