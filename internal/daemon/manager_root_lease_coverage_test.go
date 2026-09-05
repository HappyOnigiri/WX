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

func TestRootHandleForRootReportsNoDescriptorForUnknownRoot(t *testing.T) {
	m := &Manager{}
	if got := m.rootHandleForRoot(filepath.Join(t.TempDir(), "unknown")); got != nil {
		t.Fatalf("unknown root unexpectedly returned a handle: %v", got)
	}
}

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

func TestOwnedPathExistsReportsUnreadablePath(t *testing.T) {
	_, manager, _, _, _, _ := managerCoverageFixture(t)
	root := manager.Config().Storage.WorktreeRoot
	if _, _, err := manager.createSlotRoot(filepath.Join(root, "bootstrap", "root"), filepath.Join(root, "bootstrap", "root")); err != nil {
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

	if _, _, err := m.createSlotRoot(filepath.Join(cfg.Storage.WorktreeRoot, "slot", "root"), filepath.Join(cfg.Storage.WorktreeRoot, "slot", "root")); err != nil {
		t.Fatal(err)
	}
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

	if _, _, err := m.createSlotRoot(filepath.Join(cfg.Storage.WorktreeRoot, "slot2", "root"), filepath.Join(cfg.Storage.WorktreeRoot, "slot2", "root")); err == nil || !strings.Contains(err.Error(), "wx root path names a different directory") {
		t.Fatalf("swapped worktree root was not detected: %v", err)
	}
}

func TestCreateSlotRootFailsWhenWorktreeRootIsReadOnly(t *testing.T) {
	ctx, manager, _, _, _, _ := managerCoverageFixture(t)
	_ = ctx
	root := manager.Config().Storage.WorktreeRoot
	if _, _, err := manager.createSlotRoot(filepath.Join(root, "bootstrap", "root"), filepath.Join(root, "bootstrap", "root")); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(root, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(root, 0o700) })
	if _, _, err := manager.createSlotRoot(filepath.Join(root, "blocked", "root"), filepath.Join(root, "blocked", "root")); err == nil {
		t.Fatal("slot root creation succeeded despite a read-only worktree root")
	}
}

func TestMaterializeWorkspaceRootFailsWhenSlotDirectoryIsUnreadable(t *testing.T) {
	ctx, manager, _, _, _, _ := managerCoverageFixture(t)
	_ = ctx
	root := manager.Config().Storage.WorktreeRoot
	slotPath := filepath.Join(root, "materialize", "unreadable", "root")
	if _, _, err := manager.createSlotRoot(slotPath, slotPath); err != nil {
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

func TestRootDirectoryUsageFailsWhenAnEntryIsUnreadable(t *testing.T) {
	_, manager, _, _, _, _ := managerCoverageFixture(t)
	root := manager.Config().Storage.WorktreeRoot
	blocked := filepath.Join(root, "blocked")
	if _, _, err := manager.createSlotRoot(blocked, blocked); err != nil {
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

func TestReadyMatchesReportsUnreadableSlotDirectory(t *testing.T) {
	ctx, manager, _, workspaceRecord, resolved, _ := managerCoverageFixture(t)
	root := manager.Config().Storage.WorktreeRoot
	if _, _, err := manager.createSlotRoot(filepath.Join(root, "bootstrap", "root"), filepath.Join(root, "bootstrap", "root")); err != nil {
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

	slot := slotAtPath(t, manager, string(workspaceRecord.ID), "blocked", filepath.Join(slotsDir, "blocked", "root"), 1, "READY")
	if ok, err := manager.readyMatches(ctx, slot, resolved); ok || !errors.Is(err, state.ErrOwnership) {
		t.Fatalf("unreadable slot directory ok=%v err=%v", ok, err)
	}
}

func TestReadyMatchesReportsMissingReadySlotDirectory(t *testing.T) {
	ctx, manager, _, workspaceRecord, resolved, _ := managerCoverageFixture(t)
	bootstrap := filepath.Join(manager.Config().Storage.WorktreeRoot, "ready-missing", "bootstrap", "root")
	if _, _, err := manager.createSlotRoot(bootstrap, bootstrap); err != nil {
		t.Fatal(err)
	}
	goneID := domain.StableID("ready-missing", "gone")
	gonePath := filepath.Join(manager.Config().Storage.WorktreeRoot, "ready-missing", goneID, "root")
	slot := slotAtPath(t, manager, string(workspaceRecord.ID), goneID, gonePath, 1, "READY")
	if ok, err := manager.readyMatches(ctx, slot, resolved); err != nil || ok {
		t.Fatalf("missing ready slot directory ok=%v err=%v", ok, err)
	}
}

func TestReadyRepositoriesMatchRejectsWorktreePathsOutsideRoot(t *testing.T) {
	ctx, manager, store, workspaceRecord, resolved, _ := managerCoverageFixture(t)
	root := manager.Config().Storage.WorktreeRoot
	if _, _, err := manager.createSlotRoot(filepath.Join(root, "bootstrap", "root"), filepath.Join(root, "bootstrap", "root")); err != nil {
		t.Fatal(err)
	}
	owner, release, err := manager.existingRootDescriptor(root)
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	coldID := domain.StableID("ready-outside", "cold")
	coldSlot := slotAtPath(t, manager, string(workspaceRecord.ID), coldID, filepath.Join(root, "ready-outside", coldID, "root"), 1, "READY")
	coldDirName := escapingDirNameFor(t, root, coldSlot.Path)
	coldFingerprint, err := workspace.Fingerprint(1, resolved[0].OID, resolved[0].Repository, manager.Config())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateStandby(ctx, coldSlot,
		[]state.SlotRepository{{RepositoryID: string(resolved[0].Repository.ID), DirName: coldDirName, State: "COLD", BaseOID: resolved[0].OID, Fingerprint: coldFingerprint}}); err != nil {
		t.Fatal(err)
	}
	if ok, err := manager.readyRepositoriesMatch(ctx, coldSlot, resolved, root, owner); ok || !errors.Is(err, state.ErrOwnership) {
		t.Fatalf("cold worktree path outside root ok=%v err=%v", ok, err)
	}

	readyID := domain.StableID("ready-outside", "ready")
	readySlot := slotAtPath(t, manager, string(workspaceRecord.ID), readyID, filepath.Join(root, "ready-outside", readyID, "root"), 1, "READY")
	readyDirName := escapingDirNameFor(t, root, readySlot.Path)
	readyFingerprint, err := workspace.Fingerprint(1, resolved[0].OID, resolved[0].Repository, manager.Config())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateStandby(ctx, readySlot,
		[]state.SlotRepository{{RepositoryID: string(resolved[0].Repository.ID), DirName: readyDirName, State: "READY", BaseOID: resolved[0].OID, Fingerprint: readyFingerprint}}); err != nil {
		t.Fatal(err)
	}
	if ok, err := manager.readyRepositoriesMatch(ctx, readySlot, resolved, root, owner); ok || !errors.Is(err, state.ErrOwnership) {
		t.Fatalf("ready worktree path outside root ok=%v err=%v", ok, err)
	}
}

func TestReadyRepositoriesMatchReportsUnopenableReadyWorktree(t *testing.T) {
	ctx, manager, store, workspaceRecord, resolved, _ := managerCoverageFixture(t)
	root := manager.Config().Storage.WorktreeRoot
	if _, _, err := manager.createSlotRoot(filepath.Join(root, "bootstrap", "root"), filepath.Join(root, "bootstrap", "root")); err != nil {
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

	slot := slotAtPath(t, manager, string(workspaceRecord.ID), blockedID, slotRoot, 1, "READY")
	if _, err := store.CreateStandby(ctx, slot,
		[]state.SlotRepository{{RepositoryID: string(resolved[0].Repository.ID), DirName: filepath.Base(worktreePath), State: "READY", BaseOID: resolved[0].OID, Fingerprint: fingerprint}}); err != nil {
		t.Fatal(err)
	}

	if ok, err := manager.readyRepositoriesMatch(ctx, slot, resolved, root, owner); ok || !errors.Is(err, state.ErrOwnership) {
		t.Fatalf("unopenable ready worktree ok=%v err=%v", ok, err)
	}
}

func TestScheduleColdRepositoryRemovalsQuarantinesUnverifiableWorktreePath(t *testing.T) {
	ctx, manager, store, workspaceRecord, resolved, _ := managerCoverageFixture(t)
	slotID := domain.StableID("cold-schedule", "outside")
	outsidePath := filepath.Join(t.TempDir(), "outside-worktree")
	if _, err := store.CreateStandby(ctx,
		slotAtPath(t, manager, string(workspaceRecord.ID), slotID, filepath.Join(manager.Config().Storage.WorktreeRoot, "cold-schedule", slotID, "root"), 1, "RETIRING"),
		[]state.SlotRepository{{RepositoryID: string(resolved[0].Repository.ID), DirName: "repository", State: "RETIRING", BaseOID: resolved[0].OID}}); err != nil {
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

func TestOwnedRootArtifactPathsSkipsIncompleteWorkspaceAndUnboundEntries(t *testing.T) {
	_, manager, _, workspaceRecord, _, _ := managerCoverageFixture(t)
	root := manager.Config().Storage.WorktreeRoot
	if err := os.MkdirAll(filepath.Join(root, string(workspaceRecord.ID)), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, unboundNamespace), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, unboundNamespace, "not-a-slot"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "_recovery", "workspace-snapshots"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "_recovery", "workspace-snapshots", "bundle.tar"), []byte("x"), 0o600); err != nil {
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

func TestOwnedRootArtifactPathsReportsUnreadableWorkspaceSlots(t *testing.T) {
	_, manager, _, workspaceRecord, _, _ := managerCoverageFixture(t)
	root := manager.Config().Storage.WorktreeRoot
	slotsDir := filepath.Join(root, string(workspaceRecord.ID))
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

func TestOwnedRootArtifactPathsReportsUnreadableUnboundNamespace(t *testing.T) {
	_, manager, _, _, _, _ := managerCoverageFixture(t)
	root := manager.Config().Storage.WorktreeRoot
	unboundDir := filepath.Join(root, unboundNamespace)
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
	if _, err := store.CreateSlotSession(ctx, testSlotRow(t, m, "", "slot", 1, "RETIRING"), nil, session, ""); err != nil {
		t.Fatal(err)
	}

	m.quarantineOwnershipFailure("slot", []string{"RETIRING"}, errors.New("transient failure"))
	m.quarantineCleanupFailure("slot", errors.New("transient failure"))

	if slot, err := store.Slot(ctx, "slot"); err != nil || slot.State != "RETIRING" {
		t.Fatalf("non-ownership failure changed slot state: slot=%+v err=%v", slot, err)
	}
}
