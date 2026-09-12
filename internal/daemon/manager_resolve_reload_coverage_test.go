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

// 退役直後のSTALEを観測するため、直列で実行する。
// 並列実行の負荷では後続の削除ジョブが先に走り、REMOVINGへ進んでしまう。
func TestResolveAndLeaseRetiresStaleReadySlotAndAllocatesFresh(t *testing.T) {
	root := t.TempDir()
	repository := filepath.Join(root, "repo")
	initGitRepo(t, repository)
	store, err := openTestStoreAtPath(t, filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cfg := config.Defaults()
	cfg.Storage.WorktreeRoot = filepath.Join(root, "worktrees")
	cfg.Worktree.ReuseStandby = false
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

	lease, err := m.ResolveAndLease(ctx, repository, nil, "codex", os.Getpid(), leaseAttrs{})
	if err != nil {
		t.Fatalf("resolve and lease: %v", err)
	}
	if lease.SessionID == staleID {
		t.Fatal("stale READY slot was reused despite a BaseOID mismatch")
	}
	// allocate が起動する background GC は STALE slot を REMOVING へ進めるため、どちらも退役とみなす。
	if slot, err := store.Slot(ctx, staleID); err != nil || (slot.State != "STALE" && slot.State != "REMOVING") {
		t.Fatalf("stale ready slot=%+v err=%v", slot, err)
	}
}

func TestResolveAndLeaseReusesReadySlotForMatchingExplicitBranch(t *testing.T) {
	t.Parallel()
	requireDaemonIntegration(t)
	root := t.TempDir()
	repository := filepath.Join(root, "repo")
	initGitRepo(t, repository)
	store, err := openTestStoreAtPath(t, filepath.Join(root, "state.db"))
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
	raw := openTestDatabase(t, filepath.Join(root, "state.db"))
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

	lease, err := m.ResolveAndLease(ctx, repository, []string{"main"}, "codex", os.Getpid(), leaseAttrs{})
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

func TestResolveAndLeaseRejectsHotStandbyAfterWorktreeLinkAppears(t *testing.T) {
	t.Parallel()
	requireDaemonIntegration(t)
	root := t.TempDir()
	repository := filepath.Join(root, "repo")
	initGitRepo(t, repository)
	if err := os.WriteFile(filepath.Join(repository, ".gitignore"), []byte("app/\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repository, ".worktreelink"), []byte("app/tmp\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	databasePath := filepath.Join(root, "state.db")
	store, err := openTestStoreAtPath(t, databasePath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	cfg := config.Defaults()
	cfg.Storage.WorktreeRoot = filepath.Join(root, "worktrees")
	cfg.Worktree.Undefined = "hot"
	cfg.Worktree.ReuseStandby = false
	cfg.Pool.WarmPerWorkspace = 1
	m := testManager(t, cfg, store)
	m.git.SetTimeout(10 * time.Second)
	t.Cleanup(m.Close)
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
		t.Fatalf("missing .worktreelink source blocked hot standby: %v", err)
	}
	j, err := store.RecoverJobs(ctx, false)
	if err != nil || len(j) != 1 || j[0].Kind != "PREPARE" {
		t.Fatalf("standby jobs=%+v err=%v", j, err)
	}
	prepared, err := store.ClaimJob(ctx, j[0].ID, "test")
	if err != nil {
		t.Fatal(err)
	}
	if err := m.runRecoveredJob(ctx, prepared); err != nil {
		t.Fatalf("prepare hot standby with missing .worktreelink source: %v", err)
	}
	if err := store.FinishJob(ctx, prepared.ID, "test", nil); err != nil {
		t.Fatal(err)
	}
	ready, ok, err := store.ReadySlot(ctx, string(w.ID))
	if err != nil || !ok {
		t.Fatalf("ready slot=%+v ok=%v err=%v", ready, ok, err)
	}
	if _, err := os.Lstat(filepath.Join(ready.Path, "repo", "app")); !os.IsNotExist(err) {
		t.Fatalf("missing .worktreelink source touched standby: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(repository, "app", "tmp"), 0o700); err != nil {
		t.Fatal(err)
	}
	lease, err := m.ResolveAndLease(ctx, repository, nil, "codex", os.Getpid(), leaseAttrs{})
	if err != nil {
		t.Fatalf("resolve and lease after .worktreelink source appeared: %v", err)
	}
	if lease.SessionID == ready.ID {
		t.Fatalf("hot standby with changed .worktreelink source presence was reused: %s", lease.SessionID)
	}
}

func TestResolveAndLeaseKeepsWarmPoolWhenExplicitBranchDoesNotMatch(t *testing.T) {
	t.Parallel()
	requireDaemonIntegration(t)
	root := t.TempDir()
	repository := filepath.Join(root, "repo")
	initGitRepo(t, repository)
	databasePath := filepath.Join(root, "state.db")
	store, err := openTestStoreAtPath(t, databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cfg := config.Defaults()
	cfg.Storage.WorktreeRoot = filepath.Join(root, "worktrees")
	cfg.Worktree.Undefined = "hot"
	cfg.Worktree.ReuseStandby = false
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

	gitRun(t, repository, "checkout", "-q", "-b", "other")
	gitRun(t, repository, "commit", "--allow-empty", "-m", "other")
	gitRun(t, repository, "checkout", "-q", "main")

	lease, err := m.ResolveAndLease(ctx, repository, []string{"other"}, "codex", os.Getpid(), leaseAttrs{})
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
	t.Parallel()
	root := t.TempDir()
	repository := filepath.Join(root, "repo")
	initGitRepo(t, repository)
	store, err := openTestStoreAtPath(t, filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cfg := config.Defaults()
	cfg.Storage.WorktreeRoot = filepath.Join(root, "worktrees")
	cfg.Worktree.ReuseStandby = false
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

	if _, err := m.ResolveAndLease(ctx, repository, nil, "codex", os.Getpid(), leaseAttrs{}); !errors.Is(err, state.ErrOwnership) {
		t.Fatalf("resolve and lease with unverifiable ready repository error=%v", err)
	}
	if slot, err := store.Slot(ctx, badID); err != nil || slot.State != "QUARANTINED" {
		t.Fatalf("unverifiable ready slot=%+v err=%v", slot, err)
	}
}
