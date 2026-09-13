package daemon

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/HappyOnigiri/WX/internal/config"
	"github.com/HappyOnigiri/WX/internal/state"
)

// replenishingCleanFixture は補充が有効な workspace を用意する。
// 既定の worktree mode は "ask" で、そのままだと standbyReplenishmentEnabledForRoot が全ての再開を飛ばしてしまう。
func replenishingCleanFixture(t *testing.T) (*Manager, *state.Store, string) {
	t.Helper()
	return cleanFixture(t, func(cfg *config.Config) {
		cfg.Worktree.Undefined = "hot"
		cfg.Pool.WarmPerWorkspace = 1
	})
}

// removeStandbyAndFinish は待機用 slot を削除の完了まで進め、run が閉じるのを待つ。
func removeStandbyAndFinish(t *testing.T, store *state.Store, runID, slotID string) {
	t.Helper()
	waitCleanTargetState(t, store, runID, slotID, cleanTargetRemoving)
	if _, err := store.FinishRemoval(context.Background(), slotID); err != nil {
		t.Fatal(err)
	}
	waitCleanTargetState(t, store, runID, slotID, cleanTargetDone)
	waitCleanRunDone(t, store, runID)
}

// waitCleanReplenishDone は再開処理が記録まで終わるのを待つ。
// 再開は run を閉じた後の別処理なので、run の完了だけでは足りない。
// 停止の解除は再開処理の途中で起きるので、解除だけを待つと ReplenishState と ReplenishResult が未記入のことがある。
func waitCleanReplenishDone(t *testing.T, store *state.Store, runID string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		run, _, err := store.CleanRunByID(context.Background(), runID)
		if err != nil {
			t.Fatal(err)
		}
		if run.ReplenishState == state.CleanReplenishDone {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("clean replenishment for run %s did not finish", runID)
}

func TestCleanReplenishResumesTheWorkspaceItStopped(t *testing.T) {
	manager, store, workspaceID := replenishingCleanFixture(t)
	ctx := context.Background()
	slot := testSlot(t, manager, workspaceID, "standby", 1, "READY")
	if _, err := store.CreateStandby(ctx, slot, nil); err != nil {
		t.Fatal(err)
	}
	reply, err := manager.Clean(ctx, CleanRequest{Standby: true, Replenish: true})
	if err != nil {
		t.Fatal(err)
	}
	runID, _ := reply["run_id"].(string)
	if pending, _ := reply["replenish_pending"].(bool); !pending || runID == "" {
		t.Fatalf("accepted reply=%v", reply)
	}
	removeStandbyAndFinish(t, store, runID, "standby")
	waitCleanReplenishDone(t, store, runID)
	run, _, err := store.CleanRunByID(ctx, runID)
	if err != nil || run.ReplenishState != state.CleanReplenishDone {
		t.Fatalf("run after replenishment=%+v err=%v", run, err)
	}
	// 補充が予約されていなければ、停止だけ解除して枠が空のまま残る。
	var result CleanReplenishResult
	if err := json.Unmarshal([]byte(run.ReplenishResult), &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Workspaces) != 1 || len(result.Failures) != 0 {
		t.Fatalf("replenish result=%+v", result)
	}
	if jobID, _ := result.Workspaces[0]["job_id"].(string); jobID == "" {
		t.Fatalf("replenishment scheduled no job: %+v", result.Workspaces[0])
	}
}

// 再開は run ごとに 1 度だけ行う。driver は再起動や合流で何度でも起きるため、副作用が繰り返されないことを固定する。
func TestCleanReplenishRunsOnlyOncePerRun(t *testing.T) {
	manager, store, workspaceID := replenishingCleanFixture(t)
	ctx := context.Background()
	slot := testSlot(t, manager, workspaceID, "standby", 1, "READY")
	if _, err := store.CreateStandby(ctx, slot, nil); err != nil {
		t.Fatal(err)
	}
	reply, err := manager.Clean(ctx, CleanRequest{Standby: true, Replenish: true})
	if err != nil {
		t.Fatal(err)
	}
	runID, _ := reply["run_id"].(string)
	removeStandbyAndFinish(t, store, runID, "standby")
	waitCleanReplenishDone(t, store, runID)
	// 再開後に改めて止めた停止行を、閉じた run の再実行が解除しないことを確かめる。
	if err := store.SuspendReplenish(ctx, workspaceID, state.SuspendReplenishReasonStandbyFailure, "job"); err != nil {
		t.Fatal(err)
	}
	manager.runCleanReplenish(ctx, runID, true)
	if suspended, err := store.ReplenishSuspended(ctx, workspaceID); err != nil || !suspended {
		t.Fatalf("a finished replenishment ran again: suspended=%v err=%v", suspended, err)
	}
}

// 失敗が残った workspace は戻さない。作って即消す往復を避け、環境の回復確認は wx retry-standby に委ねる。
func TestCleanReplenishSkipsWorkspacesWithFailedTargets(t *testing.T) {
	manager, store, workspaceID := replenishingCleanFixture(t)
	ctx := context.Background()
	slot := testSlot(t, manager, workspaceID, "standby", 1, "READY")
	if _, err := store.CreateStandby(ctx, slot, nil); err != nil {
		t.Fatal(err)
	}
	runID := beginCleanWithoutDriver(t, manager, store, false, true)
	if err := store.SetCleanTargetState(ctx, runID, "standby", []string{cleanTargetPending}, cleanTargetFailed, "removal failed"); err != nil {
		t.Fatal(err)
	}
	result := manager.cleanReplenishResult(ctx, runID)
	if len(result.Workspaces) != 0 || len(result.Failures) != 0 {
		t.Fatalf("result=%+v, want no workspace resumed", result)
	}
	if suspended, err := store.ReplenishSuspended(ctx, workspaceID); err != nil || !suspended {
		t.Fatalf("failed target still resumed replenishment: %v err=%v", suspended, err)
	}
}

// 別要因で止まった workspace は、この run の停止行ではないので戻さない。
func TestCleanReplenishLeavesSuspensionsFromOtherReasons(t *testing.T) {
	manager, store, workspaceID := replenishingCleanFixture(t)
	ctx := context.Background()
	runID := beginCleanWithoutDriver(t, manager, store, false, true)
	if err := store.SuspendReplenish(ctx, workspaceID, state.SuspendReplenishReasonStandbyFailure, "job"); err != nil {
		t.Fatal(err)
	}
	result := manager.cleanReplenishResult(ctx, runID)
	if len(result.Workspaces) != 0 || len(result.Failures) != 0 {
		t.Fatalf("result=%+v, want no workspace resumed", result)
	}
	if suspended, err := store.ReplenishSuspended(ctx, workspaceID); err != nil || !suspended {
		t.Fatalf("a suspension from another reason was resumed: %v err=%v", suspended, err)
	}
}

// 補充が無効な workspace は RetryStandby がエラーにするので、事前に外して失敗として並べない。
func TestCleanReplenishSkipsWorkspacesWithoutReplenishment(t *testing.T) {
	manager, store, workspaceID := cleanFixture(t)
	ctx := context.Background()
	runID := beginCleanWithoutDriver(t, manager, store, false, true)
	if err := store.SuspendReplenish(ctx, workspaceID, state.SuspendReplenishReasonClean, runID); err != nil {
		t.Fatal(err)
	}
	result := manager.cleanReplenishResult(ctx, runID)
	if len(result.Workspaces) != 0 || len(result.Failures) != 0 {
		t.Fatalf("result=%+v, want the disabled workspace skipped without a failure", result)
	}
}
