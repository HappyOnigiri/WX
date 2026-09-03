package workspace

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/HappyOnigiri/WX/internal/config"
	"github.com/HappyOnigiri/WX/internal/discovery"
	"github.com/HappyOnigiri/WX/internal/domain"
	"github.com/HappyOnigiri/WX/internal/gitx"
	"github.com/HappyOnigiri/WX/internal/state"
)

func TestPrepareRequiresSQLiteOwnershipForForgedMatchingMarkerAndLock(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	repositoryPath := filepath.Join(root, "repository")
	worktreeRoot := filepath.Join(root, "worktrees")
	if err := os.MkdirAll(repositoryPath, 0o700); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, repositoryPath, "init", "-b", "main")
	gitCommand(t, repositoryPath, "config", "user.name", "test")
	gitCommand(t, repositoryPath, "config", "user.email", "test@example.com")
	if err := os.WriteFile(filepath.Join(repositoryPath, "tracked"), []byte("base\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, repositoryPath, "add", ".")
	gitCommand(t, repositoryPath, "commit", "-m", "initial")
	head := gitOutput(t, repositoryPath, "rev-parse", "HEAD")
	common := gitOutput(t, repositoryPath, "rev-parse", "--path-format=absolute", "--git-common-dir")
	repo := discovery.Repository{ID: domain.RepositoryID("repository"), MainPath: domain.CanonicalPath(repositoryPath), CommonDir: domain.CanonicalPath(common), RelativePath: ".", DefaultBranch: "main"}
	w := discovery.Workspace{ID: domain.WorkspaceID("workspace"), Root: domain.CanonicalPath(repositoryPath), Kind: "repository", Repositories: []discovery.Repository{repo}}
	store, err := state.Open(filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if _, err := store.UpsertWorkspaceGeneration(ctx, w); err != nil {
		t.Fatal(err)
	}
	slotPath := filepath.Join(worktreeRoot, "slots", "slot", "root")
	if _, err := store.CreateStandby(ctx, state.Slot{ID: "slot", WorkspaceID: string(w.ID), Generation: 1, Path: slotPath, State: "PREPARING"}, []state.SlotRepository{{RepositoryID: string(repo.ID), WorktreePath: slotPath, State: "PREPARING", RequestedRef: "main", BaseOID: head, Fingerprint: "fingerprint"}}); err != nil {
		t.Fatal(err)
	}

	foreignPath := filepath.Join(worktreeRoot, "foreign", "root")
	if err := os.MkdirAll(filepath.Dir(foreignPath), 0o700); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, repositoryPath, "worktree", "add", "--detach", foreignPath, head)
	owner, _, err := domain.OpenOwnedRoot(worktreeRoot, worktreeRoot)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = owner.Close() })
	if err := EnsureOwnershipMarkerAt(owner, worktreeRoot, foreignPath, "slot", common); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, repositoryPath, "worktree", "lock", "--reason", "wx:slot:READY", foreignPath)

	cfg := config.Defaults()
	cfg.Storage.WorktreeRoot = worktreeRoot
	preparer := Preparer{Git: &gitx.Runner{Timeout: 5 * time.Second}, Config: cfg, Ownership: store, SlotPath: slotPath, OwnedRoot: owner, RootPath: worktreeRoot}
	err = preparer.Prepare(ctx, repo, foreignPath, head, "slot")
	if !errors.Is(err, state.ErrOwnership) {
		t.Fatalf("forged marker/lock accepted or wrong error: %v", err)
	}
	if _, err := os.Stat(filepath.Join(foreignPath, ".git")); err != nil {
		t.Fatalf("foreign worktree was removed or changed: %v", err)
	}
	lockReason, locked, found, err := RegisteredWorktreeLockStatus(ctx, preparer.Git, string(repo.MainPath), foreignPath)
	if err != nil || !found || !locked || lockReason != "wx:slot:READY" {
		t.Fatalf("foreign lock changed after rejection: reason=%q locked=%v found=%v err=%v", lockReason, locked, found, err)
	}

	_, err = store.ValidateWorktreeOwnership(ctx, state.WorktreeOwnershipRequest{
		SlotID: "slot", RepositoryID: string(repo.ID), SlotPath: slotPath, WorktreePath: foreignPath, CommonDir: common,
		AllowedSlotStates: []string{"PREPARING"}, AllowedRepositoryStates: []string{"PREPARING"},
	})
	if !errors.Is(err, state.ErrOwnership) {
		t.Fatalf("foreign path passed direct SQLite ownership check: %v", err)
	}
	_, err = store.ValidateWorktreeOwnership(ctx, state.WorktreeOwnershipRequest{
		SlotID: "slot", RepositoryID: string(repo.ID), SlotPath: filepath.Join(worktreeRoot, "stale-slot"), WorktreePath: slotPath, CommonDir: common,
		AllowedSlotStates: []string{"PREPARING"}, AllowedRepositoryStates: []string{"PREPARING"},
	})
	if !errors.Is(err, state.ErrOwnership) {
		t.Fatalf("stale slot path passed SQLite ownership check: %v", err)
	}

	if err := preparer.Prepare(ctx, repo, slotPath, head, "slot"); err != nil {
		t.Fatalf("owned prepare rejected: %v", err)
	}
	if err := store.SetSlotRepositoryState(ctx, "slot", string(repo.ID), []string{"PREPARING"}, "READY"); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkReady(ctx, "slot"); err != nil {
		t.Fatal(err)
	}
	owned, err := store.ValidateWorktreeOwnership(ctx, state.WorktreeOwnershipRequest{
		SlotID: "slot", RepositoryID: string(repo.ID), SlotPath: slotPath, WorktreePath: slotPath, CommonDir: common,
		AllowedSlotStates: []string{"READY"}, AllowedRepositoryStates: []string{"READY"},
	})
	if err != nil {
		t.Fatalf("owned READY path failed SQLite ownership check: %v", err)
	}
	if owned.SlotID != "slot" || owned.WorkspaceID != string(w.ID) || owned.RepositoryID != string(repo.ID) || owned.SlotPath != slotPath || owned.WorktreePath != slotPath || owned.CommonDir != common || owned.SlotState != "READY" || owned.RepositoryState != "READY" {
		t.Fatalf("owned proof does not preserve exact identity: %+v", owned)
	}
}
