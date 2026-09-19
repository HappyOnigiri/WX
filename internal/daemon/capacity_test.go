package daemon

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
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

func TestSparsePrepareStillVerifiesLFSPaths(t *testing.T) {
	t.Parallel()
	ctx, manager, store, workspaceRecord, resolved, _ := managerCoverageFixture(t, "repository")
	repository := string(resolved[0].Repository.MainPath)
	payload := []byte(strings.Repeat("payload", 18))
	digest := sha256.Sum256(payload)
	oid := hex.EncodeToString(digest[:])
	pointer := fmt.Sprintf("version https://git-lfs.github.com/spec/v1\noid sha256:%s\nsize %d\n", oid, len(payload))
	if err := os.WriteFile(filepath.Join(repository, ".gitattributes"), []byte("*.bin filter=lfs\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repository, "asset.bin"), []byte(pointer), 0o600); err != nil {
		t.Fatal(err)
	}
	gitRun(t, repository, "add", ".")
	gitRun(t, repository, "commit", "-m", "add sparse LFS pointer")
	gitRun(t, repository, "config", "core.sparseCheckout", "true")
	cachePath := filepath.Join(string(resolved[0].Repository.CommonDir), "lfs", "objects", oid[:2], oid[2:4], oid)
	if err := os.MkdirAll(filepath.Dir(cachePath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cachePath, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	resolved[0].OID = gitOutput(t, repository, "rev-parse", "HEAD")
	manager.freeSpace = func(*os.File) (string, int64, error) { return "test-volume", 1 << 40, nil }

	slotID := domain.StableID("capacity", "sparse-lfs")
	slot := testSlotRow(t, manager, string(workspaceRecord.ID), slotID, 1, "PREPARING")
	slotIdentity, _, err := manager.createSlotRoot(slot.Path, slot.Path)
	if err != nil {
		t.Fatalf("create slot root: %v", err)
	}
	slot.DirIdentity = slotIdentity
	metadata := state.SlotRepository{
		RepositoryID: string(resolved[0].Repository.ID),
		DirName:      testDirName(resolved[0].Repository, manager.Config()),
		State:        "PREPARING",
		RequestedRef: resolved[0].RequestedRef,
		BaseOID:      resolved[0].OID,
	}
	if _, err := store.CreateStandby(ctx, slot, []state.SlotRepository{metadata}); err != nil {
		t.Fatal(err)
	}
	err = manager.prepareSlot(ctx, slotID, workspaceRecord, resolved, []state.SlotRepository{metadata})
	if err == nil || !strings.Contains(err.Error(), "LFS path asset.bin") {
		t.Fatalf("sparse prepare error=%v, want LFS path verification failure", err)
	}
}

func TestSparsePrepareSkipsExcludedRequestedLFSPath(t *testing.T) {
	t.Parallel()
	ctx, manager, store, workspaceRecord, resolved, _ := managerCoverageFixture(t, "repository")
	repository := string(resolved[0].Repository.MainPath)
	if err := os.WriteFile(filepath.Join(repository, ".gitattributes"), []byte("*.bin filter=lfs\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	insidePath := filepath.Join(repository, "inside", "kept.txt")
	if err := os.MkdirAll(filepath.Dir(insidePath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(insidePath, []byte("kept\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitRun(t, repository, "add", ".")
	gitRun(t, repository, "commit", "-m", "add sparse source path")
	gitRun(t, repository, "checkout", "-b", "requested")
	outsidePath := filepath.Join(repository, "outside", "asset.bin")
	if err := os.MkdirAll(filepath.Dir(outsidePath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(outsidePath, []byte("version https://git-lfs.github.com/spec/v1\noid sha256:"+strings.Repeat("a", 64)+"\nsize 123\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitRun(t, repository, "add", ".")
	gitRun(t, repository, "commit", "-m", "add requested sparse LFS path")
	requestedOID := gitOutput(t, repository, "rev-parse", "HEAD")
	gitRun(t, repository, "checkout", "main")
	gitRun(t, repository, "config", "core.sparseCheckout", "true")
	gitRun(t, repository, "sparse-checkout", "set", "--cone", "inside")
	resolved[0].OID = requestedOID
	manager.freeSpace = func(*os.File) (string, int64, error) { return "test-volume", 1 << 40, nil }

	slotID := domain.StableID("capacity", "sparse-lfs-excluded")
	slot := testSlotRow(t, manager, string(workspaceRecord.ID), slotID, 1, "PREPARING")
	slotIdentity, _, err := manager.createSlotRoot(slot.Path, slot.Path)
	if err != nil {
		t.Fatalf("create slot root: %v", err)
	}
	slot.DirIdentity = slotIdentity
	metadata := state.SlotRepository{
		RepositoryID: string(resolved[0].Repository.ID),
		DirName:      testDirName(resolved[0].Repository, manager.Config()),
		State:        "PREPARING",
		RequestedRef: resolved[0].RequestedRef,
		BaseOID:      requestedOID,
	}
	if _, err := store.CreateStandby(ctx, slot, []state.SlotRepository{metadata}); err != nil {
		t.Fatal(err)
	}
	if err := manager.prepareSlot(ctx, slotID, workspaceRecord, resolved, []state.SlotRepository{metadata}); err != nil {
		t.Fatalf("sparse prepare with excluded requested LFS path: %v", err)
	}
	prepared, err := store.Slot(ctx, slotID)
	if err != nil {
		t.Fatal(err)
	}
	if prepared.State != "READY" {
		t.Fatalf("prepared slot=%+v, want READY", prepared)
	}
}

func TestSparsePrepareStillPreflightsMissingLFSObjects(t *testing.T) {
	t.Parallel()
	ctx, manager, store, workspaceRecord, resolved, _ := managerCoverageFixture(t, "repository")
	repository := string(resolved[0].Repository.MainPath)
	payload := []byte(strings.Repeat("payload", 18))
	digest := sha256.Sum256(payload)
	oid := hex.EncodeToString(digest[:])
	pointer := fmt.Sprintf("version https://git-lfs.github.com/spec/v1\noid sha256:%s\nsize %d\n", oid, len(payload))
	if err := os.WriteFile(filepath.Join(repository, ".gitattributes"), []byte("*.bin filter=lfs\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repository, "asset.bin"), []byte(pointer), 0o600); err != nil {
		t.Fatal(err)
	}
	gitRun(t, repository, "add", ".")
	gitRun(t, repository, "commit", "-m", "add sparse LFS pointer")
	gitRun(t, repository, "config", "core.sparseCheckout", "true")
	resolved[0].OID = gitOutput(t, repository, "rev-parse", "HEAD")
	manager.freeSpace = func(*os.File) (string, int64, error) { return "test-volume", 1 << 40, nil }

	slotID := domain.StableID("capacity", "sparse-lfs-missing")
	slot := testSlotRow(t, manager, string(workspaceRecord.ID), slotID, 1, "PREPARING")
	slotIdentity, _, err := manager.createSlotRoot(slot.Path, slot.Path)
	if err != nil {
		t.Fatalf("create slot root: %v", err)
	}
	slot.DirIdentity = slotIdentity
	metadata := state.SlotRepository{
		RepositoryID: string(resolved[0].Repository.ID),
		DirName:      testDirName(resolved[0].Repository, manager.Config()),
		State:        "PREPARING",
		RequestedRef: resolved[0].RequestedRef,
		BaseOID:      resolved[0].OID,
	}
	if _, err := store.CreateStandby(ctx, slot, []state.SlotRepository{metadata}); err != nil {
		t.Fatal(err)
	}
	_, err = manager.enforcePrepareCapacity(ctx, slot, workspaceRecord, resolved, []state.SlotRepository{metadata}, manager.Config())
	var missing *MissingLFSObjectsError
	if !errors.As(err, &missing) {
		t.Fatalf("sparse capacity error=%v, want missing LFS object", err)
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
	if first.MissingLFSObjects != 1 || first.LFSCacheBytes != 123 || first.LFS[0].Cached || first.LFS[0].CacheState != workspace.LFSCacheMissing {
		t.Fatalf("first estimate was mutated by refresh=%+v, want its original missing state", first)
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
	if second.MissingLFSObjects != 0 || second.LFSCacheBytes != 0 || !second.LFS[0].Cached || second.LFS[0].CacheState != workspace.LFSCacheHealthy {
		t.Fatalf("second estimate was mutated by later refresh=%+v, want its original healthy state", second)
	}
}

func TestCapacityCacheRecomputesSparseSelectionForNewSlots(t *testing.T) {
	t.Parallel()
	ctx, manager, store, workspaceRecord, resolved, _ := managerCoverageFixture(t, "repository")
	repository := string(resolved[0].Repository.MainPath)
	insideOID, outsideOID := strings.Repeat("a", 64), strings.Repeat("b", 64)
	if err := os.WriteFile(filepath.Join(repository, ".gitattributes"), []byte("*.bin filter=lfs\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	insidePath := filepath.Join(repository, "inside", "kept.bin")
	if err := os.MkdirAll(filepath.Dir(insidePath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(insidePath, []byte("version https://git-lfs.github.com/spec/v1\noid sha256:"+insideOID+"\nsize 123\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitRun(t, repository, "add", ".")
	gitRun(t, repository, "commit", "-m", "source LFS pointer")
	gitRun(t, repository, "checkout", "-b", "requested")
	outsidePath := filepath.Join(repository, "outside", "added.bin")
	if err := os.MkdirAll(filepath.Dir(outsidePath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(outsidePath, []byte("version https://git-lfs.github.com/spec/v1\noid sha256:"+outsideOID+"\nsize 123\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitRun(t, repository, "add", ".")
	gitRun(t, repository, "commit", "-m", "requested LFS pointer")
	requestedOID := gitOutput(t, repository, "rev-parse", "HEAD")
	gitRun(t, repository, "checkout", "main")
	resolved[0].OID = requestedOID
	for _, oid := range []string{insideOID, outsideOID} {
		cachePath := filepath.Join(string(resolved[0].Repository.CommonDir), "lfs", "objects", oid[:2], oid[2:4], oid)
		if err := os.MkdirAll(filepath.Dir(cachePath), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(cachePath, make([]byte, 123), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	manager.freeSpace = func(*os.File) (string, int64, error) { return "test-volume", 1 << 40, nil }
	metadata := func() state.SlotRepository {
		return state.SlotRepository{
			RepositoryID: string(resolved[0].Repository.ID),
			DirName:      testDirName(resolved[0].Repository, manager.Config()),
			State:        "PREPARING",
			RequestedRef: resolved[0].RequestedRef,
			BaseOID:      requestedOID,
		}
	}
	prepare := func(slotID string) CapacityReport {
		slot := testSlot(t, manager, string(workspaceRecord.ID), slotID, 1, "PREPARING")
		row := metadata()
		if _, err := store.CreateStandby(ctx, slot, []state.SlotRepository{row}); err != nil {
			t.Fatal(err)
		}
		report, err := manager.enforcePrepareCapacity(ctx, slot, workspaceRecord, resolved, []state.SlotRepository{row}, manager.Config())
		if err != nil {
			t.Fatalf("capacity preflight %s: %v", slotID, err)
		}
		return report
	}
	gitRun(t, repository, "sparse-checkout", "set", "--no-cone", "/inside/")
	first := prepare(domain.StableID("capacity-cache", "inside"))
	gitRun(t, repository, "sparse-checkout", "set", "--no-cone", "/outside/")
	second := prepare(domain.StableID("capacity-cache", "outside"))
	if len(first.Repositories) != 1 || len(first.Repositories[0].LFS) != 1 || first.Repositories[0].LFS[0].Paths[0] != "inside/kept.bin" {
		t.Fatalf("inside sparse report=%+v, want inside LFS path", first)
	}
	if len(second.Repositories) != 1 || len(second.Repositories[0].LFS) != 1 || second.Repositories[0].LFS[0].Paths[0] != "outside/added.bin" {
		t.Fatalf("outside sparse report=%+v, want outside LFS path", second)
	}
}
