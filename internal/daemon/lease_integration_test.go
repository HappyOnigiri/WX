package daemon

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/HappyOnigiri/WX/internal/discovery"
	"github.com/HappyOnigiri/WX/internal/state"
	"github.com/HappyOnigiri/WX/internal/workspace"
)

func TestSingleRepositoryColdRemovalRecreatesReadySlotRoot(t *testing.T) {
	t.Parallel()
	requireDaemonIntegration(t)
	f := manualManagerFixture(t, func(s *managerFixtureSetup) {
		s.Config.Worktree.Undefined = "hot"
		s.Config.Pool.WarmPerWorkspace = 1
	})
	cfg, store, m := f.Config, f.Store, f.Manager
	m.git.SetTimeout(10 * time.Second)
	repoPath := filepath.Join(f.Root, "repo")
	initGitRepo(t, repoPath)
	ctx := context.Background()
	discoverer := discovery.Discoverer{Git: m.git, Config: cfg}
	w, err := discoverer.Resolve(ctx, repoPath)
	if err != nil {
		t.Fatal(err)
	}
	w = registerTestWorkspace(t, store, w)
	raw := openTestDatabase(t, f.DatabasePath)
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
	candidates, err := store.ColdRepositoryCandidates(ctx, state.FormatTime(time.Now().Add(time.Hour)))
	if err != nil || len(candidates) != 1 {
		t.Fatalf("cold candidates=%+v err=%v", candidates, err)
	}
	job, changed, err := store.ScheduleColdRepositoryRemoval(ctx, candidates[0])
	if err != nil || !changed {
		t.Fatalf("schedule cold job=%+v changed=%v err=%v", job, changed, err)
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
	slot, err := store.Slot(ctx, ready.ID)
	repository, repositoryErr := store.SlotRepository(ctx, ready.ID, candidates[0].RepositoryID)
	if err != nil || repositoryErr != nil || slot.State != "READY" || repository.State != "COLD" {
		t.Fatalf("cold state slot=%+v repository=%+v err=%v repositoryErr=%v", slot, repository, err, repositoryErr)
	}
	if _, err := os.Lstat(filepath.Join(ready.Path, repository.DirName)); !os.IsNotExist(err) {
		t.Fatalf("retired repository worktree still exists: %v", err)
	}
	entries, err := os.ReadDir(ready.Path)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != workspace.OwnershipMarkerName(candidates[0].RepositoryID) {
		names := make([]string, 0, len(entries))
		for _, entry := range entries {
			names = append(names, entry.Name())
		}
		t.Fatalf("retired slot directory contents=%v, want only the ownership marker", names)
	}
	lease, err := m.ResolveAndLease(ctx, repoPath, nil, "claude", 0, leaseAttrs{})
	if err != nil {
		t.Fatal(err)
	}
	if lease.SessionID != ready.ID {
		t.Fatalf("cold lease session=%q, want the READY slot %q", lease.SessionID, ready.ID)
	}
	want := filepath.Join(ready.Path, repository.DirName)
	if lease.Path != want {
		t.Fatalf("cold lease path=%q, want the repository directory %q", lease.Path, want)
	}
	if info, err := os.Lstat(lease.Path); err != nil || !info.IsDir() {
		t.Fatalf("cold lease path is not a directory: info=%+v err=%v", info, err)
	}
}

func TestWarmSlotLeaseHandsOutTheRepositoryDirectory(t *testing.T) {
	t.Parallel()
	requireDaemonIntegration(t)
	f := manualManagerFixture(t, func(s *managerFixtureSetup) {
		s.Config.Worktree.Undefined = "hot"
		s.Config.Pool.WarmPerWorkspace = 1
	})
	cfg, store, m := f.Config, f.Store, f.Manager
	m.git.SetTimeout(10 * time.Second)
	repoPath := filepath.Join(f.Root, "repo")
	initGitRepo(t, repoPath)
	ctx := context.Background()
	discoverer := discovery.Discoverer{Git: m.git, Config: cfg}
	w, err := discoverer.Resolve(ctx, repoPath)
	if err != nil {
		t.Fatal(err)
	}
	w = registerTestWorkspace(t, store, w)
	raw := openTestDatabase(t, f.DatabasePath)
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
	repositories, err := store.SlotRepositories(ctx, ready.ID)
	if err != nil || len(repositories) != 1 {
		t.Fatalf("slot repositories=%+v err=%v", repositories, err)
	}
	lease, err := m.ResolveAndLease(ctx, repoPath, nil, "claude", 0, leaseAttrs{})
	if err != nil {
		t.Fatal(err)
	}
	if lease.SessionID != ready.ID {
		t.Fatalf("lease session=%q, want the warm slot %q (a cold start would defeat the test)", lease.SessionID, ready.ID)
	}
	if !lease.Ready {
		t.Fatalf("warm lease reported not ready: %+v", lease)
	}
	want := filepath.Join(ready.Path, repositories[0].DirName)
	if lease.Path != want {
		t.Fatalf("warm lease path=%q, want the repository directory %q", lease.Path, want)
	}
	if _, err := os.Lstat(filepath.Join(lease.Path, ".git")); err != nil {
		t.Fatalf("warm lease path is not a Git worktree: %v", err)
	}
	markerName := workspace.OwnershipMarkerName(repositories[0].RepositoryID)
	if _, err := os.Lstat(filepath.Join(lease.Path, markerName)); !os.IsNotExist(err) {
		t.Fatalf("ownership marker is visible inside the leased directory: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(ready.Path, markerName)); err != nil {
		t.Fatalf("ownership marker is missing from the slot directory: %v", err)
	}
}

// ReadySlotは最古のREADYを決定的に返すため、2つのgoroutineは同じslotを選び得る。
// 負けた側はstate不一致として次のREADYへ回り、二重leaseもcold fallbackも起こさない。
func TestWarmPoolMaintainsCapacityAndNeverDoubleLeases(t *testing.T) {
	t.Parallel()
	requireDaemonIntegration(t)
	f := runningManagerFixture(t, func(s *managerFixtureSetup) {
		s.Config.Worktree.Undefined = "hot"
		s.Config.Pool.WarmPerWorkspace = 2
		s.Config.Pool.PreparationConcurrency = 3
		s.Config.Retention.HotStandby.Duration = time.Hour
		s.Config.Discovery.ReconcileInterval.Duration = time.Hour
	})
	cfg, store, m := f.Config, f.Store, f.Manager
	repo := filepath.Join(f.Root, "repo")
	initGitRepo(t, repo)
	ctx := context.Background()
	first, err := m.ResolveAndLease(ctx, repo, nil, "codex", os.Getpid(), leaseAttrs{})
	if err != nil {
		t.Fatal(err)
	}
	if err := waitReady(ctx, m, 10*time.Second, first.SessionID, first.Token); err != nil {
		t.Fatal(err)
	}
	firstSlot, err := store.Slot(ctx, first.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	// 貸出はReadySlotCountと同じ条件でしか候補を選ばないため、workspaceもgenerationも見ないStatusでは前提として弱い。
	waitUntil(t, 10*time.Second, func() bool {
		count, _ := store.ReadySlotCount(ctx, firstSlot.WorkspaceID)
		return count >= cfg.Pool.WarmPerWorkspace
	})

	// COLD の repository を含む待機枠は再利用しても Ready を返さないため、貸出前の状態を控える。
	// 控えないと、Ready でなかった原因が cold start への転落か COLD の再利用かを失敗ログから区別できない。
	standbyBefore := standbyRepositoryStates(t, store, firstSlot.WorkspaceID)

	leases := make(chan Lease, 2)
	errs := make(chan error, 2)
	for range 2 {
		go func() {
			lease, err := m.ResolveAndLease(ctx, repo, nil, "codex", os.Getpid(), leaseAttrs{})
			leases <- lease
			errs <- err
		}()
	}
	a, b := <-leases, <-leases
	if errA, errB := <-errs, <-errs; errA != nil || errB != nil {
		t.Fatalf("concurrent leases errors: %v, %v", errA, errB)
	}
	if a.SessionID == b.SessionID || a.Path == b.Path || a.SessionID == first.SessionID || b.SessionID == first.SessionID {
		t.Fatalf("slots were reused: first=%+v a=%+v b=%+v", first, a, b)
	}
	if !a.Ready || !b.Ready {
		t.Fatalf("warm leases were not ready: a=%+v b=%+v standby before the leases=%v", a, b, standbyBefore)
	}
	waitUntil(t, 10*time.Second, func() bool {
		count, _ := store.ReadySlotCount(ctx, firstSlot.WorkspaceID)
		return count == cfg.Pool.WarmPerWorkspace
	})
}

// standbyRepositoryStates は workspace の待機枠ごとに repository の state を返す。
func standbyRepositoryStates(t *testing.T, store *state.Store, workspaceID string) map[string][]string {
	t.Helper()
	slots, err := store.ReadySlots(context.Background(), workspaceID)
	if err != nil {
		t.Fatal(err)
	}
	states := map[string][]string{}
	for _, slot := range slots {
		repositories, err := store.SlotRepositories(context.Background(), slot.ID)
		if err != nil {
			t.Fatal(err)
		}
		for _, repository := range repositories {
			states[slot.ID] = append(states[slot.ID], repository.State)
		}
	}
	return states
}
