package daemon

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/HappyOnigiri/WX/internal/config"
	"github.com/HappyOnigiri/WX/internal/discovery"
	"github.com/HappyOnigiri/WX/internal/state"
)

func TestMaintenanceLoopHandlesReloadAndTimer(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	store, err := state.Open(filepath.Join(home, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	cfg := config.Defaults()
	cfg.Storage.WorktreeRoot = filepath.Join(home, "worktrees")
	cfg.Pool.WarmPerWorkspace = 0
	cfg.Discovery.ReconcileInterval.Duration = 10 * time.Millisecond
	if err := config.Save(cfg); err != nil {
		t.Fatal(err)
	}
	manager := testManager(t, cfg, store)
	manager.reloads = make(chan struct{}, 1)
	done := make(chan struct{})
	go func() {
		manager.maintainLifecycle()
		close(done)
	}()
	waitUntil(t, 2*time.Second, func() bool {
		manager.mu.RLock()
		defer manager.mu.RUnlock()
		return !manager.lastBackup.IsZero()
	})
	manager.reloads <- struct{}{}
	started := manager.started
	waitUntil(t, 2*time.Second, func() bool {
		manager.mu.RLock()
		defer manager.mu.RUnlock()
		return manager.lastReload.After(started)
	})
	manager.cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("maintenance loop did not stop")
	}
}

func TestForgetFailsClosedWhenAFailedSlotCannotBeRetired(t *testing.T) {
	t.Parallel()
	requireDaemonIntegration(t)
	root := t.TempDir()
	repository := filepath.Join(root, "repo")
	initGitRepo(t, repository)
	databasePath := filepath.Join(root, "state.db")
	store, err := state.Open(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cfg := config.Defaults()
	cfg.Storage.WorktreeRoot = filepath.Join(root, "worktrees")
	cfg.Worktree.Undefined = "hot"
	cfg.Pool.WarmPerWorkspace = 1
	m := testManager(t, cfg, store)
	m.git.SetTimeout(10 * time.Second)
	defer m.Close()
	ctx := context.Background()

	discoverer := discovery.Discoverer{Git: m.git, Config: cfg}
	w, err := discoverer.Resolve(ctx, repository)
	if err != nil {
		t.Fatal(err)
	}
	w = registerTestWorkspace(t, store, w)
	raw := openTestDatabase(t, databasePath)
	if _, err := raw.ExecContext(ctx, `UPDATE repositories SET last_leased_at=?`, state.FormatTime(time.Now())); err != nil {
		t.Fatal(err)
	}
	if err := m.ensureStandby(ctx, w); err != nil {
		t.Fatal(err)
	}
	jobs, err := store.RecoverJobs(ctx, false)
	if err != nil || len(jobs) != 1 || jobs[0].Kind != "PREPARE" {
		t.Fatalf("standby jobs=%+v err=%v", jobs, err)
	}
	prepared, err := store.ClaimJob(ctx, jobs[0].ID, "test")
	if err != nil {
		t.Fatal(err)
	}
	if err := m.runRecoveredJob(ctx, prepared); err != nil {
		t.Fatal(err)
	}
	if err := store.FinishJob(ctx, prepared.ID, "test", nil); err != nil {
		t.Fatal(err)
	}
	ready, ok, err := store.ReadySlot(ctx, string(w.ID))
	if err != nil || !ok {
		t.Fatalf("ready slot=%+v ok=%v err=%v", ready, ok, err)
	}
	if _, err := raw.ExecContext(ctx, `UPDATE slots SET state='FAILED',rel_path=? WHERE id=?`, filepath.Join("..", "outside-slot"), ready.ID); err != nil {
		t.Fatal(err)
	}

	if err := m.Forget(ctx, repository); err == nil {
		t.Fatal("forget completed despite an unretirable FAILED slot")
	}
	if _, err := store.Workspace(ctx, string(w.ID)); err != nil {
		t.Fatalf("workspace was forgotten despite the failed retirement: %v", err)
	}
	slot, err := store.Slot(ctx, ready.ID)
	if err != nil || slot.State != "QUARANTINED" {
		t.Fatalf("slot with an unprovable path was not quarantined: slot=%+v err=%v", slot, err)
	}
	if _, statErr := os.Stat(ready.Path); statErr != nil {
		t.Fatalf("worktree of an unprovable slot path was removed: %v", statErr)
	}
}

func TestForgetRetiresFailedSlotBeforePermanentlyLeakingIt(t *testing.T) {
	t.Parallel()
	requireDaemonIntegration(t)
	root := t.TempDir()
	repository := filepath.Join(root, "repo")
	initGitRepo(t, repository)
	databasePath := filepath.Join(root, "state.db")
	store, err := state.Open(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cfg := config.Defaults()
	cfg.Storage.WorktreeRoot = filepath.Join(root, "worktrees")
	cfg.Worktree.Undefined = "hot"
	cfg.Pool.WarmPerWorkspace = 1
	m := testManager(t, cfg, store)
	m.git.SetTimeout(10 * time.Second)
	defer m.Close()
	ctx := context.Background()

	discoverer := discovery.Discoverer{Git: m.git, Config: cfg}
	w, err := discoverer.Resolve(ctx, repository)
	if err != nil {
		t.Fatal(err)
	}
	w = registerTestWorkspace(t, store, w)
	raw := openTestDatabase(t, databasePath)
	if _, err := raw.ExecContext(ctx, `UPDATE repositories SET last_leased_at=?`, state.FormatTime(time.Now())); err != nil {
		t.Fatal(err)
	}
	if err := m.ensureStandby(ctx, w); err != nil {
		t.Fatal(err)
	}
	jobs, err := store.RecoverJobs(ctx, false)
	if err != nil || len(jobs) != 1 || jobs[0].Kind != "PREPARE" {
		t.Fatalf("standby jobs=%+v err=%v", jobs, err)
	}
	prepared, err := store.ClaimJob(ctx, jobs[0].ID, "test")
	if err != nil {
		t.Fatal(err)
	}
	if err := m.runRecoveredJob(ctx, prepared); err != nil {
		t.Fatal(err)
	}
	if err := store.FinishJob(ctx, prepared.ID, "test", nil); err != nil {
		t.Fatal(err)
	}
	ready, ok, err := store.ReadySlot(ctx, string(w.ID))
	if err != nil || !ok {
		t.Fatalf("ready slot=%+v ok=%v err=%v", ready, ok, err)
	}
	if _, err := raw.ExecContext(ctx, `UPDATE slots SET state='FAILED' WHERE id=?`, ready.ID); err != nil {
		t.Fatal(err)
	}

	if err := m.Forget(ctx, repository); err != nil {
		t.Fatalf("forget with a FAILED slot: %v", err)
	}
	if _, err := store.Workspace(ctx, string(w.ID)); err == nil {
		t.Fatal("workspace was not forgotten")
	}
	slot, err := store.Slot(ctx, ready.ID)
	if err != nil || slot.State != "ARCHIVED" {
		t.Fatalf("failed slot was not retired: slot=%+v err=%v", slot, err)
	}
	if _, statErr := os.Stat(ready.Path); !os.IsNotExist(statErr) {
		t.Fatalf("failed slot worktree was not removed: err=%v", statErr)
	}
}
