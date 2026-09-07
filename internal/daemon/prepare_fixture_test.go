package daemon

import (
	"os"
	"testing"

	"github.com/HappyOnigiri/WX/internal/config"
	"github.com/HappyOnigiri/WX/internal/domain"
	"github.com/HappyOnigiri/WX/internal/gitx"
	"github.com/HappyOnigiri/WX/internal/state"
	"github.com/HappyOnigiri/WX/internal/workspace"
)

func descriptorBoundPreparerForTest(t *testing.T, runner *gitx.Runner, cfg config.Config, store *state.Store, slot state.Slot) workspace.Preparer {
	t.Helper()
	root := cfg.Storage.WorktreeRoot
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	owner, _, err := domain.OpenOwnedRoot(root, root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = owner.Close() })
	return workspace.Preparer{
		Git: runner, Config: cfg, Ownership: store, OwnedRoot: owner, RootPath: root,
		SlotPath: slot.Path, RootID: slot.RootID, SlotRelPath: slot.RelPath,
	}
}
