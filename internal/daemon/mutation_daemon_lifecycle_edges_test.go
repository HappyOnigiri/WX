package daemon

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/HappyOnigiri/WorktreeX/internal/config"
	"github.com/HappyOnigiri/WorktreeX/internal/launchd"
	"github.com/HappyOnigiri/WorktreeX/internal/state"
)

func TestMutationReadyMatchesRejectsConfiguredRootExpansionFailure(t *testing.T) {
	t.Parallel()
	f := manualManagerFixture(t)
	cfg := f.Manager.Config()
	cfg.Storage.WorktreeRoot = "~otheruser/worktrees"
	cfg.System.Storage.WorktreeRoot = cfg.Storage.WorktreeRoot
	f.Manager.mu.Lock()
	f.Manager.cfg = cfg
	f.Manager.roots = map[string]bool{}
	f.Manager.rootIdentities = map[string]string{}
	f.Manager.mu.Unlock()

	matched, err := f.Manager.readyMatches(context.Background(), state.Slot{ID: "invalid-root", Path: "relative-slot"}, nil)
	if err != nil || matched {
		t.Fatalf("readyMatches matched=%v err=%v, want a quiet mismatch for an invalid configured root", matched, err)
	}
}

func TestMutationRetainLeaseReportsConfiguredRootFailureBeforeDescriptorLookup(t *testing.T) {
	t.Parallel()
	f := manualManagerFixture(t)
	cfg := f.Manager.Config()
	cfg.Storage.WorktreeRoot = "~otheruser/worktrees"
	cfg.System.Storage.WorktreeRoot = cfg.Storage.WorktreeRoot
	f.Manager.mu.Lock()
	f.Manager.cfg = cfg
	f.Manager.roots = map[string]bool{}
	f.Manager.rootIdentities = map[string]string{}
	f.Manager.mu.Unlock()

	err := f.Manager.retainLease("invalid-root", "relative-slot")
	if !errors.Is(err, state.ErrOwnership) {
		t.Fatalf("retainLease error=%v, want ownership failure", err)
	}
	if strings.Contains(err.Error(), "hold wx root descriptor") {
		t.Fatalf("retainLease performed a descriptor lookup after root expansion failed: %v", err)
	}
}

func TestMutationRecoveredJobAttemptOneKeepsFailedSlotUnchanged(t *testing.T) {
	t.Parallel()
	ctx, manager, store, workspaceRecord, _, _ := managerCoverageFixture(t, "repository")
	slot := testSlotRow(t, manager, string(workspaceRecord.ID), "attempt-one-no-reset", 1, "FAILED")
	if _, err := store.CreateStandby(ctx, slot, nil); err != nil {
		t.Fatal(err)
	}
	job := state.Job{ID: "attempt-one-no-reset-job", Kind: "PREPARE", SlotID: slot.ID, WorkspaceID: "missing-workspace", Attempt: 1}
	if err := manager.runRecoveredJob(ctx, job); err == nil {
		t.Fatal("recovered job unexpectedly succeeded for a missing workspace")
	}
	got, err := store.Slot(ctx, slot.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != "FAILED" {
		t.Fatalf("attempt=1 recovery changed slot state to %q, want FAILED", got.State)
	}
}

func TestMutationGCSkipsZeroRepositoryPruneLog(t *testing.T) {
	t.Parallel()
	ctx, manager, _, _, _, _ := managerCoverageFixture(t, "repository")
	logs := mutationLog(t, manager)
	if _, err := manager.GC(ctx, false); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(logs.tail(), "pruned repository records") {
		t.Fatalf("GC reported repository pruning when no records were removed: %s", logs.tail())
	}
}

func TestMutationReloadRetiresAnInvalidPreviousConfiguredRoot(t *testing.T) {
	f := manualManagerFixture(t)
	t.Setenv("HOME", f.Root)
	newRoot := filepath.Join(f.Root, "reloaded-worktrees")
	cfg := f.Config
	cfg.Storage.WorktreeRoot = newRoot
	cfg.System.Storage.WorktreeRoot = newRoot
	if err := config.Save(cfg); err != nil {
		t.Fatal(err)
	}
	invalidRoot := "~otheruser/worktrees"
	f.Manager.mu.Lock()
	f.Manager.cfg.Storage.WorktreeRoot = invalidRoot
	f.Manager.cfg.System.Storage.WorktreeRoot = invalidRoot
	f.Manager.roots[filepath.Clean(invalidRoot)] = true
	f.Manager.mu.Unlock()

	if err := f.Manager.reloadConfig(false); err != nil {
		t.Fatal(err)
	}
	f.Manager.mu.RLock()
	active := f.Manager.roots[filepath.Clean(invalidRoot)]
	f.Manager.mu.RUnlock()
	if active {
		t.Fatalf("invalid previous configured root remained active after reload: %q", invalidRoot)
	}
}

func TestMutationLaunchdManagedProcessHelper(t *testing.T) {
	if os.Getenv("WX_LAUNCHD_ORPHAN_HELPER") != "1" {
		return
	}
	resultPath := os.Getenv("WX_LAUNCHD_RESULT")
	if resultPath == "" {
		t.Fatal("orphan helper result path is empty")
	}
	if err := os.Setenv("XPC_SERVICE_NAME", launchd.Label); err != nil {
		t.Fatal(err)
	}
	// 親プロセスが書き込み途中の空ファイルを結果として読まないよう、完成後に公開する。
	partialPath := resultPath + ".partial"
	if err := os.WriteFile(partialPath, []byte(boolString(launchdManagedProcess())), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(partialPath, resultPath); err != nil {
		t.Fatal(err)
	}
}

func TestMutationLaunchdManagedProcessRequiresLaunchdEnvironment(t *testing.T) {
	resultPath := filepath.Join(t.TempDir(), "launchd-managed-result")
	cmd := exec.Command("/bin/sh", "-c", `"$0" -test.run=^TestMutationLaunchdManagedProcessHelper$ -test.count=1 &`, os.Args[0])
	env := make([]string, 0, len(os.Environ())+3)
	for _, value := range os.Environ() {
		if strings.HasPrefix(value, "XPC_SERVICE_NAME=") {
			continue
		}
		env = append(env, value)
	}
	env = append(env,
		"WX_LAUNCHD_ORPHAN_HELPER=1",
		"WX_LAUNCHD_RESULT="+resultPath,
		"XPC_SERVICE_NAME="+launchd.Label,
	)
	cmd.Env = env
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(resultPath); err == nil {
			if string(data) != "true" {
				t.Fatalf("orphan launchd helper reported %q, want true", data)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("orphan launchd helper did not write %s", resultPath)
}

func boolString(value bool) string {
	if value {
		return "true"
	}
	return "false"
}
