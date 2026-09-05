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

	// openTestStore は testRootID を active generation として seed した。別 pathname を登録すると以前の row を削除せず新しいものだけを active にする。slot は以前の row でも解決するためである。
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

	// pathname を再登録しても ID を保つ。slot はその ID に紐付くためである。
	againID, err := store.EnsureActiveRoot(ctx, "/wx-next", "next-identity")
	if err != nil || againID != newID {
		t.Fatalf("re-registration id=%q err=%v, want %q", againID, err, newID)
	}
	// 元の pathname に戻ると元の row を再 active 化する。
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

// TestEnsureActiveRootRefusesAnInodeChangeWithLiveReferences は root identity 規則のフェイルクローズ側を検証する。
// wx の管理下で置換された root directory は、wx が元に書いた directory 内 location を durable row がまだ主張する間は採用できない。
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
		// snapshot は slot が参照しない二つ目 generation にあるため、採用拒否の原因は workspace_snapshots row だけになる。
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

// TestEnsureActiveRootAdoptsAnInodeChangeWithNoReferences はもう一方を検証する。
// ユーザーが root directory を削除し wx が再作成しても state を取り残さない場合は、永続拒否せず row を更新する。
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

// TestPruneRootsDeletesAGenerationOnceItsSnapshotIsGone は、上の参照検査における workspace_snapshots 側を検証する。
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

// TestNewUnusedShortIDSkipsTakenIdentifiers は、生成器が対象 table を参照することを検証する。
// これにより random workspace ID が upsert の ON CONFLICT 経由で既存 row を密かに奪わない。
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

// TestSlotAndRepositoryPathsComposeFromTheirRootGeneration は read 側の派生規則を固定する。
// absolute path は保存しないため、retired generation の slot もその generation の root 配下を返す。
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

// TestSlotLayoutUniquenessIsPerRootGeneration は新 layout が依存する schema 制約を検証する。
// root generation ごとに slot directory は1つ、slot ごとに repository directory 名は1つである。
func TestSlotLayoutUniquenessIsPerRootGeneration(t *testing.T) {
	store := openTestStore(t)
	seedWorkspace(t, store)
	ctx := context.Background()
	seedRoot(t, store, "root02", "/wx-other", "other-identity", false)
	if _, err := store.CreateStandby(ctx, Slot{ID: "slot01", WorkspaceID: "workspace", Generation: 1, RootID: testRootID, RelPath: "workspace/slot01", State: "READY"}, nil); err != nil {
		t.Fatal(err)
	}
	// 別 generation の同じ相対 location は別 directory なので受理する。
	if _, err := store.CreateStandby(ctx, Slot{ID: "slot02", WorkspaceID: "workspace", Generation: 1, RootID: "root02", RelPath: "workspace/slot01", State: "READY"}, nil); err != nil {
		t.Fatalf("same relative location in another generation rejected: %v", err)
	}
	// 同じ generation の同じ location は同じ directory である。
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
	// filepath.IsLocal は "a/b" を受理するため、記録する dir_name は深さを明示的に拒否する。
	// repository worktree は slot directory の直下でなければならず、UNIQUE(slot_id, dir_name) だけでは強制できない。
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
	// layout 前の database は既に user_version=1 を返すため migration loop は何も適用せず、
	// この不一致を放置すると RPC が1回ずつ失敗して初めて表面化する。
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
