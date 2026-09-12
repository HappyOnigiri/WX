package state

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/HappyOnigiri/WX/internal/discovery"
	"github.com/HappyOnigiri/WX/internal/domain"
)

func TestSessionRepositoryMembershipSurvivesWorkspaceReconciliation(t *testing.T) {
	t.Parallel()
	store := openTestStore(t)
	ctx := context.Background()
	root := t.TempDir()
	workspace := discovery.Workspace{
		ID:   "workspace",
		Root: domain.CanonicalPath(root),
		Kind: "multi_repository",
		Repositories: []discovery.Repository{
			{ID: "repository-a", MainPath: domain.CanonicalPath(filepath.Join(root, "a")), CommonDir: domain.CanonicalPath(filepath.Join(root, "a.git")), RelativePath: "a", DefaultBranch: "main"},
			{ID: "repository-b", MainPath: domain.CanonicalPath(filepath.Join(root, "b")), CommonDir: domain.CanonicalPath(filepath.Join(root, "b.git")), RelativePath: "nested/b", DefaultBranch: "main"},
		},
	}
	registered, _, err := store.UpsertWorkspaceGeneration(ctx, workspace)
	if err != nil {
		t.Fatal(err)
	}
	workspaceID := string(registered.ID)
	session := Session{ID: "session", WorkspaceID: workspaceID, SlotID: "slot", State: "ACTIVE", AgentKind: "codex", TokenHash: HashToken("token")}
	repositories := []SlotRepository{
		{RepositoryID: "repository-a", DirName: "a", State: "READY", RequestedRef: "main", BaseOID: "a", Fingerprint: "a"},
		{RepositoryID: "repository-b", DirName: "b", State: "READY", RequestedRef: "main", BaseOID: "b", Fingerprint: "b"},
	}
	if _, err := store.CreateSlotSession(ctx, Slot{ID: "slot", WorkspaceID: workspaceID, Generation: 1, RootID: testRootID, RelPath: filepath.Join(workspaceID, "slot"), State: "LEASED"}, repositories, session, ""); err != nil {
		t.Fatal(err)
	}
	workspace.Repositories = workspace.Repositories[:1]
	if _, _, err := store.UpsertWorkspaceGeneration(ctx, workspace); err != nil {
		t.Fatal(err)
	}
	historical, err := store.SessionWorkspace(ctx, session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(historical.Repositories) != 2 || historical.Repositories[0].RelativePath != "a" || historical.Repositories[1].RelativePath != "nested/b" {
		t.Fatalf("historical membership=%+v", historical.Repositories)
	}
	current, err := store.Workspace(ctx, workspaceID)
	if err != nil || len(current.Repositories) != 1 {
		t.Fatalf("current membership=%+v err=%v", current.Repositories, err)
	}
}

func TestCanonicalWorkspaceKeepsSlotsWhenMainWorktreeMoves(t *testing.T) {
	t.Parallel()
	store := openTestStore(t)
	ctx := context.Background()
	oldMain := filepath.Join(t.TempDir(), "old-main")
	newMain := filepath.Join(t.TempDir(), "new-main")
	common := filepath.Join(t.TempDir(), "common.git")
	registered := discovery.Workspace{
		ID:   "old-workspace-id",
		Root: domain.CanonicalPath(oldMain), Kind: "repository",
		Repositories: []discovery.Repository{{
			ID: "repository", MainPath: domain.CanonicalPath(oldMain), CommonDir: domain.CanonicalPath(common), RelativePath: ".", DefaultBranch: "main",
		}},
	}
	stored, _, err := store.UpsertWorkspaceGeneration(ctx, registered)
	if err != nil {
		t.Fatal(err)
	}
	workspaceID := string(stored.ID)
	if _, err := store.CreateStandby(ctx, Slot{ID: "standby", WorkspaceID: workspaceID, Generation: 1, RootID: testRootID, RelPath: filepath.Join(workspaceID, "standby"), State: "READY"}, nil); err != nil {
		t.Fatal(err)
	}

	discovered := registered
	discovered.ID = "propos"
	discovered.Root = domain.CanonicalPath(newMain)
	discovered.Repositories[0].MainPath = domain.CanonicalPath(newMain)
	canonical, err := store.CanonicalWorkspace(ctx, discovered)
	if err != nil {
		t.Fatal(err)
	}
	if string(canonical.ID) != workspaceID || canonical.Root != discovered.Root {
		t.Fatalf("canonical workspace=%+v, want existing ID with moved root", canonical)
	}
	if _, _, err := store.UpsertWorkspaceGeneration(ctx, discovered); err != nil {
		t.Fatal(err)
	}
	status, err := store.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if status.Workspaces != 1 {
		t.Fatalf("workspace count=%d, want one registered identity", status.Workspaces)
	}
	updated, err := store.Workspace(ctx, workspaceID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Root != discovered.Root || updated.Repositories[0].MainPath != discovered.Repositories[0].MainPath {
		t.Fatalf("updated workspace=%+v", updated)
	}
	if slots, err := store.ReadySlots(ctx, workspaceID); err != nil || len(slots) != 1 || slots[0].ID != "standby" {
		t.Fatalf("existing slots=%+v err=%v", slots, err)
	}
	if slots, err := store.ReadySlots(ctx, string(discovered.ID)); err != nil || len(slots) != 0 {
		t.Fatalf("new identity unexpectedly has slots=%+v err=%v", slots, err)
	}
}

func TestCanonicalWorkspaceRelocationRejectsConflictsWithoutMutation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("duplicate common directory identity", func(t *testing.T) {
		store := openTestStore(t)
		common := "/repository/common.git"
		registered := discovery.Workspace{ID: "propos", Root: "/registered", Kind: "repository", Repositories: []discovery.Repository{{ID: "repository", MainPath: "/registered", CommonDir: domain.CanonicalPath(common), RelativePath: ".", DefaultBranch: "main"}}}
		stored, _, err := store.UpsertWorkspaceGeneration(ctx, registered)
		if err != nil {
			t.Fatal(err)
		}
		// 既存の不整合な registry を再現する。common directory は repository ごとに一意だが、破損した registry は同じ repository を複数 workspace row に関連付け得る。
		if _, err := store.db.ExecContext(ctx, `INSERT INTO workspaces(id,root_path,kind,generation,discovery_state,first_seen_at,last_seen_at,last_reconciled_at) VALUES(?,?,?,?,?,?,?,?)`, "conflict", "/conflict", "repository", 1, "READY", now(), now(), now()); err != nil {
			t.Fatal(err)
		}
		if _, err := store.db.ExecContext(ctx, `INSERT INTO workspace_repositories(workspace_id,repository_id,relative_path,ordinal) VALUES(?,?,?,?)`, "conflict", "repository", ".", 0); err != nil {
			t.Fatal(err)
		}
		candidate := registered
		candidate.ID = "new-identity"
		candidate.Root = "/new-root"
		candidate.Repositories[0].MainPath = "/new-root"
		if _, err := store.CanonicalWorkspace(ctx, candidate); err == nil || !strings.Contains(err.Error(), "multiple registered workspaces") {
			t.Fatalf("conflicting common directory accepted: %v", err)
		}
		if _, _, err := store.UpsertWorkspaceGeneration(ctx, candidate); err == nil {
			t.Fatal("upsert proceeded through conflicting common directory")
		}
		status, err := store.Status(ctx)
		if err != nil || status.Workspaces != 2 {
			t.Fatalf("conflict changed registry: status=%+v err=%v", status, err)
		}
		loaded, err := store.Workspace(ctx, string(stored.ID))
		if err != nil || loaded.Root != registered.Root {
			t.Fatalf("registered workspace changed after conflict: workspace=%+v err=%v", loaded, err)
		}
	})

	t.Run("root uniqueness conflict", func(t *testing.T) {
		store := openTestStore(t)
		old := discovery.Workspace{ID: "propos", Root: "/old", Kind: "repository", Repositories: []discovery.Repository{{ID: "old-repository", MainPath: "/old", CommonDir: "/old/common.git", RelativePath: ".", DefaultBranch: "main"}}}
		other := discovery.Workspace{ID: "propo2", Root: "/new", Kind: "repository", Repositories: []discovery.Repository{{ID: "other-repository", MainPath: "/new", CommonDir: "/new/common.git", RelativePath: ".", DefaultBranch: "main"}}}
		storedOld, _, err := store.UpsertWorkspaceGeneration(ctx, old)
		if err != nil {
			t.Fatal(err)
		}
		oldID := string(storedOld.ID)
		if _, _, err := store.UpsertWorkspaceGeneration(ctx, other); err != nil {
			t.Fatal(err)
		}
		session := Session{ID: "session", WorkspaceID: oldID, SlotID: "slot", State: "ACTIVE", AgentKind: "codex", TokenHash: HashToken("token")}
		if _, err := store.CreateSlotSession(ctx, Slot{ID: "slot", WorkspaceID: oldID, Generation: 1, RootID: testRootID, RelPath: filepath.Join(oldID, "slot"), State: "LEASED"}, nil, session, ""); err != nil {
			t.Fatal(err)
		}
		oldRoot, oldMain := old.Root, old.Repositories[0].MainPath
		candidate := old
		candidate.Repositories = append([]discovery.Repository(nil), old.Repositories...)
		candidate.Root = "/new"
		candidate.Repositories[0].MainPath = "/new"
		if _, _, err := store.UpsertWorkspaceGeneration(ctx, candidate); err == nil {
			t.Fatal("root uniqueness conflict was accepted")
		}
		loaded, err := store.Workspace(ctx, oldID)
		if err != nil || loaded.Root != oldRoot || loaded.Repositories[0].MainPath != oldMain {
			t.Fatalf("root conflict partially moved workspace: workspace=%+v err=%v", loaded, err)
		}
		storedSession, err := store.SessionByID(ctx, session.ID)
		if err != nil || storedSession.WorkspaceID != oldID {
			t.Fatalf("root conflict detached session: session=%+v err=%v", storedSession, err)
		}
	})

	t.Run("storage failure", func(t *testing.T) {
		store := openTestStore(t)
		old := discovery.Workspace{ID: "propos", Root: "/old", Kind: "repository", Repositories: []discovery.Repository{{ID: "repository", MainPath: "/old", CommonDir: "/old/common.git", RelativePath: ".", DefaultBranch: "main"}}}
		storedOld, _, err := store.UpsertWorkspaceGeneration(ctx, old)
		if err != nil {
			t.Fatal(err)
		}
		oldID := string(storedOld.ID)
		if _, err := store.db.ExecContext(ctx, `CREATE TRIGGER fail_relocation_repository_update BEFORE UPDATE OF main_worktree_path ON repositories BEGIN SELECT RAISE(ABORT,'fault'); END`); err != nil {
			t.Fatal(err)
		}
		oldRoot, oldMain := old.Root, old.Repositories[0].MainPath
		candidate := old
		candidate.Repositories = append([]discovery.Repository(nil), old.Repositories...)
		candidate.Root = "/new"
		candidate.Repositories[0].MainPath = "/new"
		if _, _, err := store.UpsertWorkspaceGeneration(ctx, candidate); err == nil {
			t.Fatal("storage failure was ignored")
		}
		loaded, err := store.Workspace(ctx, oldID)
		if err != nil || loaded.Root != oldRoot || loaded.Repositories[0].MainPath != oldMain {
			t.Fatalf("storage failure partially moved workspace: workspace=%+v err=%v", loaded, err)
		}
		var workspaces int
		if err := store.db.QueryRowContext(ctx, `SELECT count(*) FROM workspaces`).Scan(&workspaces); err != nil || workspaces != 1 {
			t.Fatalf("storage failure created a duplicate workspace: count=%d err=%v", workspaces, err)
		}
	})
}

func TestForgetWorkspaceRefusesLiveRecoveryMappings(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		seed  func(*testing.T, *Store, context.Context)
		want  string
		clear func(*testing.T, *Store, context.Context)
	}{
		{
			name: "session mapping",
			seed: func(t *testing.T, store *Store, ctx context.Context) {
				session := Session{ID: "session", WorkspaceID: "workspace", SlotID: "slot", State: "ARCHIVED", AgentKind: "codex", TokenHash: HashToken("token")}
				if _, err := store.CreateSlotSession(ctx, Slot{ID: "slot", WorkspaceID: "workspace", Generation: 1, RootID: testRootID, RelPath: "workspace/slot", State: "ARCHIVED"}, nil, session, ""); err != nil {
					t.Fatal(err)
				}
			},
			want: "live session mappings",
			clear: func(t *testing.T, store *Store, ctx context.Context) {
				if _, err := store.db.ExecContext(ctx, `UPDATE sessions SET state='EXPIRED' WHERE id='session'`); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "repository snapshot",
			seed: func(t *testing.T, store *Store, ctx context.Context) {
				session := Session{ID: "session", WorkspaceID: "workspace", SlotID: "slot", State: "EXPIRED", AgentKind: "codex", TokenHash: HashToken("token")}
				if _, err := store.CreateSlotSession(ctx, Slot{ID: "slot", WorkspaceID: "workspace", Generation: 1, RootID: testRootID, RelPath: "workspace/slot", State: "ARCHIVED"}, nil, session, ""); err != nil {
					t.Fatal(err)
				}
				if err := store.SaveSnapshot(ctx, Snapshot{ID: "snapshot", SessionID: "session", RepositoryID: "repository", HeadOID: "head", HeadRef: "refs/wx/recovery/head", IndexTreeOID: "index", WorktreeOID: "worktree", WorktreeRef: "refs/wx/recovery/worktree", Status: "ARCHIVED", CreatedAt: now(), ExpiresAt: FormatTime(time.Now().Add(time.Hour))}); err != nil {
					t.Fatal(err)
				}
			},
			want: "recovery snapshots",
			clear: func(t *testing.T, store *Store, ctx context.Context) {
				if err := store.ExpireSessionSnapshots(ctx, "session"); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "workspace snapshot",
			seed: func(t *testing.T, store *Store, ctx context.Context) {
				session := Session{ID: "session", WorkspaceID: "workspace", SlotID: "slot", State: "EXPIRED", AgentKind: "codex", TokenHash: HashToken("token")}
				if _, err := store.CreateSlotSession(ctx, Slot{ID: "slot", WorkspaceID: "workspace", Generation: 1, RootID: testRootID, RelPath: "workspace/slot", State: "ARCHIVED"}, nil, session, ""); err != nil {
					t.Fatal(err)
				}
				if err := store.SaveWorkspaceSnapshot(ctx, WorkspaceSnapshot{SessionID: "session", RootID: testRootID, RelPath: "_recovery/workspace-snapshots/snapshot.tar", SHA256: strings.Repeat("a", 64), Status: "ARCHIVED", CreatedAt: now(), ExpiresAt: FormatTime(time.Now().Add(time.Hour))}); err != nil {
					t.Fatal(err)
				}
			},
			want: "a workspace recovery snapshot",
			clear: func(t *testing.T, store *Store, ctx context.Context) {
				if err := store.ExpireSessionSnapshots(ctx, "session"); err != nil {
					t.Fatal(err)
				}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := openTestStore(t)
			seedWorkspace(t, store)
			ctx := context.Background()
			test.seed(t, store, ctx)
			if err := store.ForgetWorkspace(ctx, "/workspace"); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("forget error=%v, want refusal containing %q", err, test.want)
			}
			if _, err := store.Workspace(ctx, "workspace"); err != nil {
				t.Fatalf("refused forget removed workspace: %v", err)
			}
			test.clear(t, store, ctx)
			if err := store.ForgetWorkspace(ctx, "/workspace"); err != nil {
				t.Fatalf("forget after recovery cleanup: %v", err)
			}
		})
	}
}

func TestWorkspaceMembershipChangeAdvancesGenerationAndStalesOldStandby(t *testing.T) {
	t.Parallel()
	store := openTestStore(t)
	ctx := context.Background()
	w := discovery.Workspace{ID: "propos", Root: "/workspace", Kind: "multi_repository", Repositories: []discovery.Repository{{ID: "repository", MainPath: "/workspace/repository", CommonDir: "/workspace/repository/.git", RelativePath: "repository", DefaultBranch: "main"}}}
	registered, generation, err := store.UpsertWorkspaceGeneration(ctx, w)
	if err != nil || generation != 1 {
		t.Fatalf("initial generation=%d err=%v", generation, err)
	}
	workspaceID := string(registered.ID)
	// ID は caller の値を使わず再抽選する。directory 名になるため短い base36 値であり、無関係な workspace row と衝突してはならない。
	if workspaceID == string(w.ID) || !domain.ValidShortID(workspaceID) {
		t.Fatalf("assigned workspace id=%q, want a fresh short id", workspaceID)
	}
	job, err := store.CreateStandby(ctx, Slot{ID: "slot", WorkspaceID: workspaceID, Generation: generation, RootID: testRootID, RelPath: filepath.Join(workspaceID, "slot"), State: "READY"}, []SlotRepository{{RepositoryID: "repository", DirName: "repository", State: "READY", RequestedRef: "main", BaseOID: "abc", Fingerprint: "fingerprint"}})
	if err != nil || job.Kind != "PREPARE" {
		t.Fatalf("create standby job=%+v err=%v", job, err)
	}
	// multi-repository workspace は root path で再解決するため、二回目の呼出しは ID を再抽選して重複の generation を進めず、同じ row に到達する必要がある。
	if again, generation, err := store.UpsertWorkspaceGeneration(ctx, w); err != nil || generation != 1 || string(again.ID) != workspaceID {
		t.Fatalf("unchanged generation=%d id=%q err=%v", generation, again.ID, err)
	}
	w.Repositories = append(w.Repositories, discovery.Repository{ID: "repository-2", MainPath: "/workspace/repository-2", CommonDir: "/workspace/repository-2/.git", RelativePath: "repository-2", DefaultBranch: "main"})
	if again, generation, err := store.UpsertWorkspaceGeneration(ctx, w); err != nil || generation != 2 || string(again.ID) != workspaceID {
		t.Fatalf("changed generation=%d id=%q err=%v", generation, again.ID, err)
	}
	slot, err := store.Slot(ctx, "slot")
	if err != nil || slot.State != "STALE" {
		t.Fatalf("old slot=%+v err=%v", slot, err)
	}
	loaded, err := store.Workspace(ctx, workspaceID)
	if err != nil || len(loaded.Repositories) != 2 {
		t.Fatalf("workspace repositories=%d err=%v", len(loaded.Repositories), err)
	}
}

func TestUpsertWorkspaceGenerationRejectsKindChangeAtSameRoot(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		existing  discovery.Workspace
		candidate discovery.Workspace
	}{
		{
			name: "multi repository to repository",
			existing: discovery.Workspace{
				ID: "multi-proposal", Root: "/workspace", Kind: "multi_repository",
				Repositories: []discovery.Repository{{ID: "repository-a", MainPath: "/workspace/a", CommonDir: "/workspace/a/.git", RelativePath: "a", DefaultBranch: "main"}},
			},
			candidate: discovery.Workspace{
				ID: "repository-proposal", Root: "/workspace", Kind: "repository",
				Repositories: []discovery.Repository{{ID: "repository-root", MainPath: "/workspace", CommonDir: "/workspace/.git", RelativePath: ".", DefaultBranch: "main"}},
			},
		},
		{
			name: "repository to multi repository",
			existing: discovery.Workspace{
				ID: "repository-proposal", Root: "/workspace", Kind: "repository",
				Repositories: []discovery.Repository{{ID: "repository-root", MainPath: "/workspace", CommonDir: "/workspace/.git", RelativePath: ".", DefaultBranch: "main"}},
			},
			candidate: discovery.Workspace{
				ID: "multi-proposal", Root: "/workspace", Kind: "multi_repository",
				Repositories: []discovery.Repository{{ID: "repository-a", MainPath: "/workspace/a", CommonDir: "/workspace/a/.git", RelativePath: "a", DefaultBranch: "main"}},
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := openTestStore(t)
			ctx := context.Background()
			stored, generation, err := store.UpsertWorkspaceGeneration(ctx, test.existing)
			if err != nil || generation != 1 {
				t.Fatalf("initial workspace=%+v generation=%d err=%v", stored, generation, err)
			}
			_, _, err = store.UpsertWorkspaceGeneration(ctx, test.candidate)
			if !errors.Is(err, ErrWorkspaceKindConflict) || strings.Contains(err.Error(), "UNIQUE constraint failed") {
				t.Fatalf("kind change error=%v, want dedicated conflict without raw UNIQUE error", err)
			}
			if !strings.Contains(err.Error(), test.existing.Kind) || !strings.Contains(err.Error(), test.candidate.Kind) {
				t.Fatalf("kind change error=%v, want both workspace kinds", err)
			}
			loaded, err := store.Workspace(ctx, string(stored.ID))
			if err != nil || loaded.Kind != test.existing.Kind || loaded.Root != test.existing.Root {
				t.Fatalf("existing workspace changed: workspace=%+v err=%v", loaded, err)
			}
			status, err := store.Status(ctx)
			if err != nil || status.Workspaces != 1 {
				t.Fatalf("workspace count=%d err=%v, want one unchanged registration", status.Workspaces, err)
			}
		})
	}
}

func TestUpsertWorkspaceGenerationRejectsSameKindRootIdentityChange(t *testing.T) {
	t.Parallel()
	store := openTestStore(t)
	ctx := context.Background()
	existing := discovery.Workspace{
		ID: "existing-proposal", Root: "/workspace", Kind: "repository",
		Repositories: []discovery.Repository{{ID: "repository", MainPath: "/workspace", CommonDir: "/workspace/.git", RelativePath: ".", DefaultBranch: "main"}},
	}
	candidate := existing
	candidate.ID = "candidate-proposal"
	candidate.Repositories = []discovery.Repository{{ID: "other-repository", MainPath: "/workspace", CommonDir: "/other/.git", RelativePath: ".", DefaultBranch: "main"}}
	stored, _, err := store.UpsertWorkspaceGeneration(ctx, existing)
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = store.UpsertWorkspaceGeneration(ctx, candidate)
	if !errors.Is(err, ErrWorkspaceIdentityConflict) || strings.Contains(err.Error(), "UNIQUE constraint failed") {
		t.Fatalf("identity change error=%v, want dedicated conflict without raw UNIQUE error", err)
	}
	loaded, err := store.Workspace(ctx, string(stored.ID))
	if err != nil || loaded.Kind != existing.Kind || len(loaded.Repositories) != 1 || loaded.Repositories[0].ID != existing.Repositories[0].ID {
		t.Fatalf("existing workspace changed: workspace=%+v err=%v", loaded, err)
	}
}

func TestSessionWorkspaceRejectsEmptyMembershipAndMissingSession(t *testing.T) {
	t.Parallel()
	store := openTestStore(t)
	ctx := context.Background()

	w := discovery.Workspace{ID: "propos", Root: "/empty", Kind: "repository"}
	registered, _, err := store.UpsertWorkspaceGeneration(ctx, w)
	if err != nil {
		t.Fatal(err)
	}
	workspaceID := string(registered.ID)
	session := Session{ID: "empty-session", WorkspaceID: workspaceID, SlotID: "empty-slot", State: "ACTIVE", AgentKind: "codex", TokenHash: HashToken("token")}
	if _, err := store.CreateSlotSession(ctx, Slot{ID: session.SlotID, WorkspaceID: workspaceID, State: "LEASED", RootID: testRootID, RelPath: filepath.Join(workspaceID, session.SlotID)}, nil, session, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SessionWorkspace(ctx, session.ID); err == nil || !strings.Contains(err.Error(), "no recorded repository") {
		t.Fatalf("session workspace error=%v", err)
	}
	if _, err := store.SessionWorkspace(ctx, "missing-session"); err == nil {
		t.Fatal("missing session workspace lookup succeeded")
	}
}

func TestSessionWorkspaceRejectsMissingHistoricalMembership(t *testing.T) {
	t.Parallel()
	store := openTestStore(t)
	seedWorkspace(t, store)
	ctx := context.Background()
	session := Session{ID: "without-membership", WorkspaceID: "workspace", SlotID: "without-membership", State: "ACTIVE", AgentKind: "codex", TokenHash: HashToken("token")}
	if _, err := store.CreateSlotSession(ctx, Slot{ID: session.SlotID, WorkspaceID: session.WorkspaceID, Generation: 1, RootID: testRootID, RelPath: filepath.Join(session.WorkspaceID, session.SlotID), State: "LEASED"}, nil, session, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SessionWorkspace(ctx, session.ID); err == nil {
		t.Fatal("session without durable repository membership was accepted")
	}
}

func TestSessionWorkspaceRejectsUnscannableHistoricalMembership(t *testing.T) {
	t.Parallel()
	store := openTestStore(t)
	statements := []string{
		"DROP TABLE sessions",
		"CREATE VIEW sessions AS SELECT 'session' AS id,'workspace' AS workspace_id",
		"DROP TABLE workspaces",
		"CREATE VIEW workspaces AS SELECT 'workspace' AS id,'/workspace' AS root_path,'repository' AS kind",
		"DROP TABLE session_repositories",
		"CREATE VIEW session_repositories AS SELECT 'session' AS session_id,'repository' AS repository_id,'.' AS relative_path,0 AS ordinal",
		"DROP TABLE repositories",
		"CREATE VIEW repositories AS SELECT 'repository' AS id,NULL AS main_worktree_path,'/common' AS common_git_dir,'main' AS default_branch",
	}
	for _, statement := range statements {
		if _, err := store.db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.SessionWorkspace(context.Background(), "session"); err == nil {
		t.Fatal("unscannable historical repository membership was accepted")
	}
}

func TestForgetWorkspaceStopsAtEveryDurableBoundary(t *testing.T) {
	t.Parallel()
	for _, table := range []string{"slots", "sessions", "snapshots", "workspace_snapshots", "jobs"} {
		t.Run("query "+table, func(t *testing.T) {
			store := openTestStore(t)
			seedWorkspace(t, store)
			if _, err := store.db.Exec(`DROP TABLE ` + table); err != nil {
				t.Fatal(err)
			}
			if err := store.ForgetWorkspace(context.Background(), "/workspace"); err == nil {
				t.Fatalf("forget succeeded without %s", table)
			}
		})
	}

	newArchivedWorkspace := func(t *testing.T) *Store {
		t.Helper()
		store := openTestStore(t)
		seedWorkspace(t, store)
		ctx := context.Background()
		session := Session{ID: "archived", WorkspaceID: "workspace", SlotID: "archived", State: "EXPIRED", AgentKind: "codex", TokenHash: HashToken("archived")}
		if _, err := store.CreateSlotSession(ctx, Slot{ID: session.SlotID, WorkspaceID: "workspace", Generation: 1, RootID: testRootID, RelPath: filepath.Join("workspace", session.SlotID), State: "ARCHIVED"}, nil, session, ""); err != nil {
			t.Fatal(err)
		}
		return store
	}
	for _, test := range []struct {
		name    string
		trigger string
	}{
		{name: "slot update", trigger: `CREATE TRIGGER fail_forget_slot BEFORE UPDATE ON slots BEGIN SELECT RAISE(ABORT,'fault'); END`},
		{name: "session update", trigger: `CREATE TRIGGER fail_forget_session BEFORE UPDATE ON sessions BEGIN SELECT RAISE(ABORT,'fault'); END`},
		{name: "job update", trigger: `CREATE TRIGGER fail_forget_job BEFORE UPDATE ON jobs BEGIN SELECT RAISE(ABORT,'fault'); END`},
		{name: "repository membership delete", trigger: `CREATE TRIGGER fail_forget_membership BEFORE DELETE ON workspace_repositories BEGIN SELECT RAISE(ABORT,'fault'); END`},
		{name: "workspace delete", trigger: `CREATE TRIGGER fail_forget_workspace BEFORE DELETE ON workspaces BEGIN SELECT RAISE(ABORT,'fault'); END`},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := newArchivedWorkspace(t)
			if test.name == "job update" {
				if _, err := store.db.Exec(`INSERT INTO jobs(id,kind,state,attempt,not_before) VALUES('finished','PREPARE','SUCCEEDED',0,NULL)`); err != nil {
					t.Fatal(err)
				}
				if _, err := store.db.Exec(`UPDATE jobs SET workspace_id='workspace' WHERE id='finished'`); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := store.db.Exec(test.trigger); err != nil {
				t.Fatal(err)
			}
			if err := store.ForgetWorkspace(context.Background(), "/workspace"); err == nil {
				t.Fatal("forget succeeded despite durable cleanup fault")
			}
		})
	}
}

func TestUpsertWorkspaceGenerationPropagatesMembershipTransactionFaults(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	baseWorkspace := func() discovery.Workspace {
		return discovery.Workspace{ID: "propos", Root: "/workspace", Kind: "multi_repository", Repositories: []discovery.Repository{{ID: "repository", MainPath: "/workspace/repository", CommonDir: "/workspace/repository/.git", RelativePath: "repository", DefaultBranch: "main"}}}
	}
	addedRepository := func() discovery.Workspace {
		w := baseWorkspace()
		w.Repositories = append(w.Repositories, discovery.Repository{ID: "repository-2", MainPath: "/workspace/repository-2", CommonDir: "/workspace/repository-2/.git", RelativePath: "repository-2", DefaultBranch: "main"})
		return w
	}

	t.Run("stale standby update fault", func(t *testing.T) {
		store := openTestStore(t)
		registered, _, err := store.UpsertWorkspaceGeneration(ctx, baseWorkspace())
		if err != nil {
			t.Fatal(err)
		}
		workspaceID := string(registered.ID)
		if _, err := store.CreateStandby(ctx, Slot{ID: "membership-fault-slot", WorkspaceID: workspaceID, Generation: 1, RootID: testRootID, RelPath: filepath.Join(workspaceID, "membership-fault-slot"), State: "READY"}, []SlotRepository{{RepositoryID: "repository", DirName: "repository", State: "READY", RequestedRef: "main", BaseOID: "abc", Fingerprint: "fingerprint"}}); err != nil {
			t.Fatal(err)
		}
		if _, err := store.db.Exec(`CREATE TRIGGER fail_membership_stale BEFORE UPDATE OF state ON slots WHEN OLD.id='membership-fault-slot' BEGIN SELECT RAISE(ABORT,'fault'); END`); err != nil {
			t.Fatal(err)
		}
		if _, _, err := store.UpsertWorkspaceGeneration(ctx, addedRepository()); err == nil {
			t.Fatal("membership change succeeded despite a stale-standby update fault")
		}
		if slot, err := store.Slot(ctx, "membership-fault-slot"); err != nil || slot.State != "READY" {
			t.Fatalf("rolled-back membership change slot=%+v err=%v", slot, err)
		}
	})

	t.Run("membership replacement fault", func(t *testing.T) {
		store := openTestStore(t)
		registered, _, err := store.UpsertWorkspaceGeneration(ctx, baseWorkspace())
		if err != nil {
			t.Fatal(err)
		}
		workspaceID := string(registered.ID)
		if _, err := store.db.Exec(`CREATE TRIGGER fail_membership_delete BEFORE DELETE ON workspace_repositories BEGIN SELECT RAISE(ABORT,'fault'); END`); err != nil {
			t.Fatal(err)
		}
		if _, _, err := store.UpsertWorkspaceGeneration(ctx, addedRepository()); err == nil {
			t.Fatal("membership change succeeded despite a membership replacement fault")
		}
		loaded, err := store.Workspace(ctx, workspaceID)
		if err != nil || len(loaded.Repositories) != 1 {
			t.Fatalf("rolled-back workspace repositories=%d err=%v", len(loaded.Repositories), err)
		}
	})
}
