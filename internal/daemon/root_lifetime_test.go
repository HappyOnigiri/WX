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
	// Reload tests exercise descriptor ownership directly and do not need
	// preparation workers. Marking the worker pool closed keeps reloads from
	// introducing unrelated goroutines while preserving Manager.Close ordering.
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

// A retired root that still owns an archived slot must remain discoverable by
// GC after its last live descriptor reference is released. This reproduces a
// reload followed by archival cleanup without starting lifecycle workers.
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
	if err := store.UpsertWorkspace(ctx, discovery.Workspace{ID: "workspace", Root: discoveryPath(home), Kind: "repository"}); err != nil {
		t.Fatal(err)
	}
	session := state.Session{ID: "session", WorkspaceID: "workspace", SlotID: "slot", State: "ARCHIVED", AgentKind: "codex", TokenHash: state.HashToken("token")}
	if _, err := store.CreateSlotSession(ctx, state.Slot{ID: "slot", WorkspaceID: "workspace", Generation: 1, Path: filepath.Join(oldRoot, "slot"), State: "SNAPSHOTTED"}, nil, session, ""); err != nil {
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
	count, err := manager.GC(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("GC count=%d, want one archived candidate", count)
	}
	jobs, err := store.RecoverJobs(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 || jobs[0].Kind != "REMOVE" {
		t.Fatalf("GC did not schedule retired-root removal: jobs=%+v", jobs)
	}
}

// NEW-5: unique root rotations must close unused retired descriptors instead
// of retaining one descriptor per historical configuration generation.
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
		handleCount := len(manager.rootHandles)
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

// A root used by a synchronous operation remains usable after reload and is
// closed immediately after its reference is released.
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

// A lease is a durable root reference: reload may retire its generation, but
// Release is the point at which the descriptor becomes eligible for closing.
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

// Reload, allocation, and shutdown overlap at deterministic barriers. The
// allocation may finish before shutdown wins the gate, but it must never use a
// closed descriptor and Close must always return after all references drain.
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
	active := len(manager.rootHandles)
	retired := len(manager.retiredRefs)
	refs := manager.rootReferenceCountLocked()
	manager.mu.RUnlock()
	if active != 0 || retired != 0 || refs != 0 {
		t.Fatalf("shutdown left descriptors active=%d retired=%d refs=%d", active, retired, refs)
	}
}

// A same-path inode replacement must not be silently accepted as the same
// generation. Reload remains atomic and keeps the old descriptor/configuration
// so subsequent allocation can fail closed instead of mixing namespaces.
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
	if err := store.UpsertWorkspace(context.Background(), discovery.Workspace{ID: "workspace", Root: discoveryPath(home), Kind: "repository"}); err != nil {
		t.Fatal(err)
	}
	const slotID = "preparing-slot"
	slotPath := filepath.Join(oldRoot, "workspaces", "workspace", "slots", slotID, "root")
	writeRootLifetimeConfig(t, home, newRoot)
	if err := manager.reloadConfig(false); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateStandby(context.Background(), state.Slot{ID: slotID, WorkspaceID: "workspace", Generation: 1, Path: slotPath, State: "PREPARING"}, nil); err != nil {
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
	compatibility := len(manager.rootHandles)
	retired := len(manager.retiredRefs)
	refs := manager.rootReferenceCountLocked()
	manager.mu.RUnlock()
	if active != 0 || compatibility != 0 || retired != 0 || refs != 0 {
		t.Fatalf("concurrent close left root state active=%d compatibility=%d retired=%d refs=%d", active, compatibility, retired, refs)
	}
}
