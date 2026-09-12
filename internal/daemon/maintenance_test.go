package daemon

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/HappyOnigiri/WX/internal/config"
	"github.com/HappyOnigiri/WX/internal/diag"
	"github.com/HappyOnigiri/WX/internal/discovery"
	"github.com/HappyOnigiri/WX/internal/state"
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
	cfg.Discovery.ReconcileInterval.Duration = 10 * time.Millisecond
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

	if err := m.Forget(ctx, repository); err == nil {
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

	if err := m.Forget(ctx, repository); err != nil {
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
	if err := m.Forget(ctx, repository); err == nil {
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
	if err := m.Forget(ctx, repository); err != nil {
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
