package daemon

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/HappyOnigiri/WorktreeX/internal/config"
	"github.com/HappyOnigiri/WorktreeX/internal/discovery"
	"github.com/HappyOnigiri/WorktreeX/internal/gitx"
	"github.com/HappyOnigiri/WorktreeX/internal/state"
	"github.com/HappyOnigiri/WorktreeX/internal/workspace"
)

// 起動直後の recovery は、すでに LEASED になった session の補充だけを一度積む。
// slot・session の台帳を確認してから job を queue へ渡すため、再起動で通常貸出の補充が止まらない。
func TestReconcileStandbyReplenishmentsQueuesRecoveredEnsure(t *testing.T) {
	ctx, manager, store, workspaceRecord, _, _ := managerCoverageFixture(t, "repository")
	slot := testSlotRow(t, manager, string(workspaceRecord.ID), "recovered-lease", 1, "LEASED")
	session := state.Session{
		ID:          "recovered-session",
		WorkspaceID: string(workspaceRecord.ID),
		SlotID:      slot.ID,
		State:       "ACTIVE",
		AgentKind:   "codex",
		TokenHash:   state.HashToken("recovered-token"),
	}
	if _, err := store.CreateSlotSession(ctx, slot, nil, session, ""); err != nil {
		t.Fatal(err)
	}

	manager.reconcileStandbyReplenishments(ctx)
	jobs, err := store.RecoverJobs(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 || jobs[0].Kind != "ENSURE_STANDBY" || jobs[0].WorkspaceID != string(workspaceRecord.ID) {
		t.Fatalf("recovered jobs=%+v, want one ENSURE_STANDBY for %s", jobs, workspaceRecord.ID)
	}
	work, execution, ok := manager.jobQueue.take()
	if !ok || work.id != jobs[0].ID || work.class != jobClassMaintenance {
		t.Fatalf("queued recovery work=%+v ok=%t, want ENSURE_STANDBY %s in maintenance queue", work, ok, jobs[0].ID)
	}
	manager.jobQueue.finish(work, execution)
}

func TestWorkspaceConfigurationChangedDetectsGenerationBoundary(t *testing.T) {
	base := discovery.Workspace{
		ID:   "workspace",
		Root: "/workspaces/example",
		Kind: "repository",
		Repositories: []discovery.Repository{{
			ID: "repository", RelativePath: ".",
		}},
	}
	membershipChanged := base
	membershipChanged.Repositories = []discovery.Repository{{ID: "other", RelativePath: "."}}
	for _, test := range []struct {
		name               string
		latest             discovery.Workspace
		previousGeneration int
		latestGeneration   int
		want               bool
	}{
		{name: "unchanged", latest: base, previousGeneration: 1, latestGeneration: 1, want: false},
		{name: "generation advanced", latest: base, previousGeneration: 1, latestGeneration: 2, want: true},
		{name: "membership changed", latest: membershipChanged, previousGeneration: 1, latestGeneration: 1, want: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := workspaceConfigurationChanged(base, test.latest, test.previousGeneration, test.latestGeneration); got != test.want {
				t.Fatalf("workspaceConfigurationChanged()=%t, want %t", got, test.want)
			}
		})
	}
}

// idle 更新の予約が貸出に先を越されたときは、次の候補へ進めるため競合を成功扱いにする。
func TestStartIdleStandbyUpdateTreatsLostReservationAsNoop(t *testing.T) {
	f := newReuseStandbyFixture(t)
	ctx := context.Background()
	slot := f.readyStandby(t)
	f.advanceMain(t)
	resolved, err := f.manager.resolveBranches(ctx, f.workspace, nil)
	if err != nil {
		t.Fatal(err)
	}
	database := openTestDatabase(t, filepath.Join(f.root, "state.db"))
	// planStandbyUpdate は READY の実体を検証できるが、予約直前に別の条件が変わった状態を再現する。
	if _, err := database.ExecContext(ctx, `UPDATE slots SET placement_history_complete=0 WHERE id=?`, slot.ID); err != nil {
		t.Fatal(err)
	}

	started, err := f.manager.startIdleStandbyUpdate(ctx, f.workspace, slot, resolved)
	if err != nil || started {
		t.Fatalf("lost reservation started=%t err=%v, want false,nil", started, err)
	}
	if updates := f.pendingUpdateJobs(t); len(updates) != 0 {
		t.Fatalf("update jobs=%+v, want none after reservation race", updates)
	}
}

// idle 更新は一巡につき1 slotだけを予約し、残りの READY 枠を貸出可能なまま保つ。
func TestRefreshIdleStandbysUpdatesOneCandidatePerPass(t *testing.T) {
	f := newReuseStandbyFixtureWithWarmCount(t, 2, initGitRepo)
	ctx := context.Background()
	f.advanceMain(t)
	resolved, err := f.manager.resolveBranches(ctx, f.workspace, nil)
	if err != nil {
		t.Fatal(err)
	}

	f.manager.refreshIdleStandbys(ctx, f.workspace, resolved)
	updates := f.pendingUpdateJobs(t)
	if len(updates) != 1 {
		t.Fatalf("update jobs=%+v, want one reservation per pass", updates)
	}
	ready, err := f.store.ReadySlots(ctx, string(f.workspace.ID))
	if err != nil {
		t.Fatal(err)
	}
	if len(ready) != 1 {
		t.Fatalf("ready slots=%+v, want one slot left READY", ready)
	}
	if got, err := f.store.Slot(ctx, updates[0].SlotID); err != nil || got.State != "PREPARING" {
		t.Fatalf("reserved slot=%+v err=%v, want PREPARING", got, err)
	}
}

// 更新計画は最初に見つけた不一致の理由を予約後のログへ持ち回る。
func TestPlanStandbyUpdateRecordsFirstMismatch(t *testing.T) {
	f := newReuseStandbyFixture(t)
	ctx := context.Background()
	slot := f.readyStandby(t)
	f.advanceMain(t)
	resolved, err := f.manager.resolveBranches(ctx, f.workspace, nil)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := f.manager.planStandbyUpdate(ctx, f.workspace, slot, resolved, nil)
	if err != nil {
		t.Fatal(err)
	}
	if plan.mismatch.reason == "" {
		t.Fatalf("plan=%+v, want a mismatch reason", plan)
	}
}

// multi_repository の更新計画は root 実体も検査する。欠損した slot を予約へ進めない。
func TestPlanStandbyUpdateRejectsMissingMultiRepositoryRoot(t *testing.T) {
	ctx, manager, _, workspaceRecord, _, _ := managerCoverageFixture(t, "multi_repository")
	slot := testSlotRow(t, manager, string(workspaceRecord.ID), "missing-multi-root", 1, "READY")
	slot.PlacementHistoryComplete = true
	if _, err := manager.planStandbyUpdate(ctx, workspaceRecord, slot, nil, nil); err == nil {
		t.Fatal("multi-repository plan accepted a missing slot root")
	}
}

func TestStandbyStateRaceRecognizesOnlyContention(t *testing.T) {
	wrappedIneligible := fmt.Errorf("reservation changed: %w", state.ErrSlotStateIneligible)
	wrappedNotUpdateable := fmt.Errorf("reservation changed: %w", state.ErrStandbyNotUpdateable)
	for _, test := range []struct {
		name string
		err  error
		want bool
	}{
		{name: "nil", err: nil, want: false},
		{name: "generic", err: errors.New("database unavailable"), want: false},
		{name: "slot state", err: wrappedIneligible, want: true},
		{name: "standby update", err: wrappedNotUpdateable, want: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := standbyStateRace(test.err); got != test.want {
				t.Fatalf("standbyStateRace(%v)=%t, want %t", test.err, got, test.want)
			}
		})
	}
}

// repository workspace は root placement を持たないため、保存済み repository 状態だけで READY を維持できる。
func TestStandbyReadyUsableSkipsRepositoryRootValidation(t *testing.T) {
	ctx, manager, _, workspaceRecord, resolved, _ := managerCoverageFixture(t, "repository")
	slot := testSlotRow(t, manager, string(workspaceRecord.ID), "missing-repository-root", 1, "READY")
	slot.PlacementHistoryComplete = true
	storedWorkspace := workspaceRecord
	storedWorkspace.Repositories = nil

	usable, err := manager.standbyReadyUsable(ctx, slot, storedWorkspace, resolved, true)
	if err != nil || !usable {
		t.Fatalf("standbyReadyUsable=%t err=%v, want true,nil", usable, err)
	}
}

// 現在の main と完全一致する READY は、更新互換 fingerprint が欠けていてもそのまま使える。
// 保存済み状態の再検査へ進むと、legacy metadata を理由に不要な退役へ進んでしまう。
func TestStandbyReadyUsableKeepsExactMatchWithoutCompatibilityFingerprint(t *testing.T) {
	f := newReuseStandbyFixture(t)
	ctx := context.Background()
	slot := f.readyStandby(t)
	resolved, err := f.manager.resolveBranches(ctx, f.workspace, nil)
	if err != nil {
		t.Fatal(err)
	}
	database := openTestDatabase(t, filepath.Join(f.root, "state.db"))
	if _, err := database.ExecContext(ctx, `UPDATE slot_repositories SET compatibility_fingerprint='' WHERE slot_id=?`, slot.ID); err != nil {
		t.Fatal(err)
	}

	usable, err := f.manager.standbyReadyUsable(ctx, slot, f.workspace, resolved, true)
	if err != nil || !usable {
		t.Fatalf("standbyReadyUsable=%t err=%v, want true,nil for an exact READY match", usable, err)
	}
}

func TestRunStandbyUpdateSkipsCompletedJobsOnlyForMatchingContext(t *testing.T) {
	ctx, manager, store, workspaceRecord, _, databasePath := managerCoverageFixture(t, "repository")
	database := openTestDatabase(t, databasePath)
	cases := []struct {
		name       string
		slotState  string
		jobSession string
		wantSkip   bool
	}{
		{name: "leased idle replay", slotState: "LEASED", wantSkip: true},
		{name: "leased session replay", slotState: "LEASED", jobSession: "session", wantSkip: true},
		{name: "ready idle replay", slotState: "READY", wantSkip: true},
		{name: "ready leased context", slotState: "READY", jobSession: "session", wantSkip: false},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			slot := testSlotRow(t, manager, string(workspaceRecord.ID), testSlotID("completed-"+test.name), 1, test.slotState)
			if _, err := store.CreateStandby(ctx, slot, nil); err != nil {
				t.Fatal(err)
			}
			if _, err := database.ExecContext(ctx, `UPDATE slots SET update_completed_at=? WHERE id=?`, state.FormatTime(time.Now()), slot.ID); err != nil {
				t.Fatal(err)
			}
			job := state.Job{ID: "completed-" + testSlotID(test.name), Kind: "UPDATE", WorkspaceID: string(workspaceRecord.ID), SlotID: slot.ID, SessionID: test.jobSession}
			err := manager.runStandbyUpdate(ctx, job)
			if (err == nil) != test.wantSkip {
				t.Fatalf("runStandbyUpdate err=%v, want skip=%t", err, test.wantSkip)
			}
		})
	}
}

// 更新開始後に workspace を読めなくなった場合は PREPARING の実体を自動再利用せず隔離する。
func TestRunStandbyUpdateQuarantinesFailureAfterBegin(t *testing.T) {
	f := newReuseStandbyFixture(t)
	ctx := context.Background()
	slot := f.readyStandby(t)
	if err := f.store.SetSlotState(ctx, slot.ID, []string{"READY"}, "PREPARING", "TEST_UPDATE"); err != nil {
		t.Fatal(err)
	}
	job, err := f.store.CreateJob(ctx, "UPDATE", string(f.workspace.ID), slot.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	job.WorkspaceID = "missing-workspace"

	if err := f.manager.runStandbyUpdate(ctx, job); err == nil {
		t.Fatal("standby update with missing workspace succeeded")
	}
	got, err := f.store.Slot(ctx, slot.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != "QUARANTINED" || got.FailureCode != "UPDATE_FAILED" {
		t.Fatalf("failed standby=%+v, want QUARANTINED/UPDATE_FAILED", got)
	}
}

// 予約時に保存した copy mode は、daemon の現在設定が変わっても更新へ引き継ぐ。
func TestRunStandbyUpdateUsesPersistedCopyMode(t *testing.T) {
	f := newReuseStandbyFixture(t)
	ctx := context.Background()
	// loaded v2 config は present map を持つため、更新時に RepositoryDefaults へ入れた mode を resolver が保持する。
	home := t.TempDir()
	t.Setenv("HOME", home)
	configPath := filepath.Join(home, ".config", "wx", "config.yaml")
	if err := os.MkdirAll(filepath.Dir(configPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, []byte("version: 2\nrepository_defaults:\n  storage:\n    copy_mode: cow\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	base := f.manager.Config()
	loaded.System = base.System
	loaded.WorkspaceDefaults = base.WorkspaceDefaults
	loaded.Worktree = base.Worktree
	loaded.Storage = base.Storage
	loaded.Pool = base.Pool
	loaded.Workspaces = base.Workspaces
	loaded.Repositories = base.Repositories
	f.manager.mu.Lock()
	f.manager.cfg = loaded
	f.manager.mu.Unlock()
	slot := f.readyStandby(t)
	compatibility, err := workspace.UpdateCompatibilityFingerprint(slot.Generation, f.workspace.Repositories[0], f.manager.Config())
	if err != nil {
		t.Fatal(err)
	}
	database := openTestDatabase(t, filepath.Join(f.root, "state.db"))
	if _, err := database.ExecContext(ctx, `UPDATE slot_repositories SET compatibility_fingerprint=? WHERE slot_id=?`, compatibility, slot.ID); err != nil {
		t.Fatal(err)
	}
	f.advanceMain(t)
	resolved, err := f.manager.resolveBranches(ctx, f.workspace, nil)
	if err != nil {
		t.Fatal(err)
	}
	f.manager.refreshIdleStandbys(ctx, f.workspace, resolved)
	updates := f.pendingUpdateJobs(t)
	if len(updates) != 1 {
		t.Fatalf("update jobs=%+v, want one update", updates)
	}
	if _, err := database.ExecContext(ctx, `UPDATE slots SET update_copy_mode=? WHERE id=?`, config.CopyModeCopy, updates[0].SlotID); err != nil {
		t.Fatal(err)
	}
	storedSlot, err := f.store.Slot(ctx, updates[0].SlotID)
	if err != nil || storedSlot.UpdateCopyMode != config.CopyModeCopy {
		t.Fatalf("stored update copy mode=%q err=%v, want %q", storedSlot.UpdateCopyMode, err, config.CopyModeCopy)
	}

	if err := f.manager.runStandbyUpdate(ctx, updates[0]); err != nil {
		t.Fatal(err)
	}
	measurements := f.manager.PrepareMeasurements(updates[0].SlotID, "")
	if len(measurements) == 0 {
		t.Fatal("standby update recorded no measurement")
	}
	for _, phase := range measurements[0].Phases {
		if strings.HasPrefix(phase.Name, "cow") {
			t.Fatalf("persisted copy mode entered CoW phase %q", phase.Name)
		}
	}
	got, err := f.store.Slot(ctx, updates[0].SlotID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != "READY" {
		t.Fatalf("updated standby=%+v, want READY", got)
	}
}

// idle 更新の完了条件が変わった場合は、PREPARING の実体を隔離して再利用しない。
func TestRunIdleStandbyUpdateQuarantinesFinishRace(t *testing.T) {
	f := newReuseStandbyFixture(t)
	ctx := context.Background()
	f.advanceMain(t)
	resolved, err := f.manager.resolveBranches(ctx, f.workspace, nil)
	if err != nil {
		t.Fatal(err)
	}
	f.manager.refreshIdleStandbys(ctx, f.workspace, resolved)
	updates := f.pendingUpdateJobs(t)
	if len(updates) != 1 {
		t.Fatalf("update jobs=%+v, want one update", updates)
	}
	database := openTestDatabase(t, filepath.Join(f.root, "state.db"))
	if _, err := database.ExecContext(ctx, `UPDATE slots SET owner_session_id=? WHERE id=?`, "foreign-session", updates[0].SlotID); err != nil {
		t.Fatal(err)
	}

	if err := f.manager.runStandbyUpdate(ctx, updates[0]); err == nil {
		t.Fatal("idle standby update succeeded after its owner changed")
	}
	got, err := f.store.Slot(ctx, updates[0].SlotID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != "QUARANTINED" || got.FailureCode != "UPDATE_FAILED" {
		t.Fatalf("failed idle standby=%+v, want QUARANTINED/UPDATE_FAILED", got)
	}
}

// 貸出付き更新の完了時に session が想定外の状態なら、更新を成功扱いにせず隔離する。
func TestRunLeasedStandbyUpdateQuarantinesFinishRace(t *testing.T) {
	f := newReuseStandbyFixture(t)
	ctx := context.Background()
	f.advanceMain(t)
	lease, err := f.manager.ResolveAndLease(ctx, f.repository, nil, "codex", os.Getpid(), leaseAttrs{})
	if err != nil {
		t.Fatal(err)
	}
	if lease.Route != RouteUpdate {
		t.Fatalf("lease route=%q, want update", lease.Route)
	}
	jops, err := f.store.RecoverJobs(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	var update state.Job
	for _, job := range jops {
		if job.Kind == "UPDATE" && job.SessionID == lease.SessionID {
			update = job
			break
		}
	}
	if update.ID == "" {
		t.Fatalf("jobs=%+v, want leased UPDATE", jops)
	}
	database := openTestDatabase(t, filepath.Join(f.root, "state.db"))
	if _, err := database.ExecContext(ctx, `UPDATE sessions SET state='EXPIRED' WHERE id=?`, lease.SessionID); err != nil {
		t.Fatal(err)
	}

	if err := f.manager.runStandbyUpdate(ctx, update); err == nil {
		t.Fatal("leased standby update succeeded with an expired session")
	}
	got, err := f.store.Slot(ctx, update.SlotID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != "QUARANTINED" || got.FailureCode != "UPDATE_FAILED" {
		t.Fatalf("failed leased standby=%+v, want QUARANTINED/UPDATE_FAILED", got)
	}
}

// multi_repository の root placement も更新の公開対象である。repository だけを更新すると root 資産が古いまま残る。
func TestRunIdleStandbyUpdateSyncsMultiRepositoryRoot(t *testing.T) {
	f := newReuseStandbyFixture(t)
	ctx := context.Background()
	database := openTestDatabase(t, filepath.Join(f.root, "state.db"))
	if _, err := database.ExecContext(ctx, `UPDATE workspaces SET kind='multi_repository' WHERE id=?`, f.workspace.ID); err != nil {
		t.Fatal(err)
	}
	f.workspace.Kind = "multi_repository"
	if err := os.WriteFile(filepath.Join(f.repository, "AGENTS.md"), []byte("updated root asset\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f.advanceMain(t)
	resolved, err := f.manager.resolveBranches(ctx, f.workspace, nil)
	if err != nil {
		t.Fatal(err)
	}
	f.manager.refreshIdleStandbys(ctx, f.workspace, resolved)
	updates := f.pendingUpdateJobs(t)
	if len(updates) != 1 {
		t.Fatalf("update jobs=%+v, want one update", updates)
	}
	if err := f.manager.runStandbyUpdate(ctx, updates[0]); err != nil {
		t.Fatal(err)
	}
	slot, err := f.store.Slot(ctx, updates[0].SlotID)
	if err != nil {
		t.Fatal(err)
	}
	if slot.State != "READY" || slot.UpdateCompletedAt == "" {
		t.Fatalf("updated standby=%+v, want READY with a completion timestamp", slot)
	}
	got, err := os.ReadFile(filepath.Join(slot.Path, "AGENTS.md"))
	if err != nil || string(got) != "updated root asset\n" {
		t.Fatalf("updated root asset=%q err=%v, want the new content", got, err)
	}
}

// 複数 repository の更新区間は、実行中表示でも 1 始まりの対象番号を保つ。
func TestRunStandbyUpdateScopesRepositoryIndexFromOne(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	bundle := filepath.Join(root, "bundle")
	initGitRepo(t, filepath.Join(bundle, "server"))
	initGitRepo(t, filepath.Join(bundle, "client"))
	cfg := config.Defaults()
	cfg.Storage.WorktreeRoot = filepath.Join(root, "worktrees")
	cfg.Worktree.Undefined = "hot"
	cfg.Pool.WarmPerWorkspace = 1
	runner := &gitx.Runner{Timeout: 30 * time.Second}
	ctx := context.Background()
	workspaceRecord, err := (&discovery.Discoverer{Git: runner, Config: cfg}).Resolve(ctx, bundle)
	if err != nil {
		t.Fatal(err)
	}
	store, err := openTestStoreAtPath(t, filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	workspaceRecord, _, err = store.UpsertWorkspaceGeneration(ctx, workspaceRecord)
	if err != nil {
		t.Fatal(err)
	}
	manager := testManager(t, cfg, store)
	manager.git = runner
	t.Cleanup(manager.Close)
	databasePath := filepath.Join(root, "state.db")
	database := openTestDatabase(t, databasePath)
	if _, err := database.ExecContext(ctx, `UPDATE repositories SET last_leased_at=?`, state.FormatTime(time.Now())); err != nil {
		t.Fatal(err)
	}
	base := manager.Config()
	manager.mu.Lock()
	manager.cfg = base
	manager.mu.Unlock()
	var prepareLogs bytes.Buffer
	manager.log = slog.New(slog.NewTextHandler(&prepareLogs, nil))
	if err := manager.ensureStandby(ctx, workspaceRecord); err != nil {
		t.Fatal(err)
	}
	jobs, err := store.RecoverJobs(ctx, false)
	if err != nil || len(jobs) != 1 || jobs[0].Kind != "PREPARE" {
		t.Fatalf("prepare jobs=%+v err=%v, want one PREPARE", jobs, err)
	}
	claimed, err := store.ClaimJob(ctx, jobs[0].ID, "mutation-test")
	if err != nil {
		t.Fatal(err)
	}
	preparedSlot, _ := store.Slot(ctx, claimed.SlotID)
	preparedRepos, _ := store.SlotRepositories(ctx, claimed.SlotID)
	if err := manager.runRecoveredJob(ctx, claimed); err != nil {
		t.Fatalf("%v slot=%+v repos=%+v\nlogs:\n%s", err, preparedSlot, preparedRepos, prepareLogs.String())
	}
	if err := store.FinishJob(ctx, claimed.ID, "mutation-test", nil); err != nil {
		t.Fatal(err)
	}
	ready, ok, err := store.ReadySlot(ctx, string(workspaceRecord.ID))
	if err != nil || !ok {
		t.Fatalf("ready=%+v ok=%t err=%v", ready, ok, err)
	}
	storedRepositories, err := store.SlotRepositories(ctx, ready.ID)
	if err != nil || len(storedRepositories) != 2 {
		t.Fatalf("stored repositories=%+v err=%v, want two repositories", storedRepositories, err)
	}
	firstRecord := workspaceRecord.Repositories[0]
	for _, repository := range workspaceRecord.Repositories {
		if string(repository.ID) == storedRepositories[0].RepositoryID {
			firstRecord = repository
			break
		}
	}
	base.Repositories = map[string]config.Repository{
		string(firstRecord.MainPath): {
			Prepare: config.Prepare{Command: []string{"sh", "-c", "sleep 2"}, Inputs: []string{"tracked.txt"}},
		},
	}
	manager.mu.Lock()
	manager.cfg = base
	manager.mu.Unlock()
	compatibility, err := workspace.UpdateCompatibilityFingerprint(ready.Generation, firstRecord, manager.Config())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.ExecContext(ctx, `UPDATE slot_repositories SET compatibility_fingerprint=? WHERE slot_id=? AND repository_id=?`, compatibility, ready.ID, firstRecord.ID); err != nil {
		t.Fatal(err)
	}
	firstRepository := string(firstRecord.MainPath)
	if err := os.WriteFile(filepath.Join(firstRepository, "tracked.txt"), []byte("updated first repository\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitRun(t, firstRepository, "add", "tracked.txt")
	gitRun(t, firstRepository, "commit", "-m", "advance first repository")
	lease, err := manager.ResolveAndLease(ctx, string(workspaceRecord.Root), nil, "codex", os.Getpid(), leaseAttrs{})
	if err != nil {
		t.Fatal(err)
	}
	if lease.Route != RouteUpdate {
		t.Fatalf("lease route=%q, want update", lease.Route)
	}
	jops, err := store.RecoverJobs(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	var update state.Job
	for _, job := range jops {
		if job.Kind == "UPDATE" && job.SessionID == lease.SessionID {
			update = job
			break
		}
	}
	if update.ID == "" {
		t.Fatalf("jobs=%+v, want leased UPDATE", jops)
	}
	checkoutStarted := make(chan struct{})
	allowCheckout := make(chan struct{})
	var checkoutOnce, releaseCheckoutOnce sync.Once
	releaseCheckout := func() { releaseCheckoutOnce.Do(func() { close(allowCheckout) }) }
	defer releaseCheckout()
	manager.git.SetBeforeRunAtHook(func(args []string) {
		if !slices.Contains(args, "checkout") {
			return
		}
		checkoutOnce.Do(func() {
			close(checkoutStarted)
			<-allowCheckout
		})
	})
	errCh := make(chan error, 1)
	go func() { errCh <- manager.runStandbyUpdate(ctx, update) }()
	select {
	case <-checkoutStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("standby update did not reach checkout")
	}
	active, running, ok := manager.ActivePhase(update.SlotID)
	if !running || !ok || active.Name != "update-checkout" || active.Scope.Index != 1 || active.Scope.Total != 2 {
		t.Fatalf("active phase=%+v running=%t ok=%t, want update-checkout 1/2", active, running, ok)
	}
	releaseCheckout()
	var prepareScopeIndex, prepareScopeTotal int
	waitUntil(t, 5*time.Second, func() bool {
		active, running, ok := manager.ActivePhase(update.SlotID)
		if !running || !ok || active.Name != "update-prepare-command" {
			return false
		}
		prepareScopeIndex, prepareScopeTotal = active.Scope.Index, active.Scope.Total
		return true
	})
	if runErr := <-errCh; runErr != nil {
		t.Fatal(runErr)
	}
	if prepareScopeIndex != 1 || prepareScopeTotal != 2 {
		t.Fatalf("prepare command scope=%d/%d, want 1/2", prepareScopeIndex, prepareScopeTotal)
	}
}

// 予約作成が失敗しても、正常に隔離できた場合は誤った二次エラーをログへ出さない。
func TestReserveStandbySlotDoesNotLogFalseQuarantineFailure(t *testing.T) {
	ctx, manager, store, workspaceRecord, _, _ := managerCoverageFixture(t, "repository")
	var logs bytes.Buffer
	manager.log = slog.New(slog.NewTextHandler(&logs, nil))
	_, rootID, err := manager.activeRoot()
	if err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside-slot")
	job, retry, err := manager.reserveStandbySlot(ctx, "outside-slot", rootID, "outside-slot", outside, string(workspaceRecord.ID), 1, 1, nil)
	if err == nil || retry || job.ID != "" {
		t.Fatalf("reserveStandbySlot job=%+v retry=%t err=%v, want allocation failure", job, retry, err)
	}
	got, err := store.Slot(ctx, "outside-slot")
	if err != nil {
		t.Fatal(err)
	}
	if got.State != "QUARANTINED" || got.FailureCode != "STANDBY_ALLOCATION_FAILED" {
		t.Fatalf("reserved slot=%+v, want QUARANTINED/STANDBY_ALLOCATION_FAILED", got)
	}
	if strings.Contains(logs.String(), "quarantine failed standby reservation failed") {
		t.Fatalf("logged a false quarantine failure: %s", logs.String())
	}
}

func TestScheduleStandbyJobSkipsUnreservedSlot(t *testing.T) {
	t.Parallel()
	manager := &Manager{ctx: context.Background(), jobQueue: newJobQueue(1)}
	defer manager.jobQueue.close()
	pending := func() int {
		interactive, _ := manager.jobQueue.counts(jobClassInteractive)
		maintenance, _ := manager.jobQueue.counts(jobClassMaintenance)
		return interactive + maintenance
	}

	manager.scheduleStandbyJob(state.Job{})
	if got := pending(); got != 0 {
		t.Fatalf("pending jobs after an unreserved slot=%d, want 0", got)
	}
	manager.scheduleStandbyJob(state.Job{ID: "standby-job", Kind: "PREPARE"})
	if got := pending(); got != 1 {
		t.Fatalf("pending jobs after a reserved slot=%d, want 1", got)
	}
}

func TestLeaseAfterStandbyUpdateFailureKeepsLeaseAndSchedulesReplenishment(t *testing.T) {
	t.Parallel()
	ctx, manager, store, workspaceRecord, _, _ := managerCoverageFixture(t, "repository")
	cfg := manager.Config()
	cfg.Worktree.Undefined = "hot"
	cfg.Pool.WarmPerWorkspace = 1
	cfg.Retention.HotStandby.Duration = time.Hour
	manager.mu.Lock()
	manager.cfg = cfg
	manager.mu.Unlock()
	if !manager.standbyReplenishmentEnabled(workspaceRecord) {
		t.Fatal("standby replenishment is disabled for the test workspace")
	}

	repository := workspaceRecord.Repositories[0]
	_, generation, err := store.WorkspaceWithGeneration(ctx, string(workspaceRecord.ID))
	if err != nil {
		t.Fatal(err)
	}
	slot := testSlot(t, manager, string(workspaceRecord.ID), "late-update-failure", generation, "PREPARING")
	initial := state.SlotRepository{RepositoryID: string(repository.ID), DirName: "repository", State: "READY", RequestedRef: "main", BaseOID: "old-oid", Fingerprint: "old-fingerprint", CompatibilityFingerprint: "compatible"}
	prepareJob, err := store.CreateStandby(ctx, slot, []state.SlotRepository{initial})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.ReplacePlacements(ctx, slot.ID, nil); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkReady(ctx, slot.ID); err != nil {
		t.Fatal(err)
	}
	claimedPrepare, err := store.ClaimJob(ctx, prepareJob.ID, "prepare")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.FinishJob(ctx, claimedPrepare.ID, "prepare", nil); err != nil {
		t.Fatal(err)
	}
	token := "late-update-failure-token"
	session := state.Session{ID: "late-update-failure-session", WorkspaceID: string(workspaceRecord.ID), State: "STARTING", AgentKind: "codex", TokenHash: state.HashToken(token)}
	target := state.SlotRepository{RepositoryID: string(repository.ID), RequestedRef: "main", BaseOID: "new-oid", Fingerprint: "new-fingerprint", CompatibilityFingerprint: "compatible", UpdateBaseOID: "old-oid", UpdateFingerprint: "old-fingerprint"}
	if _, err := store.ReserveStandbyUpdate(ctx, slot.ID, session, []state.SlotRepository{target}, nil, "copy"); err != nil {
		t.Fatal(err)
	}
	if err := store.BeginStandbyUpdate(ctx, slot.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkRepositoryUpdateRunning(ctx, slot.ID, string(repository.ID)); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkStandbyUpdateEarlyReady(ctx, slot.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordEarlyReadyPrepareFailure(ctx, slot.ID, "UPDATE_FAILED:late-check", "", "update-prepare-command"); err != nil {
		t.Fatal(err)
	}
	if err := manager.leaseAfterStandbyUpdateFailure(ctx, workspaceRecord, slot.ID); err != nil {
		t.Fatal(err)
	}
	leased, err := store.Slot(ctx, slot.ID)
	if err != nil || leased.State != "LEASED" || leased.FailureCode != "UPDATE_FAILED:late-check" || leased.EarlyReadyAt == "" || leased.UpdateCompletedAt == "" || leased.PlacementHistoryComplete {
		t.Fatalf("slot after late update failure=%+v err=%v, want a leased slot with failure and incomplete placement history", leased, err)
	}
	notice, err := manager.ClaimPrepareFailureNotice(ctx, session.ID, token)
	if err != nil || !strings.Contains(notice, leased.FailureCode) {
		t.Fatalf("first prepare notice=%q err=%v, want the recorded failure", notice, err)
	}
	if repeat, err := manager.ClaimPrepareFailureNotice(ctx, session.ID, token); err != nil || repeat != "" {
		t.Fatalf("prepare notice repeated=%q err=%v", repeat, err)
	}
	work, execution, ok := manager.jobQueue.take()
	if !ok || work.class != jobClassMaintenance {
		t.Fatalf("queued work=%+v ok=%t, want standby replenishment in maintenance queue", work, ok)
	}
	defer manager.jobQueue.finish(work, execution)
	jobs, err := store.RecoverJobs(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, job := range jobs {
		if job.ID == work.id && job.Kind == "ENSURE_STANDBY" && job.WorkspaceID == string(workspaceRecord.ID) {
			return
		}
	}
	t.Fatalf("queued work %s has no pending ENSURE_STANDBY job: %+v", work.id, jobs)
}
