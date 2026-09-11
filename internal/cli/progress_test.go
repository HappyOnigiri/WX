package cli

import (
	"bytes"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/HappyOnigiri/WX/internal/config"
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

// 待機行は経路のラベルへ切り替わり、daemon が返した区間名へ更新される。
// 端末でだけ描くため、出力先と描画の有無を与えて契約だけを確かめる。
func TestLeaseProgressShowsTheRouteAndRunningPhase(t *testing.T) {
	client, handler, _, ctx := leaseFixture(t)
	handler.mu.Lock()
	handler.leaseProgress = map[string]any{"state": "PREPARING", "running": true, "phase": "cow-place", "phase_elapsed_ms": 12000}
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
	// 終わった区間は所要時間つきの行として残り、待機行の描き替えでは消えない。
	if !strings.Contains(drawn, "12.0s  cow-place\n") {
		t.Fatalf("the finished phase was not settled on its own line: %q", drawn)
	}
	// 待機行は終了時に消え、以後の出力へ残らない。
	if !strings.HasSuffix(drawn, "\r") {
		t.Fatalf("finish did not erase the waiting line: %q", drawn)
	}
}

// 区間の切れ目では区間名が空になる。準備 job が走っている間は直前の区間を据え置き、
// 待機中の表示へ戻さない。戻すと、実際には進んでいる準備が止まって見える。
func TestLeaseProgressKeepsThePhaseBetweenIntervals(t *testing.T) {
	t.Parallel()
	out := &syncWriter{}
	waiting := newLeaseProgress(out, true)
	waiting.route = leaseRouteLabel(daemon.RouteColdStart)
	waiting.update(daemon.LeaseProgress{State: "PREPARING", Running: true, Phase: "checkout", PhaseElapsedMS: 300})
	waiting.update(daemon.LeaseProgress{State: "PREPARING", Running: true})
	if got := waiting.label(); got != "Cold start: checkout" {
		t.Fatalf("label=%q, want the previous phase to stay while the job runs", got)
	}
	if strings.Contains(out.String(), leaseQueuedLabel) {
		t.Fatalf("the gap between phases was drawn as waiting: %q", out.String())
	}
}

// 準備 job がまだ走っていない間だけ待機中と表示する。
func TestLeaseProgressShowsQueuedOnlyBeforeTheJobRuns(t *testing.T) {
	t.Parallel()
	waiting := newLeaseProgress(&syncWriter{}, true)
	waiting.route = leaseRouteLabel(daemon.RouteColdStart)
	waiting.update(daemon.LeaseProgress{State: "PREPARING"})
	if got := waiting.label(); got != "Cold start: "+leaseQueuedLabel {
		t.Fatalf("label=%q, want the queued label before the job starts", got)
	}
	waiting.update(daemon.LeaseProgress{State: "PREPARING", Running: true, Phase: "git-register"})
	waiting.update(daemon.LeaseProgress{State: "PREPARING", Running: false})
	if got := waiting.label(); got != "Cold start: git-register" {
		t.Fatalf("label=%q, want the last phase to stay once preparation has started", got)
	}
}

// 区間名は repository ごとに繰り返すため、対象と何件目かを添えて何周目かを読めるようにする。
func TestLeaseProgressLabelsTheRepositoryOfEachPhase(t *testing.T) {
	t.Parallel()
	out := &syncWriter{}
	waiting := newLeaseProgress(out, true)
	waiting.route = leaseRouteLabel(daemon.RouteColdStart)
	waiting.update(daemon.LeaseProgress{State: "PREPARING", Running: true, Phase: "checkout", Target: "app", TargetIndex: 1, TargetTotal: 5, PhaseElapsedMS: 400})
	if got := waiting.label(); got != "Cold start: app (1/5) checkout" {
		t.Fatalf("label=%q, want the repository and its position", got)
	}
	// 同じ区間名でも対象が変われば別の区間として確定行に残す。
	waiting.update(daemon.LeaseProgress{State: "PREPARING", Running: true, Phase: "checkout", Target: "web", TargetIndex: 2, TargetTotal: 5})
	if !strings.Contains(out.String(), "0.4s  app (1/5) checkout\n") {
		t.Fatalf("the previous repository's phase was not settled: %q", out.String())
	}
	if got := waiting.label(); got != "Cold start: web (2/5) checkout" {
		t.Fatalf("label=%q, want the next repository", got)
	}
}

// 待機行は agent 起動の直前に消えるため、掛かった時間は終了時に1行として残す。
func TestLeaseProgressSettlesTheTotalOnce(t *testing.T) {
	t.Parallel()
	out := &syncWriter{}
	waiting := newLeaseProgress(out, true)
	waiting.route = leaseRouteLabel(daemon.RouteColdStart)
	waiting.update(daemon.LeaseProgress{State: "PREPARING", Running: true, Phase: "checkout", PhaseElapsedMS: 100})
	waiting.finish()
	waiting.finish()
	if got := strings.Count(out.String(), "Cold start\n"); got != 1 {
		t.Fatalf("the total was written %d times, want exactly one summary: %q", got, out.String())
	}
}

// 準備を待たずに終えた回は総括を残さない。貸出のたびに1行増えると、待たなかったことが読み取れない。
func TestLeaseProgressLeavesNothingWhenNothingWasWaitedFor(t *testing.T) {
	t.Parallel()
	out := &syncWriter{}
	waiting := newLeaseProgress(out, true)
	waiting.finish()
	if strings.Contains(out.String(), "\n") {
		t.Fatalf("a lease that waited for no preparation left a line: %q", out.String())
	}
}

// readiness.progress で進捗表示を切れる。既定は有効で、無効にすると端末でも描かない。
// 端末判定はテストから作れないため、判定を引数で受ける経路で設定の効き方だけを確かめる。
func TestLeaseProgressFollowsTheReadinessProgressSetting(t *testing.T) {
	t.Parallel()
	client := Client{Config: config.Defaults()}
	if !client.leaseProgressEnabled(true) {
		t.Fatal("the default configuration disabled the progress display")
	}
	if client.leaseProgressEnabled(false) {
		t.Fatal("the progress display was enabled for a non-terminal output")
	}
	client.Config.Readiness.Progress = false
	if client.leaseProgressEnabled(true) {
		t.Fatal("readiness.progress=false still enabled the progress display")
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
