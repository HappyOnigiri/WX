package daemon

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/HappyOnigiri/WX/internal/state"
)

// TestReleaseIsIdempotentAfterAlreadyReleasingSession verifies that
// releasing an already-releasing session is a harmless no-op (the SQLite
// UPDATE affects zero rows, changed=false) instead of erroring or
// scheduling a second, duplicate snapshot job for the same session.
func TestReleaseIsIdempotentAfterAlreadyReleasingSession(t *testing.T) {
	root := t.TempDir()
	store, err := state.Open(filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	if _, err := store.CreateSlotSession(ctx, state.Slot{ID: "dup", Path: filepath.Join(root, "root"), State: "LEASED"}, nil, state.Session{ID: "dup", SlotID: "dup", State: "ACTIVE", AgentKind: "codex", TokenHash: state.HashToken("token")}, ""); err != nil {
		t.Fatal(err)
	}
	manager := &Manager{store: store, jobs: make(chan jobWork, 4), ctx: context.Background()}
	if err := manager.Release(ctx, "dup", "token", "client-exit"); err != nil {
		t.Fatal(err)
	}
	if err := manager.Release(ctx, "dup", "token", "client-exit"); err != nil {
		t.Fatalf("idempotent release error=%v", err)
	}
	if got := len(manager.jobs); got != 1 {
		t.Fatalf("duplicate release scheduled %d jobs, want 1", got)
	}
}

// TestAllocateFailsWhenWorktreeRootCannotBeCreated verifies that allocate
// surfaces a physical worktree-root creation failure (an ancestor path
// component that is a regular file, not a directory) instead of returning a
// lease for a slot whose root was never actually created.
func TestAllocateFailsWhenWorktreeRootCannotBeCreated(t *testing.T) {
	ctx, manager, _, workspaceRecord, resolved, _ := managerCoverageFixture(t)
	blocker := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(blocker, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	manager.mu.Lock()
	cfg := manager.cfg
	cfg.Storage.WorktreeRoot = filepath.Join(blocker, "worktrees")
	manager.cfg = cfg
	manager.mu.Unlock()

	if _, err := manager.allocate(ctx, workspaceRecord, resolved, 1, "codex", os.Getpid(), "STARTING", ""); err == nil {
		t.Fatal("allocate succeeded with an unusable worktree root")
	}
}

// TestAllocateReleasesLeaseWhenSessionPersistenceFails drives allocate
// through a crash between physical slot-root creation and the durable
// session/slot insert (simulated with an injected SQLite trigger). The
// lease taken to protect the new root must be released on that failure, or
// it would pin the descriptor open forever and eventually deadlock a
// subsequent manager close.
func TestAllocateReleasesLeaseWhenSessionPersistenceFails(t *testing.T) {
	ctx, manager, _, workspaceRecord, resolved, databasePath := managerCoverageFixture(t)
	raw := openManagerCoverageDB(t, databasePath)
	if _, err := raw.ExecContext(ctx, `CREATE TRIGGER fail_allocate_insert BEFORE INSERT ON slots BEGIN SELECT RAISE(ABORT,'injected slot insert failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.allocate(ctx, workspaceRecord, resolved, 1, "codex", os.Getpid(), "STARTING", ""); err == nil {
		t.Fatal("allocate succeeded despite an injected persistence failure")
	}
	manager.mu.RLock()
	leaseCount := len(manager.leases)
	manager.mu.RUnlock()
	if leaseCount != 0 {
		t.Fatalf("failed allocation left %d dangling lease(s)", leaseCount)
	}
}

// TestReconcileArtifactsSkipsUnverifiableAndArchivedPaths verifies two
// fail-closed guarantees of the startup artifact reconciler: a slot whose
// path cannot be proven owned (outside every known wx root) is left
// untouched rather than auto-quarantined merely because it could not be
// checked, and an already-ARCHIVED slot is excluded from the missing/unknown
// path diagnostics entirely.
func TestReconcileArtifactsSkipsUnverifiableAndArchivedPaths(t *testing.T) {
	ctx, manager, store, _, _, _ := managerCoverageFixture(t)
	outside := filepath.Join(t.TempDir(), "outside-slot")
	outsideSession := state.Session{ID: "outside", SlotID: "outside", State: "ACTIVE", AgentKind: "codex", TokenHash: state.HashToken("outside")}
	if _, err := store.CreateSlotSession(ctx, state.Slot{ID: "outside", Generation: 1, Path: outside, State: "LEASED"}, nil, outsideSession, ""); err != nil {
		t.Fatal(err)
	}
	archivedPath := filepath.Join(t.TempDir(), "archived-slot")
	archivedSession := state.Session{ID: "archived", SlotID: "archived", State: "ARCHIVED", AgentKind: "codex", TokenHash: state.HashToken("archived")}
	if _, err := store.CreateSlotSession(ctx, state.Slot{ID: "archived", Generation: 1, Path: archivedPath, State: "ARCHIVED"}, nil, archivedSession, ""); err != nil {
		t.Fatal(err)
	}

	manager.reconcileArtifacts(ctx)
	if slot, err := store.Slot(ctx, "outside"); err != nil || slot.State != "LEASED" {
		t.Fatalf("unverifiable artifact path was mutated: slot=%+v err=%v", slot, err)
	}

	diagnostics := manager.artifactDiagnostics(ctx)
	errorsList, _ := diagnostics["errors"].([]string)
	found := false
	for _, item := range errorsList {
		if strings.Contains(item, "outside") {
			found = true
		}
	}
	if !found {
		t.Fatalf("unverifiable artifact path was not reported: diagnostics=%v", diagnostics)
	}
	missing, _ := diagnostics["missing_paths"].([]string)
	for _, item := range missing {
		if strings.Contains(item, "archived-slot") {
			t.Fatalf("archived artifact incorrectly reported missing: %v", missing)
		}
	}
}
