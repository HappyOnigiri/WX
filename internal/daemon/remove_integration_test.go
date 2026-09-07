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

func TestRemovalJobReplaysAfterPhysicalDeletionBeforeStateCommit(t *testing.T) {
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
	ctx := context.Background()
	discoverer := discovery.Discoverer{Git: runner, Config: cfg}
	w, err := discoverer.Resolve(ctx, repoPath)
	if err != nil {
		t.Fatal(err)
	}
	w = registerTestWorkspace(t, store, w)
	resolved, err := pool.ResolveBranches(ctx, runner, w, nil)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := domain.NewID()
	slotRelative, err := slotRelPath(string(w.ID), id)
	if err != nil {
		t.Fatal(err)
	}
	slotRoot := filepath.Join(cfg.Storage.WorktreeRoot, slotRelative)
	repos := []state.SlotRepository{{RepositoryID: string(w.Repositories[0].ID), DirName: testDirName(w.Repositories[0], cfg), State: "PREPARING", RequestedRef: resolved[0].RequestedRef, BaseOID: resolved[0].OID, Fingerprint: "test"}}
	session := state.Session{ID: id, WorkspaceID: string(w.ID), SlotID: id, State: "STARTING", AgentKind: "codex", TokenHash: state.HashToken("token")}
	if err := os.MkdirAll(slotRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	slot := storeSlotAt(t, store, cfg.Storage.WorktreeRoot, string(w.ID), id, slotRoot, 1, "PREPARING")
	job, err := store.CreateSlotSession(ctx, slot, repos, session, "PREPARE")
	if err != nil {
		t.Fatal(err)
	}
	preparer := descriptorBoundPreparerForTest(t, runner, cfg, store, slot)
	if err := preparer.Prepare(ctx, w.Repositories[0], filepath.Join(slotRoot, repos[0].DirName), resolved[0].OID, id); err != nil {
		t.Fatal(err)
	}
	claimed, err := store.ClaimJob(ctx, job.ID, "setup")
	if err != nil {
		t.Fatal(err)
	}
	if err := m.prepareSlot(ctx, id, w, resolved, repos); err != nil {
		t.Fatal(err)
	}
	if err := store.FinishJob(ctx, claimed.ID, "setup", nil); err != nil {
		t.Fatal(err)
	}
	snapshotJob, changed, err := store.Release(ctx, id, string(w.ID), id)
	if err != nil || !changed {
		t.Fatalf("release changed=%v err=%v", changed, err)
	}
	claimedSnapshot, err := store.ClaimJob(ctx, snapshotJob.ID, "setup")
	if err != nil {
		t.Fatal(err)
	}
	archiveManager := archive.Manager{Git: runner, Preparer: &preparer, Ownership: store}
	expires := time.Now().Add(time.Hour)
	snapshot, err := archiveManager.SnapshotWithPersistence(ctx, w.Repositories[0], filepath.Join(slotRoot, repos[0].DirName), id, expires, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SaveSnapshot(ctx, snapshot); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkArchived(ctx, id, id, expires.Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	if err := store.FinishJob(ctx, claimedSnapshot.ID, "setup", nil); err != nil {
		t.Fatal(err)
	}
	removeJob, changed, err := store.ScheduleRemoval(ctx, id, id)
	if err != nil || !changed {
		t.Fatalf("schedule removal changed=%v err=%v", changed, err)
	}
	if _, err := store.ClaimJob(ctx, removeJob.ID, "crashed-daemon"); err != nil {
		t.Fatal(err)
	}
	removalSlot, err := store.Slot(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.removeSlotWorktrees(ctx, archiveManager, cfg.Storage.WorktreeRoot, removalSlot, id); err != nil {
		t.Fatal(err)
	}
	if slot, _ := store.Slot(ctx, id); slot.State != "REMOVING" {
		t.Fatalf("state was committed before simulated crash: %s", slot.State)
	}
	recovered, err := store.RecoverJobs(ctx, true)
	if err != nil || len(recovered) != 1 {
		t.Fatalf("recovered removal jobs=%+v err=%v", recovered, err)
	}
	if err := m.runRecoveredJob(ctx, recovered[0]); err != nil {
		t.Fatalf("replay removal: %v", err)
	}
	if slot, _ := store.Slot(ctx, id); slot.State != "ARCHIVED" {
		t.Fatalf("replayed removal state=%s", slot.State)
	}
}
