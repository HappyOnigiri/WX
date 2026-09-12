package daemon

import (
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/HappyOnigiri/WX/internal/config"
)

// 補充と GC は同じ workspace 個別の保持期間で判断する。
// 補充だけが global を見ていると、個別指定が 0 の workspace で待機枠を作っては即座に回収する往復になる。
func TestStandbyReplenishmentFollowsWorkspaceHotStandby(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	cfg := config.Defaults()
	cfg.Storage.WorktreeRoot = filepath.Join(root, "worktrees")
	cfg.Worktree.Undefined = "hot"
	cfg.Pool.WarmPerWorkspace = 1
	cfg.Retention.HotStandby.Duration = time.Hour
	zero := config.Duration{}
	cfg.Workspaces["/stopped"] = config.Workspace{Retention: config.WorkspaceRetention{HotStandby: &zero}}
	store, err := openTestStoreAtPath(t, filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	m := New(cfg, store, slog.New(slog.NewTextHandler(io.Discard, nil)))
	defer m.Close()
	if m.standbyReplenishmentEnabledForRoot("/stopped") {
		t.Fatal("replenishment stayed on for a workspace whose hot_standby is zero")
	}
	if !m.standbyReplenishmentEnabledForRoot("/running") {
		t.Fatal("replenishment stopped for a workspace on the global retention")
	}
	// GC 側の判定も同じ値を使うので、補充が止まった workspace の待機枠は保持されない。
	if hot, _ := m.Config().HotStandbyForWorkspace("/stopped"); hot != 0 {
		t.Fatalf("hot standby=%s, want the workspace override", hot)
	}
	if hot, _ := m.Config().HotStandbyForWorkspace("/running"); hot != time.Hour {
		t.Fatalf("hot standby=%s, want the global retention", hot)
	}
}
