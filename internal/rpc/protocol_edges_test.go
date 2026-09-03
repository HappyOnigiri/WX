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

type deadlineFaultConn struct {
	net.Conn
	readErr, deadlineErr error
}

type delayedJSON struct {
	started chan struct{}
	release <-chan struct{}
}

func (p delayedJSON) MarshalJSON() ([]byte, error) {
	close(p.started)
	<-p.release
	return []byte(`{}`), nil
}

func (c deadlineFaultConn) SetReadDeadline(time.Time) error { return c.readErr }

func (c deadlineFaultConn) SetDeadline(time.Time) error { return c.deadlineErr }

func TestServeConnStopsWhenDeadlineInstallationFails(t *testing.T) {
	t.Run("frame deadline", func(t *testing.T) {
		serverSide, clientSide := net.Pipe()
		defer func() { _ = clientSide.Close() }()
		done := make(chan struct{})
		go func() {
			(&Server{Handler: echoHandler{}}).serveConn(context.Background(), deadlineFaultConn{Conn: serverSide, readErr: errors.New("read deadline fault")})
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("serveConn remained blocked after frame deadline failure")
		}
	})

	t.Run("handler deadline", func(t *testing.T) {
		serverSide, clientSide := net.Pipe()
		defer func() { _ = clientSide.Close() }()
		done := make(chan struct{})
		go func() {
			(&Server{Handler: echoHandler{}}).serveConn(context.Background(), deadlineFaultConn{Conn: serverSide, deadlineErr: errors.New("handler deadline fault")})
			close(done)
		}()
		if err := writeFrame(clientSide, Request{Version: ProtocolVersion, ID: "request", Method: "echo"}); err != nil {
			t.Fatal(err)
		}
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("serveConn remained blocked after handler deadline failure")
		}
	})
}

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

func TestCallReturnsParentCancellationWhenRequestEncodingOutlivesIt(t *testing.T) {
	socket := shortSocketPath(t, "deadline-during-encoding.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseNow := func() { releaseOnce.Do(func() { close(release) }) }
	defer releaseNow()
	serverDone := make(chan struct{})
	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr == nil {
			defer conn.Close()
			<-release
		}
		close(serverDone)
	}()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := make(chan struct{})
	result := make(chan error, 1)
	go func() {
		result <- (Client{Socket: socket, Timeout: time.Second}).Call(ctx, "mutate", delayedJSON{started: started, release: release}, nil)
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("request encoding did not start")
	}
	cancel()
	releaseNow()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("encoding cancellation error=%v", err)
	}
	select {
	case <-serverDone:
	case <-time.After(time.Second):
		t.Fatal("server did not accept deadline test connection")
	}
}

func TestRequestDeadlineClampsImplausibleClientRequestedDeadline(t *testing.T) {
	requested := time.Now().Add(365 * 24 * time.Hour).UTC().Format(time.RFC3339Nano)
	before := time.Now()
	deadline, err := (&Server{HandlerTimeout: time.Hour}).requestDeadline(context.Background(), requested)
	if err != nil {
		t.Fatal(err)
	}
	if max := before.Add(defaultMaxHandlerTimeout + time.Second); deadline.After(max) {
		t.Fatalf("client-requested deadline was not clamped: got %v, want at most %v", deadline, max)
	}
	if deadline.Before(before.Add(defaultMaxHandlerTimeout - time.Second)) {
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
