package daemon

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/HappyOnigiri/WX/internal/config"
	"github.com/HappyOnigiri/WX/internal/domain"
	"github.com/HappyOnigiri/WX/internal/state"
)

// TestColdWorktreeUnmaterializedAcceptsAbsentAndEmptyDirectories covers the
// single rule that replaced readyRepositoriesMatch's per-workspace-kind COLD
// branches. A cold lease creates the repository directory so the client can
// open it as its CWD before preparation runs, so "empty" is a real state and
// only content means the repository was actually checked out.
func TestColdWorktreeUnmaterializedAcceptsAbsentAndEmptyDirectories(t *testing.T) {
	root := t.TempDir()
	owner, _, err := domain.OpenOwnedRoot(root, root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = owner.Close() })

	absent := filepath.Join(root, "slot", "absent")
	if err := os.MkdirAll(filepath.Dir(absent), 0o700); err != nil {
		t.Fatal(err)
	}
	if ok, err := coldWorktreeUnmaterialized(owner, root, absent); err != nil || !ok {
		t.Fatalf("absent cold worktree ok=%v err=%v", ok, err)
	}

	empty := filepath.Join(root, "slot", "empty")
	if err := os.MkdirAll(empty, 0o700); err != nil {
		t.Fatal(err)
	}
	if ok, err := coldWorktreeUnmaterialized(owner, root, empty); err != nil || !ok {
		t.Fatalf("empty cold worktree ok=%v err=%v", ok, err)
	}

	populated := filepath.Join(root, "slot", "populated")
	if err := os.MkdirAll(populated, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(populated, "tracked"), []byte("checked out\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if ok, err := coldWorktreeUnmaterialized(owner, root, populated); err != nil || ok {
		t.Fatalf("populated cold worktree ok=%v err=%v", ok, err)
	}

	link := filepath.Join(root, "slot", "link")
	if err := os.Symlink(t.TempDir(), link); err != nil {
		t.Fatal(err)
	}
	if ok, err := coldWorktreeUnmaterialized(owner, root, link); err != nil || ok {
		t.Fatalf("symlinked cold worktree ok=%v err=%v", ok, err)
	}

	outside := filepath.Join(t.TempDir(), "outside")
	if ok, err := coldWorktreeUnmaterialized(owner, root, outside); ok || !errors.Is(err, state.ErrOwnership) {
		t.Fatalf("cold worktree outside root ok=%v err=%v", ok, err)
	}
}

// TestOwnedSlotDirectoriesListsOnlyPhysicalSlotDirectories covers the
// enumeration side of the layout: one level below a workspace namespace, and
// nothing that is not a physical directory.
func TestOwnedSlotDirectoriesListsOnlyPhysicalSlotDirectories(t *testing.T) {
	root := t.TempDir()
	owner, _, err := domain.OpenOwnedRoot(root, root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = owner.Close() })

	if paths, err := ownedSlotDirectories(owner, root, "absent-namespace"); err != nil || len(paths) != 0 {
		t.Fatalf("absent namespace paths=%v err=%v", paths, err)
	}

	namespace := "wsp001"
	slot := filepath.Join(root, namespace, testSlotID("Slot-One"))
	if err := os.MkdirAll(slot, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, namespace, "regular"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(t.TempDir(), filepath.Join(root, namespace, "link")); err != nil {
		t.Fatal(err)
	}
	paths, err := ownedSlotDirectories(owner, root, namespace)
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 1 || filepath.Clean(paths[0]) != filepath.Clean(slot) {
		t.Fatalf("slot directories=%v, want only %s", paths, slot)
	}
}

// TestActiveRootAndRootIDForPathFailClosedWithoutARegisteredGeneration
// verifies that neither accessor invents a root generation: a slot row
// inserted without one could not be located again, so both must refuse.
func TestActiveRootAndRootIDForPathFailClosedWithoutARegisteredGeneration(t *testing.T) {
	root := t.TempDir()
	store, err := state.Open(filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	cfg := config.Defaults()
	cfg.Storage.WorktreeRoot = filepath.Join(root, "worktrees")
	manager := testManager(t, cfg, store)
	t.Cleanup(manager.Close)

	// testManager registers the configured root the way New() does.
	rootPath, rootID, err := manager.activeRoot()
	if err != nil || rootID == "" || rootPath != filepath.Clean(cfg.Storage.WorktreeRoot) {
		t.Fatalf("active root path=%q id=%q err=%v", rootPath, rootID, err)
	}
	if _, resolvedID, err := manager.rootIDForPath(filepath.Join(rootPath, "wsp001", "slt001")); err != nil || resolvedID != rootID {
		t.Fatalf("root id for slot path=%q err=%v, want %q", resolvedID, err, rootID)
	}
	if _, _, err := manager.rootIDForPath(filepath.Join(t.TempDir(), "elsewhere")); !errors.Is(err, state.ErrOwnership) {
		t.Fatalf("root id outside every root err=%v", err)
	}

	// Forgetting the mapping is what "no registered generation" looks like
	// after a failed EnsureActiveRoot at startup.
	manager.mu.Lock()
	delete(manager.rootIDs, rootPath)
	manager.mu.Unlock()
	if _, _, err := manager.activeRoot(); !errors.Is(err, state.ErrOwnership) {
		t.Fatalf("active root without a generation err=%v", err)
	}
	if _, _, err := manager.rootIDForPath(filepath.Join(rootPath, "wsp001", "slt001")); !errors.Is(err, state.ErrOwnership) {
		t.Fatalf("root id without a generation err=%v", err)
	}
}

// TestRegisterAndLoadRootGenerationsRepinRetiredRoots proves the durable half
// of root co-existence: a previously configured root keeps its row and its ID
// after a reload, and loadRootGenerations repins it so its slots stay
// addressable across a restart.
func TestRegisterAndLoadRootGenerationsRepinRetiredRoots(t *testing.T) {
	home := t.TempDir()
	store, err := state.Open(filepath.Join(home, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	cfg := config.Defaults()
	cfg.Storage.WorktreeRoot = filepath.Join(home, "first")
	manager := testManager(t, cfg, store)
	t.Cleanup(manager.Close)
	ctx := context.Background()

	firstPath, firstID, err := manager.activeRoot()
	if err != nil {
		t.Fatal(err)
	}
	secondPath := filepath.Join(home, "second")
	if err := os.MkdirAll(secondPath, 0o700); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(secondPath)
	if err != nil {
		t.Fatal(err)
	}
	identity, err := domain.FileIdentity(info)
	if err != nil {
		t.Fatal(err)
	}
	manager.mu.Lock()
	manager.cfg.Storage.WorktreeRoot = secondPath
	manager.mu.Unlock()
	manager.registerRootGeneration(ctx, secondPath, identity)

	_, secondID, err := manager.activeRoot()
	if err != nil {
		t.Fatal(err)
	}
	if secondID == "" || secondID == firstID {
		t.Fatalf("second generation id=%q, want a new generation distinct from %q", secondID, firstID)
	}
	roots, err := store.Roots(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(roots) != 2 {
		t.Fatalf("registered roots=%+v, want both generations retained", roots)
	}
	for _, root := range roots {
		switch root.Path {
		case firstPath:
			if root.Active {
				t.Fatalf("previous root %s is still active", root.Path)
			}
		case secondPath:
			if !root.Active {
				t.Fatalf("configured root %s is not active", root.Path)
			}
		default:
			t.Fatalf("unexpected registered root %+v", root)
		}
	}

	// A restart starts with an empty registry; loadRootGenerations must bring
	// the retired generation back so its slots remain resolvable.
	manager.mu.Lock()
	manager.rootIDs = map[string]string{}
	manager.mu.Unlock()
	manager.loadRootGenerations(ctx)
	manager.mu.RLock()
	reloaded := manager.rootIDs[firstPath]
	manager.mu.RUnlock()
	if reloaded != firstID {
		t.Fatalf("reloaded retired generation id=%q, want %q", reloaded, firstID)
	}
	if _, _, err := manager.rootIDForPath(filepath.Join(firstPath, "wsp001", "slt001")); err != nil {
		t.Fatalf("slot on the retired root is no longer resolvable: %v", err)
	}
}

// TestLoadRootGenerationsRefusesAReplacedRootDirectory proves that a restart
// binds a durable generation only to the inode SQLite recorded for it. The
// pathname survives a manual delete and recreation, so without comparing the
// recorded identity the old generation would be re-bound to the replacement
// and every later proof under it would reach a directory wx never owned.
func TestLoadRootGenerationsRefusesAReplacedRootDirectory(t *testing.T) {
	ctx := context.Background()
	base := t.TempDir()
	store, err := state.Open(filepath.Join(base, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	cfg := config.Defaults()
	rootPath := filepath.Join(base, "worktrees")
	cfg.Storage.WorktreeRoot = rootPath
	manager := testManager(t, cfg, store)
	t.Cleanup(manager.Close)
	rootID := registerTestRoot(t, manager, rootPath)

	// Control: with the recorded directory still in place, a restart with an
	// empty registry republishes the generation. Without this the refusal
	// below would pass for the wrong reason.
	resetRootRegistryForTest(manager)
	manager.loadRootGenerations(ctx)
	manager.mu.RLock()
	republished := manager.rootIDs[rootPath]
	manager.mu.RUnlock()
	if republished != rootID {
		t.Fatalf("intact root republished as %q, want %q", republished, rootID)
	}

	if err := os.RemoveAll(rootPath); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(rootPath, 0o700); err != nil {
		t.Fatal(err)
	}
	resetRootRegistryForTest(manager)
	manager.loadRootGenerations(ctx)
	manager.mu.RLock()
	rebound := manager.rootIDs[rootPath]
	manager.mu.RUnlock()
	if rebound != "" {
		t.Fatalf("replaced root directory was re-bound to generation %q", rebound)
	}
	// Fail closed: nothing under the replaced pathname resolves to a
	// generation, so no durable state can be created or removed there.
	if _, _, err := manager.rootIDForPath(filepath.Join(rootPath, "wsp001", "slt001")); !errors.Is(err, state.ErrOwnership) {
		t.Fatalf("slot under a replaced root resolved: %v", err)
	}
}

// resetRootRegistryForTest empties the in-memory root registries the way a
// daemon restart does, leaving SQLite as the only source of generations.
func resetRootRegistryForTest(m *Manager) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ensureRootStateLocked()
	for path, entry := range m.rootRefs {
		m.closeRootLocked(path, entry)
	}
	m.rootIDs = map[string]string{}
	m.rootIdentities = map[string]string{}
	m.rootRefs = map[string]*managedRoot{}
}

// TestDirectoryIdentityAtReportsTheOpenedInode verifies the identity helper
// used to record slots.dir_identity and to answer the lease.
func TestDirectoryIdentityAtReportsTheOpenedInode(t *testing.T) {
	root := t.TempDir()
	owner, _, err := domain.OpenOwnedRoot(root, root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = owner.Close() })
	slot := filepath.Join(root, "wsp001", "slt001")
	if err := os.MkdirAll(slot, 0o700); err != nil {
		t.Fatal(err)
	}
	relative, ok := relativeWithinRoot(root, slot)
	if !ok {
		t.Fatalf("slot %s is not inside root %s", slot, root)
	}
	identity, err := directoryIdentityAt(owner, relative)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(slot)
	if err != nil {
		t.Fatal(err)
	}
	want, err := domain.FileIdentity(info)
	if err != nil {
		t.Fatal(err)
	}
	if identity != want {
		t.Fatalf("directory identity=%q want %q", identity, want)
	}
	if _, err := directoryIdentityAt(owner, filepath.Join("wsp001", "missing")); err == nil {
		t.Fatal("identity of a missing directory succeeded")
	}
}

// TestAllocationRetriesASlotIDCollision proves the collision path: the INSERT
// is the only place that can decide the race, so a duplicate slot ID must be
// reported by it and redrawn rather than silently taking over another slot.
func TestAllocationRetriesASlotIDCollision(t *testing.T) {
	ctx, manager, store, workspaceRecord, resolved, _ := managerCoverageFixture(t, "repository")
	rootPath, rootID, err := manager.activeRoot()
	if err != nil {
		t.Fatal(err)
	}
	taken, err := newSlotID()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateStandby(ctx, testSlot(t, manager, string(workspaceRecord.ID), taken, 1, "READY"), nil); err != nil {
		t.Fatal(err)
	}

	// Reusing the existing ID must be reported as a collision (retry=true), so
	// the caller redraws instead of failing the allocation.
	_, retry, err := manager.allocateWithID(ctx, taken, rootPath, rootID, "token", workspaceRecord, resolved, 1, "codex", 0, "STARTING", "PREPARING", "PREPARE", "")
	if err == nil || !retry {
		t.Fatalf("duplicate slot id retry=%v err=%v, want a retryable collision", retry, err)
	}
	if !state.IsIDCollision(err) {
		t.Fatalf("duplicate slot id error=%v, want a SQLite constraint violation", err)
	}

	// The same contract holds for the unbound and standby allocation paths.
	if _, retry, err := manager.allocateResumeSlotWithID(ctx, taken, rootPath, rootID, "token", "codex", 0); err == nil || !retry {
		t.Fatalf("duplicate unbound slot id retry=%v err=%v", retry, err)
	}
	if _, err := manager.createStandbySlot(ctx, rootPath, rootID, workspaceRecord, resolved, 1, nil); err != nil {
		t.Fatalf("standby allocation with a free id: %v", err)
	}

	// A fresh ID allocates normally and lands in the documented layout.
	lease, err := manager.allocate(ctx, workspaceRecord, resolved, 1, "codex", 0, "STARTING", "")
	if err != nil {
		t.Fatal(err)
	}
	slot, err := store.Slot(ctx, lease.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if !domain.ValidShortID(slot.ID) {
		t.Fatalf("allocated slot id=%q is not a short identifier", slot.ID)
	}
	if slot.RelPath != filepath.Join(string(workspaceRecord.ID), slot.ID) {
		t.Fatalf("allocated slot rel_path=%q, want %q", slot.RelPath, filepath.Join(string(workspaceRecord.ID), slot.ID))
	}
	// A single-repository workspace leases the repository directory, so the
	// ownership marker stays in the parent and out of the agent's view.
	if want := filepath.Join(slot.Path, testDirName(resolved[0].Repository, manager.Config())); lease.Path != want {
		t.Fatalf("lease path=%q, want %q", lease.Path, want)
	}
	if !strings.HasPrefix(lease.Path, slot.Path+string(filepath.Separator)) {
		t.Fatalf("lease path %q is not below its slot directory %q", lease.Path, slot.Path)
	}
}
