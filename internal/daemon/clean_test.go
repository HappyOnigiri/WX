package daemon

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/HappyOnigiri/WX/internal/config"
	"github.com/HappyOnigiri/WX/internal/discovery"
	"github.com/HappyOnigiri/WX/internal/state"
)

// cleanFixture は clean の対象選定と進行を確かめるための manager・store・workspace を用意する。
func cleanFixture(t *testing.T) (*Manager, *state.Store, string) {
	t.Helper()
	root := t.TempDir()
	store, err := state.Open(filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	cfg := config.Defaults()
	cfg.Storage.WorktreeRoot = filepath.Join(root, "worktrees")
	manager := testManager(t, cfg, store)
	t.Cleanup(manager.Close)
	repository := discovery.Repository{ID: "repository", MainPath: discoveryPath(filepath.Join(root, "repository")), CommonDir: discoveryPath(filepath.Join(root, "repository", ".git")), DefaultBranch: "main"}
	registered := registerTestWorkspace(t, store, discovery.Workspace{ID: "workspace", Root: discoveryPath(root), Kind: "repository", Repositories: []discovery.Repository{repository}})
	return manager, store, string(registered.ID)
}

// beginCleanWithoutDriver は driver を起動せずに run を登録する。段階ごとの遷移を決定的に確かめる test が使う。
func beginCleanWithoutDriver(t *testing.T, manager *Manager, store *state.Store, all, standby bool) string {
	t.Helper()
	ctx := context.Background()
	candidates, err := store.CleanCandidates(ctx)
	if err != nil {
		t.Fatal(err)
	}
	targets := planCleanTargets(candidates, all, standby)
	mode := cleanMode(all, standby)
	runID, _, err := store.BeginCleanRun(ctx, "run", mode, targets, cleanWorkspaces(targets, mode != "normal"))
	if err != nil {
		t.Fatal(err)
	}
	return runID
}

// waitCleanTargetState は driver が対象を目的の状態へ進めるまで待つ。
func waitCleanTargetState(t *testing.T, store *state.Store, runID, slotID, want string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		targets, err := store.CleanTargets(context.Background(), runID)
		if err != nil {
			t.Fatal(err)
		}
		if targetByID(targets, slotID).State == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("clean target %s did not reach %s", slotID, want)
}

// waitCleanRunDone は run が閉じるまで待つ。
func waitCleanRunDone(t *testing.T, store *state.Store, runID string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		run, _, err := store.CleanRunByID(context.Background(), runID)
		if err != nil {
			t.Fatal(err)
		}
		if run.State == state.CleanRunDone {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("clean run %s did not finish", runID)
}

func targetByID(targets []state.CleanTarget, slotID string) state.CleanTarget {
	for _, target := range targets {
		if target.SlotID == slotID {
			return target
		}
	}
	return state.CleanTarget{}
}

func replyTargets(t *testing.T, reply map[string]any) []state.CleanTarget {
	t.Helper()
	targets, ok := reply["targets"].([]state.CleanTarget)
	if !ok {
		t.Fatalf("reply targets=%v", reply["targets"])
	}
	return targets
}

func TestPlanCleanTargetsSeparatesUnusedInUseAndUnprovableSlots(t *testing.T) {
	candidates := []state.CleanCandidate{
		{SlotID: "standby", SlotState: "READY"},
		{SlotID: "replenishing", SlotState: "PREPARING"},
		{SlotID: "stale", SlotState: "STALE"},
		{SlotID: "archived", SlotState: "SNAPSHOTTED", SessionID: "old", SessionState: "ARCHIVED"},
		{SlotID: "held", SlotState: "QUARANTINED"},
		{SlotID: "active", SlotState: "LEASED", SessionID: "live", SessionState: "ACTIVE"},
		{SlotID: "unbound", SlotState: "UNBOUND", SessionID: "resume", SessionState: "UNBOUND"},
		{SlotID: "dirty-unbound", SlotState: "UNBOUND", SessionID: "resume2", SessionState: "UNBOUND", Repositories: 1},
		{SlotID: "restoring", SlotState: "RESTORING", SessionID: "restore", SessionState: "RESTORING", ParentSnapshots: 1},
		{SlotID: "lost-restore", SlotState: "RESTORING", SessionID: "restore2", SessionState: "RESTORING"},
	}
	normal := planCleanTargets(candidates, false, false)
	// 待機用 slot は貸出前でも残す。世代遅れの STALE は再利用されないので待機用として扱わない。
	for _, slotID := range []string{"standby", "replenishing"} {
		if got := targetByID(normal, slotID); got.State != cleanTargetSkipped || got.Reason == "" {
			t.Fatalf("standby slot %s in normal mode=%+v", slotID, got)
		}
	}
	if got := targetByID(normal, "stale").State; got != cleanTargetPending {
		t.Fatalf("stale slot state=%s", got)
	}
	if got := targetByID(normal, "archived").State; got != cleanTargetPending {
		t.Fatalf("archived session slot state=%s", got)
	}
	if got := targetByID(normal, "held").State; got != cleanTargetQuarantined {
		t.Fatalf("quarantined slot state=%s", got)
	}
	if got := targetByID(normal, "active"); got.State != cleanTargetSkipped || got.Reason == "" {
		t.Fatalf("in-use slot in normal mode=%+v", got)
	}
	// --standby は待機用だけを加え、使用中の session には触れない。
	standby := planCleanTargets(candidates, false, true)
	for _, slotID := range []string{"standby", "replenishing"} {
		if got := targetByID(standby, slotID).State; got != cleanTargetPending {
			t.Fatalf("--standby target %s state=%s", slotID, got)
		}
	}
	if got := targetByID(standby, "active"); got.State != cleanTargetSkipped || got.Reason == "" {
		t.Fatalf("in-use slot in standby mode=%+v", got)
	}
	all := planCleanTargets(candidates, true, false)
	for _, slotID := range []string{"standby", "replenishing"} {
		if got := targetByID(all, slotID).State; got != cleanTargetPending {
			t.Fatalf("--all target %s state=%s", slotID, got)
		}
	}
	for _, slotID := range []string{"active", "unbound", "restoring"} {
		if got := targetByID(all, slotID).State; got != cleanTargetPending {
			t.Fatalf("--all target %s state=%s", slotID, got)
		}
	}
	for _, slotID := range []string{"dirty-unbound", "lost-restore"} {
		if got := targetByID(all, slotID); got.State != cleanTargetSkipped || got.Reason == "" {
			t.Fatalf("unprovable target %s=%+v", slotID, got)
		}
	}
	if got := targetByID(all, "held").State; got != cleanTargetQuarantined {
		t.Fatalf("--all still deletes quarantined slots: state=%s", got)
	}
}

func TestCleanWorkspacesAndSummaryCountOnlyActionableTargets(t *testing.T) {
	targets := []state.CleanTarget{
		{SlotID: "a", WorkspaceID: "one", State: cleanTargetPending},
		{SlotID: "b", WorkspaceID: "one", State: cleanTargetPending},
		{SlotID: "c", WorkspaceID: "two", State: cleanTargetSkipped},
		{SlotID: "d", State: cleanTargetPending},
	}
	workspaces := cleanWorkspaces(targets, true)
	if len(workspaces) != 1 || workspaces[0] != "one" {
		t.Fatalf("suspended workspaces=%v", workspaces)
	}
	// 待機用 worktree を残すモードは補充も止めない。
	if kept := cleanWorkspaces(targets, false); len(kept) != 0 {
		t.Fatalf("workspaces suspended while standby worktrees are kept=%v", kept)
	}
	summary := cleanSummary(targets)
	if summary["total"] != 4 || summary[cleanTargetPending] != 3 || summary[cleanTargetSkipped] != 1 || summary[cleanTargetDone] != 0 {
		t.Fatalf("summary=%v", summary)
	}
}

func TestCleanDryRunChangesNothing(t *testing.T) {
	manager, store, workspaceID := cleanFixture(t)
	ctx := context.Background()
	slot := testSlot(t, manager, workspaceID, "standby", 1, "READY")
	if _, err := store.CreateStandby(ctx, slot, nil); err != nil {
		t.Fatal(err)
	}
	reply, err := manager.Clean(ctx, false, true, true)
	if err != nil {
		t.Fatal(err)
	}
	if reply["dry_run"] != true || reply["state"] != "DRY_RUN" || reply["run_id"] != "" {
		t.Fatalf("dry-run reply=%v", reply)
	}
	targets := replyTargets(t, reply)
	if len(targets) != 1 || targets[0].State != cleanTargetPending {
		t.Fatalf("dry-run targets=%+v", targets)
	}
	if _, found, err := store.ActiveCleanRun(ctx); err != nil || found {
		t.Fatalf("dry run created a clean run: found=%v err=%v", found, err)
	}
	if suspended, err := store.ReplenishSuspended(ctx, workspaceID); err != nil || suspended {
		t.Fatalf("dry run suspended replenishment: %v err=%v", suspended, err)
	}
	stored, err := store.Slot(ctx, "standby")
	if err != nil || stored.State != "READY" {
		t.Fatalf("dry run changed slot state=%+v err=%v", stored, err)
	}
}

func TestCleanKeepsStandbyWorktreesUntilTheyAreAskedFor(t *testing.T) {
	manager, store, workspaceID := cleanFixture(t)
	ctx := context.Background()
	slot := testSlot(t, manager, workspaceID, "standby", 1, "READY")
	if _, err := store.CreateStandby(ctx, slot, nil); err != nil {
		t.Fatal(err)
	}
	reply, err := manager.Clean(ctx, false, false, false)
	if err != nil {
		t.Fatal(err)
	}
	runID, _ := reply["run_id"].(string)
	targets := replyTargets(t, reply)
	if len(targets) != 1 || targets[0].State != cleanTargetSkipped || targets[0].Reason == "" {
		t.Fatalf("standby target in normal mode=%+v", targets)
	}
	waitCleanRunDone(t, store, runID)
	stored, err := store.Slot(ctx, "standby")
	if err != nil || stored.State != "READY" {
		t.Fatalf("standby slot after a normal clean=%+v err=%v", stored, err)
	}
	// 待機用 worktree を残す以上、補充も止めない。
	if suspended, err := store.ReplenishSuspended(ctx, workspaceID); err != nil || suspended {
		t.Fatalf("normal clean suspended replenishment: %v err=%v", suspended, err)
	}
}

func TestCleanRemovesUnusedStandbyAndFinishesTheRun(t *testing.T) {
	manager, store, workspaceID := cleanFixture(t)
	ctx := context.Background()
	slot := testSlot(t, manager, workspaceID, "standby", 1, "READY")
	if _, err := store.CreateStandby(ctx, slot, nil); err != nil {
		t.Fatal(err)
	}
	reply, err := manager.Clean(ctx, false, true, false)
	if err != nil {
		t.Fatal(err)
	}
	runID, _ := reply["run_id"].(string)
	if runID == "" {
		t.Fatalf("accepted reply=%v", reply)
	}
	if suspended, err := store.ReplenishSuspended(ctx, workspaceID); err != nil || !suspended {
		t.Fatalf("replenishment was not suspended: %v err=%v", suspended, err)
	}
	waitCleanTargetState(t, store, runID, "standby", cleanTargetRemoving)
	stored, err := store.Slot(ctx, "standby")
	if err != nil || stored.State != "REMOVING" {
		t.Fatalf("slot after scheduling=%+v err=%v", stored, err)
	}
	// 削除ジョブの完了だけを監視し、worker を占有したまま待たないことを確かめる。
	if err := store.FinishRemoval(ctx, "standby"); err != nil {
		t.Fatal(err)
	}
	waitCleanTargetState(t, store, runID, "standby", cleanTargetDone)
	waitCleanRunDone(t, store, runID)
	// 補充停止は run の完了では解除せず、次に貸出・resume が成功するまで残る。
	if suspended, err := store.ReplenishSuspended(ctx, workspaceID); err != nil || !suspended {
		t.Fatalf("replenishment resumed too early: %v err=%v", suspended, err)
	}
	manager.resumeReplenish(ctx, workspaceID)
	if suspended, err := store.ReplenishSuspended(ctx, workspaceID); err != nil || suspended {
		t.Fatalf("replenishment stayed suspended: %v err=%v", suspended, err)
	}
}

func TestCleanQuarantinedSlotIsKeptWithAReason(t *testing.T) {
	manager, store, workspaceID := cleanFixture(t)
	ctx := context.Background()
	slot := testSlot(t, manager, workspaceID, "held", 1, "READY")
	if _, err := store.CreateStandby(ctx, slot, nil); err != nil {
		t.Fatal(err)
	}
	if err := store.SetSlotState(ctx, "held", []string{"READY"}, "QUARANTINED", "TEST"); err != nil {
		t.Fatal(err)
	}
	reply, err := manager.Clean(ctx, true, false, false)
	if err != nil {
		t.Fatal(err)
	}
	runID, _ := reply["run_id"].(string)
	targets, err := store.CleanTargets(ctx, runID)
	if err != nil || len(targets) != 1 || targets[0].State != cleanTargetQuarantined || targets[0].Reason == "" {
		t.Fatalf("quarantined target=%+v err=%v", targets, err)
	}
	stored, err := store.Slot(ctx, "held")
	if err != nil || stored.State != "QUARANTINED" {
		t.Fatalf("quarantined slot changed=%+v err=%v", stored, err)
	}
	waitCleanRunDone(t, store, runID)
}

func TestCleanAllRequestsTerminationAndTimesOutWithoutKilling(t *testing.T) {
	manager, store, workspaceID := cleanFixture(t)
	ctx := context.Background()
	slot := testSlot(t, manager, workspaceID, "leased", 1, "LEASED")
	session := state.Session{ID: "leased", WorkspaceID: workspaceID, SlotID: "leased", State: "ACTIVE", AgentKind: "codex", TokenHash: state.HashToken("token")}
	if _, err := store.CreateSlotSession(ctx, slot, nil, session, ""); err != nil {
		t.Fatal(err)
	}
	// 通常モードは使用中の session に触れない。
	normal, err := manager.Clean(ctx, false, false, true)
	if err != nil {
		t.Fatal(err)
	}
	if got := targetByID(replyTargets(t, normal), "leased").State; got != cleanTargetSkipped {
		t.Fatalf("normal mode target state=%s", got)
	}
	runID := beginCleanWithoutDriver(t, manager, store, true, false)
	waiting := map[string]time.Time{}
	if _, err := manager.advanceClean(ctx, runID, waiting); err != nil {
		t.Fatal(err)
	}
	targets, err := store.CleanTargets(ctx, runID)
	if err != nil || targetByID(targets, "leased").State != cleanTargetTerminating {
		t.Fatalf("targets after termination request=%+v err=%v", targets, err)
	}
	request, found, err := store.PendingTermination(ctx, "leased")
	if err != nil || !found {
		t.Fatalf("pending termination: found=%v err=%v", found, err)
	}
	// heartbeat と agent 登録の応答は、その要求をそのまま client へ渡す。
	heartbeat := manager.terminationReply(ctx, "leased", map[string]any{"ok": true})
	terminate, ok := heartbeat["terminate"].(map[string]any)
	if !ok || terminate["request_id"] != request.RequestID {
		t.Fatalf("heartbeat reply=%v", heartbeat)
	}
	// 期限を過ぎても停止を確認できない対象は失敗として閉じ、session と slot はそのまま残す。
	expired := targetByID(targets, "leased")
	expired.TerminateDeadline = state.FormatTime(time.Now().Add(-time.Second))
	run, _, err := store.CleanRunByID(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	manager.advanceTerminating(ctx, run, expired)
	targets, err = store.CleanTargets(ctx, runID)
	if err != nil || targetByID(targets, "leased").State != cleanTargetFailed {
		t.Fatalf("targets after timeout=%+v err=%v", targets, err)
	}
	if _, found, err := store.PendingTermination(ctx, "leased"); err != nil || found {
		t.Fatalf("timed-out request still pending: found=%v err=%v", found, err)
	}
	stored, err := store.Slot(ctx, "leased")
	if err != nil || stored.State != "LEASED" {
		t.Fatalf("timed-out slot=%+v err=%v", stored, err)
	}
}

func TestConfirmTerminationReleasesThroughTheNormalPath(t *testing.T) {
	manager, store, workspaceID := cleanFixture(t)
	ctx := context.Background()
	slot := testSlot(t, manager, workspaceID, "leased", 1, "LEASED")
	session := state.Session{ID: "leased", WorkspaceID: workspaceID, SlotID: "leased", State: "ACTIVE", AgentKind: "codex", TokenHash: state.HashToken("token")}
	if _, err := store.CreateSlotSession(ctx, slot, nil, session, ""); err != nil {
		t.Fatal(err)
	}
	runID := beginCleanWithoutDriver(t, manager, store, true, false)
	waiting := map[string]time.Time{}
	if _, err := manager.advanceClean(ctx, runID, waiting); err != nil {
		t.Fatal(err)
	}
	request, _, err := store.PendingTermination(ctx, "leased")
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.ConfirmTermination(ctx, "leased", "wrong-token", request.RequestID); err == nil {
		t.Fatal("a caller without the session token confirmed the termination")
	}
	if err := manager.ConfirmTermination(ctx, "leased", "token", request.RequestID); err != nil {
		t.Fatal(err)
	}
	released, err := store.SessionByID(ctx, "leased")
	if err != nil || released.State != "RELEASING" {
		t.Fatalf("session after confirmation=%+v err=%v", released, err)
	}
	stored, err := store.Slot(ctx, "leased")
	if err != nil || stored.State != "DRAINING" {
		t.Fatalf("slot after confirmation=%+v err=%v", stored, err)
	}
	if _, err := manager.advanceClean(ctx, runID, waiting); err != nil {
		t.Fatal(err)
	}
	targets, err := store.CleanTargets(ctx, runID)
	if err != nil || targetByID(targets, "leased").State != cleanTargetSaving {
		t.Fatalf("targets after confirmation=%+v err=%v", targets, err)
	}
}

func TestCleanWaitsForABusySlotAndGivesUpAtTheBoundaryLimit(t *testing.T) {
	manager, store, workspaceID := cleanFixture(t)
	ctx := context.Background()
	slot := testSlot(t, manager, workspaceID, "busy", 1, "PREPARING")
	if _, err := store.CreateStandby(ctx, slot, nil); err != nil {
		t.Fatal(err)
	}
	runID := beginCleanWithoutDriver(t, manager, store, false, true)
	waiting := map[string]time.Time{}
	for range 2 {
		if done, err := manager.advanceClean(ctx, runID, waiting); err != nil || done {
			t.Fatalf("run closed while a slot was still preparing: done=%v err=%v", done, err)
		}
	}
	targets, err := store.CleanTargets(ctx, runID)
	if err != nil || targetByID(targets, "busy").State != cleanTargetPending {
		t.Fatalf("targets while preparing=%+v err=%v", targets, err)
	}
	waiting["busy"] = time.Now().Add(-cleanBoundaryWait - time.Second)
	done, err := manager.advanceClean(ctx, runID, waiting)
	if err != nil || !done {
		t.Fatalf("run after the boundary limit: done=%v err=%v", done, err)
	}
	targets, err = store.CleanTargets(ctx, runID)
	if err != nil || targetByID(targets, "busy").State != cleanTargetFailed {
		t.Fatalf("targets after the boundary limit=%+v err=%v", targets, err)
	}
}

func TestCleanRunRefusesNewLeasesAndRejoinsInsteadOfDuplicating(t *testing.T) {
	manager, store, workspaceID := cleanFixture(t)
	ctx := context.Background()
	slot := testSlot(t, manager, workspaceID, "standby", 1, "READY")
	if _, err := store.CreateStandby(ctx, slot, nil); err != nil {
		t.Fatal(err)
	}
	first, err := manager.Clean(ctx, false, true, false)
	if err != nil {
		t.Fatal(err)
	}
	second, err := manager.Clean(ctx, false, true, false)
	if err != nil {
		t.Fatal(err)
	}
	if first["run_id"] != second["run_id"] {
		t.Fatalf("rerun created a second run: %v then %v", first["run_id"], second["run_id"])
	}
	// 待機用 worktree を含めるかは mode の一部なので、対象の違う再実行は合流させない。
	for _, conflicting := range []struct{ all, standby bool }{{all: true}, {}} {
		if _, err := manager.Clean(ctx, conflicting.all, conflicting.standby, false); !IsCleanConflict(err) {
			t.Fatalf("mode conflict for all=%v standby=%v err=%v", conflicting.all, conflicting.standby, err)
		}
	}
	newSlot := testSlot(t, manager, workspaceID, "fresh", 1, "PREPARING")
	newSession := state.Session{ID: "fresh", WorkspaceID: workspaceID, SlotID: "fresh", State: "STARTING", AgentKind: "codex", TokenHash: state.HashToken("token")}
	_, err = store.CreateSlotSession(ctx, newSlot, nil, newSession, "PREPARE")
	if !IsCleanConflict(err) || !errors.Is(err, state.ErrCleanInProgress) {
		t.Fatalf("lease during clean err=%v", err)
	}
}

func TestCleanStatusReportsAcceptedRunAndResumesAfterRestart(t *testing.T) {
	manager, store, workspaceID := cleanFixture(t)
	ctx := context.Background()
	slot := testSlot(t, manager, workspaceID, "standby", 1, "READY")
	if _, err := store.CreateStandby(ctx, slot, nil); err != nil {
		t.Fatal(err)
	}
	reply, err := manager.Clean(ctx, false, true, false)
	if err != nil {
		t.Fatal(err)
	}
	runID, _ := reply["run_id"].(string)
	status, err := manager.CleanStatus(ctx, runID)
	if err != nil || status["run_id"] != runID || status["state"] != state.CleanRunRunning {
		t.Fatalf("status=%v err=%v", status, err)
	}
	if _, err := manager.CleanStatus(ctx, "missing"); err == nil {
		t.Fatal("status for an unknown run succeeded")
	}
	// 再起動後も同じ run を再開する。driver は run ごとに 1 つだけ立ち上がる。
	manager.resumeCleanRuns(ctx)
	manager.mu.RLock()
	driving := manager.cleanDrivers[runID]
	manager.mu.RUnlock()
	if !driving {
		t.Fatal("resumed clean run has no driver")
	}
}

func TestReplenishSuspensionStopsStandbyCreation(t *testing.T) {
	manager, store, workspaceID := cleanFixture(t)
	ctx := context.Background()
	if _, _, err := store.BeginCleanRun(ctx, "run", "normal", nil, []string{workspaceID}); err != nil {
		t.Fatal(err)
	}
	workspaceRecord, err := store.Workspace(ctx, workspaceID)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.ensureStandby(ctx, workspaceRecord); err != nil {
		t.Fatal(err)
	}
	if count := store.StandbyCount(ctx, workspaceID); count != 0 {
		t.Fatalf("standby slots created while replenishment was suspended: %d", count)
	}
}
