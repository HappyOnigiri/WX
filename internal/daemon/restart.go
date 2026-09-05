package daemon

import (
	"context"
	"errors"
	"os"
	"syscall"
	"time"

	"github.com/HappyOnigiri/WX/internal/domain"
	"github.com/HappyOnigiri/WX/internal/launchd"
)

// 自身を置き換える launchctl 呼び出しの制限時間。
// kickstart -k はこのプロセスへ SIGTERM を送るため Manager の context は使わない。
// そうしないと終了処理で launchctl が置換を依頼する前に取り消される。
const restartKickstartTimeout = 10 * time.Second

// Handler.Handle の終了後もしばらくライフサイクルゲートを閉じておく時間。
// handler 復帰後に rpc.Server が応答を書き込むため、この間のシグナルは接続断として届き得る。
const lifecycleReplyGrace = 100 * time.Millisecond

// 失敗した kickstart または停止シグナルを再試行する回数の上限。
// ここまで失敗するものは再試行で直る一時障害とは扱わない。
const maxLifecycleAttempts = 3

// デーモン自身のバイナリを識別する情報。
// inode だけでは上書きを、mtime とサイズだけでは同値な置換を見逃すため、3 つを比較する。
type executableSnapshot struct {
	identity string
	modTime  time.Time
	size     int64
}

func (s executableSnapshot) matches(other executableSnapshot) bool {
	return s.identity == other.identity && s.size == other.size && s.modTime.Equal(other.modTime)
}

func snapshotExecutable(path string) (executableSnapshot, error) {
	info, err := os.Stat(path)
	if err != nil {
		return executableSnapshot{}, err
	}
	identity, err := domain.FileIdentity(info)
	if err != nil {
		return executableSnapshot{}, err
	}
	return executableSnapshot{identity: identity, modTime: info.ModTime(), size: info.Size()}, nil
}

// 定期検査の基準を記録する。基準を取得できない場合は監視を無効にする。
// os.Executable は起動時のパスを返し続けるため、stat 失敗を未変更の根拠にしてはいけない。
func (m *Manager) watchExecutable(executable string, executableErr error) {
	if executableErr != nil {
		m.log.Warn("daemon executable is unknown; automatic restart after a binary replacement is disabled", "error", executableErr)
		return
	}
	snapshot, err := snapshotExecutable(executable)
	if err != nil {
		m.log.Warn("daemon executable could not be identified; automatic restart after a binary replacement is disabled", "path", executable, "error", err)
		return
	}
	m.mu.Lock()
	m.executablePath = executable
	m.executableBaseline = snapshot
	m.executableWatch = true
	m.mu.Unlock()
}

// 起動時の基準とバイナリが一致しなくなったら再起動を保留する。
// 実行中プロセスは古い inode を使い続けるためフラグは下げず、停止要求があれば検査しない。
func (m *Manager) detectExecutableReplacement() {
	m.mu.RLock()
	watching, path, baseline := m.executableWatch, m.executablePath, m.executableBaseline
	pending := m.restartPending || m.stopPending
	m.mu.RUnlock()
	if !watching || pending {
		return
	}
	current, err := snapshotExecutable(path)
	if err != nil {
		// install(1) は一時ファイルを rename するため、パスが一時的に消える。
		// 置換と断定せず次の周期で再検査する。
		if !errors.Is(err, os.ErrNotExist) {
			m.log.Warn("daemon executable could not be inspected", "path", path, "error", err)
		}
		return
	}
	if current.matches(baseline) {
		return
	}
	m.mu.Lock()
	// stat 中はロックを保持しないため、その間に別の要求が入り得る。
	// 2 つの意図を同時に保留せず、明示的な停止を再起動で上書きしない。
	if m.restartPending || m.stopPending {
		m.mu.Unlock()
		return
	}
	m.restartPending = true
	m.mu.Unlock()
	m.log.Info("daemon executable was replaced; restart is pending", "path", path, "baseline_identity", baseline.identity, "current_identity", current.identity)
}

// 明示的な再起動要求を記録する。実行は runPendingLifecycle に任せ、進行中 RPC を中断しない。
// BeginRPCRequest と CompleteRPCRequest の間で停止すると予約が PENDING のまま残るためである。
// 再起動と停止は互いに保留を上書きし、ゲート上には常に 1 つの意図だけを置く。
func (m *Manager) RequestRestart(ctx context.Context) map[string]any {
	m.mu.Lock()
	if m.lifecycleSignalDeliveredLocked() {
		stopping, restarting := m.stopPending, m.restartPending
		m.mu.Unlock()
		m.log.Warn("daemon restart was requested while another lifecycle action was already under way")
		return m.conflictReply(ctx, stopping, restarting)
	}
	already := m.restartPending
	m.restartPending = true
	m.stopPending = false
	m.resetLifecycleRetriesLocked()
	m.mu.Unlock()
	m.notifyLifecycleCheck()
	m.log.Info("daemon restart was requested; it will be issued once the daemon is idle")
	reply := m.lifecycleSnapshot(ctx)
	reply["restart_pending"] = true
	reply["already_pending"] = already
	return reply
}

// 明示的な停止要求を記録する。SIGTERM も進行中 RPC を救えず応答なしで接続を閉じるため、
// 再起動と同じアイドルゲートを通し、予約が未完了のまま残ることを防ぐ。
func (m *Manager) RequestStop(ctx context.Context) map[string]any {
	m.mu.Lock()
	if m.lifecycleSignalDeliveredLocked() {
		stopping, restarting := m.stopPending, m.restartPending
		m.mu.Unlock()
		m.log.Warn("daemon stop was requested while another lifecycle action was already under way")
		return m.conflictReply(ctx, stopping, restarting)
	}
	already := m.stopPending
	m.stopPending = true
	m.restartPending = false
	m.resetLifecycleRetriesLocked()
	m.mu.Unlock()
	m.notifyLifecycleCheck()
	m.log.Info("daemon stop was requested; it will be issued once the daemon is idle")
	reply := m.lifecycleSnapshot(ctx)
	reply["stop_pending"] = true
	reply["already_pending"] = already
	return reply
}

// 既にシグナルを送った後の競合要求に応答する。シグナルは取り消せないため、実行中の意図を返す。
func (m *Manager) conflictReply(ctx context.Context, stopping, restarting bool) map[string]any {
	reply := m.lifecycleSnapshot(ctx)
	reply["stop_pending"] = stopping
	reply["restart_pending"] = restarting
	reply["conflict"] = true
	return reply
}

// 既にソケットへ応答するデーモンへ wx daemon start が送る要求。
// まだシグナルを送っていない停止だけを取り消し、送信済みなら停止中であることを返す。
func (m *Manager) RequestStart(ctx context.Context) map[string]any {
	m.mu.Lock()
	cancelled := m.stopPending && !m.lifecycleSignalDeliveredLocked()
	if cancelled {
		m.stopPending = false
		m.resetLifecycleRetriesLocked()
	}
	stopping := m.stopPending
	m.mu.Unlock()
	if cancelled {
		m.log.Info("pending daemon stop was cancelled by a start request")
	}
	reply := m.lifecycleSnapshot(ctx)
	reply["stop_pending"] = stopping
	reply["stop_cancelled"] = cancelled
	return reply
}

// アイドルゲートが待っている条件を返し、同期コマンドがタイムアウト理由を表示できるようにする。
// inflight_requests は保護対象の要求だけを数え、launchd_managed は置換の有無を示す。
func (m *Manager) lifecycleSnapshot(ctx context.Context) map[string]any {
	m.mu.RLock()
	inflight := m.inflightRequests - m.inflightLifecycle
	m.mu.RUnlock()
	jobs := 0
	if status, err := m.store.Status(ctx); err == nil {
		jobs = status.Jobs
	}
	return map[string]any{
		"pid":               os.Getpid(),
		"launchd_managed":   m.underLaunchd(),
		"inflight_requests": inflight,
		"queued_jobs":       jobs,
	}
}

// maintainJobs を起こし、要求が 10 秒のジョブ周期を待たずにゲート検査されるようにする。
func (m *Manager) notifyLifecycleCheck() {
	select {
	case m.lifecycleChecks <- struct{}{}:
	default:
	}
}

// 停止または再起動がゲート待ちかを返す。maintainJobs は短い周期での再検査に使う。
func (m *Manager) lifecyclePending() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return (m.restartPending || m.stopPending) && !m.lifecycleClaimed
}

// 作業を落とさず追従できる時点で保留中の停止・再起動を実行する。
// 実行中の RPC と job が捌けた時点で発行し、次の要求が来るかどうかは待たない。
// 停止は launchd を必要とせず、--foreground で起動したデーモンにも適用する。
func (m *Manager) runPendingLifecycle() {
	m.mu.Lock()
	stop, restart := m.stopPending, m.restartPending
	if (!stop && !restart) || m.lifecycleClaimed || !m.lifecycleGateOpenLocked() {
		m.mu.Unlock()
		return
	}
	path, baseline := m.executablePath, m.executableBaseline
	m.mu.Unlock()
	status, err := m.store.Status(m.ctx)
	if err != nil {
		if m.ctx.Err() == nil {
			m.log.Warn("pending daemon lifecycle action could not read job state", "error", err)
		}
		return
	}
	if status.Jobs > 0 {
		return
	}
	if restart && !m.underLaunchd() {
		m.warnRestartUnmanaged()
		return
	}
	m.mu.Lock()
	// ジョブ検索中はロックを保持しないため意図が上書きされ得る。
	// 古い意図のシグナルは直前の要求と逆になるため破棄し、新しい意図を次回に評価する。
	if !m.lifecycleIntentUnchangedLocked(stop, restart) {
		m.mu.Unlock()
		m.notifyLifecycleCheck()
		return
	}
	if m.lifecycleClaimed || !m.lifecycleGateOpenLocked() {
		m.mu.Unlock()
		return
	}
	// 発行前に意図を確保し、シグナル送信成功時だけ確定する。
	// 成功後に kickstart -k を繰り返すと、launchd が起動した置換プロセスを終了させる。
	m.lifecycleClaimed = true
	m.mu.Unlock()
	// シグナル送信は maintainJobs の外で行う。Close が同じ goroutine を待つため、内部で実行すると
	// 自身が起こした終了処理が launchctl の完了を待つ循環になる。
	if stop {
		m.log.Info("stopping daemon on request")
		go m.issueStop()
		return
	}
	m.log.Info("restarting daemon", "path", path, "baseline_identity", baseline.identity)
	go m.issueRestart(path)
}

// 自身を置き換える launchctl を実行する。kickstart -k が自身へ SIGTERM を送るため
// Manager の context から派生させず、launchd への依頼完了を待つ。
func (m *Manager) issueRestart(path string) {
	ctx, cancel := context.WithTimeout(context.Background(), restartKickstartTimeout)
	defer cancel()
	err := m.kickstartService(ctx)
	if err == nil {
		return
	}
	// kickstart が失敗した場合は SIGTERM が届いておらず、このプロセスは実行を継続する。
	attempts, exhausted := m.releaseLifecycleClaim()
	if exhausted {
		m.log.Error("daemon restart failed and will not be retried; restart wx daemon manually", "path", path, "attempts", attempts, "error", err)
		return
	}
	m.log.Warn("daemon restart failed; retrying on the next check", "path", path, "attempts", attempts, "error", err)
}

// launchd の SIGTERM と同じ経路で自身を終了させ、signal.NotifyContext と Manager.Close を通す。
// 終了コード 0 により LaunchAgent の KeepAlive{SuccessfulExit:false} による再起動を防ぐ。
func (m *Manager) issueStop() {
	err := m.terminateSelf()
	if err == nil {
		return
	}
	attempts, exhausted := m.releaseLifecycleClaim()
	if exhausted {
		m.log.Error("daemon stop failed and will not be retried; stop wx daemon manually", "attempts", attempts, "error", err)
		return
	}
	m.log.Warn("daemon stop failed; retrying on the next check", "attempts", attempts, "error", err)
}

// 未送信の意図を次回検査へ戻す。再試行は上限で打ち切り、明示的な再要求で予算をリセットする。
// 送信済みの claim は保持し、同じシグナルを繰り返さない。呼び出し時は m.mu を保持する。
func (m *Manager) resetLifecycleRetriesLocked() {
	if m.lifecycleSignalDeliveredLocked() {
		return
	}
	m.lifecycleAttempts = 0
	m.lifecycleClaimed = false
}

// claim が送信済みか、再試行上限で解除されたものかを区別する。前者だけが不可逆である。
// 呼び出し時は m.mu を保持する。
func (m *Manager) lifecycleSignalDeliveredLocked() bool {
	return m.lifecycleClaimed && m.lifecycleAttempts < maxLifecycleAttempts
}

func (m *Manager) releaseLifecycleClaim() (attempts int, exhausted bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.lifecycleAttempts++
	exhausted = m.lifecycleAttempts >= maxLifecycleAttempts
	m.lifecycleClaimed = exhausted
	return m.lifecycleAttempts, exhausted
}

// 評価開始時の保留意図がまだ同じかを返す。呼び出し時は m.mu を保持する。
func (m *Manager) lifecycleIntentUnchangedLocked(stop, restart bool) bool {
	return m.stopPending == stop && m.restartPending == restart
}

// デーモンが置換・停止できる程度にアイドルかを返す。判定は実行中の要求だけを見る。
// 要求と要求の隙間を待たないのは、heartbeat のような周期的 RPC が固定間隔のアイドルを恒久的に塞ぎ得るためである。
// 隙間で置換されたクライアントは rpc.Client の ConnectRetry と冪等キーで追従する。呼び出し時は m.mu を保持する。
func (m *Manager) lifecycleGateOpenLocked() bool {
	if m.inflightRequests > 0 {
		return false
	}
	return m.lastLifecycleEnd.IsZero() || time.Since(m.lastLifecycleEnd) >= lifecycleReplyGrace
}

func (m *Manager) kickstartService(ctx context.Context) error {
	m.mu.RLock()
	kickstart := m.kickstart
	m.mu.RUnlock()
	if kickstart == nil {
		kickstart = launchd.Kickstart
	}
	return kickstart(ctx)
}

func (m *Manager) terminateSelf() error {
	m.mu.RLock()
	terminate := m.terminate
	m.mu.RUnlock()
	if terminate == nil {
		terminate = signalSelfTerminate
	}
	return terminate()
}

func signalSelfTerminate() error {
	return syscall.Kill(os.Getpid(), syscall.SIGTERM)
}

// launchd がこのプロセスを管理しているかを返す。--foreground のプロセスは自己 kickstart しない。
// そうすると置換側がソケットロックを失って終了し、手動プロセスだけが残る。
func (m *Manager) underLaunchd() bool {
	m.mu.RLock()
	managed := m.launchdManaged
	m.mu.RUnlock()
	if managed == nil {
		managed = launchdManagedProcess
	}
	return managed()
}

func launchdManagedProcess() bool {
	return os.Getppid() == 1 && os.Getenv("XPC_SERVICE_NAME") == launchd.Label
}

func (m *Manager) warnRestartUnmanaged() {
	m.mu.Lock()
	warned := m.restartUnmanaged
	m.restartUnmanaged = true
	path := m.executablePath
	m.mu.Unlock()
	if warned {
		return
	}
	m.log.Warn("daemon executable was replaced but this daemon is not managed by launchd; restart it manually", "path", path)
}

// beginRequest と endRequest は全 RPC を囲み、RPC サーバーが管理しない handler 数を記録する。
// lifecycle 要求も接続保護のため数えるが、報告する inflight からは除いて利用者操作だけを示す。
func (m *Manager) beginRequest(lifecycle bool) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.inflightRequests++
	if lifecycle {
		m.inflightLifecycle++
	}
	m.mu.Unlock()
}

func (m *Manager) endRequest(lifecycle bool) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.inflightRequests--
	if lifecycle {
		m.inflightLifecycle--
		// まだ応答を書き込んでいないため、ゲートを閉じ始める時刻を記録する。
		m.lastLifecycleEnd = time.Now()
	}
	m.mu.Unlock()
}
