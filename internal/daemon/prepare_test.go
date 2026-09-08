package daemon

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/HappyOnigiri/WX/internal/config"
	"github.com/HappyOnigiri/WX/internal/domain"
	"github.com/HappyOnigiri/WX/internal/pool"
	"github.com/HappyOnigiri/WX/internal/state"
	"github.com/HappyOnigiri/WX/internal/workspace"
)

func TestNewPreparerKeepsRetiredRootForInFlightSlot(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	oldRoot := filepath.Join(base, "old-root")
	newRoot := filepath.Join(base, "new-root")
	if err := os.Mkdir(oldRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(newRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := config.Defaults()
	cfg.Storage.WorktreeRoot = newRoot
	m := &Manager{
		cfg:   cfg,
		roots: map[string]bool{oldRoot: false, newRoot: true},
	}
	t.Cleanup(m.Close)
	if _, release, err := m.existingRootDescriptor(oldRoot); err != nil {
		t.Fatal(err)
	} else {
		t.Cleanup(release)
	}

	preparer := m.newPreparer(cfg, state.Slot{RootID: "root-1", RelPath: filepath.Join("wsp001", "slt001"), Path: filepath.Join(oldRoot, "wsp001", "slt001")})
	if preparer.RootPath != oldRoot {
		t.Fatalf("in-flight slot preparer root=%q want retired root %q", preparer.RootPath, oldRoot)
	}
	if preparer.OwnedRoot == nil {
		t.Fatal("in-flight slot preparer did not retain retired root descriptor")
	}
	if _, err := preparer.OwnedRoot.Lstat("."); err != nil {
		t.Fatalf("in-flight slot preparer returned unusable root descriptor: %v", err)
	}
}

func TestPrepareSlotFailureAndReplayBoundaries(t *testing.T) {
	t.Parallel()
	ctx, manager, store, workspaceRecord, resolved, _ := managerCoverageFixture(t, "repository")
	resolved[0].Repository.RelativePath = "."
	if err := manager.prepareSlot(ctx, "missing", workspaceRecord, resolved, nil); err == nil {
		t.Fatal("missing slot preparation succeeded")
	}
	archived := domain.StableID("prepare-coverage", "archived")
	if _, err := store.CreateStandby(ctx, testSlot(t, manager, string(workspaceRecord.ID), archived, 1, "ARCHIVED"), nil); err != nil {
		t.Fatal(err)
	}
	if err := manager.prepareSlot(ctx, archived, workspaceRecord, nil, nil); err == nil {
		t.Fatal("archived slot preparation succeeded")
	}

	mismatch := domain.StableID("prepare-coverage", "mismatch")
	if _, err := store.CreateStandby(ctx, testSlot(t, manager, string(workspaceRecord.ID), mismatch, 1, "PREPARING"), nil); err != nil {
		t.Fatal(err)
	}
	if err := manager.prepareSlot(ctx, mismatch, workspaceRecord, resolved, nil); err == nil {
		t.Fatal("mismatched repository metadata succeeded")
	}

	dirName := testDirName(resolved[0].Repository, manager.Config())
	fingerprint, err := workspace.Fingerprint(1, resolved[0].OID, resolved[0].Repository, manager.Config())
	if err != nil {
		t.Fatal(err)
	}
	readyID := domain.StableID("prepare-coverage", "ready-replay")
	readySlot := testSlot(t, manager, string(workspaceRecord.ID), readyID, 1, "PREPARING")
	if _, err := store.CreateStandby(ctx, readySlot,
		[]state.SlotRepository{{RepositoryID: string(resolved[0].Repository.ID), DirName: dirName, State: "READY", RequestedRef: "main", BaseOID: resolved[0].OID, Fingerprint: fingerprint}}); err != nil {
		t.Fatal(err)
	}
	if err := manager.prepareSlot(ctx, readyID, workspaceRecord, resolved, []state.SlotRepository{{RepositoryID: string(resolved[0].Repository.ID)}}); !errors.Is(err, state.ErrOwnership) {
		t.Fatalf("unowned READY replay error=%v", err)
	}
	if slot, err := store.Slot(ctx, readyID); err != nil || slot.State != "QUARANTINED" {
		t.Fatalf("unowned READY replay slot=%+v err=%v", slot, err)
	}

	validID := domain.StableID("prepare-coverage", "ready-replay-owned")
	validSlot := testSlot(t, manager, string(workspaceRecord.ID), validID, 1, "PREPARING")
	validWorktree := filepath.Join(validSlot.Path, dirName)
	if _, err := store.CreateStandby(ctx, validSlot,
		[]state.SlotRepository{{RepositoryID: string(resolved[0].Repository.ID), DirName: dirName, State: "PREPARING", RequestedRef: "main", BaseOID: resolved[0].OID, Fingerprint: fingerprint}}); err != nil {
		t.Fatal(err)
	}
	releaseRoot, err := manager.holdRootForPath(validSlot.Path)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.newPreparer(manager.Config(), validSlot).Prepare(ctx, resolved[0].Repository, validWorktree, resolved[0].OID, validID); err != nil {
		releaseRoot()
		t.Fatalf("create worktree for valid READY replay: %v", err)
	}
	releaseRoot()
	if err := store.SetSlotRepositoryState(ctx, validID, string(resolved[0].Repository.ID), []string{"PREPARING"}, "READY"); err != nil {
		t.Fatal(err)
	}
	if err := manager.prepareSlot(ctx, validID, workspaceRecord, resolved, []state.SlotRepository{{RepositoryID: string(resolved[0].Repository.ID)}}); err != nil {
		t.Fatalf("owned READY replay: %v", err)
	}
	if slot, err := store.Slot(ctx, validID); err != nil || slot.State != "READY" {
		t.Fatalf("owned READY replay slot=%+v err=%v", slot, err)
	}

	missingRepoID := domain.StableID("prepare-coverage", "missing-repository")
	if _, err := store.CreateStandby(ctx, testSlot(t, manager, string(workspaceRecord.ID), missingRepoID, 1, "PREPARING"),
		[]state.SlotRepository{{RepositoryID: string(resolved[0].Repository.ID), DirName: dirName, State: "PREPARING", BaseOID: resolved[0].OID, Fingerprint: fingerprint}}); err != nil {
		t.Fatal(err)
	}
	unknownResolved := append([]pool.Resolved(nil), resolved...)
	unknownResolved[0].Repository.ID = "unknown"
	if err := manager.prepareSlot(ctx, missingRepoID, workspaceRecord, unknownResolved, []state.SlotRepository{{RepositoryID: "unknown"}}); err == nil {
		t.Fatal("missing stored repository preparation succeeded")
	}

	failureID := domain.StableID("prepare-coverage", "preparer-failure")
	failureSlot := testSlot(t, manager, string(workspaceRecord.ID), failureID, 1, "PREPARING")
	if err := os.WriteFile(filepath.Join(failureSlot.Path, dirName), []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateStandby(ctx, failureSlot,
		[]state.SlotRepository{{RepositoryID: string(resolved[0].Repository.ID), DirName: dirName, State: "PREPARING", RequestedRef: "main", BaseOID: resolved[0].OID, Fingerprint: fingerprint}}); err != nil {
		t.Fatal(err)
	}
	if err := manager.prepareSlot(ctx, failureID, workspaceRecord, resolved, []state.SlotRepository{{RepositoryID: string(resolved[0].Repository.ID)}}); err == nil {
		t.Fatal("unsafe worktree preparation succeeded")
	}
	slot, err := store.Slot(ctx, failureID)
	if err != nil || slot.State != "QUARANTINED" {
		t.Fatalf("failed preparation slot=%+v err=%v", slot, err)
	}

	ambiguousID := domain.StableID("prepare-coverage", "ambiguous-command")
	if _, err := store.CreateStandby(ctx, testSlot(t, manager, string(workspaceRecord.ID), ambiguousID, 1, "PREPARING"),
		[]state.SlotRepository{{RepositoryID: string(resolved[0].Repository.ID), DirName: dirName, State: "PREPARE_RUNNING", RequestedRef: "main", BaseOID: resolved[0].OID, Fingerprint: fingerprint}}); err != nil {
		t.Fatal(err)
	}
	manager.mu.Lock()
	cfg := manager.cfg
	cfg.Repositories[string(resolved[0].Repository.MainPath)] = config.Repository{Prepare: config.Prepare{Command: []string{"true"}}}
	manager.cfg = cfg
	manager.mu.Unlock()
	if err := manager.prepareSlot(ctx, ambiguousID, workspaceRecord, resolved, []state.SlotRepository{{RepositoryID: string(resolved[0].Repository.ID)}}); err == nil {
		t.Fatal("ambiguous prepare command replay succeeded")
	}
	slot, err = store.Slot(ctx, ambiguousID)
	if err != nil || slot.State != "QUARANTINED" {
		t.Fatalf("ambiguous preparation slot=%+v err=%v", slot, err)
	}
}

func TestMaterializeWorkspaceRootFailsWhenSlotDirectoryIsUnreadable(t *testing.T) {
	t.Parallel()
	ctx, manager, _, _, _, _ := managerCoverageFixture(t)
	_ = ctx
	root := manager.Config().Storage.WorktreeRoot
	slotPath := filepath.Join(root, "materialize", "unreadable", "root")
	if _, _, err := manager.createSlotRoot(slotPath, slotPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(slotPath, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(slotPath, 0o700) })

	source := t.TempDir()
	if err := manager.materializeWorkspaceRoot(source, slotPath, config.Workspace{}); err == nil {
		t.Fatal("workspace root materialization succeeded despite an unopenable slot directory")
	}
}
