package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/HappyOnigiri/WX/internal/config"
	"github.com/HappyOnigiri/WX/internal/discovery"
	"github.com/HappyOnigiri/WX/internal/state"
)

func rootLifetimeManager(t *testing.T, cfg config.Config, store *state.Store) *Manager {
	t.Helper()
	manager := testManager(t, cfg, store)
	manager.workersMu.Lock()
	manager.closed = true
	manager.workersMu.Unlock()
	return manager
}

func writeRootLifetimeConfig(t *testing.T, home, root string) {
	t.Helper()
	path := filepath.Join(home, ".config", "wx", "config.yaml")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	document := fmt.Sprintf("version: 1\nstorage:\n  worktree_root: %s\n", root)
	if err := os.WriteFile(path, []byte(document), 0o600); err != nil {
		t.Fatal(err)
	}
}

func writeRootLifetimeGCConfig(t *testing.T, home, root string) {
	t.Helper()
	path := filepath.Join(home, ".config", "wx", "config.yaml")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	document := fmt.Sprintf("version: 1\nstorage:\n  worktree_root: %s\nretention:\n  ended_worktree: 0s\n", root)
	if err := os.WriteFile(path, []byte(document), 0o600); err != nil {
		t.Fatal(err)
	}
}

func openDescriptorCount(t *testing.T) int {
	t.Helper()
	directory, err := os.Open("/dev/fd")
	if err != nil {
		t.Fatal(err)
	}
	defer directory.Close()
	entries, err := directory.Readdirnames(-1)
	if err != nil {
		t.Fatal(err)
	}
	return len(entries)
}

func TestGCDiscoversArchivedSlotOnClosedRetiredRoot(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	oldRoot := filepath.Join(home, "root-old")
	newRoot := filepath.Join(home, "root-new")
	if err := os.Mkdir(oldRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(newRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(oldRoot, "slot"), 0o700); err != nil {
		t.Fatal(err)
	}
	store, err := state.Open(filepath.Join(home, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ctx := context.Background()
	workspaceID := string(registerTestWorkspace(t, store, discovery.Workspace{Root: discoveryPath(home), Kind: "repository"}).ID)
	session := state.Session{ID: "session", WorkspaceID: workspaceID, SlotID: "slot", State: "ARCHIVED", AgentKind: "codex", TokenHash: state.HashToken("token")}
	if _, err := store.CreateSlotSession(ctx, storeSlotAt(t, store, oldRoot, workspaceID, "slot", filepath.Join(oldRoot, "slot"), 1, "SNAPSHOTTED"), nil, session, ""); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkArchived(ctx, session.ID, session.SlotID, state.FormatTime(time.Now().Add(time.Hour))); err != nil {
		t.Fatal(err)
	}
	cfg := config.Defaults()
	cfg.Storage.WorktreeRoot = oldRoot
	cfg.Retention.EndedWorktree.Duration = 0
	manager := rootLifetimeManager(t, cfg, store)
	t.Cleanup(manager.Close)
	if _, release, err := manager.rootDescriptor(oldRoot); err != nil {
		t.Fatal(err)
	} else {
		release()
	}
	writeRootLifetimeGCConfig(t, home, newRoot)
	if err := manager.reloadConfig(false); err != nil {
		t.Fatal(err)
	}
	result, err := manager.GC(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	if result.Scheduled != 1 || result.Candidates != 1 {
		t.Fatalf("GC result=%+v, want one archived candidate reservation", result)
	}
	jobs, err := store.RecoverJobs(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 || jobs[0].Kind != "REMOVE" {
		t.Fatalf("GC did not schedule retired-root removal: jobs=%+v", jobs)
	}
}

func TestReloadUniqueRootsRetiresDescriptorsWithinBound(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	initialRoot := filepath.Join(home, "root-initial")
	if err := os.Mkdir(initialRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	store, err := state.Open(filepath.Join(home, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	cfg := config.Defaults()
	cfg.Storage.WorktreeRoot = initialRoot
	manager := rootLifetimeManager(t, cfg, store)
	t.Cleanup(manager.Close)
	baseline := openDescriptorCount(t)
	for index := 0; index < 48; index++ {
		root := filepath.Join(home, fmt.Sprintf("root-%02d", index))
		if err := os.Mkdir(root, 0o700); err != nil {
			t.Fatal(err)
		}
		writeRootLifetimeConfig(t, home, root)
		if err := manager.reloadConfig(false); err != nil {
			t.Fatalf("reload %d: %v", index, err)
		}
		manager.mu.RLock()
		handleCount := len(manager.rootRefs)
		retiredCount := len(manager.retiredRefs)
		rootPathCount := len(manager.roots)
		manager.mu.RUnlock()
		if handleCount != 1 || retiredCount != 0 || rootPathCount != 1 {
			t.Fatalf("reload %d retained handles=%d retired roots=%d root paths=%d", index, handleCount, retiredCount, rootPathCount)
		}
	}
	if got := openDescriptorCount(t); got > baseline+3 {
		t.Fatalf("descriptor count grew from %d to %d after unique reload soak", baseline, got)
	}
	manager.Close()
	if got := openDescriptorCount(t); got > baseline+2 {
		t.Fatalf("descriptor count remained %d after Manager.Close (baseline %d)", got, baseline)
	}
}

func TestRetiredRootSurvivesInflightOperationUntilRelease(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	oldRoot := filepath.Join(home, "root-old")
	newRoot := filepath.Join(home, "root-new")
	if err := os.Mkdir(oldRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(newRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	store, err := state.Open(filepath.Join(home, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	cfg := config.Defaults()
	cfg.Storage.WorktreeRoot = oldRoot
	manager := rootLifetimeManager(t, cfg, store)
	t.Cleanup(manager.Close)
	owner, release, err := manager.rootDescriptor(oldRoot)
	if err != nil {
		t.Fatal(err)
	}
	manager.mu.RLock()
	entry := manager.rootRefs[oldRoot]
	manager.mu.RUnlock()
	if entry == nil {
		t.Fatal("in-flight root reference was not registered")
	}
	writeRootLifetimeConfig(t, home, newRoot)
	if err := manager.reloadConfig(false); err != nil {
		t.Fatal(err)
	}
	manager.mu.RLock()
	retired := len(manager.retiredRefs[oldRoot])
	refs := entry.refs
	manager.mu.RUnlock()
	if retired != 1 || refs != 1 {
		t.Fatalf("retired root entries=%d refs=%d, want one active reference", retired, refs)
	}
	if err := owner.MkdirAll("operation-finished", 0o700); err != nil {
		t.Fatalf("in-flight old-root operation failed after reload: %v", err)
	}
	release()
	manager.mu.RLock()
	closed := entry.closed
	retired = len(manager.retiredRefs[oldRoot])
	manager.mu.RUnlock()
	if !closed || retired != 0 {
		t.Fatalf("released old root closed=%v retired entries=%d", closed, retired)
	}
	if _, err := os.Stat(filepath.Join(oldRoot, "operation-finished")); err != nil {
		t.Fatalf("old-root operation was not persisted: %v", err)
	}
}

func TestLeaseReleaseRetiresOldRootDescriptor(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	oldRoot := filepath.Join(home, "root-old")
	newRoot := filepath.Join(home, "root-new")
	leasePath := filepath.Join(oldRoot, "workspaces", "slot", "root")
	if err := os.MkdirAll(leasePath, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(newRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	store, err := state.Open(filepath.Join(home, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	cfg := config.Defaults()
	cfg.Storage.WorktreeRoot = oldRoot
	manager := rootLifetimeManager(t, cfg, store)
	t.Cleanup(manager.Close)
	if err := manager.retainLease("session", leasePath); err != nil {
		t.Fatal(err)
	}
	writeRootLifetimeConfig(t, home, newRoot)
	if err := manager.reloadConfig(false); err != nil {
		t.Fatal(err)
	}
	manager.mu.RLock()
	entries := manager.retiredRefs[oldRoot]
	if len(entries) != 1 || entries[0].refs != 1 {
		manager.mu.RUnlock()
		t.Fatalf("lease did not pin retired root: entries=%d", len(entries))
	}
	entry := entries[0]
	manager.mu.RUnlock()
	manager.releaseLease("session")
	manager.mu.RLock()
	closed := entry.closed
	remaining := len(manager.retiredRefs[oldRoot])
	leaseCount := len(manager.leases)
	manager.mu.RUnlock()
	if !closed || remaining != 0 || leaseCount != 0 {
		t.Fatalf("release left old descriptor closed=%v retired=%d leases=%d", closed, remaining, leaseCount)
	}
}

func TestConcurrentCloseReloadAndAllocationHasNoUseAfterClose(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	oldRoot := filepath.Join(home, "root-old")
	newRoot := filepath.Join(home, "root-new")
	if err := os.Mkdir(oldRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(newRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	store, err := state.Open(filepath.Join(home, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	cfg := config.Defaults()
	cfg.Storage.WorktreeRoot = oldRoot
	manager := rootLifetimeManager(t, cfg, store)

	allocationEntered := make(chan struct{})
	allowAllocation := make(chan struct{})
	manager.beforeSlotRootCreate = func() {
		close(allocationEntered)
		<-allowAllocation
	}
	closeEntered := make(chan struct{})
	allowClose := make(chan struct{})
	manager.beforeRootClose = func() {
		close(closeEntered)
		<-allowClose
	}

	allocationDone := make(chan error, 1)
	go func() {
		_, allocationErr := manager.AllocateResumeSlot(context.Background(), "codex", os.Getpid())
		allocationDone <- allocationErr
	}()
	<-allocationEntered
	writeRootLifetimeConfig(t, home, newRoot)
	reloadDone := make(chan error, 1)
	go func() { reloadDone <- manager.reloadConfig(false) }()
	closeDone := make(chan struct{})
	go func() {
		manager.Close()
		close(closeDone)
	}()
	<-closeEntered
	if err := <-reloadDone; err != nil && !errors.Is(err, errManagerClosed) {
		t.Fatalf("concurrent reload failed unexpectedly: %v", err)
	}
	close(allowClose)
	close(allowAllocation)
	allocationErr := <-allocationDone
	if allocationErr != nil && !errors.Is(allocationErr, errManagerClosed) {
		t.Fatalf("concurrent allocation failed unexpectedly: %v", allocationErr)
	}
	<-closeDone
	manager.mu.RLock()
	active := len(manager.rootRefs)
	retired := len(manager.retiredRefs)
	refs := manager.rootReferenceCountLocked()
	manager.mu.RUnlock()
	if active != 0 || retired != 0 || refs != 0 {
		t.Fatalf("shutdown left descriptors active=%d retired=%d refs=%d", active, retired, refs)
	}
}

func TestReloadRejectsSamePathRootInodeReplacementAtomically(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	root := filepath.Join(home, "worktrees")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	store, err := state.Open(filepath.Join(home, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	cfg := config.Defaults()
	cfg.Storage.WorktreeRoot = root
	manager := rootLifetimeManager(t, cfg, store)
	t.Cleanup(manager.Close)
	if _, release, err := manager.rootDescriptor(root); err != nil {
		t.Fatal(err)
	} else {
		release()
	}
	manager.mu.RLock()
	entry := manager.rootRefs[root]
	manager.mu.RUnlock()
	if entry == nil {
		t.Fatal("manager did not register the current root descriptor")
	}
	oldRoot := root + "-old"
	if err := os.Rename(root, oldRoot); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	writeRootLifetimeConfig(t, home, root)
	if err := manager.reloadConfig(false); err == nil {
		t.Fatal("reload accepted a same-path inode replacement")
	}
	if got := manager.Config().Storage.WorktreeRoot; got != root {
		t.Fatalf("failed reload changed config root to %q, want %q", got, root)
	}
	manager.mu.RLock()
	current := manager.rootRefs[root]
	retired := len(manager.retiredRefs[root])
	closed := entry.closed
	manager.mu.RUnlock()
	if current != entry || retired != 0 || closed {
		t.Fatalf("failed reload changed descriptor state: current=%p want=%p retired=%d closed=%v", current, entry, retired, closed)
	}
}

func TestReloadRejectsReplacementOfClosedHistoricalRoot(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	oldRoot := filepath.Join(home, "root-old")
	newRoot := filepath.Join(home, "root-new")
	if err := os.Mkdir(oldRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(newRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	store, err := state.Open(filepath.Join(home, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	cfg := config.Defaults()
	cfg.Storage.WorktreeRoot = oldRoot
	manager := rootLifetimeManager(t, cfg, store)
	t.Cleanup(manager.Close)
	if _, release, err := manager.rootDescriptor(oldRoot); err != nil {
		t.Fatal(err)
	} else {
		release()
	}
	writeRootLifetimeConfig(t, home, newRoot)
	if err := manager.reloadConfig(false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(oldRoot); err != nil {
		t.Fatal(err)
	}
	originalRoot := oldRoot + "-original"
	if err := os.Rename(oldRoot, originalRoot); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(oldRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	writeRootLifetimeConfig(t, home, oldRoot)
	if err := manager.reloadConfig(false); err == nil {
		t.Fatal("reload accepted a replacement of a closed historical root")
	}
	if got := manager.Config().Storage.WorktreeRoot; got != newRoot {
		t.Fatalf("failed historical-root reload changed config to %q, want %q", got, newRoot)
	}
	if _, release, err := manager.existingRootDescriptor(oldRoot); err == nil {
		release()
		t.Fatal("historical slot lookup accepted a replacement root inode")
	}
	if _, release, err := manager.holdVerifiedRootForPath(filepath.Join(oldRoot, "slot")); err == nil {
		release()
		t.Fatal("cleanup accepted a replacement historical root inode")
	}
}

func TestRootReplacementQuarantinesPreparationBeforeDescriptorAcquire(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	oldRoot := filepath.Join(home, "root-old")
	newRoot := filepath.Join(home, "root-new")
	if err := os.Mkdir(oldRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(newRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	store, err := state.Open(filepath.Join(home, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	cfg := config.Defaults()
	cfg.Storage.WorktreeRoot = oldRoot
	manager := rootLifetimeManager(t, cfg, store)
	t.Cleanup(manager.Close)
	if _, release, err := manager.rootDescriptor(oldRoot); err != nil {
		t.Fatal(err)
	} else {
		release()
	}
	staleWorkspaceID := string(registerTestWorkspace(t, store, discovery.Workspace{Root: discoveryPath(home), Kind: "repository"}).ID)
	const slotID = "preparing-slot"
	staleRootID := registerTestRoot(t, manager, oldRoot)
	staleRelative, err := slotRelPath(staleWorkspaceID, slotID, false)
	if err != nil {
		t.Fatal(err)
	}
	slotPath := filepath.Join(oldRoot, staleRelative)
	if err := os.MkdirAll(slotPath, 0o700); err != nil {
		t.Fatal(err)
	}
	writeRootLifetimeConfig(t, home, newRoot)
	if err := manager.reloadConfig(false); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateStandby(context.Background(), state.Slot{ID: slotID, WorkspaceID: staleWorkspaceID, Generation: 1, RootID: staleRootID, RelPath: staleRelative, Path: slotPath, State: "PREPARING"}, nil); err != nil {
		t.Fatal(err)
	}
	originalRoot := oldRoot + "-original"
	if err := os.Rename(oldRoot, originalRoot); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(oldRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := manager.prepareSlot(context.Background(), slotID, discovery.Workspace{}, nil, nil); !errors.Is(err, state.ErrOwnership) {
		t.Fatalf("preparation accepted replaced historical root: %v", err)
	}
	slot, err := store.Slot(context.Background(), slotID)
	if err != nil {
		t.Fatal(err)
	}
	if slot.State != "QUARANTINED" {
		t.Fatalf("replaced historical root left slot in %s, want QUARANTINED", slot.State)
	}
}

func TestRootStatusRejectsReplacedRootGeneration(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	root := filepath.Join(home, "worktrees")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "owned.txt"), []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := state.Open(filepath.Join(home, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	cfg := config.Defaults()
	cfg.Storage.WorktreeRoot = root
	manager := rootLifetimeManager(t, cfg, store)
	t.Cleanup(manager.Close)
	if _, release, err := manager.rootDescriptor(root); err != nil {
		t.Fatal(err)
	} else {
		release()
	}
	if err := os.Rename(root, root+"-old"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "replacement.txt"), []byte("replacement"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := manager.rootDirectoryUsage(root); !errors.Is(err, state.ErrOwnership) {
		t.Fatalf("status accepted replaced root generation: %v", err)
	}
}

func TestReloadRejectsOverlappingRootWhileGenerationIsHeld(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	oldRoot := filepath.Join(home, "worktrees")
	nestedRoot := filepath.Join(oldRoot, "nested")
	if err := os.MkdirAll(nestedRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	store, err := state.Open(filepath.Join(home, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	cfg := config.Defaults()
	cfg.Storage.WorktreeRoot = oldRoot
	manager := rootLifetimeManager(t, cfg, store)
	t.Cleanup(manager.Close)
	_, release, err := manager.rootDescriptor(oldRoot)
	if err != nil {
		t.Fatal(err)
	}
	writeRootLifetimeConfig(t, home, nestedRoot)
	if err := manager.reloadConfig(false); err == nil {
		release()
		t.Fatal("reload accepted an overlapping root while the old generation was held")
	}
	if got := manager.Config().Storage.WorktreeRoot; got != oldRoot {
		t.Fatalf("failed overlapping reload changed config root to %q, want %q", got, oldRoot)
	}
	release()
	if err := manager.reloadConfig(false); err != nil {
		t.Fatalf("reload after old generation release failed: %v", err)
	}
}

func TestConcurrentCloseIsIdempotent(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	root := filepath.Join(home, "worktrees")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	store, err := state.Open(filepath.Join(home, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	cfg := config.Defaults()
	cfg.Storage.WorktreeRoot = root
	manager := rootLifetimeManager(t, cfg, store)
	if _, release, err := manager.rootDescriptor(root); err != nil {
		t.Fatal(err)
	} else {
		release()
	}
	const callers = 16
	start := make(chan struct{})
	done := make(chan struct{}, callers)
	for range callers {
		go func() {
			<-start
			manager.Close()
			done <- struct{}{}
		}()
	}
	close(start)
	for range callers {
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("concurrent Manager.Close did not return")
		}
	}
	manager.mu.RLock()
	active := len(manager.rootRefs)
	retired := len(manager.retiredRefs)
	refs := manager.rootReferenceCountLocked()
	manager.mu.RUnlock()
	if active != 0 || retired != 0 || refs != 0 {
		t.Fatalf("concurrent close left root state active=%d retired=%d refs=%d", active, retired, refs)
	}
}

func TestRootReferenceAccountingAndBackgroundAdmission(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	manager := &Manager{
		roots:          map[string]bool{root: true},
		rootRefs:       map[string]*managedRoot{},
		retiredRefs:    map[string][]*managedRoot{},
		rootIdentities: map[string]string{},
	}
	owner, release, err := manager.rootDescriptor(root)
	if err != nil {
		t.Fatal(err)
	}
	if owner == nil || manager.rootHandleForPath(filepath.Join(root, "nested")) == nil {
		t.Fatal("active root descriptor was not discoverable")
	}
	manager.mu.Lock()
	entry := manager.rootRefs[root]
	if entry == nil || !manager.rootHasReferencesLocked(root) || manager.rootReferenceCountLocked() != 1 {
		manager.mu.Unlock()
		t.Fatalf("active root accounting is incorrect: entry=%+v refs=%d", entry, manager.rootReferenceCountLocked())
	}
	_, acquired, found, acquireErr := manager.acquireRootLocked(root, false)
	manager.mu.Unlock()
	if acquireErr != nil || !found || acquired != entry {
		t.Fatalf("reacquire root=%v found=%v err=%v", acquired, found, acquireErr)
	}
	manager.releaseRoot(root, entry)
	release()
	manager.mu.RLock()
	if manager.rootReferenceCountLocked() != 0 || manager.rootHasReferencesLocked(root) {
		manager.mu.RUnlock()
		t.Fatal("root references were not released")
	}
	manager.mu.RUnlock()

	started := make(chan struct{})
	if !manager.startBackground(func() { close(started) }) {
		t.Fatal("background task was rejected before shutdown")
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("background task did not run")
	}
	manager.backgroundMu.Lock()
	manager.backgroundClosing = true
	manager.backgroundMu.Unlock()
	if manager.startBackground(func() {}) {
		t.Fatal("background task was admitted after shutdown")
	}
	manager.backgroundWG.Wait()

	manager.mu.Lock()
	manager.rootClosing = true
	_, _, _, acquireErr = manager.acquireRootLocked(root, false)
	manager.mu.Unlock()
	if !errors.Is(acquireErr, errManagerClosed) {
		t.Fatalf("root acquire during shutdown error=%v", acquireErr)
	}
	manager.Close()
}

func TestPinnedRootOperationsValidateAndMaterializeThroughDescriptors(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	root := filepath.Join(base, "worktrees")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	store, err := state.Open(filepath.Join(base, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	cfg := config.Defaults()
	cfg.Storage.WorktreeRoot = root
	manager := testManager(t, cfg, store)
	t.Cleanup(manager.Close)

	owner, release, err := manager.rootDescriptor(root)
	if err != nil {
		t.Fatal(err)
	}
	if identity, identityErr := descriptorIdentity(owner); identityErr != nil || identity == "" {
		t.Fatalf("descriptor identity=%q err=%v", identity, identityErr)
	}
	release()
	if _, err := descriptorIdentity(nil); err == nil {
		t.Fatal("nil descriptor identity succeeded")
	}
	closed, err := os.OpenRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := closed.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := descriptorIdentity(closed); err == nil {
		t.Fatal("closed descriptor identity succeeded")
	}

	slotPath := filepath.Join(root, "workspace", "slot")
	if _, _, err := manager.createSlotRoot(slotPath, slotPath); err != nil {
		t.Fatalf("create slot root: %v", err)
	}
	if identity, err := manager.ownedDirectoryIdentity(slotPath); err != nil || identity == "" {
		t.Fatalf("lease root identity=%q err=%v", identity, err)
	}
	if _, err := manager.ownedDirectoryIdentity(filepath.Join(root, "missing")); err == nil {
		t.Fatal("missing lease root identity succeeded")
	}
	if _, _, err := manager.createSlotRoot(filepath.Join(base, "outside"), filepath.Join(base, "outside")); err == nil {
		t.Fatal("outside slot root creation succeeded")
	}

	file := filepath.Join(root, "owned.txt")
	if err := os.WriteFile(file, []byte("owned"), 0o600); err != nil {
		t.Fatal(err)
	}
	if exists, err := manager.ownedPathExists(file); err != nil || !exists {
		t.Fatalf("owned file exists=%v err=%v", exists, err)
	}
	if exists, err := manager.ownedPathExists(filepath.Join(root, "not-present")); err != nil || exists {
		t.Fatalf("missing owned path exists=%v err=%v", exists, err)
	}
	if exists, err := manager.ownedPathExists(filepath.Join(base, "outside")); !errors.Is(err, state.ErrOwnership) || exists {
		t.Fatalf("outside owned path exists=%v err=%v", exists, err)
	}
	if heldRoot, releaseRoot, err := manager.holdVerifiedRootForPath(file); err != nil || heldRoot != root {
		t.Fatalf("verified root=%q err=%v", heldRoot, err)
	} else {
		releaseRoot()
	}
	if _, releaseRoot, err := manager.holdVerifiedRootForPath(filepath.Join(base, "outside")); err == nil {
		releaseRoot()
		t.Fatal("outside verified root succeeded")
	}

	source := filepath.Join(base, "source")
	if err := os.Mkdir(source, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "instructions"), []byte("rules\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	materialized := filepath.Join(root, "materialized")
	if err := os.Mkdir(materialized, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := manager.materializeWorkspaceRoot(source, materialized, config.Workspace{Copy: []string{"instructions"}}); err != nil {
		t.Fatalf("descriptor-bound root materialization: %v", err)
	}
	if data, err := os.ReadFile(filepath.Join(materialized, "instructions")); err != nil || string(data) != "rules\n" {
		t.Fatalf("materialized data=%q err=%v", data, err)
	}

	validSlot := filepath.Join(root, "wsp001", "slt001")
	if err := os.MkdirAll(validSlot, 0o700); err != nil {
		t.Fatal(err)
	}
	validUnboundSlot := filepath.Join(root, unboundNamespace, "slt002")
	if err := os.MkdirAll(validUnboundSlot, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "wsp002"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "wsp002", "regular"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "_recovery", "workspace-snapshots"), 0o700); err != nil {
		t.Fatal(err)
	}
	paths, err := manager.ownedRootArtifactPaths(root)
	if err != nil {
		t.Fatalf("owned root artifacts: %v", err)
	}
	if !containsString(paths, validSlot) || !containsString(paths, validUnboundSlot) {
		t.Fatalf("owned root artifacts=%v", paths)
	}
	for _, unexpected := range []string{filepath.Join(root, "wsp002", "regular"), filepath.Join(root, "_recovery", "workspace-snapshots")} {
		if containsString(paths, unexpected) {
			t.Fatalf("owned root artifacts=%v reported %s", paths, unexpected)
		}
	}
}

func TestAdoptRootRejectsShutdownAndIdentityChanges(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	root := filepath.Join(base, "root")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}

	identityManager := &Manager{rootIdentities: map[string]string{root: "different"}}
	opened, err := os.OpenRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := identityManager.adoptRoot(root, opened, true); err == nil {
		t.Fatal("root inode replacement was accepted")
	}
	closingManager := &Manager{rootIdentities: map[string]string{}, rootClosing: true}
	opened, err = os.OpenRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := closingManager.adoptRoot(root, opened, true); !errors.Is(err, errManagerClosed) {
		t.Fatalf("shutdown root adoption error=%v", err)
	}

	existingPath := filepath.Join(base, "existing")
	if err := os.Mkdir(existingPath, 0o700); err != nil {
		t.Fatal(err)
	}
	existing, err := os.OpenRoot(existingPath)
	if err != nil {
		t.Fatal(err)
	}
	manager := &Manager{rootRefs: map[string]*managedRoot{existingPath: {root: existing, refs: 0}}, rootIdentities: map[string]string{}}
	other, err := os.OpenRoot(existingPath)
	if err != nil {
		t.Fatal(err)
	}
	returned, release, err := manager.adoptRoot(existingPath, other, true)
	if err != nil || returned != existing {
		t.Fatalf("existing root adoption=%v err=%v", returned, err)
	}
	release()
	manager.Close()
}

func TestReleaseRootAndCloseRootLockedGuardClauses(t *testing.T) {
	t.Parallel()
	manager := &Manager{
		rootRefs:       map[string]*managedRoot{},
		retiredRefs:    map[string][]*managedRoot{},
		rootIdentities: map[string]string{},
	}

	manager.releaseRoot("missing", nil)
	closedEntry := &managedRoot{closed: true}
	manager.releaseRoot("closed", closedEntry)
	zeroRefsEntry := &managedRoot{refs: 0}
	manager.releaseRoot("zero-refs", zeroRefsEntry)

	manager.closeRootLocked("missing", nil)
	manager.closeRootLocked("closed", closedEntry)
	if !closedEntry.closed {
		t.Fatal("already-closed entry lost its closed flag")
	}

	root := t.TempDir()
	opened, err := os.OpenRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	entry := &managedRoot{root: opened, refs: 1}
	other := &managedRoot{root: nil, refs: 1}
	manager.retiredRefs[root] = []*managedRoot{entry, other}
	manager.rootRefs[root] = entry
	manager.roots = map[string]bool{root: true}
	manager.closeRootLocked(root, entry)
	if !entry.closed {
		t.Fatal("closeRootLocked did not mark the entry closed")
	}
	if _, err := opened.Lstat("."); err == nil {
		t.Fatal("closeRootLocked did not close the underlying descriptor")
	}
	if _, stillTracked := manager.rootRefs[root]; stillTracked {
		t.Fatal("closed entry remained the active root reference")
	}
	if retired := manager.retiredRefs[root]; len(retired) != 1 || retired[0] != other {
		t.Fatalf("closed entry was not filtered out of the retired list: %+v", retired)
	}

	manager.closeRootLocked(root, other)
	if _, stillPresent := manager.retiredRefs[root]; stillPresent {
		t.Fatal("retired list was not removed once empty")
	}
}
