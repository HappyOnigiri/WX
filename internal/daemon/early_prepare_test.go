package daemon

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/HappyOnigiri/WX/internal/config"
	"github.com/HappyOnigiri/WX/internal/state"
)

// 全準備の待機を短い期限で検査するため、daemon の他テストとは直列に実行する。
func TestEarlyReadinessWaitsForAllRepositoriesButNotRemainingCheckout(t *testing.T) {
	f := runningManagerFixture(t, func(s *managerFixtureSetup) {
		s.Config.Worktree.Undefined = "hot"
		s.Config.Pool.WarmPerWorkspace = 0
		s.Config.Storage.CopyMode = config.CopyModeCopy
	})
	for _, name := range []string{"service", "web"} {
		path := filepath.Join(f.Root, name)
		initGitRepo(t, path)
		if err := os.WriteFile(filepath.Join(path, "AGENTS.md"), []byte(name+" rules"), 0o600); err != nil {
			t.Fatal(err)
		}
		gitOutput(t, path, "add", "AGENTS.md")
		gitOutput(t, path, "commit", "-m", "rules")
	}
	if err := os.WriteFile(filepath.Join(f.Root, "AGENTS.md"), []byte("workspace rules"), 0o600); err != nil {
		t.Fatal(err)
	}
	entered, proceed := make(chan struct{}), make(chan struct{})
	var once sync.Once
	var checkoutCount int
	var mu sync.Mutex
	f.Manager.git.SetBeforeRunAtHook(func(args []string) {
		// checkout は `-c` の設定指定を伴うため、先頭ではなく引数全体から subcommand を探す。
		if !slices.Contains(args, "checkout-index") {
			return
		}
		mu.Lock()
		checkoutCount++
		count := checkoutCount
		mu.Unlock()
		if count == 3 {
			close(entered)
			<-proceed
		}
	})
	defer once.Do(func() { close(proceed) })
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	lease, err := f.Manager.ResolveAndLease(ctx, f.Root, nil, "codex", os.Getpid(), leaseAttrs{})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if err := f.Manager.WaitEarlyReady(ctx, lease.SessionID, lease.Token); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"AGENTS.md", "service/AGENTS.md", "web/AGENTS.md"} {
		if _, err := os.Stat(filepath.Join(lease.Path, path)); err != nil {
			t.Fatalf("early path %s: %v", path, err)
		}
	}
	if _, err := os.Lstat(filepath.Join(lease.Path, ".git")); !os.IsNotExist(err) {
		t.Fatalf("non-Git root has .git: %v", err)
	}
	slot, err := f.Store.Slot(ctx, lease.SessionID)
	if err != nil || slot.State != "PREPARING" || slot.EarlyReadyAt == "" {
		t.Fatalf("slot=%+v: %v", slot, err)
	}
	waitCtx, stop := context.WithTimeout(ctx, 30*time.Millisecond)
	err = f.Manager.WaitReady(waitCtx, lease.SessionID, lease.Token)
	stop()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("full readiness unblocked early: %v", err)
	}
	if err := f.Manager.WaitEarlyReady(ctx, lease.SessionID, "wrong"); err == nil {
		t.Fatal("unauthenticated wait succeeded")
	}
	once.Do(func() { close(proceed) })
	if err := f.Manager.WaitReady(ctx, lease.SessionID, lease.Token); err != nil {
		t.Fatal(err)
	}
	if err := f.Manager.WaitEarlyReady(ctx, lease.SessionID, lease.Token); err != nil {
		t.Fatal(err)
	}
}

func TestInterruptedStagedPreparationIsQuarantinedWithoutReplay(t *testing.T) {
	t.Parallel()
	for _, early := range []bool{false, true} {
		t.Run(map[bool]string{false: "before-early", true: "after-early"}[early], func(t *testing.T) {
			f := manualManagerFixture(t, func(s *managerFixtureSetup) { s.Config.Worktree.Undefined = "hot"; s.Config.Pool.WarmPerWorkspace = 0 })
			repository := filepath.Join(f.Root, "repository")
			initGitRepo(t, repository)
			ctx := context.Background()
			lease, err := f.Manager.ResolveAndLease(ctx, repository, nil, "codex", os.Getpid(), leaseAttrs{})
			if err != nil {
				t.Fatal(err)
			}
			if err := f.Store.BeginStagedPreparation(ctx, lease.SessionID); err != nil {
				t.Fatal(err)
			}
			if early {
				if err := f.Store.MarkEarlyReady(ctx, lease.SessionID); err != nil {
					t.Fatal(err)
				}
			}
			slot, err := f.Store.Slot(ctx, lease.SessionID)
			if err != nil {
				t.Fatal(err)
			}
			sentinel := filepath.Join(slot.Path, "keep-user-work")
			if err := os.WriteFile(sentinel, []byte("preserve"), 0o600); err != nil {
				t.Fatal(err)
			}
			err = f.Manager.runRecoveredJob(ctx, state.Job{Kind: "PREPARE", SlotID: slot.ID, WorkspaceID: slot.WorkspaceID, SessionID: lease.SessionID, Attempt: 2})
			if !errors.Is(err, state.ErrOwnership) {
				t.Fatalf("recovery: %v", err)
			}
			if err := f.Manager.WaitEarlyReady(ctx, lease.SessionID, lease.Token); err == nil || !strings.Contains(err.Error(), "QUARANTINED") {
				t.Fatalf("stale early readiness accepted: %v", err)
			}
			if data, err := os.ReadFile(sentinel); err != nil || string(data) != "preserve" {
				t.Fatalf("user work changed: %q, %v", data, err)
			}
		})
	}
}

// hook が exit 非0 で落ちた回は、Git が既に書いている stderr のログへ辿れる場所を slot に残す。
// これが無いと隔離の理由が `git hook failed with exit N` だけになり、hook が何を言って落ちたかへ辿れない。
func TestStagedPrepareKeepsTheDetailLogOfAFailingHook(t *testing.T) {
	f, repository := hookPrepareFixture(t, "#!/bin/sh\nprintf 'submodule init failed\\n' >&2\nexit 3\n")
	ctx := context.Background()
	slotID, prepareErr := runHookPrepare(ctx, t, f, repository)
	if prepareErr == nil {
		t.Fatal("preparation succeeded with a post-checkout hook that exits non-zero")
	}
	slot, err := f.Store.Slot(ctx, slotID)
	if err != nil {
		t.Fatal(err)
	}
	if slot.State != "QUARANTINED" || slot.FailureCode != "PREPARE_FAILED" {
		t.Fatalf("slot state=%s failure_code=%s", slot.State, slot.FailureCode)
	}
	if slot.FailureDetailPath == "" {
		t.Fatal("quarantined slot has no failure detail path")
	}
	content, err := os.ReadFile(slot.FailureDetailPath)
	if err != nil || !strings.Contains(string(content), "submodule init failed") {
		t.Fatalf("detail log %s = %q: %v", slot.FailureDetailPath, content, err)
	}
}

// exit 0 の hook が出した出力は準備を成功させたまま notice として計測へ載せ、全文を詳細ログへ残す。
func TestStagedPrepareReportsOutputOfASuccessfulHook(t *testing.T) {
	f, repository := hookPrepareFixture(t, "#!/bin/sh\nprintf 'submodule update skipped\\n' >&2\nexit 0\n")
	ctx := context.Background()
	slotID, prepareErr := runHookPrepare(ctx, t, f, repository)
	if prepareErr != nil {
		t.Fatal(prepareErr)
	}
	measurements := f.Manager.PrepareMeasurements(slotID, "")
	if len(measurements) != 1 {
		t.Fatalf("measurements = %+v, want one", measurements)
	}
	notices := measurements[0].Notices
	if len(notices) != 1 || notices[0].Phase != "post-checkout" {
		t.Fatalf("notices = %+v", notices)
	}
	if !strings.Contains(notices[0].Output, "submodule update skipped") {
		t.Fatalf("notice output = %q", notices[0].Output)
	}
	content, err := os.ReadFile(notices[0].DetailPath)
	if err != nil || !strings.Contains(string(content), "submodule update skipped") {
		t.Fatalf("detail log %s = %q: %v", notices[0].DetailPath, content, err)
	}
}

// hookPrepareFixture は post-checkout hook を持つ単一リポジトリと、詳細ログの置き場を備えた Manager を用意する。
// worker を起動しない fixture を使うのは、Git runner の詳細ログ設定が daemon 起動時にだけ書かれる値だからである。
func hookPrepareFixture(t *testing.T, hook string) (*managerFixture, string) {
	t.Helper()
	f := manualManagerFixture(t, func(s *managerFixtureSetup) {
		s.Config.Worktree.Undefined = "hot"
		s.Config.Pool.WarmPerWorkspace = 0
		s.Config.Storage.CopyMode = config.CopyModeCopy
	})
	details := filepath.Join(f.Root, "details")
	f.Manager.prepareDetailDir = details
	f.Manager.git.SetDetailDir(details)
	f.Manager.git.SetTimeout(30 * time.Second)
	repository := filepath.Join(f.Root, "repository")
	initGitRepo(t, repository)
	if err := os.WriteFile(filepath.Join(repository, ".git", "hooks", "post-checkout"), []byte(hook), 0o700); err != nil {
		t.Fatal(err)
	}
	return f, repository
}

// runHookPrepare は貸出で作られた準備 job をその場で実行し、対象 slot と準備の結果を返す。
func runHookPrepare(ctx context.Context, t *testing.T, f *managerFixture, repository string) (string, error) {
	t.Helper()
	lease, err := f.Manager.ResolveAndLease(ctx, repository, nil, "codex", os.Getpid(), leaseAttrs{})
	if err != nil {
		t.Fatal(err)
	}
	jobs, err := f.Store.RecoverJobs(ctx, false)
	if err != nil || len(jobs) != 1 {
		t.Fatalf("prepare jobs=%+v err=%v", jobs, err)
	}
	job, err := f.Store.ClaimJob(ctx, jobs[0].ID, "prepare")
	if err != nil {
		t.Fatal(err)
	}
	return lease.SessionID, f.Manager.runRecoveredJob(ctx, job)
}
