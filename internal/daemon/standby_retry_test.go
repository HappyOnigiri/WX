package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/HappyOnigiri/WX/internal/config"
	"github.com/HappyOnigiri/WX/internal/discovery"
	"github.com/HappyOnigiri/WX/internal/state"
)

// retryStandbyFixture は補充が有効な workspace を 1 つ持つ Manager を用意する。
func retryStandbyFixture(t *testing.T, mode string) (*Manager, *state.Store, discovery.Workspace) {
	t.Helper()
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	initGitRepo(t, repo)
	cfg := config.Defaults()
	cfg.Storage.WorktreeRoot = filepath.Join(root, "worktrees")
	cfg.Worktree.Undefined = mode
	cfg.Pool.WarmPerWorkspace = 1
	store, err := openTestStoreAtPath(t, filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	m := testManager(t, cfg, store)
	m.git.SetTimeout(10 * time.Second)
	t.Cleanup(m.Close)
	discoverer := discovery.Discoverer{Git: m.git, Config: cfg}
	w, err := discoverer.Resolve(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	return m, store, registerTestWorkspace(t, store, w)
}

// failFirstStandby は補充された待機 slot を準備失敗で終わらせ、その slot ID と PREPARE job を返す。
// job を終わらせないとリトライ中として扱われ、補充停止も記録されない。
func failFirstStandby(t *testing.T, m *Manager, store *state.Store, w discovery.Workspace) (string, state.Job) {
	t.Helper()
	ctx := context.Background()
	if err := m.ensureStandby(ctx, w); err != nil {
		t.Fatal(err)
	}
	jobs, err := store.RecoverJobs(ctx, false)
	if err != nil || len(jobs) != 1 || jobs[0].Kind != "PREPARE" {
		t.Fatalf("standby jobs=%+v err=%v", jobs, err)
	}
	prepare := jobs[0]
	if err := store.SetSlotState(ctx, prepare.SlotID, []string{"PREPARING"}, "FAILED", "PREPARE_FAILED"); err != nil {
		t.Fatal(err)
	}
	claimed, err := store.ClaimJob(ctx, prepare.ID, "test")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.FinishJob(ctx, claimed.ID, "test", errors.New("prepare failed")); err != nil {
		t.Fatal(err)
	}
	m.suspendStandbyReplenishment(ctx, prepare)
	return prepare.SlotID, prepare
}

// runPendingEnsureStandby は予約済みの ENSURE_STANDBY を 1 件実行する。
func runPendingEnsureStandby(t *testing.T, m *Manager, store *state.Store) {
	t.Helper()
	ctx := context.Background()
	jobs, err := store.RecoverJobs(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, job := range jobs {
		if job.Kind != "ENSURE_STANDBY" {
			continue
		}
		claimed, err := store.ClaimJob(ctx, job.ID, "retry-test")
		if err != nil {
			t.Fatal(err)
		}
		if err := m.runRecoveredJob(ctx, claimed); err != nil {
			t.Fatalf("ensure standby: %v", err)
		}
		if err := store.FinishJob(ctx, claimed.ID, "retry-test", nil); err != nil {
			t.Fatal(err)
		}
		return
	}
	t.Fatalf("retry did not enqueue an ensure job: %+v", jobs)
}

// TestRetryStandbyReclaimsFailedStandbyBeforeReplenishing は、準備に失敗した待機 slot が残る workspace でも
// `wx retry-standby` が補充を進めることを確認する。
// FAILED は待機枠に数えるため、回収しないと補充の不足が 0 になり、GC が隔離を消すまで枠が空かない。
func TestRetryStandbyReclaimsFailedStandbyBeforeReplenishing(t *testing.T) {
	t.Parallel()
	requireDaemonIntegration(t)
	m, store, w := retryStandbyFixture(t, "hot")
	ctx := context.Background()
	failedSlot, _ := failFirstStandby(t, m, store, w)
	if count := store.StandbyCount(ctx, string(w.ID)); count != 1 {
		t.Fatalf("standby count while the failed slot remains=%d, want one", count)
	}

	retry, err := m.RetryStandby(ctx, string(w.Root))
	if err != nil {
		t.Fatal(err)
	}
	if retry["removed_failed"] != 1 {
		t.Fatalf("retry reply=%v, want one reclaimed failed slot", retry)
	}
	slot, err := store.Slot(ctx, failedSlot)
	if err != nil {
		t.Fatal(err)
	}
	if slot.State != "REMOVING" {
		t.Fatalf("failed slot state after retry=%q, want REMOVING", slot.State)
	}
	// REMOVING は待機枠に数えないので、削除の完了を待たずに補充が走る。
	if count := store.StandbyCount(ctx, string(w.ID)); count != 0 {
		t.Fatalf("standby count after reclaiming the failed slot=%d, want zero", count)
	}

	runPendingEnsureStandby(t, m, store)
	if count := store.StandbyCount(ctx, string(w.ID)); count != 1 {
		t.Fatalf("standby count after the retried replenishment=%d, want one", count)
	}
	jobs, err := store.RecoverJobs(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	var prepared bool
	for _, job := range jobs {
		if job.Kind == "PREPARE" && job.SlotID != failedSlot {
			prepared = true
		}
	}
	if !prepared {
		t.Fatalf("retry did not create a new standby slot: %+v", jobs)
	}
}

// TestRetryStandbyAllResumesEveryStoppedWorkspace は `--all` が停止中の workspace をまとめて戻すことを確認する。
// 補充が無効な workspace は RetryStandby が拒否するため、停止行があっても対象にしない。
func TestRetryStandbyAllResumesEveryStoppedWorkspace(t *testing.T) {
	t.Parallel()
	requireDaemonIntegration(t)
	m, store, hot := retryStandbyFixture(t, "hot")
	ctx := context.Background()
	failFirstStandby(t, m, store, hot)

	// 同じ Manager に 2 つ目の workspace を登録し、片方だけ補充を無効にする。
	second := filepath.Join(filepath.Dir(string(hot.Root)), "repo2")
	initGitRepo(t, second)
	discoverer := discovery.Discoverer{Git: m.git, Config: m.Config()}
	resolved, err := discoverer.Resolve(ctx, second)
	if err != nil {
		t.Fatal(err)
	}
	other := registerTestWorkspace(t, store, resolved)
	if err := store.SuspendReplenish(ctx, string(other.ID), state.SuspendReplenishReasonClean, "run-1"); err != nil {
		t.Fatal(err)
	}
	off := filepath.Join(filepath.Dir(string(hot.Root)), "repo3")
	initGitRepo(t, off)
	resolvedOff, err := discoverer.Resolve(ctx, off)
	if err != nil {
		t.Fatal(err)
	}
	disabled := registerTestWorkspace(t, store, resolvedOff)
	if err := store.SuspendReplenish(ctx, string(disabled.ID), state.SuspendReplenishReasonClean, "run-1"); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	m.cfg.Workspaces[off] = config.Workspace{Worktree: "off"}
	m.mu.Unlock()

	// 解除できない workspace を 1 つ混ぜる。root が消えた workspace は canonicalize で落ちる。
	missing := filepath.Join(filepath.Dir(string(hot.Root)), "repo4")
	initGitRepo(t, missing)
	resolvedMissing, err := discoverer.Resolve(ctx, missing)
	if err != nil {
		t.Fatal(err)
	}
	gone := registerTestWorkspace(t, store, resolvedMissing)
	if err := store.SuspendReplenish(ctx, string(gone.ID), state.SuspendReplenishReasonClean, "run-1"); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(missing); err != nil {
		t.Fatal(err)
	}

	result, err := m.RetryStandbyAll(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// 1 件の失敗では止めず、残りを処理したうえで理由を返す。
	if len(result.Failures) != 1 || result.Failures[0].Root != string(gone.Root) {
		t.Fatalf("retry-standby --all failures=%+v", result.Failures)
	}
	resumed := map[string]bool{}
	for _, reply := range result.Workspaces {
		resumed[fmt.Sprint(reply["root"])] = true
	}
	if len(resumed) != 2 || !resumed[string(hot.Root)] || !resumed[string(other.Root)] {
		t.Fatalf("retry-standby --all resumed=%v", result.Workspaces)
	}
	for _, w := range []discovery.Workspace{hot, other} {
		if suspended, err := store.ReplenishSuspended(ctx, string(w.ID)); err != nil || suspended {
			t.Fatalf("workspace %s suspended after --all=%v err=%v", w.Root, suspended, err)
		}
	}
	// 補充が無効な workspace は解除せず、停止行も残す。hot へ戻したときに再び案内できる状態を保つ。
	if suspended, err := store.ReplenishSuspended(ctx, string(disabled.ID)); err != nil || !suspended {
		t.Fatalf("workspace without replenishment suspended=%v err=%v", suspended, err)
	}
}
