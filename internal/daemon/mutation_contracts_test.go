package daemon

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/HappyOnigiri/WorktreeX/internal/state"
	"github.com/HappyOnigiri/WorktreeX/internal/update"
	"github.com/HappyOnigiri/WorktreeX/internal/workspace"
)

func TestMutationUpdateChildEnvAcceptsAnEmptyEnvironment(t *testing.T) {
	t.Parallel()
	env := updateChildEnv(nil, "/wx/bin")
	if len(env) != 1 || env[0] != "PATH=/wx/bin:"+updateApplySystemPath {
		t.Fatalf("empty child environment=%v, want only the constructed PATH", env)
	}
}

func TestMutationAutomaticApplyReportsItsFinalFailedStart(t *testing.T) {
	t.Parallel()
	manager, store, spy := updateApplyFixture(t)
	spy.failure = errors.New("spawn failed")
	var logs bytes.Buffer
	manager.log = slog.New(slog.NewTextHandler(&logs, nil))

	when := time.Now().Add(-2*state.UpdateApplyRetryInterval - time.Minute)
	for want := 1; want < state.UpdateApplyMaxAttempts; want++ {
		attempt, err := store.ClaimUpdateApply(context.Background(), "v1.1.0", when)
		if err != nil || attempt != want {
			t.Fatalf("seed apply attempt=%d err=%v, want %d", attempt, err, want)
		}
		when = when.Add(state.UpdateApplyRetryInterval)
	}
	manager.maybeApplyUpdate(context.Background())
	if len(spy.argv) != 1 {
		t.Fatalf("final apply attempt count=%d, want one", len(spy.argv))
	}
	if !strings.Contains(logs.String(), "final attempt") || !strings.Contains(logs.String(), "could not be started") {
		t.Fatalf("final failed apply was not described: %s", logs.String())
	}
}

func TestMutationFailedUpdateCheckReportsRecordingFailure(t *testing.T) {
	t.Parallel()
	f := manualManagerFixture(t)
	enabled := true
	f.Manager.cfg.System.Update.AutoCheck = &enabled
	f.Manager.updateProbe = releaseProbe("v1.0.0", func(context.Context) (update.Release, error) {
		return update.Release{}, errors.New("offline")
	})
	db := openTestDatabase(t, f.DatabasePath)
	if _, err := db.Exec(`CREATE TRIGGER mutation_update_check_record_failure BEFORE UPDATE OF last_error ON update_checks WHEN NEW.last_error<>'' BEGIN SELECT RAISE(ABORT,'record failed'); END`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = db.Exec(`DROP TRIGGER IF EXISTS mutation_update_check_record_failure`) })
	var logs bytes.Buffer
	f.Manager.log = slog.New(slog.NewTextHandler(&logs, nil))
	f.Manager.maybeCheckUpdate(context.Background())
	if !strings.Contains(logs.String(), "update check result could not be recorded") {
		t.Fatalf("recording failure was not logged: %s", logs.String())
	}
}

func TestMutationResolveLeaseAttrsKeepsPositiveOwnerPID(t *testing.T) {
	t.Parallel()
	f := manualManagerFixture(t)
	attrs, err := f.Manager.resolveLeaseAttrs(context.Background(), state.LeaseKindPath, "", "", 1)
	if err != nil || attrs.OwnerPID != 1 {
		t.Fatalf("positive owner PID attrs=%+v err=%v", attrs, err)
	}
}

func TestMutationReleaseAgentSessionHonorsLiveSessionEndHook(t *testing.T) {
	t.Parallel()
	ctx, manager, store, workspaceRecord, _, databasePath := managerCoverageFixture(t, "repository")
	slot := testSlot(t, manager, string(workspaceRecord.ID), "live-agent-end", 1, "LEASED")
	session := state.Session{
		ID: slot.ID, WorkspaceID: string(workspaceRecord.ID), SlotID: slot.ID, State: "ACTIVE",
		AgentKind: "codex", AgentSessionID: "native-live", ClientPID: os.Getpid(), TokenHash: state.HashToken("live-agent-end"),
	}
	if _, err := store.CreateSlotSession(ctx, slot, nil, session, ""); err != nil {
		t.Fatal(err)
	}
	if err := manager.BindAgentSession(ctx, session.ID, "live-agent-end", session.AgentSessionID); err != nil {
		t.Fatal(err)
	}
	released, err := manager.ReleaseAgentSession(ctx, session.ID, "live-agent-end", "session-end-hook", session.AgentSessionID)
	if err != nil || !released {
		t.Fatalf("live SessionEnd released=%v err=%v", released, err)
	}
	stored, err := store.SessionByID(ctx, session.ID)
	if err != nil || stored.State != "ACTIVE" {
		t.Fatalf("live SessionEnd changed state=%s err=%v", stored.State, err)
	}

	// native session の確認後に保存が失敗しても、エラーとして返す。
	failureID := "agent-release-error"
	failureSlot := testSlot(t, manager, string(workspaceRecord.ID), failureID, 1, "LEASED")
	failureSession := state.Session{ID: failureID, WorkspaceID: string(workspaceRecord.ID), SlotID: failureID, State: "ACTIVE", AgentKind: "codex", AgentSessionID: "native-error", TokenHash: state.HashToken(failureID)}
	if _, err := store.CreateSlotSession(ctx, failureSlot, nil, failureSession, ""); err != nil {
		t.Fatal(err)
	}
	if err := manager.BindAgentSession(ctx, failureID, failureID, failureSession.AgentSessionID); err != nil {
		t.Fatal(err)
	}
	db := openTestDatabase(t, databasePath)
	if _, err := db.Exec(`CREATE TRIGGER mutation_agent_release_failure BEFORE UPDATE OF state ON sessions WHEN OLD.id='agent-release-error' BEGIN SELECT RAISE(ABORT,'release failed'); END`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = db.Exec(`DROP TRIGGER IF EXISTS mutation_agent_release_failure`) })
	released, err = manager.ReleaseAgentSession(ctx, failureID, failureID, "client-exit", failureSession.AgentSessionID)
	if err == nil || !strings.Contains(err.Error(), "release failed") {
		t.Fatalf("failed agent release released=%v err=%v", released, err)
	}
	stored, err = store.SessionByID(ctx, failureID)
	if err != nil || stored.State != "ACTIVE" {
		t.Fatalf("failed agent release changed state=%s err=%v", stored.State, err)
	}
}

func TestMutationBindAgentSessionFromHookLogsOnlyAuxiliarySuccess(t *testing.T) {
	t.Parallel()
	ctx, manager, store, workspaceRecord, _, databasePath := managerCoverageFixture(t, "repository")
	primaryID := "hook-primary"
	primarySlot := testSlot(t, manager, string(workspaceRecord.ID), primaryID, 1, "LEASED")
	primary := state.Session{ID: primaryID, WorkspaceID: string(workspaceRecord.ID), SlotID: primaryID, State: "ACTIVE", AgentKind: "codex", TokenHash: state.HashToken(primaryID)}
	if _, err := store.CreateSlotSession(ctx, primarySlot, nil, primary, ""); err != nil {
		t.Fatal(err)
	}
	if err := manager.BindAgentSession(ctx, primaryID, primaryID, "native-primary"); err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	manager.log = slog.New(slog.NewTextHandler(&logs, nil))
	auxiliary, err := manager.BindAgentSessionFromHook(ctx, primaryID, primaryID, "native-fork", "fork")
	if err != nil || auxiliary {
		t.Fatalf("auxiliary bind primary=%v err=%v", auxiliary, err)
	}
	if !strings.Contains(logs.String(), "ignored an auxiliary Codex session start") {
		t.Fatalf("auxiliary bind was not logged: %s", logs.String())
	}

	failureID := "hook-error"
	failureSlot := testSlot(t, manager, string(workspaceRecord.ID), failureID, 1, "LEASED")
	failure := state.Session{ID: failureID, WorkspaceID: string(workspaceRecord.ID), SlotID: failureID, State: "ACTIVE", AgentKind: "codex", TokenHash: state.HashToken(failureID)}
	if _, err := store.CreateSlotSession(ctx, failureSlot, nil, failure, ""); err != nil {
		t.Fatal(err)
	}
	db := openTestDatabase(t, databasePath)
	if _, err := db.Exec(`CREATE TRIGGER mutation_hook_bind_failure BEFORE UPDATE OF agent_session_id ON sessions WHEN OLD.id='hook-error' BEGIN SELECT RAISE(ABORT,'bind failed'); END`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = db.Exec(`DROP TRIGGER IF EXISTS mutation_hook_bind_failure`) })
	logs.Reset()
	auxiliary, err = manager.BindAgentSessionFromHook(ctx, failureID, failureID, "native-error", "startup")
	if auxiliary || err == nil || !strings.Contains(err.Error(), "bind failed") {
		t.Fatalf("failed bind primary=%v err=%v", auxiliary, err)
	}
	if strings.Contains(logs.String(), "ignored an auxiliary Codex session start") {
		t.Fatalf("failed bind was logged as auxiliary success: %s", logs.String())
	}
}

func TestMutationRemoveSlotJobSchedulesReplenishment(t *testing.T) {
	t.Parallel()
	ctx, manager, store, workspaceRecord, _, _ := managerCoverageFixture(t, "repository")
	slot := testSlot(t, manager, string(workspaceRecord.ID), "remove-replenish", 1, "PREPARING")
	prepare, err := store.CreateStandby(ctx, slot, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetSlotState(ctx, slot.ID, []string{"PREPARING"}, "REMOVING", ""); err != nil {
		t.Fatal(err)
	}
	claimed, err := store.ClaimJob(ctx, prepare.ID, "test")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.FinishJob(ctx, claimed.ID, "test", nil); err != nil {
		t.Fatal(err)
	}
	if err := manager.removeSlotJob(ctx, state.Job{ID: "remove-replenish-job", Kind: "REMOVE", SlotID: slot.ID}); err != nil {
		t.Fatal(err)
	}
	matches := 0
	manager.jobQueue.mu.Lock()
	for _, queued := range manager.jobQueue.pending[jobClassMaintenance] {
		if queued.class == jobClassMaintenance {
			matches++
		}
	}
	manager.jobQueue.mu.Unlock()
	if matches == 0 {
		t.Fatal("removeSlotJob did not schedule the replenishment job")
	}
}

func TestMutationRootSelectionUsesTheLongestRegisteredRoot(t *testing.T) {
	t.Parallel()
	f := manualManagerFixture(t)
	root := filepath.Clean(f.Config.Storage.WorktreeRoot)
	nested := filepath.Join(root, "nested")
	f.Manager.mu.Lock()
	f.Manager.roots = map[string]bool{root: true, nested: true}
	f.Manager.rootIdentities = map[string]string{root: "root", nested: "nested"}
	f.Manager.mu.Unlock()
	got, ok := f.Manager.rootForPath(filepath.Join(nested, "slot"))
	if !ok || got != nested {
		t.Fatalf("rootForPath=%q,%v, want nested root", got, ok)
	}
}

func TestMutationLoadRootGenerationsKeepsMatchingPinnedGeneration(t *testing.T) {
	t.Parallel()
	f := manualManagerFixture(t)
	rows, err := f.Store.Roots(context.Background())
	if err != nil || len(rows) == 0 {
		t.Fatalf("roots=%+v err=%v", rows, err)
	}
	row := rows[0]
	f.Manager.mu.Lock()
	f.Manager.rootIdentities = map[string]string{row.Path: row.Identity}
	f.Manager.rootIDs = map[string]string{}
	f.Manager.mu.Unlock()
	f.Manager.loadRootGenerations(context.Background())
	f.Manager.mu.RLock()
	got := f.Manager.rootIDs[row.Path]
	f.Manager.mu.RUnlock()
	if got != row.ID {
		t.Fatalf("matching pinned generation ID=%q, want %q", got, row.ID)
	}
}

func TestMutationArchivedSnapshotStatesAreIdempotent(t *testing.T) {
	t.Parallel()
	for _, stateName := range []string{"SNAPSHOTTED", "ARCHIVED"} {
		if !archivedSnapshotSlotState(stateName) {
			t.Errorf("archived slot state %q was not recognized", stateName)
		}
	}
	if archivedSnapshotSlotState("DRAINING") {
		t.Fatal("draining slot was treated as an archived snapshot")
	}
}

func TestMutationSubmoduleReportsUseStrictOrdering(t *testing.T) {
	t.Parallel()
	manager := &Manager{}
	outcomes := &workspace.SubmoduleOutcomes{}
	outcomes.BeginRepository("repo-b")
	outcomes.BeginRepository("repo-a")
	outcomes.Add(workspace.SubmoduleOutcome{Repository: "repo-b", Path: "b", Depth: 2, Action: workspace.SubmoduleActionMaterialized})
	outcomes.Add(workspace.SubmoduleOutcome{Repository: "repo-b", Path: "a", Depth: 2, Action: workspace.SubmoduleActionSkipped})
	outcomes.Add(workspace.SubmoduleOutcome{Repository: "repo-a", Path: "a", Depth: 1, Action: workspace.SubmoduleActionMaterialized})
	report := manager.recordPrepareSubmodules(outcomes)
	if len(report.Summaries) != 3 || report.Summaries[0].Repository != "repo-a" || report.Summaries[1].Depth != 1 {
		t.Fatalf("summaries=%+v", report.Summaries)
	}
	if len(report.Details) != 3 || report.Details[0].Action != workspace.SubmoduleActionSkipped || report.Details[1].Path != "a" || report.Details[2].Path != "b" {
		t.Fatalf("details=%+v", report.Details)
	}
}

func TestMutationSubmoduleRefFindingsPreserveMismatchedKind(t *testing.T) {
	t.Parallel()
	findings := submoduleRefFindings([]submoduleRefIssue{
		{Kind: submoduleRefMissing, ModuleDir: "/module", Ref: "refs/wx/recovery/a", Path: "a"},
		{Kind: submoduleRefMismatched, ModuleDir: "/module", Ref: "refs/wx/recovery/b", Path: "b"},
	})
	if len(findings) != 2 || !strings.Contains(findings[1].Summary, "does not point") {
		t.Fatalf("submodule findings=%+v", findings)
	}
}

func TestMutationSlotRepositoryViewsSortNamesDeterministically(t *testing.T) {
	t.Parallel()
	views := slotRepositoryViews(map[string]workspace.RepositoryUsage{
		"repo-z": {Files: 2, AllocatedBytes: 20},
		"repo-a": {Files: 1, AllocatedBytes: 10},
	})
	if len(views) != 2 || views[0].Name != "repo-a" || views[1].Name != "repo-z" {
		t.Fatalf("repository views=%+v, want name order", views)
	}
}

func TestMutationGCUsesPastRetentionCutoffAndAddsEveryCandidate(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	cutoff := gcRetentionCutoff(now, time.Hour)
	if cutoff != state.FormatTime(now.Add(-time.Hour)) {
		t.Fatalf("retention cutoff=%q, want one hour before now", cutoff)
	}
	progress := gcProgress{}
	for _, count := range []int{1, 2, 3, 4, 5, 6, 7} {
		progress.Candidates += count
	}
	if progress.Candidates != 28 {
		t.Fatalf("candidate total=%d, want 28", progress.Candidates)
	}
}

func TestMutationReleaseExitedDirectLeasesDoesNotLogSuccessfulRelease(t *testing.T) {
	t.Parallel()
	ctx, manager, store, workspaceRecord, _, _ := managerCoverageFixture(t, "repository")
	slot := testSlot(t, manager, string(workspaceRecord.ID), "direct-release", 1, "LEASED")
	session := state.Session{ID: slot.ID, WorkspaceID: string(workspaceRecord.ID), SlotID: slot.ID, State: "ACTIVE", AgentKind: "codex", LeaseKind: state.LeaseKindPath, LeaseOwnerPID: 99999999, TokenHash: state.HashToken(slot.ID)}
	if _, err := store.CreateSlotSession(ctx, slot, nil, session, ""); err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	manager.log = slog.New(slog.NewTextHandler(&logs, nil))
	manager.releaseExitedDirectLeases(ctx)
	if strings.Contains(logs.String(), "lease release failed") {
		t.Fatalf("successful direct lease release was logged as failed: %s", logs.String())
	}
	stored, err := store.SessionByID(ctx, session.ID)
	if err != nil || stored.State != "RELEASING" {
		t.Fatalf("direct lease state=%s err=%v, want RELEASING", stored.State, err)
	}
}
