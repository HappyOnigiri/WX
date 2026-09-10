package daemon

import (
	"path/filepath"
	"testing"

	"github.com/HappyOnigiri/WX/internal/domain"
	"github.com/HappyOnigiri/WX/internal/state"
)

func TestScheduleColdRepositoryRemovalsSurvivesQuarantineStorageFailure(t *testing.T) {
	t.Parallel()
	ctx, manager, store, workspaceRecord, resolved, databasePath := managerCoverageFixture(t)
	slotID := domain.StableID("cold-schedule", "quarantine-fault")
	outsidePath := filepath.Join(t.TempDir(), "outside-worktree-fault")
	if _, err := store.CreateStandby(ctx,
		slotAtPath(t, manager, string(workspaceRecord.ID), slotID, filepath.Join(manager.Config().Storage.WorktreeRoot, "cold-schedule", slotID, "root"), 1, "RETIRING"),
		[]state.SlotRepository{{RepositoryID: string(resolved[0].Repository.ID), DirName: "repository", State: "RETIRING", BaseOID: resolved[0].OID}}); err != nil {
		t.Fatal(err)
	}

	raw := openTestDatabase(t, databasePath)
	if _, err := raw.ExecContext(ctx, `CREATE TRIGGER fail_quarantine_cleanup BEFORE UPDATE ON slots WHEN NEW.state='QUARANTINED' BEGIN SELECT RAISE(ABORT,'injected quarantine failure'); END`); err != nil {
		t.Fatal(err)
	}

	candidates := []state.ColdRepositoryCandidate{{SlotID: slotID, WorkspaceID: string(workspaceRecord.ID), RepositoryID: string(resolved[0].Repository.ID), WorktreePath: outsidePath}}
	if result := manager.scheduleColdRepositoryRemovals(ctx, candidates, map[string]bool{}); result.Scheduled != 0 {
		t.Fatalf("unverifiable cold repository was scheduled despite injected failure: result=%+v", result)
	}
	if slot, err := store.Slot(ctx, slotID); err != nil || slot.State != "RETIRING" {
		t.Fatalf("slot state changed despite injected quarantine failure: slot=%+v err=%v", slot, err)
	}
}

func TestScheduleColdRepositoryRemovalsKeepsAlreadyRetiringSlot(t *testing.T) {
	t.Parallel()
	ctx, manager, store, workspaceRecord, resolved, _ := managerCoverageFixture(t)
	slotID := domain.StableID("cold-schedule", "outside")
	outsidePath := filepath.Join(t.TempDir(), "outside-worktree")
	if _, err := store.CreateStandby(ctx,
		slotAtPath(t, manager, string(workspaceRecord.ID), slotID, filepath.Join(manager.Config().Storage.WorktreeRoot, "cold-schedule", slotID, "root"), 1, "RETIRING"),
		[]state.SlotRepository{{RepositoryID: string(resolved[0].Repository.ID), DirName: "repository", State: "RETIRING", BaseOID: resolved[0].OID}}); err != nil {
		t.Fatal(err)
	}
	candidates := []state.ColdRepositoryCandidate{{SlotID: slotID, WorkspaceID: string(workspaceRecord.ID), RepositoryID: string(resolved[0].Repository.ID), WorktreePath: outsidePath}}
	if result := manager.scheduleColdRepositoryRemovals(ctx, candidates, map[string]bool{}); result.Scheduled != 0 {
		t.Fatalf("unverifiable cold repository was scheduled: result=%+v", result)
	}
	if slot, err := store.Slot(ctx, slotID); err != nil || slot.State != "RETIRING" {
		t.Fatalf("unverifiable cold repository slot=%+v err=%v", slot, err)
	}
}
