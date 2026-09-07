package daemon

import (
	"context"
	"encoding/json"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"
)

// lifecycleSignalBudget は goroutine 越しに届くシグナル発行と claim 更新を待つ上限。
// 待つのはこの 2 つだけで、ゲートの評価自体は runPendingLifecycle の復帰で確定する。
const lifecycleSignalBudget = 2 * time.Second

// signalLog は kickstart・停止シグナルの呼び出しを数え、到達を待ち手へ通知する。
// 通知は容量 1 の channel で coalesce するため、待ち始める前の呼び出しも落とさない。
// 通知だけを真実にはせず、件数は常に mutex 下で読み直す。
type signalLog struct {
	mu       sync.Mutex
	n        int
	err      error
	recorded chan struct{}
}

func newSignalLog() *signalLog {
	return &signalLog{recorded: make(chan struct{}, 1)}
}

func (k *signalLog) record() error {
	k.mu.Lock()
	k.n++
	err := k.err
	k.mu.Unlock()
	select {
	case k.recorded <- struct{}{}:
	default:
	}
	return err
}

func (k *signalLog) count() int {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.n
}

func (k *signalLog) failWith(err error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.err = err
}

// want は n 件目の呼び出しの到達を通知で待ち、件数が n と一致することを確かめる。
// シグナル送信は issueStop/issueRestart の goroutine で起きるため、ここだけは期限付きで待つ。
func (k *signalLog) want(t *testing.T, n int) {
	t.Helper()
	timer := time.NewTimer(lifecycleSignalBudget)
	defer timer.Stop()
	for {
		if got := k.count(); got >= n {
			if got != n {
				t.Fatalf("recorded calls=%d, want %d", got, n)
			}
			return
		}
		select {
		case <-k.recorded:
		case <-timer.C:
			t.Fatalf("timed out waiting for %d call(s); recorded calls=%d", n, k.count())
		}
	}
}

// at は呼び出し件数が n のままであることを同期的に確かめる。
// runPendingLifecycle はゲートと claim を同期評価し、発行する場合だけ goroutine を起こすため、
// 復帰時点で「まだ発行していない」ことが確定しており待つ必要がない。
func (k *signalLog) at(t *testing.T, n int) {
	t.Helper()
	if got := k.count(); got != n {
		t.Fatalf("recorded calls=%d, want it to stay at %d", got, n)
	}
}

// wantGateHeldClosed は閉じたゲートの評価結果を確かめる。
// claim を取らず件数も動かないことが、意図を保留したまま発行を見送った根拠になる。
func wantGateHeldClosed(t *testing.T, m *Manager, k *signalLog, n int) {
	t.Helper()
	if lifecycleActionClaimed(m) {
		t.Fatal("a closed lifecycle gate still claimed the action")
	}
	k.at(t, n)
}

// wantClaimHeld は配送済み claim が保持され、再評価が同じシグナルを繰り返さないことを確かめる。
func wantClaimHeld(t *testing.T, m *Manager, k *signalLog, n int) {
	t.Helper()
	if !lifecycleActionClaimed(m) {
		t.Fatal("a delivered signal's claim was not retained")
	}
	k.at(t, n)
}

// waitForClaim は claim の解除・確定を待つ。issueStop/issueRestart は record の後に claim を動かすため、
// 呼び出し件数の到達を解除完了と扱えない。期限切れでは attempts・claim・件数を診断として出す。
func waitForClaim(t *testing.T, m *Manager, k *signalLog, want bool) {
	t.Helper()
	for deadline := time.Now().Add(lifecycleSignalBudget); time.Now().Before(deadline); {
		if lifecycleActionClaimed(m) == want {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	m.mu.RLock()
	attempts, claimed := m.lifecycleAttempts, m.lifecycleClaimed
	m.mu.RUnlock()
	t.Fatalf("timed out waiting for claimed=%v; claimed=%v attempts=%d recorded calls=%d", want, claimed, attempts, k.count())
}

func lifecycleActionClaimed(m *Manager) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.lifecycleClaimed
}

// restartFixture は再起動・停止の検査向けに、手動 Manager を launchd 管理下に見せる目的別のアダプタである。
// 基礎の準備は manualManagerFixture に任せ、ここでは signal と実行ファイル監視の差分だけを組む。
func restartFixture(t *testing.T) (*Manager, string, *signalLog) {
	t.Helper()
	f := manualManagerFixture(t)
	root, manager := f.Root, f.Manager
	manager.launchdManaged = func() bool { return true }
	kickstarts := newSignalLog()
	manager.kickstart = func(context.Context) error { return kickstarts.record() }
	executable := filepath.Join(root, "wx")
	if err := os.WriteFile(executable, []byte("original"), 0o700); err != nil {
		t.Fatal(err)
	}
	manager.watchExecutable(executable, nil)
	if !manager.executableWatch {
		t.Fatal("executable watch was not armed")
	}
	return manager, executable, kickstarts
}

func stopFixture(t *testing.T) (*Manager, *signalLog) {
	t.Helper()
	manager, _, _ := restartFixture(t)
	stops := newSignalLog()
	manager.terminate = func() error { return stops.record() }
	return manager, stops
}

func replaceExecutable(t *testing.T, path string) {
	t.Helper()
	replacement := path + ".new"
	if err := os.WriteFile(replacement, []byte("replacement build"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacement, path); err != nil {
		t.Fatal(err)
	}
}

func TestUnchangedExecutableNeverRestartsTheDaemon(t *testing.T) {
	manager, _, kickstarts := restartFixture(t)
	manager.detectExecutableReplacement()
	manager.runPendingLifecycle()
	if manager.restartPending {
		t.Fatal("unchanged executable raised a pending restart")
	}
	wantGateHeldClosed(t, manager, kickstarts, 0)
}

func TestReplacedExecutableRestartsWhenIdle(t *testing.T) {
	manager, executable, kickstarts := restartFixture(t)
	replaceExecutable(t, executable)
	manager.detectExecutableReplacement()
	if !manager.restartPending {
		t.Fatal("replaced executable did not raise a pending restart")
	}
	status, err := manager.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if pending, _ := status["restart_pending"].(bool); !pending {
		t.Fatalf("status does not report the pending restart: %v", status["restart_pending"])
	}
	manager.runPendingLifecycle()
	kickstarts.want(t, 1)
	manager.runPendingLifecycle()
	wantClaimHeld(t, manager, kickstarts, 1)
}

func TestMissingExecutablePathDefersInsteadOfRestarting(t *testing.T) {
	manager, executable, kickstarts := restartFixture(t)
	if err := os.Remove(executable); err != nil {
		t.Fatal(err)
	}
	manager.detectExecutableReplacement()
	if manager.restartPending {
		t.Fatal("a pathname that momentarily does not exist raised a pending restart")
	}
	replaceExecutable(t, executable)
	manager.detectExecutableReplacement()
	manager.runPendingLifecycle()
	kickstarts.want(t, 1)
}

func TestPendingRestartWaitsForJobsAndRequests(t *testing.T) {
	manager, executable, kickstarts := restartFixture(t)
	ctx := context.Background()
	job, err := manager.store.CreateJob(ctx, "ENSURE_STANDBY", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	replaceExecutable(t, executable)
	manager.detectExecutableReplacement()
	manager.runPendingLifecycle()
	wantGateHeldClosed(t, manager, kickstarts, 0)
	if _, err := manager.store.ClaimJob(ctx, job.ID, "restart-test"); err != nil {
		t.Fatal(err)
	}
	manager.runPendingLifecycle()
	wantGateHeldClosed(t, manager, kickstarts, 0)
	if err := manager.store.FinishJob(ctx, job.ID, "restart-test", nil); err != nil {
		t.Fatal(err)
	}
	manager.beginRequest(false)
	manager.runPendingLifecycle()
	wantGateHeldClosed(t, manager, kickstarts, 0)
	manager.endRequest(false)
	manager.runPendingLifecycle()
	kickstarts.want(t, 1)
}

// 実行中の要求が捌けた時点で発行し、次の要求が来るかどうかは待たないことを固定する。
func TestAPendingRestartIssuesAsSoonAsTheLastRequestEnds(t *testing.T) {
	manager, executable, kickstarts := restartFixture(t)
	replaceExecutable(t, executable)
	manager.detectExecutableReplacement()
	manager.beginRequest(false)
	manager.runPendingLifecycle()
	wantGateHeldClosed(t, manager, kickstarts, 0)
	manager.endRequest(false)
	manager.runPendingLifecycle()
	kickstarts.want(t, 1)
}

func elapseLifecycleGate(m *Manager) {
	m.mu.Lock()
	m.lastLifecycleEnd = time.Now().Add(-lifecycleReplyGrace)
	m.mu.Unlock()
}

func TestRequestedRestartStillWaitsForTheIdleGate(t *testing.T) {
	manager, _, kickstarts := restartFixture(t)
	manager.beginRequest(false)
	manager.beginRequest(true)
	manager.RequestRestart(context.Background())
	if !manager.restartPending {
		t.Fatal("an explicit request did not raise the pending restart")
	}
	manager.runPendingLifecycle()
	wantGateHeldClosed(t, manager, kickstarts, 0)
	manager.endRequest(true)
	manager.runPendingLifecycle()
	wantGateHeldClosed(t, manager, kickstarts, 0)
	manager.endRequest(false)
	manager.runPendingLifecycle()
	wantGateHeldClosed(t, manager, kickstarts, 0)
	elapseLifecycleGate(manager)
	manager.runPendingLifecycle()
	kickstarts.want(t, 1)
}

func TestLifecycleRequestWaitsUntilItsReplyIsDue(t *testing.T) {
	manager, _, kickstarts := restartFixture(t)
	handler := Handler{Manager: manager}
	if _, err := handler.Handle(context.Background(), "RequestRestart", nil); err != nil {
		t.Fatal(err)
	}
	manager.mu.Lock()
	lastLifecycle := manager.lastLifecycleEnd
	manager.mu.Unlock()
	if lastLifecycle.IsZero() {
		t.Fatal("the lifecycle request did not record when its reply became due")
	}
	manager.runPendingLifecycle()
	wantGateHeldClosed(t, manager, kickstarts, 0)
	manager.mu.Lock()
	manager.lastLifecycleEnd = time.Now().Add(-lifecycleReplyGrace)
	manager.mu.Unlock()
	manager.runPendingLifecycle()
	kickstarts.want(t, 1)
}

func TestUnmanagedDaemonKeepsThePendingRestart(t *testing.T) {
	manager, executable, kickstarts := restartFixture(t)
	manager.launchdManaged = func() bool { return false }
	replaceExecutable(t, executable)
	manager.detectExecutableReplacement()
	manager.runPendingLifecycle()
	manager.runPendingLifecycle()
	wantGateHeldClosed(t, manager, kickstarts, 0)
	if !manager.restartPending {
		t.Fatal("the unmanaged daemon lowered the pending restart")
	}
	if !manager.restartUnmanaged {
		t.Fatal("the unmanaged daemon warning was not recorded")
	}
}

func TestExecutableWatchStaysDisabledWithoutABaseline(t *testing.T) {
	manager, executable, kickstarts := restartFixture(t)
	manager.executableWatch = false
	manager.watchExecutable(filepath.Join(filepath.Dir(executable), "absent"), nil)
	if manager.executableWatch {
		t.Fatal("watch was armed without a baseline")
	}
	manager.watchExecutable("", os.ErrPermission)
	if manager.executableWatch {
		t.Fatal("watch was armed after os.Executable failed")
	}
	manager.detectExecutableReplacement()
	manager.runPendingLifecycle()
	if manager.restartPending {
		t.Fatalf("disabled watch raised a pending restart")
	}
	wantGateHeldClosed(t, manager, kickstarts, 0)
}

func TestRestartAccountingBracketsEveryHandledRequest(t *testing.T) {
	manager, _, _ := restartFixture(t)
	handler := Handler{Manager: manager}
	if _, err := handler.Handle(context.Background(), "unknown", json.RawMessage(nil)); err == nil {
		t.Fatal("unknown method succeeded")
	}
	if manager.inflightRequests != 0 {
		t.Fatalf("inflightRequests=%d after a failed dispatch", manager.inflightRequests)
	}
	// lifecycle 経路が bracket を通ったことは、応答待ちの記録が残ることで確かめる。
	if _, err := handler.Handle(context.Background(), "RequestRestart", nil); err != nil {
		t.Fatal(err)
	}
	if manager.inflightRequests != 0 {
		t.Fatalf("inflightRequests=%d after a lifecycle dispatch", manager.inflightRequests)
	}
	if manager.lastLifecycleEnd.IsZero() {
		t.Fatal("a dispatched request was not counted as in-flight")
	}
}

func TestKickstartServiceFailureIsRetriedUpToTheAttemptLimit(t *testing.T) {
	manager, executable, kickstarts := restartFixture(t)
	kickstarts.failWith(context.DeadlineExceeded)
	replaceExecutable(t, executable)
	manager.detectExecutableReplacement()
	for attempt := 1; attempt <= maxLifecycleAttempts; attempt++ {
		manager.runPendingLifecycle()
		kickstarts.want(t, attempt)
		claimed := attempt == maxLifecycleAttempts
		waitForClaim(t, manager, kickstarts, claimed)
	}
	manager.runPendingLifecycle()
	wantClaimHeld(t, manager, kickstarts, maxLifecycleAttempts)
}

func TestAnExhaustedRestartDoesNotParkALaterStop(t *testing.T) {
	manager, executable, kickstarts := restartFixture(t)
	stops := newSignalLog()
	manager.terminate = func() error { return stops.record() }
	kickstarts.failWith(context.DeadlineExceeded)
	replaceExecutable(t, executable)
	manager.detectExecutableReplacement()
	for attempt := 1; attempt <= maxLifecycleAttempts; attempt++ {
		manager.runPendingLifecycle()
		kickstarts.want(t, attempt)
	}
	waitForClaim(t, manager, kickstarts, true)
	manager.RequestStop(context.Background())
	if lifecycleActionClaimed(manager) {
		t.Fatal("an explicit stop did not lift the latched claim")
	}
	manager.runPendingLifecycle()
	stops.want(t, 1)
}

func TestANewRequestKeepsAClaimWhoseSignalWasDelivered(t *testing.T) {
	manager, stops := stopFixture(t)
	ctx := context.Background()
	manager.RequestStop(ctx)
	manager.runPendingLifecycle()
	stops.want(t, 1)
	manager.RequestRestart(ctx)
	if !lifecycleActionClaimed(manager) {
		t.Fatal("a delivered signal's claim was released by a later request")
	}
	manager.runPendingLifecycle()
	wantClaimHeld(t, manager, stops, 1)
}

func TestManagedProcessDetectionRejectsAnInteractiveDaemon(t *testing.T) {
	t.Setenv("XPC_SERVICE_NAME", "not-the-wx-label")
	if launchdManagedProcess() {
		t.Fatal("an interactively started daemon was reported as launchd-managed")
	}
	manager, _, _ := restartFixture(t)
	manager.launchdManaged = nil
	if manager.underLaunchd() {
		t.Fatal("the default detection accepted an interactive daemon")
	}
}

func TestDetectExecutableReplacementIsIdempotentOncePending(t *testing.T) {
	manager, executable, _ := restartFixture(t)
	replaceExecutable(t, executable)
	manager.detectExecutableReplacement()
	baseline := manager.executableBaseline
	replaceExecutable(t, executable)
	manager.detectExecutableReplacement()
	if manager.executableBaseline != baseline {
		t.Fatal("a second replacement moved the baseline")
	}
	if !manager.restartPending {
		t.Fatal("the pending restart was lowered")
	}
}

func TestSnapshotMatchesDistinguishesEveryTrackedAttribute(t *testing.T) {
	now := time.Now()
	base := executableSnapshot{identity: "1:2", modTime: now, size: 10}
	cases := map[string]executableSnapshot{
		"identity": {identity: "1:3", modTime: now, size: 10},
		"mtime":    {identity: "1:2", modTime: now.Add(time.Second), size: 10},
		"size":     {identity: "1:2", modTime: now, size: 11},
	}
	if !base.matches(base) {
		t.Fatal("identical snapshots did not match")
	}
	for name, other := range cases {
		if base.matches(other) {
			t.Fatalf("snapshot differing in %s matched", name)
		}
	}
}

func TestRequestedStopWaitsForTheSameIdleGateAsARestart(t *testing.T) {
	manager, stops := stopFixture(t)
	ctx := context.Background()
	job, err := manager.store.CreateJob(ctx, "ENSURE_STANDBY", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	manager.beginRequest(true)
	manager.RequestStop(ctx)
	if !manager.stopPending {
		t.Fatal("an explicit request did not raise the pending stop")
	}
	status, err := manager.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if pending, _ := status["stop_pending"].(bool); !pending {
		t.Fatalf("status does not report the pending stop: %v", status["stop_pending"])
	}
	manager.runPendingLifecycle()
	wantGateHeldClosed(t, manager, stops, 0)
	manager.endRequest(true)
	elapseLifecycleGate(manager)
	manager.runPendingLifecycle()
	wantGateHeldClosed(t, manager, stops, 0)
	if _, err := manager.store.ClaimJob(ctx, job.ID, "stop-test"); err != nil {
		t.Fatal(err)
	}
	if err := manager.store.FinishJob(ctx, job.ID, "stop-test", nil); err != nil {
		t.Fatal(err)
	}
	manager.runPendingLifecycle()
	stops.want(t, 1)
	manager.runPendingLifecycle()
	wantClaimHeld(t, manager, stops, 1)
}

func TestStopDoesNotRequireLaunchdButRestartStillDoes(t *testing.T) {
	manager, stops := stopFixture(t)
	manager.launchdManaged = func() bool { return false }
	manager.RequestStop(context.Background())
	manager.runPendingLifecycle()
	stops.want(t, 1)

	restarting, kickstarts := stopFixture(t)
	restarting.launchdManaged = func() bool { return false }
	restarting.RequestRestart(context.Background())
	restarting.runPendingLifecycle()
	wantGateHeldClosed(t, restarting, kickstarts, 0)
	if !restarting.restartUnmanaged {
		t.Fatal("the unmanaged daemon warning was not recorded for a requested restart")
	}
}

func TestEachLifecycleRequestSupersedesTheOther(t *testing.T) {
	manager, stops := stopFixture(t)
	ctx := context.Background()
	manager.RequestStop(ctx)
	manager.RequestRestart(ctx)
	if manager.stopPending || !manager.restartPending {
		t.Fatalf("restart did not supersede the stop: stop=%v restart=%v", manager.stopPending, manager.restartPending)
	}
	manager.RequestStop(ctx)
	if !manager.stopPending || manager.restartPending {
		t.Fatalf("stop did not supersede the restart: stop=%v restart=%v", manager.stopPending, manager.restartPending)
	}
	if !manager.lifecyclePending() {
		t.Fatal("a raised stop was not reported as pending")
	}
	manager.runPendingLifecycle()
	stops.want(t, 1)
	if manager.lifecyclePending() {
		t.Fatal("a claimed stop is still reported as pending")
	}
}

func TestARequestAfterADeliveredSignalIsRefusedAsAConflict(t *testing.T) {
	manager, stops := stopFixture(t)
	ctx := context.Background()
	manager.RequestStop(ctx)
	manager.runPendingLifecycle()
	stops.want(t, 1)
	reply := manager.RequestRestart(ctx)
	if conflict, _ := reply["conflict"].(bool); !conflict {
		t.Fatalf("the restart request was not refused as a conflict: %v", reply)
	}
	if stopping, _ := reply["stop_pending"].(bool); !stopping {
		t.Fatalf("the conflict did not name the stop under way: %v", reply)
	}
	manager.mu.RLock()
	restarting := manager.restartPending
	manager.mu.RUnlock()
	if restarting {
		t.Fatal("a refused request still raised its intent")
	}
}

func TestAStaleIntentIsNotIssuedAfterTheJobQuery(t *testing.T) {
	manager, _ := stopFixture(t)
	manager.RequestStop(context.Background())
	manager.mu.RLock()
	defer manager.mu.RUnlock()
	if !manager.lifecycleIntentUnchangedLocked(true, false) {
		t.Fatal("the raised stop was reported as changed")
	}
	if manager.lifecycleIntentUnchangedLocked(false, true) {
		t.Fatal("a restart evaluation was allowed to proceed against a pending stop")
	}
}

func TestStartRequestCallsBackAStopThatWasNeverIssued(t *testing.T) {
	manager, stops := stopFixture(t)
	ctx := context.Background()
	manager.RequestStop(ctx)
	reply := manager.RequestStart(ctx)
	if cancelled, _ := reply["stop_cancelled"].(bool); !cancelled {
		t.Fatalf("start did not report the cancelled stop: %v", reply)
	}
	if stopping, _ := reply["stop_pending"].(bool); stopping {
		t.Fatalf("start left the stop pending: %v", reply)
	}
	manager.runPendingLifecycle()
	wantGateHeldClosed(t, manager, stops, 0)
}

func TestStartRequestCannotCallBackADeliveredStop(t *testing.T) {
	manager, stops := stopFixture(t)
	ctx := context.Background()
	manager.RequestStop(ctx)
	manager.runPendingLifecycle()
	stops.want(t, 1)
	reply := manager.RequestStart(ctx)
	if cancelled, _ := reply["stop_cancelled"].(bool); cancelled {
		t.Fatalf("start claimed to have cancelled a delivered stop: %v", reply)
	}
	if stopping, _ := reply["stop_pending"].(bool); !stopping {
		t.Fatalf("start did not report the daemon as stopping: %v", reply)
	}
}

func TestReplacementDetectionRechecksTheIntentBeforeRaisingIt(t *testing.T) {
	manager, _ := stopFixture(t)
	executable := manager.executablePath
	replaceExecutable(t, executable)
	manager.mu.Lock()
	manager.stopPending = true
	manager.mu.Unlock()
	manager.detectExecutableReplacement()
	manager.mu.RLock()
	stopping, restarting := manager.stopPending, manager.restartPending
	manager.mu.RUnlock()
	if restarting {
		t.Fatal("the detection raised a restart on top of a pending stop")
	}
	if !stopping {
		t.Fatal("the detection lowered the pending stop")
	}
}

func TestPendingStopSuppressesReplacementDetection(t *testing.T) {
	manager, stops := stopFixture(t)
	executable := manager.executablePath
	manager.RequestStop(context.Background())
	replaceExecutable(t, executable)
	manager.detectExecutableReplacement()
	if manager.restartPending {
		t.Fatal("a replaced executable overrode the pending stop")
	}
	manager.runPendingLifecycle()
	stops.want(t, 1)
}

func TestLifecycleReplyReportsWhatTheGateIsWaitingFor(t *testing.T) {
	manager, _ := stopFixture(t)
	ctx := context.Background()
	if _, err := manager.store.CreateJob(ctx, "ENSURE_STANDBY", "", "", ""); err != nil {
		t.Fatal(err)
	}
	manager.beginRequest(true)
	reply := manager.RequestStop(ctx)
	if pid, _ := reply["pid"].(int); pid != os.Getpid() {
		t.Fatalf("reply pid=%v, want %d", reply["pid"], os.Getpid())
	}
	if inflight, _ := reply["inflight_requests"].(int); inflight != 0 {
		t.Fatalf("reply inflight_requests=%v, want the lifecycle RPC itself to be excluded", reply["inflight_requests"])
	}
	if jobs, _ := reply["queued_jobs"].(int); jobs != 1 {
		t.Fatalf("reply queued_jobs=%v, want 1", reply["queued_jobs"])
	}
	if already, _ := reply["already_pending"].(bool); already {
		t.Fatal("the first stop request reported itself as already pending")
	}
	manager.endRequest(true)
	repeat := manager.RequestStop(ctx)
	if already, _ := repeat["already_pending"].(bool); !already {
		t.Fatal("the second stop request did not report the standing one")
	}
	manager.beginRequest(false)
	served := manager.RequestStop(ctx)
	if inflight, _ := served["inflight_requests"].(int); inflight != 1 {
		t.Fatalf("reply inflight_requests=%v, want the served request to be named", served["inflight_requests"])
	}
	manager.endRequest(false)
	restart := manager.RequestRestart(ctx)
	if pending, _ := restart["restart_pending"].(bool); !pending {
		t.Fatalf("restart reply=%v", restart)
	}
	if _, ok := restart["queued_jobs"]; !ok {
		t.Fatalf("the restart reply carries no gate snapshot: %v", restart)
	}
}

func TestStopSignalFailureIsRetriedUpToTheAttemptLimit(t *testing.T) {
	manager, stops := stopFixture(t)
	stops.failWith(os.ErrPermission)
	manager.RequestStop(context.Background())
	for attempt := 1; attempt <= maxLifecycleAttempts; attempt++ {
		manager.runPendingLifecycle()
		stops.want(t, attempt)
		claimed := attempt == maxLifecycleAttempts
		waitForClaim(t, manager, stops, claimed)
	}
	manager.runPendingLifecycle()
	wantClaimHeld(t, manager, stops, maxLifecycleAttempts)
}

func TestTerminateSelfSignalsThisProcess(t *testing.T) {
	received := make(chan os.Signal, 1)
	signal.Notify(received, syscall.SIGTERM)
	defer signal.Stop(received)
	manager, _, _ := restartFixture(t)
	manager.terminate = nil
	if err := manager.terminateSelf(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-received:
	case <-time.After(2 * time.Second):
		t.Fatal("SIGTERM was not delivered to this process")
	}
}

func TestLifecycleCheckNotificationNeverBlocks(t *testing.T) {
	manager, _ := stopFixture(t)
	manager.lifecycleChecks = make(chan struct{}, 1)
	for i := 0; i < 3; i++ {
		manager.notifyLifecycleCheck()
	}
	if len(manager.lifecycleChecks) != 1 {
		t.Fatalf("buffered notifications=%d, want the channel to coalesce them", len(manager.lifecycleChecks))
	}
}
