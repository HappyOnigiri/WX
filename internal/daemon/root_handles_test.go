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
