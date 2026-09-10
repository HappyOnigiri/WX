package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/HappyOnigiri/WX/internal/config"
	"github.com/HappyOnigiri/WX/internal/state"
)

func TestScheduleDropsWorkAfterCancellation(t *testing.T) {
	t.Parallel()
	store, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	m := testManager(t, config.Defaults(), store)
	defer m.Close()
	m.cancel()
	m.schedule(state.Job{ID: "dropped", Kind: "SNAPSHOT", SessionID: "session"})
	if pending, _ := m.jobQueue.counts(jobClassInteractive); pending != 0 {
		t.Fatalf("canceled schedule queued %d jobs", pending)
	}
}

func TestScheduleLeavesOverflowForDurableRecovery(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m := &Manager{jobQueue: newJobQueue(1), log: slog.New(slog.NewTextHandler(newDiagnosticLog(managerFixtureLogLimit), nil)), ctx: ctx, cancel: cancel}
	for index := 0; index <= queuedJobCapacity; index++ {
		m.schedule(state.Job{ID: fmt.Sprintf("standby-%d", index), Kind: "PREPARE"})
	}
	if pending, _ := m.jobQueue.counts(jobClassMaintenance); pending != queuedJobCapacity {
		t.Fatalf("maintenance queue length=%d, want %d", pending, queuedJobCapacity)
	}
	work, slot, ok := m.jobQueue.take()
	if !ok || work.id != "standby-0" {
		t.Fatalf("queued work=%+v ok=%v", work, ok)
	}
	m.jobQueue.finish(work, slot)
}

func TestWorkerStopsRetryingAfterBoundedAttempts(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	store, err := state.Open(filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	cfg := config.Defaults()
	cfg.Storage.WorktreeRoot = filepath.Join(root, "worktrees")
	m := testManager(t, cfg, store)
	t.Cleanup(m.Close)
	ctx := context.Background()
	job, err := store.CreateJob(ctx, "REMOVE", "", "missing-slot", "")
	if err != nil {
		t.Fatal(err)
	}
	for attempt := 1; attempt < maxJobAttempts; attempt++ {
		claimed, err := store.ClaimJob(ctx, job.ID, "seed")
		if err != nil {
			t.Fatal(err)
		}
		if err := store.RetryJob(ctx, claimed.ID, "seed", 0, "TEST_RETRY"); err != nil {
			t.Fatal(err)
		}
	}
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		m.dispatchJobs()
	}()
	m.schedule(job)
	deadline := time.Now().Add(2 * time.Second)
	for {
		status, err := store.Status(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if status.Jobs == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("exhausted removal job remained retryable")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestWorkerDefersLiveAgentDependencyWithoutRetryConsumption(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	databasePath := filepath.Join(root, "state.db")
	store, err := state.Open(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	cfg := config.Defaults()
	cfg.Storage.WorktreeRoot = filepath.Join(root, "worktrees")
	m := testManager(t, cfg, store)
	t.Cleanup(m.Close)
	ctx := context.Background()
	job, err := store.CreateSlotSession(ctx,
		slotAtPath(t, m, "", "live-snapshot", filepath.Join(cfg.Storage.WorktreeRoot, "live-snapshot"), 0, "DRAINING"), nil,
		state.Session{ID: "live-snapshot", SlotID: "live-snapshot", State: "RELEASING", AgentKind: "codex", TokenHash: state.HashToken("token")}, "SNAPSHOT")
	if err != nil {
		t.Fatal(err)
	}
	raw := openTestDatabase(t, databasePath)
	if _, err := raw.ExecContext(ctx, `UPDATE sessions SET agent_pid=? WHERE id='live-snapshot'`, os.Getpid()); err != nil {
		t.Fatal(err)
	}
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		m.dispatchJobs()
	}()
	m.schedule(job)
	waitUntil(t, 5*time.Second, func() bool {
		var count int
		return raw.QueryRowContext(ctx, `SELECT count(*) FROM events WHERE kind='job_dependency_wait' AND session_id='live-snapshot'`).Scan(&count) == nil && count == 1
	})
	jobs, err := store.RecoverJobs(ctx, false)
	if err != nil || len(jobs) != 1 || jobs[0].ID != job.ID || jobs[0].Attempt != 0 {
		t.Fatalf("dependency-bound job consumed retry budget: jobs=%+v err=%v", jobs, err)
	}
}

func TestJobClassOfSeparatesUserFacingWorkFromMaintenance(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		job  state.Job
		want jobClass
	}{
		{job: state.Job{Kind: "PREPARE", SessionID: "session"}, want: jobClassInteractive},
		{job: state.Job{Kind: "PREPARE"}, want: jobClassMaintenance},
		{job: state.Job{Kind: "RESTORE", SessionID: "session"}, want: jobClassInteractive},
		{job: state.Job{Kind: "SNAPSHOT", SessionID: "session"}, want: jobClassInteractive},
		{job: state.Job{Kind: "ENSURE_STANDBY"}, want: jobClassMaintenance},
		{job: state.Job{Kind: "REMOVE", SessionID: "session"}, want: jobClassMaintenance},
		{job: state.Job{Kind: "REMOVE_REPOSITORY"}, want: jobClassMaintenance},
	} {
		if got := jobClassOf(test.job); got != test.want {
			t.Fatalf("%s job with session=%q class=%s, want %s", test.job.Kind, test.job.SessionID, got, test.want)
		}
	}
}

func TestDispatcherKeepsUserFacingJobsRunnableWhileMaintenanceIsBlocked(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	store, err := state.Open(filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	cfg := config.Defaults()
	cfg.Storage.WorktreeRoot = filepath.Join(root, "worktrees")
	cfg.Pool.PreparationConcurrency = 1
	m := testManager(t, cfg, store)
	t.Cleanup(m.Close)
	ctx := context.Background()
	// 保守クラスの実行を止め、待機枠のコピーが長引いた状態を作る。解放は Close でも起きる。
	release := make(chan struct{})
	snapshotRan := make(chan struct{}, 1)
	m.mu.Lock()
	m.beforeJobRun = func(job state.Job) {
		if job.Kind != "ENSURE_STANDBY" {
			snapshotRan <- struct{}{}
			return
		}
		select {
		case <-release:
		case <-m.ctx.Done():
		}
	}
	m.mu.Unlock()
	var scheduled []state.Job
	for _, kind := range []string{"ENSURE_STANDBY", "ENSURE_STANDBY", "SNAPSHOT"} {
		job, createErr := store.CreateJob(ctx, kind, "missing-workspace", "", "missing-session")
		if createErr != nil {
			t.Fatal(createErr)
		}
		scheduled = append(scheduled, job)
	}
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		m.dispatchJobs()
	}()
	for _, job := range scheduled {
		m.schedule(job)
	}
	waitUntil(t, 5*time.Second, func() bool {
		pending, running := m.jobQueue.counts(jobClassInteractive)
		return pending == 0 && running == 0
	})
	select {
	case <-snapshotRan:
	default:
		t.Fatal("the user-facing job did not run while maintenance was blocked")
	}
	if _, err := m.Status(ctx); err != nil {
		t.Fatalf("status while maintenance was blocked: %v", err)
	}
	if pending, running := m.jobQueue.counts(jobClassMaintenance); pending != 1 || running != maintenanceJobSlots {
		t.Fatalf("maintenance pending=%d running=%d, want one waiting behind one running", pending, running)
	}
	close(release)
	waitUntil(t, 5*time.Second, func() bool {
		pending, running := m.jobQueue.counts(jobClassMaintenance)
		return pending == 0 && running == 0
	})
}
