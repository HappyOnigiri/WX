package rpc

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/HappyOnigiri/WX/internal/testsupport"
)

// peerCloseHandler は切断通知が届くまで待ち、届いたかどうかを報告する handler である。
type peerCloseHandler struct {
	entered chan struct{}
	noticed chan bool
}

func (h peerCloseHandler) Handle(ctx context.Context, _ string, _ json.RawMessage) (any, error) {
	close(h.entered)
	select {
	case <-PeerClosed(ctx):
		h.noticed <- true
	case <-ctx.Done():
		h.noticed <- false
	}
	return map[string]bool{"ok": true}, nil
}

// 待機中の handler は、応答を受け取る client が接続を閉じたことを PeerClosed で知る。
// WaitReady のように長く待つ handler が、宛先の無い待機を打ち切る根拠になる。
func TestServerTellsTheHandlerWhenTheClientClosesTheConnection(t *testing.T) {
	socket := testsupport.SocketPath(t, "wxd.sock")
	serverCtx, cancelServer := context.WithCancel(context.Background())
	defer cancelServer()
	handler := peerCloseHandler{entered: make(chan struct{}), noticed: make(chan bool, 1)}
	server := &Server{Socket: socket, Handler: handler}
	done := make(chan error, 1)
	go func() { done <- server.Serve(serverCtx) }()
	testsupport.WaitForSocket(t, socket, done)

	callCtx, cancelCall := context.WithCancel(context.Background())
	callDone := make(chan error, 1)
	go func() {
		// client 側の ctx cancel は接続を閉じる。Ctrl-C で client が消えたときと同じく、
		// server から見れば応答の宛先が居なくなった状態になる。
		callDone <- (Client{Socket: socket, Timeout: 5 * time.Second}).Call(callCtx, "WaitReady", map[string]string{"session_id": "s"}, nil)
	}()
	<-handler.entered
	cancelCall()
	select {
	case noticed := <-handler.noticed:
		if !noticed {
			t.Fatal("the handler was not told that the client had disconnected")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the handler never learned about the disconnect")
	}
	<-callDone
}

// 通知の載っていない ctx では PeerClosed は nil を返し、select は永久に待つ枝になる。
func TestPeerClosedIsNilWithoutANotification(t *testing.T) {
	if PeerClosed(context.Background()) != nil {
		t.Fatal("PeerClosed returned a channel for a context without a notification")
	}
}
