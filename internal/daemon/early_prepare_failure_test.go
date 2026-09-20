package daemon

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/HappyOnigiri/WX/internal/discovery"
	"github.com/HappyOnigiri/WX/internal/state"
)

// failingPostCheckoutHook は必ず非ゼロで終わる post-checkout である。
// early ready はこの hook より前に解けるため、エージェントは既に起動している。
const failingPostCheckoutHook = "#!/bin/sh\nprintf 'post-checkout refused\\n' >&2\nexit 3\n"

// installPostCheckoutHook は repository の common directory へ hook を置く。
func installPostCheckoutHook(t *testing.T, repository, script string) {
	t.Helper()
	common := gitOutput(t, repository, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err := os.WriteFile(filepath.Join(common, "hooks", "post-checkout"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
}

// early ready の後に準備が失敗しても貸出は続き、終了した session の作業は snapshot に届く。
// 隔離すると返却が LEASED を通らず、`cannot be released from` を繰り返したまま作業が失われる。
func TestPrepareFailureAfterEarlyReadyKeepsTheLeaseAndSnapshots(t *testing.T) {
	t.Parallel()
	requireDaemonIntegration(t)
	f := runningManagerFixture(t, func(s *managerFixtureSetup) {
		s.Config.Pool.WarmPerWorkspace = 0
		s.Config.Retention.EndedWorktree.Duration = 0
		s.Config.Readiness.Timeout.Duration = 10 * time.Second
	})
	repository := filepath.Join(f.Root, "repo")
	initGitRepo(t, repository)
	installPostCheckoutHook(t, repository, failingPostCheckoutHook)
	ctx := context.Background()
	lease, err := f.Manager.ResolveAndLease(ctx, repository, nil, "codex", os.Getpid(), leaseAttrs{})
	if err != nil {
		t.Fatal(err)
	}
	if err := waitReady(ctx, f.Manager, 60*time.Second, lease.SessionID, lease.Token); err != nil {
		t.Fatalf("readiness after a failed preparation: %v", err)
	}
	slot, err := f.Store.Slot(ctx, lease.SessionID)
	if err != nil || slot.State != "LEASED" || !strings.HasPrefix(slot.FailureCode, "PREPARE_FAILED") || slot.FailurePhase != "post-checkout" {
		t.Fatalf("slot=%+v: %v", slot, err)
	}
	repositories, err := f.Store.SlotRepositories(ctx, lease.SessionID)
	if err != nil || len(repositories) != 1 || repositories[0].State != "READY" {
		t.Fatalf("continued lease repositories=%+v err=%v, want repository state READY", repositories, err)
	}
	notice, err := f.Manager.ClaimPrepareFailureNotice(ctx, lease.SessionID, lease.Token)
	if err != nil || !strings.Contains(notice, "phase=post-checkout") || !strings.Contains(notice, slot.FailureCode) {
		t.Fatalf("notice=%q: %v", notice, err)
	}
	repeat, err := f.Manager.ClaimPrepareFailureNotice(ctx, lease.SessionID, lease.Token)
	if err != nil || repeat != "" {
		t.Fatalf("notice repeated: %q: %v", repeat, err)
	}
	if err := os.WriteFile(filepath.Join(lease.Path, "untracked.txt"), []byte("work\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := f.Manager.Release(ctx, lease.SessionID, lease.Token, "test"); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, 60*time.Second, func() bool {
		released, slotErr := f.Store.Slot(ctx, lease.SessionID)
		return slotErr == nil && released.State == "SNAPSHOTTED"
	})
	if logs := f.logs.tail(); strings.Contains(logs, "cannot be released from") {
		t.Fatalf("release refused the continued lease:\n%s", logs)
	}
}

// 失敗した hook の出力は gitx の詳細ログが持つ。notice へ積み直すと、失敗を
// 「失敗せずに出力した」として報告することになる。
func TestFailedPostCheckoutIsNotReportedAsOutputWithoutFailing(t *testing.T) {
	t.Parallel()
	requireDaemonIntegration(t)
	f := runningManagerFixture(t, func(s *managerFixtureSetup) {
		s.Config.Pool.WarmPerWorkspace = 0
		s.Config.Readiness.Timeout.Duration = 10 * time.Second
	})
	repository := filepath.Join(f.Root, "repo")
	initGitRepo(t, repository)
	installPostCheckoutHook(t, repository, failingPostCheckoutHook)
	ctx := context.Background()
	lease, err := f.Manager.ResolveAndLease(ctx, repository, nil, "codex", os.Getpid(), leaseAttrs{})
	if err != nil {
		t.Fatal(err)
	}
	if err := waitReady(ctx, f.Manager, 60*time.Second, lease.SessionID, lease.Token); err != nil {
		t.Fatal(err)
	}
	if logs := f.logs.tail(); strings.Contains(logs, "prepare produced output without failing") {
		t.Fatalf("a failed hook was reported as output without failing:\n%s", logs)
	}
}

// owner session を持たない待機枠の補充は、これまでどおり隔離する。
// 自動再実行も再利用もしないという不変条件がこの経路に掛かっている。
func TestStandbyPrepareFailureIsStillQuarantined(t *testing.T) {
	t.Parallel()
	requireDaemonIntegration(t)
	f := manualManagerFixture(t, func(s *managerFixtureSetup) {
		s.Config.Worktree.Undefined = "hot"
		s.Config.Pool.WarmPerWorkspace = 1
		s.Config.Readiness.Timeout.Duration = 10 * time.Second
	})
	repository := filepath.Join(f.Root, "repo")
	initGitRepo(t, repository)
	installPostCheckoutHook(t, repository, failingPostCheckoutHook)
	ctx := context.Background()
	discoverer := discovery.Discoverer{Git: f.Manager.git, Config: f.Config}
	resolved, err := discoverer.Resolve(ctx, repository)
	if err != nil {
		t.Fatal(err)
	}
	w := registerTestWorkspace(t, f.Store, resolved)
	// 直近の貸出がない repository には中身を持たない COLD の待機枠しか作られず、checkout も hook も走らない。
	raw := openTestDatabase(t, f.DatabasePath)
	t.Cleanup(func() { _ = raw.Close() })
	if _, err := raw.ExecContext(ctx, `UPDATE repositories SET last_leased_at=?`, state.FormatTime(time.Now())); err != nil {
		t.Fatal(err)
	}
	if err := f.Manager.ensureStandby(ctx, w); err != nil {
		t.Fatal(err)
	}
	jobs, err := f.Store.RecoverJobs(ctx, false)
	if err != nil || len(jobs) != 1 || jobs[0].Kind != "PREPARE" {
		t.Fatalf("standby jobs=%+v err=%v", jobs, err)
	}
	if err := f.Manager.runRecoveredJob(ctx, jobs[0]); err == nil {
		t.Fatal("standby preparation succeeded with a failing post-checkout hook")
	}
	slot, err := f.Store.Slot(ctx, jobs[0].SlotID)
	if err != nil || slot.State != "QUARANTINED" {
		t.Fatalf("slot=%+v: %v", slot, err)
	}
}

// early ready の state 書込みに失敗した準備は、書込み成功とみなして先へ進めない。
func TestPrepareStagedSlotPropagatesEarlyReadyStateFailure(t *testing.T) {
	t.Parallel()
	f, repository := hookPrepareFixture(t, "")
	ctx := context.Background()
	lease, err := f.Manager.ResolveAndLease(ctx, repository, nil, "codex", os.Getpid(), leaseAttrs{})
	if err != nil {
		t.Fatal(err)
	}
	database := openTestDatabase(t, f.DatabasePath)
	trigger := fmt.Sprintf("CREATE TRIGGER fail_early_ready BEFORE UPDATE OF early_ready_at ON slots WHEN NEW.id='%s' BEGIN SELECT RAISE(ABORT,'injected early-ready failure'); END", lease.SessionID)
	if _, err := database.ExecContext(ctx, trigger); err != nil {
		t.Fatal(err)
	}
	jobs, err := f.Store.RecoverJobs(ctx, false)
	if err != nil || len(jobs) != 1 {
		t.Fatalf("prepare jobs=%+v err=%v", jobs, err)
	}
	job, err := f.Store.ClaimJob(ctx, jobs[0].ID, "early-ready-failure")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Manager.runRecoveredJob(ctx, job); err == nil || !strings.Contains(err.Error(), "injected early-ready failure") {
		t.Fatalf("prepare error=%v, want early-ready state failure", err)
	}
	slot, err := f.Store.Slot(ctx, lease.SessionID)
	if err != nil || slot.State != "QUARANTINED" {
		t.Fatalf("slot=%+v err=%v, want quarantined state after early-ready failure", slot, err)
	}
}

// early ready 後の lease 遷移で FinishPreparation が失敗した場合は、その失敗を
// 成功扱いにして PREPARING の slot を残さない。
func TestLeaseAfterPrepareFailurePropagatesFinishPreparationFailure(t *testing.T) {
	t.Parallel()
	ctx, manager, store, workspaceRecord, resolved, databasePath := managerCoverageFixture(t, "repository")
	slot := testSlot(t, manager, string(workspaceRecord.ID), "lease-finish-failure", 1, "PREPARING")
	repository := state.SlotRepository{RepositoryID: string(resolved[0].Repository.ID), State: "PREPARE_RUNNING"}
	if _, err := store.CreateStandby(ctx, slot, []state.SlotRepository{repository}); err != nil {
		t.Fatal(err)
	}
	database := openTestDatabase(t, databasePath)
	if _, err := database.ExecContext(ctx, `CREATE TRIGGER fail_finish_preparation BEFORE UPDATE OF state ON slots WHEN NEW.id='lease-finish-failure' AND NEW.state='READY' BEGIN SELECT RAISE(ABORT,'injected finish preparation failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err := manager.leaseAfterPrepareFailure(ctx, slot.ID); err == nil || !strings.Contains(err.Error(), "injected finish preparation failure") {
		t.Fatalf("leaseAfterPrepareFailure error=%v, want finish failure", err)
	}
	got, err := store.Slot(ctx, slot.ID)
	if err != nil || got.State != "QUARANTINED" {
		t.Fatalf("slot after failed finish=%+v err=%v, want quarantine after ambiguous finish", got, err)
	}
}
