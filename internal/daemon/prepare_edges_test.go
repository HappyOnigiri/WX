package daemon

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/HappyOnigiri/WX/internal/config"
	"github.com/HappyOnigiri/WX/internal/discovery"
	"github.com/HappyOnigiri/WX/internal/state"
)

func TestMultiRepositoryRootMaterializationFailurePersistsFailedState(t *testing.T) {
	t.Parallel()
	f := manualManagerFixture(t, func(s *managerFixtureSetup) {
		s.Config.Workspaces[s.Root] = config.Workspace{Link: []string{"missing-root-entry"}}
	})
	root, cfg, store, m := f.Root, f.Config, f.Store, f.Manager
	ctx := context.Background()
	w := discovery.Workspace{Root: discoveryPath(root), Kind: "multi_repository"}
	w = registerTestWorkspace(t, store, w)
	slotPath := filepath.Join(cfg.Storage.WorktreeRoot, "slot")
	if err := os.MkdirAll(slotPath, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateStandby(ctx, slotAtPath(t, m, string(w.ID), "slot", slotPath, 1, "PREPARING"), nil); err != nil {
		t.Fatal(err)
	}
	if err := m.prepareSlot(ctx, "slot", w, nil, nil); err == nil {
		t.Fatal("root materialization with a missing link succeeded")
	}
	slot, err := store.Slot(ctx, "slot")
	if err != nil || slot.State != "FAILED" {
		t.Fatalf("materialization failure slot=%+v err=%v", slot, err)
	}
	restorePath := filepath.Join(cfg.Storage.WorktreeRoot, "restore")
	if err := os.MkdirAll(restorePath, 0o700); err != nil {
		t.Fatal(err)
	}
	restoreSession := state.Session{ID: "restore", WorkspaceID: string(w.ID), SlotID: "restore", State: "RESTORING", AgentKind: "codex", TokenHash: state.HashToken("restore")}
	if _, err := store.CreateSlotSession(ctx, slotAtPath(t, m, string(w.ID), "restore", restorePath, 1, "RESTORING"), nil, restoreSession, ""); err != nil {
		t.Fatal(err)
	}
	if err := m.restoreSlot(ctx, "restore", w, nil, nil, nil); err == nil {
		t.Fatal("restore root materialization with a missing link succeeded")
	}
	restoredSlot, err := store.Slot(ctx, "restore")
	if err != nil || restoredSlot.State != "FAILED" {
		t.Fatalf("restore materialization failure slot=%+v err=%v", restoredSlot, err)
	}
}
