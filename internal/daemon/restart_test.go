package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/HappyOnigiri/WX/internal/config"
	"github.com/HappyOnigiri/WX/internal/state"
)

// signalLog records the lifecycle path's launchctl calls and stop signals.
// Both run on their own goroutine (neither must block the manager's
// WaitGroup), so the counter is shared state and the assertions below have to
// wait for it rather than read it straight after the call that scheduled it.
type signalLog struct {
	mu  sync.Mutex
	n   int
	err error
}

func (k *signalLog) record() error {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.n++
	return k.err
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

func (k *signalLog) want(t *testing.T, n int) {
	t.Helper()
	waitFor(t, fmt.Sprintf("%d call(s)", n), func() bool { return k.count() >= n })
	if got := k.count(); got != n {
		t.Fatalf("recorded calls=%d, want %d", got, n)
	}
}

// stayAt fails if another call is issued. The grace period is what makes
// it meaningful: the call that would have issued one has already returned.
func (k *signalLog) stayAt(t *testing.T, n int) {
	t.Helper()
	time.Sleep(50 * time.Millisecond)
	if got := k.count(); got != n {
		t.Fatalf("recorded calls=%d, want it to stay at %d", got, n)
	}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func lifecycleActionClaimed(m *Manager) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.lifecycleClaimed
}

// restartFixture builds a manager that watches a stand-in executable inside a
// temporary directory, so replacement can be simulated without touching the
// test binary itself.
func restartFixture(t *testing.T) (*Manager, string, *signalLog) {
	t.Helper()
	root := t.TempDir()
	cfg := config.Defaults()
	cfg.Storage.WorktreeRoot = filepath.Join(root, "worktrees")
	store, err := state.Open(filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	manager := testManager(t, cfg, store)
	manager.log = slog.New(slog.NewTextHandler(io.Discard, nil))
	manager.launchdManaged = func() bool { return true }
	kickstarts := &signalLog{}
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

// stopFixture is restartFixture with the SIGTERM seam captured too, so a stop
// can be driven to completion without ending the test binary.
func stopFixture(t *testing.T) (*Manager, *signalLog) {
	t.Helper()
	manager, _, _ := restartFixture(t)
	stops := &signalLog{}
	manager.terminate = func() error { return stops.record() }
	return manager, stops
}

// replaceExecutable renames a new file over the pathname, which is what
// install(1) does and what leaves the running process on the old inode.
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
	kickstarts.stayAt(t, 0)
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
	// The restart is requested exactly once: cmd/wx stops honouring SIGTERM
	// after the first one, so a repeat would only kill the replacement.
	manager.runPendingLifecycle()
	kickstarts.stayAt(t, 1)
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
	kickstarts.stayAt(t, 0)
	if _, err := manager.store.ClaimJob(ctx, job.ID, "restart-test"); err != nil {
		t.Fatal(err)
	}
	manager.runPendingLifecycle()
	kickstarts.stayAt(t, 0)
	if err := manager.store.FinishJob(ctx, job.ID, "restart-test", nil); err != nil {
		t.Fatal(err)
	}
	manager.beginRequest()
	manager.runPendingLifecycle()
	kickstarts.stayAt(t, 0)
	manager.endRequest()
	manager.runPendingLifecycle()
	kickstarts.stayAt(t, 0)
	// One wx invocation is several RPCs with idle moments between them, so the
	// gate only opens once the daemon has stayed idle for lifecycleQuietPeriod.
	manager.mu.Lock()
	manager.lastRequestEnd = time.Now().Add(-lifecycleQuietPeriod)
	manager.mu.Unlock()
	manager.runPendingLifecycle()
	kickstarts.want(t, 1)
}

func TestRequestedRestartStillWaitsForTheIdleGate(t *testing.T) {
	manager, _, kickstarts := restartFixture(t)
	manager.beginRequest()
	manager.RequestRestart(context.Background())
	if !manager.restartPending {
		t.Fatal("an explicit request did not raise the pending restart")
	}
	manager.runPendingLifecycle()
	kickstarts.stayAt(t, 0)
	manager.endRequest()
	manager.runPendingLifecycle()
	kickstarts.stayAt(t, 0)
	manager.mu.Lock()
	manager.lastRequestEnd = time.Now().Add(-lifecycleQuietPeriod)
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
	kickstarts.stayAt(t, 0)
	if !manager.restartPending || manager.lifecycleClaimed {
		t.Fatalf("restart state pending=%v claimed=%v", manager.restartPending, manager.lifecycleClaimed)
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
	kickstarts.stayAt(t, 0)
}

func TestRestartAccountingBracketsEveryHandledRequest(t *testing.T) {
	manager, _, _ := restartFixture(t)
	if _, err := (Handler{Manager: manager}).Handle(context.Background(), "unknown", json.RawMessage(nil)); err == nil {
		t.Fatal("unknown method succeeded")
	}
	if manager.inflightRequests != 0 {
		t.Fatalf("inflightRequests=%d after a failed dispatch", manager.inflightRequests)
	}
	// lastRequestEnd is stamped only when the count reaches zero from above, so
	// it stays unset if Handler.Handle loses either half of the bracket:
	// without beginRequest the count goes to -1, without endRequest it stays at
	// one, and without both it never moves.
	if manager.lastRequestEnd.IsZero() {
		t.Fatal("a dispatched request was not counted as in-flight")
	}
}

func TestHandledRequestHoldsOffAPendingRestart(t *testing.T) {
	manager, executable, kickstarts := restartFixture(t)
	replaceExecutable(t, executable)
	manager.detectExecutableReplacement()
	if _, err := (Handler{Manager: manager}).Handle(context.Background(), "unknown", json.RawMessage(nil)); err == nil {
		t.Fatal("unknown method succeeded")
	}
	manager.runPendingLifecycle()
	kickstarts.stayAt(t, 0)
	manager.mu.Lock()
	manager.lastRequestEnd = time.Now().Add(-lifecycleQuietPeriod)
	manager.mu.Unlock()
	manager.runPendingLifecycle()
	kickstarts.want(t, 1)
}

func TestKickstartServiceFailureIsRetriedUpToTheAttemptLimit(t *testing.T) {
	manager, executable, kickstarts := restartFixture(t)
	kickstarts.failWith(context.DeadlineExceeded)
	replaceExecutable(t, executable)
	manager.detectExecutableReplacement()
	// A kickstart that failed delivered no SIGTERM, so the daemon is still on
	// the replaced binary and the claim has to come back for the next check.
	for attempt := 1; attempt <= maxLifecycleAttempts; attempt++ {
		manager.runPendingLifecycle()
		kickstarts.want(t, attempt)
		claimed := attempt == maxLifecycleAttempts
		waitFor(t, "the claim to settle", func() bool { return lifecycleActionClaimed(manager) == claimed })
	}
	manager.runPendingLifecycle()
	kickstarts.stayAt(t, maxLifecycleAttempts)
}

// TestAnExhaustedRestartDoesNotParkALaterStop is the reason the attempt budget
// is cleared by an explicit request: the latch is shared by both intents, so a
// launchctl that failed its way to the limit would otherwise leave the daemon
// impossible to stop for the rest of its life.
func TestAnExhaustedRestartDoesNotParkALaterStop(t *testing.T) {
	manager, executable, kickstarts := restartFixture(t)
	stops := &signalLog{}
	manager.terminate = func() error { return stops.record() }
	kickstarts.failWith(context.DeadlineExceeded)
	replaceExecutable(t, executable)
	manager.detectExecutableReplacement()
	for attempt := 1; attempt <= maxLifecycleAttempts; attempt++ {
		manager.runPendingLifecycle()
		kickstarts.want(t, attempt)
	}
	waitFor(t, "the claim to latch", func() bool { return lifecycleActionClaimed(manager) })
	manager.RequestStop(context.Background())
	if lifecycleActionClaimed(manager) {
		t.Fatal("an explicit stop did not lift the latched claim")
	}
	manager.mu.Lock()
	manager.lastRequestEnd = time.Now().Add(-lifecycleQuietPeriod)
	manager.mu.Unlock()
	manager.runPendingLifecycle()
	stops.want(t, 1)
}

// TestANewRequestKeepsAClaimWhoseSignalWasDelivered is the other half: the
// claim that is not latched belongs to a signal already on its way, and
// clearing it would let a second kickstart -k kill the replacement launchd
// just started.
func TestANewRequestKeepsAClaimWhoseSignalWasDelivered(t *testing.T) {
	manager, stops := stopFixture(t)
	ctx := context.Background()
	manager.RequestStop(ctx)
	manager.mu.Lock()
	manager.lastRequestEnd = time.Now().Add(-lifecycleQuietPeriod)
	manager.mu.Unlock()
	manager.runPendingLifecycle()
	stops.want(t, 1)
	manager.RequestRestart(ctx)
	if !lifecycleActionClaimed(manager) {
		t.Fatal("a delivered signal's claim was released by a later request")
	}
	manager.runPendingLifecycle()
	stops.stayAt(t, 1)
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
	manager.beginRequest()
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
	// In flight, then a queued job, then the quiet period: each of the gate's
	// conditions holds the stop back on its own.
	manager.runPendingLifecycle()
	stops.stayAt(t, 0)
	manager.endRequest()
	manager.mu.Lock()
	manager.lastRequestEnd = time.Now().Add(-lifecycleQuietPeriod)
	manager.mu.Unlock()
	manager.runPendingLifecycle()
	stops.stayAt(t, 0)
	if _, err := manager.store.ClaimJob(ctx, job.ID, "stop-test"); err != nil {
		t.Fatal(err)
	}
	if err := manager.store.FinishJob(ctx, job.ID, "stop-test", nil); err != nil {
		t.Fatal(err)
	}
	manager.mu.Lock()
	manager.lastRequestEnd = time.Now()
	manager.mu.Unlock()
	manager.runPendingLifecycle()
	stops.stayAt(t, 0)
	manager.mu.Lock()
	manager.lastRequestEnd = time.Now().Add(-lifecycleQuietPeriod)
	manager.mu.Unlock()
	manager.runPendingLifecycle()
	stops.want(t, 1)
	// The daemon honours only the first SIGTERM, so the claim is permanent.
	manager.runPendingLifecycle()
	stops.stayAt(t, 1)
}

// TestStopDoesNotRequireLaunchd is the difference between the two intents: a
// daemon started by hand with wx daemon start --foreground has to be
// stoppable, and signalling this very process cannot start a second daemon the
// way a kickstart could.
func TestStopDoesNotRequireLaunchdButRestartStillDoes(t *testing.T) {
	manager, stops := stopFixture(t)
	manager.launchdManaged = func() bool { return false }
	manager.RequestStop(context.Background())
	manager.mu.Lock()
	manager.lastRequestEnd = time.Now().Add(-lifecycleQuietPeriod)
	manager.mu.Unlock()
	manager.runPendingLifecycle()
	stops.want(t, 1)

	restarting, kickstarts := stopFixture(t)
	restarting.launchdManaged = func() bool { return false }
	restarting.RequestRestart(context.Background())
	restarting.mu.Lock()
	restarting.lastRequestEnd = time.Now().Add(-lifecycleQuietPeriod)
	restarting.mu.Unlock()
	restarting.runPendingLifecycle()
	kickstarts.stayAt(t, 0)
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
	manager.mu.Lock()
	manager.lastRequestEnd = time.Now().Add(-lifecycleQuietPeriod)
	manager.mu.Unlock()
	manager.runPendingLifecycle()
	stops.want(t, 1)
	if manager.lifecyclePending() {
		t.Fatal("a claimed stop is still reported as pending")
	}
}

// TestARequestAfterADeliveredSignalIsRefusedAsAConflict keeps the reply
// honest: once a signal is on its way the opposite request cannot supersede
// it, so answering as if it had would have the caller wait out its whole
// budget for a state the daemon is not heading to.
func TestARequestAfterADeliveredSignalIsRefusedAsAConflict(t *testing.T) {
	manager, stops := stopFixture(t)
	ctx := context.Background()
	manager.RequestStop(ctx)
	manager.mu.Lock()
	manager.lastRequestEnd = time.Now().Add(-lifecycleQuietPeriod)
	manager.mu.Unlock()
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

// TestAStaleIntentIsNotIssuedAfterTheJobQuery covers the window the job query
// opens: the lock is released across it, so the intent read before it can have
// been replaced by the time the action would be claimed.
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

// TestStartRequestCallsBackAStopThatWasNeverIssued closes the gap a stop that
// outlived its caller leaves behind: the intent stays pending, and a later
// start that only looked at the socket would report a running daemon that is
// still going to exit.
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
	manager.mu.Lock()
	manager.lastRequestEnd = time.Now().Add(-lifecycleQuietPeriod)
	manager.mu.Unlock()
	manager.runPendingLifecycle()
	stops.stayAt(t, 0)
}

// TestStartRequestCannotCallBackADeliveredStop is the other half: the SIGTERM
// is already on its way, so the reply has to say the daemon is still stopping
// rather than claim it is running.
func TestStartRequestCannotCallBackADeliveredStop(t *testing.T) {
	manager, stops := stopFixture(t)
	ctx := context.Background()
	manager.RequestStop(ctx)
	manager.mu.Lock()
	manager.lastRequestEnd = time.Now().Add(-lifecycleQuietPeriod)
	manager.mu.Unlock()
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

// TestPendingStopSuppressesReplacementDetection guards the one way an operator
// who asked for a stop could get a restart instead: the executable watch fires
// on the same tick and would otherwise raise restartPending over the stop.
func TestPendingStopSuppressesReplacementDetection(t *testing.T) {
	manager, stops := stopFixture(t)
	executable := manager.executablePath
	manager.RequestStop(context.Background())
	replaceExecutable(t, executable)
	manager.detectExecutableReplacement()
	if manager.restartPending {
		t.Fatal("a replaced executable overrode the pending stop")
	}
	manager.mu.Lock()
	manager.lastRequestEnd = time.Now().Add(-lifecycleQuietPeriod)
	manager.mu.Unlock()
	manager.runPendingLifecycle()
	stops.want(t, 1)
}

func TestLifecycleReplyReportsWhatTheGateIsWaitingFor(t *testing.T) {
	manager, _ := stopFixture(t)
	ctx := context.Background()
	if _, err := manager.store.CreateJob(ctx, "ENSURE_STANDBY", "", "", ""); err != nil {
		t.Fatal(err)
	}
	// One in-flight request stands for the lifecycle RPC itself, which the
	// snapshot must not count against the daemon it is describing.
	manager.beginRequest()
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
	manager.endRequest()
	repeat := manager.RequestStop(ctx)
	if already, _ := repeat["already_pending"].(bool); !already {
		t.Fatal("the second stop request did not report the standing one")
	}
	if remaining, _ := repeat["quiet_period_remaining_ms"].(int64); remaining <= 0 {
		t.Fatalf("quiet_period_remaining_ms=%v, want the period the request just restarted", repeat["quiet_period_remaining_ms"])
	}
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
	manager.mu.Lock()
	manager.lastRequestEnd = time.Now().Add(-lifecycleQuietPeriod)
	manager.mu.Unlock()
	// A signal that was never delivered leaves the daemon running, so the claim
	// has to come back for the next check until the attempt limit is reached.
	for attempt := 1; attempt <= maxLifecycleAttempts; attempt++ {
		manager.runPendingLifecycle()
		stops.want(t, attempt)
		claimed := attempt == maxLifecycleAttempts
		waitFor(t, "the claim to settle", func() bool { return lifecycleActionClaimed(manager) == claimed })
	}
	manager.runPendingLifecycle()
	stops.stayAt(t, maxLifecycleAttempts)
}

// TestTerminateSelfSignalsThisProcess exercises the production seam. Notifying
// on SIGTERM first is what keeps the default disposition (terminate the test
// binary) from taking effect.
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
