package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/HappyOnigiri/WorktreeX/internal/config"
	"github.com/HappyOnigiri/WorktreeX/internal/diag"
	"github.com/HappyOnigiri/WorktreeX/internal/discovery"
	"github.com/HappyOnigiri/WorktreeX/internal/state"
)

func TestLeasePolicyAndStandbyPermissions(t *testing.T) {
	requireDaemonIntegration(t)
	for _, mode := range []string{"ask", "off", "cold", "hot"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			repo := filepath.Join(root, "repo")
			initGitRepo(t, repo)
			cfg := config.Defaults()
			cfg.Storage.WorktreeRoot = filepath.Join(root, "worktrees")
			cfg.Worktree.Undefined = mode
			store, err := openTestStoreAtPath(t, filepath.Join(root, "state.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = store.Close() }()
			m := testManager(t, cfg, store)
			defer m.Close()
			ctx := context.Background()
			discoverer := discovery.Discoverer{Git: m.git, Config: cfg}
			w, err := discoverer.Resolve(ctx, repo)
			if err != nil {
				t.Fatal(err)
			}
			w = registerTestWorkspace(t, store, w)
			if err := m.ensureStandby(ctx, w); err != nil {
				t.Fatal(err)
			}
			count := store.StandbyCount(ctx, string(w.ID))
			if (mode == "hot" && count != 1) || (mode != "hot" && count != 0) {
				t.Fatalf("standby=%d", count)
			}
			lease, err := m.leaseWithPolicy(ctx, repo, nil, "codex", os.Getpid(), false, leaseAttrs{})
			if mode == "off" || mode == "ask" {
				if err == nil {
					t.Fatal("unapproved creation accepted")
				}
				lease, err = m.leaseWithPolicy(ctx, repo, nil, "codex", os.Getpid(), true, leaseAttrs{})
			}
			if err != nil || lease.SessionID == "" {
				t.Fatalf("lease=%+v err=%v", lease, err)
			}
			if m.Config().Worktree.Undefined != mode {
				t.Fatal("temporary permission persisted")
			}
		})
	}
}

func TestColdPolicyDoesNotLeaseExistingReadySlot(t *testing.T) {
	requireDaemonIntegration(t)
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	initGitRepo(t, repo)
	cfg := config.Defaults()
	cfg.Storage.WorktreeRoot = filepath.Join(root, "worktrees")
	cfg.Worktree.Undefined = "hot"
	store, err := openTestStoreAtPath(t, filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	m := testManager(t, cfg, store)
	defer m.Close()
	ctx := context.Background()
	discoverer := discovery.Discoverer{Git: m.git, Config: cfg}
	w, err := discoverer.Resolve(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	w = registerTestWorkspace(t, store, w)
	if err := m.ensureStandby(ctx, w); err != nil {
		t.Fatal(err)
	}
	jobs, err := store.RecoverJobs(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, job := range jobs {
		if err := m.runRecoveredJob(ctx, job); err != nil {
			t.Fatal(err)
		}
	}
	ready, ok, err := store.ReadySlot(ctx, string(w.ID))
	if err != nil || !ok {
		t.Fatalf("ready=%v err=%v", ok, err)
	}
	m.mu.Lock()
	m.cfg.Workspaces[repo] = config.Workspace{Worktree: "cold"}
	m.mu.Unlock()
	lease, err := m.leaseWithPolicy(ctx, repo, nil, "codex", os.Getpid(), false, leaseAttrs{})
	if err != nil || lease.SessionID == ready.ID {
		t.Fatalf("lease=%+v ready=%s err=%v", lease, ready.ID, err)
	}
	// 待機中の slot は貸出に使われない。cold へ変えた workspace の待機枠は 0 になり、
	// 新規貸出が起こす背景 GC が並走して READY を回収するため、READY のまま残ることは求めない。
	// 貸出が奪っていないことは owner が付かないことで確かめる（GC は owner を NULL のまま REMOVING にする）。
	standby, err := store.Slot(ctx, ready.ID)
	if err != nil || standby.OwnerSessionID != "" || (standby.State != "READY" && standby.State != "REMOVING") {
		t.Fatalf("standby taken by the cold lease: %+v err=%v", standby, err)
	}
}

// standby の準備が失敗した workspace では自動補充が止まり、wx retry-standby で再開することを確かめる。
// 隔離実体は GC が消すので、停止しないと削除と補充が交互に繰り返される。
func TestStandbyReplenishmentStopsAfterAPreparationFailure(t *testing.T) {
	t.Parallel()
	requireDaemonIntegration(t)
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	initGitRepo(t, repo)
	cfg := config.Defaults()
	cfg.Storage.WorktreeRoot = filepath.Join(root, "worktrees")
	cfg.Worktree.Undefined = "hot"
	cfg.Pool.WarmPerWorkspace = 1
	store, err := openTestStoreAtPath(t, filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	m := testManager(t, cfg, store)
	m.git.SetTimeout(10 * time.Second)
	defer m.Close()
	ctx := context.Background()
	discoverer := discovery.Discoverer{Git: m.git, Config: cfg}
	w, err := discoverer.Resolve(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	w = registerTestWorkspace(t, store, w)

	if err := m.ensureStandby(ctx, w); err != nil {
		t.Fatal(err)
	}
	if count := store.StandbyCount(ctx, string(w.ID)); count != 1 {
		t.Fatalf("standby count after the first replenishment=%d, want one", count)
	}
	// 補充された slot をリトライ切れと同じ形で隔離し、準備失敗の停止を記録する。
	jobs, err := store.RecoverJobs(ctx, false)
	if err != nil || len(jobs) != 1 || jobs[0].Kind != "PREPARE" {
		t.Fatalf("standby jobs=%+v err=%v", jobs, err)
	}
	prepare := jobs[0]
	if err := store.SetSlotState(ctx, prepare.SlotID, []string{"PREPARING"}, "QUARANTINED", "JOB_RETRY_EXHAUSTED"); err != nil {
		t.Fatal(err)
	}
	claimed, err := store.ClaimJob(ctx, prepare.ID, "test")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.FinishJob(ctx, claimed.ID, "test", errors.New("prepare failed")); err != nil {
		t.Fatal(err)
	}
	// 停止したことは一度だけ Warn で伝える。10 分ごとの reconcile で同じ警告を積まない。
	var logs bytes.Buffer
	m.log = slog.New(slog.NewTextHandler(&logs, nil))
	m.suspendStandbyReplenishment(ctx, prepare)
	m.suspendStandbyReplenishment(ctx, prepare)
	if got := strings.Count(logs.String(), "standby replenishment stopped after a preparation failure"); got != 1 {
		t.Fatalf("suspension warnings=%d, want one: %s", got, logs.String())
	}
	if err := m.ensureStandby(ctx, w); err != nil {
		t.Fatal(err)
	}
	if jobs, err := store.RecoverJobs(ctx, false); err != nil || len(jobs) != 0 {
		t.Fatalf("replenishment continued while suspended: jobs=%+v err=%v", jobs, err)
	}
	status, err := m.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	blocked, ok := status["standby_replenishment"].([]state.StandbyReplenishmentDiagnostic)
	if !ok || len(blocked) != 1 || blocked[0].Reason != state.SuspendReplenishReasonStandbyFailure || blocked[0].Detail != prepare.ID {
		t.Fatalf("standby recovery diagnostics=%v", status["standby_replenishment"])
	}
	if !strings.Contains(blocked[0].Action, "wx retry-standby") {
		t.Fatalf("standby recovery action=%q", blocked[0].Action)
	}
	// 「準備に失敗」で止めず、記録した失敗理由まで原因に引き継ぐ。
	suspension := doctorProblem(t, m.Doctor(ctx), diag.CheckStandbyReplenishment)
	if !strings.Contains(suspension.Cause, prepare.ID) || !strings.Contains(suspension.Cause, "prepare failed") {
		t.Fatalf("doctor standby recovery cause=%q", suspension.Cause)
	}
	if !strings.Contains(suspension.Action, "wx retry-standby") || suspension.Target != string(w.Root) {
		t.Fatalf("doctor standby recovery finding=%+v", suspension)
	}
	// 停止中の検査に、同時に「停止していない」という正常確認を並べない。
	for _, finding := range doctorFindings(m.Doctor(ctx), diag.CheckStandbyReplenishment) {
		if finding.Severity == diag.SeverityOK {
			t.Fatalf("standby findings claimed replenishment is not stopped=%+v", finding)
		}
	}
	retry, err := m.RetryStandby(ctx, string(w.Root))
	if err != nil {
		t.Fatal(err)
	}
	if retry["generation"] != 1 || retry["resumed"] != true || retry["scheduled"] != true {
		t.Fatalf("retry reply=%v", retry)
	}
	if suspended, err := store.ReplenishSuspended(ctx, string(w.ID)); err != nil || suspended {
		t.Fatalf("suspended after retry=%v err=%v", suspended, err)
	}
	jobs, err = store.RecoverJobs(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	var ensure state.Job
	for _, job := range jobs {
		if job.Kind == "ENSURE_STANDBY" {
			ensure = job
			break
		}
	}
	if ensure.ID == "" {
		t.Fatalf("manual retry did not enqueue ensure job: %+v", jobs)
	}
	ensureClaimed, err := store.ClaimJob(ctx, ensure.ID, "retry-test")
	if err != nil {
		t.Fatal(err)
	}
	if err := m.runRecoveredJob(ctx, ensureClaimed); err != nil {
		t.Fatalf("manual retry job: %v", err)
	}
	if err := store.FinishJob(ctx, ensureClaimed.ID, "retry-test", nil); err != nil {
		t.Fatal(err)
	}
	if got := store.StandbyCount(ctx, string(w.ID)); got != 1 {
		t.Fatalf("standby count after manual retry=%d, want one", got)
	}
	status, err = m.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if blocked, ok := status["standby_replenishment"].([]state.StandbyReplenishmentDiagnostic); !ok || len(blocked) != 0 {
		t.Fatalf("standby recovery diagnostics after retry=%v", status["standby_replenishment"])
	}
}

// 補充対象外の workspace では停止の診断を出さないことを確かめる。
// `wx clear` は policy を問わず停止を記録するため、出すと実行できない `wx retry-standby` を案内してしまう。
func TestStandbySuspensionIsHiddenWithoutReplenishment(t *testing.T) {
	t.Parallel()
	requireDaemonIntegration(t)
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	initGitRepo(t, repo)
	cfg := config.Defaults()
	cfg.Storage.WorktreeRoot = filepath.Join(root, "worktrees")
	cfg.Worktree.Undefined = "off"
	cfg.Pool.WarmPerWorkspace = 1
	store, err := openTestStoreAtPath(t, filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	m := testManager(t, cfg, store)
	m.git.SetTimeout(10 * time.Second)
	defer m.Close()
	ctx := context.Background()
	discoverer := discovery.Discoverer{Git: m.git, Config: cfg}
	w, err := discoverer.Resolve(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	w = registerTestWorkspace(t, store, w)
	if err := store.SuspendReplenish(ctx, string(w.ID), state.SuspendReplenishReasonClean, "run-1"); err != nil {
		t.Fatal(err)
	}
	status, err := m.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if blocked, ok := status["standby_replenishment"].([]state.StandbyReplenishmentDiagnostic); !ok || len(blocked) != 0 {
		t.Fatalf("standby diagnostics for a workspace without replenishment=%v", status["standby_replenishment"])
	}
	for _, finding := range doctorFindings(m.Doctor(ctx), diag.CheckStandbyReplenishment) {
		if finding.Severity != diag.SeverityOK {
			t.Fatalf("doctor standby finding for a workspace without replenishment=%+v", finding)
		}
	}
	if _, err := m.RetryStandby(ctx, string(w.Root)); err == nil {
		t.Fatal("retry-standby accepted a workspace without replenishment")
	}
	// 停止行は残す。hot へ戻した workspace では、実行できる案内として再び現れる。
	m.mu.Lock()
	m.cfg.Workspaces[repo] = config.Workspace{Worktree: "hot"}
	m.mu.Unlock()
	status, err = m.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	blocked, ok := status["standby_replenishment"].([]state.StandbyReplenishmentDiagnostic)
	if !ok || len(blocked) != 1 || blocked[0].Reason != state.SuspendReplenishReasonClean {
		t.Fatalf("standby diagnostics after switching to hot=%v", status["standby_replenishment"])
	}
	if !strings.Contains(blocked[0].Action, "wx retry-standby") {
		t.Fatalf("standby recovery action=%q", blocked[0].Action)
	}
}

// 会話に記録された cwd で worktree の可否を決めるため、WorktreePolicy が lease と同じ workspace の方針を返すことを確かめる。
// 畳まれた slot の path は client 側では読み替えられないので、ここで workspace root へ解決できることも合わせて確かめる。
func TestWorktreePolicyResolvesWorkspaceAndRetiredSlotPath(t *testing.T) {
	requireDaemonIntegration(t)
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	initGitRepo(t, repo)
	cfg := config.Defaults()
	cfg.Storage.WorktreeRoot = filepath.Join(root, "worktrees")
	cfg.Worktree.Undefined = "cold"
	store, err := openTestStoreAtPath(t, filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	m := testManager(t, cfg, store)
	defer m.Close()
	ctx := context.Background()
	discoverer := discovery.Discoverer{Git: m.git, Config: cfg}
	w, err := discoverer.Resolve(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	registerTestWorkspace(t, store, w)

	policy := m.WorktreePolicy(ctx, repo)
	if !policy.Resolved || policy.Root != string(w.Root) || policy.Mode != "cold" {
		t.Fatalf("policy for the repository=%+v, want root=%s mode=cold", policy, w.Root)
	}

	lease, err := m.leaseWithPolicy(ctx, repo, nil, "codex", os.Getpid(), false, leaseAttrs{})
	if err != nil || lease.Path == "" {
		t.Fatalf("lease=%+v err=%v", lease, err)
	}
	// slot を畳んだ後の cwd を模す。path の実体はなく、slot の配下という位置だけが残る。
	retired := m.WorktreePolicy(ctx, filepath.Join(lease.Path, "gone", "repo"))
	if !retired.Resolved || retired.Root != string(w.Root) || retired.Mode != "cold" {
		t.Fatalf("policy for a retired slot path=%+v, want root=%s mode=cold", retired, w.Root)
	}
}

// 解決できない cwd を失敗にせず Resolved=false で返すことを確かめる。
// 呼び出し側はこれを受けて worktree 無しで会話を再開するため、ここで失敗にすると再開の手段がなくなる。
func TestWorktreePolicyReportsUnresolvedInsteadOfFailing(t *testing.T) {
	requireDaemonIntegration(t)
	root := t.TempDir()
	cfg := config.Defaults()
	cfg.Storage.WorktreeRoot = filepath.Join(root, "worktrees")
	store, err := openTestStoreAtPath(t, filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	m := testManager(t, cfg, store)
	defer m.Close()

	if policy := m.WorktreePolicy(context.Background(), filepath.Join(root, "missing")); policy.Resolved {
		t.Fatalf("policy for a missing path=%+v, want unresolved", policy)
	}
}

// Git repository でない cwd は、その directory 自身を root とする方針を返すことを確かめる。
// 巨大な multi repository workspace へ worktree を作らせないための判定がここに載る。
func TestWorktreePolicyUsesDirectoryItselfOutsideRepositories(t *testing.T) {
	requireDaemonIntegration(t)
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	plain := filepath.Join(root, "plain")
	if err := os.MkdirAll(plain, 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := config.Defaults()
	cfg.Storage.WorktreeRoot = filepath.Join(root, "worktrees")
	cfg.Worktree.Undefined = "off"
	store, err := openTestStoreAtPath(t, filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	m := testManager(t, cfg, store)
	defer m.Close()

	policy := m.WorktreePolicy(context.Background(), plain)
	if !policy.Resolved || policy.Root != plain || policy.Mode != "off" {
		t.Fatalf("policy=%+v, want root=%s mode=off", policy, plain)
	}

	// client は method 名と cwd のキーだけで判定を引くため、RPC の経路も合わせて確かめる。
	raw, err := Handler{Manager: m}.Handle(context.Background(), "WorktreePolicy", json.RawMessage(`{"cwd":`+strconv.Quote(plain)+`}`))
	if err != nil {
		t.Fatal(err)
	}
	if reply, ok := raw.(WorktreePolicyReply); !ok || reply != policy {
		t.Fatalf("RPC reply=%#v, want %+v", raw, policy)
	}
}
