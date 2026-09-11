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
	"github.com/HappyOnigiri/WX/internal/gitx"
	"github.com/HappyOnigiri/WX/internal/state"
)

func TestPlacementsForSeparatesWorkspaceRoot(t *testing.T) {
	placements := []state.Placement{{RepositoryID: "repository", RelativePath: "repo"}, {RelativePath: "root"}}
	if got := placementsFor(placements, ""); len(got) != 1 || got[0].RelativePath != "root" {
		t.Fatalf("root placements=%+v", got)
	}
}

// reuseStandbyFixture は貸出時の更新判定を確かめる最小構成で、READY standbyを1件持つ。
// worker を起動しないため、job は runPendingJobs で手動に進める。
type reuseStandbyFixture struct {
	root       string
	repository string
	store      *state.Store
	manager    *Manager
	workspace  discovery.Workspace
}

func newReuseStandbyFixture(t *testing.T) *reuseStandbyFixture {
	t.Helper()
	return newReuseStandbyFixtureWith(t, initGitRepo)
}

// newReuseStandbyFixtureWith は main repository の作り方だけを差し替えられる形で fixture を組む。
func newReuseStandbyFixtureWith(t *testing.T, initRepository func(*testing.T, string)) *reuseStandbyFixture {
	t.Helper()
	requireDaemonIntegration(t)
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	repository := filepath.Join(root, "repo")
	initRepository(t, repository)
	databasePath := filepath.Join(root, "state.db")
	store, err := state.Open(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	cfg := config.Defaults()
	cfg.Storage.WorktreeRoot = filepath.Join(root, "worktrees")
	cfg.Worktree.Undefined = "hot"
	cfg.Pool.WarmPerWorkspace = 1
	m := testManager(t, cfg, store)
	m.git = &gitx.Runner{Timeout: 30 * time.Second}
	t.Cleanup(m.Close)
	ctx := context.Background()
	discoverer := discovery.Discoverer{Git: m.git, Config: cfg}
	w, err := discoverer.Resolve(ctx, repository)
	if err != nil {
		t.Fatal(err)
	}
	w = registerTestWorkspace(t, store, w)
	raw := openTestDatabase(t, databasePath)
	// 補充を hot と判定させるため、貸出実績を持つ repository として扱う。
	if _, err := raw.ExecContext(ctx, `UPDATE repositories SET last_leased_at=?`, state.FormatTime(time.Now())); err != nil {
		t.Fatal(err)
	}
	f := &reuseStandbyFixture{root: root, repository: repository, store: store, manager: m, workspace: w}
	if err := m.ensureStandby(ctx, w); err != nil {
		t.Fatal(err)
	}
	f.runPendingJobs(t)
	return f
}

// runPendingJobs は保留中のjobがなくなるまで手動で実行する。補充のENSURE_STANDBYが積むPREPAREもこの巡で進む。
func (f *reuseStandbyFixture) runPendingJobs(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	for round := 0; round < 8; round++ {
		jobs, err := f.store.RecoverJobs(ctx, false)
		if err != nil {
			t.Fatal(err)
		}
		if len(jobs) == 0 {
			return
		}
		for _, job := range jobs {
			claimed, err := f.store.ClaimJob(ctx, job.ID, "test")
			if err != nil {
				t.Fatal(err)
			}
			runErr := f.manager.runRecoveredJob(ctx, claimed)
			if err := f.store.FinishJob(ctx, claimed.ID, "test", runErr); err != nil {
				t.Fatal(err)
			}
			if runErr != nil {
				t.Fatalf("job %s kind=%s: %v", job.ID, job.Kind, runErr)
			}
		}
	}
	t.Fatal("pending jobs did not settle")
}

// runPendingJobsOfKind は指定したkindの保留中jobだけを実行する。
// 削除の後始末のように、続けて走る補充が同じpathへ新しいslotを作ると観測できなくなる結果を確かめるために使う。
func (f *reuseStandbyFixture) runPendingJobsOfKind(t *testing.T, kind string) {
	t.Helper()
	ctx := context.Background()
	jobs, err := f.store.RecoverJobs(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	ran := 0
	for _, job := range jobs {
		if job.Kind != kind {
			continue
		}
		claimed, err := f.store.ClaimJob(ctx, job.ID, "test")
		if err != nil {
			t.Fatal(err)
		}
		runErr := f.manager.runRecoveredJob(ctx, claimed)
		if err := f.store.FinishJob(ctx, claimed.ID, "test", runErr); err != nil {
			t.Fatal(err)
		}
		if runErr != nil {
			t.Fatalf("job %s kind=%s: %v", job.ID, job.Kind, runErr)
		}
		ran++
	}
	if ran == 0 {
		t.Fatalf("no pending %s job", kind)
	}
}

// settleStandby は補充が要る状態を解消する。GCがSTALE slotを畳む間は待機枠が埋まって見えるため、
// 実daemonの周期reconcileと同じくensureStandbyをもう一度通す。
func (f *reuseStandbyFixture) settleStandby(t *testing.T) {
	t.Helper()
	f.runPendingJobs(t)
	if err := f.manager.ensureStandby(context.Background(), f.workspace); err != nil {
		t.Fatal(err)
	}
	f.runPendingJobs(t)
}

func (f *reuseStandbyFixture) readyStandby(t *testing.T) state.Slot {
	t.Helper()
	ready, ok, err := f.store.ReadySlot(context.Background(), string(f.workspace.ID))
	if err != nil || !ok {
		t.Fatalf("ready standby ok=%t err=%v", ok, err)
	}
	return ready
}

func (f *reuseStandbyFixture) slotState(t *testing.T, id string) string {
	t.Helper()
	slot, err := f.store.Slot(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return slot.State
}

// requireRetired は standby が READY から退役したことを確かめる。
// cold startの貸出は背景GCを起動し、そのGCはSTALE slotを即座にREMOVINGへ進めるため、
// 観測できる状態は呼出しとGCの前後関係で変わる。どちらでも退役の判定は満たす。
func (f *reuseStandbyFixture) requireRetired(t *testing.T, id string) {
	t.Helper()
	switch got := f.slotState(t, id); got {
	case "STALE", "REMOVING":
	default:
		t.Fatalf("retired standby state=%s, want STALE or REMOVING", got)
	}
}

// commitChangedAttributes は更新不適格になる`.gitattributes`の変更をmainへ積む。
func (f *reuseStandbyFixture) commitChangedAttributes(t *testing.T) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(f.repository, ".gitattributes"), []byte("*.txt text eol=crlf\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitRun(t, f.repository, "add", ".gitattributes")
	gitRun(t, f.repository, "commit", "-m", "add attributes")
}

// dirtyStandbyTracked は standby の worktree にある tracked file を書き換え、READY 検証で棄却される状態にする。
// 利用者や外部の道具が待機中の worktree を触った状況を再現する。
func (f *reuseStandbyFixture) dirtyStandbyTracked(t *testing.T, slotID string) {
	t.Helper()
	repositoryState, err := f.store.SlotRepository(context.Background(), slotID, string(f.workspace.Repositories[0].ID))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repositoryState.WorktreePath, "tracked.txt"), []byte("edited outside wx\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestStandbyRetiredWhenAttributesChangeAndReplenishmentRestoresWarmLease(t *testing.T) {
	f := newReuseStandbyFixture(t)
	ctx := context.Background()
	stale := f.readyStandby(t)
	f.commitChangedAttributes(t)
	lease, err := f.manager.ResolveAndLease(ctx, f.repository, nil, "codex", os.Getpid(), leaseAttrs{})
	if err != nil {
		t.Fatal(err)
	}
	if lease.SessionID == stale.ID {
		t.Fatalf("lease=%+v, want a cold start on another slot", lease)
	}
	f.requireRetired(t, stale.ID)
	f.settleStandby(t)
	repositoryState, err := f.store.SlotRepository(ctx, lease.SessionID, string(f.workspace.Repositories[0].ID))
	if err != nil {
		t.Fatal(err)
	}
	// cold startは新しい属性で展開するため、tracked fileはCRLFになる。
	got, err := os.ReadFile(filepath.Join(repositoryState.WorktreePath, "tracked.txt"))
	if err != nil || !strings.Contains(string(got), "\r\n") {
		t.Fatalf("cold start content=%q err=%v", got, err)
	}
	replenished := f.readyStandby(t)
	warm, err := f.manager.ResolveAndLease(ctx, f.repository, nil, "codex", os.Getpid(), leaseAttrs{})
	if err != nil {
		t.Fatal(err)
	}
	if warm.SessionID != replenished.ID || !warm.Ready {
		t.Fatalf("second lease=%+v, want warm lease of %s", warm, replenished.ID)
	}
}

func TestStandbyKeptReadyWhenBranchLeaseFindsItNotUpdateable(t *testing.T) {
	f := newReuseStandbyFixture(t)
	ctx := context.Background()
	standby := f.readyStandby(t)
	f.commitChangedAttributes(t)
	lease, err := f.manager.ResolveAndLease(ctx, f.repository, []string{"main"}, "codex", os.Getpid(), leaseAttrs{})
	if err != nil {
		t.Fatal(err)
	}
	if lease.SessionID == standby.ID {
		t.Fatalf("lease=%+v, want a cold start on another slot", lease)
	}
	if got := f.slotState(t, standby.ID); got != "READY" {
		t.Fatalf("standby state=%s, want READY for a --branch lease", got)
	}
}

func TestStandbyRetiredWhenReadyWorktreeHasTrackedChanges(t *testing.T) {
	f := newReuseStandbyFixture(t)
	ctx := context.Background()
	standby := f.readyStandby(t)
	f.dirtyStandbyTracked(t, standby.ID)
	lease, err := f.manager.ResolveAndLease(ctx, f.repository, nil, "codex", os.Getpid(), leaseAttrs{})
	if err != nil {
		t.Fatal(err)
	}
	if lease.SessionID == standby.ID {
		t.Fatalf("lease=%+v, want a cold start on another slot", lease)
	}
	// 棄却した候補をREADYのまま残すと、次の定期reconcileまでstatusのreadyが実態とずれる。
	f.requireRetired(t, standby.ID)
	f.settleStandby(t)
	replenished := f.readyStandby(t)
	if replenished.ID == standby.ID {
		t.Fatalf("replenished slot=%s, want a new slot", replenished.ID)
	}
	warm, err := f.manager.ResolveAndLease(ctx, f.repository, nil, "codex", os.Getpid(), leaseAttrs{})
	if err != nil {
		t.Fatal(err)
	}
	if warm.SessionID != replenished.ID || !warm.Ready {
		t.Fatalf("second lease=%+v, want warm lease of %s", warm, replenished.ID)
	}
}

func TestStandbyKeptReadyWhenBranchLeaseFindsTrackedChanges(t *testing.T) {
	f := newReuseStandbyFixture(t)
	ctx := context.Background()
	standby := f.readyStandby(t)
	f.dirtyStandbyTracked(t, standby.ID)
	lease, err := f.manager.ResolveAndLease(ctx, f.repository, []string{"main"}, "codex", os.Getpid(), leaseAttrs{})
	if err != nil {
		t.Fatal(err)
	}
	if lease.SessionID == standby.ID {
		t.Fatalf("lease=%+v, want a cold start on another slot", lease)
	}
	if got := f.slotState(t, standby.ID); got != "READY" {
		t.Fatalf("standby state=%s, want READY for a --branch lease", got)
	}
}

// 再利用を切った貸出でもtracked変更の候補は回収する。
// 回収しないと候補の検証失敗がそのまま貸出のエラーになり、cold startにも落ちない。
func TestStandbyRetiredWithTrackedChangesWithoutReuse(t *testing.T) {
	f := newReuseStandbyFixture(t)
	ctx := context.Background()
	f.manager.cfg.Worktree.ReuseStandby = false
	standby := f.readyStandby(t)
	f.dirtyStandbyTracked(t, standby.ID)
	lease, err := f.manager.ResolveAndLease(ctx, f.repository, nil, "codex", os.Getpid(), leaseAttrs{})
	if err != nil {
		t.Fatal(err)
	}
	if lease.SessionID == standby.ID {
		t.Fatalf("lease=%+v, want a cold start on another slot", lease)
	}
	f.requireRetired(t, standby.ID)
}

func TestReconcileRetiresStandbyWithoutPlacementHistoryAndDoctorReportsIt(t *testing.T) {
	f := newReuseStandbyFixture(t)
	ctx := context.Background()
	standby := f.readyStandby(t)
	if err := os.WriteFile(filepath.Join(f.repository, "tracked.txt"), []byte("advanced\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitRun(t, f.repository, "commit", "-am", "advance main")
	// 配置履歴を持つslotは、mainが進んだだけでは保存済み状態の検証で維持される。
	// 保守の一巡はそのslotをidle更新へ回すため、更新のjobを流し切ってからREADYを確かめる。
	f.manager.reconcileRegistry(ctx)
	f.runPendingJobs(t)
	if got := f.slotState(t, standby.ID); got != "READY" {
		t.Fatalf("standby with placement history=%s, want READY", got)
	}
	// idle更新で現在のmainへ揃うため、配置履歴を持たないslotの扱いを見るにはもう一度mainを進める。
	if err := os.WriteFile(filepath.Join(f.repository, "tracked.txt"), []byte("advanced again\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitRun(t, f.repository, "commit", "-am", "advance main again")
	raw := openTestDatabase(t, filepath.Join(f.root, "state.db"))
	if _, err := raw.ExecContext(ctx, `UPDATE slots SET placement_history_complete=0 WHERE id=?`, standby.ID); err != nil {
		t.Fatal(err)
	}
	report := f.manager.registrationFindings(ctx)
	legacy := false
	for _, finding := range report {
		if finding.Severity == diag.SeverityProblem {
			t.Fatalf("doctor finding=%+v, want no problem", finding)
		}
		legacy = legacy || (finding.Severity == diag.SeverityInfo && finding.Target == standby.Path)
	}
	if !legacy {
		t.Fatalf("doctor findings=%+v, want an info finding for %s", report, standby.Path)
	}
	f.manager.reconcileRegistry(ctx)
	if got := f.slotState(t, standby.ID); got != "STALE" {
		t.Fatalf("legacy standby state=%s, want STALE", got)
	}
	// 同じ巡で補充まで届き、次の貸出はwarmになる。
	f.settleStandby(t)
	replenished := f.readyStandby(t)
	if replenished.ID == standby.ID {
		t.Fatalf("replenished slot=%s, want a new slot", replenished.ID)
	}
	lease, err := f.manager.ResolveAndLease(ctx, f.repository, nil, "codex", os.Getpid(), leaseAttrs{})
	if err != nil {
		t.Fatal(err)
	}
	if lease.SessionID != replenished.ID || !lease.Ready {
		t.Fatalf("lease=%+v, want warm lease of %s", lease, replenished.ID)
	}
}

func TestStandbyUpdateReusesSlotAndSkipsHooksAndPrepare(t *testing.T) {
	requireDaemonIntegration(t)
	root := t.TempDir()
	repository := filepath.Join(root, "repo")
	initGitRepo(t, repository)
	if err := os.WriteFile(filepath.Join(repository, ".gitignore"), []byte("local.cfg\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repository, ".worktreeinclude"), []byte("local.cfg\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repository, "local.cfg"), []byte("old\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitRun(t, repository, "add", ".gitignore", ".worktreeinclude")
	gitRun(t, repository, "commit", "-m", "add include rules")
	hookLog := filepath.Join(root, "hook.log")
	hooks := filepath.Join(root, "hooks")
	if err := os.Mkdir(hooks, 0o700); err != nil {
		t.Fatal(err)
	}
	hook := "#!/bin/sh\nprintf 'hook\\n' >> " + hookLog + "\n"
	if err := os.WriteFile(filepath.Join(hooks, "post-checkout"), []byte(hook), 0o700); err != nil {
		t.Fatal(err)
	}
	gitRun(t, repository, "config", "core.hooksPath", hooks)
	prepareLog := filepath.Join(root, "prepare.log")
	store, err := state.Open(filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cfg := config.Defaults()
	cfg.Storage.WorktreeRoot = filepath.Join(root, "worktrees")
	cfg.Worktree.Undefined = "hot"
	cfg.Pool.WarmPerWorkspace = 1
	cfg.Repositories = map[string]config.Repository{repository: {Prepare: config.Prepare{Command: []string{"sh", "-c", "printf 'prepare\\n' >> " + prepareLog}}}}
	m := testManager(t, cfg, store)
	m.git = &gitx.Runner{Timeout: 10 * time.Second}
	defer m.Close()
	ctx := context.Background()
	discoverer := discovery.Discoverer{Git: m.git, Config: cfg}
	w, err := discoverer.Resolve(ctx, repository)
	if err != nil {
		t.Fatal(err)
	}
	w = registerTestWorkspace(t, store, w)
	raw := openTestDatabase(t, filepath.Join(root, "state.db"))
	defer raw.Close()
	if _, err := raw.ExecContext(ctx, `UPDATE repositories SET last_leased_at=?`, state.FormatTime(time.Now())); err != nil {
		t.Fatal(err)
	}
	if err := m.ensureStandby(ctx, w); err != nil {
		t.Fatal(err)
	}
	jobs, err := store.RecoverJobs(ctx, false)
	if err != nil || len(jobs) != 1 {
		t.Fatalf("prepare jobs=%+v err=%v", jobs, err)
	}
	prepareJob, err := store.ClaimJob(ctx, jobs[0].ID, "prepare")
	if err != nil {
		t.Fatal(err)
	}
	if err := m.runRecoveredJob(ctx, prepareJob); err != nil {
		t.Fatal(err)
	}
	if err := store.FinishJob(ctx, prepareJob.ID, "prepare", nil); err != nil {
		t.Fatal(err)
	}
	ready, ok, err := store.ReadySlot(ctx, string(w.ID))
	if err != nil || !ok || !ready.PlacementHistoryComplete {
		t.Fatalf("ready=%+v ok=%t err=%v", ready, ok, err)
	}
	if err := os.WriteFile(filepath.Join(repository, "local.cfg"), []byte("new\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repository, "tracked.txt"), []byte("new head\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitRun(t, repository, "add", "tracked.txt")
	gitRun(t, repository, "commit", "-m", "advance main")
	newOID := strings.TrimSpace(gitOutput(t, repository, "rev-parse", "HEAD"))
	lease, err := m.ResolveAndLease(ctx, repository, nil, "codex", os.Getpid(), leaseAttrs{})
	if err != nil {
		t.Fatal(err)
	}
	if lease.SessionID != ready.ID || lease.Ready {
		t.Fatalf("updated lease=%+v, want same slot behind readiness gate", lease)
	}
	jobs, err = store.RecoverJobs(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	var update state.Job
	for _, job := range jobs {
		if job.Kind == "UPDATE" {
			update = job
		}
	}
	if update.ID == "" {
		t.Fatalf("jobs=%+v, want UPDATE", jobs)
	}
	claimed, err := store.ClaimJob(ctx, update.ID, "update")
	if err != nil {
		t.Fatal(err)
	}
	if err := m.runRecoveredJob(ctx, claimed); err != nil {
		t.Fatal(err)
	}
	if err := store.FinishJob(ctx, claimed.ID, "update", nil); err != nil {
		t.Fatal(err)
	}
	if err := m.WaitReady(ctx, lease.SessionID, lease.Token); err != nil {
		t.Fatal(err)
	}
	repositoryState, err := store.SlotRepository(ctx, ready.ID, string(w.Repositories[0].ID))
	if err != nil || repositoryState.BaseOID != newOID {
		t.Fatalf("repository state=%+v err=%v", repositoryState, err)
	}
	if got, err := os.ReadFile(filepath.Join(repositoryState.WorktreePath, "local.cfg")); err != nil || string(got) != "new\n" {
		t.Fatalf("updated include=%q err=%v", got, err)
	}
	for path, want := range map[string]int{hookLog: 1, prepareLog: 1} {
		data, err := os.ReadFile(path)
		if err != nil || strings.Count(string(data), "\n") != want {
			t.Fatalf("execution log %s=%q err=%v", path, data, err)
		}
	}
}
