package daemon

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/HappyOnigiri/WX/internal/discovery"
	"github.com/HappyOnigiri/WX/internal/domain"
	"github.com/HappyOnigiri/WX/internal/pool"
	"github.com/HappyOnigiri/WX/internal/state"
)

func TestGCExpiresSnapshotRefsOnlyAfterArchivingWorktree(t *testing.T) {
	t.Parallel()
	requireDaemonIntegration(t)
	f := runningManagerFixture(t, func(s *managerFixtureSetup) {
		s.Config.Pool.WarmPerWorkspace = 0
		s.Config.Retention.EndedWorktree.Duration = 0
		s.Config.Retention.RecoverySnapshot.Duration = -time.Hour
		s.Config.Discovery.ReconcileInterval.Duration = time.Hour
	})
	store, m := f.Store, f.Manager
	repo := filepath.Join(f.Root, "repo")
	initGitRepo(t, repo)
	ctx := context.Background()
	lease, err := m.ResolveAndLease(ctx, repo, nil, "codex", os.Getpid(), leaseAttrs{})
	if err != nil {
		t.Fatal(err)
	}
	if err := waitReady(ctx, m, 10*time.Second, lease.SessionID, lease.Token); err != nil {
		t.Fatal(err)
	}
	if err := m.Release(ctx, lease.SessionID, lease.Token, "test"); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, 10*time.Second, func() bool {
		session, _ := store.SessionByID(ctx, lease.SessionID)
		return session.State == "ARCHIVED"
	})
	snapshots, err := store.Snapshots(ctx, lease.SessionID)
	if err != nil || len(snapshots) != 1 {
		t.Fatalf("snapshots=%+v err=%v", snapshots, err)
	}
	if _, err := m.GC(ctx, false); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, 10*time.Second, func() bool {
		slot, _ := store.Slot(ctx, lease.SessionID)
		_, pathErr := os.Stat(lease.Path)
		return slot.State == "ARCHIVED" && os.IsNotExist(pathErr)
	})
	if _, err := m.GC(ctx, false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(lease.Path); !os.IsNotExist(err) {
		t.Fatalf("ended worktree still exists: %v", err)
	}
	session, err := store.SessionByID(ctx, lease.SessionID)
	if err != nil || session.State != "EXPIRED" {
		t.Fatalf("expired session=%+v err=%v", session, err)
	}
	if remaining, err := store.Snapshots(ctx, lease.SessionID); err != nil || len(remaining) != 0 {
		t.Fatalf("remaining snapshots=%+v err=%v", remaining, err)
	}
	for _, ref := range []string{snapshots[0].HeadRef, snapshots[0].WorktreeRef} {
		cmd := exec.Command("git", "show-ref", "--verify", ref)
		cmd.Dir = repo
		if err := cmd.Run(); err == nil {
			t.Fatalf("expired recovery ref still exists: %s", ref)
		}
	}
}

// 隔離 slot は retention を過ぎたら GC が通常の REMOVE で消す。
// 所有権を証明できないうちは実体を残して QUARANTINED へ戻し、証明が通る次の周回で片付く。
func TestGCRemovesRegisteredQuarantineWithoutCachedIdentity(t *testing.T) {
	t.Parallel()
	requireDaemonIntegration(t)
	f := manualManagerFixture(t, func(s *managerFixtureSetup) {
		s.Config.Pool.WarmPerWorkspace = 0
		s.Config.Retention.Quarantined.Duration = 0
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
	if err := os.MkdirAll(slotRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	repos := []state.SlotRepository{{RepositoryID: string(w.Repositories[0].ID), DirName: testDirName(w.Repositories[0], cfg), State: "PREPARING", RequestedRef: resolved[0].RequestedRef, BaseOID: resolved[0].OID, Fingerprint: "test"}}
	slot := storeSlotAt(t, store, cfg.Storage.WorktreeRoot, string(w.ID), id, slotRoot, 1, "PREPARING")
	prepareJob, err := store.CreateStandby(ctx, slot, repos)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.prepareSlot(ctx, id, w, resolved, repos); err != nil {
		t.Fatal(err)
	}
	claimedPrepare, err := store.ClaimJob(ctx, prepareJob.ID, "setup")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.FinishJob(ctx, claimedPrepare.ID, "setup", nil); err != nil {
		t.Fatal(err)
	}
	if err := store.SetSlotState(ctx, id, []string{"READY"}, "QUARANTINED", "WORKTREE_OWNERSHIP_UNCERTAIN"); err != nil {
		t.Fatal(err)
	}
	candidates, err := store.QuarantinedGCCandidates(ctx, state.FormatTime(time.Now().UTC()))
	if err != nil || len(candidates) != 1 || candidates[0].SlotID != id {
		t.Fatalf("quarantined candidates=%+v err=%v", candidates, err)
	}

	// manager の古い identity cache に依存せず、DB 登録済みの隔離実体を回収する。
	if _, changed, err := store.ScheduleQuarantinedRemoval(ctx, id); err != nil || !changed {
		t.Fatalf("schedule changed=%v err=%v", changed, err)
	}
	m.mu.Lock()
	m.roots, m.rootIdentities = map[string]bool{}, nil
	m.mu.Unlock()
	recovered, err := store.RecoverJobs(ctx, true)
	if err != nil || len(recovered) != 1 {
		t.Fatalf("removal jobs=%+v err=%v", recovered, err)
	}
	if err := m.runRecoveredJob(ctx, recovered[0]); err != nil {
		t.Fatal(err)
	}
	if stored, err := store.Slot(ctx, id); err != nil || stored.State != "ARCHIVED" {
		t.Fatalf("slot after removal=%+v err=%v", stored, err)
	}
	if _, err := os.Stat(slotRoot); !os.IsNotExist(err) {
		t.Fatalf("quarantined worktree still exists: %v", err)
	}
}
