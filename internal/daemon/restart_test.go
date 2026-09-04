package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/HappyOnigiri/WX/internal/config"
	"github.com/HappyOnigiri/WX/internal/state"
)

// kickstartLog records the restart path's launchctl calls. The kickstart runs
// on its own goroutine (it must not block the manager's WaitGroup), so the
// counter is shared state and the assertions below have to wait for it rather
// than read it straight after the call that scheduled it.
type kickstartLog struct {
	mu  sync.Mutex
	n   int
	err error
}

func (k *kickstartLog) record() error {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.n++
	return k.err
}

func (k *kickstartLog) count() int {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.n
}

func (k *kickstartLog) failWith(err error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.err = err
}

func (k *kickstartLog) want(t *testing.T, n int) {
	t.Helper()
	waitFor(t, fmt.Sprintf("%d kickstart(s)", n), func() bool { return k.count() >= n })
	if got := k.count(); got != n {
		t.Fatalf("kickstarts=%d, want %d", got, n)
	}
}

// stayAt fails if another kickstart is issued. The grace period is what makes
// it meaningful: the call that would have issued one has already returned.
func (k *kickstartLog) stayAt(t *testing.T, n int) {
	t.Helper()
	time.Sleep(50 * time.Millisecond)
	if got := k.count(); got != n {
		t.Fatalf("kickstarts=%d, want it to stay at %d", got, n)
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

func restartClaimed(m *Manager) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.restartRequested
}

// restartFixture builds a manager that watches a stand-in executable inside a
// temporary directory, so replacement can be simulated without touching the
// test binary itself.
func restartFixture(t *testing.T) (*Manager, string, *kickstartLog) {
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
	kickstarts := &kickstartLog{}
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
	manager.restartIfReplaced()
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
	manager.restartIfReplaced()
	kickstarts.want(t, 1)
	// The restart is requested exactly once: cmd/wx stops honouring SIGTERM
	// after the first one, so a repeat would only kill the replacement.
	manager.restartIfReplaced()
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
	manager.restartIfReplaced()
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
	manager.restartIfReplaced()
	kickstarts.stayAt(t, 0)
	if _, err := manager.store.ClaimJob(ctx, job.ID, "restart-test"); err != nil {
		t.Fatal(err)
	}
	manager.restartIfReplaced()
	kickstarts.stayAt(t, 0)
	if err := manager.store.FinishJob(ctx, job.ID, "restart-test", nil); err != nil {
		t.Fatal(err)
	}
	manager.beginRequest()
	manager.restartIfReplaced()
	kickstarts.stayAt(t, 0)
	manager.endRequest()
	manager.restartIfReplaced()
	kickstarts.stayAt(t, 0)
	// One wx invocation is several RPCs with idle moments between them, so the
	// gate only opens once the daemon has stayed idle for restartQuietPeriod.
	manager.mu.Lock()
	manager.lastRequestEnd = time.Now().Add(-restartQuietPeriod)
	manager.mu.Unlock()
	manager.restartIfReplaced()
	kickstarts.want(t, 1)
}

func TestRequestedRestartStillWaitsForTheIdleGate(t *testing.T) {
	manager, _, kickstarts := restartFixture(t)
	manager.beginRequest()
	manager.RequestRestart()
	if !manager.restartPending {
		t.Fatal("an explicit request did not raise the pending restart")
	}
	manager.restartIfReplaced()
	kickstarts.stayAt(t, 0)
	manager.endRequest()
	manager.restartIfReplaced()
	kickstarts.stayAt(t, 0)
	manager.mu.Lock()
	manager.lastRequestEnd = time.Now().Add(-restartQuietPeriod)
	manager.mu.Unlock()
	manager.restartIfReplaced()
	kickstarts.want(t, 1)
}

func TestUnmanagedDaemonKeepsThePendingRestart(t *testing.T) {
	manager, executable, kickstarts := restartFixture(t)
	manager.launchdManaged = func() bool { return false }
	replaceExecutable(t, executable)
	manager.detectExecutableReplacement()
	manager.restartIfReplaced()
	manager.restartIfReplaced()
	kickstarts.stayAt(t, 0)
	if !manager.restartPending || manager.restartRequested {
		t.Fatalf("restart state pending=%v requested=%v", manager.restartPending, manager.restartRequested)
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
	manager.restartIfReplaced()
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
	manager.restartIfReplaced()
	kickstarts.stayAt(t, 0)
	manager.mu.Lock()
	manager.lastRequestEnd = time.Now().Add(-restartQuietPeriod)
	manager.mu.Unlock()
	manager.restartIfReplaced()
	kickstarts.want(t, 1)
}

func TestKickstartServiceFailureIsRetriedUpToTheAttemptLimit(t *testing.T) {
	manager, executable, kickstarts := restartFixture(t)
	kickstarts.failWith(context.DeadlineExceeded)
	replaceExecutable(t, executable)
	manager.detectExecutableReplacement()
	// A kickstart that failed delivered no SIGTERM, so the daemon is still on
	// the replaced binary and the claim has to come back for the next check.
	for attempt := 1; attempt <= maxRestartAttempts; attempt++ {
		manager.restartIfReplaced()
		kickstarts.want(t, attempt)
		claimed := attempt == maxRestartAttempts
		waitFor(t, "the claim to settle", func() bool { return restartClaimed(manager) == claimed })
	}
	manager.restartIfReplaced()
	kickstarts.stayAt(t, maxRestartAttempts)
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
