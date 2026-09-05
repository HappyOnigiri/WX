package daemon

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/HappyOnigiri/WX/internal/state"
)

func TestReleaseIsIdempotentAfterAlreadyReleasingSession(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	store, err := state.Open(filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	if _, err := store.CreateSlotSession(ctx, storeSlotAt(t, store, root, "", "dup", filepath.Join(root, "root"), 0, "LEASED"), nil, state.Session{ID: "dup", SlotID: "dup", State: "ACTIVE", AgentKind: "codex", TokenHash: state.HashToken("token")}, ""); err != nil {
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

func TestStandbyRegistrationFailureQuarantinesCreatedSlot(t *testing.T) {
	t.Parallel()
	ctx, manager, store, workspaceRecord, resolved, databasePath := managerCoverageFixture(t)
	manager.mu.Lock()
	cfg := manager.cfg
	cfg.Pool.WarmPerWorkspace = 1
	manager.cfg = cfg
	manager.mu.Unlock()
	raw := openManagerCoverageDB(t, databasePath)
	if _, err := raw.ExecContext(ctx, `CREATE TRIGGER fail_standby_job BEFORE INSERT ON jobs WHEN NEW.kind='PREPARE' BEGIN SELECT RAISE(ABORT,'injected standby registration failure'); END`); err != nil {
		t.Fatal(err)
	}
	rootPath, rootID, err := manager.activeRoot()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.createStandbySlot(ctx, rootPath, rootID, workspaceRecord, resolved, 1, nil); err == nil {
		t.Fatal("standby allocation succeeded despite an injected job registration failure")
	}
	artifacts, err := store.SlotArtifacts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(artifacts) != 1 || artifacts[0].State != "QUARANTINED" {
		t.Fatalf("failed standby registration artifacts=%+v, want one QUARANTINED slot", artifacts)
	}
	if info, err := os.Stat(artifacts[0].Path); err != nil || !info.IsDir() {
		t.Fatalf("quarantined standby root was not preserved: info=%v err=%v", info, err)
	}
}

func TestReconcileArtifactsSkipsUnverifiableAndArchivedPaths(t *testing.T) {
	t.Parallel()
	ctx, manager, store, _, _, _ := managerCoverageFixture(t)
	outsideSlot := testSlotRow(t, manager, "", "outside", 1, "LEASED")
	outsideSlot.RelPath = filepath.Join("..", "outside-slot")
	outsideSlot.Path = filepath.Join(manager.Config().Storage.WorktreeRoot, outsideSlot.RelPath)
	outsideSession := state.Session{ID: "outside", SlotID: "outside", State: "ACTIVE", AgentKind: "codex", TokenHash: state.HashToken("outside")}
	if _, err := store.CreateSlotSession(ctx, outsideSlot, nil, outsideSession, ""); err != nil {
		t.Fatal(err)
	}
	archivedSession := state.Session{ID: "archived", SlotID: "archived", State: "ARCHIVED", AgentKind: "codex", TokenHash: state.HashToken("archived")}
	if _, err := store.CreateSlotSession(ctx, testSlotRow(t, manager, "", "archived", 1, "ARCHIVED"), nil, archivedSession, ""); err != nil {
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
