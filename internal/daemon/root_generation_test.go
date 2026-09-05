package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/HappyOnigiri/WX/internal/config"
	"github.com/HappyOnigiri/WX/internal/domain"
	"github.com/HappyOnigiri/WX/internal/state"
)

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

func TestOwnedRootArtifactPathsScansOnlyWxNamespaces(t *testing.T) {
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
	registerTestRoot(t, manager, rootPath)
	if _, release, err := manager.existingRootDescriptor(rootPath); err != nil {
		t.Fatal(err)
	} else {
		t.Cleanup(release)
	}

	workspaceSlot := filepath.Join(rootPath, "wsp001", "slt001")
	unboundSlot := filepath.Join(rootPath, unboundNamespace, "slt002")
	for _, directory := range []string{
		workspaceSlot,
		unboundSlot,
		filepath.Join(rootPath, "_recovery", "whatever"),
		filepath.Join(rootPath, "unrelated-project", "src"),
		filepath.Join(rootPath, "toolong01", "slt003"),
		filepath.Join(rootPath, "UPPER1", "slt004"),
	} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	paths, err := manager.ownedRootArtifactPaths(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{filepath.Clean(workspaceSlot): true, filepath.Clean(unboundSlot): true}
	if len(paths) != len(want) {
		t.Fatalf("scanned paths=%v, want only the wx slot directories %v", paths, want)
	}
	for _, path := range paths {
		if !want[filepath.Clean(path)] {
			t.Fatalf("scanned path %q is not a wx slot directory", path)
		}
	}
}

func TestRootRegistrationFailureReachesTheUserWithItsCause(t *testing.T) {
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
	manager.mu.Lock()
	manager.rootIDs = map[string]string{}
	manager.mu.Unlock()
	manager.registerRootGeneration(ctx, rootPath, "")

	_, _, err = manager.activeRoot()
	if !errors.Is(err, state.ErrOwnership) {
		t.Fatalf("activeRoot error=%v, want an ownership failure", err)
	}
	if !strings.Contains(err.Error(), "inode identity") {
		t.Fatalf("activeRoot error %q does not carry the cause", err)
	}
	doctor := manager.Doctor(ctx)
	checks := doctor["checks"].(map[string]any)
	if got, _ := checks["worktree_root"].(string); got == "ok" || got == "" {
		t.Fatalf("doctor worktree_root=%q, want the registration failure", got)
	}
	status, err := manager.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := status["worktree_root_error"].(string); got == "" {
		t.Fatalf("status worktree_root_error is empty: %v", status["worktree_root_error"])
	}

	if err := os.MkdirAll(rootPath, 0o700); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	identity, err := domain.FileIdentity(info)
	if err != nil {
		t.Fatal(err)
	}
	manager.registerRootGeneration(ctx, rootPath, identity)
	manager.mu.RLock()
	remaining := manager.rootError
	manager.mu.RUnlock()
	if remaining != "" {
		t.Fatalf("root error survived a successful registration: %q", remaining)
	}
}

func TestArtifactDiagnosticsScanRetiredRootGenerations(t *testing.T) {
	ctx := context.Background()
	base := t.TempDir()
	store, err := state.Open(filepath.Join(base, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	cfg := config.Defaults()
	oldRoot := filepath.Join(base, "old-root")
	cfg.Storage.WorktreeRoot = oldRoot
	manager := testManager(t, cfg, store)
	t.Cleanup(manager.Close)
	registerTestRoot(t, manager, oldRoot)
	orphan := filepath.Join(oldRoot, "wsp001", "slt001")
	if err := os.MkdirAll(orphan, 0o700); err != nil {
		t.Fatal(err)
	}

	newRoot := filepath.Join(base, "new-root")
	cfg.Storage.WorktreeRoot = newRoot
	manager.mu.Lock()
	manager.cfg = cfg
	manager.retireRootLocked(oldRoot)
	manager.mu.Unlock()
	registerTestRoot(t, manager, newRoot)
	manager.mu.RLock()
	_, live := manager.roots[oldRoot]
	manager.mu.RUnlock()
	if live {
		t.Fatal("retired root is still in the live set; the case under test no longer applies")
	}

	artifacts := manager.artifactDiagnostics(ctx)
	if !containsString(artifacts["unknown_paths"].([]string), filepath.Clean(orphan)) {
		t.Fatalf("orphan under the retired root was not reported: %v", artifacts)
	}
	status, err := manager.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(status["worktree_roots"])
	if err != nil {
		t.Fatal(err)
	}
	var reportedRoots []struct {
		Path   string `json:"path"`
		Active bool   `json:"active"`
	}
	if err := json.Unmarshal(encoded, &reportedRoots); err != nil {
		t.Fatal(err)
	}
	reported := false
	for _, item := range reportedRoots {
		if item.Path == oldRoot {
			reported = true
			if item.Active {
				t.Fatalf("retired root is reported as active: %+v", item)
			}
		}
	}
	if !reported {
		t.Fatalf("retired root is missing from status: %s", encoded)
	}
}

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
	if _, _, err := manager.rootIDForPath(filepath.Join(rootPath, "wsp001", "slt001")); !errors.Is(err, state.ErrOwnership) {
		t.Fatalf("slot under a replaced root resolved: %v", err)
	}
}

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

	_, retry, err := manager.allocateWithID(ctx, taken, rootPath, rootID, "token", workspaceRecord, resolved, 1, "codex", 0, "STARTING", "PREPARING", "PREPARE", "")
	if err == nil || !retry {
		t.Fatalf("duplicate slot id retry=%v err=%v, want a retryable collision", retry, err)
	}
	if !state.IsIDCollision(err) {
		t.Fatalf("duplicate slot id error=%v, want a SQLite constraint violation", err)
	}

	if _, retry, err := manager.allocateResumeSlotWithID(ctx, taken, rootPath, rootID, "token", "codex", 0); err == nil || !retry {
		t.Fatalf("duplicate unbound slot id retry=%v err=%v", retry, err)
	}
	if _, err := manager.createStandbySlot(ctx, rootPath, rootID, workspaceRecord, resolved, 1, nil); err != nil {
		t.Fatalf("standby allocation with a free id: %v", err)
	}

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
	if want := filepath.Join(slot.Path, testDirName(resolved[0].Repository, manager.Config())); lease.Path != want {
		t.Fatalf("lease path=%q, want %q", lease.Path, want)
	}
	if !strings.HasPrefix(lease.Path, slot.Path+string(filepath.Separator)) {
		t.Fatalf("lease path %q is not below its slot directory %q", lease.Path, slot.Path)
	}
}
