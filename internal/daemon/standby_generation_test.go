package daemon

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/HappyOnigiri/WX/internal/config"
	"github.com/HappyOnigiri/WX/internal/discovery"
)

func TestObsoleteStandbyPreparationDoesNotSuspendCurrentGeneration(t *testing.T) {
	t.Parallel()
	requireDaemonIntegration(t)
	root := t.TempDir()
	initGitRepo(t, filepath.Join(root, "service"))
	initGitRepo(t, filepath.Join(root, "web"))
	cfg := config.Defaults()
	cfg.Storage.WorktreeRoot = filepath.Join(root, "worktrees")
	cfg.Worktree.Undefined = "hot"
	cfg.Pool.WarmPerWorkspace = 1
	store, err := openTestStoreAtPath(t, filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	m := testManager(t, cfg, store)
	defer m.Close()
	ctx := context.Background()
	discoverer := discovery.Discoverer{Git: m.git, Config: cfg}
	w, err := discoverer.Resolve(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	w = registerTestWorkspace(t, store, w)
	if err := m.ensureStandby(ctx, w); err != nil {
		t.Fatal(err)
	}
	jobs, err := store.RecoverJobs(ctx, false)
	if err != nil || len(jobs) != 1 || jobs[0].Kind != "PREPARE" {
		t.Fatalf("standby jobs=%+v err=%v", jobs, err)
	}
	oldJob, err := store.ClaimJob(ctx, jobs[0].ID, "test")
	if err != nil {
		t.Fatal(err)
	}
	// 準備ジョブを取得した後に構成を更新し、旧世代の中断が新世代の補充より先に処理される順序を固定する。
	initGitRepo(t, filepath.Join(root, "api"))
	updated, err := discoverer.Resolve(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	updated, generation, err := store.UpsertWorkspaceGeneration(ctx, updated)
	if err != nil || generation != 2 {
		t.Fatalf("updated generation=%d err=%v", generation, err)
	}
	oldSlot, err := store.Slot(ctx, oldJob.SlotID)
	if err != nil || oldSlot.State != "STALE" {
		t.Fatalf("obsolete slot=%+v err=%v", oldSlot, err)
	}
	runErr := m.runRecoveredJob(ctx, oldJob)
	if err := store.FinishJob(ctx, oldJob.ID, "test", runErr); err != nil {
		t.Fatal(err)
	}
	if suspended, err := store.ReplenishSuspended(ctx, string(updated.ID)); err != nil || suspended {
		t.Fatalf("obsolete preparation suspended current generation: suspended=%v err=%v prepare_error=%v", suspended, err, runErr)
	}
	if err := m.ensureStandby(ctx, updated); err != nil {
		t.Fatal(err)
	}
	jobs, err = store.RecoverJobs(ctx, false)
	if err != nil || len(jobs) != 1 || jobs[0].Kind != "PREPARE" || jobs[0].SlotID == oldJob.SlotID {
		t.Fatalf("current generation jobs=%+v err=%v", jobs, err)
	}
	if err := m.runRecoveredJob(ctx, jobs[0]); err != nil {
		t.Fatal(err)
	}
	ready, ok, err := store.ReadySlot(ctx, string(updated.ID))
	if err != nil || !ok || ready.Generation != 2 {
		t.Fatalf("current generation ready slot=%+v found=%v err=%v", ready, ok, err)
	}
	repos, err := store.SlotRepositories(ctx, ready.ID)
	if err != nil || len(repos) != 3 {
		t.Fatalf("current generation repositories=%+v err=%v", repos, err)
	}
}
