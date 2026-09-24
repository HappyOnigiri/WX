package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/HappyOnigiri/WorktreeX/internal/archive"
	"github.com/HappyOnigiri/WorktreeX/internal/domain"
	"github.com/HappyOnigiri/WorktreeX/internal/state"
)

func mutationLog(t *testing.T, m *Manager) *diagnosticLog {
	t.Helper()
	logs := newDiagnosticLog(managerFixtureLogLimit)
	m.log = slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	return logs
}

func mutationCleanRun(t *testing.T, store *state.Store, workspaceID, slotID, path, mode string, replenish bool, sessionID ...string) string {
	t.Helper()
	runID := "mutation-" + strings.ReplaceAll(slotID, "_", "-")
	owner := ""
	if len(sessionID) != 0 {
		owner = sessionID[0]
	}
	_, _, err := store.BeginCleanRun(context.Background(), state.CleanRun{ID: runID, Mode: mode, Replenish: replenish}, []state.CleanTarget{{
		SlotID: slotID, WorkspaceID: workspaceID, SessionID: owner, Path: path, State: cleanTargetPending,
	}}, []string{workspaceID})
	if err != nil {
		t.Fatal(err)
	}
	return runID
}

func TestMutationCleanReplenishReportsFinishFailure(t *testing.T) {
	t.Parallel()
	manager, store, workspaceID := cleanFixture(t)
	slot := testSlotRow(t, manager, workspaceID, "replenish-finish", 1, "READY")
	if _, err := store.CreateStandby(context.Background(), slot, nil); err != nil {
		t.Fatal(err)
	}
	runID := mutationCleanRun(t, store, workspaceID, slot.ID, slot.Path, "all", true)
	logs := mutationLog(t, manager)
	db := openTestDatabase(t, filepath.Join(filepath.Dir(manager.Config().Storage.WorktreeRoot), "state.db"))
	if _, err := db.Exec("UPDATE clean_runs SET state='DONE' WHERE id=?", runID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("CREATE TRIGGER mutation_finish_replenish BEFORE UPDATE OF replenish_state ON clean_runs WHEN NEW.replenish_state='DONE' BEGIN SELECT RAISE(ABORT, 'finish injected failure'); END"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = db.Exec("DROP TRIGGER IF EXISTS mutation_finish_replenish") })
	manager.runCleanReplenish(context.Background(), runID, false)
	if !strings.Contains(logs.tail(), "finish clean replenishment failed") {
		t.Fatalf("finish failure was not logged: %s", logs.tail())
	}
}

func TestMutationCleanPendingTerminationDoesNotLogOnSuccess(t *testing.T) {
	t.Parallel()
	manager, store, workspaceID := cleanFixture(t)
	ctx := context.Background()
	slot := testSlot(t, manager, workspaceID, "termination-success", 1, "LEASED")
	session := state.Session{ID: "termination-session", WorkspaceID: workspaceID, SlotID: slot.ID, State: "ACTIVE", AgentKind: "codex", TokenHash: state.HashToken("token")}
	if _, err := store.CreateSlotSession(ctx, slot, nil, session, ""); err != nil {
		t.Fatal(err)
	}
	runID := mutationCleanRun(t, store, workspaceID, slot.ID, slot.Path, "all", false, session.ID)
	logs := mutationLog(t, manager)
	targets, err := store.CleanTargets(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	manager.advancePending(ctx, state.CleanRun{ID: runID, Mode: "all"}, targets[0], map[string]time.Time{})
	if strings.Contains(logs.tail(), "request session termination deferred") {
		t.Fatalf("successful termination request was logged as deferred: %s", logs.tail())
	}
	request, found, err := store.PendingTermination(ctx, session.ID)
	if err != nil || !found || request.State != "PENDING" {
		t.Fatalf("termination request=%+v found=%v err=%v", request, found, err)
	}
}

func TestMutationCleanSchedulesReadyRemovalOnlyAfterCAS(t *testing.T) {
	t.Parallel()
	manager, store, workspaceID := cleanFixture(t)
	ctx := context.Background()
	slot := testSlot(t, manager, workspaceID, "ready-removal", 1, "READY")
	job, err := store.CreateStandby(ctx, slot, nil)
	if err != nil {
		t.Fatal(err)
	}
	db := openTestDatabase(t, filepath.Join(filepath.Dir(manager.Config().Storage.WorktreeRoot), "state.db"))
	if _, err := db.Exec("UPDATE jobs SET state='SUCCEEDED',finished_at=? WHERE id=?", state.FormatTime(time.Now()), job.ID); err != nil {
		t.Fatal(err)
	}
	runID := mutationCleanRun(t, store, workspaceID, slot.ID, slot.Path, "all", false)
	targets, err := store.CleanTargets(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	manager.advancePending(ctx, state.CleanRun{ID: runID, Mode: "all"}, targets[0], map[string]time.Time{})
	got, err := store.Slot(ctx, slot.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != "REMOVING" {
		t.Fatalf("slot state=%s, want REMOVING", got.State)
	}
	gotTargets, err := store.CleanTargets(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if gotTargets[0].State != cleanTargetRemoving {
		t.Fatalf("clean target state=%s, want %s", gotTargets[0].State, cleanTargetRemoving)
	}
}

func TestMutationCleanQuarantineScheduleReportsDatabaseFailure(t *testing.T) {
	t.Parallel()
	manager, store, workspaceID := cleanFixture(t)
	ctx := context.Background()
	slot := testSlotRow(t, manager, workspaceID, "quarantine-schedule", 1, "QUARANTINED")
	if _, err := store.CreateStandby(ctx, slot, nil); err != nil {
		t.Fatal(err)
	}
	runID := mutationCleanRun(t, store, workspaceID, slot.ID, slot.Path, "all", false)
	db := openTestDatabase(t, filepath.Join(filepath.Dir(manager.Config().Storage.WorktreeRoot), "state.db"))
	if _, err := db.Exec("CREATE TRIGGER mutation_quarantine_schedule BEFORE UPDATE OF state ON slots WHEN NEW.state='REMOVING' BEGIN SELECT RAISE(ABORT, 'schedule injected failure'); END"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = db.Exec("DROP TRIGGER IF EXISTS mutation_quarantine_schedule") })
	logs := mutationLog(t, manager)
	targets, err := store.CleanTargets(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	manager.advancePending(ctx, state.CleanRun{ID: runID, Mode: "all"}, targets[0], map[string]time.Time{})
	if !strings.Contains(logs.tail(), "clean quarantined-slot removal scheduling failed") {
		t.Fatalf("quarantine scheduling failure was not logged: %s", logs.tail())
	}
}

func TestMutationCleanScopePreservesCanonicalizationError(t *testing.T) {
	t.Parallel()
	manager, _, _ := cleanFixture(t)
	missing := filepath.Join(t.TempDir(), "does-not-exist")
	_, err := manager.cleanScope(context.Background(), missing)
	if err == nil || strings.Contains(err.Error(), "is not a registered workspace") {
		t.Fatalf("cleanScope error=%v, want canonicalization failure", err)
	}
}

func TestMutationCleanCloseTerminationReportsFinishFailure(t *testing.T) {
	t.Parallel()
	manager, store, workspaceID := cleanFixture(t)
	ctx := context.Background()
	slot := testSlot(t, manager, workspaceID, "termination-close", 1, "LEASED")
	session := state.Session{ID: "termination-close-session", WorkspaceID: workspaceID, SlotID: slot.ID, State: "ACTIVE", AgentKind: "codex", TokenHash: state.HashToken("token")}
	if _, err := store.CreateSlotSession(ctx, slot, nil, session, ""); err != nil {
		t.Fatal(err)
	}
	runID := mutationCleanRun(t, store, workspaceID, slot.ID, slot.Path, "all", false)
	if err := store.RequestSessionTermination(ctx, runID, slot.ID, session.ID, "termination-close-request", time.Now().Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	db := openTestDatabase(t, filepath.Join(filepath.Dir(manager.Config().Storage.WorktreeRoot), "state.db"))
	if _, err := db.Exec("CREATE TRIGGER mutation_finish_termination BEFORE UPDATE OF state ON session_termination_requests WHEN NEW.state='CONFIRMED' BEGIN SELECT RAISE(ABORT, 'termination injected failure'); END"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = db.Exec("DROP TRIGGER IF EXISTS mutation_finish_termination") })
	logs := mutationLog(t, manager)
	manager.closePendingTermination(ctx, session.ID, "CONFIRMED")
	if !strings.Contains(logs.tail(), "close termination request failed") {
		t.Fatalf("termination close failure was not logged: %s", logs.tail())
	}
}

func TestMutationCleanDriveReportsProgressErrorWhileContextIsActive(t *testing.T) {
	t.Parallel()
	f := manualManagerFixture(t)
	logs := mutationLog(t, f.Manager)
	done := make(chan struct{})
	go func() {
		f.Manager.driveClean("missing-clean-run")
		close(done)
	}()
	waitUntil(t, 2*time.Second, func() bool { return strings.Contains(logs.tail(), "clean progress failed") })
	f.Manager.cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("driveClean did not stop after context cancellation")
	}
}

func TestMutationCleanTargetTransitionReportsDatabaseFailure(t *testing.T) {
	t.Parallel()
	manager, store, workspaceID := cleanFixture(t)
	ctx := context.Background()
	slot := testSlotRow(t, manager, workspaceID, "target-transition", 1, "READY")
	if _, err := store.CreateStandby(ctx, slot, nil); err != nil {
		t.Fatal(err)
	}
	runID := mutationCleanRun(t, store, workspaceID, slot.ID, slot.Path, "all", false)
	db := openTestDatabase(t, filepath.Join(filepath.Dir(manager.Config().Storage.WorktreeRoot), "state.db"))
	if _, err := db.Exec("CREATE TRIGGER mutation_clean_target_transition BEFORE UPDATE OF state ON clean_targets BEGIN SELECT RAISE(ABORT, 'target transition injected failure'); END"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = db.Exec("DROP TRIGGER IF EXISTS mutation_clean_target_transition") })
	logs := mutationLog(t, manager)
	manager.moveCleanTarget(ctx, runID, state.CleanTarget{SlotID: slot.ID}, cleanTargetDone, "done")
	if !strings.Contains(logs.tail(), "clean target transition skipped") {
		t.Fatalf("target transition failure was not logged: %s", logs.tail())
	}
}

func TestMutationResumeReplenishReportsDeleteFailure(t *testing.T) {
	t.Parallel()
	manager, store, workspaceID := cleanFixture(t)
	ctx := context.Background()
	if err := store.SuspendReplenish(ctx, workspaceID, state.SuspendReplenishReasonClean, "mutation"); err != nil {
		t.Fatal(err)
	}
	db := openTestDatabase(t, filepath.Join(filepath.Dir(manager.Config().Storage.WorktreeRoot), "state.db"))
	if _, err := db.Exec("CREATE TRIGGER mutation_resume_replenish BEFORE DELETE ON replenish_suspensions BEGIN SELECT RAISE(ABORT, 'resume injected failure'); END"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = db.Exec("DROP TRIGGER IF EXISTS mutation_resume_replenish") })
	logs := mutationLog(t, manager)
	manager.resumeReplenish(ctx, workspaceID)
	if !strings.Contains(logs.tail(), "resume standby replenishment failed") {
		t.Fatalf("resume failure was not logged: %s", logs.tail())
	}
}

func TestMutationCleanReplyOmitsEmptyReplenishResult(t *testing.T) {
	t.Parallel()
	reply := cleanReply(state.CleanRun{ID: "clean-reply", Mode: "all", State: state.CleanRunDone, Replenish: true}, nil, false)
	if _, found := reply["replenish"]; found {
		t.Fatalf("empty replenish result unexpectedly present: %#v", reply)
	}
}

func TestMutationResumeCleanRunsReportsRunQueryFailure(t *testing.T) {
	t.Parallel()
	f := manualManagerFixture(t)
	logs := mutationLog(t, f.Manager)
	if err := f.Store.Close(); err != nil {
		t.Fatal(err)
	}
	f.Manager.resumeCleanRuns(context.Background())
	if !strings.Contains(logs.tail(), "resume clean runs failed") {
		t.Fatalf("resume failure was not logged: %s", logs.tail())
	}
}

func TestMutationResumeCleanRunsProcessesPendingReplenishment(t *testing.T) {
	t.Parallel()
	manager, store, workspaceID := cleanFixture(t)
	ctx := context.Background()
	if _, _, err := store.BeginCleanRun(ctx, state.CleanRun{ID: "pending-replenish", Mode: "all", Replenish: true}, nil, []string{workspaceID}); err != nil {
		t.Fatal(err)
	}
	db := openTestDatabase(t, filepath.Join(filepath.Dir(manager.Config().Storage.WorktreeRoot), "state.db"))
	if _, err := db.Exec("UPDATE clean_runs SET state='DONE' WHERE id='pending-replenish'"); err != nil {
		t.Fatal(err)
	}
	manager.resumeCleanRuns(ctx)
	run, _, err := store.CleanRunByID(ctx, "pending-replenish")
	if err != nil {
		t.Fatal(err)
	}
	if run.ReplenishState != state.CleanReplenishDone {
		t.Fatalf("replenish state=%s, want %s", run.ReplenishState, state.CleanReplenishDone)
	}
}

func TestMutationGCProgressRetainsCallerReason(t *testing.T) {
	t.Parallel()
	progress := newGCProgress()
	progress.addIssue("slot", "failed", "registered path could not be removed", errors.New("permission denied"))
	if len(progress.Reasons) != 1 {
		t.Fatalf("reasons=%v", progress.Reasons)
	}
	want := "registered path could not be removed: permission denied"
	if progress.Reasons[0].Reason != want {
		t.Fatalf("reason=%q, want %q", progress.Reasons[0].Reason, want)
	}
}

func TestMutationBackgroundGCDoesNotReportEmptyResult(t *testing.T) {
	t.Parallel()
	f := manualManagerFixture(t)
	logs := mutationLog(t, f.Manager)
	f.Manager.runBackgroundGC()
	if strings.Contains(logs.tail(), "automatic GC incomplete") {
		t.Fatalf("empty GC result was reported: %s", logs.tail())
	}
}

func TestMutationBackgroundGCIncludesReturnedError(t *testing.T) {
	t.Parallel()
	f := manualManagerFixture(t)
	logs := mutationLog(t, f.Manager)
	if err := f.Store.Close(); err != nil {
		t.Fatal(err)
	}
	f.Manager.runBackgroundGC()
	line := logs.tail()
	if !strings.Contains(line, "automatic GC incomplete") || !strings.Contains(line, "error=") {
		t.Fatalf("GC error was not preserved in log: %s", line)
	}
}

func mutationWorkspaceSnapshot(t *testing.T, manager *Manager, store *state.Store, workspaceID, sessionID string) (state.Slot, string) {
	t.Helper()
	ctx := context.Background()
	slot := testSlotRow(t, manager, workspaceID, sessionID+"-slot", 1, "SNAPSHOTTED")
	session := state.Session{ID: sessionID, WorkspaceID: workspaceID, SlotID: slot.ID, State: "ARCHIVED", AgentKind: "codex", TokenHash: state.HashToken("token")}
	if _, err := store.CreateSlotSession(ctx, slot, nil, session, ""); err != nil {
		t.Fatal(err)
	}
	relative := filepath.Join("_recovery", "workspace-snapshots", sessionID+".tar")
	root := strings.TrimSuffix(slot.Path, string(filepath.Separator)+slot.RelPath)
	archivePath := filepath.Join(root, relative)
	if err := os.MkdirAll(filepath.Dir(archivePath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(archivePath, []byte("archive"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveWorkspaceSnapshot(ctx, state.WorkspaceSnapshot{SessionID: sessionID, RootID: slot.RootID, RelPath: relative, SHA256: "sha", Status: "ARCHIVED", CreatedAt: state.FormatTime(time.Now().Add(-time.Hour)), ExpiresAt: state.FormatTime(time.Now().Add(-time.Minute))}); err != nil {
		t.Fatal(err)
	}
	return slot, archivePath
}

func TestMutationExpireWorkspaceSnapshotsRemovesRegisteredArchive(t *testing.T) {
	t.Parallel()
	ctx, manager, store, workspace, _, _ := managerCoverageFixture(t)
	_, archivePath := mutationWorkspaceSnapshot(t, manager, store, string(workspace.ID), "expire-workspace")
	progress := manager.expireWorkspaceSnapshots(ctx, map[string][]state.Snapshot{"expire-workspace": nil}, &archive.Manager{Git: manager.git})
	if progress.Completed != 1 {
		t.Fatalf("GC progress=%+v, want one completed snapshot", progress)
	}
	if _, err := os.Stat(archivePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("workspace archive remains: %v", err)
	}
	if _, found, err := store.WorkspaceSnapshot(ctx, "expire-workspace"); err != nil || found {
		t.Fatalf("workspace snapshot row found=%v err=%v", found, err)
	}
}

func mutationOpenFDCount(t *testing.T) int {
	t.Helper()
	entries, err := os.ReadDir("/dev/fd")
	if err != nil {
		t.Skipf("open descriptor accounting is unavailable: %v", err)
	}
	return len(entries)
}

func TestMutationExpireWorkspaceSnapshotsClosesRootDescriptor(t *testing.T) {
	ctx, manager, store, workspace, _, _ := managerCoverageFixture(t)
	before := mutationOpenFDCount(t)
	for i := 0; i < 24; i++ {
		sessionID := fmt.Sprintf("expire-fd-%02d", i)
		mutationWorkspaceSnapshot(t, manager, store, string(workspace.ID), sessionID)
		progress := manager.expireWorkspaceSnapshots(ctx, map[string][]state.Snapshot{sessionID: nil}, &archive.Manager{Git: manager.git})
		if progress.Completed != 1 {
			t.Fatalf("session %s progress=%+v", sessionID, progress)
		}
	}
	after := mutationOpenFDCount(t)
	if after-before > 3 {
		t.Fatalf("expired workspace roots leaked descriptors: before=%d after=%d", before, after)
	}
}

func TestMutationForgetWorkspaceSnapshotDoesNotWarnAfterRemoval(t *testing.T) {
	t.Parallel()
	_, manager, store, workspace, _, _ := managerCoverageFixture(t)
	_, archivePath := mutationWorkspaceSnapshot(t, manager, store, string(workspace.ID), "forget-workspace")
	logs := mutationLog(t, manager)
	if !manager.removeForgottenWorkspaceSnapshot(context.Background(), "forget-workspace") {
		t.Fatal("workspace snapshot was not found")
	}
	if _, err := os.Stat(archivePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("workspace archive remains: %v", err)
	}
	if strings.Contains(logs.tail(), "forget left a workspace snapshot behind") {
		t.Fatalf("successful workspace removal was logged as a failure: %s", logs.tail())
	}
}

func TestMutationForgetSnapshotRefsDoesNotWarnAfterDeletion(t *testing.T) {
	t.Parallel()
	requireDaemonIntegration(t)
	f := newOrphanRefFixture(t)
	ctx := context.Background()
	head := gitOutput(t, f.repository, "rev-parse", "HEAD")
	headRef := "refs/wx/recovery/forget/head"
	worktreeRef := "refs/wx/recovery/forget/worktree"
	gitRun(t, f.repository, "update-ref", headRef, head)
	gitRun(t, f.repository, "update-ref", worktreeRef, head)
	snapshot := state.Snapshot{SessionID: "forget-ref-session", RepositoryID: f.repositoryID, HeadOID: head, HeadRef: headRef, WorktreeOID: head, WorktreeRef: worktreeRef}
	f.manager.deleteForgottenSnapshotRefs(ctx, &archive.Manager{Git: f.manager.git}, snapshot)
	if _, err := os.Stat(filepath.Join(f.repository, ".git", "refs", "wx", "recovery", "forget", "head")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("head recovery ref remains: %v", err)
	}
	if strings.Contains(f.logs.String(), "forget left recovery refs behind") {
		t.Fatalf("successful ref deletion was logged as a failure: %s", f.logs.String())
	}
}

func mutationOrphanSession(t *testing.T, manager *Manager, store *state.Store, workspaceID, slotID, sessionID, slotState string, heartbeat time.Time) state.Session {
	t.Helper()
	ctx := context.Background()
	slot := testSlot(t, manager, workspaceID, slotID, 1, slotState)
	session := state.Session{ID: sessionID, WorkspaceID: workspaceID, SlotID: slotID, State: "ACTIVE", AgentKind: "codex", TokenHash: state.HashToken("token")}
	if _, err := store.CreateSlotSession(ctx, slot, nil, session, ""); err != nil {
		t.Fatal(err)
	}
	db := openTestDatabase(t, filepath.Join(filepath.Dir(manager.Config().Storage.WorktreeRoot), "state.db"))
	if _, err := db.Exec("UPDATE sessions SET last_heartbeat_at=? WHERE id=?", state.FormatTime(heartbeat), sessionID); err != nil {
		t.Fatal(err)
	}
	return session
}

func TestMutationReconcileOrphansUsesTheHeartbeatCutoff(t *testing.T) {
	t.Parallel()
	f := manualManagerFixture(t)
	ctx := context.Background()
	old := mutationOrphanSession(t, f.Manager, f.Store, "", "old-orphan-slot", "old-orphan", "LEASED", time.Now().Add(-time.Minute))
	// release の CAS は workspace 登録を参照しないため、fixture は空の workspace で閉じる。
	recent := mutationOrphanSession(t, f.Manager, f.Store, "", "recent-orphan-slot", "recent-orphan", "LEASED", time.Now().Add(-10*time.Second))
	boundary := mutationOrphanSession(t, f.Manager, f.Store, "", "boundary-orphan-slot", "boundary-orphan", "LEASED", time.Now().Add(-2*time.Second))
	f.Manager.reconcileOrphans(ctx)
	oldState, err := f.Store.SessionByID(ctx, old.ID)
	if err != nil {
		t.Fatal(err)
	}
	recentState, err := f.Store.SessionByID(ctx, recent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if oldState.State != "RELEASING" {
		t.Fatalf("old orphan state=%s, want RELEASING", oldState.State)
	}
	if recentState.State != "ACTIVE" {
		t.Fatalf("recent session state=%s, want ACTIVE", recentState.State)
	}
	boundaryState, err := f.Store.SessionByID(ctx, boundary.ID)
	if err != nil {
		t.Fatal(err)
	}
	if boundaryState.State != "ACTIVE" {
		t.Fatalf("boundary session state=%s, want ACTIVE", boundaryState.State)
	}
}

func TestMutationReconcileOrphansReportsReleaseFailure(t *testing.T) {
	t.Parallel()
	f := manualManagerFixture(t)
	ctx := context.Background()
	session := mutationOrphanSession(t, f.Manager, f.Store, "", "broken-orphan-slot", "broken-orphan", "READY", time.Now().Add(-time.Minute))
	logs := mutationLog(t, f.Manager)
	f.Manager.reconcileOrphans(ctx)
	if !strings.Contains(logs.tail(), "lease release failed") {
		t.Fatalf("release failure was not logged: %s", logs.tail())
	}
	if got, err := f.Store.SessionByID(ctx, session.ID); err != nil || got.State != "ACTIVE" {
		t.Fatalf("session after failed release=%+v err=%v", got, err)
	}
}

func TestMutationReconcileRegistryReportsStandbyFailure(t *testing.T) {
	t.Parallel()
	ctx, manager, _, workspace, _, _ := managerCoverageFixture(t, "repository")
	warm := 1
	manager.cfg.Pool.WarmPerWorkspace = warm
	manager.cfg.WorkspaceDefaults.WarmCount = &warm
	manager.cfg.WorkspaceDefaults.Worktree = "hot"
	root := manager.Config().Storage.WorktreeRoot
	manager.mu.Lock()
	delete(manager.rootIDs, filepath.Clean(root))
	manager.mu.Unlock()
	logs := mutationLog(t, manager)
	manager.reconcileRegistry(ctx)
	if !strings.Contains(logs.tail(), "workspace standby reconcile failed") {
		t.Fatalf("standby reconcile failure was not logged: %s", logs.tail())
	}
	_ = workspace
}

func TestMutationRegisteredSnapshotRejectsEscapingPath(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	owner, err := os.OpenRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = owner.Close() }()
	if err := removeRegisteredSnapshot(owner, "../outside"); err == nil {
		t.Fatal("escaping registered snapshot path was accepted")
	}
}

func TestMutationRegisteredSlotRejectsNestedParentSymlink(t *testing.T) {
	t.Parallel()
	ctx, manager, store, workspace, _, _ := managerCoverageFixture(t)
	root, rootID, err := manager.activeRoot()
	if err != nil {
		t.Fatal(err)
	}
	relative := filepath.Join("nested-parent", "registered-slot")
	slot := state.Slot{ID: "nested-parent-symlink", WorkspaceID: string(workspace.ID), Generation: 1, RootID: rootID, RelPath: relative, Path: filepath.Join(root, relative), State: "REMOVING"}
	if err := os.MkdirAll(slot.Path, 0o700); err != nil {
		t.Fatal(err)
	}
	moved := filepath.Join(root, "nested-parent-moved")
	if err := os.Rename(filepath.Dir(slot.Path), moved); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(moved, filepath.Dir(slot.Path)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateStandby(ctx, slot, nil); err != nil {
		t.Fatal(err)
	}
	err = manager.removeRegisteredSlot(ctx, slot)
	if !errors.Is(err, domain.ErrSymlinkPath) {
		t.Fatalf("nested parent error=%v, want %v", err, domain.ErrSymlinkPath)
	}
	if _, err := os.Stat(filepath.Join(moved, "registered-slot")); err != nil {
		t.Fatalf("symlink target changed: %v", err)
	}
}

func TestMutationRegisteredSlotProcessesAllRepositoriesAfterMissingCommonDir(t *testing.T) {
	t.Parallel()
	ctx, manager, store, workspace, _, databasePath := managerCoverageFixture(t)
	root, rootID, err := manager.activeRoot()
	if err != nil {
		t.Fatal(err)
	}
	missingCommon := filepath.Join(t.TempDir(), "missing-common")
	validCommon := filepath.Join(t.TempDir(), "valid-common")
	if err := os.MkdirAll(filepath.Join(validCommon, "worktrees", "second"), 0o700); err != nil {
		t.Fatal(err)
	}
	firstID, secondID := "aaa-missing-repository", "zzz-valid-repository"
	db := openTestDatabase(t, databasePath)
	now := state.FormatTime(time.Now())
	for _, row := range []struct {
		id, main, common string
	}{
		{firstID, filepath.Join(t.TempDir(), "first"), missingCommon},
		{secondID, filepath.Join(t.TempDir(), "second"), validCommon},
	} {
		if _, err := db.Exec("INSERT INTO repositories(id,main_worktree_path,common_git_dir,remote_name,first_seen_at,last_seen_at) VALUES(?,?,?,?,?,?)", row.id, row.main, row.common, "", now, now); err != nil {
			t.Fatal(err)
		}
	}
	relative := filepath.Join("all-repositories", "slot")
	slot := state.Slot{ID: "all-repositories-slot", WorkspaceID: string(workspace.ID), Generation: 1, RootID: rootID, RelPath: relative, Path: filepath.Join(root, relative), State: "REMOVING"}
	if err := os.MkdirAll(slot.Path, 0o700); err != nil {
		t.Fatal(err)
	}
	repos := []state.SlotRepository{{RepositoryID: firstID, DirName: "first", State: "READY"}, {RepositoryID: secondID, DirName: "second", State: "READY"}}
	if _, err := store.CreateStandby(ctx, slot, repos); err != nil {
		t.Fatal(err)
	}
	secondTarget := filepath.Join(slot.Path, "second")
	if err := os.WriteFile(filepath.Join(validCommon, "worktrees", "second", "gitdir"), []byte(secondTarget+"/.git\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := manager.removeRegisteredSlot(ctx, slot); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(validCommon, "worktrees", "second")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("second repository admin directory remains: %v", err)
	}
}

func TestMutationGitRegistrationHonorsCancellation(t *testing.T) {
	t.Parallel()
	_, manager, _, workspace, resolved, _ := managerCoverageFixture(t)
	common := string(resolved[0].Repository.CommonDir)
	admin := filepath.Join(common, "worktrees", "cancelled")
	if err := os.MkdirAll(admin, 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "missing-worktree")
	if err := os.WriteFile(filepath.Join(admin, "gitdir"), []byte(target+"/.git\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := manager.removeGitRegistrationLocked(ctx, common, target)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled registration removal error=%v, want context.Canceled", err)
	}
	_ = workspace
}

func TestMutationGitRegistrationFallsBackToAdminRemoval(t *testing.T) {
	t.Parallel()
	_, manager, _, workspace, resolved, _ := managerCoverageFixture(t)
	common := string(resolved[0].Repository.CommonDir)
	admin := filepath.Join(common, "worktrees", "broken-registration")
	if err := os.MkdirAll(admin, 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "missing-worktree")
	if err := os.WriteFile(filepath.Join(admin, "gitdir"), []byte(target+"/.git\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := manager.removeGitRegistrationLocked(context.Background(), common, target); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(admin); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("broken admin registration remains: %v", err)
	}
	_ = workspace
}

func TestMutationQuarantineOwnershipFailureMovesSlotToQuarantine(t *testing.T) {
	t.Parallel()
	ctx, manager, store, workspace, _, _ := managerCoverageFixture(t)
	slot := testSlot(t, manager, string(workspace.ID), "ownership-quarantine", 1, "RETIRING")
	if _, err := store.CreateStandby(ctx, slot, nil); err != nil {
		t.Fatal(err)
	}
	manager.quarantineOwnershipFailure(slot.ID, []string{"RETIRING"}, fmt.Errorf("%w: mismatch", state.ErrOwnership))
	got, err := store.Slot(ctx, slot.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != "QUARANTINED" {
		t.Fatalf("slot state=%s, want QUARANTINED", got.State)
	}
}

func TestMutationColdRepositoryRemovalRemovesRegisteredDirectory(t *testing.T) {
	t.Parallel()
	ctx, manager, store, workspace, resolved, _ := managerCoverageFixture(t)
	if len(resolved) != 1 {
		t.Fatalf("resolved repositories=%d", len(resolved))
	}
	repo := resolved[0].Repository
	dirName := testDirName(repo, manager.Config())
	slot := testSlot(t, manager, string(workspace.ID), "cold-repository", 1, "RETIRING")
	slotRepo := state.SlotRepository{RepositoryID: string(repo.ID), DirName: dirName, State: "RETIRING"}
	if _, err := store.CreateStandby(ctx, slot, []state.SlotRepository{slotRepo}); err != nil {
		t.Fatal(err)
	}
	worktree := filepath.Join(slot.Path, dirName)
	if err := os.MkdirAll(worktree, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(worktree, "untracked"), []byte("delete"), 0o600); err != nil {
		t.Fatal(err)
	}
	job := state.Job{ID: "cold-remove-job", SlotID: slot.ID, RepositoryID: string(repo.ID)}
	if err := manager.removeColdRepositoryJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(worktree); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cold repository directory remains: %v", err)
	}
	got, err := store.SlotRepository(ctx, slot.ID, string(repo.ID))
	if err != nil {
		t.Fatal(err)
	}
	if got.State != "COLD" {
		t.Fatalf("repository state=%s, want COLD", got.State)
	}
}
