package daemon

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/HappyOnigiri/WX/internal/archive"
	"github.com/HappyOnigiri/WX/internal/domain"
	"github.com/HappyOnigiri/WX/internal/state"
	"github.com/HappyOnigiri/WX/internal/workspace"
)

// TestRemoveColdRepositoryJobKeepsTheSlotDirectoryAndItsMarker covers what
// replaced removeColdRepositoryJob's shell-recreation branch. Every worktree
// now lives one level below its slot directory, for a single-repository
// workspace as much as for a bundle, so retiring a repository removes only
// the worktree: the slot directory survives, and so does the ownership
// marker that lets a later removal prove the slot is wx's.
func TestRemoveColdRepositoryJobKeepsTheSlotDirectoryAndItsMarker(t *testing.T) {
	ctx, manager, store, workspaceRecord, resolved, _ := managerCoverageFixture(t, "repository")
	repository := resolved[0].Repository
	head := gitOutput(t, string(repository.MainPath), "rev-parse", "HEAD")
	slotID := domain.StableID("cold-shell", "single-repo")
	slot := testSlot(t, manager, string(workspaceRecord.ID), slotID, 1, "RETIRING")
	dirName := testDirName(repository, manager.Config())
	worktreePath := filepath.Join(slot.Path, dirName)
	gitRun(t, string(repository.MainPath), "worktree", "add", "--detach", worktreePath, head)
	ensureOwnershipMarkerForTest(t, manager.Config().Storage.WorktreeRoot, worktreePath, workspace.MarkerIdentity{SlotID: slotID, RootID: slot.RootID, RepositoryID: string(repository.ID)}, string(repository.CommonDir))
	gitRun(t, string(repository.MainPath), "worktree", "lock", "--reason", "wx:"+slotID+":READY", worktreePath)

	if _, err := store.CreateStandby(ctx, slot,
		[]state.SlotRepository{{RepositoryID: string(repository.ID), DirName: dirName, State: "RETIRING", BaseOID: head}}); err != nil {
		t.Fatal(err)
	}

	if err := manager.removeColdRepositoryJob(ctx, state.Job{ID: "cold-shell", SlotID: slotID, RepositoryID: string(repository.ID)}); err != nil {
		t.Fatalf("single-repository cold removal: %v", err)
	}
	if _, err := os.Lstat(worktreePath); !os.IsNotExist(err) {
		t.Fatalf("retired worktree still exists: %v", err)
	}
	info, err := os.Lstat(slot.Path)
	if err != nil || !info.IsDir() {
		t.Fatalf("slot directory did not survive repository retirement: info=%v err=%v", info, err)
	}
	entries, err := os.ReadDir(slot.Path)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != workspace.OwnershipMarkerName(string(repository.ID)) {
		names := make([]string, 0, len(entries))
		for _, entry := range entries {
			names = append(names, entry.Name())
		}
		t.Fatalf("slot directory contents=%v, want only the ownership marker", names)
	}
	finished, err := store.Slot(ctx, slotID)
	if err != nil || finished.State != "READY" {
		t.Fatalf("single-repository cold slot=%+v err=%v", finished, err)
	}
}

// TestRemoveColdRepositoryJobQuarantinesOnUnlockedWorktree verifies that
// removeColdRepositoryJob refuses to remove (and quarantines the slot for)
// a retiring worktree that was never locked with a recognized wx lock
// reason, instead of removing a worktree it cannot prove is safe to
// reclaim.
func TestRemoveColdRepositoryJobQuarantinesOnUnlockedWorktree(t *testing.T) {
	ctx, manager, store, workspaceRecord, resolved, _ := managerCoverageFixture(t, "repository")
	repository := resolved[0].Repository
	head := gitOutput(t, string(repository.MainPath), "rev-parse", "HEAD")

	slotID := domain.StableID("cold-shell", "unlocked")
	slot := testSlot(t, manager, string(workspaceRecord.ID), slotID, 1, "RETIRING")
	dirName := testDirName(repository, manager.Config())
	worktreePath := filepath.Join(slot.Path, dirName)
	gitRun(t, string(repository.MainPath), "worktree", "add", "--detach", worktreePath, head)
	ensureOwnershipMarkerForTest(t, manager.Config().Storage.WorktreeRoot, worktreePath, workspace.MarkerIdentity{SlotID: slotID, RootID: slot.RootID, RepositoryID: string(repository.ID)}, string(repository.CommonDir))
	// Deliberately do not lock the worktree with a recognized wx reason.

	if _, err := store.CreateStandby(ctx, slot,
		[]state.SlotRepository{{RepositoryID: string(repository.ID), DirName: dirName, State: "RETIRING", BaseOID: head}}); err != nil {
		t.Fatal(err)
	}

	if err := manager.removeColdRepositoryJob(ctx, state.Job{ID: "cold-unlocked", SlotID: slotID, RepositoryID: string(repository.ID)}); err == nil {
		t.Fatal("cold repository removal succeeded for an unlocked worktree")
	}
	if _, err := os.Lstat(filepath.Join(worktreePath, ".git")); err != nil {
		t.Fatalf("unlocked worktree was removed despite a failed removal: %v", err)
	}
}

// TestRestoreSlotCompletesMultiRepositoryWorkspaceRootRecovery drives
// restoreSlot through its multi_repository workspace-root recovery tail:
// every per-repository worktree is already restored to READY, and the slot
// is waiting on the workspace-level snapshot to be replayed on top of the
// freshly materialized root before the slot can flip to READY. This proves
// the recovered workspace-root content actually lands on disk, not merely
// that restoreSlot returns nil.
func TestRestoreSlotCompletesMultiRepositoryWorkspaceRootRecovery(t *testing.T) {
	ctx, manager, store, workspaceRecord, resolved, _ := managerCoverageFixture(t)
	repository := resolved[0].Repository
	// The recorded workspace root must actually contain the repository at
	// its registered relative path ("repository"), or per-repository
	// ownership validation below rejects the association before the
	// workspace-root recovery branch under test is ever reached.
	workspaceRecord.Root = domain.CanonicalPath(filepath.Dir(string(repository.MainPath)))
	workspaceRecord, _, err := store.UpsertWorkspaceGeneration(ctx, workspaceRecord)
	if err != nil {
		t.Fatal(err)
	}
	dirName := testDirName(repository, manager.Config())

	parentID := domain.StableID("restore-multi", "parent")
	parentSlot := testSlot(t, manager, string(workspaceRecord.ID), parentID, 1, "ARCHIVED")
	parentRoot := parentSlot.Path
	const marker = "workspace root recovery marker"
	if err := os.WriteFile(filepath.Join(parentRoot, "workspace-state.txt"), []byte(marker), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateSlotSession(ctx, parentSlot,
		[]state.SlotRepository{{RepositoryID: string(repository.ID), DirName: dirName, State: "READY", BaseOID: resolved[0].OID}},
		state.Session{ID: parentID, WorkspaceID: string(workspaceRecord.ID), SlotID: parentID, State: "ARCHIVED", AgentKind: "codex", TokenHash: state.HashToken(parentID)}, ""); err != nil {
		t.Fatal(err)
	}
	owner, releaseOwner, err := manager.rootDescriptor(manager.Config().Storage.WorktreeRoot)
	if err != nil {
		t.Fatal(err)
	}
	rootSnapshot, err := archive.SnapshotWorkspaceAt(ctx, parentRoot, manager.Config().Storage.WorktreeRoot, parentSlot.RootID, owner, parentID, nil, time.Now().Add(time.Hour))
	releaseOwner()
	if err != nil {
		t.Fatalf("snapshot parent workspace root: %v", err)
	}
	if err := store.SaveWorkspaceSnapshot(ctx, rootSnapshot); err != nil {
		t.Fatal(err)
	}

	childID := domain.StableID("restore-multi", "child")
	childSlot := testSlot(t, manager, string(workspaceRecord.ID), childID, 1, "RESTORING")
	childRoot := childSlot.Path
	childWorktree := filepath.Join(childRoot, dirName)
	fingerprint, err := workspace.Fingerprint(1, resolved[0].OID, repository, dirName, manager.Config())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateSlotSession(ctx, childSlot,
		[]state.SlotRepository{{RepositoryID: string(repository.ID), DirName: dirName, State: "RESTORING", RequestedRef: "main", BaseOID: resolved[0].OID, Fingerprint: fingerprint}},
		state.Session{ID: childID, WorkspaceID: string(workspaceRecord.ID), SlotID: childID, ParentSessionID: parentID, State: "RESTORING", AgentKind: "codex", TokenHash: state.HashToken(childID)}, ""); err != nil {
		t.Fatal(err)
	}
	releaseRoot, err := manager.holdRootForPath(childRoot)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.newPreparer(manager.Config(), childSlot).PrepareForRestore(ctx, repository, childWorktree, resolved[0].OID, childID); err != nil {
		releaseRoot()
		t.Fatalf("prepare restoring repository worktree: %v", err)
	}
	releaseRoot()
	if err := store.SetSlotRepositoryState(ctx, childID, string(repository.ID), []string{"RESTORING"}, "READY"); err != nil {
		t.Fatal(err)
	}

	if err := manager.restoreSlot(ctx, childID, workspaceRecord, resolved, []state.SlotRepository{{RepositoryID: string(repository.ID)}}, nil); err != nil {
		t.Fatalf("multi-repository workspace root restore: %v", err)
	}
	// The owning session is still in state RESTORING, so FinishPreparation
	// leases the slot directly to it instead of returning it to the READY
	// pool; the interesting assertion here is the recovered file content.
	slot, err := store.Slot(ctx, childID)
	if err != nil || slot.State != "LEASED" || slot.OwnerSessionID != childID {
		t.Fatalf("restored multi-repository slot=%+v err=%v", slot, err)
	}
	data, err := os.ReadFile(filepath.Join(childRoot, "workspace-state.txt"))
	if err != nil || string(data) != marker {
		t.Fatalf("workspace root recovery did not materialize archived content: data=%q err=%v", data, err)
	}
}

// TestRestoreSlotQuarantinesUncertainWorkspaceRootOwnership verifies that a
// multi_repository restore refuses to materialize the workspace root when
// the slot's own ownership cannot be validated, quarantining the slot
// instead of writing into a path it cannot prove it owns.
func TestRestoreSlotQuarantinesUncertainWorkspaceRootOwnership(t *testing.T) {
	ctx, manager, store, workspaceRecord, resolved, _ := managerCoverageFixture(t)
	repository := resolved[0].Repository

	childID := domain.StableID("restore-multi", "unowned-child")
	dirName := testDirName(repository, manager.Config())
	fingerprint, err := workspace.Fingerprint(1, resolved[0].OID, repository, dirName, manager.Config())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateStandby(ctx, testSlot(t, manager, string(workspaceRecord.ID), childID, 1, "RESTORING"),
		[]state.SlotRepository{{RepositoryID: string(repository.ID), DirName: dirName, State: "READY", RequestedRef: "main", BaseOID: resolved[0].OID, Fingerprint: fingerprint}}); err != nil {
		t.Fatal(err)
	}

	if err := manager.restoreSlot(context.Background(), childID, workspaceRecord, resolved, []state.SlotRepository{{RepositoryID: string(repository.ID)}}, nil); !errors.Is(err, state.ErrOwnership) {
		t.Fatalf("unowned multi-repository restore error=%v", err)
	}
	if slot, err := store.Slot(ctx, childID); err != nil || slot.State != "QUARANTINED" {
		t.Fatalf("unowned multi-repository restore slot=%+v err=%v", slot, err)
	}
}

// TestPrepareSlotQuarantinesUncertainRepositoryOwnershipInMultiRepository
// verifies that preparing a multi_repository slot whose per-repository
// ownership cannot be validated (a READY repository row pointing at a path
// that was never actually prepared/owned) is quarantined instead of being
// treated as an ordinary retryable preparation failure.
func TestPrepareSlotQuarantinesUncertainRepositoryOwnershipInMultiRepository(t *testing.T) {
	ctx, manager, store, workspaceRecord, resolved, _ := managerCoverageFixture(t)
	repository := resolved[0].Repository

	slotID := domain.StableID("prepare-multi", "unowned")
	dirName := testDirName(repository, manager.Config())
	fingerprint, err := workspace.Fingerprint(1, resolved[0].OID, repository, dirName, manager.Config())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateStandby(ctx, testSlot(t, manager, string(workspaceRecord.ID), slotID, 1, "PREPARING"),
		[]state.SlotRepository{{RepositoryID: string(repository.ID), DirName: dirName, State: "READY", RequestedRef: "main", BaseOID: resolved[0].OID, Fingerprint: fingerprint}}); err != nil {
		t.Fatal(err)
	}

	if err := manager.prepareSlot(ctx, slotID, workspaceRecord, resolved, []state.SlotRepository{{RepositoryID: string(repository.ID)}}); !errors.Is(err, state.ErrOwnership) {
		t.Fatalf("unowned multi-repository prepare error=%v", err)
	}
	if slot, err := store.Slot(ctx, slotID); err != nil || slot.State != "QUARANTINED" {
		t.Fatalf("unowned multi-repository prepare slot=%+v err=%v", slot, err)
	}
}
