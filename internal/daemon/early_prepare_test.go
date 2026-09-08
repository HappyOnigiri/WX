package daemon

import (
	"context"
	"errors"
	"os"
	"path/filepath"
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
		if len(args) == 0 || args[0] != "checkout-index" {
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
