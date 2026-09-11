package daemon

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/HappyOnigiri/WX/internal/config"
)

// Cold Start の間は実行中の区間名を返し、準備が終われば実行中の区間は残らない。
// 待機中の client はこれだけを頼りに、どの処理で待たされているかを表示する。
func TestLeaseProgressReportsTheRunningPhaseDuringColdStart(t *testing.T) {
	requireDaemonIntegration(t)
	f := runningManagerFixture(t, func(s *managerFixtureSetup) {
		s.Config.Worktree.Undefined = "hot"
		s.Config.Pool.WarmPerWorkspace = 0
		s.Config.Discovery.ReconcileInterval.Duration = time.Hour
		// prepare を1秒止め、区間が確実に観測できる幅を作る。sleep の長さ自体は表示の契約ではない。
		s.Config.Repositories = map[string]config.Repository{
			filepath.Join(s.Root, "repo"): {Prepare: config.Prepare{Command: []string{"sh", "-c", "sleep 1"}}},
		}
	})
	repository := filepath.Join(f.Root, "repo")
	initGitRepo(t, repository)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	lease, err := f.Manager.ResolveAndLease(ctx, repository, nil, "codex", 1, leaseAttrs{})
	if err != nil {
		t.Fatal(err)
	}
	if lease.Route != RouteColdStart || lease.Ready {
		t.Fatalf("lease route=%q ready=%t, want a cold start behind the readiness gate", lease.Route, lease.Ready)
	}
	ready := make(chan error, 1)
	go func() { ready <- f.Manager.WaitReady(ctx, lease.SessionID, lease.Token) }()
	seen := map[string]bool{}
	for waiting := true; waiting; {
		select {
		case err := <-ready:
			if err != nil {
				t.Fatalf("wait for cold start: %v", err)
			}
			waiting = false
		case <-time.After(20 * time.Millisecond):
			progress, err := f.Manager.LeaseProgress(ctx, lease.SessionID, lease.Token)
			if err != nil {
				t.Fatalf("lease progress: %v", err)
			}
			if progress.State == "" {
				t.Fatalf("progress=%+v, want the slot state", progress)
			}
			if progress.Phase != "" {
				seen[progress.Phase] = true
				// 区間を観測できた回は準備 job が走っている。区間の切れ目と job 待ちを
				// 区別できるよう、実行中である事実は区間名とは別に伝わる必要がある。
				if !progress.Running {
					t.Fatalf("progress=%+v, want the running flag while a phase is measured", progress)
				}
			}
		}
	}
	if !seen["prepare-command"] {
		t.Fatalf("observed phases=%v, want the prepare command phase", seen)
	}
	progress, err := f.Manager.LeaseProgress(ctx, lease.SessionID, lease.Token)
	if err != nil {
		t.Fatal(err)
	}
	if progress.Phase != "" || progress.Running {
		t.Fatalf("progress=%+v, want no running preparation once it finished", progress)
	}
}

// 進捗は貸出を持つ client だけが読める。token が合わない要求は WaitReady と同じく拒否する。
func TestLeaseProgressRejectsAnUnauthenticatedRequest(t *testing.T) {
	t.Parallel()
	f := runningManagerFixture(t)
	lease, err := legacyLeaseFixture(f.Manager, "codex", 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Manager.LeaseProgress(context.Background(), lease.SessionID, "wrong-token"); err == nil {
		t.Fatal("lease progress accepted a wrong token")
	}
	progress, err := f.Manager.LeaseProgress(context.Background(), lease.SessionID, lease.Token)
	if err != nil {
		t.Fatal(err)
	}
	if progress.State == "" {
		t.Fatalf("progress=%+v, want the slot state for the lease holder", progress)
	}
}

// 完全一致 READY の貸出は待機を伴わないため、経路もそのように返す。
func TestLeaseRouteIsReadyWhenAStandbyMatches(t *testing.T) {
	requireDaemonIntegration(t)
	f := runningManagerFixture(t, func(s *managerFixtureSetup) {
		s.Config.Worktree.Undefined = "hot"
		s.Config.Pool.WarmPerWorkspace = 1
		s.Config.Discovery.ReconcileInterval.Duration = time.Hour
	})
	repository := filepath.Join(f.Root, "repo")
	initGitRepo(t, repository)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cold, err := f.Manager.ResolveAndLease(ctx, repository, nil, "codex", 1, leaseAttrs{})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Manager.WaitReady(ctx, cold.SessionID, cold.Token); err != nil {
		t.Fatal(err)
	}
	if err := f.Manager.Release(ctx, cold.SessionID, cold.Token, "test"); err != nil {
		t.Fatal(err)
	}
	// 補充した待機枠が READY になるまで待つ。ここで貸し出せる候補が無ければ経路は cold start になる。
	warm := waitForReadySlot(t, f, repository)
	lease, err := f.Manager.ResolveAndLease(ctx, repository, nil, "codex", 1, leaseAttrs{})
	if err != nil {
		t.Fatal(err)
	}
	if lease.SessionID != warm || lease.Route != RouteReady || !lease.Ready {
		t.Fatalf("lease=%+v, want the matching ready standby %s", lease, warm)
	}
}

// waitForReadySlot は補充が READY の待機枠を作るまで待ち、その slot ID を返す。
func waitForReadySlot(t *testing.T, f *managerFixture, repository string) string {
	t.Helper()
	ctx := context.Background()
	w, err := f.Store.WorkspaceByRoot(ctx, repository)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		slot, ok, err := f.Store.ReadySlot(ctx, string(w.ID))
		if err != nil {
			t.Fatal(err)
		}
		if ok {
			return slot.ID
		}
		time.Sleep(20 * time.Millisecond)
	}
	slots, _ := f.Store.ListSlots(ctx, true)
	t.Fatalf("no standby became READY; slots=%+v", slots)
	return ""
}

// 複数 repository の workspace では、同じ区間名が repository の数だけ繰り返される。
// 実行中の区間には対象と何件目かが付き、client はそれで進捗が何周目かを描き分ける。
func TestLeaseProgressNamesTheRepositoryOfEachPhase(t *testing.T) {
	requireDaemonIntegration(t)
	f := runningManagerFixture(t, func(s *managerFixtureSetup) {
		s.Config.Worktree.Undefined = "hot"
		s.Config.Pool.WarmPerWorkspace = 0
		s.Config.Discovery.ReconcileInterval.Duration = time.Hour
		// 各 repository の準備を止め、repository ごとの区間を観測できる幅を作る。
		sleep := config.Repository{Prepare: config.Prepare{Command: []string{"sh", "-c", "sleep 1"}}}
		s.Config.Repositories = map[string]config.Repository{
			filepath.Join(s.Root, "service"): sleep, filepath.Join(s.Root, "web"): sleep,
		}
	})
	initGitRepo(t, filepath.Join(f.Root, "service"))
	initGitRepo(t, filepath.Join(f.Root, "web"))
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	lease, err := f.Manager.ResolveAndLease(ctx, f.Root, nil, "codex", 1, leaseAttrs{})
	if err != nil {
		t.Fatal(err)
	}
	ready := make(chan error, 1)
	go func() { ready <- f.Manager.WaitReady(ctx, lease.SessionID, lease.Token) }()
	targets := map[string]int{}
	for waiting := true; waiting; {
		select {
		case err := <-ready:
			if err != nil {
				t.Fatalf("wait for cold start: %v", err)
			}
			waiting = false
		case <-time.After(20 * time.Millisecond):
			progress, err := f.Manager.LeaseProgress(ctx, lease.SessionID, lease.Token)
			if err != nil {
				t.Fatalf("lease progress: %v", err)
			}
			if progress.Phase == "" || progress.Target == "" {
				continue
			}
			if progress.TargetTotal != 2 || progress.TargetIndex < 1 || progress.TargetIndex > 2 {
				t.Fatalf("progress=%+v, want the position among the two repositories", progress)
			}
			targets[progress.Target] = progress.TargetIndex
		}
	}
	if targets["service"] == 0 || targets["web"] == 0 {
		t.Fatalf("observed targets=%v, want a phase from each repository", targets)
	}
	if targets["service"] == targets["web"] {
		t.Fatalf("observed targets=%v, want distinct positions per repository", targets)
	}
}
