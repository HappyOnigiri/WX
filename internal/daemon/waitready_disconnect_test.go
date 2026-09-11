package daemon

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/HappyOnigiri/WX/internal/rpc"
	"github.com/HappyOnigiri/WX/internal/state"
)

// waitReadyParams は WaitReady の要求を組み立てる。
func waitReadyParams(t *testing.T, sessionID, token string) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"session_id": sessionID, "token": token, "timeout_ms": 30000})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// closedPeer は client が既に切断した接続を表す通知である。
func closedPeer() <-chan struct{} {
	closed := make(chan struct{})
	close(closed)
	return closed
}

// wx new の待機を中断した client は token ごと消えるため、その path 貸出は誰も返せない。
// READY 前に接続が切れたら daemon 側が返却し、利用者の居ない slot を lease.ttl まで残さない。
func TestWaitReadyReleasesThePathLeaseOfADisconnectedClient(t *testing.T) {
	t.Parallel()
	f, repo := leaseWorktreeFixture(t)
	store, m := f.Store, f.Manager
	ctx := context.Background()
	lease, err := m.leaseWithPolicy(ctx, repo, nil, "wx-path", 0, false, leaseAttrs{Kind: state.LeaseKindPath})
	if err != nil {
		t.Fatal(err)
	}
	handler := Handler{Manager: m}
	if _, err := handler.Handle(rpc.WithPeerClosed(ctx, closedPeer()), "WaitReady", waitReadyParams(t, lease.SessionID, lease.Token)); err == nil {
		t.Fatal("WaitReady reported readiness although the client had already disconnected")
	}
	waitUntil(t, 30*time.Second, func() bool {
		session, err := store.SessionByID(ctx, lease.SessionID)
		return err == nil && !sessionInUse(session.State)
	})
}

// 随伴する client を持つ貸出は、その client の生存で回収できる。
// 接続が切れただけで返すと、準備待ちを取り消した wx shell の再試行先を奪ってしまう。
func TestWaitReadyKeepsAShellLeaseWhoseClientDisconnected(t *testing.T) {
	t.Parallel()
	f, repo := leaseWorktreeFixture(t)
	store, m := f.Store, f.Manager
	ctx := context.Background()
	lease, err := m.leaseWithPolicy(ctx, repo, nil, "wx-shell", os.Getpid(), false, leaseAttrs{Kind: state.LeaseKindShell})
	if err != nil {
		t.Fatal(err)
	}
	handler := Handler{Manager: m}
	if _, err := handler.Handle(rpc.WithPeerClosed(ctx, closedPeer()), "WaitReady", waitReadyParams(t, lease.SessionID, lease.Token)); err == nil {
		t.Fatal("WaitReady reported readiness although the client had already disconnected")
	}
	session, err := store.SessionByID(ctx, lease.SessionID)
	if err != nil || !sessionInUse(session.State) {
		t.Fatalf("shell lease session=%+v err=%v, want it still in use", session, err)
	}
}
