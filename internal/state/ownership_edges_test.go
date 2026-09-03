package state

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/HappyOnigiri/WX/internal/discovery"
	"github.com/HappyOnigiri/WX/internal/domain"
)

func TestValidateWorktreeOwnershipRejectsAWorktreeRecordedOutsideItsSlot(t *testing.T) {
	f := newOwnershipFixture(t)
	ctx := context.Background()
	outside := filepath.Join(f.root, "outside-worktree")
	if err := os.Mkdir(outside, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.db.ExecContext(ctx, `UPDATE slot_repositories SET worktree_path=? WHERE slot_id=? AND repository_id=?`, outside, f.slotID, f.repository); err != nil {
		t.Fatal(err)
	}
	request := f.worktreeRequest()
	request.WorktreePath = outside
	if _, err := f.store.ValidateWorktreeOwnership(ctx, request); err == nil || !strings.Contains(err.Error(), "outside its slot path") {
		t.Fatalf("worktree recorded outside its slot error=%v", err)
	}
}

func TestValidateSlotOwnershipRejectsAMissingRecordedWorkspaceRoot(t *testing.T) {
	f := newOwnershipFixture(t)
	ctx := context.Background()
	if _, err := f.store.db.ExecContext(ctx, `UPDATE workspaces SET root_path=? WHERE id=?`, filepath.Join(f.root, "missing-workspace-root"), f.workspaceID); err != nil {
		t.Fatal(err)
	}
	request := SlotOwnershipRequest{SlotID: f.slotID, WorkspaceID: f.workspaceID, Path: f.slotPath, AllowedSlotStates: []string{"READY"}}
	if err := f.store.ValidateSlotOwnership(ctx, request); err == nil || !strings.Contains(err.Error(), "recorded workspace root") {
		t.Fatalf("missing recorded workspace root error=%v", err)
	}
}

func TestOwnershipPathHelpersRejectUnsafeInputs(t *testing.T) {
	tempRoot, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(tempRoot, "root")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	inside := filepath.Join(root, "inside")
	if err := os.Mkdir(inside, 0o700); err != nil {
		t.Fatal(err)
	}

	for _, raw := range []string{"", "/absolute", "../escape"} {
		if err := validateWorkspaceRepositoryAssociation(root, inside, raw); err == nil {
			t.Fatalf("unsafe relative path %q accepted", raw)
		}
	}
	if err := validateWorkspaceRepositoryAssociation(root, inside, "other"); err == nil {
		t.Fatal("mismatched repository association accepted")
	}
	if err := validateWorkspaceRepositoryAssociation(root, inside, "inside"); err != nil {
		t.Fatalf("valid repository association rejected: %v", err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(inside, link); err != nil {
		t.Fatal(err)
	}
	if err := validateWorkspaceRepositoryAssociation(root, link, "link"); err == nil {
		t.Fatal("symlink repository association accepted")
	}

	for _, raw := range []string{"", "relative"} {
		if _, err := canonicalOwnershipPath(raw); err == nil {
			t.Fatalf("invalid ownership path %q accepted", raw)
		}
	}
	missing, err := canonicalOwnershipPath(filepath.Join(inside, "missing", "leaf"))
	if err != nil || missing != filepath.Join(inside, "missing", "leaf") {
		t.Fatalf("missing ownership suffix=%q err=%v", missing, err)
	}
	if _, err := canonicalOwnershipPath(link); err == nil {
		t.Fatal("symlink ownership path accepted")
	}
	plain := filepath.Join(root, "plain")
	if err := os.WriteFile(plain, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := canonicalOwnershipPath(filepath.Join(plain, "child")); err == nil {
		t.Fatal("path below a regular file accepted")
	}
	if _, err := canonicalExistingDirectory(""); err == nil {
		t.Fatal("empty existing directory accepted")
	}
	if _, err := canonicalExistingDirectory("relative"); err == nil {
		t.Fatal("relative existing directory accepted")
	}
	if _, err := canonicalExistingDirectory(filepath.Join(root, "missing")); err == nil {
		t.Fatal("missing existing directory accepted")
	}
	if _, err := canonicalExistingDirectory(plain); err == nil {
		t.Fatal("regular file accepted as existing directory")
	}
	if _, err := canonicalExistingDirectory(link); err == nil {
		t.Fatal("symlink accepted as existing directory")
	}
	if got, err := canonicalExistingDirectory(inside); err != nil || got != inside {
		t.Fatalf("existing directory=%q err=%v", got, err)
	}

	if err := ownershipFailure("message"); !errors.Is(err, ErrOwnership) {
		t.Fatal("ownership failure did not preserve sentinel")
	}
	if err := ownershipDatabaseFailure(context.Canceled); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation was wrapped: %v", err)
	}
	if err := ownershipDatabaseFailure(context.DeadlineExceeded); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("deadline was wrapped: %v", err)
	}
	if err := ownershipDatabaseFailure(sql.ErrNoRows); !errors.Is(err, ErrOwnership) || !strings.Contains(err.Error(), "missing") {
		t.Fatalf("no-row database failure=%v", err)
	}
	if err := ownershipDatabaseFailure(errors.New("database fault")); !errors.Is(err, ErrOwnership) {
		t.Fatalf("database failure=%v", err)
	}
}

type ownershipFixture struct {
	store       *Store
	root        string
	main        string
	common      string
	slotPath    string
	worktree    string
	alternate   string
	workspaceID string
	repository  string
	slotID      string
}

func newOwnershipFixture(t *testing.T) ownershipFixture {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	main := filepath.Join(root, "repository")
	common := filepath.Join(root, "repository.git")
	if err := os.Mkdir(main, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(common, 0o700); err != nil {
		t.Fatal(err)
	}
	store, err := Open(filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	workspaceRoot := root
	alternate := filepath.Join(root, "alternate")
	if err := os.Mkdir(alternate, 0o700); err != nil {
		t.Fatal(err)
	}
	workspace := discoveryWorkspace(workspaceRoot, main, common)
	if err := store.UpsertWorkspace(context.Background(), workspace); err != nil {
		t.Fatal(err)
	}
	slotPath := filepath.Join(root, "slots", "slot")
	worktree := filepath.Join(slotPath, "repository")
	if err := os.MkdirAll(slotPath, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateStandby(context.Background(), Slot{ID: "slot", WorkspaceID: "workspace", Generation: 1, Path: slotPath, State: "READY"}, []SlotRepository{{RepositoryID: "repository", WorktreePath: worktree, State: "READY", RequestedRef: "main", BaseOID: "oid", Fingerprint: "fingerprint"}}); err != nil {
		t.Fatal(err)
	}
	return ownershipFixture{store: store, root: root, main: main, common: common, slotPath: slotPath, worktree: worktree, alternate: alternate, workspaceID: "workspace", repository: "repository", slotID: "slot"}
}

func discoveryWorkspace(workspaceRoot, main, common string) discovery.Workspace {
	return discovery.Workspace{ID: "workspace", Root: domain.CanonicalPath(workspaceRoot), Kind: "repository", Repositories: []discovery.Repository{{ID: "repository", MainPath: domain.CanonicalPath(main), CommonDir: domain.CanonicalPath(common), RelativePath: "repository", DefaultBranch: "main"}}}
}

func (f ownershipFixture) worktreeRequest() WorktreeOwnershipRequest {
	return WorktreeOwnershipRequest{SlotID: f.slotID, RepositoryID: f.repository, SlotPath: f.slotPath, WorktreePath: f.worktree, CommonDir: f.common, AllowedSlotStates: []string{"READY"}, AllowedRepositoryStates: []string{"READY"}}
}

func TestValidateWorktreeOwnershipRejectsEveryIdentityBoundary(t *testing.T) {
	f := newOwnershipFixture(t)
	ctx := context.Background()
	proof, err := f.store.ValidateWorktreeOwnership(ctx, f.worktreeRequest())
	if err != nil {
		t.Fatalf("valid ownership proof failed: %v", err)
	}
	if proof.SlotID != f.slotID || proof.RepositoryID != f.repository || proof.WorkspaceRoot == "" || proof.RelativePath != "repository" {
		t.Fatalf("ownership proof=%+v", proof)
	}

	if _, err := (*Store)(nil).ValidateWorktreeOwnership(ctx, f.worktreeRequest()); !errors.Is(err, ErrOwnership) {
		t.Fatalf("nil store error=%v", err)
	}
	for _, request := range []WorktreeOwnershipRequest{
		{},
		{SlotID: f.slotID, RepositoryID: f.repository, WorktreePath: f.worktree, CommonDir: f.common},
	} {
		if _, err := f.store.ValidateWorktreeOwnership(ctx, request); !errors.Is(err, ErrOwnership) {
			t.Fatalf("incomplete request error=%v", err)
		}
	}
	if _, err := f.store.ValidateWorktreeOwnership(ctx, WorktreeOwnershipRequest{SlotID: f.slotID, RepositoryID: f.repository, WorktreePath: f.worktree, CommonDir: f.common, AllowedSlotStates: []string{"READY"}}); !errors.Is(err, ErrOwnership) {
		t.Fatalf("missing repository state set error=%v", err)
	}
	if _, err := f.store.ValidateWorktreeOwnership(ctx, WorktreeOwnershipRequest{SlotID: f.slotID, RepositoryID: f.repository, WorktreePath: f.worktree, CommonDir: f.common, AllowedRepositoryStates: []string{"READY"}}); !errors.Is(err, ErrOwnership) {
		t.Fatalf("missing slot state set error=%v", err)
	}
	if _, err := f.store.ValidateWorktreeOwnership(ctx, WorktreeOwnershipRequest{SlotID: "missing", RepositoryID: f.repository, WorktreePath: f.worktree, CommonDir: f.common, AllowedSlotStates: []string{"READY"}, AllowedRepositoryStates: []string{"READY"}}); !errors.Is(err, ErrOwnership) {
		t.Fatalf("missing row error=%v", err)
	}

	cases := []struct {
		name    string
		edit    string
		args    func(ownershipFixture) []any
		request func(WorktreeOwnershipRequest) WorktreeOwnershipRequest
	}{
		{name: "workspace mismatch", request: func(r WorktreeOwnershipRequest) WorktreeOwnershipRequest { r.WorkspaceID = "other"; return r }},
		{name: "slot state", edit: `UPDATE slots SET state='FAILED' WHERE id='slot'`},
		{name: "repository state", edit: `UPDATE slot_repositories SET state='FAILED' WHERE slot_id='slot' AND repository_id='repository'`},
		{name: "recorded slot path", edit: `UPDATE slots SET path='relative' WHERE id='slot'`},
		{name: "recorded worktree path", edit: `UPDATE slot_repositories SET worktree_path='relative' WHERE slot_id='slot'`},
		{name: "requested worktree path", request: func(r WorktreeOwnershipRequest) WorktreeOwnershipRequest { r.WorktreePath = "relative"; return r }},
		{name: "worktree mismatch", request: func(r WorktreeOwnershipRequest) WorktreeOwnershipRequest { return worktreePath(r, "other") }},
		{name: "requested slot path", request: func(r WorktreeOwnershipRequest) WorktreeOwnershipRequest { r.SlotPath = "relative"; return r }},
		{name: "slot mismatch", request: func(r WorktreeOwnershipRequest) WorktreeOwnershipRequest {
			r.SlotPath = filepath.Join(filepath.Dir(r.SlotPath), "other-slot")
			return r
		}},
		{name: "outside slot", edit: `UPDATE slot_repositories SET worktree_path=? WHERE slot_id='slot'`, args: func(f ownershipFixture) []any { return []any{filepath.Join(f.root, "outside")} }, request: func(r WorktreeOwnershipRequest) WorktreeOwnershipRequest {
			r.WorktreePath = filepath.Join(filepath.Dir(r.SlotPath), "outside")
			return r
		}},
		{name: "relative mismatch", edit: `UPDATE workspace_repositories SET relative_path='other' WHERE workspace_id='workspace' AND repository_id='repository'`},
		{name: "workspace root", edit: `UPDATE workspaces SET root_path='relative' WHERE id='workspace'`},
		{name: "repository main", edit: `UPDATE repositories SET main_worktree_path='relative' WHERE id='repository'`},
		{name: "recorded common", edit: `UPDATE repositories SET common_git_dir='relative' WHERE id='repository'`},
		{name: "requested common", request: func(r WorktreeOwnershipRequest) WorktreeOwnershipRequest { r.CommonDir = "relative"; return r }},
		{name: "common mismatch", request: func(r WorktreeOwnershipRequest) WorktreeOwnershipRequest {
			r.CommonDir = filepath.Dir(r.CommonDir)
			return r
		}},
		{name: "association mismatch", edit: `UPDATE workspaces SET root_path=? WHERE id='workspace'`, args: func(f ownershipFixture) []any { return []any{f.alternate} }},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			fixture := newOwnershipFixture(t)
			request := fixture.worktreeRequest()
			if test.edit != "" {
				var args []any
				if test.args != nil {
					args = test.args(fixture)
				}
				if _, err := fixture.store.db.ExecContext(ctx, test.edit, args...); err != nil {
					t.Fatal(err)
				}
			}
			if test.request != nil {
				request = test.request(request)
			}
			if _, err := fixture.store.ValidateWorktreeOwnership(ctx, request); !errors.Is(err, ErrOwnership) {
				t.Fatalf("invalid ownership proof succeeded: %v", err)
			}
		})
	}
}

func worktreePath(r WorktreeOwnershipRequest, name string) WorktreeOwnershipRequest {
	r.WorktreePath = filepath.Join(r.SlotPath, name)
	return r
}

func TestValidateSlotOwnershipBoundaries(t *testing.T) {
	f := newOwnershipFixture(t)
	ctx := context.Background()
	valid := SlotOwnershipRequest{SlotID: f.slotID, WorkspaceID: f.workspaceID, Path: f.slotPath, AllowedSlotStates: []string{"READY"}}
	if err := f.store.ValidateSlotOwnership(ctx, valid); err != nil {
		t.Fatalf("valid slot proof err=%v", err)
	}
	if err := (*Store)(nil).ValidateSlotOwnership(ctx, valid); !errors.Is(err, ErrOwnership) {
		t.Fatalf("nil slot store error=%v", err)
	}
	for _, request := range []SlotOwnershipRequest{{}, {SlotID: f.slotID, Path: f.slotPath}} {
		if err := f.store.ValidateSlotOwnership(ctx, request); !errors.Is(err, ErrOwnership) {
			t.Fatalf("incomplete slot request error=%v", err)
		}
	}
	for _, test := range []struct {
		name   string
		edit   string
		args   []any
		mutate func(SlotOwnershipRequest) SlotOwnershipRequest
	}{
		{name: "missing row", mutate: func(r SlotOwnershipRequest) SlotOwnershipRequest { r.SlotID = "missing"; return r }},
		{name: "workspace mismatch", mutate: func(r SlotOwnershipRequest) SlotOwnershipRequest { r.WorkspaceID = "other"; return r }},
		{name: "workspace association", edit: `UPDATE slots SET workspace_id=NULL WHERE id='slot'`},
		{name: "state mismatch", edit: `UPDATE slots SET state='FAILED' WHERE id='slot'`},
		{name: "path mismatch", mutate: func(r SlotOwnershipRequest) SlotOwnershipRequest {
			r.Path = filepath.Join(filepath.Dir(r.Path), "other")
			return r
		}},
		{name: "requested path", mutate: func(r SlotOwnershipRequest) SlotOwnershipRequest {
			r.Path = "relative"
			return r
		}},
		{name: "recorded path", edit: `UPDATE slots SET path='relative' WHERE id='slot'`},
		{name: "workspace root", edit: `UPDATE workspaces SET root_path='relative' WHERE id='workspace'`},
		{name: "missing workspace root", edit: `UPDATE workspaces SET root_path='/missing/workspace' WHERE id='workspace'`},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newOwnershipFixture(t)
			request := valid
			if test.edit != "" {
				if _, err := fixture.store.db.ExecContext(ctx, test.edit, test.args...); err != nil {
					t.Fatal(err)
				}
			}
			if test.mutate != nil {
				request = test.mutate(request)
			}
			if err := fixture.store.ValidateSlotOwnership(ctx, request); !errors.Is(err, ErrOwnership) {
				t.Fatalf("invalid slot proof succeeded: %v", err)
			}
		})
	}
}

func TestCanonicalWorkspaceKeepsNewRepositoryIdentity(t *testing.T) {
	store := openTestStore(t)
	common := t.TempDir()
	w := discovery.Workspace{
		ID:   "derived-from-main",
		Root: domain.CanonicalPath(t.TempDir()),
		Kind: "repository",
		Repositories: []discovery.Repository{{
			ID: "repository", MainPath: domain.CanonicalPath(t.TempDir()), CommonDir: domain.CanonicalPath(common), DefaultBranch: "main",
		}},
	}
	canonical, err := store.CanonicalWorkspace(context.Background(), w)
	if err != nil {
		t.Fatal(err)
	}
	if canonical.ID != w.ID {
		t.Fatalf("unregistered repository workspace identity=%q, want %q", canonical.ID, w.ID)
	}
}
