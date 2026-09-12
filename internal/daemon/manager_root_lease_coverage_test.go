package daemon

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/HappyOnigiri/WX/internal/config"
	"github.com/HappyOnigiri/WX/internal/domain"
	"github.com/HappyOnigiri/WX/internal/state"
	"github.com/HappyOnigiri/WX/internal/workspace"
)

func TestReadyMatchesReportsUnreadableSlotDirectory(t *testing.T) {
	t.Parallel()
	ctx, manager, _, workspaceRecord, resolved, _ := managerCoverageFixture(t)
	root := manager.Config().Storage.WorktreeRoot
	if _, _, err := manager.createSlotRoot(filepath.Join(root, "bootstrap", "root"), filepath.Join(root, "bootstrap", "root")); err != nil {
		t.Fatal(err)
	}
	slotsDir := filepath.Join(root, "workspaces", string(workspaceRecord.ID), "slots")
	if err := os.MkdirAll(slotsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(slotsDir, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(slotsDir, 0o700) })

	slot := slotAtPath(t, manager, string(workspaceRecord.ID), "blocked", filepath.Join(slotsDir, "blocked", "root"), 1, "READY")
	if ok, err := manager.readyMatches(ctx, slot, resolved); ok || !errors.Is(err, state.ErrOwnership) {
		t.Fatalf("unreadable slot directory ok=%v err=%v", ok, err)
	}
}

func TestReadyMatchesReportsMissingReadySlotDirectory(t *testing.T) {
	t.Parallel()
	ctx, manager, _, workspaceRecord, resolved, _ := managerCoverageFixture(t)
	bootstrap := filepath.Join(manager.Config().Storage.WorktreeRoot, "ready-missing", "bootstrap", "root")
	if _, _, err := manager.createSlotRoot(bootstrap, bootstrap); err != nil {
		t.Fatal(err)
	}
	goneID := domain.StableID("ready-missing", "gone")
	gonePath := filepath.Join(manager.Config().Storage.WorktreeRoot, "ready-missing", goneID, "root")
	slot := slotAtPath(t, manager, string(workspaceRecord.ID), goneID, gonePath, 1, "READY")
	if ok, err := manager.readyMatches(ctx, slot, resolved); err != nil || ok {
		t.Fatalf("missing ready slot directory ok=%v err=%v", ok, err)
	}
}

func TestReadyRepositoriesMatchRejectsWorktreePathsOutsideRoot(t *testing.T) {
	t.Parallel()
	ctx, manager, store, workspaceRecord, resolved, _ := managerCoverageFixture(t)
	root := manager.Config().Storage.WorktreeRoot
	if _, _, err := manager.createSlotRoot(filepath.Join(root, "bootstrap", "root"), filepath.Join(root, "bootstrap", "root")); err != nil {
		t.Fatal(err)
	}
	owner, release, err := manager.existingRootDescriptor(root)
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	coldID := domain.StableID("ready-outside", "cold")
	coldSlot := slotAtPath(t, manager, string(workspaceRecord.ID), coldID, filepath.Join(root, "ready-outside", coldID, "root"), 1, "READY")
	coldDirName := escapingDirNameFor(t, root, coldSlot.Path)
	coldFingerprint, err := workspace.Fingerprint(1, resolved[0].OID, resolved[0].Repository, manager.Config())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateStandby(ctx, coldSlot,
		[]state.SlotRepository{{RepositoryID: string(resolved[0].Repository.ID), DirName: coldDirName, State: "COLD", BaseOID: resolved[0].OID, Fingerprint: coldFingerprint}}); err != nil {
		t.Fatal(err)
	}
	if ok, err := manager.readyRepositoriesMatch(ctx, coldSlot, resolved, root, owner); ok || !errors.Is(err, state.ErrOwnership) {
		t.Fatalf("cold worktree path outside root ok=%v err=%v", ok, err)
	}

	readyID := domain.StableID("ready-outside", "ready")
	readySlot := slotAtPath(t, manager, string(workspaceRecord.ID), readyID, filepath.Join(root, "ready-outside", readyID, "root"), 1, "READY")
	readyDirName := escapingDirNameFor(t, root, readySlot.Path)
	readyFingerprint, err := workspace.Fingerprint(1, resolved[0].OID, resolved[0].Repository, manager.Config())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateStandby(ctx, readySlot,
		[]state.SlotRepository{{RepositoryID: string(resolved[0].Repository.ID), DirName: readyDirName, State: "READY", BaseOID: resolved[0].OID, Fingerprint: readyFingerprint}}); err != nil {
		t.Fatal(err)
	}
	if ok, err := manager.readyRepositoriesMatch(ctx, readySlot, resolved, root, owner); ok || !errors.Is(err, state.ErrOwnership) {
		t.Fatalf("ready worktree path outside root ok=%v err=%v", ok, err)
	}
}

func TestReadyRepositoriesMatchReportsUnopenableReadyWorktree(t *testing.T) {
	t.Parallel()
	ctx, manager, store, workspaceRecord, resolved, _ := managerCoverageFixture(t)
	root := manager.Config().Storage.WorktreeRoot
	if _, _, err := manager.createSlotRoot(filepath.Join(root, "bootstrap", "root"), filepath.Join(root, "bootstrap", "root")); err != nil {
		t.Fatal(err)
	}
	owner, release, err := manager.existingRootDescriptor(root)
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	fingerprint, err := workspace.Fingerprint(1, resolved[0].OID, resolved[0].Repository, manager.Config())
	if err != nil {
		t.Fatal(err)
	}

	blockedID := domain.StableID("ready-unopenable", "worktree")
	slotRoot := filepath.Join(root, "ready-unopenable", blockedID, "root")
	worktreePath := filepath.Join(slotRoot, "repository")
	if err := os.MkdirAll(worktreePath, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(worktreePath, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(worktreePath, 0o700) })

	slot := slotAtPath(t, manager, string(workspaceRecord.ID), blockedID, slotRoot, 1, "READY")
	if _, err := store.CreateStandby(ctx, slot,
		[]state.SlotRepository{{RepositoryID: string(resolved[0].Repository.ID), DirName: filepath.Base(worktreePath), State: "READY", BaseOID: resolved[0].OID, Fingerprint: fingerprint}}); err != nil {
		t.Fatal(err)
	}

	if ok, err := manager.readyRepositoriesMatch(ctx, slot, resolved, root, owner); ok || !errors.Is(err, state.ErrOwnership) {
		t.Fatalf("unopenable ready worktree ok=%v err=%v", ok, err)
	}
}

func TestQuarantineFailureHelpersIgnoreNonOwnershipErrors(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	cfg := config.Defaults()
	store, err := openTestStoreAtPath(t, filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	m := testManager(t, cfg, store)
	defer m.Close()
	ctx := context.Background()

	session := state.Session{ID: "slot", SlotID: "slot", State: "ACTIVE", AgentKind: "codex", TokenHash: state.HashToken("token")}
	if _, err := store.CreateSlotSession(ctx, testSlotRow(t, m, "", "slot", 1, "RETIRING"), nil, session, ""); err != nil {
		t.Fatal(err)
	}

	m.quarantineOwnershipFailure("slot", []string{"RETIRING"}, errors.New("transient failure"))
	m.quarantineCleanupFailure("slot", errors.New("transient failure"))

	if slot, err := store.Slot(ctx, "slot"); err != nil || slot.State != "RETIRING" {
		t.Fatalf("non-ownership failure changed slot state: slot=%+v err=%v", slot, err)
	}
}
