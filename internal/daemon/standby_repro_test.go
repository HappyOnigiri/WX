package daemon

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/HappyOnigiri/WX/internal/config"
	"github.com/HappyOnigiri/WX/internal/discovery"
	"github.com/HappyOnigiri/WX/internal/state"
)

// TestStandbyBuiltBeforeFirstLeaseIsCold は、貸出実績が記録される前に補充が走ると待機枠がCOLDで作られ、
// 次の貸出が warm でも Ready=false になることを確かめる調査用テストである。
// 起動直後の reconcileRegistry は最初の lease と並走するため、CIの負荷下ではこの順序が実際に起きる。
func TestStandbyBuiltBeforeFirstLeaseIsCold(t *testing.T) {
	requireDaemonIntegration(t)
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	initGitRepo(t, repo)
	cfg := config.Defaults()
	cfg.Storage.WorktreeRoot = filepath.Join(root, "worktrees")
	cfg.Worktree.Undefined = "hot"
	cfg.Pool.WarmPerWorkspace = 2
	cfg.Pool.PreparationConcurrency = 3
	cfg.Retention.HotStandby.Duration = time.Hour
	cfg.Discovery.ReconcileInterval.Duration = time.Hour
	store, err := state.Open(filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	m := New(cfg, store, newFlakeLogger(t))
	defer m.Close()
	ctx := context.Background()
	discoverer := discovery.Discoverer{Git: m.git, Config: cfg}
	w, err := discoverer.Resolve(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	w, err = store.CanonicalWorkspace(ctx, w)
	if err != nil {
		t.Fatal(err)
	}
	w, _, err = store.UpsertWorkspaceGeneration(ctx, w)
	if err != nil {
		t.Fatal(err)
	}
	// last_leased_at が入る前の補充。起動直後の reconcileRegistry が最初のleaseへ割り込んだ状態に相当する。
	if err := m.ensureStandby(ctx, w); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, 20*time.Second, func() bool {
		count, _ := store.ReadySlotCount(ctx, string(w.ID))
		return count >= cfg.Pool.WarmPerWorkspace
	})
	slots, err := store.ReadySlots(ctx, string(w.ID))
	if err != nil {
		t.Fatal(err)
	}
	for _, slot := range slots {
		repos, reposErr := store.SlotRepositories(ctx, slot.ID)
		t.Logf("standby slot=%s repositories=%+v err=%v", slot.ID, repos, reposErr)
	}
	lease, err := m.ResolveAndLease(ctx, repo, nil, "codex", os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	if !lease.Ready {
		t.Fatalf("REPRODUCED: warm lease was not ready: %+v", lease)
	}
}
