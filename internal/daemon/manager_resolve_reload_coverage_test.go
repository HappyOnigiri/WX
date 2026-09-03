package daemon

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/HappyOnigiri/WX/internal/config"
	"github.com/HappyOnigiri/WX/internal/discovery"
	"github.com/HappyOnigiri/WX/internal/domain"
	"github.com/HappyOnigiri/WX/internal/gitx"
	"github.com/HappyOnigiri/WX/internal/pool"
	"github.com/HappyOnigiri/WX/internal/state"
	"github.com/HappyOnigiri/WX/internal/workspace"
)

// TestReloadConfigFailsClosedWhileShuttingDown verifies that a config reload
// racing a manager shutdown is rejected with errManagerClosed instead of
// installing a new worktree-root descriptor into a manager that is already
// tearing down its state.
func TestReloadConfigFailsClosedWhileShuttingDown(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	store, err := state.Open(filepath.Join(home, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cfg := config.Defaults()
	cfg.Storage.WorktreeRoot = filepath.Join(home, "worktrees")
	m := testManager(t, cfg, store)
	defer m.Close()

	configPath, err := config.Path()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(configPath), 0o700); err != nil {
		t.Fatal(err)
	}
	validConfig := "version: 1\nstorage:\n  worktree_root: " + cfg.Storage.WorktreeRoot + "\n"
	if err := os.WriteFile(configPath, []byte(validConfig), 0o600); err != nil {
		t.Fatal(err)
	}

	m.mu.Lock()
	m.rootClosing = true
	m.mu.Unlock()

	if err := m.reloadConfig(false); !errors.Is(err, errManagerClosed) {
		t.Fatalf("reload during shutdown error=%v", err)
	}
}

// TestResolveAndLeaseRetiresStaleReadySlotAndAllocatesFresh verifies that a
// cached READY slot whose recorded repository state no longer matches the
// resolved branch (a stale BaseOID) is marked STALE and skipped rather than
// handed out as a lease, and that ResolveAndLease falls back to allocating a
// fresh slot instead of failing outright.
func TestResolveAndLeaseRetiresStaleReadySlotAndAllocatesFresh(t *testing.T) {
	root := t.TempDir()
	repository := filepath.Join(root, "repo")
	initGitRepo(t, repository)
	store, err := state.Open(filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cfg := config.Defaults()
	cfg.Storage.WorktreeRoot = filepath.Join(root, "worktrees")
	cfg.Pool.WarmPerWorkspace = 0
	m := testManager(t, cfg, store)
	m.git = &gitx.Runner{Timeout: 5 * time.Second}
	defer m.Close()
	ctx := context.Background()

	discoverer := discovery.Discoverer{Git: m.git, Config: cfg}
	w, err := discoverer.Resolve(ctx, repository)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertWorkspace(ctx, w); err != nil {
		t.Fatal(err)
	}

	staleID := domain.StableID("resolve-lease", "stale")
	stalePath := filepath.Join(cfg.Storage.WorktreeRoot, "workspaces", string(w.ID), "slots", staleID, "root")
	if _, err := m.createSlotRoot(stalePath); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateStandby(ctx,
		state.Slot{ID: staleID, WorkspaceID: string(w.ID), Generation: 1, Path: stalePath, State: "READY"},
		[]state.SlotRepository{{RepositoryID: string(w.Repositories[0].ID), WorktreePath: filepath.Join(stalePath, w.Repositories[0].RelativePath), State: "READY", BaseOID: "stale-oid"}}); err != nil {
		t.Fatal(err)
	}

	lease, err := m.ResolveAndLease(ctx, repository, nil, "codex", os.Getpid())
	if err != nil {
		t.Fatalf("resolve and lease: %v", err)
	}
	if lease.SessionID == staleID {
		t.Fatal("stale READY slot was reused despite a BaseOID mismatch")
	}
	if slot, err := store.Slot(ctx, staleID); err != nil || slot.State != "STALE" {
		t.Fatalf("stale ready slot=%+v err=%v", slot, err)
	}
}

// TestResolveAndLeaseQuarantinesReadySlotWithUnverifiableRepositoryPath
// verifies that a READY slot whose recorded repository worktree path has
// drifted outside every known wx root is quarantined and reported back to
// the caller as an ownership failure, instead of ResolveAndLease silently
// skipping it and allocating a fresh slot.
func TestResolveAndLeaseQuarantinesReadySlotWithUnverifiableRepositoryPath(t *testing.T) {
	root := t.TempDir()
	repository := filepath.Join(root, "repo")
	initGitRepo(t, repository)
	store, err := state.Open(filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cfg := config.Defaults()
	cfg.Storage.WorktreeRoot = filepath.Join(root, "worktrees")
	cfg.Pool.WarmPerWorkspace = 0
	m := testManager(t, cfg, store)
	m.git = &gitx.Runner{Timeout: 5 * time.Second}
	defer m.Close()
	ctx := context.Background()

	discoverer := discovery.Discoverer{Git: m.git, Config: cfg}
	w, err := discoverer.Resolve(ctx, repository)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertWorkspace(ctx, w); err != nil {
		t.Fatal(err)
	}
	resolved, err := pool.ResolveBranches(ctx, m.git, w, nil)
	if err != nil {
		t.Fatal(err)
	}
	fingerprint, err := workspace.Fingerprint(1, resolved[0].OID, resolved[0].Repository, cfg)
	if err != nil {
		t.Fatal(err)
	}

	badID := domain.StableID("resolve-lease", "bad-path")
	badPath := filepath.Join(cfg.Storage.WorktreeRoot, "workspaces", string(w.ID), "slots", badID, "root")
	if _, err := m.createSlotRoot(badPath); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside-worktree")
	if _, err := store.CreateStandby(ctx,
		state.Slot{ID: badID, WorkspaceID: string(w.ID), Generation: 1, Path: badPath, State: "READY"},
		[]state.SlotRepository{{RepositoryID: string(resolved[0].Repository.ID), WorktreePath: outside, State: "READY", BaseOID: resolved[0].OID, Fingerprint: fingerprint}}); err != nil {
		t.Fatal(err)
	}

	if _, err := m.ResolveAndLease(ctx, repository, nil, "codex", os.Getpid()); !errors.Is(err, state.ErrOwnership) {
		t.Fatalf("resolve and lease with unverifiable ready repository error=%v", err)
	}
	if slot, err := store.Slot(ctx, badID); err != nil || slot.State != "QUARANTINED" {
		t.Fatalf("unverifiable ready slot=%+v err=%v", slot, err)
	}
}
