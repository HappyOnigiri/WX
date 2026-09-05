package rpc

import (
	"bufio"
	"context"
	"errors"
	"net"
	"sync"
	"testing"
	"time"
)

func TestIdempotentCallStopsRetryAfterConnectedPeerCloses(t *testing.T) {
	socket := shortSocketPath(t, "close-before-response.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	requestRead := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseNow := func() { releaseOnce.Do(func() { close(release) }) }
	serverErr := make(chan error, 1)
	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			serverErr <- acceptErr
			return
		}
		defer conn.Close()
		var request Request
		if err := readFrame(bufio.NewReader(conn), &request); err != nil {
			serverErr <- err
			return
		}
		if request.Method != "mutate" || request.IdempotencyKey != "stable-key" {
			serverErr <- errors.New("unexpected idempotent request")
			return
		}
		close(requestRead)
		<-release
		serverErr <- nil
	}()
	defer releaseNow()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() {
		result <- (Client{Socket: socket, Timeout: time.Second}).CallWithKey(ctx, "mutate", "stable-key", map[string]int{"value": 1}, nil)
	}()
	select {
	case <-requestRead:
	case err := <-serverErr:
		t.Fatalf("server failed before request read: %v", err)
	case <-time.After(time.Second):
		t.Fatal("server did not receive idempotent request")
	}
	cancel()
	releaseNow()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("connected peer cancellation error=%v", err)
	}
	if err := <-serverErr; err != nil {
		t.Fatalf("server request handling: %v", err)
	}
}

func TestRequestDeadlineClampsImplausibleClientRequestedDeadline(t *testing.T) {
	requested := time.Now().Add(365 * 24 * time.Hour).UTC().Format(time.RFC3339Nano)
	before := time.Now()
	deadline, err := (&Server{HandlerTimeout: time.Hour}).requestDeadline(context.Background(), requested)
	if err != nil {
		t.Fatal(err)
	}
	if max := before.Add(DefaultMaxHandlerTimeout + time.Second); deadline.After(max) {
		t.Fatalf("client-requested deadline was not clamped: got %v, want at most %v", deadline, max)
	}
	if deadline.Before(before.Add(DefaultMaxHandlerTimeout - time.Second)) {
		t.Fatalf("clamp was tighter than the documented ceiling: got %v", deadline)
	}
}

func TestRequestDeadlineHonorsExplicitMaxHandlerTimeoutOverride(t *testing.T) {
	requested := time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano)
	server := &Server{HandlerTimeout: time.Hour, MaxHandlerTimeout: time.Minute}
	before := time.Now()
	deadline, err := server.requestDeadline(context.Background(), requested)
	if err != nil {
		t.Fatal(err)
	}
	if max := before.Add(90 * time.Second); deadline.After(max) {
		t.Fatalf("configured MaxHandlerTimeout was not honored: got %v, want at most %v", deadline, max)
	}
}

func TestRequestDeadlineHonorsEarlierParentDeadline(t *testing.T) {
	parent, cancel := context.WithDeadline(context.Background(), time.Now().Add(50*time.Millisecond))
	defer cancel()
	requested := time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano)
	deadline, err := (&Server{HandlerTimeout: time.Hour}).requestDeadline(parent, requested)
	if err != nil {
		t.Fatal(err)
	}
	if parentDeadline, ok := parent.Deadline(); !ok || !deadline.Equal(parentDeadline) {
		t.Fatalf("request deadline=%v parent deadline=%v", deadline, parentDeadline)
	}
}

func TestConnectRetryBridgesADaemonThatIsNotListeningYet(t *testing.T) {
	socket := shortSocketPath(t, "connect-retry.sock")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	server := &Server{Socket: socket, Handler: echoHandler{}}
	serverErr := make(chan error, 1)
	go func() {
		// 最初の接続試行が失敗した後に待受を始める。
		// launchd が daemon を置換する間に生じる空白を再現する。
		time.Sleep(150 * time.Millisecond)
		serverErr <- server.Serve(ctx)
	}()
	var result map[string]any
	if err := (Client{Socket: socket, Timeout: time.Second, ConnectRetry: 3 * time.Second}).Call(ctx, "echo", struct{}{}, &result); err != nil {
		t.Fatalf("retrying client failed: %v", err)
	}
	if result["method"] != "echo" {
		t.Fatalf("unexpected result: %v", result)
	}
	cancel()
	if err := <-serverErr; err != nil {
		t.Fatal(err)
	}
}

func TestZeroValueClientStillFailsImmediatelyWithoutADaemon(t *testing.T) {
	socket := shortSocketPath(t, "no-daemon.sock")
	start := time.Now()
	err := (Client{Socket: socket, Timeout: time.Second}).Call(context.Background(), "echo", struct{}{}, nil)
	if !IsConnectError(err) {
		t.Fatalf("error=%v, want a connect error", err)
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("call without a daemon took %s; the default must not retry", elapsed)
	}
}

func TestConnectRetryStopsAtItsBudget(t *testing.T) {
	socket := shortSocketPath(t, "retry-budget.sock")
	start := time.Now()
	err := (Client{Socket: socket, Timeout: 100 * time.Millisecond, ConnectRetry: 300 * time.Millisecond}).Call(context.Background(), "echo", struct{}{}, nil)
	if !IsConnectError(err) {
		t.Fatalf("error=%v, want a connect error", err)
	}
	if elapsed := time.Since(start); elapsed < 300*time.Millisecond || elapsed > 3*time.Second {
		t.Fatalf("retry budget elapsed=%s, want roughly 300ms", elapsed)
	}
}

func TestConnectRetryStopsWhenTheCallerGivesUp(t *testing.T) {
	socket := shortSocketPath(t, "retry-cancel.sock")
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	err := (Client{Socket: socket, Timeout: 50 * time.Millisecond, ConnectRetry: 10 * time.Second}).Call(ctx, "echo", struct{}{}, nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error=%v, want the caller deadline", err)
	}
}
