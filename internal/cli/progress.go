package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/HappyOnigiri/WX/internal/daemon"
	"github.com/HappyOnigiri/WX/internal/rpc"
	"github.com/HappyOnigiri/WX/internal/tui"
)

// leaseProgressInterval は daemon から準備の現在位置を取り直す間隔。
// dot の更新（400ms）より細かく、短い区間も1度は表示に現れる。
const leaseProgressInterval = 200 * time.Millisecond

// leaseProgressTimeout は取り直し1回の制限時間。
// 表示のための呼び出しが daemon の応答遅延で溜まらないよう、間隔の数倍で打ち切る。
const leaseProgressTimeout = 2 * time.Second

// leaseResolvingLabel は経路が決まる前の表示。ResolveAndLease は workspace の解決を同期で行うため、
// ここで待つ時間は経路の選択より前に属する。
const leaseResolvingLabel = "Resolving workspace"

// leaseQueuedPhase は job 待ち行列にいる間の表示。実行中の区間がまだ無いことを示す。
const leaseQueuedPhase = "queued"

// leaseRouteLabels は daemon.Lease.Route に対応する表示名である。
var leaseRouteLabels = map[string]string{
	daemon.RouteReady:     "Ready standby",
	daemon.RouteUpdate:    "Standby update",
	daemon.RouteColdStart: "Cold start",
	daemon.RouteRestore:   "Restoring workspace",
}

// leaseRouteLabel は経路の表示名を返す。未知の経路でも表示は止めない。
func leaseRouteLabel(route string) string {
	if label, ok := leaseRouteLabels[route]; ok {
		return label
	}
	return "Preparing workspace"
}

// leaseProgress は貸出の準備を待つ間だけ stderr へ1行の進捗を出す。
// stdout は wx new のパスや wx run の出力の契約に使われているため、混ぜない。
// 進捗は装飾なので、RPC の失敗は表示を据え置くだけで貸出の結果を変えない。
type leaseProgress struct {
	bar     *tui.Progress
	animate bool
	label   string
	started time.Time
	cancel  context.CancelFunc
	done    chan struct{}
}

// startLeaseProgress は経路が決まる前の待機行を stderr へ開始する。
// stderr が端末でなければ何も描かず、LeaseProgress も一度も呼ばない。
func startLeaseProgress() *leaseProgress {
	return newLeaseProgress(os.Stderr, tui.IsTerminal(int(os.Stderr.Fd())))
}

// newLeaseProgress は出力先と描画の有無を受け取る。出力先の判定を分けておくことで、
// 端末を用意できない環境でも取り直しと描き替えの契約を試験できる。
func newLeaseProgress(w io.Writer, animate bool) *leaseProgress {
	return &leaseProgress{
		bar: tui.StartProgress(w, animate, leaseResolvingLabel), animate: animate,
		label: leaseResolvingLabel, started: time.Now(),
	}
}

// watch は経路のラベルへ切り替え、準備の現在位置を取り直し続ける。
// ctx は待機を打ち切る呼び出し側の context で、finish が返るまでに取り直しは止まる。
func (p *leaseProgress) watch(ctx context.Context, client rpc.Client, lease daemon.Lease) {
	if !p.animate || p.cancel != nil {
		return
	}
	p.label = leaseRouteLabel(lease.Route)
	p.bar.Set(p.label, "")
	pollCtx, cancel := context.WithCancel(ctx)
	p.cancel, p.done = cancel, make(chan struct{})
	go p.poll(pollCtx, client, lease)
}

func (p *leaseProgress) poll(ctx context.Context, client rpc.Client, lease daemon.Lease) {
	defer close(p.done)
	ticker := time.NewTicker(leaseProgressInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		var progress daemon.LeaseProgress
		callCtx, cancel := context.WithTimeout(ctx, leaseProgressTimeout)
		err := client.Call(callCtx, "LeaseProgress", map[string]any{"session_id": lease.SessionID, "token": lease.Token}, &progress)
		cancel()
		if err != nil {
			continue
		}
		phase, elapsed := leaseQueuedPhase, time.Since(p.started)
		if progress.Phase != "" {
			phase, elapsed = progress.Phase, time.Duration(progress.PhaseElapsedMS)*time.Millisecond
		}
		p.bar.Set(p.label+": "+phase, fmt.Sprintf(" %ds", int(elapsed.Seconds())))
	}
}

// finish は取り直しを止めてから待機行を消す。
// agent を前面へ出す前と結果を出力する前に必ず呼ぶ必要があるため、二度目以降は何もしない。
func (p *leaseProgress) finish() {
	if p.cancel != nil {
		p.cancel()
		<-p.done
		p.cancel = nil
	}
	p.bar.Finish()
}
