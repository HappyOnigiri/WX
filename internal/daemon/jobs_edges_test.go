package daemon

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/HappyOnigiri/WX/internal/archive"
	"github.com/HappyOnigiri/WX/internal/discovery"
	"github.com/HappyOnigiri/WX/internal/state"
)

func TestManagerIdempotentJobsAndOwnershipRejections(t *testing.T) {
	t.Parallel()
	f := manualManagerFixture(t)
	root, cfg, store, m := f.Root, f.Config, f.Store, f.Manager
	ctx := context.Background()
	w := discovery.Workspace{Root: discoveryPath(root), Kind: "repository", Repositories: []discovery.Repository{{ID: "repository", MainPath: discoveryPath(filepath.Join(root, "repository")), CommonDir: discoveryPath(filepath.Join(root, "repository", ".git")), DefaultBranch: "main"}}}
	w = registerTestWorkspace(t, store, w)

	readyJob, err := store.CreateStandby(ctx, slotAtPath(t, m, string(w.ID), "ready", filepath.Join(cfg.Storage.WorktreeRoot, "ready"), 1, "PREPARING"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetSlotState(ctx, "ready", []string{"PREPARING"}, "READY", ""); err != nil {
		t.Fatal(err)
	}
	if err := m.prepareSlot(ctx, "ready", discovery.Workspace{}, nil, nil); err != nil {
		t.Fatalf("READY prepare replay: %v", err)
	}
	if err := m.restoreSlot(ctx, "ready", discovery.Workspace{}, nil, nil, nil); err != nil {
		t.Fatalf("READY restore replay: %v", err)
	}
	claimed, err := store.ClaimJob(ctx, readyJob.ID, "test")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.FinishJob(ctx, claimed.ID, "test", nil); err != nil {
		t.Fatal(err)
	}
	if err := store.SetSlotState(ctx, "ready", []string{"READY"}, "FAILED", "TEST"); err != nil {
		t.Fatal(err)
	}
	if err := m.prepareSlot(ctx, "ready", discovery.Workspace{}, nil, nil); err == nil {
		t.Fatal("FAILED slot preparation replay succeeded")
	}
	if err := m.restoreSlot(ctx, "ready", discovery.Workspace{}, nil, nil, nil); err == nil {
		t.Fatal("FAILED slot restore replay succeeded")
	}
	if _, err := m.resolvedFromStored(ctx, discovery.Workspace{}, []state.SlotRepository{{RepositoryID: "removed"}}); err == nil {
		t.Fatal("removed repository resolved from stored state")
	}

	coldRepo := state.SlotRepository{RepositoryID: "repository", DirName: "repo", State: "COLD"}
	if _, err := store.CreateStandby(ctx, slotAtPath(t, m, string(w.ID), "cold", filepath.Join(cfg.Storage.WorktreeRoot, "cold"), 1, "PREPARING"), []state.SlotRepository{coldRepo}); err != nil {
		t.Fatal(err)
	}
	if err := m.removeColdRepositoryJob(ctx, state.Job{SlotID: "cold", RepositoryID: "repository"}); err != nil {
		t.Fatalf("COLD removal replay: %v", err)
	}
	if err := m.removeColdRepositoryJob(ctx, state.Job{SlotID: "cold", RepositoryID: "missing"}); err == nil {
		t.Fatal("missing cold repository removal succeeded")
	}
	if err := m.removeSlotWorktrees(ctx, archive.Manager{}, filepath.Join(root, "owned"), state.Slot{ID: "cold", Path: filepath.Join(root, "outside")}, ""); err == nil {
		t.Fatal("outside slot worktree removal succeeded")
	}
	if snapshotsUsable([]state.Snapshot{{ExpiresAt: "invalid"}}, time.Now()) {
		t.Fatal("invalid snapshot expiry was usable")
	}
}
