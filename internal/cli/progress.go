package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"strconv"
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

// leaseQueuedLabel は準備 job が走り出す前の表示。
// 区間の切れ目でも区間名は空になるため、daemon が job の実行中と答えた間はこの表示へ落とさない。
const leaseQueuedLabel = "queued"

// leaseSettledWidth は確定行の先頭に置く所要時間の幅。
// 区間名の開始位置を揃えて、残した行を縦に読めるようにする。
const leaseSettledWidth = 7

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

// leasePhase は表示中の準備区間である。区間名は repository ごとに繰り返すため、
// 対象と何件目かを含めて同じ区間かどうかを判定する。
type leasePhase struct {
	name, target string
	index, total int
}

// text は確定行と待機行に共通の区間表記を返す。
func (k leasePhase) text() string {
	if k.target == "" && k.total < 2 {
		return k.name
	}
	label := k.target
	if label == "" {
		label = "workspace"
	}
	if k.total > 1 {
		label += " (" + strconv.Itoa(k.index) + "/" + strconv.Itoa(k.total) + ")"
	}
	return label + " " + k.name
}

// leaseProgress は貸出の準備を待つ間だけ stderr へ進捗を出す。終わった区間は1行ずつ残し、待機行だけを描き替える。
// 待機行は agent 起動の直前に消えるため、そこにしか出ない情報は速い準備では読めないまま消える。
// stdout は wx new のパスや wx run の出力の契約に使われているため混ぜず、RPC の失敗も表示を据え置くだけにする。
type leaseProgress struct {
	bar     *tui.Progress
	animate bool
	route   string
	started time.Time
	// phase は待機行に出している区間。準備 job が始まる前と区間の切れ目では零値になる。
	phase leasePhase
	// elapsed は phase について最後に受け取った経過で、確定行の所要時間になる。
	elapsed time.Duration
	// queued は準備 job がまだ走っていないことを表示済みかどうかである。
	queued bool
	// settled は確定行を1行でも残したかどうかで、総括行を出すかの判断に使う。
	settled  bool
	finished bool
	cancel   context.CancelFunc
	done     chan struct{}
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
		bar: tui.StartProgress(w, animate, leaseResolvingLabel), animate: animate, started: time.Now(),
	}
}

// watch は経路のラベルへ切り替え、準備の現在位置を取り直し続ける。
// ctx は待機を打ち切る呼び出し側の context で、finish が返るまでに取り直しは止まる。
func (p *leaseProgress) watch(ctx context.Context, client rpc.Client, lease daemon.Lease) {
	if !p.animate || p.cancel != nil {
		return
	}
	p.route = leaseRouteLabel(lease.Route)
	p.draw()
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
		p.update(progress)
	}
}

// update は受け取った現在位置を表示へ反映する。
// 区間名が空になるのは job 待ちだけでなく区間の切れ目でも起きるため、
// 準備 job が走っている間は直前の区間を据え置き、表示を待機中へ戻さない。
func (p *leaseProgress) update(progress daemon.LeaseProgress) {
	switch {
	case progress.Phase != "":
		phase := leasePhase{name: progress.Phase, target: progress.Target, index: progress.TargetIndex, total: progress.TargetTotal}
		if phase != p.phase {
			p.settle()
			p.phase, p.queued = phase, false
		}
		p.elapsed = time.Duration(progress.PhaseElapsedMS) * time.Millisecond
	case !progress.Running && p.phase == (leasePhase{}):
		p.queued = true
	}
	p.draw()
}

// draw は待機行を現在の表示へ合わせる。末尾は貸出要求からの経過で、
// 区間ごとの所要時間しか出さないと、全体でどれだけ待っているかが読めなくなる。
func (p *leaseProgress) draw() {
	p.bar.Set(p.label(), fmt.Sprintf("  %ds", int(time.Since(p.started).Seconds())))
}

func (p *leaseProgress) label() string {
	if p.route == "" {
		return leaseResolvingLabel
	}
	switch {
	case p.phase != (leasePhase{}):
		return p.route + ": " + p.phase.text()
	case p.queued:
		return p.route + ": " + leaseQueuedLabel
	default:
		return p.route
	}
}

// settle は表示中の区間を確定行として残す。待機行は描き替えで消えるため、
// 終わった区間はここでだけ記録に残り、後から準備のどこに時間が掛かったかを読める。
func (p *leaseProgress) settle() {
	if p.phase == (leasePhase{}) {
		return
	}
	p.bar.Line(fmt.Sprintf("%*s  %s", leaseSettledWidth, formatLeaseDuration(p.elapsed), p.phase.text()))
	p.settled = true
}

// formatLeaseDuration は所要時間を確定行の幅に収まる長さで返す。
func formatLeaseDuration(d time.Duration) string {
	return strconv.FormatFloat(d.Seconds(), 'f', 1, 64) + "s"
}

// finish は取り直しを止め、最後の区間と総括を残してから待機行を消す。
// agent を前面へ出す前と結果を出力する前に必ず呼ぶ必要があるため、二度目以降は何もしない。
func (p *leaseProgress) finish() {
	if p.cancel != nil {
		p.cancel()
		<-p.done
		p.cancel = nil
	}
	if p.animate && !p.finished {
		p.settle()
		// 総括は準備を待った回にだけ出す。成否は呼び出し側が別に伝えるので、ここでは掛かった時間だけを残す。
		if p.settled {
			p.bar.Line(fmt.Sprintf("%*s  %s", leaseSettledWidth, formatLeaseDuration(time.Since(p.started)), p.route))
		}
	}
	p.finished = true
	p.bar.Finish()
}
