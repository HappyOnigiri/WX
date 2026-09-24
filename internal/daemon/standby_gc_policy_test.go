package daemon

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/HappyOnigiri/WorktreeX/internal/config"
	"github.com/HappyOnigiri/WorktreeX/internal/discovery"
	"github.com/HappyOnigiri/WorktreeX/internal/gitx"
	"github.com/HappyOnigiri/WorktreeX/internal/state"
)

// standbyGCPolicyFixture は待機枠数 1 の workspace に READY slot を 2 件だけ持つ store を作る。
// mode は worktree 方針の実効値で、workspace 個別指定を置かず全体の既定として与える。
func standbyGCPolicyFixture(t *testing.T, mode string) (context.Context, *Manager, *state.Store, discovery.Workspace) {
	t.Helper()
	root := t.TempDir()
	repository := filepath.Join(root, "repository")
	initGitRepo(t, repository)
	cfg := config.Defaults()
	cfg.Storage.WorktreeRoot = filepath.Join(root, "worktrees")
	cfg.Worktree.Undefined = mode
	cfg.Pool.WarmPerWorkspace = 1
	runner := &gitx.Runner{Timeout: 30 * time.Second}
	ctx := context.Background()
	workspaceRecord, err := (&discovery.Discoverer{Git: runner, Config: cfg}).Resolve(ctx, repository)
	if err != nil {
		t.Fatal(err)
	}
	store, err := openTestStoreAtPath(t, filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	workspaceRecord, _, err = store.UpsertWorkspaceGeneration(ctx, workspaceRecord)
	if err != nil {
		t.Fatal(err)
	}
	manager := testManager(t, cfg, store)
	manager.git = runner
	manager.log = slog.New(slog.NewTextHandler(io.Discard, nil))
	t.Cleanup(manager.Close)
	for _, slotID := range []string{"standby-old", "standby-new"} {
		slotPath := filepath.Join(cfg.Storage.WorktreeRoot, "policy", slotID)
		if _, err := store.CreateStandby(ctx, slotAtPath(t, manager, string(workspaceRecord.ID), slotID, slotPath, 1, "READY"), nil); err != nil {
			t.Fatal(err)
		}
	}
	return ctx, manager, store, workspaceRecord
}

// 補充が hot 限定である以上、方針を hot から外した workspace の READY は GC が全て回収する。
// 方針だけが GC の保持側から漏れると、補充されない待機枠が待機枠数のぶんだけ無期限に残る。
func TestStandbyGCKeepsWarmSlotsOnlyInHotWorktreeMode(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"hot", "cold", "ask", "off"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			ctx, manager, store, _ := standbyGCPolicyFixture(t, mode)
			candidates, err := store.StandbyGCCandidates(ctx, gcWarmFor(manager.Config()))
			if err != nil {
				t.Fatal(err)
			}
			want := 2
			if mode == "hot" {
				// 待機枠数 1 の workspace では最も新しい READY だけが残る。
				want = 1
			}
			if len(candidates) != want {
				t.Fatalf("standby GC candidates=%+v, want %d for worktree mode %s", candidates, want, mode)
			}
		})
	}
}

// workspace 個別の方針が hot なら、全体の方針が hot でなくても待機枠は保持される。
func TestStandbyGCFollowsWorkspaceWorktreeOverride(t *testing.T) {
	t.Parallel()
	ctx, manager, store, workspaceRecord := standbyGCPolicyFixture(t, "off")
	cfg := manager.Config()
	cfg.Workspaces[string(workspaceRecord.Root)] = config.Workspace{Worktree: "hot"}
	manager.cfg = cfg
	candidates, err := store.StandbyGCCandidates(ctx, gcWarmFor(manager.Config()))
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 1 {
		t.Fatalf("standby GC candidates=%+v, want only the slot beyond the warm count", candidates)
	}
}
