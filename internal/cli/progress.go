package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"strconv"
	"time"

	"github.com/HappyOnigiri/WX/internal/daemon"
	"github.com/HappyOnigiri/WX/internal/i18n"
	"github.com/HappyOnigiri/WX/internal/rpc"
	"github.com/HappyOnigiri/WX/internal/tui"
)

// leaseProgressInterval は daemon から準備の現在位置を取り直す間隔。
// dot の更新（400ms）より細かく、短い区間も1度は表示に現れる。
const leaseProgressInterval = 200 * time.Millisecond

// leaseProgressTimeout は取り直し1回の制限時間。
// 表示のための呼び出しが daemon の応答遅延で溜まらないよう、間隔の数倍で打ち切る。
const leaseProgressTimeout = 2 * time.Second

// leaseResolvingID は経路が決まる前の表示。ResolveAndLease は workspace の解決を同期で行うため、
// ここで待つ時間は経路の選択より前に属する。
const leaseResolvingID = "cli.progress.resolving"

// leaseQueuedID は準備 job が走り出す前の表示。
// 区間の切れ目でも区間名は空になるため、daemon が job の実行中と答えた間はこの表示へ落とさない。
const leaseQueuedID = "cli.progress.phase.queued"

// leaseSettledWidth は確定行の先頭に置く所要時間の幅。
// 区間名の開始位置を揃えて、残した行を縦に読めるようにする。
const leaseSettledWidth = 7

// leaseRouteIDs は daemon.Lease.Route に対応する表示名の message ID である。
var leaseRouteIDs = map[string]string{
	daemon.RouteReady:     "cli.progress.route.ready",
	daemon.RouteUpdate:    "cli.progress.route.update",
	daemon.RouteColdStart: "cli.progress.route.cold",
	daemon.RouteRestore:   "cli.progress.route.restore",
}

// leaseReadinessIDs は実効 readiness の値を表示名へ写す。
// 値そのものは判定側の契約なので、表示文だけを i18n へ置く。
var leaseReadinessIDs = map[string]string{
	readinessReady: "cli.progress.readiness.ready",
	readinessEarly: "cli.progress.readiness.early",
	readinessFull:  "cli.progress.readiness.full",
}

// leasePhaseIDs は daemon が報告する区間名のうち、値域が固定のものだけを表示名へ写す。
// 未知の区間名は daemon 由来の値なので訳さず、原文のまま出す。
var leasePhaseIDs = map[string]string{
	"create":       "cli.progress.phase.create",
	"restore":      "cli.progress.phase.restore",
	"update":       "cli.progress.phase.update",
	"git-register": "cli.progress.phase.git_register",
	"queued":       "cli.progress.phase.queued",
}

// leaseRouteLabel は経路の表示名を返す。未知の経路でも表示は止めない。
func leaseRouteLabel(localizer *i18n.Localizer, route string) string {
	if id, ok := leaseRouteIDs[route]; ok {
		return localizer.Localize(id, nil)
	}
	return localizer.Localize("cli.progress.route.preparing", nil)
}

// leaseReadinessLabel は実効 readiness の表示名を返す。未知の値でも表示を止めない。
func leaseReadinessLabel(localizer *i18n.Localizer, mode string) string {
	if id, ok := leaseReadinessIDs[mode]; ok {
		return localizer.Localize(id, nil)
	}
	if mode == "" {
		return localizer.Localize("cli.progress.readiness.unknown", nil)
	}
	return "readiness=" + mode
}

// leasePhaseName は区間名を表示名へ写す。
func leasePhaseName(localizer *i18n.Localizer, name string) string {
	if id, ok := leasePhaseIDs[name]; ok {
		return localizer.Localize(id, nil)
	}
	return name
}

// leasePhase は表示中の準備区間である。区間名は repository ごとに繰り返すため、
// 対象と何件目かを含めて同じ区間かどうかを判定する。
type leasePhase struct {
	name, target string
	index, total int
}

// text は確定行と待機行に共通の区間表記を返す。
// target は daemon が報告する repository 名なので訳さず、区間名だけを表示言語で解決する。
func (k leasePhase) text(localizer *i18n.Localizer) string {
	name := leasePhaseName(localizer, k.name)
	if k.target == "" && k.total < 2 {
		return name
	}
	label := k.target
	if label == "" {
		label = "workspace"
	}
	if k.total > 1 {
		label += " (" + strconv.Itoa(k.index) + "/" + strconv.Itoa(k.total) + ")"
	}
	return label + " " + name
}

// leaseProgress は貸出の準備を待つ間だけ stderr へ進捗を出す。終わった区間は1行ずつ残し、待機行だけを描き替える。
// 待機行は agent 起動の直前に消えるため、そこにしか出ない情報は速い準備では読めないまま消える。
// stdout は wx new のパスや wx run の出力の契約に使われているため混ぜず、RPC の失敗も表示を据え置くだけにする。
type leaseProgress struct {
	bar     *tui.Progress
	animate bool
	// route は daemon が報告した経路の識別子で、表示名は描画時に解決する。
	// routed は経路が決まったかどうかで、未解決の待機行と区別する。
	route   string
	routed  bool
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
	// readiness は貸出後に client が選んだ実効 readiness。待機しなかったテスト用の
	// zero value では従来どおり経路だけを総括へ出す。
	readiness string
	cancel    context.CancelFunc
	done      chan struct{}
	language  i18n.Language
}

// startLeaseProgress は経路が決まる前の待機行を stderr へ開始する。
// readiness.progress が無効なときと stderr が端末でないときは何も描かず、LeaseProgress も一度も呼ばない。
func (c Client) startLeaseProgress() *leaseProgress {
	return newLeaseProgressLanguage(os.Stderr, c.leaseProgressEnabled(tui.IsTerminal(int(os.Stderr.Fd()))), i18n.Normalize(c.Config.DisplayLanguage()))
}

// leaseProgressEnabled は進捗を描くかを設定と出力先から決める。
// 端末判定を引数で受けることで、端末を用意できない環境でも設定の効き方を試験できる。
func (c Client) leaseProgressEnabled(terminal bool) bool {
	progress := c.Config.RepositoryDefaults.Readiness.Progress
	return progress != nil && *progress && terminal
}

// newLeaseProgress は出力先と描画の有無を受け取る。出力先の判定を分けておくことで、
// 端末を用意できない環境でも取り直しと描き替えの契約を試験できる。
func newLeaseProgress(w io.Writer, animate bool) *leaseProgress {
	return newLeaseProgressLanguage(w, animate, i18n.English)
}

func newLeaseProgressLanguage(w io.Writer, animate bool, lang i18n.Language) *leaseProgress {
	return &leaseProgress{
		bar: tui.StartProgress(w, animate, i18n.New(string(lang)).Localize(leaseResolvingID, nil)), animate: animate, started: time.Now(), language: lang,
	}
}

// watch は経路のラベルへ切り替え、準備の現在位置を取り直し続ける。
// ctx は待機を打ち切る呼び出し側の context で、finish が返るまでに取り直しは止まる。
func (p *leaseProgress) watch(ctx context.Context, client rpc.Client, lease daemon.Lease) {
	if !p.animate || p.cancel != nil || p.finished {
		return
	}
	p.route, p.routed = lease.Route, true
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

// setReadiness は貸出応答後に実効 readiness を記録する。
func (p *leaseProgress) setReadiness(mode string) {
	p.readiness = mode
}

// line は待機行を一時的に確定させて案内を出す。animate が無効でも tui.Progress.Line は
// 通常の一行出力として働くため、progress 設定や端末の有無に左右されない。
func (p *leaseProgress) line(text string) {
	p.bar.Line(text)
}

// summary は総括行の表示を組み立てる。経路を末尾へ置くことで、既存の経路表示と
// 進捗を読む利用者の視線を保ちつつ、実効 readiness も同じ行で確認できる。
func (p *leaseProgress) summary(localizer *i18n.Localizer) string {
	route := leaseRouteLabel(localizer, p.route)
	if p.readiness == "" {
		return route
	}
	return localizer.Localize("cli.progress.summary", map[string]any{
		"Readiness": leaseReadinessLabel(localizer, p.readiness),
		"Route":     route,
	})
}

func (p *leaseProgress) label() string {
	localizer := i18n.New(string(p.language))
	if !p.routed {
		return localizer.Localize(leaseResolvingID, nil)
	}
	route := leaseRouteLabel(localizer, p.route)
	switch {
	case p.phase != (leasePhase{}):
		return localizer.Localize("cli.progress.route_phase", map[string]any{"Route": route, "Phase": p.phase.text(localizer)})
	case p.queued:
		return localizer.Localize("cli.progress.route_phase", map[string]any{"Route": route, "Phase": localizer.Localize(leaseQueuedID, nil)})
	default:
		return route
	}
}

// settle は表示中の区間を確定行として残す。待機行は描き替えで消えるため、
// 終わった区間はここでだけ記録に残り、後から準備のどこに時間が掛かったかを読める。
func (p *leaseProgress) settle() {
	if p.phase == (leasePhase{}) {
		return
	}
	p.bar.Line(fmt.Sprintf("%*s  %s", leaseSettledWidth, formatLeaseDuration(p.elapsed), p.phase.text(i18n.New(string(p.language)))))
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
			localizer := i18n.New(string(p.language))
			p.bar.Line(fmt.Sprintf("%*s  %s", leaseSettledWidth, formatLeaseDuration(time.Since(p.started)), p.summary(localizer)))
		}
	}
	p.finished = true
	p.bar.Finish()
}
