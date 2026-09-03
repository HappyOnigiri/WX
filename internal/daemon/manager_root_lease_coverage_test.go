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
	"github.com/HappyOnigiri/WX/internal/workspace"
)

// TestCloseRootHandlesClosesRetiredDescriptors exercises the branches of
// closeRootHandles that are not reached by adoptRoot-driven tests: an active
// entry that is already closed (must not be double-closed or panic), a nil
// descriptor recorded alongside live ones (must not panic on a nil
// *os.Root.Close()), and retired-generation entries with the same shapes.
func TestCloseRootHandlesClosesRetiredDescriptors(t *testing.T) {
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

// TestRootHasReferencesLockedCountsActiveAndRetiredGenerations verifies that
// a retired-generation reference alone is sufficient to report the root as
// referenced, and that a fully dereferenced/closed state reports false.
func TestRootHasReferencesLockedCountsActiveAndRetiredGenerations(t *testing.T) {
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

// TestRootHandleForRootReportsNoDescriptorForUnknownRoot covers the branch
// that reports no descriptor at all for an unknown root.
func TestRootHandleForRootReportsNoDescriptorForUnknownRoot(t *testing.T) {
	m := &Manager{}
	if got := m.rootHandleForRoot(filepath.Join(t.TempDir(), "unknown")); got != nil {
		t.Fatalf("unknown root unexpectedly returned a handle: %v", got)
	}
}

// TestOwnedPathExistsPropagatesManagerClosedError verifies that a shutting
// down manager reports errManagerClosed verbatim from ownedPathExists rather
// than wrapping it as an ordinary ownership failure GC could quarantine.
func TestOwnedPathExistsPropagatesManagerClosedError(t *testing.T) {
	root := t.TempDir()
	cfg := config.Defaults()
	cfg.Storage.WorktreeRoot = filepath.Join(root, "worktrees")
	store, err := state.Open(filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	m := testManager(t, cfg, store)
	defer m.Close()
	m.mu.Lock()
	m.ensureRootStateLocked()
	m.rootClosing = true
	m.mu.Unlock()

	target := filepath.Join(cfg.Storage.WorktreeRoot, "slot", "root")
	if ok, err := m.ownedPathExists(target); ok || !errors.Is(err, errManagerClosed) {
		t.Fatalf("closing manager owned path check ok=%v err=%v", ok, err)
	}
}

// TestOwnedPathExistsReportsUnreadablePath verifies that ownedPathExists
// reports a genuine filesystem error (an unreadable parent directory) as an
// ownership failure distinct from an ordinary missing path, since the
// caller must not treat "cannot tell" the same as "confirmed absent" when
// deciding whether to quarantine.
func TestOwnedPathExistsReportsUnreadablePath(t *testing.T) {
	_, manager, _, _, _, _ := managerCoverageFixture(t)
	root := manager.Config().Storage.WorktreeRoot
	if _, err := manager.createSlotRoot(filepath.Join(root, "bootstrap", "root")); err != nil {
		t.Fatal(err)
	}
	blockedDir := filepath.Join(root, "blocked-parent")
	if err := os.MkdirAll(blockedDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(blockedDir, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(blockedDir, 0o700) })

	target := filepath.Join(blockedDir, "child")
	if ok, err := manager.ownedPathExists(target); ok || !errors.Is(err, state.ErrOwnership) {
		t.Fatalf("unreadable owned path ok=%v err=%v", ok, err)
	}
}

// TestRetainLeaseRejectsPathOutsideKnownRoots verifies retainLease fails
// closed with an ownership error for a path that is neither a known root
// nor within the configured worktree root, instead of silently accepting an
// unrelated path as leaseable.
func TestRetainLeaseRejectsPathOutsideKnownRoots(t *testing.T) {
	root := t.TempDir()
	cfg := config.Defaults()
	cfg.Storage.WorktreeRoot = filepath.Join(root, "worktrees")
	store, err := state.Open(filepath.Join(root, "state.db"))
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

// TestCreateSlotRootDetectsReplacedWorktreeRootDirectory proves that a
// worktree root swapped out from under an already-cached descriptor (same
// pathname, different inode) is detected and rejected rather than silently
// handed to the caller as the configured root.
func TestCreateSlotRootDetectsReplacedWorktreeRootDirectory(t *testing.T) {
	root := t.TempDir()
	cfg := config.Defaults()
	cfg.Storage.WorktreeRoot = filepath.Join(root, "worktrees")
	store, err := state.Open(filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	m := testManager(t, cfg, store)
	defer m.Close()

	if _, err := m.createSlotRoot(filepath.Join(cfg.Storage.WorktreeRoot, "slot", "root")); err != nil {
		t.Fatal(err)
	}
	// Pin the manager's cached descriptor open across the on-disk swap below
	// so createSlotRoot observes the now-stale inode it already holds.
	_, release, err := m.rootDescriptor(cfg.Storage.WorktreeRoot)
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	if err := os.RemoveAll(cfg.Storage.WorktreeRoot); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(cfg.Storage.WorktreeRoot, 0o700); err != nil {
		t.Fatal(err)
	}

	if _, err := m.createSlotRoot(filepath.Join(cfg.Storage.WorktreeRoot, "slot2", "root")); err == nil || !strings.Contains(err.Error(), "wx root path names a different directory") {
		t.Fatalf("swapped worktree root was not detected: %v", err)
	}
}

// TestCreateSlotRootFailsWhenWorktreeRootIsReadOnly verifies that
// createSlotRoot surfaces the underlying filesystem error when the
// configured worktree root itself refuses new subdirectories (for example
// because an operator changed its permissions), instead of silently
// returning a lease path that was never actually created.
func TestCreateSlotRootFailsWhenWorktreeRootIsReadOnly(t *testing.T) {
	ctx, manager, _, _, _, _ := managerCoverageFixture(t)
	_ = ctx
	root := manager.Config().Storage.WorktreeRoot
	if _, err := manager.createSlotRoot(filepath.Join(root, "bootstrap", "root")); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(root, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(root, 0o700) })
	if _, err := manager.createSlotRoot(filepath.Join(root, "blocked", "root")); err == nil {
		t.Fatal("slot root creation succeeded despite a read-only worktree root")
	}
}

// TestMaterializeWorkspaceRootFailsWhenSlotDirectoryIsUnreadable verifies
// that materializeWorkspaceRoot surfaces the underlying filesystem error
// when the destination slot directory cannot be opened (an operator
// permission change, or a hostile concurrent modification), instead of
// silently reporting a successful materialization that never happened.
func TestMaterializeWorkspaceRootFailsWhenSlotDirectoryIsUnreadable(t *testing.T) {
	ctx, manager, _, _, _, _ := managerCoverageFixture(t)
	_ = ctx
	root := manager.Config().Storage.WorktreeRoot
	slotPath := filepath.Join(root, "materialize", "unreadable", "root")
	if _, err := manager.createSlotRoot(slotPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(slotPath, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(slotPath, 0o700) })

	source := t.TempDir()
	if err := manager.materializeWorkspaceRoot(source, slotPath, config.Workspace{}); err == nil {
		t.Fatal("workspace root materialization succeeded despite an unopenable slot directory")
	}
}

// TestRootDirectoryUsageFailsWhenAnEntryIsUnreadable verifies that
// rootDirectoryUsage propagates a filesystem walk failure (an unreadable
// nested directory) as an error rather than silently reporting a partial or
// zero usage figure for a root it could not fully inspect.
func TestRootDirectoryUsageFailsWhenAnEntryIsUnreadable(t *testing.T) {
	_, manager, _, _, _, _ := managerCoverageFixture(t)
	root := manager.Config().Storage.WorktreeRoot
	blocked := filepath.Join(root, "blocked")
	if _, err := manager.createSlotRoot(blocked); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(blocked, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(blocked, 0o700) })

	if _, _, err := manager.rootDirectoryUsage(root); err == nil {
		t.Fatal("root directory usage succeeded despite an unreadable entry")
	}
}

// TestReadyMatchesReportsUnreadableSlotDirectory verifies that readyMatches
// reports a genuine filesystem error (an unreadable parent directory) as an
// ownership failure distinct from an ordinary missing slot, since the two
// require different remediation: one is routine (the slot is simply gone),
// the other means the daemon cannot currently prove anything about the path
// at all.
func TestReadyMatchesReportsUnreadableSlotDirectory(t *testing.T) {
	ctx, manager, _, workspaceRecord, resolved, _ := managerCoverageFixture(t)
	root := manager.Config().Storage.WorktreeRoot
	if _, err := manager.createSlotRoot(filepath.Join(root, "bootstrap", "root")); err != nil {
		t.Fatal(err)
	}
	slotsDir := filepath.Join(root, "workspaces", string(workspaceRecord.ID), "slots")
	if err := os.MkdirAll(slotsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(slotsDir, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(slotsDir, 0o700) })

	slot := state.Slot{ID: "blocked", WorkspaceID: string(workspaceRecord.ID), Generation: 1, Path: filepath.Join(slotsDir, "blocked", "root"), State: "READY"}
	if ok, err := manager.readyMatches(ctx, slot, resolved); ok || !errors.Is(err, state.ErrOwnership) {
		t.Fatalf("unreadable slot directory ok=%v err=%v", ok, err)
	}
}

// TestReadyMatchesReportsMissingReadySlotDirectory exercises the ENOENT
// branch reached only once the slot's root namespace itself resolves and
// opens, but the slot's own directory has since disappeared underneath a
// durable READY row.
func TestReadyMatchesReportsMissingReadySlotDirectory(t *testing.T) {
	ctx, manager, _, workspaceRecord, resolved, _ := managerCoverageFixture(t)
	bootstrap := filepath.Join(manager.Config().Storage.WorktreeRoot, "ready-missing", "bootstrap", "root")
	if _, err := manager.createSlotRoot(bootstrap); err != nil {
		t.Fatal(err)
	}
	goneID := domain.StableID("ready-missing", "gone")
	gonePath := filepath.Join(manager.Config().Storage.WorktreeRoot, "ready-missing", goneID, "root")
	slot := state.Slot{ID: goneID, WorkspaceID: string(workspaceRecord.ID), Generation: 1, Path: gonePath, State: "READY"}
	if ok, err := manager.readyMatches(ctx, slot, resolved); err != nil || ok {
		t.Fatalf("missing ready slot directory ok=%v err=%v", ok, err)
	}
}

// TestReadyRepositoriesMatchRejectsWorktreePathsOutsideRoot verifies that a
// stored repository worktree path pointing outside the owning wx root is
// reported as an ownership failure for both COLD and READY repository
// states, rather than treated as an ordinary not-ready mismatch.
func TestReadyRepositoriesMatchRejectsWorktreePathsOutsideRoot(t *testing.T) {
	ctx, manager, store, workspaceRecord, resolved, _ := managerCoverageFixture(t)
	root := manager.Config().Storage.WorktreeRoot
	if _, err := manager.createSlotRoot(filepath.Join(root, "bootstrap", "root")); err != nil {
		t.Fatal(err)
	}
	owner, release, err := manager.existingRootDescriptor(root)
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	fingerprint, err := workspace.Fingerprint(1, resolved[0].OID, resolved[0].Repository, manager.Config())
	if err != nil {
		t.Fatal(err)
	}

	coldID := domain.StableID("ready-outside", "cold")
	coldOutside := filepath.Join(t.TempDir(), "cold-outside")
	coldSlot := state.Slot{ID: coldID, WorkspaceID: string(workspaceRecord.ID), Generation: 1, Path: filepath.Join(root, "ready-outside", coldID, "root"), State: "READY"}
	if _, err := store.CreateStandby(ctx, coldSlot,
		[]state.SlotRepository{{RepositoryID: string(resolved[0].Repository.ID), WorktreePath: coldOutside, State: "COLD", BaseOID: resolved[0].OID, Fingerprint: fingerprint}}); err != nil {
		t.Fatal(err)
	}
	if ok, err := manager.readyRepositoriesMatch(ctx, coldSlot, resolved, root, owner); ok || !errors.Is(err, state.ErrOwnership) {
		t.Fatalf("cold worktree path outside root ok=%v err=%v", ok, err)
	}

	readyID := domain.StableID("ready-outside", "ready")
	readyOutside := filepath.Join(t.TempDir(), "ready-outside")
	readySlot := state.Slot{ID: readyID, WorkspaceID: string(workspaceRecord.ID), Generation: 1, Path: filepath.Join(root, "ready-outside", readyID, "root"), State: "READY"}
	if _, err := store.CreateStandby(ctx, readySlot,
		[]state.SlotRepository{{RepositoryID: string(resolved[0].Repository.ID), WorktreePath: readyOutside, State: "READY", BaseOID: resolved[0].OID, Fingerprint: fingerprint}}); err != nil {
		t.Fatal(err)
	}
	if ok, err := manager.readyRepositoriesMatch(ctx, readySlot, resolved, root, owner); ok || !errors.Is(err, state.ErrOwnership) {
		t.Fatalf("ready worktree path outside root ok=%v err=%v", ok, err)
	}
}

// TestReadyRepositoriesMatchReportsUnopenableReadyWorktree verifies that a
// READY repository worktree that Lstat can see but the daemon cannot open
// (for example because its permissions were tampered with) is reported as
// an ownership failure distinct from an ordinary not-ready mismatch.
func TestReadyRepositoriesMatchReportsUnopenableReadyWorktree(t *testing.T) {
	ctx, manager, store, workspaceRecord, resolved, _ := managerCoverageFixture(t)
	root := manager.Config().Storage.WorktreeRoot
	if _, err := manager.createSlotRoot(filepath.Join(root, "bootstrap", "root")); err != nil {
		t.Fatal(err)
	}
	owner, release, err := manager.existingRootDescriptor(root)
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	fingerprint, err := workspace.Fingerprint(1, resolved[0].OID, resolved[0].Repository, manager.Config())
	if err != nil {
		t.Fatal(err)
	}

	blockedID := domain.StableID("ready-unopenable", "worktree")
	slotRoot := filepath.Join(root, "ready-unopenable", blockedID, "root")
	worktreePath := filepath.Join(slotRoot, "repository")
	if err := os.MkdirAll(worktreePath, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(worktreePath, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(worktreePath, 0o700) })

	slot := state.Slot{ID: blockedID, WorkspaceID: string(workspaceRecord.ID), Generation: 1, Path: slotRoot, State: "READY"}
	if _, err := store.CreateStandby(ctx, slot,
		[]state.SlotRepository{{RepositoryID: string(resolved[0].Repository.ID), WorktreePath: worktreePath, State: "READY", BaseOID: resolved[0].OID, Fingerprint: fingerprint}}); err != nil {
		t.Fatal(err)
	}

	if ok, err := manager.readyRepositoriesMatch(ctx, slot, resolved, root, owner); ok || !errors.Is(err, state.ErrOwnership) {
		t.Fatalf("unopenable ready worktree ok=%v err=%v", ok, err)
	}
}

// TestScheduleColdRepositoryRemovalsQuarantinesUnverifiableWorktreePath
// verifies a candidate whose worktree path cannot be verified against a
// known wx root is quarantined and skipped rather than scheduled for
// removal.
func TestScheduleColdRepositoryRemovalsQuarantinesUnverifiableWorktreePath(t *testing.T) {
	ctx, manager, store, workspaceRecord, resolved, _ := managerCoverageFixture(t)
	slotID := domain.StableID("cold-schedule", "outside")
	outsidePath := filepath.Join(t.TempDir(), "outside-worktree")
	if _, err := store.CreateStandby(ctx,
		state.Slot{ID: slotID, WorkspaceID: string(workspaceRecord.ID), Generation: 1, Path: filepath.Join(manager.Config().Storage.WorktreeRoot, "cold-schedule", slotID, "root"), State: "RETIRING"},
		[]state.SlotRepository{{RepositoryID: string(resolved[0].Repository.ID), WorktreePath: outsidePath, State: "RETIRING", BaseOID: resolved[0].OID}}); err != nil {
		t.Fatal(err)
	}
	candidates := []state.ColdRepositoryCandidate{{SlotID: slotID, WorkspaceID: string(workspaceRecord.ID), RepositoryID: string(resolved[0].Repository.ID), WorktreePath: outsidePath}}
	if count := manager.scheduleColdRepositoryRemovals(ctx, candidates, map[string]bool{}); count != 0 {
		t.Fatalf("unverifiable cold repository was scheduled: count=%d", count)
	}
	if slot, err := store.Slot(ctx, slotID); err != nil || slot.State != "QUARANTINED" {
		t.Fatalf("unverifiable cold repository slot=%+v err=%v", slot, err)
	}
}

// TestOwnedRootArtifactPathsSkipsIncompleteWorkspaceAndUnboundEntries
// verifies that ownedRootArtifactPaths treats a workspace directory that has
// not allocated any slots yet, and an unbound entry that is not a physical
// directory, as ordinary absent artifacts rather than as errors or as
// reportable slot roots.
func TestOwnedRootArtifactPathsSkipsIncompleteWorkspaceAndUnboundEntries(t *testing.T) {
	_, manager, _, workspaceRecord, _, _ := managerCoverageFixture(t)
	root := manager.Config().Storage.WorktreeRoot
	if err := os.MkdirAll(filepath.Join(root, "workspaces", string(workspaceRecord.ID)), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "unbound"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "unbound", "not-a-slot"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	paths, err := manager.ownedRootArtifactPaths(root)
	if err != nil {
		t.Fatalf("owned root artifact paths: %v", err)
	}
	if len(paths) != 0 {
		t.Fatalf("incomplete/non-directory entries were reported as artifacts: %v", paths)
	}
}

// TestOwnedRootArtifactPathsReportsUnreadableWorkspaceSlots verifies that a
// registered workspace directory whose "slots" namespace cannot be read (as
// opposed to simply not existing yet) is reported as an ownership failure,
// since the daemon cannot prove there are no owned artifacts underneath it.
func TestOwnedRootArtifactPathsReportsUnreadableWorkspaceSlots(t *testing.T) {
	_, manager, _, workspaceRecord, _, _ := managerCoverageFixture(t)
	root := manager.Config().Storage.WorktreeRoot
	slotsDir := filepath.Join(root, "workspaces", string(workspaceRecord.ID), "slots")
	if err := os.MkdirAll(slotsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(slotsDir, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(slotsDir, 0o700) })

	if _, err := manager.ownedRootArtifactPaths(root); !errors.Is(err, state.ErrOwnership) {
		t.Fatalf("unreadable workspace slots error=%v", err)
	}
}

// TestOwnedRootArtifactPathsReportsUnreadableUnboundNamespace verifies that
// an unreadable "unbound" namespace under the wx root is reported as an
// ownership failure rather than treated the same as an absent namespace,
// mirroring the equivalent guarantee already covered for the "workspaces"
// namespace above.
func TestOwnedRootArtifactPathsReportsUnreadableUnboundNamespace(t *testing.T) {
	_, manager, _, _, _, _ := managerCoverageFixture(t)
	root := manager.Config().Storage.WorktreeRoot
	unboundDir := filepath.Join(root, "unbound")
	if err := os.MkdirAll(unboundDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(unboundDir, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(unboundDir, 0o700) })

	if _, err := manager.ownedRootArtifactPaths(root); !errors.Is(err, state.ErrOwnership) {
		t.Fatalf("unreadable unbound namespace error=%v", err)
	}
}

// TestQuarantineFailureHelpersIgnoreNonOwnershipErrors verifies the guard
// clauses in quarantineOwnershipFailure and quarantineCleanupFailure that
// leave durable state untouched for an ordinary (non-ownership) error,
// instead of quarantining a slot on every unrelated failure.
func TestQuarantineFailureHelpersIgnoreNonOwnershipErrors(t *testing.T) {
	root := t.TempDir()
	cfg := config.Defaults()
	store, err := state.Open(filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	m := testManager(t, cfg, store)
	defer m.Close()
	ctx := context.Background()

	session := state.Session{ID: "slot", SlotID: "slot", State: "ACTIVE", AgentKind: "codex", TokenHash: state.HashToken("token")}
	if _, err := store.CreateSlotSession(ctx, state.Slot{ID: "slot", Generation: 1, Path: filepath.Join(root, "slot"), State: "RETIRING"}, nil, session, ""); err != nil {
		t.Fatal(err)
	}

	m.quarantineOwnershipFailure("slot", []string{"RETIRING"}, errors.New("transient failure"))
	m.quarantineCleanupFailure("slot", errors.New("transient failure"))

	if slot, err := store.Slot(ctx, "slot"); err != nil || slot.State != "RETIRING" {
		t.Fatalf("non-ownership failure changed slot state: slot=%+v err=%v", slot, err)
	}
}
