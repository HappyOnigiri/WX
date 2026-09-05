package archive

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/HappyOnigiri/WX/internal/discovery"
	"github.com/HappyOnigiri/WX/internal/domain"
	"github.com/HappyOnigiri/WX/internal/state"
	"github.com/HappyOnigiri/WX/internal/workspace"
)

func TestRemoveWorktreeRequiresSQLiteOwnershipForForgedMatchingMarkerAndLock(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	repositoryPath := filepath.Join(root, "repository")
	worktreeRoot := filepath.Join(root, "worktrees")
	mustMkdir(t, repositoryPath)
	mustMkdir(t, worktreeRoot)
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
	w := discovery.Workspace{ID: domain.WorkspaceID("wsaaaa"), Root: domain.CanonicalPath(repositoryPath), Kind: "repository", Repositories: []discovery.Repository{repo}}
	store, err := state.Open(filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	w, _, err = store.UpsertWorkspaceGeneration(ctx, w)
	if err != nil {
		t.Fatal(err)
	}
	// slot は root generation と root 相対 path で位置付けるため、slot が参照する前に root row が必要である。
	rootIdentity, err := ownedRootIdentity(worktreeRoot)
	if err != nil {
		t.Fatal(err)
	}
	rootID, err := store.EnsureActiveRoot(ctx, worktreeRoot, rootIdentity)
	if err != nil {
		t.Fatal(err)
	}
	slotRel := filepath.Join(string(w.ID), "slotaa")
	slotPath := filepath.Join(worktreeRoot, slotRel)
	ownedPath := filepath.Join(slotPath, "repo")
	if _, err := store.CreateStandby(ctx, state.Slot{ID: "slotaa", WorkspaceID: string(w.ID), Generation: 1, RootID: rootID, RelPath: slotRel, State: "REMOVING"}, []state.SlotRepository{{RepositoryID: string(repo.ID), DirName: "repo", State: "READY", RequestedRef: "main", BaseOID: head, Fingerprint: "fingerprint"}}); err != nil {
		t.Fatal(err)
	}

	// marker と wx lock を持つ未登録 directory の worktree は正しく見えても拒否する。
	// どの location が slot に属するかは SQLite だけが証明できる。
	foreignSlotRel := filepath.Join(string(w.ID), "forgnn")
	foreignPath := filepath.Join(worktreeRoot, foreignSlotRel, "repo")
	mustMkdir(t, filepath.Dir(foreignPath))
	gitCommand(t, repositoryPath, "worktree", "add", "--detach", foreignPath, head)
	foreignOwner, _, err := domain.OpenOwnedRoot(worktreeRoot, worktreeRoot)
	if err != nil {
		t.Fatal(err)
	}
	if err := workspace.EnsureOwnershipMarkerAt(foreignOwner, worktreeRoot, foreignPath, workspace.MarkerIdentity{SlotID: "slotaa", RootID: rootID, RepositoryID: string(repo.ID)}, common); err != nil {
		_ = foreignOwner.Close()
		t.Fatal(err)
	}
	if err := foreignOwner.Close(); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, repositoryPath, "worktree", "lock", "--reason", "wx:slotaa:READY", foreignPath)

	manager := removalManager(t, worktreeRoot, store)
	manager.Preparer.RootID = rootID
	manager.Preparer.SlotPath = filepath.Dir(foreignPath)
	manager.Preparer.SlotRelPath = foreignSlotRel
	err = manager.RemoveWorktree(ctx, repo, worktreeRoot, foreignPath, head)
	if !errors.Is(err, state.ErrOwnership) {
		t.Fatalf("forged marker/lock authorized removal or wrong error: %v", err)
	}
	if _, err := os.Stat(filepath.Join(foreignPath, ".git")); err != nil {
		t.Fatalf("foreign worktree was removed or changed: %v", err)
	}
	lockReason, locked, found, err := workspace.RegisteredWorktreeLockStatus(ctx, manager.Git, string(repo.MainPath), foreignPath)
	if err != nil || !found || !locked || lockReason != "wx:slotaa:READY" {
		t.Fatalf("foreign lock changed after rejected removal: reason=%q locked=%v found=%v err=%v", lockReason, locked, found, err)
	}

	for name, request := range map[string]state.WorktreeOwnershipRequest{
		"wrong root generation": {
			SlotID: "slotaa", RepositoryID: string(repo.ID), RootID: "rtzzzz", SlotRelPath: slotRel, DirName: "repo", CommonDir: common,
			AllowedSlotStates: []string{"REMOVING"}, AllowedRepositoryStates: []string{"READY"},
		},
		"stale slot location": {
			SlotID: "slotaa", RepositoryID: string(repo.ID), RootID: rootID, SlotRelPath: filepath.Join(string(w.ID), "stalee"), DirName: "repo", CommonDir: common,
			AllowedSlotStates: []string{"REMOVING"}, AllowedRepositoryStates: []string{"READY"},
		},
		"wrong repository directory": {
			SlotID: "slotaa", RepositoryID: string(repo.ID), RootID: rootID, SlotRelPath: slotRel, DirName: "misplaced", CommonDir: common,
			AllowedSlotStates: []string{"REMOVING"}, AllowedRepositoryStates: []string{"READY"},
		},
		"unrecorded directory identity": {
			SlotID: "slotaa", RepositoryID: string(repo.ID), RootID: rootID, SlotRelPath: slotRel, DirName: "repo", DirIdentity: "1:2", CommonDir: common,
			AllowedSlotStates: []string{"REMOVING"}, AllowedRepositoryStates: []string{"READY"},
		},
		"wrong common directory": {
			SlotID: "slotaa", RepositoryID: string(repo.ID), RootID: rootID, SlotRelPath: slotRel, DirName: "repo", CommonDir: filepath.Join(root, "other-common.git"),
			AllowedSlotStates: []string{"REMOVING"}, AllowedRepositoryStates: []string{"READY"},
		},
		"ineligible state": {
			SlotID: "slotaa", RepositoryID: string(repo.ID), RootID: rootID, SlotRelPath: slotRel, DirName: "repo", CommonDir: common,
			AllowedSlotStates: []string{"READY"}, AllowedRepositoryStates: []string{"READY"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := store.ValidateWorktreeOwnership(ctx, request); !errors.Is(err, state.ErrOwnership) {
				t.Fatalf("invalid ownership proof succeeded: %v", err)
			}
		})
	}

	mustMkdir(t, slotPath)
	gitCommand(t, repositoryPath, "worktree", "add", "--detach", ownedPath, head)
	gitCommand(t, repositoryPath, "worktree", "lock", "--reason", "wx:slotaa:READY", ownedPath)
	manager.Preparer.SlotPath = slotPath
	manager.Preparer.SlotRelPath = slotRel
	if err := manager.RemoveWorktree(ctx, repo, worktreeRoot, ownedPath, head); !errors.Is(err, state.ErrOwnership) {
		t.Fatalf("missing ownership marker authorized removal or wrong error: %v", err)
	}
	if _, err := os.Stat(filepath.Join(ownedPath, ".git")); err != nil {
		t.Fatalf("unmarked worktree was removed or changed: %v", err)
	}
	ownedOwner, _, err := domain.OpenOwnedRoot(worktreeRoot, worktreeRoot)
	if err != nil {
		t.Fatal(err)
	}
	if err := workspace.EnsureOwnershipMarkerAt(ownedOwner, worktreeRoot, ownedPath, workspace.MarkerIdentity{SlotID: "slotaa", RootID: rootID, RepositoryID: string(repo.ID)}, common); err != nil {
		_ = ownedOwner.Close()
		t.Fatal(err)
	}
	if err := ownedOwner.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ValidateWorktreeOwnership(ctx, state.WorktreeOwnershipRequest{
		SlotID: "slotaa", RepositoryID: string(repo.ID), RootID: rootID, SlotRelPath: slotRel, DirName: "repo", CommonDir: common,
		AllowedSlotStates: []string{"REMOVING"}, AllowedRepositoryStates: []string{"READY"},
	}); err != nil {
		t.Fatalf("owned removal proof failed: %v", err)
	}
	// identity を記録後は、一致しない inode identity の提示を拒否する。
	identity, err := manager.Preparer.WorktreeIdentity(ownedPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.RecordSlotRepositoryIdentity(ctx, "slotaa", string(repo.ID), identity); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ValidateWorktreeOwnership(ctx, state.WorktreeOwnershipRequest{
		SlotID: "slotaa", RepositoryID: string(repo.ID), RootID: rootID, SlotRelPath: slotRel, DirName: "repo", DirIdentity: identity, CommonDir: common,
		AllowedSlotStates: []string{"REMOVING"}, AllowedRepositoryStates: []string{"READY"},
	}); err != nil {
		t.Fatalf("recorded identity proof failed: %v", err)
	}
	if _, err := store.ValidateWorktreeOwnership(ctx, state.WorktreeOwnershipRequest{
		SlotID: "slotaa", RepositoryID: string(repo.ID), RootID: rootID, SlotRelPath: slotRel, DirName: "repo", DirIdentity: identity + "0", CommonDir: common,
		AllowedSlotStates: []string{"REMOVING"}, AllowedRepositoryStates: []string{"READY"},
	}); !errors.Is(err, state.ErrOwnership) {
		t.Fatalf("mismatched identity proof succeeded: %v", err)
	}

	// 削除では開いている directory の identity を提示する必要がある。
	// 記録値が disk 上の inode と一致しなければ、破壊的な呼出しに進めてはならない。
	if err := store.RecordSlotRepositoryIdentity(ctx, "slotaa", string(repo.ID), identity+"0"); err != nil {
		t.Fatal(err)
	}
	if err := manager.RemoveWorktree(ctx, repo, worktreeRoot, ownedPath, head); !errors.Is(err, state.ErrOwnership) {
		t.Fatalf("removal with a mismatched recorded identity succeeded: %v", err)
	}
	if _, err := os.Stat(filepath.Join(ownedPath, ".git")); err != nil {
		t.Fatalf("worktree was removed despite the identity mismatch: %v", err)
	}
	if err := store.RecordSlotRepositoryIdentity(ctx, "slotaa", string(repo.ID), identity); err != nil {
		t.Fatal(err)
	}

	transitionValidator := &transitionOwnershipValidator{store: store}
	transitionManager := removalManager(t, worktreeRoot, transitionValidator)
	transitionManager.Preparer.RootID = rootID
	transitionManager.Preparer.SlotPath = slotPath
	transitionManager.Preparer.SlotRelPath = slotRel
	if err := transitionManager.RemoveWorktree(ctx, repo, worktreeRoot, ownedPath, head); !errors.Is(err, state.ErrOwnership) {
		t.Fatalf("state transition during removal was not rejected: %v", err)
	}
	if _, err := os.Stat(filepath.Join(ownedPath, ".git")); err != nil {
		t.Fatalf("worktree was removed after state transition race: %v", err)
	}
	if slot, err := store.Slot(ctx, "slotaa"); err != nil || slot.State != "READY" {
		t.Fatalf("transition race did not leave durable state unchanged by the guard: slot=%+v err=%v", slot, err)
	}
	if err := store.SetSlotState(ctx, "slotaa", []string{"READY"}, "REMOVING", "test-reset"); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, repositoryPath, "worktree", "lock", "--reason", "wx:slotaa:READY", ownedPath)
	if err := manager.RemoveWorktree(ctx, repo, worktreeRoot, ownedPath, head); err != nil {
		t.Fatalf("owned worktree removal failed: %v", err)
	}
	if _, err := os.Stat(ownedPath); !os.IsNotExist(err) {
		t.Fatalf("owned worktree still exists: %v", err)
	}
	if list := gitCommand(t, repositoryPath, "worktree", "list", "--porcelain"); strings.Contains(list, ownedPath) {
		t.Fatalf("owned worktree registration remains: %s", list)
	}
	// marker は slot directory にあるため、中断した削除でも再試行時に所有権を証明できる。
	if _, err := os.Stat(filepath.Join(slotPath, ".wx-owner-"+string(repo.ID))); err != nil {
		t.Fatalf("ownership marker was removed with the worktree: %v", err)
	}
}

// ownedRootIdentity は、daemon が root generation として登録する前の wx root の dev:ino identity を返す。
func ownedRootIdentity(root string) (string, error) {
	owner, _, err := domain.OpenOwnedRoot(root, root)
	if err != nil {
		return "", err
	}
	defer func() { _ = owner.Close() }()
	info, err := owner.Lstat(".")
	if err != nil {
		return "", err
	}
	return domain.FileIdentity(info)
}

type transitionOwnershipValidator struct {
	store *state.Store
	once  sync.Once
}

func (v *transitionOwnershipValidator) ValidateWorktreeOwnership(ctx context.Context, req state.WorktreeOwnershipRequest) (state.WorktreeOwnership, error) {
	proof, err := v.store.ValidateWorktreeOwnership(ctx, req)
	if err != nil {
		return proof, err
	}
	v.once.Do(func() {
		_ = v.store.SetSlotState(context.Background(), req.SlotID, []string{"REMOVING"}, "READY", "test-transition")
	})
	return proof, nil
}
