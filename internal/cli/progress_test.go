package cli

import (
	"bytes"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/HappyOnigiri/WX/internal/daemon"
)

// 経路の表示名は貸出応答の Route から引く。未知の経路でも表示を止めない。
func TestLeaseRouteLabelCoversEveryRouteAndFallsBack(t *testing.T) {
	t.Parallel()
	for route, want := range map[string]string{
		daemon.RouteReady:     "Ready standby",
		daemon.RouteUpdate:    "Standby update",
		daemon.RouteColdStart: "Cold start",
		daemon.RouteRestore:   "Restoring workspace",
	} {
		if got := leaseRouteLabel(route); got != want {
			t.Fatalf("leaseRouteLabel(%q)=%q, want %q", route, got, want)
		}
	}
	if got := leaseRouteLabel(""); got == "" {
		t.Fatal("an unknown route produced an empty label")
	}
}

// 端末でない stderr では進捗を描かず、LeaseProgress も一度も呼ばない。
// pipe へ落とした実行が表示のためだけに RPC を積むと、wx new / wx run の実行が装飾に引きずられる。
func TestLeaseProgressIsNotPolledWithoutATerminal(t *testing.T) {
	client, handler, _, ctx := leaseFixture(t)
	handler.mu.Lock()
	handler.lease.Ready = false
	handler.lease.Route = daemon.RouteColdStart
	handler.mu.Unlock()
	stdout := captureLeaseStdout(t, func() {
		if exit := client.RunLeaseNew(ctx, nil, false); exit != 0 {
			t.Fatalf("RunLeaseNew exit=%d", exit)
		}
	})
	if strings.TrimSpace(stdout) != handler.lease.Path {
		t.Fatalf("stdout=%q, want only the lease path", stdout)
	}
	handler.mu.Lock()
	methods := strings.Join(handler.methods, ",")
	handler.mu.Unlock()
	if !strings.Contains(methods, "WaitReady") {
		t.Fatalf("methods=%s, want the readiness wait", methods)
	}
	if strings.Contains(methods, "LeaseProgress") {
		t.Fatalf("methods=%s, want no progress polling without a terminal", methods)
	}
}

// 待機行は経路のラベルへ切り替わり、daemon が返した区間名と経過秒へ更新される。
// 端末でだけ描くため、出力先と描画の有無を与えて契約だけを確かめる。
func TestLeaseProgressShowsTheRouteAndRunningPhase(t *testing.T) {
	client, handler, _, ctx := leaseFixture(t)
	handler.mu.Lock()
	handler.leaseProgress = map[string]any{"state": "PREPARING", "phase": "cow-place", "phase_elapsed_ms": 12000}
	handler.mu.Unlock()
	out := &syncWriter{}
	waiting := newLeaseProgress(out, true)
	waiting.watch(ctx, client.RPC, daemon.Lease{SessionID: "session", Token: "token", Route: daemon.RouteColdStart})
	deadline := time.Now().Add(10 * time.Second)
	for !strings.Contains(out.String(), "Cold start: cow-place") {
		if !time.Now().Before(deadline) {
			waiting.finish()
			t.Fatalf("the running phase never reached the waiting line: %q", out.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
	waiting.finish()
	drawn := out.String()
	if !strings.Contains(drawn, "Resolving workspace") {
		t.Fatalf("the pre-route label was never drawn: %q", drawn)
	}
	if !strings.Contains(drawn, " 12s") {
		t.Fatalf("the phase elapsed time was not drawn: %q", drawn)
	}
	// 待機行は終了時に消え、以後の出力へ残らない。
	if !strings.HasSuffix(drawn, "\r") {
		t.Fatalf("finish did not erase the waiting line: %q", drawn)
	}
}

// syncWriter は取り直し goroutine の描画中でも、テストから描画済み内容を読めるようにする。
type syncWriter struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (w *syncWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Write(p)
}

func (w *syncWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}
