package daemon

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/HappyOnigiri/WX/internal/archive"
	"github.com/HappyOnigiri/WX/internal/discovery"
	"github.com/HappyOnigiri/WX/internal/state"
)

func TestRemoveSlotWorktreesRefusesIncompleteRecoveryMetadata(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name              string
		kind              string
		workspaceSnapshot string
	}{
		{name: "missing repository snapshot", kind: "repository"},
		{name: "missing workspace snapshot", kind: "multi_repository"},
		{name: "workspace snapshot outside roots", kind: "multi_repository", workspaceSnapshot: "outside"},
		{name: "missing workspace snapshot artifact", kind: "multi_repository", workspaceSnapshot: "missing"},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := manualManagerFixture(t)
			root, cfg, store, manager := f.Root, f.Config, f.Store, f.Manager
			ctx := context.Background()
			repository := discovery.Repository{ID: "repository", MainPath: discoveryPath(filepath.Join(root, "repository")), CommonDir: discoveryPath(filepath.Join(root, "repository", ".git")), DefaultBranch: "main"}
			workspaceRecord := discovery.Workspace{ID: "workspace", Root: discoveryPath(root), Kind: test.kind, Repositories: []discovery.Repository{repository}}
			registered, _, err := store.UpsertWorkspaceGeneration(ctx, workspaceRecord)
			if err != nil {
				t.Fatal(err)
			}
			workspaceID := string(registered.ID)
			slot := testSlot(t, manager, workspaceID, "slot", 1, "SNAPSHOTTED")
			session := state.Session{ID: "session", WorkspaceID: workspaceID, SlotID: "slot", State: "ARCHIVED", AgentKind: "codex", TokenHash: state.HashToken("token")}
			slotRepositories := []state.SlotRepository{{RepositoryID: "repository", DirName: "repository", State: "ARCHIVED"}}
			if _, err := store.CreateSlotSession(ctx, slot, slotRepositories, session, ""); err != nil {
				t.Fatal(err)
			}
			if test.workspaceSnapshot != "" {
				rootID := slot.RootID
				relPath := filepath.Join("..", "outside", "snapshot.tar")
				if test.workspaceSnapshot == "missing" {
					relPath = filepath.Join("_recovery", "workspace-snapshots", "missing.tar")
				}
				if err := store.SaveWorkspaceSnapshot(ctx, state.WorkspaceSnapshot{SessionID: session.ID, RootID: rootID, RelPath: relPath, SHA256: strings.Repeat("a", 64), Status: "ARCHIVED", CreatedAt: state.FormatTime(time.Now()), ExpiresAt: state.FormatTime(time.Now().Add(time.Hour))}); err != nil {
					t.Fatal(err)
				}
			}
			err = manager.removeSlotWorktrees(ctx, archive.Manager{}, cfg.Storage.WorktreeRoot, slot, session.ID)
			if err == nil {
				t.Fatal("worktree removal with incomplete recovery metadata succeeded")
			}
		})
	}
}
