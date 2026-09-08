package daemon

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/HappyOnigiri/WX/internal/archive"
	"github.com/HappyOnigiri/WX/internal/discovery"
	"github.com/HappyOnigiri/WX/internal/domain"
	"github.com/HappyOnigiri/WX/internal/pool"
	"github.com/HappyOnigiri/WX/internal/state"
)

func TestCrashRecoveryConvergesAfterReadyAndRefsExist(t *testing.T) {
	t.Parallel()
	requireDaemonIntegration(t)
	f := manualManagerFixture(t, func(s *managerFixtureSetup) {
		s.Config.Pool.WarmPerWorkspace = 0
	})
	cfg, store, m := f.Config, f.Store, f.Manager
	runner := m.git
	runner.SetTimeout(10 * time.Second)
	repoPath := filepath.Join(f.Root, "repo")
	initGitRepo(t, repoPath)
	discoverer := discovery.Discoverer{Git: runner, Config: cfg}
	ctx := context.Background()
	w, err := discoverer.Resolve(ctx, repoPath)
	if err != nil {
		t.Fatal(err)
	}
	w = registerTestWorkspace(t, store, w)
	resolved, err := pool.ResolveBranches(ctx, runner, w, nil)
	if err != nil {
		t.Fatal(err)
	}
	id, err := domain.NewID()
	if err != nil {
		t.Fatal(err)
	}
	slotRelative, err := slotRelPath(string(w.ID), id)
	if err != nil {
		t.Fatal(err)
	}
	slotRoot := filepath.Join(cfg.Storage.WorktreeRoot, slotRelative)
	if err := os.MkdirAll(slotRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	repos, err := m.slotRepos(slotRoot, w, resolved, 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	session := state.Session{ID: id, WorkspaceID: string(w.ID), SlotID: id, State: "STARTING", AgentKind: "codex", TokenHash: state.HashToken("token")}
	if err := os.MkdirAll(slotRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	slot := storeSlotAt(t, store, cfg.Storage.WorktreeRoot, string(w.ID), id, slotRoot, 1, "PREPARING")
	prepareJob, err := store.CreateSlotSession(ctx, slot, repos, session, "PREPARE")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ClaimJob(ctx, prepareJob.ID, "crashed-daemon"); err != nil {
		t.Fatal(err)
	}
	preparer := descriptorBoundPreparerForTest(t, runner, cfg, store, slot)
	if err := m.prepareSlot(ctx, id, w, resolved, repos); err != nil {
		t.Fatal(err)
	}
	jobs, err := store.RecoverJobs(ctx, true)
	if err != nil || len(jobs) != 1 {
		t.Fatalf("recover prepare jobs=%+v err=%v", jobs, err)
	}
	claimedPrepare, err := store.ClaimJob(ctx, jobs[0].ID, "restarted-daemon")
	if err != nil {
		t.Fatal(err)
	}
	if err := m.runRecoveredJob(ctx, claimedPrepare); err != nil {
		t.Fatalf("recover after worktree creation: %v", err)
	}
	if err := store.FinishJob(ctx, claimedPrepare.ID, "restarted-daemon", nil); err != nil {
		t.Fatal(err)
	}
	prepared, err := store.Slot(ctx, id)
	if err != nil || prepared.State != "LEASED" {
		t.Fatalf("prepared slot=%+v err=%v", prepared, err)
	}

	snapshotJob, changed, err := store.Release(ctx, id, string(w.ID), id)
	if err != nil || !changed {
		t.Fatalf("release changed=%v err=%v", changed, err)
	}
	if _, err := store.ClaimJob(ctx, snapshotJob.ID, "crashed-daemon"); err != nil {
		t.Fatal(err)
	}
	released, err := store.SessionByID(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	releasedAt, err := time.Parse(time.RFC3339Nano, released.ReleasedAt)
	if err != nil {
		t.Fatal(err)
	}
	archiveManager := archive.Manager{Git: runner, Preparer: &preparer, Ownership: store}
	first, err := archiveManager.SnapshotWithPersistence(ctx, resolved[0].Repository, repos[0].WorktreePath, id, releasedAt.Add(cfg.Retention.RecoverySnapshot.Duration), nil)
	if err != nil {
		t.Fatal(err)
	}
	jobs, err = store.RecoverJobs(ctx, true)
	if err != nil || len(jobs) != 1 {
		t.Fatalf("recover snapshot jobs=%+v err=%v", jobs, err)
	}
	for _, job := range jobs {
		if err := m.runRecoveredJob(ctx, job); err != nil {
			t.Fatalf("replay %s: %v", job.Kind, err)
		}
	}
	snapshots, err := store.Snapshots(ctx, id)
	if err != nil || len(snapshots) != 1 || snapshots[0].ID != first.ID || snapshots[0].WorktreeOID != first.WorktreeOID {
		t.Fatalf("recovered snapshots=%+v first=%+v err=%v", snapshots, first, err)
	}
}
