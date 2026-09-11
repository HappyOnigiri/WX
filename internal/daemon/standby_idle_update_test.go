package daemon

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/HappyOnigiri/WX/internal/state"
)

// advanceMain は standby の fingerprint をずらすだけの、更新に適合する commit を main へ積む。
func (f *reuseStandbyFixture) advanceMain(t *testing.T) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(f.repository, "tracked.txt"), []byte("new head\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitRun(t, f.repository, "add", "tracked.txt")
	gitRun(t, f.repository, "commit", "-m", "advance main")
	return strings.TrimSpace(gitOutput(t, f.repository, "rev-parse", "HEAD"))
}

// pendingUpdateJobs は保留中の UPDATE job を返す。idle 更新の予約が積まれたかの確認に使う。
func (f *reuseStandbyFixture) pendingUpdateJobs(t *testing.T) []state.Job {
	t.Helper()
	jobs, err := f.store.RecoverJobs(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	var updates []state.Job
	for _, job := range jobs {
		if job.Kind == "UPDATE" {
			updates = append(updates, job)
		}
	}
	return updates
}

// 保守の一巡が、貸出を待たずに不一致の READY standby を現在の main へ合わせる。
// 合わせておかないと、次の貸出2本が UPDATE の待ち時間を払う。
func TestIdleStandbyRefreshUpdatesMismatchedReadyBeforeLease(t *testing.T) {
	f := newReuseStandbyFixture(t)
	ctx := context.Background()
	standby := f.readyStandby(t)
	newOID := f.advanceMain(t)
	f.manager.reconcileRegistry(ctx)
	updates := f.pendingUpdateJobs(t)
	if len(updates) != 1 || updates[0].SlotID != standby.ID || updates[0].SessionID != "" {
		t.Fatalf("update jobs=%+v, want one session-less UPDATE of %s", updates, standby.ID)
	}
	if got := f.slotState(t, standby.ID); got != "PREPARING" {
		t.Fatalf("reserved standby state=%s, want PREPARING", got)
	}
	f.runPendingJobs(t)
	if got := f.slotState(t, standby.ID); got != "READY" {
		t.Fatalf("refreshed standby state=%s, want READY", got)
	}
	repositoryState, err := f.store.SlotRepository(ctx, standby.ID, string(f.workspace.Repositories[0].ID))
	if err != nil || repositoryState.BaseOID != newOID {
		t.Fatalf("refreshed repository=%+v err=%v", repositoryState, err)
	}
	// 先回りで合わせてあるので、次の貸出は UPDATE を挟まず即座に使える。
	lease, err := f.manager.ResolveAndLease(ctx, f.repository, nil, "codex", os.Getpid(), leaseAttrs{})
	if err != nil {
		t.Fatal(err)
	}
	if lease.SessionID != standby.ID || !lease.Ready {
		t.Fatalf("lease=%+v, want an immediately usable warm lease of %s", lease, standby.ID)
	}
}

// 直前に idle 更新を始めた workspace は、次の一巡では対象にしない。
// include 対象が書き換わり続けても、更新が保守の一巡ごとに連鎖しないための歯止めである。
func TestIdleStandbyRefreshRespectsCooldown(t *testing.T) {
	f := newReuseStandbyFixture(t)
	ctx := context.Background()
	f.advanceMain(t)
	f.manager.markIdleStandbyRefresh(string(f.workspace.ID))
	f.manager.reconcileRegistry(ctx)
	if updates := f.pendingUpdateJobs(t); len(updates) != 0 {
		t.Fatalf("update jobs=%+v, want none within the cooldown", updates)
	}
	f.manager.mu.Lock()
	f.manager.idleStandbyRefreshes[string(f.workspace.ID)] = time.Now().Add(-2 * idleStandbyRefreshCooldown)
	f.manager.mu.Unlock()
	f.manager.reconcileRegistry(ctx)
	if updates := f.pendingUpdateJobs(t); len(updates) != 1 {
		t.Fatalf("update jobs=%+v, want one after the cooldown", updates)
	}
}

// 貸出付きの UPDATE は利用者が待っているので利用者向け、idle 更新は保守用の枠で走らせる。
func TestJobClassOfSeparatesIdleStandbyUpdate(t *testing.T) {
	if got := jobClassOf(state.Job{Kind: "UPDATE", SessionID: "session"}); got != jobClassInteractive {
		t.Fatalf("leased update class=%v, want interactive", got)
	}
	if got := jobClassOf(state.Job{Kind: "UPDATE"}); got != jobClassMaintenance {
		t.Fatalf("idle update class=%v, want maintenance", got)
	}
}
