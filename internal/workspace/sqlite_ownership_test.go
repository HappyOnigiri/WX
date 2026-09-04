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
	repo := discovery.Repository{ID: domain.RepositoryID(testRepositoryID), MainPath: domain.CanonicalPath(repositoryPath), CommonDir: domain.CanonicalPath(common), RelativePath: ".", DefaultBranch: "main"}
	w := discovery.Workspace{ID: domain.WorkspaceID(testWorkspaceID), Root: domain.CanonicalPath(repositoryPath), Kind: "repository", Repositories: []discovery.Repository{repo}}
	store, err := state.Open(filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	registered, _, err := store.UpsertWorkspaceGeneration(ctx, w)
	if err != nil {
		t.Fatal(err)
	}
	rootID, err := store.EnsureActiveRoot(ctx, worktreeRoot, "test-root-identity")
	if err != nil {
		t.Fatal(err)
	}
	slotRel := filepath.Join(string(registered.ID), testSlotID)
	slotPath := filepath.Join(worktreeRoot, slotRel)
	target := filepath.Join(slotPath, testRepositoryID)
	if _, err := store.CreateStandby(ctx, state.Slot{ID: testSlotID, WorkspaceID: string(registered.ID), Generation: 1, RootID: rootID, RelPath: slotRel, State: "PREPARING"}, []state.SlotRepository{{RepositoryID: string(repo.ID), DirName: testRepositoryID, State: "PREPARING", RequestedRef: "main", BaseOID: head, Fingerprint: "fingerprint"}}); err != nil {
		t.Fatal(err)
	}

	// The forged worktree is a sibling inside the very slot directory the
	// marker belongs to. Since version 2 the marker is named after the
	// repository and shared by the whole slot, so this worktree carries a
	// byte-identical marker and a matching wx lock: only the recorded
	// dir_name distinguishes it from the real one.
	foreignPath := filepath.Join(slotPath, "foreign")
	if err := os.MkdirAll(slotPath, 0o700); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, repositoryPath, "worktree", "add", "--detach", foreignPath, head)
	owner, _, err := domain.OpenOwnedRoot(worktreeRoot, worktreeRoot)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = owner.Close() })
	identity := MarkerIdentity{SlotID: testSlotID, RootID: rootID, RepositoryID: string(repo.ID)}
	if err := EnsureOwnershipMarkerAt(owner, worktreeRoot, foreignPath, identity, common); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, repositoryPath, "worktree", "lock", "--reason", "wx:"+testSlotID+":READY", foreignPath)

	cfg := config.Defaults()
	cfg.Storage.WorktreeRoot = worktreeRoot
	preparer := Preparer{Git: &gitx.Runner{Timeout: 5 * time.Second}, Config: cfg, Ownership: store, SlotPath: slotPath, OwnedRoot: owner, RootPath: worktreeRoot, RootID: rootID, SlotRelPath: slotRel}
	err = preparer.Prepare(ctx, repo, foreignPath, head, testSlotID)
	if !errors.Is(err, state.ErrOwnership) {
		t.Fatalf("forged marker/lock accepted or wrong error: %v", err)
	}
	if _, err := os.Stat(filepath.Join(foreignPath, ".git")); err != nil {
		t.Fatalf("foreign worktree was removed or changed: %v", err)
	}
	lockReason, locked, found, err := RegisteredWorktreeLockStatus(ctx, preparer.Git, string(repo.MainPath), foreignPath)
	if err != nil || !found || !locked || lockReason != "wx:"+testSlotID+":READY" {
		t.Fatalf("foreign lock changed after rejection: reason=%q locked=%v found=%v err=%v", lockReason, locked, found, err)
	}

	preparing := func(request state.WorktreeOwnershipRequest) state.WorktreeOwnershipRequest {
		request.SlotID = testSlotID
		request.RepositoryID = string(repo.ID)
		request.CommonDir = common
		request.AllowedSlotStates = []string{"PREPARING"}
		request.AllowedRepositoryStates = []string{"PREPARING"}
		return request
	}
	for _, test := range []struct {
		name    string
		request state.WorktreeOwnershipRequest
	}{
		{name: "foreign directory name", request: preparing(state.WorktreeOwnershipRequest{RootID: rootID, SlotRelPath: slotRel, DirName: "foreign"})},
		{name: "stale slot location", request: preparing(state.WorktreeOwnershipRequest{RootID: rootID, SlotRelPath: filepath.Join(string(registered.ID), "stale1"), DirName: testRepositoryID})},
		{name: "other root generation", request: preparing(state.WorktreeOwnershipRequest{RootID: "rtzzzz", SlotRelPath: slotRel, DirName: testRepositoryID})},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := store.ValidateWorktreeOwnership(ctx, test.request); !errors.Is(err, state.ErrOwnership) {
				t.Fatalf("mismatched location passed SQLite ownership: %v", err)
			}
		})
	}

	if err := preparer.Prepare(ctx, repo, target, head, testSlotID); err != nil {
		t.Fatalf("owned prepare rejected: %v", err)
	}
	worktreeIdentity, err := preparer.WorktreeIdentity(target)
	if err != nil {
		t.Fatal(err)
	}
	// Presenting an identity before it has been recorded must fail closed:
	// an absent record is not a match.
	if _, err := store.ValidateWorktreeOwnership(ctx, preparing(state.WorktreeOwnershipRequest{RootID: rootID, SlotRelPath: slotRel, DirName: testRepositoryID, DirIdentity: worktreeIdentity})); !errors.Is(err, state.ErrOwnership) {
		t.Fatalf("unrecorded worktree identity passed SQLite ownership: %v", err)
	}
	if err := store.RecordSlotRepositoryIdentity(ctx, testSlotID, string(repo.ID), worktreeIdentity); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ValidateWorktreeOwnership(ctx, preparing(state.WorktreeOwnershipRequest{RootID: rootID, SlotRelPath: slotRel, DirName: testRepositoryID, DirIdentity: "0:0"})); !errors.Is(err, state.ErrOwnership) {
		t.Fatalf("mismatched worktree identity passed SQLite ownership: %v", err)
	}
	if err := store.SetSlotRepositoryState(ctx, testSlotID, string(repo.ID), []string{"PREPARING"}, "READY"); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkReady(ctx, testSlotID); err != nil {
		t.Fatal(err)
	}
	owned, err := store.ValidateWorktreeOwnership(ctx, state.WorktreeOwnershipRequest{
		SlotID: testSlotID, RepositoryID: string(repo.ID), RootID: rootID, SlotRelPath: slotRel,
		DirName: testRepositoryID, DirIdentity: worktreeIdentity, CommonDir: common,
		AllowedSlotStates: []string{"READY"}, AllowedRepositoryStates: []string{"READY"},
	})
	if err != nil {
		t.Fatalf("owned READY location failed SQLite ownership check: %v", err)
	}
	if owned.SlotID != testSlotID || owned.WorkspaceID != string(registered.ID) || owned.RepositoryID != string(repo.ID) ||
		owned.RootID != rootID || owned.RootPath != worktreeRoot || owned.SlotRelPath != slotRel ||
		owned.DirName != testRepositoryID || owned.CommonDir != common || owned.SlotState != "READY" || owned.RepositoryState != "READY" {
		t.Fatalf("owned proof does not preserve exact identity: %+v", owned)
	}
}
