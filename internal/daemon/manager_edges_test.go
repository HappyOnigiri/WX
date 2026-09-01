package daemon

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/HappyOnigiri/WX/internal/config"
	"github.com/HappyOnigiri/WX/internal/gitx"
	"github.com/HappyOnigiri/WX/internal/state"
)

func testManager(t *testing.T, cfg config.Config, store *state.Store) *Manager {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	root, err := config.ExpandHome(cfg.Storage.WorktreeRoot)
	if err != nil {
		t.Fatal(err)
	}
	return &Manager{
		cfg:     cfg,
		store:   store,
		git:     &gitx.Runner{Timeout: time.Second},
		log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		started: time.Now(),
		roots:   map[string]bool{filepath.Clean(root): true},
		jobs:    make(chan jobWork, 4),
		ctx:     ctx,
		cancel:  cancel,
	}
}

func TestManagerReloadForgetAndDiagnosticErrors(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	ctx := context.Background()
	store, err := state.Open(filepath.Join(home, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	cfg := config.Defaults()
	cfg.Storage.WorktreeRoot = filepath.Join(home, "old-root")
	m := testManager(t, cfg, store)
	t.Cleanup(m.Close)

	repository := filepath.Join(home, "repository")
	initGitRepo(t, repository)
	lease, err := m.ResolveAndLease(ctx, repository, []string{"main"}, "codex", os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	jobs, err := store.RecoverJobs(ctx, false)
	if err != nil || len(jobs) == 0 {
		t.Fatalf("preparation jobs=%v err=%v", jobs, err)
	}
	for _, job := range jobs {
		if job.Kind != "PREPARE" {
			continue
		}
		claimed, err := store.ClaimJob(ctx, job.ID, "test")
		if err != nil {
			t.Fatal(err)
		}
		if err := m.runRecoveredJob(ctx, claimed); err != nil {
			t.Fatal(err)
		}
		if err := store.FinishJob(ctx, claimed.ID, "test", nil); err != nil {
			t.Fatal(err)
		}
	}
	readyCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := m.WaitReady(readyCtx, lease.SessionID, lease.Token); err != nil {
		t.Fatal(err)
	}
	if err := m.Forget(ctx, repository); err == nil {
		t.Fatal("active workspace was forgotten")
	}
	if err := m.Forget(ctx, filepath.Join(home, "missing")); err == nil {
		t.Fatal("unknown workspace was forgotten")
	}

	newRoot := filepath.Join(home, "new-root")
	configPath, err := config.Path()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(configPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, []byte("version: 1\nstorage:\n  worktree_root: "+newRoot+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := m.reloadConfig(false); err != nil {
		t.Fatal(err)
	}
	if got := m.Config().Storage.WorktreeRoot; got != newRoot {
		t.Fatalf("reloaded root=%q", got)
	}
	if _, ok := m.rootForPath(filepath.Join(cfg.Storage.WorktreeRoot, "retired", "slot")); !ok {
		t.Fatal("retired root was not retained for safe draining")
	}
	if _, ok := m.rootForPath(filepath.Join(home, "outside")); ok {
		t.Fatal("outside path was accepted as wx-owned")
	}
	if err := os.WriteFile(configPath, []byte("unknown: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := m.reloadConfig(false); err == nil {
		t.Fatal("invalid config reload succeeded")
	}
	status, err := m.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if status["config_reload_error"] == "" || status["config_last_reload"] == "" {
		t.Fatalf("reload diagnostics=%v", status)
	}
	if formatOptionalTime(time.Time{}) != "" || formatOptionalTime(time.Unix(1, 0)) == "" {
		t.Fatal("optional time formatting is inconsistent")
	}
	if must("", errors.New("expected")) != "" || must("value", nil) != "value" {
		t.Fatal("must helper result is inconsistent")
	}
}

func TestManagerReadinessAndRecoveryFailurePaths(t *testing.T) {
	root := t.TempDir()
	store, err := state.Open(filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	cfg := config.Defaults()
	cfg.Storage.WorktreeRoot = filepath.Join(root, "worktrees")
	cfg.Readiness.Timeout.Duration = 20 * time.Millisecond
	m := testManager(t, cfg, store)
	t.Cleanup(m.Close)
	ctx := context.Background()

	lease, err := m.AllocateResumeSlot(ctx, "codex", os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	if err := m.WaitReady(ctx, lease.SessionID, "wrong"); err == nil {
		t.Fatal("wrong token passed readiness")
	}
	timed, cancel := context.WithTimeout(ctx, 5*time.Millisecond)
	if err := m.WaitReady(timed, lease.SessionID, lease.Token); !errors.Is(err, context.DeadlineExceeded) {
		cancel()
		t.Fatalf("unbound readiness error=%v", err)
	}
	cancel()
	if err := m.ValidateFreshResume(ctx, lease.SessionID, lease.Token, "new-agent"); err == nil {
		t.Fatal("UNBOUND slot unexpectedly accepted fresh resume")
	}
	if err := store.SetSlotState(ctx, lease.SessionID, []string{"UNBOUND"}, "QUARANTINED", "test"); err != nil {
		t.Fatal(err)
	}
	if err := m.WaitReady(ctx, lease.SessionID, lease.Token); err == nil {
		t.Fatal("quarantined slot reported ready")
	}
	if err := m.BindAgentSession(ctx, lease.SessionID, "wrong", "agent"); err == nil {
		t.Fatal("wrong token bound agent session")
	}
	if err := m.runRecoveredJob(ctx, state.Job{Kind: "UNKNOWN"}); err == nil {
		t.Fatal("unknown persistent job kind succeeded")
	}
	if err := m.removeSlotJob(ctx, state.Job{SlotID: lease.SessionID}); err == nil {
		t.Fatal("quarantined slot was removed")
	}
	if err := m.removeColdRepositoryJob(ctx, state.Job{SlotID: lease.SessionID, RepositoryID: "missing"}); err == nil {
		t.Fatal("missing repository retirement succeeded")
	}
	if _, err := m.ResumeStatus(ctx, "missing"); err == nil {
		t.Fatal("missing resume status succeeded")
	}
	if _, err := m.Resume(ctx, "missing", "codex", os.Getpid(), false); err == nil {
		t.Fatal("missing explicit resume succeeded")
	}
	if _, _, err := m.waitForSnapshot(ctx, "missing"); err == nil {
		t.Fatal("missing snapshot wait succeeded")
	}

	missing := state.Slot{ID: "missing", Path: filepath.Join(root, "does-not-exist")}
	if ok, err := m.readyMatches(ctx, missing, nil); err != nil || ok {
		t.Fatalf("missing READY root ok=%v err=%v", ok, err)
	}
	file := filepath.Join(root, "file")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if ok, err := m.readyMatches(ctx, state.Slot{ID: "file", Path: file}, nil); err != nil || ok {
		t.Fatalf("file READY root ok=%v err=%v", ok, err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(file, link); err != nil {
		t.Fatal(err)
	}
	if ok, err := m.readyMatches(ctx, state.Slot{ID: "link", Path: link}, nil); err != nil || ok {
		t.Fatalf("symlink READY root ok=%v err=%v", ok, err)
	}
}
