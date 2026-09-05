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
