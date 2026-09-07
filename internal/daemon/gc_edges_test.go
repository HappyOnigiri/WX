package daemon

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/HappyOnigiri/WX/internal/discovery"
	"github.com/HappyOnigiri/WX/internal/state"
)

func TestGCRefusesIncompleteMultiRepositoryRecoveryMetadata(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name              string
		kind              string
		historicalRepos   bool
		workspaceSnapshot string
	}{
		{name: "missing historical membership", kind: "repository"},
		{name: "missing workspace snapshot", kind: "multi_repository", historicalRepos: true},
		{name: "workspace snapshot outside roots", kind: "multi_repository", historicalRepos: true, workspaceSnapshot: "outside"},
		{name: "missing workspace snapshot artifact", kind: "multi_repository", historicalRepos: true, workspaceSnapshot: "missing"},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := manualManagerFixture(t)
			root, store, manager := f.Root, f.Store, f.Manager
			ctx := context.Background()
			repository := discovery.Repository{ID: "repository", MainPath: discoveryPath(filepath.Join(root, "repository")), CommonDir: discoveryPath(filepath.Join(root, "repository", ".git")), DefaultBranch: "main"}
			workspaceRecord := discovery.Workspace{Root: discoveryPath(root), Kind: test.kind, Repositories: []discovery.Repository{repository}}
			workspaceRecord = registerTestWorkspace(t, store, workspaceRecord)
			var slotRepositories []state.SlotRepository
			if test.historicalRepos {
				slotRepositories = []state.SlotRepository{{RepositoryID: "repository", DirName: "repository", State: "ARCHIVED"}}
			}
			session := state.Session{ID: "session", WorkspaceID: string(workspaceRecord.ID), SlotID: "slot", State: "ARCHIVED", AgentKind: "codex", TokenHash: state.HashToken("token")}
			slot := testSlotRow(t, manager, string(workspaceRecord.ID), "slot", 0, "SNAPSHOTTED")
			if _, err := store.CreateSlotSession(ctx, slot, slotRepositories, session, ""); err != nil {
				t.Fatal(err)
			}
			expired := state.FormatTime(time.Now().Add(-time.Hour))
			if err := store.SaveSnapshot(ctx, state.Snapshot{ID: "snapshot", SessionID: session.ID, RepositoryID: "repository", HeadOID: "head", HeadRef: "refs/wx/recovery/head", IndexTreeOID: "index", WorktreeOID: "worktree", WorktreeRef: "refs/wx/recovery/worktree", Status: "ARCHIVED", CreatedAt: expired, ExpiresAt: expired}); err != nil {
				t.Fatal(err)
			}
			if err := store.SetSlotState(ctx, session.SlotID, []string{"SNAPSHOTTED"}, "ARCHIVED", ""); err != nil {
				t.Fatal(err)
			}
			if test.workspaceSnapshot != "" {
				relPath := filepath.Join("..", "outside", "snapshot.tar")
				if test.workspaceSnapshot == "missing" {
					relPath = filepath.Join("_recovery", "workspace-snapshots", "missing.tar")
				}
				if err := store.SaveWorkspaceSnapshot(ctx, state.WorkspaceSnapshot{SessionID: session.ID, RootID: slot.RootID, RelPath: relPath, SHA256: strings.Repeat("a", 64), Status: "ARCHIVED", CreatedAt: expired, ExpiresAt: expired}); err != nil {
					t.Fatal(err)
				}
			}
			result, err := manager.GC(ctx, false)
			if err == nil && result.Pending == 0 && result.Failed == 0 {
				t.Fatalf("unsafe recovery metadata was reported as complete: result=%+v", result)
			}
			if len(result.Reasons) == 0 {
				t.Fatalf("incomplete recovery metadata lost its reason: result=%+v err=%v", result, err)
			}
			if snapshots, err := store.Snapshots(ctx, session.ID); err != nil || len(snapshots) != 1 {
				t.Fatalf("recovery metadata was discarded: snapshots=%+v err=%v", snapshots, err)
			}
		})
	}
}
