package daemon

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/HappyOnigiri/WX/internal/config"
	"github.com/HappyOnigiri/WX/internal/state"
)

// restartFixture builds a manager that watches a stand-in executable inside a
// temporary directory, so replacement can be simulated without touching the
// test binary itself.
func restartFixture(t *testing.T) (*Manager, string, *int) {
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
	kickstarts := 0
	manager.kickstart = func(context.Context) error {
		kickstarts++
		return nil
	}
	executable := filepath.Join(root, "wx")
	if err := os.WriteFile(executable, []byte("original"), 0o700); err != nil {
		t.Fatal(err)
	}
	manager.watchExecutable(executable, nil)
	if !manager.executableWatch {
		t.Fatal("executable watch was not armed")
	}
	return manager, executable, &kickstarts
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
	if *kickstarts != 0 {
		t.Fatalf("kickstarts=%d for an unchanged executable", *kickstarts)
	}
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
	if *kickstarts != 1 {
		t.Fatalf("kickstarts=%d, want 1", *kickstarts)
	}
	// The restart is requested exactly once: cmd/wx stops honouring SIGTERM
	// after the first one, so a repeat would only kill the replacement.
	manager.restartIfReplaced()
	if *kickstarts != 1 {
		t.Fatalf("kickstarts=%d after a second evaluation, want 1", *kickstarts)
	}
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
	if *kickstarts != 1 {
		t.Fatalf("kickstarts=%d after the replacement landed, want 1", *kickstarts)
	}
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
	if *kickstarts != 0 {
		t.Fatalf("kickstarts=%d while a job is queued", *kickstarts)
	}
	if _, err := manager.store.ClaimJob(ctx, job.ID, "restart-test"); err != nil {
		t.Fatal(err)
	}
	manager.restartIfReplaced()
	if *kickstarts != 0 {
		t.Fatalf("kickstarts=%d while a job is running", *kickstarts)
	}
	if err := manager.store.FinishJob(ctx, job.ID, "restart-test", nil); err != nil {
		t.Fatal(err)
	}
	manager.beginRequest()
	manager.restartIfReplaced()
	if *kickstarts != 0 {
		t.Fatalf("kickstarts=%d while an RPC is in flight", *kickstarts)
	}
	manager.endRequest()
	manager.restartIfReplaced()
	if *kickstarts != 0 {
		t.Fatalf("kickstarts=%d immediately after the last request returned", *kickstarts)
	}
	// One wx invocation is several RPCs with idle moments between them, so the
	// gate only opens once the daemon has stayed idle for restartQuietPeriod.
	manager.mu.Lock()
	manager.lastRequestEnd = time.Now().Add(-restartQuietPeriod)
	manager.mu.Unlock()
	manager.restartIfReplaced()
	if *kickstarts != 1 {
		t.Fatalf("kickstarts=%d once the daemon had been idle, want 1", *kickstarts)
	}
}

func TestUnmanagedDaemonKeepsThePendingRestart(t *testing.T) {
	manager, executable, kickstarts := restartFixture(t)
	manager.launchdManaged = func() bool { return false }
	replaceExecutable(t, executable)
	manager.detectExecutableReplacement()
	manager.restartIfReplaced()
	manager.restartIfReplaced()
	if *kickstarts != 0 {
		t.Fatalf("kickstarts=%d outside launchd", *kickstarts)
	}
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
	if manager.restartPending || *kickstarts != 0 {
		t.Fatalf("disabled watch acted: pending=%v kickstarts=%d", manager.restartPending, *kickstarts)
	}
}

func TestRestartAccountingBracketsEveryHandledRequest(t *testing.T) {
	manager, _, _ := restartFixture(t)
	if _, err := (Handler{Manager: manager}).Handle(context.Background(), "unknown", json.RawMessage(nil)); err == nil {
		t.Fatal("unknown method succeeded")
	}
	if manager.inflightRequests != 0 {
		t.Fatalf("inflightRequests=%d after a failed dispatch", manager.inflightRequests)
	}
}

func TestKickstartServiceFailureLeavesTheRestartClaimed(t *testing.T) {
	manager, executable, _ := restartFixture(t)
	manager.kickstart = func(context.Context) error { return context.DeadlineExceeded }
	replaceExecutable(t, executable)
	manager.detectExecutableReplacement()
	manager.restartIfReplaced()
	if !manager.restartRequested {
		t.Fatal("a failed kickstart released the restart claim")
	}
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
