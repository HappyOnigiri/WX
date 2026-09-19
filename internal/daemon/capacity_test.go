package daemon

import (
	"errors"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/HappyOnigiri/WX/internal/diag"
	"github.com/HappyOnigiri/WX/internal/domain"
	"github.com/HappyOnigiri/WX/internal/state"
	"github.com/HappyOnigiri/WX/internal/workspace"
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

func TestEnforcePrepareCapacityAllowsExactCapacity(t *testing.T) {
	t.Parallel()
	ctx, manager, store, workspaceRecord, _, _ := managerCoverageFixture(t, "repository")
	manager.freeSpace = func(*os.File) (string, int64, error) { return "test-volume", 0, nil }
	slotID := domain.StableID("capacity", "exact")
	slot := testSlot(t, manager, string(workspaceRecord.ID), slotID, 1, "PREPARING")
	if _, err := store.CreateStandby(ctx, slot, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.enforcePrepareCapacity(ctx, slot, workspaceRecord, nil, nil, manager.Config()); err != nil {
		t.Fatalf("exact capacity was rejected: %v", err)
	}
	got, err := store.Slot(ctx, slotID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != "PREPARING" {
		t.Fatalf("slot after exact capacity check=%+v, want PREPARING", got)
	}
}

func TestCapacityMathHandlesZeroAndOverflowBoundaries(t *testing.T) {
	t.Parallel()
	if got := capacityMul(0, 1); got != 0 {
		t.Fatalf("capacityMul(0, 1)=%d, want zero", got)
	}
	if got := capacityMul(2, 3); got != 6 {
		t.Fatalf("capacityMul(2, 3)=%d, want six", got)
	}
	if got := capacityMul(math.MaxInt64, 2); got != math.MaxInt64 {
		t.Fatalf("capacityMul overflow=%d, want MaxInt64", got)
	}
	if got := capacityAdd(math.MaxInt64, 1); got != math.MaxInt64 {
		t.Fatalf("capacityAdd overflow=%d, want MaxInt64", got)
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

func TestCapacityReportFindingsDoesNotAddSharedBytesWithoutWarmSlots(t *testing.T) {
	t.Parallel()
	report := CapacityReport{Volumes: []CapacityVolume{{Volume: "v", Target: "/worktrees", Required: 100, Free: 100, WorktreeRequired: 100, SharedRequired: 50}}}
	findings := capacityReportFindings("/workspace", report, 0)
	if len(findings) != 1 || len(findings[0].Details) < 2 {
		t.Fatalf("findings=%+v", findings)
	}
	if findings[0].Details[1] != "warm_count 0: 0 B" {
		t.Fatalf("warm detail=%q, want no standby capacity", findings[0].Details[1])
	}
}

func TestPrepareCapacityFindingsEstimateRegisteredWorkspace(t *testing.T) {
	t.Parallel()
	ctx, manager, _, _, _, _ := managerCoverageFixture(t, "repository")
	manager.freeSpace = func(*os.File) (string, int64, error) { return "test-volume", 1 << 40, nil }

	findings := manager.prepareCapacityFindings(ctx)
	if len(findings) == 0 {
		t.Fatal("prepareCapacityFindings returned no finding")
	}
	for _, finding := range findings {
		if finding.Severity == diag.SeverityUnchecked {
			t.Fatalf("registered workspace %s was left unchecked: %s", finding.Target, finding.Cause)
		}
	}
	if findings[0].Severity != diag.SeverityInfo {
		t.Fatalf("finding severity=%s, want info: %+v", findings[0].Severity, findings[0])
	}
}

func TestAuditC1DoctorCacheRefreshAfterExternalLFSFetch(t *testing.T) {
	t.Parallel()
	ctx, manager, _, workspaceRecord, resolved, _ := managerCoverageFixture(t, "repository")
	repo := resolved[0].Repository
	pointerOID := strings.Repeat("a", 64)
	pointer := "version https://git-lfs.github.com/spec/v1\noid sha256:" + pointerOID + "\nsize 123\n"
	if err := os.WriteFile(filepath.Join(string(repo.MainPath), ".gitattributes"), []byte("*.bin filter=lfs\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(string(repo.MainPath), "asset.bin"), []byte(pointer), 0o600); err != nil {
		t.Fatal(err)
	}
	gitRun(t, string(repo.MainPath), "add", ".")
	gitRun(t, string(repo.MainPath), "commit", "-m", "lfs pointer")
	oid := gitOutput(t, string(repo.MainPath), "rev-parse", "HEAD")
	preparer := &workspace.Preparer{Git: manager.git, Config: manager.Config(), WorkspaceRoot: string(workspaceRecord.Root)}
	first, err := manager.estimateCapacity(ctx, preparer, manager.Config(), repo, oid)
	if err != nil {
		t.Fatal(err)
	}
	if first.MissingLFSObjects != 1 || len(first.LFS) != 1 || first.LFS[0].CacheState != workspace.LFSCacheMissing {
		t.Fatalf("first estimate=%+v, want one missing LFS object", first)
	}
	cachePath := filepath.Join(string(repo.CommonDir), "lfs", "objects", pointerOID[:2], pointerOID[2:4], pointerOID)
	if err := os.MkdirAll(filepath.Dir(cachePath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cachePath, make([]byte, 123), 0o600); err != nil {
		t.Fatal(err)
	}
	second, err := manager.estimateCapacity(ctx, preparer, manager.Config(), repo, oid)
	if err != nil {
		t.Fatal(err)
	}
	if second.MissingLFSObjects != 0 || second.LFSCacheBytes != 0 || second.LFS[0].CacheState != workspace.LFSCacheHealthy {
		t.Fatalf("refreshed estimate=%+v, want healthy cache after external fetch", second)
	}
	if err := os.Remove(cachePath); err != nil {
		t.Fatal(err)
	}
	third, err := manager.estimateCapacity(ctx, preparer, manager.Config(), repo, oid)
	if err != nil {
		t.Fatal(err)
	}
	if third.MissingLFSObjects != 1 || third.LFS[0].CacheState != workspace.LFSCacheMissing {
		t.Fatalf("refreshed estimate=%+v, want missing cache after external removal", third)
	}
}
