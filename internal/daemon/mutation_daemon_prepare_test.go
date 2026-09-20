package daemon

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/HappyOnigiri/WX/internal/config"
	"github.com/HappyOnigiri/WX/internal/diag"
	"github.com/HappyOnigiri/WX/internal/discovery"
	"github.com/HappyOnigiri/WX/internal/domain"
	"github.com/HappyOnigiri/WX/internal/gitx"
	"github.com/HappyOnigiri/WX/internal/pool"
	"github.com/HappyOnigiri/WX/internal/state"
	"github.com/HappyOnigiri/WX/internal/update"
	"github.com/HappyOnigiri/WX/internal/workspace"
)

func TestMutationOutcomeLessOrdersEveryFieldStrictly(t *testing.T) {
	t.Parallel()
	base := workspace.SubmoduleOutcome{Repository: "repo", Path: "path", Depth: 1, Action: "action", Reason: "reason"}
	cases := []struct {
		name string
		less func(*workspace.SubmoduleOutcome, *workspace.SubmoduleOutcome)
	}{
		{name: "repository", less: func(a, b *workspace.SubmoduleOutcome) { a.Repository, b.Repository = "a", "b" }},
		{name: "depth", less: func(a, b *workspace.SubmoduleOutcome) { a.Depth, b.Depth = 1, 2 }},
		{name: "path", less: func(a, b *workspace.SubmoduleOutcome) { a.Path, b.Path = "a", "b" }},
		{name: "action", less: func(a, b *workspace.SubmoduleOutcome) { a.Action, b.Action = "a", "b" }},
		{name: "reason", less: func(a, b *workspace.SubmoduleOutcome) { a.Reason, b.Reason = "a", "b" }},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			a, b := base, base
			test.less(&a, &b)
			if !outcomeLess(a, b) {
				t.Fatalf("outcomeLess(%+v,%+v)=false", a, b)
			}
			if outcomeLess(b, a) {
				t.Fatalf("outcomeLess(%+v,%+v)=true", b, a)
			}
			if outcomeLess(a, a) {
				t.Fatalf("outcomeLess(%+v,%+v)=true for equal outcomes", a, a)
			}
		})
	}
}

func TestMutationRecordPrepareSubmodulesEmptyCollectorReturnsNil(t *testing.T) {
	t.Parallel()
	manager := &Manager{}
	if report := manager.recordPrepareSubmodules(&workspace.SubmoduleOutcomes{}); report != nil {
		t.Fatalf("empty submodule report=%+v, want nil", report)
	}
}

func TestMutationUsageArithmeticAndSharedCacheBoundaries(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		total, removed, want int64
	}{
		{total: 100, removed: 40, want: 60},
		{total: 40, removed: 100, want: 0},
		{total: 0, removed: 0, want: 0},
	} {
		if got := subtractUsage(test.total, test.removed); got != test.want {
			t.Errorf("subtractUsage(%d,%d)=%d, want %d", test.total, test.removed, got, test.want)
		}
	}
	previous := workspace.SharedFileCache{"other/slot/file": {Shared: true}}
	measured := workspace.SharedFileCache{"this/slot/file": {Shared: false}}
	merged := mergeSlotSharedFileCache(previous, measured, "this/slot")
	if _, ok := merged["other/slot/file"]; !ok {
		t.Fatalf("cache entry for another slot was lost: %+v", merged)
	}
	if got, ok := merged["this/slot/file"]; !ok || got.Shared {
		t.Fatalf("measured cache entry=%+v present=%v, want replacement", got, ok)
	}
}

func TestMutationPrepareTimerMarkEarlyIsIdempotentAndNilSafe(t *testing.T) {
	t.Parallel()
	var nilTimer *prepareTimer
	nilTimer.markEarly()
	timer := &prepareTimer{}
	timer.markEarly()
	if timer.early.IsZero() {
		t.Fatal("first markEarly call did not record a timestamp")
	}
	first := timer.early
	timer.markEarly()
	if timer.early != first {
		t.Fatalf("second markEarly call changed timestamp from %s to %s", first, timer.early)
	}
}

func TestMutationCapacityReportFindingsCoversWarmAndLegacyBoundaries(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		volume   CapacityVolume
		warm     int
		severity diag.Severity
		detail   string
	}{
		{
			name:     "exact one slot",
			volume:   CapacityVolume{Volume: "v", Target: "/worktrees", Required: 100, Free: 100, WorktreeRequired: 100},
			warm:     1,
			severity: diag.SeverityInfo,
			detail:   "warm_count 1: 100 B",
		},
		{
			name:     "one slot shortage",
			volume:   CapacityVolume{Volume: "v", Target: "/worktrees", Required: 101, Free: 100, WorktreeRequired: 101},
			warm:     1,
			severity: diag.SeverityProblem,
			detail:   "warm_count 1: 101 B",
		},
		{
			name:     "no warm slots",
			volume:   CapacityVolume{Volume: "v", Target: "/worktrees", Required: 100, Free: 100, WorktreeRequired: 100, SharedRequired: 50},
			warm:     0,
			severity: diag.SeverityInfo,
			detail:   "warm_count 0: 0 B",
		},
		{
			name:     "legacy volume fallback",
			volume:   CapacityVolume{Volume: "v", Target: "/worktrees", Required: 100, Free: 200},
			warm:     2,
			severity: diag.SeverityInfo,
			detail:   "warm_count 2: 200 B",
		},
		{
			name:     "shared-only volume",
			volume:   CapacityVolume{Volume: "v", Target: "/cache", Required: 50, Free: 50, SharedRequired: 50},
			warm:     2,
			severity: diag.SeverityInfo,
			detail:   "warm_count 2: 50 B",
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			findings := capacityReportFindings("/workspace", CapacityReport{Volumes: []CapacityVolume{test.volume}}, test.warm)
			if len(findings) != 1 {
				t.Fatalf("findings=%+v, want one finding", findings)
			}
			if findings[0].Severity != test.severity {
				t.Fatalf("severity=%s, want %s: %+v", findings[0].Severity, test.severity, findings[0])
			}
			if len(findings[0].Details) < 2 || findings[0].Details[1] != test.detail {
				t.Fatalf("warm detail=%v, want %q", findings[0].Details, test.detail)
			}
		})
	}
}

func TestMutationLFSObjectsByRepositoryFiltersIncompleteEstimates(t *testing.T) {
	t.Parallel()
	report := CapacityReport{Repositories: []workspace.CapacityEstimate{
		{RepositoryID: "", LFS: []workspace.LFSObjectInfo{{OID: "empty-id"}}},
		{RepositoryID: "empty-lfs"},
		{RepositoryID: "repo", LFS: []workspace.LFSObjectInfo{{OID: "sha256:one"}}},
	}}
	got := lfsObjectsByRepository(report)
	if len(got) != 1 || len(got["repo"]) != 1 || got["repo"][0].OID != "sha256:one" {
		t.Fatalf("lfs objects by repository=%+v, want only repo object", got)
	}
}

func TestMutationEstimateCapacityUsesCachedEstimateWithSparseDisabled(t *testing.T) {
	t.Parallel()
	ctx, manager, _, w, resolved, _ := managerCoverageFixture(t, "repository")
	repo := resolved[0].Repository
	preparer := &workspace.Preparer{Git: manager.git, WorkspaceRoot: string(w.Root)}
	cfg := manager.Config()
	key := strings.Join([]string{
		string(repo.ID),
		resolved[0].OID,
		cfg.CopyModeForWorkspaceRepository(preparer.WorkspaceRoot, repo.RelativePath, string(repo.MainPath)),
		strconv.Itoa(cfg.COWMinSizeKiBForWorkspaceRepository(preparer.WorkspaceRoot, repo.RelativePath, string(repo.MainPath))),
	}, "\x00")
	want := workspace.CapacityEstimate{RepositoryID: string(repo.ID), WorktreeBytes: 321}
	manager.capacityCache = map[string]workspace.CapacityEstimate{key: want}
	got, err := manager.estimateCapacity(ctx, preparer, cfg, repo, resolved[0].OID)
	if err != nil {
		t.Fatal(err)
	}
	if got.RepositoryID != want.RepositoryID || got.WorktreeBytes != want.WorktreeBytes {
		t.Fatalf("cached estimate=%+v, want %+v", got, want)
	}
}

func TestMutationLFSPreflightRejectsUnrepairableObject(t *testing.T) {
	t.Parallel()
	ctx, manager, store, w, resolved, _ := managerCoverageFixture(t, "repository")
	repoPath := string(resolved[0].Repository.MainPath)
	if err := os.WriteFile(filepath.Join(repoPath, ".gitattributes"), []byte("asset.bin filter=lfs diff=lfs merge=lfs -text\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	pointer := "version https://git-lfs.github.com/spec/v1\noid sha256:" + strings.Repeat("0", 64) + "\nsize 100\n"
	if err := os.WriteFile(filepath.Join(repoPath, "asset.bin"), []byte(pointer), 0o600); err != nil {
		t.Fatal(err)
	}
	gitRun(t, repoPath, "add", ".gitattributes", "asset.bin")
	gitRun(t, repoPath, "commit", "-m", "add missing lfs object")
	resolved, err := pool.ResolveBranches(ctx, manager.git, w, nil)
	if err != nil {
		t.Fatal(err)
	}
	slot := testSlot(t, manager, string(w.ID), "lfs-preflight", 1, "PREPARING")
	stored := []state.SlotRepository{{
		RepositoryID: string(resolved[0].Repository.ID),
		DirName:      testDirName(resolved[0].Repository, manager.Config()),
		State:        "PREPARING",
		RequestedRef: resolved[0].RequestedRef,
		BaseOID:      resolved[0].OID,
	}}
	if _, err := store.CreateStandby(ctx, slot, stored); err != nil {
		t.Fatal(err)
	}
	manager.freeSpace = func(*os.File) (string, int64, error) { return "capacity-test", 1 << 40, nil }
	report, err := manager.enforcePrepareCapacity(ctx, slot, w, resolved, stored, manager.Config())
	var missing *MissingLFSObjectsError
	if !errors.As(err, &missing) {
		t.Fatalf("preflight error=%v, want missing LFS error", err)
	}
	if len(report.Repositories) != 1 || report.Repositories[0].MissingLFSObjects != 1 {
		t.Fatalf("capacity report=%+v, want one missing LFS object", report)
	}
	got, err := store.Slot(ctx, slot.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != "FAILED" || got.FailureCode != "PREPARE_LFS_MISSING" {
		t.Fatalf("slot after missing LFS preflight=%+v", got)
	}
}

func TestMutationCapturePlacementsPreservesUnstagedRepositoriesAndRecordsRoot(t *testing.T) {
	t.Parallel()
	ctx, manager, store, w, resolved, _ := managerCoverageFixture(t, "multi_repository")
	slot := testSlot(t, manager, string(w.ID), "capture-placements", 1, "PREPARING")
	repo := resolved[0].Repository
	stored := state.SlotRepository{RepositoryID: string(repo.ID), DirName: testDirName(repo, manager.Config()), State: "READY", RequestedRef: resolved[0].RequestedRef, BaseOID: resolved[0].OID}
	if _, err := store.CreateStandby(ctx, slot, []state.SlotRepository{stored}); err != nil {
		t.Fatal(err)
	}
	previous := state.Placement{RepositoryID: string(repo.ID), RelativePath: "preserved", Kind: "copy", SourcePath: "/source/preserved", ContentSHA256: "old"}
	if err := store.ReplacePlacements(ctx, slot.ID, []state.Placement{previous}); err != nil {
		t.Fatal(err)
	}
	root, _, err := manager.activeRoot()
	if err != nil {
		t.Fatal(err)
	}
	_, release, err := manager.rootDescriptor(root)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	rootFile := filepath.Join(slot.Path, "root-copy")
	if err := os.WriteFile(rootFile, []byte("root"), 0o600); err != nil {
		t.Fatal(err)
	}
	preparer := manager.newPreparer(manager.Config(), slot)
	got, err := manager.capturePlacements(ctx, slot, w, resolved, preparer, map[string][]state.Placement{
		"": {{RelativePath: "root-copy", Kind: "copy"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("captured placements=%+v, want previous repository and root placement", got)
	}
	if got[0].RepositoryID != string(repo.ID) || got[0].RelativePath != "preserved" {
		t.Fatalf("previous repository placement was not preserved: %+v", got)
	}
	if got[1].RepositoryID != "" || got[1].RelativePath != "root-copy" || got[1].Kind != "copy" {
		t.Fatalf("workspace root placement=%+v", got[1])
	}
}

func TestMutationMaterializeStagedRootRequiresStableIdentity(t *testing.T) {
	t.Parallel()
	ctx, manager, store, w, _, _ := managerCoverageFixture(t, "multi_repository")
	slot := testSlot(t, manager, string(w.ID), "staged-root", 1, "PREPARING")
	if _, err := store.CreateStandby(ctx, slot, nil); err != nil {
		t.Fatal(err)
	}
	root, _, err := manager.activeRoot()
	if err != nil {
		t.Fatal(err)
	}
	_, release, err := manager.existingRootDescriptor(root)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	source := t.TempDir()
	if err := os.WriteFile(filepath.Join(source, "early.txt"), []byte("early"), 0o600); err != nil {
		t.Fatal(err)
	}
	err = manager.materializeStagedRoot(ctx, slot, func(destination *os.Root, early bool) error {
		if !early {
			t.Fatal("materialize callback was not marked early")
		}
		return workspace.MaterializeRootAt(nil, source, destination, workspace.RootRules{Copy: []string{"early.txt"}})
	}, true)
	if err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(filepath.Join(slot.Path, "early.txt")); err != nil || string(data) != "early" {
		t.Fatalf("staged root file=%q err=%v", data, err)
	}
}

func TestMutationResolveBranchesLogsFetchFallback(t *testing.T) {
	t.Parallel()
	repoPath := filepath.Join(t.TempDir(), "repo")
	initGitRepo(t, repoPath)
	var logs bytes.Buffer
	cfg := config.Defaults()
	cfg.Worktree.FetchDefaultBranch = true
	manager := &Manager{cfg: cfg, git: &gitx.Runner{}, log: slog.New(slog.NewTextHandler(&logs, nil))}
	w := discovery.Workspace{Root: domain.CanonicalPath(repoPath), Repositories: []discovery.Repository{{
		ID: "repo", MainPath: domain.CanonicalPath(repoPath), CommonDir: domain.CanonicalPath(filepath.Join(repoPath, ".git")), RelativePath: ".", DefaultBranch: "main",
	}}}
	resolved, err := manager.resolveBranches(context.Background(), w, nil)
	if err != nil || len(resolved) != 1 || resolved[0].OID == "" {
		t.Fatalf("resolved=%+v err=%v, want local fallback", resolved, err)
	}
	if !strings.Contains(logs.String(), "default branch fetch skipped remote update; using local base") {
		t.Fatalf("fetch fallback warning was not logged: %s", logs.String())
	}
}

func TestMutationUpdateCheckSuccessDoesNotLogFailure(t *testing.T) {
	t.Parallel()
	manager, _ := newUpdateManager(t, releaseProbe("v1.0.0", func(context.Context) (update.Release, error) {
		return update.Release{Tag: "v1.1.0", URL: "https://example.test/v1.1.0"}, nil
	}))
	var logs bytes.Buffer
	manager.log = slog.New(slog.NewTextHandler(&logs, nil))
	manager.maybeCheckUpdate(context.Background())
	if strings.Contains(logs.String(), "update check result could not be recorded") {
		t.Fatalf("successful update check was logged as a recording failure: %s", logs.String())
	}
}

func TestMutationCleanReplenishSuccessDoesNotLogFailure(t *testing.T) {
	manager, store, workspaceID := replenishingCleanFixture(t)
	ctx := context.Background()
	var logs bytes.Buffer
	manager.log = slog.New(slog.NewTextHandler(&logs, nil))
	slot := testSlot(t, manager, workspaceID, "mutation-clean-standby", 1, "READY")
	if _, err := store.CreateStandby(ctx, slot, nil); err != nil {
		t.Fatal(err)
	}
	reply, err := manager.Clean(ctx, CleanRequest{Standby: true, Replenish: true})
	if err != nil {
		t.Fatal(err)
	}
	runID, _ := reply["run_id"].(string)
	if runID == "" {
		t.Fatalf("clean reply=%v", reply)
	}
	removeStandbyAndFinish(t, store, runID, slot.ID)
	waitCleanReplenishDone(t, store, runID)
	if strings.Contains(logs.String(), "finish clean replenishment failed") {
		t.Fatalf("successful clean replenishment was logged as a failure: %s", logs.String())
	}
}

func TestMutationRootRegistrationSuccessDoesNotLogFailure(t *testing.T) {
	t.Parallel()
	ctx, manager, _, _, _, _ := managerCoverageFixture(t, "repository")
	root, _, err := manager.activeRoot()
	if err != nil {
		t.Fatal(err)
	}
	identity, err := pathIdentity(root)
	if err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	manager.log = slog.New(slog.NewTextHandler(&logs, nil))
	manager.registerRootGeneration(ctx, root, identity)
	if strings.Contains(logs.String(), "register worktree root generation failed") {
		t.Fatalf("successful root registration was logged as a failure: %s", logs.String())
	}
}

func TestMutationRetryRootIdentityBackfillsPinnedAndDescriptorIdentity(t *testing.T) {
	t.Parallel()
	_, manager, _, _, _, _ := managerCoverageFixture(t, "repository")
	root, _, err := manager.activeRoot()
	if err != nil {
		t.Fatal(err)
	}
	_, release, err := manager.rootDescriptor(root)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	manager.mu.Lock()
	manager.ensureRootStateLocked()
	manager.rootIdentities[root] = ""
	manager.rootRefs[root].identity = ""
	manager.mu.Unlock()
	identity, err := manager.retryRootIdentity(root)
	if err != nil || identity == "" {
		t.Fatalf("retryRootIdentity=%q err=%v", identity, err)
	}
	manager.mu.RLock()
	pinned := manager.rootIdentities[root]
	descriptor := manager.rootRefs[root].identity
	manager.mu.RUnlock()
	if pinned != identity || descriptor != identity {
		t.Fatalf("identities pinned=%q descriptor=%q want %q", pinned, descriptor, identity)
	}
}

func TestMutationCapacityErrorReportsOnlyShortVolumes(t *testing.T) {
	t.Parallel()
	err := (&InsufficientPrepareSpaceError{Report: CapacityReport{Volumes: []CapacityVolume{
		{Target: "/full", Required: 11, Free: 10},
		{Target: "/enough", Required: 10, Free: 10},
	}}}).Error()
	if !strings.Contains(err, "/full requires 11 bytes") || strings.Contains(err, "/enough requires") {
		t.Fatalf("capacity error=%q, want only the short volume", err)
	}
}
