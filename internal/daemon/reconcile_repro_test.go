package daemon

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/HappyOnigiri/WX/internal/config"
	"github.com/HappyOnigiri/WX/internal/discovery"
	"github.com/HappyOnigiri/WX/internal/state"
)

// TestReconcileArtifactsQuarantinesInFlightReservation は、進行中の確保をreconcileが隔離するかを直接確かめる調査用テストである。
// reconcileArtifacts は ALLOCATING/REGISTERING を無条件に「中断された確保」と見なすため、
// 予約直後に走ると ConfirmSlotCreation が CAS 失敗するはずである。
func TestReconcileArtifactsQuarantinesInFlightReservation(t *testing.T) {
	requireDaemonIntegration(t)
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	initGitRepo(t, repo)
	cfg := config.Defaults()
	cfg.Storage.WorktreeRoot = filepath.Join(root, "worktrees")
	cfg.Worktree.Undefined = "hot"
	cfg.Pool.WarmPerWorkspace = 2
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
	w, generation, err := store.UpsertWorkspaceGeneration(ctx, w)
	if err != nil {
		t.Fatal(err)
	}
	rootPath, rootID, err := m.activeRoot()
	if err != nil {
		t.Fatal(err)
	}
	id, err := newSlotID()
	if err != nil {
		t.Fatal(err)
	}
	relPath, err := slotRelPath(string(w.ID), id)
	if err != nil {
		t.Fatal(err)
	}
	reserved, err := store.ReserveStandbyIfNeeded(ctx, state.Slot{ID: id, WorkspaceID: string(w.ID), Generation: generation, RootID: rootID, RelPath: relPath}, cfg.Pool.WarmPerWorkspace)
	if err != nil || !reserved {
		t.Fatalf("reserve standby slot reserved=%v err=%v", reserved, err)
	}
	// 予約とディレクトリ作成の間にreconcileが走る状況を再現する。
	m.reconcileArtifacts(ctx)
	slot, err := store.Slot(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("after reconcile: state=%s failure_code=%s", slot.State, slot.FailureCode)
	identity, _, err := m.createSlotRoot(filepath.Join(rootPath, relPath), filepath.Join(rootPath, relPath))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.ConfirmSlotCreation(ctx, id, identity); err != nil {
		t.Fatalf("REPRODUCED: %v (slot state after reconcile=%s code=%s)", err, slot.State, slot.FailureCode)
	}
}
