package daemon

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/HappyOnigiri/WX/internal/config"
	"github.com/HappyOnigiri/WX/internal/state"
)

// drainPendingJobs はworkerを持たないtest Managerの代わりに、PENDING jobを無くなるまで実行する。
func drainPendingJobs(t *testing.T, m *Manager, store *state.Store) {
	t.Helper()
	ctx := context.Background()
	for range 8 {
		jobs, err := store.RecoverJobs(ctx, false)
		if err != nil {
			t.Fatal(err)
		}
		if len(jobs) == 0 {
			return
		}
		for _, job := range jobs {
			claimed, claimErr := store.ClaimJob(ctx, job.ID, "test")
			if claimErr != nil {
				t.Fatal(claimErr)
			}
			runErr := m.runRecoveredJob(ctx, claimed)
			if err := store.FinishJob(ctx, job.ID, "test", runErr); err != nil {
				t.Fatal(err)
			}
			if runErr != nil {
				t.Fatalf("job %s(%s): %v", job.ID, job.Kind, runErr)
			}
		}
	}
	t.Fatal("pending jobs did not drain")
}

// 検証に落ちるREADY候補が待機枠の設定値より多くても、残る正常な候補まで見切ることを確かめる。
// 再試行を設定値で打ち切ると、GCや併走leaseに候補を削られただけでcold startへ落ちる。
func TestLeaseTriesEveryReadyCandidateBeyondTheConfiguredWarmSize(t *testing.T) {
	requireDaemonIntegration(t)
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	initGitRepo(t, repo)
	cfg := config.Defaults()
	cfg.Storage.WorktreeRoot = filepath.Join(root, "worktrees")
	cfg.Worktree.Undefined = "hot"
	cfg.Pool.WarmPerWorkspace = 1
	store, err := state.Open(filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	m := testManager(t, cfg, store)
	defer m.Close()
	ctx := context.Background()
	if _, err := m.leaseWithPolicy(ctx, repo, nil, "codex", os.Getpid(), false, leaseAttrs{}); err != nil {
		t.Fatal(err)
	}
	drainPendingJobs(t, m, store)
	workspace, err := store.WorkspaceByRoot(ctx, string(canonicalTestPath(t, repo)))
	if err != nil {
		t.Fatal(err)
	}
	standby, ok, err := store.ReadySlot(ctx, string(workspace.ID))
	if err != nil || !ok {
		t.Fatalf("standby slot=%+v found=%v err=%v", standby, ok, err)
	}

	// 実体を持たない候補を足す。ready_atがNULLなので、ReadySlotはこれらを準備済みslotより先に返す。
	invalid := []string{"invalid-a", "invalid-b"}
	for _, id := range invalid {
		slot := state.Slot{ID: id, WorkspaceID: string(workspace.ID), Generation: standby.Generation, RootID: standby.RootID, RelPath: filepath.Join(string(workspace.ID), id), State: "READY"}
		if _, err := store.CreateStandby(ctx, slot, nil); err != nil {
			t.Fatal(err)
		}
	}
	if count, err := store.ReadySlotCount(ctx, string(workspace.ID)); err != nil || count != 3 {
		t.Fatalf("ready slot count=%d err=%v", count, err)
	}

	lease, err := m.leaseWithPolicy(ctx, repo, nil, "codex", os.Getpid(), false, leaseAttrs{})
	if err != nil {
		t.Fatal(err)
	}
	if !lease.Ready || lease.SessionID != standby.ID {
		t.Fatalf("lease=%+v standby=%s", lease, standby.ID)
	}
	for _, id := range invalid {
		if slot, err := store.Slot(ctx, id); err != nil || slot.State != "STALE" {
			t.Fatalf("invalid candidate slot=%+v err=%v", slot, err)
		}
	}
}
