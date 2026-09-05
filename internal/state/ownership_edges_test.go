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

// TestValidateOwnershipRelativeRejectsUnsafeLocations は、所有権の両入口が request と
// 記録済み row の全 root-relative location に適用する共通ガードを検証する。
func TestValidateOwnershipRelativeRejectsUnsafeLocations(t *testing.T) {
	for _, value := range []string{"", ".", "..", "/absolute", "../escape", "trailing/", "double//slash", "./here"} {
		if err := validateOwnershipRelative("slot path", value); err == nil {
			t.Errorf("unsafe relative location %q accepted", value)
		}
	}
	for _, value := range []string{"abc123/def456", "_unbound/def456", "WX"} {
		if err := validateOwnershipRelative("slot path", value); err != nil {
			t.Errorf("safe relative location %q rejected: %v", value, err)
		}
	}
	if err := validateOwnershipRelative("slot path", ""); err == nil || !strings.Contains(err.Error(), "slot path is empty") {
		t.Fatalf("empty location error=%v", err)
	}
}

type ownershipFixture struct {
	store       *Store
	root        string
	rootID      string
	rootPath    string
	main        string
	common      string
	slotRel     string
	dirName     string
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
	for _, dir := range []string{main, common, filepath.Join(root, "alternate")} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	store, err := Open(filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	rootPath := filepath.Join(root, "wx")
	if err := os.Mkdir(rootPath, 0o700); err != nil {
		t.Fatal(err)
	}
	seedRoot(t, store, testRootID, rootPath, "root-identity", true)
	registered, _, err := store.UpsertWorkspaceGeneration(context.Background(), discoveryWorkspace(root, main, common))
	if err != nil {
		t.Fatal(err)
	}
	workspaceID := string(registered.ID)
	slotRel := filepath.Join(workspaceID, "slot01")
	slotPath := filepath.Join(rootPath, slotRel)
	worktree := filepath.Join(slotPath, "repository")
	if err := os.MkdirAll(worktree, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateStandby(context.Background(), Slot{ID: "slot01", WorkspaceID: workspaceID, Generation: 1, RootID: testRootID, RelPath: slotRel, State: "READY"}, []SlotRepository{{RepositoryID: "repository", DirName: "repository", State: "READY", RequestedRef: "main", BaseOID: "oid", Fingerprint: "fingerprint"}}); err != nil {
		t.Fatal(err)
	}
	return ownershipFixture{
		store: store, root: root, rootID: testRootID, rootPath: rootPath,
		main: main, common: common, slotRel: slotRel, dirName: "repository",
		slotPath: slotPath, worktree: worktree, alternate: filepath.Join(root, "alternate"),
		workspaceID: workspaceID, repository: "repository", slotID: "slot01",
	}
}

func discoveryWorkspace(workspaceRoot, main, common string) discovery.Workspace {
	return discovery.Workspace{ID: "proposal", Root: domain.CanonicalPath(workspaceRoot), Kind: "repository", Repositories: []discovery.Repository{{ID: "repository", MainPath: domain.CanonicalPath(main), CommonDir: domain.CanonicalPath(common), RelativePath: "repository", DefaultBranch: "main"}}}
}

func (f ownershipFixture) worktreeRequest() WorktreeOwnershipRequest {
	return WorktreeOwnershipRequest{
		SlotID: f.slotID, RepositoryID: f.repository, RootID: f.rootID,
		SlotRelPath: f.slotRel, DirName: f.dirName, CommonDir: f.common,
		AllowedSlotStates: []string{"READY"}, AllowedRepositoryStates: []string{"READY"},
	}
}

func (f ownershipFixture) slotRequest() SlotOwnershipRequest {
	return SlotOwnershipRequest{SlotID: f.slotID, WorkspaceID: f.workspaceID, RootID: f.rootID, RelPath: f.slotRel, AllowedSlotStates: []string{"READY"}}
}

func TestValidateWorktreeOwnershipProvesRecordedLocation(t *testing.T) {
	f := newOwnershipFixture(t)
	ctx := context.Background()
	proof, err := f.store.ValidateWorktreeOwnership(ctx, f.worktreeRequest())
	if err != nil {
		t.Fatalf("valid ownership proof failed: %v", err)
	}
	if proof.SlotID != f.slotID || proof.RepositoryID != f.repository {
		t.Fatalf("ownership proof identity=%+v", proof)
	}
	if proof.RootID != f.rootID || proof.RootPath != f.rootPath || proof.SlotRelPath != f.slotRel || proof.DirName != f.dirName {
		t.Fatalf("ownership proof location=%+v", proof)
	}
	if proof.WorkspaceRoot != f.root || proof.MainWorktreePath != f.main || proof.CommonDir != f.common || proof.RelativePath != "repository" {
		t.Fatalf("ownership proof source side=%+v", proof)
	}
	if proof.SlotState != "READY" || proof.RepositoryState != "READY" || proof.Generation != 1 {
		t.Fatalf("ownership proof state=%+v", proof)
	}
}

func TestValidateWorktreeOwnershipRejectsIncompleteRequests(t *testing.T) {
	f := newOwnershipFixture(t)
	ctx := context.Background()
	if _, err := (*Store)(nil).ValidateWorktreeOwnership(ctx, f.worktreeRequest()); !errors.Is(err, ErrOwnership) {
		t.Fatalf("nil store error=%v", err)
	}
	incomplete := map[string]func(WorktreeOwnershipRequest) WorktreeOwnershipRequest{
		"empty":              func(WorktreeOwnershipRequest) WorktreeOwnershipRequest { return WorktreeOwnershipRequest{} },
		"no slot":            func(r WorktreeOwnershipRequest) WorktreeOwnershipRequest { r.SlotID = ""; return r },
		"no repository":      func(r WorktreeOwnershipRequest) WorktreeOwnershipRequest { r.RepositoryID = ""; return r },
		"no root generation": func(r WorktreeOwnershipRequest) WorktreeOwnershipRequest { r.RootID = ""; return r },
		"no common dir":      func(r WorktreeOwnershipRequest) WorktreeOwnershipRequest { r.CommonDir = ""; return r },
		"no slot rel path":   func(r WorktreeOwnershipRequest) WorktreeOwnershipRequest { r.SlotRelPath = ""; return r },
		"unsafe slot rel path": func(r WorktreeOwnershipRequest) WorktreeOwnershipRequest {
			r.SlotRelPath = "../escape"
			return r
		},
		"slot rel path is root": func(r WorktreeOwnershipRequest) WorktreeOwnershipRequest { r.SlotRelPath = "."; return r },
		"no dir name":           func(r WorktreeOwnershipRequest) WorktreeOwnershipRequest { r.DirName = ""; return r },
		"unsafe dir name": func(r WorktreeOwnershipRequest) WorktreeOwnershipRequest {
			r.DirName = "nested/repository"
			return r
		},
		"no slot states": func(r WorktreeOwnershipRequest) WorktreeOwnershipRequest {
			r.AllowedSlotStates = nil
			return r
		},
		"no repository states": func(r WorktreeOwnershipRequest) WorktreeOwnershipRequest {
			r.AllowedRepositoryStates = nil
			return r
		},
		"missing row": func(r WorktreeOwnershipRequest) WorktreeOwnershipRequest { r.SlotID = "absent"; return r },
	}
	for name, mutate := range incomplete {
		t.Run(name, func(t *testing.T) {
			if _, err := f.store.ValidateWorktreeOwnership(ctx, mutate(f.worktreeRequest())); !errors.Is(err, ErrOwnership) {
				t.Fatalf("invalid request accepted: %v", err)
			}
		})
	}
}

func TestValidateWorktreeOwnershipRejectsEveryIdentityBoundary(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name    string
		edit    string
		args    func(ownershipFixture) []any
		request func(ownershipFixture, WorktreeOwnershipRequest) WorktreeOwnershipRequest
	}{
		{name: "workspace mismatch", request: func(_ ownershipFixture, r WorktreeOwnershipRequest) WorktreeOwnershipRequest {
			r.WorkspaceID = "other"
			return r
		}},
		{name: "slot state", edit: `UPDATE slots SET state='FAILED' WHERE id='slot01'`},
		{name: "repository state", edit: `UPDATE slot_repositories SET state='FAILED' WHERE slot_id='slot01' AND repository_id='repository'`},
		{name: "root generation mismatch", edit: `INSERT INTO roots(id,path,identity,active,created_at) VALUES('other0','/other','other-identity',0,'2024-01-01T00:00:00.000000000Z')`, request: func(_ ownershipFixture, r WorktreeOwnershipRequest) WorktreeOwnershipRequest {
			r.RootID = "other0"
			return r
		}},
		{name: "requested slot location", request: func(f ownershipFixture, r WorktreeOwnershipRequest) WorktreeOwnershipRequest {
			r.SlotRelPath = filepath.Join(f.workspaceID, "slot02")
			return r
		}},
		{name: "requested dir name", request: func(_ ownershipFixture, r WorktreeOwnershipRequest) WorktreeOwnershipRequest {
			r.DirName = "other"
			return r
		}},
		{name: "recorded slot location is unsafe", edit: `UPDATE slots SET rel_path='.' WHERE id='slot01'`},
		{name: "recorded dir name is unsafe", edit: `UPDATE slot_repositories SET dir_name='..' WHERE slot_id='slot01'`},
		{name: "identity is not recorded", request: func(_ ownershipFixture, r WorktreeOwnershipRequest) WorktreeOwnershipRequest {
			r.DirIdentity = "1:2"
			return r
		}},
		{name: "identity mismatch", edit: `UPDATE slot_repositories SET dir_identity='3:4' WHERE slot_id='slot01'`, request: func(_ ownershipFixture, r WorktreeOwnershipRequest) WorktreeOwnershipRequest {
			r.DirIdentity = "1:2"
			return r
		}},
		{name: "relative mismatch", edit: `UPDATE workspace_repositories SET relative_path='other'`},
		{name: "workspace root", edit: `UPDATE workspaces SET root_path='relative'`},
		{name: "repository main", edit: `UPDATE repositories SET main_worktree_path='relative' WHERE id='repository'`},
		{name: "recorded common", edit: `UPDATE repositories SET common_git_dir='relative' WHERE id='repository'`},
		{name: "requested common", request: func(_ ownershipFixture, r WorktreeOwnershipRequest) WorktreeOwnershipRequest {
			r.CommonDir = "relative"
			return r
		}},
		{name: "common mismatch", request: func(_ ownershipFixture, r WorktreeOwnershipRequest) WorktreeOwnershipRequest {
			r.CommonDir = filepath.Dir(r.CommonDir)
			return r
		}},
		{name: "association mismatch", edit: `UPDATE workspaces SET root_path=?`, args: func(f ownershipFixture) []any { return []any{f.alternate} }},
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
				request = test.request(fixture, request)
			}
			if _, err := fixture.store.ValidateWorktreeOwnership(ctx, request); !errors.Is(err, ErrOwnership) {
				t.Fatalf("invalid ownership proof succeeded: %v", err)
			}
		})
	}
}

// TestValidateWorktreeOwnershipAcceptsAMatchingIdentity は fail-closed な identity 規則の成功側を検証する。
// wx が記録した identity を caller が提示すれば受理されるため、上の拒否は identity の提示自体ではなく不一致による。
func TestValidateWorktreeOwnershipAcceptsAMatchingIdentity(t *testing.T) {
	f := newOwnershipFixture(t)
	ctx := context.Background()
	if err := f.store.RecordSlotRepositoryIdentity(ctx, f.slotID, f.repository, "16777220:9911"); err != nil {
		t.Fatal(err)
	}
	request := f.worktreeRequest()
	request.DirIdentity = "16777220:9911"
	proof, err := f.store.ValidateWorktreeOwnership(ctx, request)
	if err != nil {
		t.Fatalf("matching identity rejected: %v", err)
	}
	if proof.DirName != f.dirName {
		t.Fatalf("ownership proof=%+v", proof)
	}
}

func TestValidateSlotOwnershipBoundaries(t *testing.T) {
	f := newOwnershipFixture(t)
	ctx := context.Background()
	if err := f.store.ValidateSlotOwnership(ctx, f.slotRequest()); err != nil {
		t.Fatalf("valid slot proof err=%v", err)
	}
	if err := (*Store)(nil).ValidateSlotOwnership(ctx, f.slotRequest()); !errors.Is(err, ErrOwnership) {
		t.Fatalf("nil slot store error=%v", err)
	}
	for _, test := range []struct {
		name   string
		edit   string
		args   []any
		mutate func(ownershipFixture, SlotOwnershipRequest) SlotOwnershipRequest
	}{
		{name: "empty request", mutate: func(ownershipFixture, SlotOwnershipRequest) SlotOwnershipRequest { return SlotOwnershipRequest{} }},
		{name: "no root generation", mutate: func(_ ownershipFixture, r SlotOwnershipRequest) SlotOwnershipRequest { r.RootID = ""; return r }},
		{name: "no states", mutate: func(_ ownershipFixture, r SlotOwnershipRequest) SlotOwnershipRequest {
			r.AllowedSlotStates = nil
			return r
		}},
		{name: "unsafe rel path", mutate: func(_ ownershipFixture, r SlotOwnershipRequest) SlotOwnershipRequest {
			r.RelPath = "../escape"
			return r
		}},
		{name: "missing row", mutate: func(_ ownershipFixture, r SlotOwnershipRequest) SlotOwnershipRequest { r.SlotID = "absent"; return r }},
		{name: "workspace mismatch", mutate: func(_ ownershipFixture, r SlotOwnershipRequest) SlotOwnershipRequest {
			r.WorkspaceID = "other"
			return r
		}},
		{name: "workspace association", edit: `UPDATE slots SET workspace_id=NULL WHERE id='slot01'`},
		{name: "state mismatch", edit: `UPDATE slots SET state='FAILED' WHERE id='slot01'`},
		{name: "root generation mismatch", edit: `INSERT INTO roots(id,path,identity,active,created_at) VALUES('other0','/other','other-identity',0,'2024-01-01T00:00:00.000000000Z')`, mutate: func(_ ownershipFixture, r SlotOwnershipRequest) SlotOwnershipRequest {
			r.RootID = "other0"
			return r
		}},
		{name: "rel path mismatch", mutate: func(f ownershipFixture, r SlotOwnershipRequest) SlotOwnershipRequest {
			r.RelPath = filepath.Join(f.workspaceID, "slot02")
			return r
		}},
		{name: "recorded rel path is unsafe", edit: `UPDATE slots SET rel_path='.' WHERE id='slot01'`},
		{name: "identity is not recorded", mutate: func(_ ownershipFixture, r SlotOwnershipRequest) SlotOwnershipRequest {
			r.DirIdentity = "1:2"
			return r
		}},
		{name: "identity mismatch", edit: `UPDATE slots SET dir_identity='3:4' WHERE id='slot01'`, mutate: func(_ ownershipFixture, r SlotOwnershipRequest) SlotOwnershipRequest {
			r.DirIdentity = "1:2"
			return r
		}},
		{name: "workspace root", edit: `UPDATE workspaces SET root_path='relative'`},
		{name: "missing workspace root", edit: `UPDATE workspaces SET root_path='/missing/workspace'`},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newOwnershipFixture(t)
			request := fixture.slotRequest()
			if test.edit != "" {
				if _, err := fixture.store.db.ExecContext(ctx, test.edit, test.args...); err != nil {
					t.Fatal(err)
				}
			}
			if test.mutate != nil {
				request = test.mutate(fixture, request)
			}
			if err := fixture.store.ValidateSlotOwnership(ctx, request); !errors.Is(err, ErrOwnership) {
				t.Fatalf("invalid slot proof succeeded: %v", err)
			}
		})
	}
}

// TestValidateSlotOwnershipAcceptsAMatchingIdentity は、上の identity 不一致による拒否と対になる。
func TestValidateSlotOwnershipAcceptsAMatchingIdentity(t *testing.T) {
	f := newOwnershipFixture(t)
	ctx := context.Background()
	if _, err := f.store.db.ExecContext(ctx, `UPDATE slots SET dir_identity='16777220:7' WHERE id=?`, f.slotID); err != nil {
		t.Fatal(err)
	}
	request := f.slotRequest()
	request.DirIdentity = "16777220:7"
	if err := f.store.ValidateSlotOwnership(ctx, request); err != nil {
		t.Fatalf("matching slot identity rejected: %v", err)
	}
}

func TestCanonicalWorkspaceKeepsNewRepositoryIdentity(t *testing.T) {
	store := openTestStore(t)
	common := t.TempDir()
	w := discovery.Workspace{
		ID:   "propos",
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
		t.Fatalf("unregistered repository workspace identity=%q, want the caller's proposal %q", canonical.ID, w.ID)
	}
}

// TestSlotStateMismatchIsDistinguishableFromOwnershipDoubt は、state だけが食い違う失敗を
// 呼び出し元が再取得で回復できるよう、位置・identity の不一致と区別できることを検証する。
func TestSlotStateMismatchIsDistinguishableFromOwnershipDoubt(t *testing.T) {
	ctx := context.Background()
	f := newOwnershipFixture(t)
	if _, err := f.store.db.ExecContext(ctx, `UPDATE slots SET state='LEASED' WHERE id=?`, f.slotID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.ValidateWorktreeOwnership(ctx, f.worktreeRequest()); !errors.Is(err, ErrSlotStateIneligible) || !errors.Is(err, ErrOwnership) {
		t.Fatalf("worktree state mismatch=%v", err)
	}
	if err := f.store.ValidateSlotOwnership(ctx, f.slotRequest()); !errors.Is(err, ErrSlotStateIneligible) || !errors.Is(err, ErrOwnership) {
		t.Fatalf("slot state mismatch=%v", err)
	}
	eligible := newOwnershipFixture(t)
	mislocated := eligible.slotRequest()
	mislocated.RelPath = filepath.Join(eligible.workspaceID, "elsewhere")
	if err := eligible.store.ValidateSlotOwnership(ctx, mislocated); errors.Is(err, ErrSlotStateIneligible) || !errors.Is(err, ErrOwnership) {
		t.Fatalf("location mismatch was reported as a state mismatch: %v", err)
	}
}
