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
	w = registerTestWorkspace(t, store, w)

	staleID := domain.StableID("resolve-lease", "stale")
	stalePath := filepath.Join(cfg.Storage.WorktreeRoot, string(w.ID), staleID)
	if _, _, err := m.createSlotRoot(stalePath, stalePath); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateStandby(ctx,
		slotAtPath(t, m, string(w.ID), staleID, stalePath, 1, "READY"),
		[]state.SlotRepository{{RepositoryID: string(w.Repositories[0].ID), DirName: testDirName(w.Repositories[0], cfg), State: "READY", BaseOID: "stale-oid"}}); err != nil {
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

// TestResolveAndLeaseReusesReadySlotForMatchingExplicitBranch verifies that
// an explicit --branch request is no longer refused the READY pool outright:
// when the requested branch resolves to exactly what a READY slot already
// holds (here, --branch main against a slot built from main with nothing new
// to fetch), ResolveAndLease reuses it instead of unconditionally cold
// allocating a fresh slot.
func TestResolveAndLeaseReusesReadySlotForMatchingExplicitBranch(t *testing.T) {
	requireDaemonIntegration(t)
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
	// ensureStandby only checks out repositories used within hot_standby;
	// mark this repository as previously leased so the standby it builds
	// below is an actual hot checkout, not a COLD placeholder. That isolates
	// this test to the exact-match reuse decision this test is about,
	// independent of the cold-materialize path exercised elsewhere.
	raw := openManagerCoverageDB(t, filepath.Join(root, "state.db"))
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
	if repositories, err := store.SlotRepositories(ctx, ready.ID); err != nil || len(repositories) != 1 || repositories[0].State != "READY" {
		t.Fatalf("standby fixture is not a hot checkout: repositories=%+v err=%v", repositories, err)
	}

	lease, err := m.ResolveAndLease(ctx, repository, []string{"main"}, "codex", os.Getpid())
	if err != nil {
		t.Fatalf("resolve and lease with explicit branch: %v", err)
	}
	if lease.SessionID != ready.ID {
		t.Fatalf("explicit --branch main did not reuse the matching READY slot: got session %s, want %s", lease.SessionID, ready.ID)
	}
	if !lease.Ready {
		t.Fatalf("reused exact-match slot was not reported ready: %+v", lease)
	}
}

// TestResolveAndLeaseKeepsWarmPoolWhenExplicitBranchDoesNotMatch guards the
// other side of consulting the READY pool for an explicit --branch request: a
// slot that holds a different base OID is not stale, it is simply not what
// this request wants. Retiring it would let a single `wx --branch other`
// invocation wipe the warm pool built for main, making the *next* plain `wx`
// a cold start as well — strictly worse than not consulting the pool at all.
func TestResolveAndLeaseKeepsWarmPoolWhenExplicitBranchDoesNotMatch(t *testing.T) {
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
	raw := openManagerCoverageDB(t, databasePath)
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

	// A branch whose tip is a different commit than the standby's base OID.
	gitRun(t, repository, "checkout", "-q", "-b", "other")
	gitRun(t, repository, "commit", "--allow-empty", "-m", "other")
	gitRun(t, repository, "checkout", "-q", "main")

	lease, err := m.ResolveAndLease(ctx, repository, []string{"other"}, "codex", os.Getpid())
	if err != nil {
		t.Fatalf("resolve and lease with a mismatching branch: %v", err)
	}
	if lease.SessionID == ready.ID {
		t.Fatal("mismatching branch reused the main-based standby slot")
	}
	slot, err := store.Slot(ctx, ready.ID)
	if err != nil {
		t.Fatal(err)
	}
	if slot.State != "READY" {
		t.Fatalf("warm pool slot state=%s failure_code=%s, want READY", slot.State, slot.FailureCode)
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
	w = registerTestWorkspace(t, store, w)
	resolved, err := pool.ResolveBranches(ctx, m.git, w, nil)
	if err != nil {
		t.Fatal(err)
	}
	badID := domain.StableID("resolve-lease", "bad-path")
	badPath := filepath.Join(cfg.Storage.WorktreeRoot, string(w.ID), badID)
	if _, _, err := m.createSlotRoot(badPath, badPath); err != nil {
		t.Fatal(err)
	}
	// The recorded directory name escapes the wx root. The fingerprint still
	// has to match, or the slot would be reported as an ordinary not-ready
	// slot before the ownership check under test runs.
	badDirName := escapingDirNameFor(t, cfg.Storage.WorktreeRoot, badPath)
	fingerprint, err := workspace.Fingerprint(1, resolved[0].OID, resolved[0].Repository, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateStandby(ctx,
		slotAtPath(t, m, string(w.ID), badID, badPath, 1, "READY"),
		[]state.SlotRepository{{RepositoryID: string(resolved[0].Repository.ID), DirName: badDirName, State: "READY", BaseOID: resolved[0].OID, Fingerprint: fingerprint}}); err != nil {
		t.Fatal(err)
	}

	if _, err := m.ResolveAndLease(ctx, repository, nil, "codex", os.Getpid()); !errors.Is(err, state.ErrOwnership) {
		t.Fatalf("resolve and lease with unverifiable ready repository error=%v", err)
	}
	if slot, err := store.Slot(ctx, badID); err != nil || slot.State != "QUARANTINED" {
		t.Fatalf("unverifiable ready slot=%+v err=%v", slot, err)
	}
}

// TestForgetFailsClosedWhenAFailedSlotCannotBeRetired covers the other half of
// the FAILED-slot retirement: when the retirement cannot complete, forget must
// report the failure and leave the workspace registered. Completing it would
// clear workspace_id and make the slot's path permanently unprovable, which is
// exactly the leak the retirement exists to prevent. The unprovable path must
// also end up QUARANTINED rather than deleted.
func TestForgetFailsClosedWhenAFailedSlotCannotBeRetired(t *testing.T) {
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
	raw := openManagerCoverageDB(t, databasePath)
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
	// Fail the slot and move its recorded location outside every known wx
	// root, so the retirement cannot prove ownership of what it would have to
	// delete. A slot is now located by root generation plus root-relative
	// path, so the escape is spelled in rel_path.
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

// TestForgetRetiresFailedSlotBeforePermanentlyLeakingIt verifies that
// forgetting a workspace with a FAILED slot physically removes that slot's
// worktree instead of merely clearing its workspace_id: once the link is
// gone, ValidateWorktreeOwnership can never again prove ownership for that
// path, which would otherwise leak it forever with no automatic recovery.
func TestForgetRetiresFailedSlotBeforePermanentlyLeakingIt(t *testing.T) {
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
	// A never-leased single-repository workspace's standby would otherwise
	// register its sole repository COLD (see ensureStandby's hot_standby
	// filter); mark it previously leased so this builds a real checkout to
	// force into FAILED below.
	raw := openManagerCoverageDB(t, databasePath)
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
	// Force the otherwise-healthy standby into FAILED: forget must treat it
	// the same as any other genuinely failed preparation.
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
