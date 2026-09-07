package daemon

import (
	"os"
	"path/filepath"
	"testing"
)

func TestManagerHandlesUnavailableRootAndZeroLifecycleInterval(t *testing.T) {
	t.Parallel()
	f := runningManagerFixture(t, func(s *managerFixtureSetup) {
		blockedRoot := filepath.Join(s.Root, "blocked-root")
		if err := os.WriteFile(blockedRoot, []byte("file"), 0o600); err != nil {
			t.Fatal(err)
		}
		s.Config.Storage.WorktreeRoot = blockedRoot
		s.Config.Pool.PreparationConcurrency = 0
	})
	manager := f.Manager
	if len(manager.roots) != 0 {
		t.Fatalf("unavailable worktree root was registered: %v", manager.roots)
	}
	manager.Close()

	// 周期処理だけを手で回す Manager は、この検査が自分で作って自分で閉じる。
	lifecycle := testManager(t, f.Config, f.Store)
	lifecycle.cfg.Discovery.ReconcileInterval.Duration = 0
	lifecycle.cancel()
	lifecycle.maintainLifecycle()
	lifecycle.Close()
}
