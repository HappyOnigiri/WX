package daemon

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/HappyOnigiri/WorktreeX/internal/config"
	"github.com/HappyOnigiri/WorktreeX/internal/state"
	"github.com/HappyOnigiri/WorktreeX/internal/workspace"
)

// withUpdatePrepareCommand は READY standby を作った後に、tracked.txt を入力にする prepare command を設定する。
// 設定の変更で動く更新互換 fingerprint を standby の記録へ合わせ、次の貸出が更新経路を通るようにする。
func (f *reuseStandbyFixture) withUpdatePrepareCommand(t *testing.T, script string) state.Slot {
	t.Helper()
	ctx := context.Background()
	ready := f.readyStandby(t)
	cfg := f.manager.Config()
	cfg.Repositories = map[string]config.Repository{string(f.workspace.Repositories[0].MainPath): {Prepare: config.Prepare{Command: []string{"sh", "-c", script}, Inputs: []string{"tracked.txt"}}}}
	f.manager.mu.Lock()
	f.manager.cfg = cfg
	f.manager.mu.Unlock()
	database := openTestDatabase(t, filepath.Join(f.root, "state.db"))
	t.Cleanup(func() { _ = database.Close() })
	for _, repository := range f.workspace.Repositories {
		compatibility, err := workspace.UpdateCompatibilityFingerprint(ready.Generation, repository, f.manager.Config())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := database.ExecContext(ctx, `UPDATE slot_repositories SET compatibility_fingerprint=? WHERE slot_id=? AND repository_id=?`, compatibility, ready.ID, repository.ID); err != nil {
			t.Fatal(err)
		}
	}
	return ready
}

// leasedUpdateJob は貸出付きの UPDATE job を返す。
func (f *reuseStandbyFixture) leasedUpdateJob(t *testing.T, sessionID string) state.Job {
	t.Helper()
	for _, job := range f.pendingUpdateJobs(t) {
		if job.SessionID == sessionID {
			return job
		}
	}
	t.Fatalf("no leased UPDATE job for session %s", sessionID)
	return state.Job{}
}

// 貸出付きの更新は checkout と配置を終えた時点で Early Ready を出し、後半の完了を待たずにエージェントを起動させる。
// 後半が失敗しても貸出を隔離せずに LEASED へ進め、失敗は最初のプロンプトで 1 回だけ伝わり、返却は snapshot へ届く。
// commentlint:allow-long -- Early Ready の境界と、その後の失敗で守る3つの結果を1つの流れで固定する
func TestLeasedStandbyUpdateReleasesEarlyAndKeepsLeaseAfterLateFailure(t *testing.T) {
	f := newReuseStandbyFixture(t)
	ctx := context.Background()
	gate := filepath.Join(f.root, "release-prepare")
	f.withUpdatePrepareCommand(t, "while [ ! -e '"+gate+"' ]; do sleep 0.05; done; exit 3")
	f.advanceMain(t)
	lease, err := f.manager.ResolveAndLease(ctx, f.repository, nil, "codex", os.Getpid(), leaseAttrs{})
	if err != nil {
		t.Fatal(err)
	}
	if lease.Route != RouteUpdate || lease.Ready {
		t.Fatalf("lease=%+v, want the standby update route behind readiness", lease)
	}
	update := f.leasedUpdateJob(t, lease.SessionID)
	done := make(chan error, 1)
	go func() { done <- f.manager.runStandbyUpdate(ctx, update) }()
	waitCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := f.manager.WaitEarlyReady(waitCtx, lease.SessionID, lease.Token); err != nil {
		t.Fatalf("early readiness during the update: %v", err)
	}
	select {
	case runErr := <-done:
		t.Fatalf("update finished before its late half was released: %v", runErr)
	default:
	}
	early, err := f.store.Slot(ctx, lease.SessionID)
	if err != nil || early.State != "PREPARING" || early.EarlyReadyAt == "" {
		t.Fatalf("slot at early readiness=%+v err=%v, want PREPARING with early_ready_at", early, err)
	}
	if err := os.WriteFile(gate, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if runErr := <-done; runErr != nil {
		t.Fatalf("late failure was not absorbed by the lease: %v", runErr)
	}
	slot, err := f.store.Slot(ctx, lease.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if slot.State != "LEASED" || !strings.HasPrefix(slot.FailureCode, "UPDATE_FAILED") || slot.FailurePhase == "" || slot.UpdateCompletedAt == "" || slot.PlacementHistoryComplete {
		t.Fatalf("slot after late failure=%+v, want LEASED with the failure recorded and no placement history", slot)
	}
	repositories, err := f.store.SlotRepositories(ctx, slot.ID)
	if err != nil {
		t.Fatal(err)
	}
	head := strings.TrimSpace(gitOutput(t, f.repository, "rev-parse", "HEAD"))
	for _, repository := range repositories {
		if repository.State != "READY" || repository.BaseOID != head {
			t.Fatalf("repository after late failure=%+v, want READY at %s", repository, head)
		}
	}
	// 再配送された job は完了済みの更新を二度走らせない。
	if err := f.manager.runStandbyUpdate(ctx, update); err != nil {
		t.Fatalf("redelivered update: %v", err)
	}
	notice, err := f.manager.ClaimPrepareFailureNotice(ctx, lease.SessionID, lease.Token)
	if err != nil || !strings.Contains(notice, slot.FailureCode) {
		t.Fatalf("notice=%q err=%v, want the recorded failure", notice, err)
	}
	if repeat, err := f.manager.ClaimPrepareFailureNotice(ctx, lease.SessionID, lease.Token); err != nil || repeat != "" {
		t.Fatalf("notice repeated: %q err=%v", repeat, err)
	}
	if err := os.WriteFile(filepath.Join(lease.Path, "untracked.txt"), []byte("work\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := f.manager.Release(ctx, lease.SessionID, lease.Token, "test"); err != nil {
		t.Fatal(err)
	}
	f.runPendingJobsOfKind(t, "SNAPSHOT")
	released, err := f.store.Slot(ctx, lease.SessionID)
	if err != nil || (released.State != "SNAPSHOTTED" && released.State != "ARCHIVED") {
		t.Fatalf("released slot=%+v err=%v, want the work snapshotted", released, err)
	}
}

// 貸出を伴わない idle 更新は起動を待つ相手がいないため、Early Ready を出さずに READY へ戻る。
func TestIdleStandbyUpdateDoesNotPublishEarlyReady(t *testing.T) {
	f := newReuseStandbyFixture(t)
	ctx := context.Background()
	f.advanceMain(t)
	resolved, err := f.manager.resolveBranches(ctx, f.workspace, nil)
	if err != nil {
		t.Fatal(err)
	}
	f.manager.refreshIdleStandbys(ctx, f.workspace, resolved)
	updates := f.pendingUpdateJobs(t)
	if len(updates) != 1 {
		t.Fatalf("update jobs=%+v, want one idle update", updates)
	}
	if err := f.manager.runStandbyUpdate(ctx, updates[0]); err != nil {
		t.Fatal(err)
	}
	slot, err := f.store.Slot(ctx, updates[0].SlotID)
	if err != nil || slot.State != "READY" || slot.EarlyReadyAt != "" {
		t.Fatalf("idle updated slot=%+v err=%v, want READY without early readiness", slot, err)
	}
}

// Early Ready の前に失敗した更新は、従来どおり隔離して cold start で作り直せる印を残す。
func TestLeasedStandbyUpdateFailureBeforeEarlyReadyIsQuarantined(t *testing.T) {
	f := newReuseStandbyFixture(t)
	ctx := context.Background()
	f.advanceMain(t)
	lease, err := f.manager.ResolveAndLease(ctx, f.repository, nil, "codex", os.Getpid(), leaseAttrs{})
	if err != nil {
		t.Fatal(err)
	}
	update := f.leasedUpdateJob(t, lease.SessionID)
	// 前半の最初の検証で落ちるよう、予約後に standby の tracked file を汚す。
	f.dirtyStandbyTracked(t, update.SlotID)
	if err := f.manager.runStandbyUpdate(ctx, update); err == nil {
		t.Fatal("update of a dirty standby succeeded")
	}
	slot, err := f.store.Slot(ctx, update.SlotID)
	if err != nil || slot.State != "QUARANTINED" || slot.FailureCode != "UPDATE_FAILED" || slot.EarlyReadyAt != "" {
		t.Fatalf("slot=%+v err=%v, want QUARANTINED/UPDATE_FAILED before early readiness", slot, err)
	}
	waitCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := f.manager.WaitEarlyReady(waitCtx, lease.SessionID, lease.Token); !IsColdStartRetryable(err) {
		t.Fatalf("early readiness error=%v, want the cold start retry marker", err)
	}
}
