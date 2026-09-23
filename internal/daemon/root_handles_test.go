package daemon

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/HappyOnigiri/WX/internal/config"
	"github.com/HappyOnigiri/WX/internal/state"
)

func TestCloseRootHandlesClosesRetiredDescriptors(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	openDir := func(name string) *os.Root {
		t.Helper()
		p := filepath.Join(base, name)
		if err := os.Mkdir(p, 0o700); err != nil {
			t.Fatal(err)
		}
		r, err := os.OpenRoot(p)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}

	liveActive := openDir("active-live")
	alreadyClosedActive := openDir("active-closed")
	if err := alreadyClosedActive.Close(); err != nil {
		t.Fatal(err)
	}
	liveRetired := openDir("retired-live")
	alreadyClosedRetired := openDir("retired-closed")
	if err := alreadyClosedRetired.Close(); err != nil {
		t.Fatal(err)
	}

	m := &Manager{
		rootRefs: map[string]*managedRoot{
			filepath.Join(base, "active-live"):   {root: liveActive, refs: 0},
			filepath.Join(base, "active-nil"):    {root: nil, refs: 0},
			filepath.Join(base, "active-closed"): {root: alreadyClosedActive, refs: 0, closed: true},
		},
		retiredRefs: map[string][]*managedRoot{
			filepath.Join(base, "retired-mixed"): {
				{root: liveRetired, refs: 0},
				{root: nil, refs: 0},
				{root: alreadyClosedRetired, refs: 0, closed: true},
				nil,
			},
		},
	}

	m.closeRootHandles()

	if _, err := liveActive.Lstat("."); err == nil {
		t.Fatal("active live root descriptor was not closed")
	}
	if _, err := liveRetired.Lstat("."); err == nil {
		t.Fatal("retired live root descriptor was not closed")
	}
	if len(m.rootRefs) != 0 {
		t.Fatalf("rootRefs not cleared: %v", m.rootRefs)
	}
	if len(m.retiredRefs) != 0 {
		t.Fatalf("retiredRefs not cleared: %v", m.retiredRefs)
	}
}

func TestRootHasReferencesLockedCountsActiveAndRetiredGenerations(t *testing.T) {
	t.Parallel()
	m := &Manager{
		rootRefs: map[string]*managedRoot{
			"root": {refs: 0, closed: false},
		},
		retiredRefs: map[string][]*managedRoot{
			"root": {
				{refs: 0, closed: true},
				{refs: 2, closed: false},
			},
		},
	}
	m.mu.RLock()
	hasRefs := m.rootHasReferencesLocked("root")
	m.mu.RUnlock()
	if !hasRefs {
		t.Fatal("retired generation reference was not detected")
	}

	m.retiredRefs["root"][1].refs = 0
	m.mu.RLock()
	hasRefs = m.rootHasReferencesLocked("root")
	m.mu.RUnlock()
	if hasRefs {
		t.Fatal("dereferenced root incorrectly reported as referenced")
	}
}

func TestRootHandleForRootReportsNoDescriptorForUnknownRoot(t *testing.T) {
	t.Parallel()
	m := &Manager{}
	if got := m.rootHandleForRoot(filepath.Join(t.TempDir(), "unknown")); got != nil {
		t.Fatalf("unknown root unexpectedly returned a handle: %v", got)
	}
}

func TestAcquireRootLockedUsesOldestRetiredGenerationBoundary(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	opened, err := os.OpenRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	valid := &managedRoot{root: opened}
	invalidNewest := &managedRoot{closed: true}
	m := &Manager{retiredRefs: map[string][]*managedRoot{root: {valid, invalidNewest}}}
	m.mu.Lock()
	got, entry, found, err := m.acquireRootLocked(root, true)
	m.mu.Unlock()
	if err != nil || !found || got != opened || entry != valid {
		t.Fatalf("retired root acquisition got=%v entry=%p found=%v err=%v", got, entry, found, err)
	}
	if valid.refs != 1 {
		t.Fatalf("retired root refs=%d, want 1", valid.refs)
	}
	m.releaseRoot(root, valid)
	if valid.refs != 0 {
		t.Fatalf("retired root refs after release=%d, want 0", valid.refs)
	}
	if err := opened.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestAdoptRootExistingGenerationIncrementsReference(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	existing, err := os.OpenRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	entry := &managedRoot{root: existing}
	m := &Manager{rootRefs: map[string]*managedRoot{root: entry}}
	duplicate, err := os.OpenRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	got, release, err := m.adoptRoot(root, duplicate, true)
	if err != nil || got != existing {
		t.Fatalf("existing root adoption got=%v err=%v", got, err)
	}
	if entry.refs != 1 {
		t.Fatalf("existing root refs=%d, want 1", entry.refs)
	}
	release()
	if entry.refs != 0 {
		t.Fatalf("existing root refs after release=%d, want 0", entry.refs)
	}
	if err := existing.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestCloseRootLockedHandlesEmptyRetiredBoundary(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	m := &Manager{
		rootRefs:    map[string]*managedRoot{},
		retiredRefs: map[string][]*managedRoot{root: {}},
	}
	entry := &managedRoot{}
	m.closeRootLocked(root, entry)
	if !entry.closed {
		t.Fatal("root entry was not marked closed")
	}
	if retired, ok := m.retiredRefs[root]; !ok || len(retired) != 0 {
		t.Fatalf("empty retired generation list changed at the strict boundary: present=%v entries=%v", ok, retired)
	}
}

func TestReleaseRootDoesNotUnderflowReferenceCount(t *testing.T) {
	t.Parallel()
	entry := &managedRoot{}
	m := &Manager{}
	m.releaseRoot("root", entry)
	if entry.refs != 0 {
		t.Fatalf("root refs=%d, want zero-reference release to be ignored", entry.refs)
	}
}

func TestRootHandleForRootSkipsInvalidNewestRetiredGeneration(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	opened, err := os.OpenRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = opened.Close() })
	m := &Manager{retiredRefs: map[string][]*managedRoot{
		root: {{root: opened}, {closed: true}},
	}}
	if got := m.rootHandleForRoot(root); got != opened {
		t.Fatalf("retired root handle=%v, want the preceding live generation", got)
	}
}

func TestRetainLeaseRejectsPathOutsideKnownRoots(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	cfg := config.Defaults()
	cfg.Storage.WorktreeRoot = filepath.Join(root, "worktrees")
	store, err := openTestStoreAtPath(t, filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	m := testManager(t, cfg, store)
	defer m.Close()

	outside := filepath.Join(t.TempDir(), "outside")
	if err := m.retainLease("session", outside); !errors.Is(err, state.ErrOwnership) {
		t.Fatalf("outside-root lease error=%v", err)
	}
	m.mu.RLock()
	_, leased := m.leases["session"]
	m.mu.RUnlock()
	if leased {
		t.Fatal("outside-root path was recorded as a lease")
	}
}

func TestHoldRootForPathSkipsPathOutsideConfiguredRoot(t *testing.T) {
	t.Parallel()
	f := manualManagerFixture(t)

	release, err := f.Manager.holdRootForPath(filepath.Join(t.TempDir(), "outside"))
	if err != nil {
		t.Fatalf("outside-root path hold error=%v, want no-op success", err)
	}
	release()
}
