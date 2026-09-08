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

	"github.com/HappyOnigiri/WX/internal/config"
	"github.com/HappyOnigiri/WX/internal/diag"
	"github.com/HappyOnigiri/WX/internal/discovery"
	"github.com/HappyOnigiri/WX/internal/state"
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
			store, err := state.Open(filepath.Join(root, "state.db"))
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
			lease, err := m.leaseWithPolicy(ctx, repo, nil, "codex", os.Getpid(), false)
			if mode == "off" || mode == "ask" {
				if err == nil {
					t.Fatal("unapproved creation accepted")
				}
				lease, err = m.leaseWithPolicy(ctx, repo, nil, "codex", os.Getpid(), true)
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
	store, err := state.Open(filepath.Join(root, "state.db"))
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
	lease, err := m.leaseWithPolicy(ctx, repo, nil, "codex", os.Getpid(), false)
	if err != nil || lease.SessionID == ready.ID {
		t.Fatalf("lease=%+v ready=%s err=%v", lease, ready.ID, err)
	}
	after, ok, err := store.ReadySlot(ctx, string(w.ID))
	if err != nil || !ok || after.ID != ready.ID {
		t.Fatalf("standby changed: %+v err=%v", after, err)
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
	store, err := state.Open(filepath.Join(root, "state.db"))
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
	store, err := state.Open(filepath.Join(root, "state.db"))
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
