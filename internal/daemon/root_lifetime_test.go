package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/HappyOnigiri/WX/internal/config"
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
		manager.mu.RUnlock()
		if handleCount != 1 || retiredCount != 0 {
			t.Fatalf("reload %d retained handles=%d retired roots=%d", index, handleCount, retiredCount)
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
