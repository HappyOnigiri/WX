package archive

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/HappyOnigiri/WX/internal/discovery"
	"github.com/HappyOnigiri/WX/internal/domain"
	"github.com/HappyOnigiri/WX/internal/gitx"
	"github.com/HappyOnigiri/WX/internal/state"
	"github.com/HappyOnigiri/WX/internal/workspace"
)

func TestRemoveWorktreeRequiresSQLiteOwnershipForForgedMatchingMarkerAndLock(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	repositoryPath := filepath.Join(root, "repository")
	worktreeRoot := filepath.Join(root, "worktrees")
	mustMkdir(t, repositoryPath)
	gitCommand(t, repositoryPath, "init", "-b", "main")
	gitCommand(t, repositoryPath, "config", "user.name", "test")
	gitCommand(t, repositoryPath, "config", "user.email", "test@example.com")
	if err := os.WriteFile(filepath.Join(repositoryPath, "tracked"), []byte("base\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, repositoryPath, "add", ".")
	gitCommand(t, repositoryPath, "commit", "-m", "initial")
	head := gitCommand(t, repositoryPath, "rev-parse", "HEAD")
	common := gitCommand(t, repositoryPath, "rev-parse", "--path-format=absolute", "--git-common-dir")
	repo := discovery.Repository{ID: domain.RepositoryID("repository"), MainPath: domain.CanonicalPath(repositoryPath), CommonDir: domain.CanonicalPath(common), RelativePath: ".", DefaultBranch: "main"}
	w := discovery.Workspace{ID: domain.WorkspaceID("workspace"), Root: domain.CanonicalPath(repositoryPath), Kind: "repository", Repositories: []discovery.Repository{repo}}
	store, err := state.Open(filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.UpsertWorkspace(ctx, w); err != nil {
		t.Fatal(err)
	}
	ownedPath := filepath.Join(worktreeRoot, "slots", "slot", "root")
	if _, err := store.CreateStandby(ctx, state.Slot{ID: "slot", WorkspaceID: string(w.ID), Generation: 1, Path: ownedPath, State: "REMOVING"}, []state.SlotRepository{{RepositoryID: string(repo.ID), WorktreePath: ownedPath, State: "READY", RequestedRef: "main", BaseOID: head, Fingerprint: "fingerprint"}}); err != nil {
		t.Fatal(err)
	}

	foreignPath := filepath.Join(worktreeRoot, "foreign", "root")
	mustMkdir(t, filepath.Dir(foreignPath))
	gitCommand(t, repositoryPath, "worktree", "add", "--detach", foreignPath, head)
	if err := workspace.EnsureOwnershipMarker(worktreeRoot, foreignPath, "slot", common); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, repositoryPath, "worktree", "lock", "--reason", "wx:slot:READY", foreignPath)

	manager := &Manager{Git: &gitx.Runner{Timeout: 5 * time.Second}, Ownership: store}
	err = manager.RemoveWorktree(ctx, repo, worktreeRoot, foreignPath, head)
	if !errors.Is(err, state.ErrOwnership) {
		t.Fatalf("forged marker/lock authorized removal or wrong error: %v", err)
	}
	if _, err := os.Stat(filepath.Join(foreignPath, ".git")); err != nil {
		t.Fatalf("foreign worktree was removed or changed: %v", err)
	}
	lockReason, locked, found, err := workspace.RegisteredWorktreeLockStatus(ctx, manager.Git, string(repo.MainPath), foreignPath)
	if err != nil || !found || !locked || lockReason != "wx:slot:READY" {
		t.Fatalf("foreign lock changed after rejected removal: reason=%q locked=%v found=%v err=%v", lockReason, locked, found, err)
	}

	for name, request := range map[string]state.WorktreeOwnershipRequest{
		"stale path": {
			SlotID: "slot", RepositoryID: string(repo.ID), SlotPath: filepath.Join(worktreeRoot, "stale-slot"), WorktreePath: ownedPath, CommonDir: common,
			AllowedSlotStates: []string{"REMOVING"}, AllowedRepositoryStates: []string{"READY"},
		},
		"wrong common directory": {
			SlotID: "slot", RepositoryID: string(repo.ID), WorktreePath: ownedPath, CommonDir: filepath.Join(root, "other-common.git"),
			AllowedSlotStates: []string{"REMOVING"}, AllowedRepositoryStates: []string{"READY"},
		},
		"ineligible state": {
			SlotID: "slot", RepositoryID: string(repo.ID), WorktreePath: ownedPath, CommonDir: common,
			AllowedSlotStates: []string{"READY"}, AllowedRepositoryStates: []string{"READY"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := store.ValidateWorktreeOwnership(ctx, request); !errors.Is(err, state.ErrOwnership) {
				t.Fatalf("invalid ownership proof succeeded: %v", err)
			}
		})
	}

	mustMkdir(t, filepath.Dir(ownedPath))
	gitCommand(t, repositoryPath, "worktree", "add", "--detach", ownedPath, head)
	if err := workspace.EnsureOwnershipMarker(worktreeRoot, ownedPath, "slot", common); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, repositoryPath, "worktree", "lock", "--reason", "wx:slot:READY", ownedPath)
	if _, err := store.ValidateWorktreeOwnership(ctx, state.WorktreeOwnershipRequest{
		SlotID: "slot", RepositoryID: string(repo.ID), SlotPath: ownedPath, WorktreePath: ownedPath, CommonDir: common,
		AllowedSlotStates: []string{"REMOVING"}, AllowedRepositoryStates: []string{"READY"},
	}); err != nil {
		t.Fatalf("owned removal proof failed: %v", err)
	}
	if err := manager.RemoveWorktree(ctx, repo, worktreeRoot, ownedPath, head); err != nil {
		t.Fatalf("owned worktree removal failed: %v", err)
	}
	if _, err := os.Stat(ownedPath); !os.IsNotExist(err) {
		t.Fatalf("owned worktree still exists: %v", err)
	}
	if list := gitCommand(t, repositoryPath, "worktree", "list", "--porcelain"); strings.Contains(list, ownedPath) {
		t.Fatalf("owned worktree registration remains: %s", list)
	}
}
