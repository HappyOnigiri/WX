package daemon

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/HappyOnigiri/WX/internal/config"
	"github.com/HappyOnigiri/WX/internal/domain"
	"github.com/HappyOnigiri/WX/internal/state"
)

func TestSlotRelPathGeneratesTheDocumentedLayout(t *testing.T) {
	t.Parallel()
	relPath, err := slotRelPath("wsp001", "slt001")
	if err != nil {
		t.Fatal(err)
	}
	if relPath != filepath.Join("wsp001", "slt001") {
		t.Fatalf("bound slot rel path=%q", relPath)
	}
	if _, err := slotRelPath("", "slt003"); !errors.Is(err, state.ErrOwnership) {
		t.Fatalf("empty workspace id error=%v", err)
	}
	for _, slotID := range []string{"", ".", "..", "a/b", `a\b`, "_reserved"} {
		if _, err := slotRelPath("wsp001", slotID); !errors.Is(err, state.ErrOwnership) {
			t.Errorf("slot id %q error=%v", slotID, err)
		}
	}
	for _, workspaceID := range []string{"", ".", "..", "a/b", "_unbound", "_recovery"} {
		if _, err := slotRelPath(workspaceID, "slt001"); !errors.Is(err, state.ErrOwnership) {
			t.Errorf("workspace id %q error=%v", workspaceID, err)
		}
	}
}

func TestValidateLayoutComponentReservesTheUnderscorePrefix(t *testing.T) {
	t.Parallel()
	if err := validateLayoutComponent("repository directory", "WX"); err != nil {
		t.Fatalf("plain name error=%v", err)
	}
	for _, value := range []string{"_unbound", "_recovery", "_anything"} {
		if err := validateLayoutComponent("workspace id", value); !errors.Is(err, state.ErrOwnership) {
			t.Errorf("reserved prefix %q error=%v", value, err)
		}
	}
}

func TestLeasePathDependsOnWorkspaceKind(t *testing.T) {
	t.Parallel()
	slotPath := filepath.Join(string(filepath.Separator)+"wx", "wsp001", "slt001")
	single := []state.SlotRepository{{RepositoryID: "r1", DirName: "WX"}}
	if got := leasePath(slotPath, "repository", single); got != filepath.Join(slotPath, "WX") {
		t.Fatalf("single-repository lease path=%q", got)
	}
	multi := []state.SlotRepository{{RepositoryID: "r1", DirName: "server"}, {RepositoryID: "r2", DirName: "client"}}
	if got := leasePath(slotPath, "multi_repository", multi); got != slotPath {
		t.Fatalf("multi-repository lease path=%q", got)
	}
	if got := leasePath(slotPath, "", nil); got != slotPath {
		t.Fatalf("unbound lease path=%q", got)
	}
	if got := leasePath(slotPath, "repository", []state.SlotRepository{{RepositoryID: "r1"}}); got != slotPath {
		t.Fatalf("nameless repository lease path=%q", got)
	}
}

func TestNewSlotIDProducesShortIdentifiers(t *testing.T) {
	t.Parallel()
	id, err := newSlotID()
	if err != nil {
		t.Fatal(err)
	}
	if !domain.ValidShortID(id) {
		t.Fatalf("slot id=%q is not a short identifier", id)
	}
	if err := validateLayoutComponent("slot id", id); err != nil {
		t.Fatalf("slot id=%q is not a usable layout component: %v", id, err)
	}
}

func TestCreateSlotRootDetectsReplacedWorktreeRootDirectory(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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

func TestAllocateFailsWhenWorktreeRootCannotBeCreated(t *testing.T) {
	t.Parallel()
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

func TestAllocateReleasesLeaseWhenSessionPersistenceFails(t *testing.T) {
	t.Parallel()
	ctx, manager, store, workspaceRecord, resolved, databasePath := managerCoverageFixture(t)
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
	artifacts, err := store.SlotArtifacts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(artifacts) != 0 {
		t.Fatalf("slot reservation failure left registered artifacts=%+v", artifacts)
	}
}

func TestAllocateRegistrationFailureQuarantinesCreatedSlot(t *testing.T) {
	t.Parallel()
	ctx, manager, store, workspaceRecord, resolved, databasePath := managerCoverageFixture(t)
	raw := openManagerCoverageDB(t, databasePath)
	if _, err := raw.ExecContext(ctx, `CREATE TRIGGER fail_allocate_session BEFORE INSERT ON sessions BEGIN SELECT RAISE(ABORT,'injected session registration failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.allocate(ctx, workspaceRecord, resolved, 1, "codex", os.Getpid(), "STARTING", ""); err == nil {
		t.Fatal("allocate succeeded despite an injected session registration failure")
	}
	manager.mu.RLock()
	leaseCount := len(manager.leases)
	manager.mu.RUnlock()
	if leaseCount != 0 {
		t.Fatalf("failed registration left %d dangling lease(s)", leaseCount)
	}
	artifacts, err := store.SlotArtifacts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(artifacts) != 1 || artifacts[0].State != "QUARANTINED" {
		t.Fatalf("failed registration artifacts=%+v, want one QUARANTINED slot", artifacts)
	}
	if info, err := os.Stat(artifacts[0].Path); err != nil || !info.IsDir() {
		t.Fatalf("quarantined slot root was not preserved: info=%v err=%v", info, err)
	}
	slot, err := store.Slot(ctx, artifacts[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if slot.DirIdentity == "" {
		t.Fatal("quarantined slot lost the created directory identity")
	}
}

func TestAllocationDoesNotAdoptExistingUnregisteredDirectory(t *testing.T) {
	t.Parallel()
	ctx, manager, store, w, resolved, _ := managerCoverageFixture(t, "repository")
	root, rootID, err := manager.activeRoot()
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, string(w.ID), "taken")
	manager.beforeSlotRootCreate = func() {
		if err := os.MkdirAll(target, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(target, "keep"), []byte("keep"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := manager.allocateWithID(ctx, "taken", root, rootID, "token", w, resolved, 1, "codex", 0, "STARTING", "PREPARING", "PREPARE", ""); !errors.Is(err, errSlotPathExists) {
		t.Fatalf("collision error=%v", err)
	}
	if _, err := store.Slot(ctx, "taken"); err == nil {
		t.Fatal("unregistered directory became a managed slot")
	}
	if data, err := os.ReadFile(filepath.Join(target, "keep")); err != nil || string(data) != "keep" {
		t.Fatalf("existing directory changed: %q %v", data, err)
	}
}
