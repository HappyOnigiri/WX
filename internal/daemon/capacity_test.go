package daemon

import (
	"errors"
	"os"
	"testing"

	"github.com/HappyOnigiri/WX/internal/diag"
	"github.com/HappyOnigiri/WX/internal/domain"
	"github.com/HappyOnigiri/WX/internal/state"
)

func TestPrepareCapacityShortageFailsBeforeStagedPreparation(t *testing.T) {
	t.Parallel()
	ctx, manager, store, workspaceRecord, resolved, _ := managerCoverageFixture(t, "repository")
	manager.freeSpace = func(*os.File) (string, int64, error) { return "test-volume", 0, nil }

	slotID := domain.StableID("capacity", "insufficient")
	slot := testSlot(t, manager, string(workspaceRecord.ID), slotID, 1, "PREPARING")
	if _, err := store.CreateStandby(ctx, slot, []state.SlotRepository{{
		RepositoryID: string(resolved[0].Repository.ID),
		DirName:      testDirName(resolved[0].Repository, manager.Config()),
		State:        "PREPARING",
		RequestedRef: resolved[0].RequestedRef,
		BaseOID:      resolved[0].OID,
	}}); err != nil {
		t.Fatal(err)
	}
	err := manager.prepareSlot(ctx, slotID, workspaceRecord, resolved, []state.SlotRepository{{
		RepositoryID: string(resolved[0].Repository.ID),
		State:        "PREPARING",
		RequestedRef: resolved[0].RequestedRef,
		BaseOID:      resolved[0].OID,
	}})
	var capacityErr *InsufficientPrepareSpaceError
	if !errors.As(err, &capacityErr) {
		t.Fatalf("prepare error=%v, want insufficient capacity", err)
	}
	got, err := store.Slot(ctx, slotID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != "FAILED" || got.FailureCode != "PREPARE_INSUFFICIENT_SPACE" {
		t.Fatalf("slot after preflight=%+v, want FAILED with capacity code", got)
	}
	if got.PreparationStartedAt != "" {
		t.Fatalf("preflight recorded staged preparation start: %q", got.PreparationStartedAt)
	}
}

func TestCapacityReportFindingsTreatSparseCheckoutAsNonBlocking(t *testing.T) {
	t.Parallel()
	report := CapacityReport{
		Sparse:  true,
		Volumes: []CapacityVolume{{Volume: "v", Target: "/worktrees", Required: 100, Free: 1}},
	}
	findings := capacityReportFindings("/workspace", report, 1)
	if len(findings) != 1 {
		t.Fatalf("findings=%d, want one", len(findings))
	}
	if findings[0].Severity != diag.SeverityInfo {
		t.Fatalf("sparse finding severity=%s, want info", findings[0].Severity)
	}
}

func TestCapacityReportFindingsCountsSharedLFSCacheOnce(t *testing.T) {
	t.Parallel()
	report := CapacityReport{Volumes: []CapacityVolume{{Volume: "v", Target: "/worktrees", Required: 150, Free: 251, WorktreeRequired: 100, SharedRequired: 50}}}
	findings := capacityReportFindings("/workspace", report, 2)
	if len(findings) != 1 || len(findings[0].Details) < 2 {
		t.Fatalf("findings=%+v", findings)
	}
	if findings[0].Details[1] != "warm_count 2: 250 B" {
		t.Fatalf("warm detail=%q, want shared cache counted once", findings[0].Details[1])
	}
}
