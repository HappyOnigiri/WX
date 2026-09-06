package daemon

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/HappyOnigiri/WX/internal/config"
	"github.com/HappyOnigiri/WX/internal/state"
)

func TestScheduleDropsWorkWhenCanceledQueueIsFull(t *testing.T) {
	t.Parallel()
	store, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	m := testManager(t, config.Defaults(), store)
	defer m.Close()
	for index := 0; index < cap(m.jobs); index++ {
		m.jobs <- jobWork{id: fmt.Sprintf("queued-%d", index)}
	}
	m.cancel()
	m.schedule(state.Job{ID: "dropped"})
	if got := len(m.jobs); got != cap(m.jobs) {
		t.Fatalf("canceled schedule changed queue length=%d, want %d", got, cap(m.jobs))
	}
}

func TestScheduleLeavesOverflowForDurableRecovery(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m := &Manager{jobs: make(chan jobWork, 1), ctx: ctx, cancel: cancel}
	m.schedule(state.Job{ID: "first"})
	m.schedule(state.Job{ID: "overflow"})
	if queued := <-m.jobs; queued.id != "first" {
		t.Fatalf("queued work=%+v", queued)
	}
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
	go m.runWorker(0, make(chan struct{}))
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
	raw, err := sql.Open("sqlite", databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	if _, err := raw.ExecContext(ctx, `UPDATE sessions SET agent_pid=? WHERE id='live-snapshot'`, os.Getpid()); err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{})
	m.wg.Add(1)
	go m.runWorker(99, stop)
	m.schedule(job)
	waitUntil(t, 5*time.Second, func() bool {
		var count int
		return raw.QueryRowContext(ctx, `SELECT count(*) FROM events WHERE kind='job_dependency_wait' AND session_id='live-snapshot'`).Scan(&count) == nil && count == 1
	})
	jobs, err := store.RecoverJobs(ctx, false)
	if err != nil || len(jobs) != 1 || jobs[0].ID != job.ID || jobs[0].Attempt != 0 {
		t.Fatalf("dependency-bound job consumed retry budget: jobs=%+v err=%v", jobs, err)
	}
	close(stop)
}
