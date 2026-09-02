package archive

import (
	"context"
	"errors"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/HappyOnigiri/WX/internal/discovery"
	"github.com/HappyOnigiri/WX/internal/domain"
	"github.com/HappyOnigiri/WX/internal/state"
	"github.com/HappyOnigiri/WX/internal/workspace"
)

func TestRemovalOwnershipHelpersFailClosed(t *testing.T) {
	if removalOwnershipFailure(nil) != nil {
		t.Fatal("nil removal error was wrapped")
	}
	if !errors.Is(removalOwnershipFailure(state.ErrOwnership), state.ErrOwnership) {
		t.Fatal("ownership sentinel was replaced")
	}
	if err := removalOwnershipFailure(errors.New("filesystem fault")); !errors.Is(err, state.ErrOwnership) || !strings.Contains(err.Error(), "filesystem fault") {
		t.Fatalf("wrapped removal error=%v", err)
	}
	for _, reason := range []string{"wx:slot:READY", "wx:slot:PREPARING", "wx:slot:RESTORING"} {
		if err := validateWxLockReason(reason, "slot"); err != nil {
			t.Fatalf("valid lock reason %q rejected: %v", reason, err)
		}
	}
	if err := validateWxLockReason("", "slot"); err == nil {
		t.Fatal("empty lock reason accepted")
	}
	if err := validateWxLockReason("wx:other:READY", "slot"); err == nil {
		t.Fatal("foreign lock reason accepted")
	}

	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "slot", "root"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := validateRemovalPathComponents(root, filepath.Join("slot", "root")); err != nil {
		t.Fatal(err)
	}
	if err := validateRemovalPathComponents(root, filepath.Join("slot", "missing")); err != nil {
		t.Fatalf("missing leaf rejected: %v", err)
	}
	if err := validateRemovalPathComponents(root, filepath.Join("missing", "leaf")); err == nil {
		t.Fatal("missing intermediate path accepted")
	}
	if err := os.Symlink(t.TempDir(), filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	if err := validateRemovalPathComponents(root, "link/child"); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("symlink removal path error=%v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "file"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := validateRemovalPathComponents(root, "file/child"); err == nil {
		t.Fatal("path below regular file accepted")
	}

	repo := discovery.Repository{ID: "repository", MainPath: domain.CanonicalPath(root), CommonDir: domain.CanonicalPath(root)}
	manager := &Manager{}
	if err := manager.validateStateOwnership(context.Background(), repo, "target", "slot", nil, nil); !errors.Is(err, state.ErrOwnership) {
		t.Fatalf("missing ownership validator error=%v", err)
	}
	manager.Preparer = &workspace.Preparer{Ownership: allowOwnershipValidator{}}
	if err := manager.validateStateOwnership(context.Background(), repo, "target", "slot", []string{"READY"}, []string{"READY"}); err != nil {
		t.Fatalf("preparer ownership fallback=%v", err)
	}
	manager.Ownership = rejectingOwnershipValidator{err: errors.New("proof changed")}
	if err := manager.validateStateOwnership(context.Background(), repo, "target", "slot", nil, nil); !strings.Contains(err.Error(), "proof changed") {
		t.Fatalf("validator failure=%v", err)
	}
}

type rejectingOwnershipValidator struct{ err error }

func (v rejectingOwnershipValidator) ValidateWorktreeOwnership(context.Context, state.WorktreeOwnershipRequest) (state.WorktreeOwnership, error) {
	return state.WorktreeOwnership{}, v.err
}

func TestArchivePathAndWorkspaceRuleHelpers(t *testing.T) {
	for _, value := range []string{"", ".", "..", "../escape", "/absolute", "a/../b", `back\slash`, "nul\x00byte"} {
		if _, err := archiveRelative(value); err == nil {
			t.Fatalf("unsafe archive path %q accepted", value)
		}
	}
	for _, value := range []string{"file", "nested/file", "a-b_1"} {
		if got, err := archiveRelative(value); err != nil || got != value {
			t.Fatalf("safe archive path %q -> %q err=%v", value, got, err)
		}
	}
	if got, err := normalizeWorkspaceExclusions([]string{"z", "a", "z", "a/b"}); err != nil || !reflect.DeepEqual(got, []string{"a", "a/b", "z"}) {
		t.Fatalf("normalized exclusions=%v err=%v", got, err)
	}
	if _, err := normalizeWorkspaceExclusions([]string{"../escape"}); err == nil {
		t.Fatal("unsafe workspace exclusion accepted")
	}
	if !workspacePathExcluded("repo/file", []string{"repo"}) || workspacePathExcluded("repository", []string{"repo"}) {
		t.Fatal("workspace exclusion boundary failed")
	}
	if !workspacePathContainsExclusion("repo", []string{"repo/file"}) || workspacePathContainsExclusion("repo/file", []string{"repo"}) {
		t.Fatal("workspace exclusion ancestor boundary failed")
	}
	if got := workspaceSnapshotRelativePath("session"); got != path.Join(workspaceSnapshotDirectory, domain.StableID("workspace-snapshot", "session")+".tar") {
		t.Fatalf("snapshot relative path=%q", got)
	}
}

func TestArchiveGitValueAndRestorePreconditions(t *testing.T) {
	repository, repo, manager, _ := archiveFixture(t)
	value, err := manager.gitValue(context.Background(), repository, nil, "rev-parse", "HEAD")
	if err != nil || value == "" {
		t.Fatalf("git value=%q err=%v", value, err)
	}
	if _, err := manager.gitValue(context.Background(), repository, nil, "rev-parse", "not-a-ref"); err == nil {
		t.Fatal("invalid Git value succeeded")
	}
	snapshot, err := manager.Snapshot(context.Background(), repo, repository, "precondition", time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	noPreparer := &Manager{Git: manager.Git}
	if err := noPreparer.Restore(context.Background(), repo, filepath.Join(filepath.Dir(repository), "target"), "slot", snapshot); err == nil || !strings.Contains(err.Error(), "preparer") {
		t.Fatalf("restore precondition error=%v", err)
	}
	snapshot.ExpiresAt = "not-time"
	if err := noPreparer.Restore(context.Background(), repo, repository, "slot", snapshot); err == nil {
		t.Fatal("invalid expiry restore succeeded")
	}
}

func TestWorkspaceSnapshotPreconditionsAndPruneFailures(t *testing.T) {
	ctx := context.Background()
	ownershipRoot := t.TempDir()
	bundleRoot := filepath.Join(ownershipRoot, "bundle")
	if err := os.Mkdir(bundleRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if _, err := SnapshotWorkspace(ctx, outside, ownershipRoot, "outside", nil, time.Now().Add(time.Hour)); err == nil {
		t.Fatal("workspace snapshot accepted a bundle outside the ownership root")
	}
	missing := filepath.Join(ownershipRoot, "missing")
	if _, err := SnapshotWorkspace(ctx, missing, ownershipRoot, "missing", nil, time.Now().Add(time.Hour)); err == nil {
		t.Fatal("workspace snapshot accepted a missing bundle root")
	}
	fileRoot := filepath.Join(ownershipRoot, "file-root")
	if err := os.WriteFile(fileRoot, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := SnapshotWorkspace(ctx, fileRoot, ownershipRoot, "file", nil, time.Now().Add(time.Hour)); err == nil {
		t.Fatal("workspace snapshot accepted a regular-file bundle root")
	}
	if _, err := SnapshotWorkspace(ctx, bundleRoot, ownershipRoot, "unsafe", []string{"../escape"}, time.Now().Add(time.Hour)); err == nil {
		t.Fatal("workspace snapshot accepted an unsafe exclusion")
	}

	invalid := state.WorkspaceSnapshot{SessionID: "session", ArchivePath: filepath.Join(ownershipRoot, "wrong.tar"), Status: "READY"}
	if err := ValidateWorkspaceSnapshot(ownershipRoot, invalid, time.Now()); err == nil {
		t.Fatal("non-archived workspace snapshot was accepted")
	}
	invalid.Status = "ARCHIVED"
	invalid.ExpiresAt = "not-a-time"
	if err := ValidateWorkspaceSnapshot(ownershipRoot, invalid, time.Now()); err == nil {
		t.Fatal("workspace snapshot with malformed expiry was accepted")
	}
	invalid.ExpiresAt = time.Now().Add(time.Hour).Format(time.RFC3339Nano)
	if err := ValidateWorkspaceSnapshot(ownershipRoot, invalid, time.Now()); err == nil {
		t.Fatal("workspace snapshot with mismatched path was accepted")
	}
	invalid.ArchivePath = filepath.Join(ownershipRoot, filepath.FromSlash(workspaceSnapshotRelativePath(invalid.SessionID)))
	if err := ValidateWorkspaceSnapshot(ownershipRoot, invalid, time.Now()); err == nil {
		t.Fatal("missing workspace snapshot artifact was accepted")
	}
	if err := DeleteWorkspaceSnapshot(ownershipRoot, state.WorkspaceSnapshot{SessionID: "session", ArchivePath: filepath.Join(ownershipRoot, "wrong.tar")}); err == nil {
		t.Fatal("snapshot deletion accepted a mismatched artifact path")
	}
	if err := DeleteWorkspaceSnapshot(filepath.Join(ownershipRoot, "missing-root"), invalid); err == nil {
		t.Fatal("snapshot deletion accepted a missing ownership root")
	}

	pruneRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(pruneRoot, "repository"), []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	owner, err := workspace.OpenPhysicalRoot(pruneRoot)
	if err != nil {
		t.Fatal(err)
	}
	if err := pruneWorkspaceRoot(owner, ".", []string{"repository/file"}); err == nil {
		t.Fatal("pruning accepted a non-directory exclusion ancestor")
	}
	if err := owner.Close(); err != nil {
		t.Fatal(err)
	}
	if err := pruneWorkspaceRoot(owner, ".", nil); err == nil {
		t.Fatal("pruning a closed root succeeded")
	}

	recursiveRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(recursiveRoot, "repository", "nested"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(recursiveRoot, "repository", "nested", "keep"), []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(recursiveRoot, "repository", "remove"), []byte("remove"), 0o600); err != nil {
		t.Fatal(err)
	}
	recursiveOwner, err := workspace.OpenPhysicalRoot(recursiveRoot)
	if err != nil {
		t.Fatal(err)
	}
	if err := pruneWorkspaceRoot(recursiveOwner, ".", []string{"repository/nested/keep"}); err != nil {
		t.Fatal(err)
	}
	if err := recursiveOwner.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(recursiveRoot, "repository", "nested", "keep")); err != nil {
		t.Fatalf("excluded descendant was pruned: %v", err)
	}
	if _, err := os.Stat(filepath.Join(recursiveRoot, "repository", "remove")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("non-excluded sibling remains, err=%v", err)
	}
}

func TestRestoreRevalidatesOwnershipAtHandoffs(t *testing.T) {
	for _, test := range []struct {
		name   string
		failAt int
	}{
		{name: "before links", failAt: 5},
		{name: "before restore handoff", failAt: 6},
		{name: "after restore handoff", failAt: 7},
		{name: "after resume prepare", failAt: 8},
		{name: "after status", failAt: 9},
		{name: "before finish", failAt: 10},
	} {
		t.Run(test.name, func(t *testing.T) {
			repository, repo, manager, worktreeRoot := archiveFixture(t)
			snapshot, err := manager.Snapshot(context.Background(), repo, repository, "source", time.Now().Add(time.Hour))
			if err != nil {
				t.Fatal(err)
			}
			validator := &countingOwnershipValidator{failAt: test.failAt}
			manager.Preparer.Ownership = validator
			target := filepath.Join(worktreeRoot, test.name, "root")
			err = manager.Restore(context.Background(), repo, target, test.name, snapshot)
			if err == nil {
				t.Fatal("restore succeeded after ownership proof was invalidated")
			}
			if !errors.Is(err, state.ErrOwnership) {
				t.Fatalf("restore ownership error=%v", err)
			}
			if validator.calls < test.failAt {
				t.Fatalf("ownership validator stopped at call %d, want at least %d", validator.calls, test.failAt)
			}
		})
	}
}

func TestRemoveMissingWorktreeReportsEveryHandoffFailure(t *testing.T) {
	for _, test := range []struct {
		name       string
		fault      string
		occurrence int
		want       string
		setup      func(*testing.T, *Manager, discovery.Repository, string)
	}{
		{name: "common directory marker mismatch", want: "ownership marker", setup: func(t *testing.T, manager *Manager, repo discovery.Repository, target string) {
			marker := filepath.Join(filepath.Dir(target), ".wx-owner-"+domain.StableID("worktree", filepath.Clean(target)))
			if err := os.Remove(marker); err != nil {
				t.Fatal(err)
			}
			otherCommon := filepath.Join(filepath.Dir(string(repo.CommonDir)), "other.git")
			if err := os.Mkdir(otherCommon, 0o700); err != nil {
				t.Fatal(err)
			}
			ownershipRoot := filepath.Dir(filepath.Dir(filepath.Dir(target)))
			if err := workspace.EnsureOwnershipMarker(ownershipRoot, target, "slot", otherCommon); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "foreign lock reason", want: "lock reason", setup: func(t *testing.T, manager *Manager, repo discovery.Repository, target string) {
			gitCommand(t, string(repo.MainPath), "worktree", "unlock", target)
			gitCommand(t, string(repo.MainPath), "worktree", "lock", "--reason", "foreign", target)
		}},
		{name: "state proof before unlock", want: "state changed", setup: func(t *testing.T, manager *Manager, repo discovery.Repository, target string) {
			manager.Ownership = rejectingOwnershipValidator{err: errors.New("state changed")}
		}},
		{name: "unlock failure", fault: " worktree unlock ", occurrence: 1, want: "git worktree failed"},
		{name: "post-unlock listing failure", fault: " worktree list --porcelain -z ", occurrence: 2, want: "git worktree failed"},
		{name: "revalidation state proof", want: "state proof changed", setup: func(t *testing.T, manager *Manager, repo discovery.Repository, target string) {
			manager.Ownership = &countingOwnershipValidator{failAt: 2}
		}},
		{name: "remove failure", fault: " worktree remove --force ", occurrence: 1, want: "git worktree failed"},
	} {
		t.Run(test.name, func(t *testing.T) {
			repository, repo, manager, worktreeRoot := archiveFixture(t)
			head := gitCommand(t, repository, "rev-parse", "HEAD")
			target := filepath.Join(worktreeRoot, "slots", "slot", "root")
			mustMkdir(t, filepath.Dir(target))
			gitCommand(t, repository, "worktree", "add", "--detach", target, head)
			markOwnedWorktree(t, worktreeRoot, target, "slot", repo.CommonDir)
			if test.setup != nil {
				test.setup(t, manager, repo, target)
			}
			if test.fault != "" {
				installGitFault(t, test.fault, test.occurrence)
			}
			if err := os.RemoveAll(target); err != nil {
				t.Fatal(err)
			}
			err := manager.RemoveWorktree(context.Background(), repo, worktreeRoot, target, head)
			if err == nil {
				t.Fatal("missing worktree failure was ignored")
			}
			if test.want != "" && !strings.Contains(err.Error(), test.want) {
				t.Fatalf("missing worktree error=%v, want substring %q", err, test.want)
			}
		})
	}
}

type countingOwnershipValidator struct{ calls, failAt int }

func (v *countingOwnershipValidator) ValidateWorktreeOwnership(context.Context, state.WorktreeOwnershipRequest) (state.WorktreeOwnership, error) {
	v.calls++
	if v.calls == v.failAt {
		return state.WorktreeOwnership{}, errors.New("state proof changed")
	}
	return state.WorktreeOwnership{}, nil
}
