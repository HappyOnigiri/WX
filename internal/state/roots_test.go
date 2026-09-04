package state

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/HappyOnigiri/WX/internal/domain"
)

func TestEnsureActiveRootRegistersAndRetiresGenerations(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()

	// openTestStore seeded testRootID as the active generation. Registering a
	// different pathname must make it the only active one without deleting
	// the previous row, because slots still resolve through it.
	newID, err := store.EnsureActiveRoot(ctx, "/wx-next", "next-identity")
	if err != nil {
		t.Fatal(err)
	}
	if !domain.ValidShortID(newID) {
		t.Fatalf("generated root id=%q is not a short id", newID)
	}
	roots, err := store.Roots(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(roots) != 2 {
		t.Fatalf("roots=%+v, want the retired and the active generation", roots)
	}
	byID := map[string]Root{}
	for _, root := range roots {
		byID[root.ID] = root
	}
	if previous := byID[testRootID]; previous.Active || previous.Path != testRootPath {
		t.Fatalf("previous generation=%+v, want retired at its own path", previous)
	}
	if current := byID[newID]; !current.Active || current.Path != "/wx-next" || current.Identity != "next-identity" {
		t.Fatalf("active generation=%+v", current)
	}
	var retiredAt string
	if err := store.db.QueryRowContext(ctx, `SELECT COALESCE(retired_at,'') FROM roots WHERE id=?`, testRootID).Scan(&retiredAt); err != nil {
		t.Fatal(err)
	}
	if retiredAt == "" {
		t.Fatal("retired generation has no retired_at timestamp")
	}

	// Re-registering the pathname keeps its ID: slots are attached to it.
	againID, err := store.EnsureActiveRoot(ctx, "/wx-next", "next-identity")
	if err != nil || againID != newID {
		t.Fatalf("re-registration id=%q err=%v, want %q", againID, err, newID)
	}
	// Coming back to the original pathname reactivates the original row.
	backID, err := store.EnsureActiveRoot(ctx, testRootPath, "root-identity")
	if err != nil || backID != testRootID {
		t.Fatalf("reactivated id=%q err=%v, want %q", backID, err, testRootID)
	}
	roots, err = store.Roots(ctx)
	if err != nil {
		t.Fatal(err)
	}
	active := 0
	for _, root := range roots {
		if root.Active {
			active++
			if root.ID != testRootID {
				t.Fatalf("active generation=%+v, want the reactivated one", root)
			}
		}
	}
	if active != 1 {
		t.Fatalf("active generations=%d, want exactly one", active)
	}
	if err := store.db.QueryRowContext(ctx, `SELECT COALESCE(retired_at,'') FROM roots WHERE id=?`, testRootID).Scan(&retiredAt); err != nil {
		t.Fatal(err)
	}
	if retiredAt != "" {
		t.Fatalf("reactivated generation kept retired_at=%q", retiredAt)
	}
}

func TestEnsureActiveRootRequiresPathAndIdentity(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	for _, test := range []struct{ path, identity string }{{"", "identity"}, {"/wx-next", ""}, {"", ""}} {
		if _, err := store.EnsureActiveRoot(ctx, test.path, test.identity); err == nil {
			t.Errorf("EnsureActiveRoot(%q,%q) succeeded", test.path, test.identity)
		}
	}
}

// TestEnsureActiveRootRefusesAnInodeChangeWithLiveReferences is the
// fail-closed half of the root-identity rule: a root directory that was
// replaced under wx's feet cannot be adopted while durable rows still claim
// locations inside the directory wx originally wrote.
func TestEnsureActiveRootRefusesAnInodeChangeWithLiveReferences(t *testing.T) {
	ctx := context.Background()
	t.Run("slot reference", func(t *testing.T) {
		store := openTestStore(t)
		seedWorkspace(t, store)
		if _, err := store.CreateStandby(ctx, Slot{ID: "slot01", WorkspaceID: "workspace", Generation: 1, RootID: testRootID, RelPath: "workspace/slot01", State: "READY"}, nil); err != nil {
			t.Fatal(err)
		}
		_, err := store.EnsureActiveRoot(ctx, testRootPath, "replaced-identity")
		if !errors.Is(err, ErrOwnership) || !strings.Contains(err.Error(), "inode changed") {
			t.Fatalf("replaced root with a live slot error=%v", err)
		}
		var identity string
		if err := store.db.QueryRowContext(ctx, `SELECT identity FROM roots WHERE id=?`, testRootID).Scan(&identity); err != nil {
			t.Fatal(err)
		}
		if identity != "root-identity" {
			t.Fatalf("refused registration still rewrote the identity to %q", identity)
		}
	})

	t.Run("workspace snapshot reference", func(t *testing.T) {
		store := openTestStore(t)
		seedWorkspace(t, store)
		// The snapshot lives in a second generation that no slot points at,
		// so only the workspace_snapshots row can be what refuses adoption.
		seedRoot(t, store, "root02", "/wx-snapshot", "snapshot-identity", false)
		session := Session{ID: "session", WorkspaceID: "workspace", SlotID: "slot01", State: "ACTIVE", AgentKind: "codex", TokenHash: HashToken("token")}
		if _, err := store.CreateSlotSession(ctx, Slot{ID: "slot01", WorkspaceID: "workspace", Generation: 1, RootID: testRootID, RelPath: "workspace/slot01", State: "LEASED"}, nil, session, ""); err != nil {
			t.Fatal(err)
		}
		if err := store.SaveWorkspaceSnapshot(ctx, WorkspaceSnapshot{SessionID: "session", RootID: "root02", RelPath: "_recovery/workspace-snapshots/session.tar", SHA256: "sha", Status: "ARCHIVED", CreatedAt: now(), ExpiresAt: now()}); err != nil {
			t.Fatal(err)
		}
		if _, err := store.EnsureActiveRoot(ctx, "/wx-snapshot", "replaced-identity"); !errors.Is(err, ErrOwnership) {
			t.Fatalf("replaced root with a live snapshot error=%v", err)
		}
	})
}

// TestEnsureActiveRootAdoptsAnInodeChangeWithNoReferences is the other half:
// a root directory the user deleted and wx recreated strands nothing, so its
// row is updated rather than refused forever.
func TestEnsureActiveRootAdoptsAnInodeChangeWithNoReferences(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	id, err := store.EnsureActiveRoot(ctx, testRootPath, "recreated-identity")
	if err != nil || id != testRootID {
		t.Fatalf("recreated root id=%q err=%v", id, err)
	}
	roots, err := store.Roots(ctx)
	if err != nil || len(roots) != 1 {
		t.Fatalf("roots=%+v err=%v", roots, err)
	}
	if roots[0].Identity != "recreated-identity" || !roots[0].Active {
		t.Fatalf("recreated root=%+v", roots[0])
	}
}

func TestPruneRootsKeepsReferencedAndActiveGenerations(t *testing.T) {
	store := openTestStore(t)
	seedWorkspace(t, store)
	ctx := context.Background()
	seedRoot(t, store, "root02", "/wx-referenced", "referenced-identity", false)
	seedRoot(t, store, "root03", "/wx-orphan", "orphan-identity", false)
	if _, err := store.CreateStandby(ctx, Slot{ID: "slot01", WorkspaceID: "workspace", Generation: 1, RootID: "root02", RelPath: "workspace/slot01", State: "READY"}, nil); err != nil {
		t.Fatal(err)
	}
	if err := store.PruneRoots(ctx); err != nil {
		t.Fatal(err)
	}
	roots, err := store.Roots(ctx)
	if err != nil {
		t.Fatal(err)
	}
	kept := map[string]bool{}
	for _, root := range roots {
		kept[root.ID] = true
	}
	if !kept[testRootID] {
		t.Fatal("prune deleted the active generation")
	}
	if !kept["root02"] {
		t.Fatal("prune deleted a generation a slot still references")
	}
	if kept["root03"] {
		t.Fatal("prune kept an unreferenced retired generation")
	}
}

// TestPruneRootsDeletesAGenerationOnceItsSnapshotIsGone pairs with the
// workspace_snapshots half of the reference check above.
func TestPruneRootsDeletesAGenerationOnceItsSnapshotIsGone(t *testing.T) {
	store := openTestStore(t)
	seedWorkspace(t, store)
	ctx := context.Background()
	seedRoot(t, store, "root02", "/wx-snapshot", "snapshot-identity", false)
	session := Session{ID: "session", WorkspaceID: "workspace", SlotID: "slot01", State: "ACTIVE", AgentKind: "codex", TokenHash: HashToken("token")}
	if _, err := store.CreateSlotSession(ctx, Slot{ID: "slot01", WorkspaceID: "workspace", Generation: 1, RootID: testRootID, RelPath: "workspace/slot01", State: "LEASED"}, nil, session, ""); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveWorkspaceSnapshot(ctx, WorkspaceSnapshot{SessionID: "session", RootID: "root02", RelPath: "_recovery/workspace-snapshots/session.tar", SHA256: "sha", Status: "ARCHIVED", CreatedAt: now(), ExpiresAt: now()}); err != nil {
		t.Fatal(err)
	}
	if err := store.PruneRoots(ctx); err != nil {
		t.Fatal(err)
	}
	var remaining int
	if err := store.db.QueryRowContext(ctx, `SELECT count(*) FROM roots WHERE id='root02'`).Scan(&remaining); err != nil {
		t.Fatal(err)
	}
	if remaining != 1 {
		t.Fatal("prune deleted a generation a workspace snapshot still references")
	}
	if _, err := store.db.ExecContext(ctx, `DELETE FROM workspace_snapshots WHERE session_id='session'`); err != nil {
		t.Fatal(err)
	}
	if err := store.PruneRoots(ctx); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRowContext(ctx, `SELECT count(*) FROM roots WHERE id='root02'`).Scan(&remaining); err != nil {
		t.Fatal(err)
	}
	if remaining != 0 {
		t.Fatal("prune kept a generation with no remaining references")
	}
}

func TestIsIDCollisionRecognisesOnlyConstraintViolations(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	_, err := store.db.ExecContext(ctx, `INSERT INTO roots(id,path,identity,active,created_at) VALUES(?,?,?,1,?)`, testRootID, "/duplicate", "identity", now())
	if err == nil {
		t.Fatal("duplicate root primary key was accepted")
	}
	if !IsIDCollision(err) {
		t.Fatalf("primary key violation not reported as an ID collision: %v", err)
	}
	_, err = store.db.ExecContext(ctx, `INSERT INTO roots(id,path,identity,active,created_at) VALUES('root02',?,'identity',1,?)`, testRootPath, now())
	if err == nil {
		t.Fatal("duplicate root path was accepted")
	}
	if !IsIDCollision(err) {
		t.Fatalf("unique violation not reported as an ID collision: %v", err)
	}
	if _, err := store.db.ExecContext(ctx, `INSERT INTO roots(id,path) VALUES('root03','/missing-columns')`); err == nil {
		t.Fatal("row missing NOT NULL columns was accepted")
	} else if IsIDCollision(err) {
		t.Fatalf("NOT NULL violation reported as an ID collision: %v", err)
	}
	if IsIDCollision(nil) {
		t.Fatal("nil error reported as an ID collision")
	}
	if IsIDCollision(errors.New("plain failure")) {
		t.Fatal("plain error reported as an ID collision")
	}
}

// TestNewUnusedShortIDSkipsTakenIdentifiers proves the generator consults the
// table it is drawing for, which is what keeps a random workspace ID from
// silently taking over an existing row through the upsert's ON CONFLICT.
func TestNewUnusedShortIDSkipsTakenIdentifiers(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	id, err := newUnusedShortID(ctx, store.db, "roots")
	if err != nil {
		t.Fatal(err)
	}
	if !domain.ValidShortID(id) {
		t.Fatalf("generated id=%q is not a short id", id)
	}
	if id == testRootID {
		t.Fatal("generated id collided with the seeded row")
	}
	if _, err := newUnusedShortID(ctx, store.db, "missing_table"); err == nil {
		t.Fatal("draw against a missing table succeeded")
	}
}

func TestRecordSlotRepositoryIdentityRequiresAnExistingRow(t *testing.T) {
	store := openTestStore(t)
	seedWorkspace(t, store)
	ctx := context.Background()
	if _, err := store.CreateStandby(ctx, Slot{ID: "slot01", WorkspaceID: "workspace", Generation: 1, RootID: testRootID, RelPath: "workspace/slot01", State: "READY"}, []SlotRepository{{RepositoryID: "repository", DirName: "repository", State: "PREPARING", RequestedRef: "main", BaseOID: "abc", Fingerprint: "fp"}}); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordSlotRepositoryIdentity(ctx, "slot01", "repository", ""); err == nil {
		t.Fatal("empty identity was recorded")
	}
	if err := store.RecordSlotRepositoryIdentity(ctx, "slot01", "absent", "1:2"); err == nil {
		t.Fatal("identity was recorded for an unregistered repository")
	}
	if err := store.RecordSlotRepositoryIdentity(ctx, "slot01", "repository", "1:2"); err != nil {
		t.Fatal(err)
	}
	stored, err := store.SlotRepository(ctx, "slot01", "repository")
	if err != nil {
		t.Fatal(err)
	}
	if stored.DirIdentity != "1:2" {
		t.Fatalf("recorded identity=%q", stored.DirIdentity)
	}
	if stored.WorktreePath != filepath.Join(testRootPath, "workspace", "slot01", "repository") {
		t.Fatalf("composed worktree path=%q", stored.WorktreePath)
	}
}

// TestSlotAndRepositoryPathsComposeFromTheirRootGeneration pins the read-side
// derivation: nothing stores an absolute path any more, so a slot in a
// retired generation must still report a path under that generation's root.
func TestSlotAndRepositoryPathsComposeFromTheirRootGeneration(t *testing.T) {
	store := openTestStore(t)
	seedWorkspace(t, store)
	ctx := context.Background()
	const retiredRootPath = "/wx-retired"
	seedRoot(t, store, "root02", retiredRootPath, "retired-identity", false)
	if _, err := store.CreateStandby(ctx, Slot{ID: "slot01", WorkspaceID: "workspace", Generation: 1, RootID: "root02", RelPath: "workspace/slot01", DirIdentity: "9:9", State: "READY"}, []SlotRepository{{RepositoryID: "repository", DirName: "WX", State: "READY", RequestedRef: "main", BaseOID: "abc", Fingerprint: "fp"}}); err != nil {
		t.Fatal(err)
	}
	slot, err := store.Slot(ctx, "slot01")
	if err != nil {
		t.Fatal(err)
	}
	if slot.RootID != "root02" || slot.RelPath != "workspace/slot01" || slot.DirIdentity != "9:9" {
		t.Fatalf("slot location=%+v", slot)
	}
	if slot.Path != filepath.Join(retiredRootPath, "workspace", "slot01") {
		t.Fatalf("composed slot path=%q", slot.Path)
	}
	repositories, err := store.SlotRepositories(ctx, "slot01")
	if err != nil || len(repositories) != 1 {
		t.Fatalf("slot repositories=%+v err=%v", repositories, err)
	}
	if repositories[0].DirName != "WX" || repositories[0].WorktreePath != filepath.Join(retiredRootPath, "workspace", "slot01", "WX") {
		t.Fatalf("composed worktree path=%+v", repositories[0])
	}
	ready, err := store.ReadySlots(ctx, "workspace")
	if err != nil || len(ready) != 1 || ready[0].Path != slot.Path {
		t.Fatalf("READY slots=%+v err=%v", ready, err)
	}
	artifacts, err := store.SlotArtifacts(ctx)
	if err != nil || len(artifacts) != 1 || artifacts[0].Path != slot.Path {
		t.Fatalf("slot artifacts=%+v err=%v", artifacts, err)
	}
}

// TestSlotLayoutUniquenessIsPerRootGeneration proves the schema constraints
// the new layout depends on: one slot directory per root generation, and one
// repository directory name per slot.
func TestSlotLayoutUniquenessIsPerRootGeneration(t *testing.T) {
	store := openTestStore(t)
	seedWorkspace(t, store)
	ctx := context.Background()
	seedRoot(t, store, "root02", "/wx-other", "other-identity", false)
	if _, err := store.CreateStandby(ctx, Slot{ID: "slot01", WorkspaceID: "workspace", Generation: 1, RootID: testRootID, RelPath: "workspace/slot01", State: "READY"}, nil); err != nil {
		t.Fatal(err)
	}
	// The same relative location in a different generation is a different
	// directory, so it must be accepted.
	if _, err := store.CreateStandby(ctx, Slot{ID: "slot02", WorkspaceID: "workspace", Generation: 1, RootID: "root02", RelPath: "workspace/slot01", State: "READY"}, nil); err != nil {
		t.Fatalf("same relative location in another generation rejected: %v", err)
	}
	// The same location in the same generation is the same directory.
	_, err := store.CreateStandby(ctx, Slot{ID: "slot03", WorkspaceID: "workspace", Generation: 1, RootID: testRootID, RelPath: "workspace/slot01", State: "READY"}, nil)
	if err == nil {
		t.Fatal("duplicate slot location in one generation was accepted")
	}
	if !IsIDCollision(err) {
		t.Fatalf("duplicate slot location error is not a constraint violation: %v", err)
	}
	if _, err := store.db.ExecContext(ctx, `INSERT INTO slot_repositories(slot_id,repository_id,dir_name,state,requested_ref,base_oid,prepare_fingerprint) VALUES('slot01','repository','WX','READY','main','abc','fp')`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `INSERT INTO repositories(id,main_worktree_path,common_git_dir,default_branch,remote_name,first_seen_at,last_seen_at) VALUES('repository-2','/other','/other/.git','main','',?,?)`, now(), now()); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `INSERT INTO slot_repositories(slot_id,repository_id,dir_name,state,requested_ref,base_oid,prepare_fingerprint) VALUES('slot01','repository-2','WX','READY','main','abc','fp')`); err == nil {
		t.Fatal("two repositories claimed one directory name in a slot")
	}
}

func TestValidateWorktreeOwnershipRejectsAMultiComponentRepositoryDirectory(t *testing.T) {
	// filepath.IsLocal accepts "a/b", so a recorded dir_name has to be
	// refused for depth explicitly: a repository worktree must be a direct
	// child of its slot directory, and UNIQUE(slot_id, dir_name) alone does
	// not enforce that.
	if err := validateOwnershipComponent("repository directory", "nested/repository"); err == nil {
		t.Fatal("a two-component repository directory was accepted")
	}
	if err := validateOwnershipComponent("repository directory", "repository"); err != nil {
		t.Fatalf("single-component repository directory rejected: %v", err)
	}
	if err := validateOwnershipRelative("slot path", "workspace/slot"); err != nil {
		t.Fatalf("two-component slot path rejected: %v", err)
	}
}

func TestOpenRefusesADatabaseFromThePreviousWorktreeLayout(t *testing.T) {
	// A pre-layout database already reports user_version=1, so the migration
	// loop applies nothing and the mismatch would otherwise surface one
	// failed RPC at a time.
	path := filepath.Join(t.TempDir(), "state.db")
	legacy, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`CREATE TABLE slots(id TEXT PRIMARY KEY, path TEXT UNIQUE NOT NULL, state TEXT NOT NULL)`,
		`PRAGMA user_version=1`,
	} {
		if _, err := legacy.ExecContext(context.Background(), statement); err != nil {
			t.Fatal(err)
		}
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}
	store, err := Open(path)
	if err == nil {
		_ = store.Close()
		t.Fatal("a database from the previous worktree layout was accepted")
	}
	if !strings.Contains(err.Error(), "previous worktree layout") {
		t.Fatalf("error=%v; it must name the layout change and say what to remove", err)
	}
	if !errors.Is(err, ErrPreviousWorktreeLayout) {
		t.Fatalf("error=%v; it must carry ErrPreviousWorktreeLayout", err)
	}
}
