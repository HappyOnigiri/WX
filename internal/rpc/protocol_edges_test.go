package rpc

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/HappyOnigiri/WX/internal/testsupport"
)

func TestIdempotentCallStopsRetryAfterConnectedPeerCloses(t *testing.T) {
	socket := testsupport.SocketPath(t, "close-before-response.sock")
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

func TestHandlerTimeoutUsesDefaultAtZero(t *testing.T) {
	if got := (&Server{}).handlerTimeout(); got != defaultServerHandlerTimeout {
		t.Fatalf("zero HandlerTimeout=%s, want default %s", got, defaultServerHandlerTimeout)
	}
}

func TestPruneIdempotencyRemovesEntryAtExactTTL(t *testing.T) {
	now := time.Now()
	server := &Server{idem: map[string]*idempotentEntry{
		"exact": {done: closedChannel(), ended: now.Add(-idempotencyTTL)},
	}}

	server.pruneIdempotencyLocked(now)
	if _, ok := server.idem["exact"]; ok {
		t.Fatal("idempotency entry at the TTL was retained")
	}
}

func TestZeroClientTimeoutUsesDefaultRequestDeadline(t *testing.T) {
	socket := testsupport.SocketPath(t, "zero-timeout.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	serverErr := make(chan error, 1)
	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			serverErr <- acceptErr
			return
		}
		defer conn.Close()
		_ = conn.SetReadDeadline(time.Now().Add(time.Second))
		var request Request
		if err := readFrame(bufio.NewReader(conn), &request); err != nil {
			serverErr <- err
			return
		}
		if request.Deadline == "" {
			serverErr <- errors.New("zero client timeout omitted the default request deadline")
			return
		}
		serverErr <- writeFrame(conn, Response{Version: ProtocolVersion, ID: request.ID})
	}()

	callErr := (Client{Socket: socket}).Call(context.Background(), "echo", nil, nil)
	_ = listener.Close()
	if err := <-serverErr; err != nil {
		t.Fatalf("server: %v", err)
	}
	if callErr != nil {
		t.Fatalf("zero client timeout call failed: %v", callErr)
	}
}

func TestIdempotentCallRetriesExactlyThreeTransientTransportFailures(t *testing.T) {
	socket := testsupport.SocketPath(t, "transport-retries.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	var accepted atomic.Int32
	serverDone := make(chan struct{})
	go func() {
		defer close(serverDone)
		for {
			conn, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}
			accepted.Add(1)
			_ = conn.SetReadDeadline(time.Now().Add(time.Second))
			var request Request
			_ = readFrame(bufio.NewReader(conn), &request)
			_ = conn.Close()
		}
	}()

	// 短い期限では負荷で三回目の transport failure より先に caller の deadline
	// が切れ、検査対象外の context deadline exceeded が返る。retry 回数だけを
	// 検査するため、呼び出し側の期限は設けない。
	ctx, cancel := context.WithCancel(context.Background())
	callErr := (Client{Socket: socket, Timeout: time.Second}).CallWithKey(ctx, "mutate", "retry-key", map[string]int{"value": 1}, nil)
	cancel()
	_ = listener.Close()
	<-serverDone

	if callErr == nil || (!errors.Is(callErr, io.EOF) && !errors.Is(callErr, io.ErrUnexpectedEOF)) {
		t.Fatalf("retry result=%v, want the final transient transport error", callErr)
	}
	if got := accepted.Load(); got != 3 {
		t.Fatalf("transient transport failures accepted %d requests, want 3", got)
	}
}

func TestReadFrameAcceptsPayloadAtMaximumFrameSize(t *testing.T) {
	payload := strings.Repeat("x", maxFrame-2)
	data, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) != maxFrame {
		t.Fatalf("encoded payload length=%d, want %d", len(data), maxFrame)
	}
	frame := make([]byte, 4+len(data))
	binary.BigEndian.PutUint32(frame[:4], uint32(len(data)))
	copy(frame[4:], data)

	var got string
	if err := readFrame(bytes.NewReader(frame), &got); err != nil {
		t.Fatalf("maximum-size frame was rejected: %v", err)
	}
	if got != payload {
		t.Fatalf("decoded maximum-size payload length=%d, want %d", len(got), len(payload))
	}
}

func TestWriteFrameAcceptsPayloadAtMaximumFrameSize(t *testing.T) {
	payload := strings.Repeat("x", maxFrame-2)
	var frame bytes.Buffer
	if err := writeFrame(&frame, payload); err != nil {
		t.Fatalf("maximum-size frame was rejected: %v", err)
	}
	if got := len(frame.Bytes()); got != 4+maxFrame {
		t.Fatalf("written frame length=%d, want %d", got, 4+maxFrame)
	}
	if got := binary.BigEndian.Uint32(frame.Bytes()[:4]); got != maxFrame {
		t.Fatalf("written payload length=%d, want %d", got, maxFrame)
	}
}

func TestConnectRetryBridgesADaemonThatIsNotListeningYet(t *testing.T) {
	socket := testsupport.SocketPath(t, "connect-retry.sock")
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
	socket := testsupport.SocketPath(t, "no-daemon.sock")
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
	socket := testsupport.SocketPath(t, "retry-budget.sock")
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
	socket := testsupport.SocketPath(t, "retry-cancel.sock")
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	err := (Client{Socket: socket, Timeout: 50 * time.Millisecond, ConnectRetry: 10 * time.Second}).Call(ctx, "echo", struct{}{}, nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error=%v, want the caller deadline", err)
	}
}

// MaxHandlerTimeoutFunc は要求ごとの上限を返し、起動時に固めた MaxHandlerTimeout より優先する。
// 設定の再読込で readiness の予算が増えても、古い上限が handler を先に打ち切らないためである。
func TestRequestDeadlinePrefersLiveMaxHandlerTimeout(t *testing.T) {
	live := time.Minute
	server := &Server{HandlerTimeout: time.Hour, MaxHandlerTimeout: time.Minute, MaxHandlerTimeoutFunc: func() time.Duration { return live }}
	requested := time.Now().Add(72 * time.Hour).UTC().Format(time.RFC3339Nano)
	live = 2 * time.Hour
	before := time.Now()
	deadline, err := server.requestDeadline(context.Background(), requested)
	if err != nil {
		t.Fatal(err)
	}
	if deadline.Before(before.Add(2*time.Hour - time.Minute)) {
		t.Fatalf("the startup ceiling clamped the live ceiling: got %v, want at least %v", deadline, before.Add(2*time.Hour))
	}
	// 0 を返す関数は上限を持たない指定なので、静的な MaxHandlerTimeout へ落ちる。
	server.MaxHandlerTimeoutFunc = func() time.Duration { return 0 }
	deadline, err = server.requestDeadline(context.Background(), requested)
	if err != nil {
		t.Fatal(err)
	}
	if deadline.After(before.Add(time.Minute + time.Second)) {
		t.Fatalf("a zero live ceiling did not fall back to MaxHandlerTimeout: got %v", deadline)
	}
}
