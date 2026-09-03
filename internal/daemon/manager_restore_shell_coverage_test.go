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

// TestRemoveColdRepositoryJobRecreatesSingleRepositorySlotShell exercises the
// branch of removeColdRepositoryJob taken when the retiring repository's
// worktree path is the slot root itself (a single-repository workspace),
// which must recreate an empty, owned shell directory in its place after the
// git worktree is removed so a later allocation can reuse the slot path.
func TestRemoveColdRepositoryJobRecreatesSingleRepositorySlotShell(t *testing.T) {
	ctx, manager, store, workspaceRecord, resolved, _ := managerCoverageFixture(t)
	workspaceRecord.Kind = "repository"
	workspaceRecord.Repositories[0].RelativePath = "."
	if err := store.UpsertWorkspace(ctx, workspaceRecord); err != nil {
		t.Fatal(err)
	}
	repository := resolved[0].Repository
	head := gitOutput(t, string(repository.MainPath), "rev-parse", "HEAD")

	slotID := domain.StableID("cold-shell", "single-repo")
	slotPath := filepath.Join(manager.Config().Storage.WorktreeRoot, "cold-shell", slotID, "root")
	if _, err := manager.createSlotRoot(slotPath); err != nil {
		t.Fatal(err)
	}
	// createSlotRoot leaves the directory in place; a real worktree checkout
	// needs to replace it, matching how a live prepare would populate it.
	if err := os.RemoveAll(slotPath); err != nil {
		t.Fatal(err)
	}
	gitRun(t, string(repository.MainPath), "worktree", "add", "--detach", slotPath, head)
	ensureOwnershipMarkerForTest(t, manager.Config().Storage.WorktreeRoot, slotPath, slotID, string(repository.CommonDir))
	gitRun(t, string(repository.MainPath), "worktree", "lock", "--reason", "wx:"+slotID+":READY", slotPath)

	// Kind "repository" canonicalizes to the workspace already registered for
	// this common directory, so the workspace ID stays workspaceRecord.ID;
	// only the recorded root needs to line up with the repository's own path.
	workspaceRecord.Root = domain.CanonicalPath(string(repository.MainPath))
	if err := store.UpsertWorkspace(ctx, workspaceRecord); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateStandby(ctx,
		state.Slot{ID: slotID, WorkspaceID: string(workspaceRecord.ID), Generation: 1, Path: slotPath, State: "RETIRING"},
		[]state.SlotRepository{{RepositoryID: string(repository.ID), WorktreePath: slotPath, State: "RETIRING", BaseOID: head}}); err != nil {
		t.Fatal(err)
	}

	if err := manager.removeColdRepositoryJob(ctx, state.Job{ID: "cold-shell", SlotID: slotID, RepositoryID: string(repository.ID)}); err != nil {
		t.Fatalf("single-repository cold removal: %v", err)
	}
	info, err := os.Lstat(slotPath)
	if err != nil || !info.IsDir() {
		t.Fatalf("cold shell was not recreated as a directory: info=%v err=%v", info, err)
	}
	entries, err := os.ReadDir(slotPath)
	if err != nil || len(entries) != 0 {
		t.Fatalf("recreated cold shell was not empty: entries=%v err=%v", entries, err)
	}
	slot, err := store.Slot(ctx, slotID)
	if err != nil || slot.State != "READY" {
		t.Fatalf("single-repository cold slot=%+v err=%v", slot, err)
	}
}

// TestRemoveColdRepositoryJobQuarantinesOnUnlockedWorktree verifies that
// removeColdRepositoryJob refuses to remove (and quarantines the slot for)
// a retiring worktree that was never locked with a recognized wx lock
// reason, instead of removing a worktree it cannot prove is safe to
// reclaim.
func TestRemoveColdRepositoryJobQuarantinesOnUnlockedWorktree(t *testing.T) {
	ctx, manager, store, workspaceRecord, resolved, _ := managerCoverageFixture(t)
	workspaceRecord.Kind = "repository"
	workspaceRecord.Repositories[0].RelativePath = "."
	if err := store.UpsertWorkspace(ctx, workspaceRecord); err != nil {
		t.Fatal(err)
	}
	repository := resolved[0].Repository
	head := gitOutput(t, string(repository.MainPath), "rev-parse", "HEAD")

	slotID := domain.StableID("cold-shell", "unlocked")
	slotPath := filepath.Join(manager.Config().Storage.WorktreeRoot, "cold-shell", slotID, "root")
	if _, err := manager.createSlotRoot(slotPath); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(slotPath); err != nil {
		t.Fatal(err)
	}
	gitRun(t, string(repository.MainPath), "worktree", "add", "--detach", slotPath, head)
	ensureOwnershipMarkerForTest(t, manager.Config().Storage.WorktreeRoot, slotPath, slotID, string(repository.CommonDir))
	// Deliberately do not lock the worktree with a recognized wx reason.

	workspaceRecord.Root = domain.CanonicalPath(string(repository.MainPath))
	if err := store.UpsertWorkspace(ctx, workspaceRecord); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateStandby(ctx,
		state.Slot{ID: slotID, WorkspaceID: string(workspaceRecord.ID), Generation: 1, Path: slotPath, State: "RETIRING"},
		[]state.SlotRepository{{RepositoryID: string(repository.ID), WorktreePath: slotPath, State: "RETIRING", BaseOID: head}}); err != nil {
		t.Fatal(err)
	}

	if err := manager.removeColdRepositoryJob(ctx, state.Job{ID: "cold-unlocked", SlotID: slotID, RepositoryID: string(repository.ID)}); err == nil {
		t.Fatal("cold repository removal succeeded for an unlocked worktree")
	}
	if _, err := os.Lstat(filepath.Join(slotPath, ".git")); err != nil {
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
	if err := store.UpsertWorkspace(ctx, workspaceRecord); err != nil {
		t.Fatal(err)
	}

	parentID := domain.StableID("restore-multi", "parent")
	parentRoot := filepath.Join(manager.Config().Storage.WorktreeRoot, "restore-multi", parentID, "root")
	if _, err := manager.createSlotRoot(parentRoot); err != nil {
		t.Fatal(err)
	}
	const marker = "workspace root recovery marker"
	if err := os.WriteFile(filepath.Join(parentRoot, "workspace-state.txt"), []byte(marker), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateSlotSession(ctx,
		state.Slot{ID: parentID, WorkspaceID: string(workspaceRecord.ID), Generation: 1, Path: parentRoot, State: "ARCHIVED"},
		[]state.SlotRepository{{RepositoryID: string(repository.ID), WorktreePath: filepath.Join(parentRoot, "repository"), State: "READY", BaseOID: resolved[0].OID}},
		state.Session{ID: parentID, WorkspaceID: string(workspaceRecord.ID), SlotID: parentID, State: "ARCHIVED", AgentKind: "codex", TokenHash: state.HashToken(parentID)}, ""); err != nil {
		t.Fatal(err)
	}
	owner, releaseOwner, err := manager.rootDescriptor(manager.Config().Storage.WorktreeRoot)
	if err != nil {
		t.Fatal(err)
	}
	rootSnapshot, err := archive.SnapshotWorkspaceAt(ctx, parentRoot, manager.Config().Storage.WorktreeRoot, owner, parentID, nil, time.Now().Add(time.Hour))
	releaseOwner()
	if err != nil {
		t.Fatalf("snapshot parent workspace root: %v", err)
	}
	if err := store.SaveWorkspaceSnapshot(ctx, rootSnapshot); err != nil {
		t.Fatal(err)
	}

	childID := domain.StableID("restore-multi", "child")
	childRoot := filepath.Join(manager.Config().Storage.WorktreeRoot, "restore-multi", childID, "root")
	childWorktree := filepath.Join(childRoot, "repository")
	fingerprint, err := workspace.Fingerprint(1, resolved[0].OID, repository, manager.Config())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateSlotSession(ctx,
		state.Slot{ID: childID, WorkspaceID: string(workspaceRecord.ID), Generation: 1, Path: childRoot, State: "RESTORING"},
		[]state.SlotRepository{{RepositoryID: string(repository.ID), WorktreePath: childWorktree, State: "RESTORING", RequestedRef: "main", BaseOID: resolved[0].OID, Fingerprint: fingerprint}},
		state.Session{ID: childID, WorkspaceID: string(workspaceRecord.ID), SlotID: childID, ParentSessionID: parentID, State: "RESTORING", AgentKind: "codex", TokenHash: state.HashToken(childID)}, ""); err != nil {
		t.Fatal(err)
	}
	releaseRoot, err := manager.holdRootForPath(childRoot)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.newPreparer(manager.Config(), childRoot).PrepareForRestore(ctx, repository, childWorktree, resolved[0].OID, childID); err != nil {
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
	childRoot := filepath.Join(manager.Config().Storage.WorktreeRoot, "restore-multi", childID, "root")
	childWorktree := filepath.Join(childRoot, "repository")
	fingerprint, err := workspace.Fingerprint(1, resolved[0].OID, repository, manager.Config())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateStandby(ctx,
		state.Slot{ID: childID, WorkspaceID: string(workspaceRecord.ID), Generation: 1, Path: childRoot, State: "RESTORING"},
		[]state.SlotRepository{{RepositoryID: string(repository.ID), WorktreePath: childWorktree, State: "READY", RequestedRef: "main", BaseOID: resolved[0].OID, Fingerprint: fingerprint}}); err != nil {
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
	slotRoot := filepath.Join(manager.Config().Storage.WorktreeRoot, "prepare-multi", slotID, "root")
	slotWorktree := filepath.Join(slotRoot, "repository")
	fingerprint, err := workspace.Fingerprint(1, resolved[0].OID, repository, manager.Config())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateStandby(ctx,
		state.Slot{ID: slotID, WorkspaceID: string(workspaceRecord.ID), Generation: 1, Path: slotRoot, State: "PREPARING"},
		[]state.SlotRepository{{RepositoryID: string(repository.ID), WorktreePath: slotWorktree, State: "READY", RequestedRef: "main", BaseOID: resolved[0].OID, Fingerprint: fingerprint}}); err != nil {
		t.Fatal(err)
	}

	if err := manager.prepareSlot(ctx, slotID, workspaceRecord, resolved, []state.SlotRepository{{RepositoryID: string(repository.ID)}}); !errors.Is(err, state.ErrOwnership) {
		t.Fatalf("unowned multi-repository prepare error=%v", err)
	}
	if slot, err := store.Slot(ctx, slotID); err != nil || slot.State != "QUARANTINED" {
		t.Fatalf("unowned multi-repository prepare slot=%+v err=%v", slot, err)
	}
}
