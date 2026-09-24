package daemon

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/HappyOnigiri/WorktreeX/internal/config"
	"github.com/HappyOnigiri/WorktreeX/internal/diag"
	"github.com/HappyOnigiri/WorktreeX/internal/discovery"
	"github.com/HappyOnigiri/WorktreeX/internal/state"
)

func TestMaintenanceLoopHandlesReloadAndTimer(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	store, err := openTestStoreAtPath(t, filepath.Join(home, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	cfg := config.Defaults()
	cfg.Storage.WorktreeRoot = filepath.Join(home, "worktrees")
	cfg.Pool.WarmPerWorkspace = 0
	cfg.System.Discovery.ReconcileInterval.Duration = 10 * time.Millisecond
	if err := config.Save(cfg); err != nil {
		t.Fatal(err)
	}
	manager := testManager(t, cfg, store)
	manager.reloads = make(chan struct{}, 1)
	done := make(chan struct{})
	go func() {
		manager.maintainLifecycle()
		close(done)
	}()
	waitUntil(t, 2*time.Second, func() bool {
		manager.mu.RLock()
		defer manager.mu.RUnlock()
		return !manager.lastBackup.IsZero()
	})
	manager.reloads <- struct{}{}
	started := manager.started
	waitUntil(t, 2*time.Second, func() bool {
		manager.mu.RLock()
		defer manager.mu.RUnlock()
		return manager.lastReload.After(started)
	})
	manager.cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("maintenance loop did not stop")
	}
}

func TestForgetFailsClosedWhenAFailedSlotCannotBeRetired(t *testing.T) {
	t.Parallel()
	requireDaemonIntegration(t)
	root := t.TempDir()
	repository := filepath.Join(root, "repo")
	initGitRepo(t, repository)
	databasePath := filepath.Join(root, "state.db")
	store, err := openTestStoreAtPath(t, databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cfg := config.Defaults()
	cfg.Storage.WorktreeRoot = filepath.Join(root, "worktrees")
	cfg.Worktree.Undefined = "hot"
	cfg.Pool.WarmPerWorkspace = 1
	m := testManager(t, cfg, store)
	m.git.SetTimeout(10 * time.Second)
	defer m.Close()
	ctx := context.Background()

	discoverer := discovery.Discoverer{Git: m.git, Config: cfg}
	w, err := discoverer.Resolve(ctx, repository)
	if err != nil {
		t.Fatal(err)
	}
	w = registerTestWorkspace(t, store, w)
	raw := openTestDatabase(t, databasePath)
	if _, err := raw.ExecContext(ctx, `UPDATE repositories SET last_leased_at=?`, state.FormatTime(time.Now())); err != nil {
		t.Fatal(err)
	}
	if err := m.ensureStandby(ctx, w); err != nil {
		t.Fatal(err)
	}
	jobs, err := store.RecoverJobs(ctx, false)
	if err != nil || len(jobs) != 1 || jobs[0].Kind != "PREPARE" {
		t.Fatalf("standby jobs=%+v err=%v", jobs, err)
	}
	prepared, err := store.ClaimJob(ctx, jobs[0].ID, "test")
	if err != nil {
		t.Fatal(err)
	}
	if err := m.runRecoveredJob(ctx, prepared); err != nil {
		t.Fatal(err)
	}
	if err := store.FinishJob(ctx, prepared.ID, "test", nil); err != nil {
		t.Fatal(err)
	}
	ready, ok, err := store.ReadySlot(ctx, string(w.ID))
	if err != nil || !ok {
		t.Fatalf("ready slot=%+v ok=%v err=%v", ready, ok, err)
	}
	if _, err := raw.ExecContext(ctx, `UPDATE slots SET state='FAILED',rel_path=? WHERE id=?`, filepath.Join("..", "outside-slot"), ready.ID); err != nil {
		t.Fatal(err)
	}

	if _, err := m.Forget(ctx, repository, false); err == nil {
		t.Fatal("forget completed despite an unretirable FAILED slot")
	}
	if _, err := store.Workspace(ctx, string(w.ID)); err != nil {
		t.Fatalf("workspace was forgotten despite the failed retirement: %v", err)
	}
	slot, err := store.Slot(ctx, ready.ID)
	if err != nil || slot.State != "QUARANTINED" {
		t.Fatalf("slot with an unprovable path was not quarantined: slot=%+v err=%v", slot, err)
	}
	if _, statErr := os.Stat(ready.Path); statErr != nil {
		t.Fatalf("worktree of an unprovable slot path was removed: %v", statErr)
	}
}

func TestForgetRetiresFailedSlotBeforePermanentlyLeakingIt(t *testing.T) {
	t.Parallel()
	requireDaemonIntegration(t)
	root := t.TempDir()
	repository := filepath.Join(root, "repo")
	initGitRepo(t, repository)
	databasePath := filepath.Join(root, "state.db")
	store, err := openTestStoreAtPath(t, databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cfg := config.Defaults()
	cfg.Storage.WorktreeRoot = filepath.Join(root, "worktrees")
	cfg.Worktree.Undefined = "hot"
	cfg.Pool.WarmPerWorkspace = 1
	m := testManager(t, cfg, store)
	m.git.SetTimeout(10 * time.Second)
	defer m.Close()
	ctx := context.Background()

	discoverer := discovery.Discoverer{Git: m.git, Config: cfg}
	w, err := discoverer.Resolve(ctx, repository)
	if err != nil {
		t.Fatal(err)
	}
	w = registerTestWorkspace(t, store, w)
	raw := openTestDatabase(t, databasePath)
	if _, err := raw.ExecContext(ctx, `UPDATE repositories SET last_leased_at=?`, state.FormatTime(time.Now())); err != nil {
		t.Fatal(err)
	}
	if err := m.ensureStandby(ctx, w); err != nil {
		t.Fatal(err)
	}
	jobs, err := store.RecoverJobs(ctx, false)
	if err != nil || len(jobs) != 1 || jobs[0].Kind != "PREPARE" {
		t.Fatalf("standby jobs=%+v err=%v", jobs, err)
	}
	prepared, err := store.ClaimJob(ctx, jobs[0].ID, "test")
	if err != nil {
		t.Fatal(err)
	}
	if err := m.runRecoveredJob(ctx, prepared); err != nil {
		t.Fatal(err)
	}
	if err := store.FinishJob(ctx, prepared.ID, "test", nil); err != nil {
		t.Fatal(err)
	}
	ready, ok, err := store.ReadySlot(ctx, string(w.ID))
	if err != nil || !ok {
		t.Fatalf("ready slot=%+v ok=%v err=%v", ready, ok, err)
	}
	if _, err := raw.ExecContext(ctx, `UPDATE slots SET state='FAILED' WHERE id=?`, ready.ID); err != nil {
		t.Fatal(err)
	}

	if _, err := m.Forget(ctx, repository, false); err != nil {
		t.Fatalf("forget with a FAILED slot: %v", err)
	}
	if _, err := store.Workspace(ctx, string(w.ID)); err == nil {
		t.Fatal("workspace was not forgotten")
	}
	slot, err := store.Slot(ctx, ready.ID)
	if err != nil || slot.State != "ARCHIVED" {
		t.Fatalf("failed slot was not retired: slot=%+v err=%v", slot, err)
	}
	if _, statErr := os.Stat(ready.Path); !os.IsNotExist(statErr) {
		t.Fatalf("failed slot worktree was not removed: err=%v", statErr)
	}
}

// TestDiscardRecoveryRetiresQuarantinedSlotsAndUnblocksForget は recovery ref の欠損で
// 行き止まりになった workspace を CLI の操作だけで forget できることを確認する。
func TestDiscardRecoveryRetiresQuarantinedSlotsAndUnblocksForget(t *testing.T) {
	t.Parallel()
	requireDaemonIntegration(t)
	root := t.TempDir()
	repository := filepath.Join(root, "repo")
	initGitRepo(t, repository)
	databasePath := filepath.Join(root, "state.db")
	store, err := openTestStoreAtPath(t, databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cfg := config.Defaults()
	cfg.Storage.WorktreeRoot = filepath.Join(root, "worktrees")
	cfg.Worktree.Undefined = "hot"
	cfg.Pool.WarmPerWorkspace = 1
	m := testManager(t, cfg, store)
	m.git.SetTimeout(10 * time.Second)
	defer m.Close()
	ctx := context.Background()

	discoverer := discovery.Discoverer{Git: m.git, Config: cfg}
	w, err := discoverer.Resolve(ctx, repository)
	if err != nil {
		t.Fatal(err)
	}
	w = registerTestWorkspace(t, store, w)
	raw := openTestDatabase(t, databasePath)
	if _, err := raw.ExecContext(ctx, `UPDATE repositories SET last_leased_at=?`, state.FormatTime(time.Now())); err != nil {
		t.Fatal(err)
	}
	ready := prepareTestStandby(ctx, t, m, store, w)
	// 保存済みの返却を模す。snapshot は本物の ref を指す必要がないので、ref が消えた後の状態だけを作る。
	session := state.Session{ID: "session", WorkspaceID: string(w.ID), SlotID: ready.ID, State: "ARCHIVED", AgentKind: "codex", TokenHash: state.HashToken("token")}
	if _, err := raw.ExecContext(ctx, `INSERT INTO sessions(id,workspace_id,slot_id,state,agent_kind,session_token_hash,created_at,archived_at,expires_at) VALUES(?,?,?,?,?,?,?,?,?)`,
		session.ID, session.WorkspaceID, session.SlotID, session.State, session.AgentKind, session.TokenHash, state.FormatTime(time.Now()), state.FormatTime(time.Now()), state.FormatTime(time.Now().Add(time.Hour))); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveSnapshot(ctx, state.Snapshot{
		ID: "snapshot", SessionID: "session", RepositoryID: string(w.Repositories[0].ID),
		HeadOID: "head", HeadRef: "refs/wx/recovery/head", IndexTreeOID: "index", WorktreeOID: "worktree", WorktreeRef: "refs/wx/recovery/worktree",
		Status: "ARCHIVED", CreatedAt: state.FormatTime(time.Now()), ExpiresAt: state.FormatTime(time.Now().Add(time.Hour)),
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.QuarantineMissingRecoveryRef(ctx, "refs/wx/recovery/head"); err != nil {
		t.Fatal(err)
	}

	// 隔離すると ref の照合対象から外れるため、行き止まりを報告するのはこの finding だけである。
	if !hasFinding(m.Doctor(ctx).Findings, "can no longer be restored", diag.SeverityProblem) {
		t.Fatal("doctor did not report the quarantined recovery state")
	}

	dry, err := m.DiscardRecovery(ctx, repository, true)
	if err != nil || len(dry.Targets) != 1 || dry.Discarded != 0 || dry.Retired != 0 {
		t.Fatalf("dry run=%+v err=%v", dry, err)
	}
	if dry.Targets[0].SlotID != ready.ID || dry.Targets[0].SlotState != "QUARANTINED" || dry.Targets[0].Snapshots != 1 {
		t.Fatalf("dry run target=%+v", dry.Targets[0])
	}
	if _, statErr := os.Stat(ready.Path); statErr != nil {
		t.Fatalf("dry run removed the quarantined worktree: %v", statErr)
	}
	if _, err := m.Forget(ctx, repository, false); err == nil {
		t.Fatal("forget completed while the quarantined recovery state was still recorded")
	}

	discarded, err := m.DiscardRecovery(ctx, repository, false)
	if err != nil || discarded.Discarded != 1 || discarded.Retired != 1 {
		t.Fatalf("discard=%+v err=%v", discarded, err)
	}
	slot, err := store.Slot(ctx, ready.ID)
	if err != nil || slot.State != "ARCHIVED" {
		t.Fatalf("quarantined slot was not retired: slot=%+v err=%v", slot, err)
	}
	if _, statErr := os.Stat(ready.Path); !os.IsNotExist(statErr) {
		t.Fatalf("quarantined worktree was not removed: err=%v", statErr)
	}
	if _, err := m.Forget(ctx, repository, false); err != nil {
		t.Fatalf("forget after discarding the quarantined recovery state: %v", err)
	}
	if hasFinding(m.Doctor(ctx).Findings, "can no longer be restored", diag.SeverityProblem) {
		t.Fatal("doctor still reports the discarded recovery state")
	}
	// 破棄した workspace は登録が消えるため、続けて実行しても登録済み workspace としては扱わない。
	if _, err := m.DiscardRecovery(ctx, repository, true); err == nil {
		t.Fatal("discard-recovery accepted a path that is not a registered workspace")
	}
}

// TestForgetReclaimsStandbyWithoutReplenishingIt は、待機枠しか持たない workspace が
// フラグ無しの forget だけで解除でき、回収で空いた枠を補充が作り直さないことを固定する。
func TestForgetReclaimsStandbyWithoutReplenishingIt(t *testing.T) {
	t.Parallel()
	requireDaemonIntegration(t)
	fixture := newForgetFixture(t)
	ctx := context.Background()
	ready := prepareTestStandby(ctx, t, fixture.manager, fixture.store, fixture.workspace)

	result, err := fixture.manager.Forget(ctx, fixture.repository, false)
	if err != nil {
		t.Fatalf("forget with a READY standby: %v", err)
	}
	if result.ReclaimedSlots != 1 || result.DiscardedSessions != 0 {
		t.Fatalf("forget result=%+v, want one reclaimed standby and no discarded recovery state", result)
	}
	if _, err := fixture.store.Workspace(ctx, string(fixture.workspace.ID)); err == nil {
		t.Fatal("workspace is still registered after forget")
	}
	slot, err := fixture.store.Slot(ctx, ready.ID)
	if err != nil || slot.State != "ARCHIVED" {
		t.Fatalf("standby slot was not reclaimed: slot=%+v err=%v", slot, err)
	}
	if _, statErr := os.Stat(ready.Path); !os.IsNotExist(statErr) {
		t.Fatalf("standby worktree was not removed: err=%v", statErr)
	}
	// 補充が走り直していれば、この workspace の slot 行か PENDING の job が残る。
	var live int
	if err := fixture.raw.QueryRowContext(ctx, `SELECT count(*) FROM slots WHERE state<>'ARCHIVED'`).Scan(&live); err != nil || live != 0 {
		t.Fatalf("slots outside ARCHIVED after forget: count=%d err=%v", live, err)
	}
	if err := fixture.raw.QueryRowContext(ctx, `SELECT count(*) FROM jobs WHERE state IN ('PENDING','RUNNING')`).Scan(&live); err != nil || live != 0 {
		t.Fatalf("jobs left pending after forget: count=%d err=%v", live, err)
	}
}

// TestForgetDiscardsRecoveryOnlyWhenAsked は、復元資産の破棄が --discard-recovery でだけ起きること、
// 断るときの案内がそのフラグを指すこと、破棄では ref と保存ファイルまで消えることを固定する。
func TestForgetDiscardsRecoveryOnlyWhenAsked(t *testing.T) {
	t.Parallel()
	requireDaemonIntegration(t)
	fixture := newForgetFixture(t)
	ctx := context.Background()
	recovery := fixture.seedRecovery(ctx, t)

	if _, refuseErr := fixture.manager.Forget(ctx, fixture.repository, false); !errors.Is(refuseErr, state.ErrWorkspaceHasRecovery) || !strings.Contains(refuseErr.Error(), "--discard-recovery") {
		t.Fatalf("forget error=%v, want a refusal that points at --discard-recovery", refuseErr)
	}
	if _, statErr := os.Stat(recovery.slot.Path); statErr != nil {
		t.Fatalf("refused forget removed the saved worktree: %v", statErr)
	}
	if _, statErr := os.Stat(recovery.archivePath); statErr != nil {
		t.Fatalf("refused forget removed the workspace snapshot: %v", statErr)
	}

	result, discardErr := fixture.manager.Forget(ctx, fixture.repository, true)
	if discardErr != nil {
		t.Fatalf("forget --discard-recovery: %v", discardErr)
	}
	if result.DiscardedSessions != 1 || result.DiscardedSnapshots != 1 || result.DiscardedWorkspaceSnapshots != 1 || result.ReclaimedSlots != 1 {
		t.Fatalf("forget result=%+v, want the recorded recovery state to be discarded", result)
	}
	if _, err := fixture.store.Workspace(ctx, string(fixture.workspace.ID)); err == nil {
		t.Fatal("workspace is still registered after forget")
	}
	fixture.requireNoRecoveryRows(ctx, t)
	if _, statErr := os.Stat(recovery.slot.Path); !os.IsNotExist(statErr) {
		t.Fatalf("saved worktree was not removed: err=%v", statErr)
	}
	if _, statErr := os.Stat(recovery.archivePath); !os.IsNotExist(statErr) {
		t.Fatalf("workspace snapshot was not removed: err=%v", statErr)
	}
	if refs := gitOutput(t, fixture.repository, "for-each-ref", "--format=%(refname)", "refs/wx/"); strings.TrimSpace(refs) != "" {
		t.Fatalf("recovery refs left behind: %q", refs)
	}
}

// TestForgetDiscardsRecoveryWhenTheSourceRepositoryIsGone は、ref を消せない登録でも解除が終わることを固定する。
// root ごと消えた workspace ではこの削除が必ず失敗し、止めると登録を消す経路が無くなる。
func TestForgetDiscardsRecoveryWhenTheSourceRepositoryIsGone(t *testing.T) {
	t.Parallel()
	requireDaemonIntegration(t)
	fixture := newForgetFixture(t)
	ctx := context.Background()
	recovery := fixture.seedRecovery(ctx, t)
	if err := os.RemoveAll(fixture.repository); err != nil {
		t.Fatal(err)
	}

	if _, err := fixture.manager.Forget(ctx, fixture.repository, true); err != nil {
		t.Fatalf("forget --discard-recovery without the source repository: %v", err)
	}
	if _, err := fixture.store.WorkspaceByRoot(ctx, fixture.repository); err == nil {
		t.Fatal("workspace is still registered after forget")
	}
	fixture.requireNoRecoveryRows(ctx, t)
	if _, statErr := os.Stat(recovery.slot.Path); !os.IsNotExist(statErr) {
		t.Fatalf("saved worktree was not removed: err=%v", statErr)
	}
}

// forgetFixture は forget のテストが共有する repository・store・manager の組である。
type forgetFixture struct {
	repository string
	store      *state.Store
	raw        *sql.DB
	manager    *Manager
	workspace  discovery.Workspace
}

// forgetRecovery は seedRecovery が作った復元資産の位置である。
type forgetRecovery struct {
	slot        state.Slot
	archivePath string
}

func newForgetFixture(t *testing.T) forgetFixture {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	repository := filepath.Join(root, "repo")
	initGitRepo(t, repository)
	databasePath := filepath.Join(root, "state.db")
	store, err := openTestStoreAtPath(t, databasePath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	cfg := config.Defaults()
	cfg.Storage.WorktreeRoot = filepath.Join(root, "worktrees")
	cfg.Worktree.Undefined = "hot"
	cfg.Pool.WarmPerWorkspace = 1
	manager := testManager(t, cfg, store)
	manager.git.SetTimeout(10 * time.Second)
	t.Cleanup(manager.Close)
	ctx := context.Background()
	discoverer := discovery.Discoverer{Git: manager.git, Config: cfg}
	w, err := discoverer.Resolve(ctx, repository)
	if err != nil {
		t.Fatal(err)
	}
	w = registerTestWorkspace(t, store, w)
	raw := openTestDatabase(t, databasePath)
	if _, err := raw.ExecContext(ctx, `UPDATE repositories SET last_leased_at=?`, state.FormatTime(time.Now())); err != nil {
		t.Fatal(err)
	}
	return forgetFixture{repository: repository, store: store, raw: raw, manager: manager, workspace: w}
}

// seedRecovery は保存済みの返却を模し、slot・session・snapshot・recovery ref・保存ファイルを揃える。
func (f forgetFixture) seedRecovery(ctx context.Context, t *testing.T) forgetRecovery {
	t.Helper()
	slot := prepareTestStandby(ctx, t, f.manager, f.store, f.workspace)
	head := strings.TrimSpace(gitOutput(t, f.repository, "rev-parse", "HEAD"))
	gitRun(t, f.repository, "update-ref", "refs/wx/recovery/head", head)
	gitRun(t, f.repository, "update-ref", "refs/wx/recovery/worktree", head)
	at := state.FormatTime(time.Now())
	expiry := state.FormatTime(time.Now().Add(time.Hour))
	if _, err := f.raw.ExecContext(ctx, `INSERT INTO sessions(id,workspace_id,slot_id,state,agent_kind,session_token_hash,created_at,archived_at,expires_at) VALUES('session',?,?,'ARCHIVED','codex',?,?,?,?)`,
		string(f.workspace.ID), slot.ID, state.HashToken("token"), at, at, expiry); err != nil {
		t.Fatal(err)
	}
	if _, err := f.raw.ExecContext(ctx, `UPDATE slots SET state='SNAPSHOTTED',owner_session_id='session' WHERE id=?`, slot.ID); err != nil {
		t.Fatal(err)
	}
	if err := f.store.SaveSnapshot(ctx, state.Snapshot{
		ID: "snapshot", SessionID: "session", RepositoryID: string(f.workspace.Repositories[0].ID),
		HeadOID: head, HeadRef: "refs/wx/recovery/head", IndexTreeOID: head, WorktreeOID: head, WorktreeRef: "refs/wx/recovery/worktree",
		Status: "ARCHIVED", CreatedAt: at, ExpiresAt: expiry,
	}); err != nil {
		t.Fatal(err)
	}
	if err := f.store.SaveWorkspaceSnapshot(ctx, state.WorkspaceSnapshot{
		SessionID: "session", RootID: slot.RootID, RelPath: filepath.Join("_recovery", "workspace-snapshots", "session.tar"),
		SHA256: strings.Repeat("a", 64), Status: "ARCHIVED", CreatedAt: at, ExpiresAt: expiry,
	}); err != nil {
		t.Fatal(err)
	}
	saved, found, err := f.store.WorkspaceSnapshot(ctx, "session")
	if err != nil || !found {
		t.Fatalf("workspace snapshot record found=%v err=%v", found, err)
	}
	if err := os.MkdirAll(filepath.Dir(saved.ArchivePath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(saved.ArchivePath, []byte("archive"), 0o600); err != nil {
		t.Fatal(err)
	}
	return forgetRecovery{slot: slot, archivePath: saved.ArchivePath}
}

// requireNoRecoveryRows は破棄後に復元資産の行が残っていないことを確かめる。
func (f forgetFixture) requireNoRecoveryRows(ctx context.Context, t *testing.T) {
	t.Helper()
	for _, table := range []string{"snapshots", "workspace_snapshots"} {
		var rows int
		if err := f.raw.QueryRowContext(ctx, `SELECT count(*) FROM `+table).Scan(&rows); err != nil || rows != 0 {
			t.Fatalf("%s rows after discarding the recovery state: count=%d err=%v", table, rows, err)
		}
	}
	var live int
	if err := f.raw.QueryRowContext(ctx, `SELECT count(*) FROM sessions WHERE state<>'EXPIRED'`).Scan(&live); err != nil || live != 0 {
		t.Fatalf("sessions outside EXPIRED after forget: count=%d err=%v", live, err)
	}
}

// hasFinding は summary の一部と severity が一致する finding があるかを返す。
func hasFinding(findings []diag.Finding, fragment string, severity diag.Severity) bool {
	for _, finding := range findings {
		if finding.Severity == severity && strings.Contains(finding.Summary, fragment) {
			return true
		}
	}
	return false
}

// prepareTestStandby は待機枠 1 つを READY まで進めて返す。
func prepareTestStandby(ctx context.Context, t *testing.T, m *Manager, store *state.Store, w discovery.Workspace) state.Slot {
	t.Helper()
	if err := m.ensureStandby(ctx, w); err != nil {
		t.Fatal(err)
	}
	jobs, err := store.RecoverJobs(ctx, false)
	if err != nil || len(jobs) != 1 || jobs[0].Kind != "PREPARE" {
		t.Fatalf("standby jobs=%+v err=%v", jobs, err)
	}
	prepared, err := store.ClaimJob(ctx, jobs[0].ID, "test")
	if err != nil {
		t.Fatal(err)
	}
	if err := m.runRecoveredJob(ctx, prepared); err != nil {
		t.Fatal(err)
	}
	if err := store.FinishJob(ctx, prepared.ID, "test", nil); err != nil {
		t.Fatal(err)
	}
	ready, ok, err := store.ReadySlot(ctx, string(w.ID))
	if err != nil || !ok {
		t.Fatalf("ready slot=%+v ok=%v err=%v", ready, ok, err)
	}
	return ready
}

// TestForgetRemovesRegistrationWhoseRootIsGone は root ごと消えた登録も forget で解除できることを固定する。
// doctor は root を解決できない登録にも forget を案内し、登録を消す経路は forget しかないため、
// canonical 化できない path を一律に拒むとその登録には出口が無くなる。
func TestForgetRemovesRegistrationWhoseRootIsGone(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	store, err := openTestStoreAtPath(t, filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	cfg := config.Defaults()
	cfg.Storage.WorktreeRoot = filepath.Join(root, "worktrees")
	manager := testManager(t, cfg, store)
	t.Cleanup(manager.Close)
	workspaceRoot := filepath.Join(root, "gone")
	if err := os.MkdirAll(workspaceRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	repository := discovery.Repository{ID: "repository", MainPath: discoveryPath(workspaceRoot), CommonDir: discoveryPath(filepath.Join(workspaceRoot, ".git")), DefaultBranch: "main"}
	registerTestWorkspace(t, store, discovery.Workspace{ID: "workspace", Root: discoveryPath(workspaceRoot), Kind: "repository", Repositories: []discovery.Repository{repository}})
	if err := os.RemoveAll(workspaceRoot); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Forget(ctx, workspaceRoot, false); err != nil {
		t.Fatalf("Forget error=%v, want the registration to be removed", err)
	}
	if _, err := store.WorkspaceByRoot(ctx, workspaceRoot); err == nil {
		t.Fatal("workspace is still registered after forget")
	}
	// 登録の無い path は実体が無くても受け付けない。打ち間違えを黙って成功させないためである。
	if _, err := manager.Forget(ctx, filepath.Join(root, "never-registered"), false); err == nil {
		t.Fatal("unregistered path was forgotten")
	}
}
