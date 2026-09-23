package daemon

import (
	"bytes"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/HappyOnigiri/WX/internal/config"
	"github.com/HappyOnigiri/WX/internal/state"
)

func TestReconcileArtifactsSurvivesQuarantineStorageFailure(t *testing.T) {
	t.Parallel()
	ctx, manager, store, _, _, databasePath := managerCoverageFixture(t)
	missingID := "missing-artifact"
	if _, err := store.CreateSlotSession(ctx,
		slotAtPath(t, manager, "", missingID, filepath.Join(manager.Config().Storage.WorktreeRoot, "missing-artifact"), 1, "LEASED"),
		nil,
		state.Session{ID: missingID, SlotID: missingID, State: "ACTIVE", AgentKind: "codex", TokenHash: state.HashToken(missingID)}, ""); err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	manager.log = slog.New(slog.NewTextHandler(&logs, nil))

	raw := openTestDatabase(t, databasePath)
	if _, err := raw.ExecContext(ctx, `CREATE TRIGGER fail_quarantine_update BEFORE UPDATE ON slots WHEN NEW.state='QUARANTINED' BEGIN SELECT RAISE(ABORT,'injected quarantine failure'); END`); err != nil {
		t.Fatal(err)
	}

	manager.reconcileArtifacts(ctx)
	if slot, err := store.Slot(ctx, missingID); err != nil || slot.State != "LEASED" {
		t.Fatalf("slot state changed despite injected quarantine failure: slot=%+v err=%v", slot, err)
	}
	if !strings.Contains(logs.String(), "quarantine missing owned path failed") {
		t.Fatalf("missing-path quarantine failure was not logged: %s", logs.String())
	}
}

func TestReconcileArtifactsDoesNotLogSuccessfulDiagnosticWritesAsFailures(t *testing.T) {
	t.Parallel()
	ctx, manager, _, _, _, _ := managerCoverageFixture(t)
	orphanPath := filepath.Join(manager.Config().Storage.WorktreeRoot, "wsp999", "orphan")
	if err := os.MkdirAll(orphanPath, 0o700); err != nil {
		t.Fatal(err)
	}
	before := manager.artifactDiagnostics(ctx)
	if !containsString(before["unknown_paths"].([]string), orphanPath) {
		t.Fatalf("orphan path was not discovered before reconciliation: %v", before)
	}
	var logs bytes.Buffer
	manager.log = slog.New(slog.NewTextHandler(&logs, nil))

	manager.reconcileArtifacts(ctx)

	for _, message := range []string{"record quarantined artifact failed", "prune resolved quarantine records failed"} {
		if strings.Contains(logs.String(), message) {
			t.Fatalf("successful diagnostic persistence logged %q: %s", message, logs.String())
		}
	}
}

func TestReconcileArtifactsSchedulesRecoveredJobs(t *testing.T) {
	t.Parallel()
	ctx, manager, store, workspaceRecord, _, _ := managerCoverageFixture(t)
	sessionID := "recover-preparing-slot"
	session := state.Session{
		ID: sessionID, WorkspaceID: string(workspaceRecord.ID), SlotID: sessionID,
		State: "STARTING", AgentKind: "codex", TokenHash: state.HashToken(sessionID),
	}
	if _, err := store.CreateSlotSession(ctx, testSlotRow(t, manager, string(workspaceRecord.ID), sessionID, 1, "PREPARING"), nil, session, ""); err != nil {
		t.Fatal(err)
	}

	manager.reconcileArtifacts(ctx)

	var queued *queuedJob
	manager.jobQueue.mu.Lock()
	for class := jobClassInteractive; class < jobClassCount && queued == nil; class++ {
		for _, candidate := range manager.jobQueue.pending[class] {
			if candidate.slotID == sessionID {
				copy := candidate
				queued = &copy
				break
			}
		}
	}
	manager.jobQueue.mu.Unlock()
	if queued == nil {
		t.Fatal("reconcile did not queue the recovered prepare job")
	}
	job, err := store.JobByID(ctx, queued.id)
	if err != nil || job.Kind != "PREPARE" || job.State != "PENDING" || job.SlotID != sessionID {
		t.Fatalf("recovered job=%+v err=%v, want pending PREPARE for slot %s", job, err, sessionID)
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

func TestOwnedPathExistsPropagatesManagerClosedError(t *testing.T) {
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
	t.Parallel()
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

// ownedRootArtifactPaths の対象は reconcile の隔離記録の範囲そのものなので、`_recovery` 配下を含めない。
// 削除できる実体の広い列挙は scanUnmanagedArtifacts が持ち、そちらのテストが `_recovery` 配下を対象として固定する。
func TestOwnedRootArtifactPathsSkipsIncompleteWorkspaceAndUnboundEntries(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
