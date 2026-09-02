package rpc

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"
)

type deadlineFaultConn struct {
	net.Conn
	readErr, deadlineErr error
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
	accepted := make(chan struct{})
	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			return
		}
		close(accepted)
		_ = conn.Close()
	}()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		<-accepted
		time.Sleep(5 * time.Millisecond)
		cancel()
	}()
	if err := (Client{Socket: socket, Timeout: time.Second}).CallWithKey(ctx, "mutate", "stable-key", map[string]int{"value": 1}, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("connected peer cancellation error=%v", err)
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
