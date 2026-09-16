package daemon

import (
	"context"
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
