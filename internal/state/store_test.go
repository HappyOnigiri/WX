package state

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/HappyOnigiri/WX/internal/discovery"
	"github.com/HappyOnigiri/WX/internal/domain"
)

func TestCreateSlotSessionCommitsJobAtomically(t *testing.T) {
	store := openTestStore(t)
	seedWorkspace(t, store)
	ctx := context.Background()
	session := Session{ID: "session", WorkspaceID: "workspace", SlotID: "slot", State: "STARTING", AgentKind: "codex", TokenHash: HashToken("token")}
	job, err := store.CreateSlotSession(ctx, Slot{ID: "slot", WorkspaceID: "workspace", Generation: 1, Path: filepath.Join(t.TempDir(), "slot"), State: "PREPARING"}, []SlotRepository{{RepositoryID: "repository", WorktreePath: filepath.Join(t.TempDir(), "worktree"), State: "PREPARING", RequestedRef: "main", BaseOID: "abc", Fingerprint: "fingerprint"}}, session, "PREPARE")
	if err != nil {
		t.Fatal(err)
	}
	var slotState, sessionState, jobState string
	if err := store.db.QueryRow(`SELECT sl.state,se.state,j.state FROM slots sl JOIN sessions se ON se.slot_id=sl.id JOIN jobs j ON j.slot_id=sl.id WHERE sl.id='slot'`).Scan(&slotState, &sessionState, &jobState); err != nil {
		t.Fatal(err)
	}
	if slotState != "PREPARING" || sessionState != "STARTING" || jobState != "PENDING" || job.Kind != "PREPARE" {
		t.Fatalf("atomic state = slot:%s session:%s job:%s kind:%s", slotState, sessionState, jobState, job.Kind)
	}
}

func TestFailedStandbyBlocksUnboundedPoolRefill(t *testing.T) {
	store := openTestStore(t)
	seedWorkspace(t, store)
	ctx := context.Background()
	root := t.TempDir()
	job, err := store.CreateStandby(ctx, Slot{ID: "blocked", WorkspaceID: "workspace", Generation: 1, Path: filepath.Join(root, "blocked"), State: "PREPARING"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := store.StandbyCount(ctx, "workspace"); got != 1 {
		t.Fatalf("preparing standby count=%d", got)
	}
	if err := store.SetSlotState(ctx, "blocked", []string{"PREPARING"}, "FAILED", "PREPARE_FAILED"); err != nil {
		t.Fatal(err)
	}
	if got := store.StandbyCount(ctx, "workspace"); got != 1 {
		t.Fatalf("failed standby no longer blocks refill: count=%d", got)
	}
	claimed, err := store.ClaimJob(ctx, job.ID, "test")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.FinishJob(ctx, claimed.ID, "test", errors.New("prepare failed")); err != nil {
		t.Fatal(err)
	}
}

func TestReleaseCreatesExactlyOneSnapshotJob(t *testing.T) {
	store := openTestStore(t)
	seedWorkspace(t, store)
	ctx := context.Background()
	session := Session{ID: "session", WorkspaceID: "workspace", SlotID: "slot", State: "ACTIVE", AgentKind: "codex", TokenHash: HashToken("token")}
	if _, err := store.CreateSlotSession(ctx, Slot{ID: "slot", WorkspaceID: "workspace", Generation: 1, Path: filepath.Join(t.TempDir(), "slot"), State: "LEASED"}, nil, session, ""); err != nil {
		t.Fatal(err)
	}
	job, changed, err := store.Release(ctx, session.ID, session.WorkspaceID, session.SlotID)
	if err != nil || !changed || job.Kind != "SNAPSHOT" {
		t.Fatalf("first release: changed=%v job=%+v err=%v", changed, job, err)
	}
	if _, changed, err := store.Release(ctx, session.ID, session.WorkspaceID, session.SlotID); err != nil || changed {
		t.Fatalf("duplicate release: changed=%v err=%v", changed, err)
	}
	var jobs int
	if err := store.db.QueryRow(`SELECT count(*) FROM jobs WHERE session_id='session' AND kind='SNAPSHOT'`).Scan(&jobs); err != nil {
		t.Fatal(err)
	}
	if jobs != 1 {
		t.Fatalf("snapshot jobs=%d, want 1", jobs)
	}
}

func TestSessionRepositoryMembershipSurvivesWorkspaceReconciliation(t *testing.T) {
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
	if _, err := store.UpsertWorkspaceGeneration(ctx, workspace); err != nil {
		t.Fatal(err)
	}
	session := Session{ID: "session", WorkspaceID: "workspace", SlotID: "slot", State: "ACTIVE", AgentKind: "codex", TokenHash: HashToken("token")}
	repositories := []SlotRepository{
		{RepositoryID: "repository-a", WorktreePath: filepath.Join(root, "slot", "a"), State: "READY", RequestedRef: "main", BaseOID: "a", Fingerprint: "a"},
		{RepositoryID: "repository-b", WorktreePath: filepath.Join(root, "slot", "nested", "b"), State: "READY", RequestedRef: "main", BaseOID: "b", Fingerprint: "b"},
	}
	if _, err := store.CreateSlotSession(ctx, Slot{ID: "slot", WorkspaceID: "workspace", Generation: 1, Path: filepath.Join(root, "slot"), State: "LEASED"}, repositories, session, ""); err != nil {
		t.Fatal(err)
	}
	workspace.Repositories = workspace.Repositories[:1]
	if _, err := store.UpsertWorkspaceGeneration(ctx, workspace); err != nil {
		t.Fatal(err)
	}
	historical, err := store.SessionWorkspace(ctx, session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(historical.Repositories) != 2 || historical.Repositories[0].RelativePath != "a" || historical.Repositories[1].RelativePath != "nested/b" {
		t.Fatalf("historical membership=%+v", historical.Repositories)
	}
	current, err := store.Workspace(ctx, "workspace")
	if err != nil || len(current.Repositories) != 1 {
		t.Fatalf("current membership=%+v err=%v", current.Repositories, err)
	}
}

func TestCanonicalWorkspaceKeepsSlotsWhenMainWorktreeMoves(t *testing.T) {
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
	if _, err := store.UpsertWorkspaceGeneration(ctx, registered); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateStandby(ctx, Slot{ID: "standby", WorkspaceID: string(registered.ID), Generation: 1, Path: filepath.Join(t.TempDir(), "standby"), State: "READY"}, nil); err != nil {
		t.Fatal(err)
	}

	discovered := registered
	discovered.ID = "new-workspace-id"
	discovered.Root = domain.CanonicalPath(newMain)
	discovered.Repositories[0].MainPath = domain.CanonicalPath(newMain)
	canonical, err := store.CanonicalWorkspace(ctx, discovered)
	if err != nil {
		t.Fatal(err)
	}
	if canonical.ID != registered.ID || canonical.Root != discovered.Root {
		t.Fatalf("canonical workspace=%+v, want existing ID with moved root", canonical)
	}
	if _, err := store.UpsertWorkspaceGeneration(ctx, discovered); err != nil {
		t.Fatal(err)
	}
	status, err := store.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if status.Workspaces != 1 {
		t.Fatalf("workspace count=%d, want one registered identity", status.Workspaces)
	}
	updated, err := store.Workspace(ctx, string(registered.ID))
	if err != nil {
		t.Fatal(err)
	}
	if updated.Root != discovered.Root || updated.Repositories[0].MainPath != discovered.Repositories[0].MainPath {
		t.Fatalf("updated workspace=%+v", updated)
	}
	if slots, err := store.ReadySlots(ctx, string(registered.ID)); err != nil || len(slots) != 1 || slots[0].ID != "standby" {
		t.Fatalf("existing slots=%+v err=%v", slots, err)
	}
	if slots, err := store.ReadySlots(ctx, string(discovered.ID)); err != nil || len(slots) != 0 {
		t.Fatalf("new identity unexpectedly has slots=%+v err=%v", slots, err)
	}
}

func TestCanonicalWorkspaceRelocationRejectsConflictsWithoutMutation(t *testing.T) {
	ctx := context.Background()

	t.Run("duplicate common directory identity", func(t *testing.T) {
		store := openTestStore(t)
		common := "/repository/common.git"
		registered := discovery.Workspace{ID: "registered", Root: "/registered", Kind: "repository", Repositories: []discovery.Repository{{ID: "repository", MainPath: "/registered", CommonDir: domain.CanonicalPath(common), RelativePath: ".", DefaultBranch: "main"}}}
		if _, err := store.UpsertWorkspaceGeneration(ctx, registered); err != nil {
			t.Fatal(err)
		}
		// This models a pre-existing inconsistent registry. The common directory
		// is unique per repository, but a damaged registry can still attach that
		// repository to multiple workspace rows.
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
		if _, err := store.UpsertWorkspaceGeneration(ctx, candidate); err == nil {
			t.Fatal("upsert proceeded through conflicting common directory")
		}
		status, err := store.Status(ctx)
		if err != nil || status.Workspaces != 2 {
			t.Fatalf("conflict changed registry: status=%+v err=%v", status, err)
		}
		loaded, err := store.Workspace(ctx, string(registered.ID))
		if err != nil || loaded.Root != registered.Root {
			t.Fatalf("registered workspace changed after conflict: workspace=%+v err=%v", loaded, err)
		}
	})

	t.Run("root uniqueness conflict", func(t *testing.T) {
		store := openTestStore(t)
		old := discovery.Workspace{ID: "old", Root: "/old", Kind: "repository", Repositories: []discovery.Repository{{ID: "old-repository", MainPath: "/old", CommonDir: "/old/common.git", RelativePath: ".", DefaultBranch: "main"}}}
		other := discovery.Workspace{ID: "other", Root: "/new", Kind: "repository", Repositories: []discovery.Repository{{ID: "other-repository", MainPath: "/new", CommonDir: "/new/common.git", RelativePath: ".", DefaultBranch: "main"}}}
		if _, err := store.UpsertWorkspaceGeneration(ctx, old); err != nil {
			t.Fatal(err)
		}
		if _, err := store.UpsertWorkspaceGeneration(ctx, other); err != nil {
			t.Fatal(err)
		}
		session := Session{ID: "session", WorkspaceID: string(old.ID), SlotID: "slot", State: "ACTIVE", AgentKind: "codex", TokenHash: HashToken("token")}
		if _, err := store.CreateSlotSession(ctx, Slot{ID: "slot", WorkspaceID: string(old.ID), Generation: 1, Path: "/wx/slot", State: "LEASED"}, nil, session, ""); err != nil {
			t.Fatal(err)
		}
		oldRoot, oldMain := old.Root, old.Repositories[0].MainPath
		candidate := old
		candidate.Repositories = append([]discovery.Repository(nil), old.Repositories...)
		candidate.ID = "new-identity"
		candidate.Root = "/new"
		candidate.Repositories[0].MainPath = "/new"
		if _, err := store.UpsertWorkspaceGeneration(ctx, candidate); err == nil {
			t.Fatal("root uniqueness conflict was accepted")
		}
		loaded, err := store.Workspace(ctx, string(old.ID))
		if err != nil || loaded.Root != oldRoot || loaded.Repositories[0].MainPath != oldMain {
			t.Fatalf("root conflict partially moved workspace: workspace=%+v err=%v", loaded, err)
		}
		storedSession, err := store.SessionByID(ctx, session.ID)
		if err != nil || storedSession.WorkspaceID != string(old.ID) {
			t.Fatalf("root conflict detached session: session=%+v err=%v", storedSession, err)
		}
	})

	t.Run("storage failure", func(t *testing.T) {
		store := openTestStore(t)
		old := discovery.Workspace{ID: "old", Root: "/old", Kind: "repository", Repositories: []discovery.Repository{{ID: "repository", MainPath: "/old", CommonDir: "/old/common.git", RelativePath: ".", DefaultBranch: "main"}}}
		if _, err := store.UpsertWorkspaceGeneration(ctx, old); err != nil {
			t.Fatal(err)
		}
		if _, err := store.db.ExecContext(ctx, `CREATE TRIGGER fail_relocation_repository_update BEFORE UPDATE OF main_worktree_path ON repositories BEGIN SELECT RAISE(ABORT,'fault'); END`); err != nil {
			t.Fatal(err)
		}
		oldRoot, oldMain := old.Root, old.Repositories[0].MainPath
		candidate := old
		candidate.Repositories = append([]discovery.Repository(nil), old.Repositories...)
		candidate.ID = "new-identity"
		candidate.Root = "/new"
		candidate.Repositories[0].MainPath = "/new"
		if _, err := store.UpsertWorkspaceGeneration(ctx, candidate); err == nil {
			t.Fatal("storage failure was ignored")
		}
		loaded, err := store.Workspace(ctx, string(old.ID))
		if err != nil || loaded.Root != oldRoot || loaded.Repositories[0].MainPath != oldMain {
			t.Fatalf("storage failure partially moved workspace: workspace=%+v err=%v", loaded, err)
		}
		if _, err := store.Workspace(ctx, string(candidate.ID)); err == nil {
			t.Fatal("storage failure created a duplicate workspace")
		}
	})
}

func TestResumeBindingsUpdateAllRepositoryLeaseTimestamps(t *testing.T) {
	tests := []struct {
		name string
		bind func(*testing.T, *Store, context.Context, string) error
	}{
		{
			name: "native fresh resume",
			bind: func(t *testing.T, store *Store, ctx context.Context, old string) error {
				parent := Session{ID: "parent", WorkspaceID: "workspace", SlotID: "parent", State: "EXPIRED", AgentKind: "codex", TokenHash: HashToken("parent")}
				if _, err := store.CreateSlotSession(ctx, Slot{ID: "parent", WorkspaceID: "workspace", Generation: 1, Path: "/workspace/parent", State: "SNAPSHOTTED"}, nil, parent, ""); err != nil {
					t.Fatal(err)
				}
				if _, err := store.db.ExecContext(ctx, `UPDATE sessions SET agent_session_id='agent' WHERE id='parent'`); err != nil {
					t.Fatal(err)
				}
				current := Session{ID: "current", SlotID: "current", State: "UNBOUND", AgentKind: "codex", TokenHash: HashToken("current")}
				if _, err := store.CreateSlotSession(ctx, Slot{ID: "current", Path: "/workspace/current", State: "UNBOUND"}, nil, current, ""); err != nil {
					t.Fatal(err)
				}
				if _, err := store.db.ExecContext(ctx, `UPDATE repositories SET last_leased_at=?`, old); err != nil {
					t.Fatal(err)
				}
				_, err := store.BindFreshResumeSlot(ctx, "current", "parent", "workspace", "agent", 1, []SlotRepository{{RepositoryID: "repository", WorktreePath: "/workspace/current/repository", State: "PREPARING", RequestedRef: "main", BaseOID: "head", Fingerprint: "fingerprint"}})
				return err
			},
		},
		{
			name: "snapshot resume",
			bind: func(t *testing.T, store *Store, ctx context.Context, old string) error {
				parentRepos := []SlotRepository{{RepositoryID: "repository", WorktreePath: "/workspace/parent/repository", State: "READY", RequestedRef: "main", BaseOID: "head", Fingerprint: "fingerprint"}}
				parent := Session{ID: "parent", WorkspaceID: "workspace", SlotID: "parent", State: "ARCHIVED", AgentKind: "codex", TokenHash: HashToken("parent")}
				if _, err := store.CreateSlotSession(ctx, Slot{ID: "parent", WorkspaceID: "workspace", Generation: 1, Path: "/workspace/parent", State: "ARCHIVED"}, parentRepos, parent, ""); err != nil {
					t.Fatal(err)
				}
				if _, err := store.db.ExecContext(ctx, `UPDATE sessions SET agent_session_id='agent' WHERE id='parent'`); err != nil {
					t.Fatal(err)
				}
				child := Session{ID: "child", SlotID: "child", State: "UNBOUND", AgentKind: "codex", TokenHash: HashToken("child")}
				if _, err := store.CreateSlotSession(ctx, Slot{ID: "child", Path: "/workspace/child", State: "UNBOUND"}, nil, child, ""); err != nil {
					t.Fatal(err)
				}
				if _, err := store.db.ExecContext(ctx, `UPDATE repositories SET last_leased_at=?`, old); err != nil {
					t.Fatal(err)
				}
				_, err := store.BindResumeSlot(ctx, "child", "parent", "workspace", "agent", 1, nil)
				return err
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := openTestStore(t)
			seedWorkspace(t, store)
			ctx := context.Background()
			secondSeen := now()
			if _, err := store.db.ExecContext(ctx, `INSERT INTO repositories(id,main_worktree_path,common_git_dir,default_branch,first_seen_at,last_seen_at) VALUES(?,?,?,?,?,?)`, "repository-two", "/workspace/two", "/workspace/two.git", "main", secondSeen, secondSeen); err != nil {
				t.Fatal(err)
			}
			if _, err := store.db.ExecContext(ctx, `INSERT INTO workspace_repositories(workspace_id,repository_id,relative_path,ordinal) VALUES(?,?,?,?)`, "workspace", "repository-two", "two", 1); err != nil {
				t.Fatal(err)
			}
			oldTime := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
			old := FormatTime(oldTime)
			if err := test.bind(t, store, ctx, old); err != nil {
				t.Fatal(err)
			}
			for _, repositoryID := range []string{"repository", "repository-two"} {
				var value sql.NullString
				if err := store.db.QueryRowContext(ctx, `SELECT last_leased_at FROM repositories WHERE id=?`, repositoryID).Scan(&value); err != nil {
					t.Fatal(err)
				}
				if !value.Valid {
					t.Fatalf("resume binding did not lease repository %s", repositoryID)
				}
				leasedAt, err := time.Parse(time.RFC3339Nano, value.String)
				if err != nil {
					t.Fatalf("repository %s last_leased_at=%q: %v", repositoryID, value.String, err)
				}
				if !leasedAt.After(oldTime) {
					t.Fatalf("repository %s last_leased_at=%s, want after %s", repositoryID, leasedAt, oldTime)
				}
			}
		})
	}
}

func TestWorkspaceSnapshotMetadataGatesMultiRepositoryArchive(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	root := t.TempDir()
	workspace := discovery.Workspace{ID: "workspace", Root: domain.CanonicalPath(root), Kind: "multi_repository", Repositories: []discovery.Repository{{ID: "repository", MainPath: domain.CanonicalPath(filepath.Join(root, "repository")), CommonDir: domain.CanonicalPath(filepath.Join(root, "repository.git")), RelativePath: "repository", DefaultBranch: "main"}}}
	if _, err := store.UpsertWorkspaceGeneration(ctx, workspace); err != nil {
		t.Fatal(err)
	}
	session := Session{ID: "session", WorkspaceID: "workspace", SlotID: "slot", State: "RELEASING", AgentKind: "codex", TokenHash: HashToken("token")}
	repositories := []SlotRepository{{RepositoryID: "repository", WorktreePath: filepath.Join(root, "slot", "repository"), State: "LEASED", RequestedRef: "main", BaseOID: "base", Fingerprint: "fingerprint"}}
	if _, err := store.CreateSlotSession(ctx, Slot{ID: "slot", WorkspaceID: "workspace", Generation: 1, Path: filepath.Join(root, "slot"), State: "SNAPSHOTTING"}, repositories, session, ""); err != nil {
		t.Fatal(err)
	}
	expires := FormatTime(time.Now().Add(time.Hour))
	if err := store.MarkArchived(ctx, "session", "slot", expires); err == nil {
		t.Fatal("multi-repository session archived without a workspace root snapshot")
	}
	snapshot := WorkspaceSnapshot{SessionID: "session", ArchivePath: filepath.Join(root, "recovery", "snapshot.tar"), SHA256: strings.Repeat("a", 64), Status: "ARCHIVED", CreatedAt: now(), ExpiresAt: expires}
	if err := store.SaveWorkspaceSnapshot(ctx, snapshot); err != nil {
		t.Fatal(err)
	}
	loaded, found, err := store.WorkspaceSnapshot(ctx, "session")
	if err != nil || !found || loaded != snapshot {
		t.Fatalf("workspace snapshot found=%v loaded=%+v err=%v", found, loaded, err)
	}
	conflict := snapshot
	conflict.SHA256 = strings.Repeat("b", 64)
	if err := store.SaveWorkspaceSnapshot(ctx, conflict); err == nil {
		t.Fatal("conflicting workspace snapshot metadata was accepted")
	}
	if err := store.MarkArchived(ctx, "session", "slot", expires); err != nil {
		t.Fatal(err)
	}
	if err := store.ExpireSessionSnapshots(ctx, "session"); err != nil {
		t.Fatal(err)
	}
	if _, found, err := store.WorkspaceSnapshot(ctx, "session"); err != nil || found {
		t.Fatalf("expired workspace snapshot found=%v err=%v", found, err)
	}
}

func TestReleaseDuringPreparationDefersSnapshotUntilPreparationFinishes(t *testing.T) {
	store := openTestStore(t)
	seedWorkspace(t, store)
	ctx := context.Background()
	session := Session{ID: "session", WorkspaceID: "workspace", SlotID: "slot", State: "STARTING", AgentKind: "codex", TokenHash: HashToken("token")}
	if _, err := store.CreateSlotSession(ctx, Slot{ID: "slot", WorkspaceID: "workspace", Generation: 1, Path: filepath.Join(t.TempDir(), "slot"), State: "PREPARING"}, nil, session, "PREPARE"); err != nil {
		t.Fatal(err)
	}
	if job, scheduled, err := store.Release(ctx, session.ID, session.WorkspaceID, session.SlotID); err != nil || scheduled || job.ID != "" {
		t.Fatalf("release during preparation: scheduled=%v job=%+v err=%v", scheduled, job, err)
	}
	storedSession, err := store.SessionByID(ctx, session.ID)
	if err != nil || storedSession.State != "RELEASING" {
		t.Fatalf("session=%+v err=%v", storedSession, err)
	}
	slot, err := store.Slot(ctx, session.SlotID)
	if err != nil || slot.State != "PREPARING" {
		t.Fatalf("slot=%+v err=%v", slot, err)
	}
	var snapshots int
	if err := store.db.QueryRow(`SELECT count(*) FROM jobs WHERE session_id=? AND kind='SNAPSHOT'`, session.ID).Scan(&snapshots); err != nil {
		t.Fatal(err)
	}
	if snapshots != 0 {
		t.Fatalf("snapshot jobs before preparation=%d", snapshots)
	}

	job, scheduled, err := store.FinishPreparationWithRelease(ctx, session.SlotID)
	if err != nil || !scheduled || job.Kind != "SNAPSHOT" || job.SessionID != session.ID || job.WorkspaceID != session.WorkspaceID {
		t.Fatalf("finish preparation: scheduled=%v job=%+v err=%v", scheduled, job, err)
	}
	slot, err = store.Slot(ctx, session.SlotID)
	if err != nil || slot.State != "DRAINING" {
		t.Fatalf("finished slot=%+v err=%v", slot, err)
	}
	if err := store.db.QueryRow(`SELECT count(*) FROM jobs WHERE session_id=? AND kind='SNAPSHOT'`, session.ID).Scan(&snapshots); err != nil {
		t.Fatal(err)
	}
	if snapshots != 1 {
		t.Fatalf("snapshot jobs after preparation=%d", snapshots)
	}
	if _, changed, err := store.Release(ctx, session.ID, session.WorkspaceID, session.SlotID); err != nil || changed {
		t.Fatalf("duplicate release: changed=%v err=%v", changed, err)
	}
}

func TestReleaseCleansUpUnboundAndRestoringSessions(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	for _, sessionState := range []string{"UNBOUND", "RESTORING"} {
		t.Run(sessionState, func(t *testing.T) {
			id := strings.ToLower(sessionState)
			session := Session{ID: id, SlotID: id, State: sessionState, AgentKind: "codex", TokenHash: HashToken("token")}
			if _, err := store.CreateSlotSession(ctx, Slot{ID: id, Path: filepath.Join(t.TempDir(), id), State: sessionState}, nil, session, ""); err != nil {
				t.Fatal(err)
			}
			job, changed, err := store.Release(ctx, id, "", id)
			if err != nil || !changed || job.Kind != "REMOVE" || job.SessionID != "" {
				t.Fatalf("release job=%+v changed=%v err=%v", job, changed, err)
			}
			storedSession, err := store.SessionByID(ctx, id)
			if err != nil || storedSession.State != "EXPIRED" {
				t.Fatalf("session=%+v err=%v", storedSession, err)
			}
			slot, err := store.Slot(ctx, id)
			if err != nil || slot.State != "REMOVING" || slot.OwnerSessionID != "" {
				t.Fatalf("slot=%+v err=%v", slot, err)
			}
			if _, changed, err := store.Release(ctx, id, "", id); err != nil || changed {
				t.Fatalf("duplicate release changed=%v err=%v", changed, err)
			}
		})
	}
}

func TestUnboundReleaseRollsBackWhenSlotStateChanged(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	session := Session{ID: "unbound", SlotID: "unbound", State: "UNBOUND", AgentKind: "codex", TokenHash: HashToken("token")}
	if _, err := store.CreateSlotSession(ctx, Slot{ID: "unbound", Path: filepath.Join(t.TempDir(), "unbound"), State: "UNBOUND"}, nil, session, ""); err != nil {
		t.Fatal(err)
	}
	if err := store.SetSlotState(ctx, "unbound", []string{"UNBOUND"}, "QUARANTINED", "TEST"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Release(ctx, "unbound", "", "unbound"); err == nil {
		t.Fatal("release succeeded after slot ownership state changed")
	}
	storedSession, err := store.SessionByID(ctx, "unbound")
	if err != nil || storedSession.State != "UNBOUND" {
		t.Fatalf("release transaction did not roll back: session=%+v err=%v", storedSession, err)
	}
}

func TestStateMachineRejectsStaleAndIncompleteTransitions(t *testing.T) {
	store := openTestStore(t)
	seedWorkspace(t, store)
	ctx := context.Background()
	root := t.TempDir()

	standby, err := store.CreateStandby(ctx, Slot{ID: "ready", WorkspaceID: "workspace", Generation: 1, Path: filepath.Join(root, "ready"), State: "PREPARING"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetSlotState(ctx, "ready", []string{"PREPARING"}, "READY", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := store.LeaseReadyWithCold(ctx, "missing", Session{ID: "missing", WorkspaceID: "workspace", SlotID: "missing", AgentKind: "codex", TokenHash: HashToken("token")}); err == nil {
		t.Fatal("cold lease accepted a missing slot")
	}
	if _, err := store.LeaseReadyWithCold(ctx, "ready", Session{ID: "ready", WorkspaceID: "workspace", SlotID: "ready", AgentKind: "codex", TokenHash: HashToken("token")}); err == nil {
		t.Fatal("cold lease accepted a slot without COLD repositories")
	}
	claimed, err := store.ClaimJob(ctx, standby.ID, "test")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.FinishJob(ctx, claimed.ID, "test", nil); err != nil {
		t.Fatal(err)
	}

	active := Session{ID: "active", WorkspaceID: "workspace", SlotID: "active", State: "ACTIVE", AgentKind: "codex", TokenHash: HashToken("token")}
	if _, err := store.CreateSlotSession(ctx, Slot{ID: "active", WorkspaceID: "workspace", Generation: 1, Path: filepath.Join(root, "active"), State: "LEASED"}, nil, active, ""); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkSessionState(ctx, "active", []string{"ACTIVE"}, "EXPIRED"); err != nil {
		t.Fatal(err)
	}
	if err := store.Heartbeat(ctx, "active", "token"); err == nil {
		t.Fatal("expired session heartbeat succeeded")
	}
	if sessions, err := store.ListSessions(ctx, false); err != nil || len(sessions) != 0 {
		t.Fatalf("non-expired session list=%+v err=%v", sessions, err)
	}

	parent := Session{ID: "parent-unmapped", WorkspaceID: "workspace", SlotID: "parent-unmapped", State: "ARCHIVED", AgentKind: "codex", TokenHash: HashToken("parent")}
	if _, err := store.CreateSlotSession(ctx, Slot{ID: "parent-unmapped", WorkspaceID: "workspace", Generation: 1, Path: filepath.Join(root, "parent-unmapped"), State: "SNAPSHOTTED"}, nil, parent, ""); err != nil {
		t.Fatal(err)
	}
	child := Session{ID: "child-unmapped", SlotID: "child-unmapped", State: "UNBOUND", AgentKind: "codex", TokenHash: HashToken("child")}
	if _, err := store.CreateSlotSession(ctx, Slot{ID: "child-unmapped", Path: filepath.Join(root, "child-unmapped"), State: "UNBOUND"}, nil, child, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := store.BindResumeSlot(ctx, "child-unmapped", "parent-unmapped", "workspace", "agent", 1, nil); err == nil || !strings.Contains(err.Error(), "mapping changed") {
		t.Fatalf("unmapped parent resume error=%v", err)
	}

	if err := store.MarkArchived(ctx, "active", "active", now()); err == nil {
		t.Fatal("invalid session archive transition succeeded")
	}
	if err := store.BeginSnapshot(ctx, "active", "active"); err == nil {
		t.Fatal("invalid snapshot transition succeeded")
	}
	if err := store.SetSlotRepositoryState(ctx, "ready", "missing", []string{"READY"}, "COLD"); err == nil {
		t.Fatal("missing slot repository transition succeeded")
	}
}

func TestRecoveryRefQueriesAndPendingMappingFailure(t *testing.T) {
	store := openTestStore(t)
	seedWorkspace(t, store)
	ctx := context.Background()
	root := t.TempDir()
	parent := Session{ID: "parent", WorkspaceID: "workspace", SlotID: "parent", State: "ARCHIVED", AgentKind: "codex", TokenHash: HashToken("parent")}
	if _, err := store.CreateSlotSession(ctx, Slot{ID: "parent", WorkspaceID: "workspace", Generation: 1, Path: filepath.Join(root, "parent"), State: "SNAPSHOTTED"}, nil, parent, ""); err != nil {
		t.Fatal(err)
	}
	if err := store.BindAgentSession(ctx, "parent", "agent"); err != nil {
		t.Fatal(err)
	}
	child := Session{ID: "child", SlotID: "child", State: "UNBOUND", AgentKind: "codex", TokenHash: HashToken("child")}
	if _, err := store.CreateSlotSession(ctx, Slot{ID: "child", Path: filepath.Join(root, "child"), State: "UNBOUND"}, nil, child, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := store.BindResumeSlot(ctx, "child", "parent", "workspace", "agent", 1, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`UPDATE sessions SET agent_session_id=NULL WHERE id='parent'`); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.FinishPreparationWithRelease(ctx, "child"); err == nil || !strings.Contains(err.Error(), "mapping changed") {
		t.Fatalf("changed parent mapping error=%v", err)
	}

	snapshot := Snapshot{ID: "snapshot", SessionID: "parent", RepositoryID: "repository", HeadOID: "head", HeadRef: "refs/wx/recovery/head", IndexTreeOID: "index", WorktreeOID: "worktree", WorktreeRef: "refs/wx/recovery/worktree", Status: "ARCHIVED", CreatedAt: now(), ExpiresAt: now()}
	if err := store.SaveSnapshot(ctx, snapshot); err != nil {
		t.Fatal(err)
	}
	expectations, err := store.RecoveryRefExpectations(ctx, "repository")
	if err != nil || len(expectations) != 2 {
		t.Fatalf("recovery ref expectations=%+v err=%v", expectations, err)
	}
	if expectations[0].OID != "head" || expectations[1].OID != "worktree" || expectations[0].InFlight || expectations[1].InFlight {
		t.Fatalf("archived recovery ref expectations=%+v", expectations)
	}
}

func TestRecoveryRefExpectationsMarkActiveSnapshotJobInFlight(t *testing.T) {
	store := openTestStore(t)
	seedWorkspace(t, store)
	ctx := context.Background()
	root := t.TempDir()
	session := Session{ID: "snapshotting", WorkspaceID: "workspace", SlotID: "snapshotting", State: "SNAPSHOTTING", AgentKind: "codex", TokenHash: HashToken("snapshotting")}
	job, err := store.CreateSlotSession(ctx, Slot{ID: session.SlotID, WorkspaceID: session.WorkspaceID, Generation: 1, Path: filepath.Join(root, session.SlotID), State: "SNAPSHOTTING"}, nil, session, "SNAPSHOT")
	if err != nil {
		t.Fatal(err)
	}
	snapshot := Snapshot{ID: "snapshotting-snapshot", SessionID: session.ID, RepositoryID: "repository", HeadOID: "head", HeadRef: "refs/wx/recovery/snapshotting/head", IndexTreeOID: "index", WorktreeOID: "worktree", WorktreeRef: "refs/wx/recovery/snapshotting/worktree", Status: "ARCHIVED", CreatedAt: now(), ExpiresAt: FormatTime(time.Now().Add(time.Hour))}
	if err := store.SaveSnapshot(ctx, snapshot); err != nil {
		t.Fatal(err)
	}
	expectations, err := store.RecoveryRefExpectations(ctx, "repository")
	if err != nil || len(expectations) != 2 {
		t.Fatalf("in-flight expectations=%+v err=%v", expectations, err)
	}
	for _, expectation := range expectations {
		if !expectation.InFlight || expectation.SessionID != session.ID || expectation.SessionState != session.State {
			t.Fatalf("in-flight expectation=%+v", expectation)
		}
	}
	claimed, err := store.ClaimJob(ctx, job.ID, "test")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`UPDATE jobs SET lease_expires_at=? WHERE id=?`, FormatTime(time.Now().Add(-time.Minute)), claimed.ID); err != nil {
		t.Fatal(err)
	}
	expectations, err = store.RecoveryRefExpectations(ctx, "repository")
	if err != nil || len(expectations) != 2 {
		t.Fatalf("expired-lease expectations=%+v err=%v", expectations, err)
	}
	for _, expectation := range expectations {
		if expectation.InFlight {
			t.Fatalf("expired snapshot job remained in-flight: %+v", expectation)
		}
	}
	if err := store.FinishJob(ctx, claimed.ID, "test", errors.New("simulated archive failure")); err != nil {
		t.Fatal(err)
	}
	expectations, err = store.RecoveryRefExpectations(ctx, "repository")
	if err != nil || len(expectations) != 2 {
		t.Fatalf("failed-job expectations=%+v err=%v", expectations, err)
	}
	for _, expectation := range expectations {
		if expectation.InFlight {
			t.Fatalf("failed snapshot job remained in-flight: %+v", expectation)
		}
	}
}

func TestActiveRestoreProtectsSnapshotAndParentBindingCanTransfer(t *testing.T) {
	store := openTestStore(t)
	seedWorkspace(t, store)
	ctx := context.Background()
	root := t.TempDir()
	parent := Session{ID: "parent", WorkspaceID: "workspace", SlotID: "parent", State: "ARCHIVED", AgentKind: "codex", TokenHash: HashToken("parent")}
	if _, err := store.CreateSlotSession(ctx, Slot{ID: "parent", WorkspaceID: "workspace", Generation: 1, Path: filepath.Join(root, "parent"), State: "SNAPSHOTTED"}, nil, parent, ""); err != nil {
		t.Fatal(err)
	}
	if err := store.BindAgentSession(ctx, "parent", "agent"); err != nil {
		t.Fatal(err)
	}
	snapshot := Snapshot{ID: "snapshot", SessionID: "parent", RepositoryID: "repository", HeadOID: "head", HeadRef: "refs/wx/recovery/head", IndexTreeOID: "index", WorktreeOID: "worktree", WorktreeRef: "refs/wx/recovery/worktree", Status: "ARCHIVED", CreatedAt: now(), ExpiresAt: now()}
	if err := store.SaveSnapshot(ctx, snapshot); err != nil {
		t.Fatal(err)
	}
	resume := Session{ID: "resume", SlotID: "resume", State: "UNBOUND", AgentKind: "codex", TokenHash: HashToken("resume")}
	if _, err := store.CreateSlotSession(ctx, Slot{ID: "resume", Path: filepath.Join(root, "resume"), State: "UNBOUND"}, nil, resume, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := store.BindResumeSlot(ctx, "resume", "parent", "workspace", "agent", 1, nil); err != nil {
		t.Fatal(err)
	}
	if err := store.ExpireSessionSnapshots(ctx, "parent"); err == nil || !strings.Contains(err.Error(), "active restore") {
		t.Fatalf("active restore snapshot expiration error=%v", err)
	}

	transfer := Session{ID: "transfer", WorkspaceID: "workspace", SlotID: "transfer", ParentSessionID: "parent", State: "STARTING", AgentKind: "codex", TokenHash: HashToken("transfer")}
	if _, err := store.CreateSlotSession(ctx, Slot{ID: "transfer", WorkspaceID: "workspace", Generation: 1, Path: filepath.Join(root, "transfer"), State: "LEASED"}, nil, transfer, ""); err != nil {
		t.Fatal(err)
	}
	if err := store.BindAgentSession(ctx, "transfer", "agent"); err != nil {
		t.Fatal(err)
	}
	mapped, err := store.FindByAgentSession(ctx, "codex", "agent")
	if err != nil || mapped.ID != "transfer" {
		t.Fatalf("transferred mapping=%+v err=%v", mapped, err)
	}
}

func TestColdRemovalSchedulingIsIdempotent(t *testing.T) {
	store := openTestStore(t)
	seedWorkspace(t, store)
	ctx := context.Background()
	root := t.TempDir()
	repository := SlotRepository{RepositoryID: "repository", WorktreePath: filepath.Join(root, "ready", "repo"), State: "READY", RequestedRef: "main", BaseOID: "head", Fingerprint: "fp"}
	job, err := store.CreateStandby(ctx, Slot{ID: "ready", WorkspaceID: "workspace", Generation: 1, Path: filepath.Join(root, "ready"), State: "PREPARING"}, []SlotRepository{repository})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetSlotState(ctx, "ready", []string{"PREPARING"}, "READY", ""); err != nil {
		t.Fatal(err)
	}
	claimed, err := store.ClaimJob(ctx, job.ID, "test")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.FinishJob(ctx, claimed.ID, "test", nil); err != nil {
		t.Fatal(err)
	}
	candidate := ColdRepositoryCandidate{SlotID: "ready", WorkspaceID: "workspace", RepositoryID: "repository", WorktreePath: repository.WorktreePath}
	if _, changed, err := store.ScheduleColdRepositoryRemoval(ctx, candidate); err != nil || !changed {
		t.Fatalf("first cold scheduling changed=%v err=%v", changed, err)
	}
	if _, changed, err := store.ScheduleColdRepositoryRemoval(ctx, candidate); err != nil || changed {
		t.Fatalf("duplicate cold scheduling changed=%v err=%v", changed, err)
	}
}

func TestTransactionalTransitionsRollBackOnCompanionStateChanges(t *testing.T) {
	store := openTestStore(t)
	seedWorkspace(t, store)
	ctx := context.Background()
	root := t.TempDir()

	for _, operation := range []string{"archive", "snapshot"} {
		t.Run(operation, func(t *testing.T) {
			id := operation
			session := Session{ID: id, WorkspaceID: "workspace", SlotID: id, State: "ACTIVE", AgentKind: "codex", TokenHash: HashToken("token")}
			if _, err := store.CreateSlotSession(ctx, Slot{ID: id, WorkspaceID: "workspace", Generation: 1, Path: filepath.Join(root, id), State: "LEASED"}, nil, session, ""); err != nil {
				t.Fatal(err)
			}
			if _, changed, err := store.Release(ctx, id, "workspace", id); err != nil || !changed {
				t.Fatalf("release changed=%v err=%v", changed, err)
			}
			if err := store.SetSlotState(ctx, id, []string{"DRAINING"}, "FAILED", "TEST"); err != nil {
				t.Fatal(err)
			}
			if operation == "archive" {
				if err := store.MarkArchived(ctx, id, id, now()); err == nil {
					t.Fatal("archive succeeded with a failed slot")
				}
			} else if err := store.BeginSnapshot(ctx, id, id); err == nil {
				t.Fatal("snapshot succeeded with a failed slot")
			}
			stored, err := store.SessionByID(ctx, id)
			if err != nil || stored.State != "RELEASING" {
				t.Fatalf("transaction did not roll back: session=%+v err=%v", stored, err)
			}
		})
	}

	parent := Session{ID: "parent", WorkspaceID: "workspace", SlotID: "parent", State: "ARCHIVED", AgentKind: "codex", TokenHash: HashToken("parent")}
	if _, err := store.CreateSlotSession(ctx, Slot{ID: "parent", WorkspaceID: "workspace", Generation: 1, Path: filepath.Join(root, "parent"), State: "SNAPSHOTTED"}, nil, parent, ""); err != nil {
		t.Fatal(err)
	}
	if err := store.BindAgentSession(ctx, "parent", "agent"); err != nil {
		t.Fatal(err)
	}
	child := Session{ID: "child", SlotID: "child", State: "UNBOUND", AgentKind: "codex", TokenHash: HashToken("child")}
	if _, err := store.CreateSlotSession(ctx, Slot{ID: "child", Path: filepath.Join(root, "child"), State: "UNBOUND"}, nil, child, ""); err != nil {
		t.Fatal(err)
	}
	if err := store.SetSlotState(ctx, "child", []string{"UNBOUND"}, "QUARANTINED", "TEST"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.BindResumeSlot(ctx, "child", "parent", "workspace", "agent", 1, nil); err == nil || !strings.Contains(err.Error(), "slot is no longer UNBOUND") {
		t.Fatalf("changed resume slot error=%v", err)
	}
	storedChild, err := store.SessionByID(ctx, "child")
	if err != nil || storedChild.State != "UNBOUND" {
		t.Fatalf("resume transaction did not roll back: child=%+v err=%v", storedChild, err)
	}
	if err := store.ExpireSessionSnapshots(ctx, "missing"); err == nil {
		t.Fatal("missing session snapshot expiration succeeded")
	}
}

func TestStoreFilesystemCreationFailures(t *testing.T) {
	root := t.TempDir()
	blocker := filepath.Join(root, "file")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(filepath.Join(blocker, "state.db")); err == nil {
		t.Fatal("database opened through a regular-file parent")
	}
	store := openTestStore(t)
	original := store.path
	store.path = filepath.Join(blocker, "state.db")
	if _, err := store.Backup(context.Background(), 1, time.Hour); err == nil {
		t.Fatal("backup created through a regular-file parent")
	}
	store.path = original
}

func TestCompanionTableFailuresRollBackStateTransactions(t *testing.T) {
	t.Run("create session repository metadata", func(t *testing.T) {
		store := openTestStore(t)
		seedWorkspace(t, store)
		if _, err := store.db.Exec(`DROP TABLE slot_repositories`); err != nil {
			t.Fatal(err)
		}
		session := Session{ID: "slot", WorkspaceID: "workspace", SlotID: "slot", State: "STARTING", AgentKind: "codex", TokenHash: HashToken("token")}
		_, err := store.CreateSlotSession(context.Background(), Slot{ID: "slot", WorkspaceID: "workspace", Generation: 1, Path: "/wx/slot", State: "PREPARING"}, []SlotRepository{{RepositoryID: "repository", WorktreePath: "/wx/slot/repo"}}, session, "")
		if err == nil {
			t.Fatal("slot creation succeeded without repository metadata table")
		}
		var slots int
		if err := store.db.QueryRow(`SELECT count(*) FROM slots WHERE id='slot'`).Scan(&slots); err != nil || slots != 0 {
			t.Fatalf("slot transaction was not rolled back: slots=%d err=%v", slots, err)
		}
	})

	t.Run("create session row", func(t *testing.T) {
		store := openTestStore(t)
		seedWorkspace(t, store)
		if _, err := store.db.Exec(`DROP TABLE sessions`); err != nil {
			t.Fatal(err)
		}
		session := Session{ID: "slot", WorkspaceID: "workspace", SlotID: "slot", State: "STARTING", AgentKind: "codex", TokenHash: HashToken("token")}
		if _, err := store.CreateSlotSession(context.Background(), Slot{ID: "slot", WorkspaceID: "workspace", Generation: 1, Path: "/wx/slot", State: "PREPARING"}, nil, session, ""); err == nil {
			t.Fatal("slot creation succeeded without sessions table")
		}
	})

	t.Run("standby durable job", func(t *testing.T) {
		store := openTestStore(t)
		seedWorkspace(t, store)
		if _, err := store.db.Exec(`DROP TABLE jobs`); err != nil {
			t.Fatal(err)
		}
		if _, err := store.CreateStandby(context.Background(), Slot{ID: "slot", WorkspaceID: "workspace", Generation: 1, Path: "/wx/slot", State: "PREPARING"}, nil); err == nil {
			t.Fatal("standby creation succeeded without jobs table")
		}
	})

	t.Run("session durable job", func(t *testing.T) {
		store := openTestStore(t)
		seedWorkspace(t, store)
		if _, err := store.db.Exec(`DROP TABLE jobs`); err != nil {
			t.Fatal(err)
		}
		session := Session{ID: "slot", WorkspaceID: "workspace", SlotID: "slot", State: "STARTING", AgentKind: "codex", TokenHash: HashToken("token")}
		if _, err := store.CreateSlotSession(context.Background(), Slot{ID: "slot", WorkspaceID: "workspace", Generation: 1, Path: "/wx/slot", State: "PREPARING"}, nil, session, "PREPARE"); err == nil {
			t.Fatal("slot creation succeeded without durable jobs table")
		}
	})

	t.Run("standby repository metadata", func(t *testing.T) {
		store := openTestStore(t)
		seedWorkspace(t, store)
		if _, err := store.db.Exec(`DROP TABLE slot_repositories`); err != nil {
			t.Fatal(err)
		}
		repository := SlotRepository{RepositoryID: "repository", WorktreePath: "/wx/slot/repository", State: "PREPARING"}
		if _, err := store.CreateStandby(context.Background(), Slot{ID: "slot", WorkspaceID: "workspace", Generation: 1, Path: "/wx/slot", State: "PREPARING"}, []SlotRepository{repository}); err == nil {
			t.Fatal("standby creation succeeded without repository metadata table")
		}
	})

	t.Run("lease session row", func(t *testing.T) {
		store := openTestStore(t)
		seedWorkspace(t, store)
		ctx := context.Background()
		job, err := store.CreateStandby(ctx, Slot{ID: "slot", WorkspaceID: "workspace", Generation: 1, Path: "/wx/slot", State: "PREPARING"}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.SetSlotState(ctx, "slot", []string{"PREPARING"}, "READY", ""); err != nil {
			t.Fatal(err)
		}
		claimed, err := store.ClaimJob(ctx, job.ID, "test")
		if err != nil {
			t.Fatal(err)
		}
		if err := store.FinishJob(ctx, claimed.ID, "test", nil); err != nil {
			t.Fatal(err)
		}
		if _, err := store.db.Exec(`DROP TABLE sessions`); err != nil {
			t.Fatal(err)
		}
		session := Session{ID: "slot", WorkspaceID: "workspace", SlotID: "slot", AgentKind: "codex", TokenHash: HashToken("token")}
		if err := store.LeaseReady(ctx, "slot", session); err == nil {
			t.Fatal("lease succeeded without sessions table")
		}
		slot, err := store.Slot(ctx, "slot")
		if err != nil || slot.State != "READY" {
			t.Fatalf("lease transaction did not roll back: slot=%+v err=%v", slot, err)
		}
	})

	t.Run("lease repository timestamp", func(t *testing.T) {
		store := openTestStore(t)
		seedWorkspace(t, store)
		ctx := context.Background()
		job, err := store.CreateStandby(ctx, Slot{ID: "slot", WorkspaceID: "workspace", Generation: 1, Path: "/wx/slot", State: "PREPARING"}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.SetSlotState(ctx, "slot", []string{"PREPARING"}, "READY", ""); err != nil {
			t.Fatal(err)
		}
		claimed, _ := store.ClaimJob(ctx, job.ID, "test")
		if err := store.FinishJob(ctx, claimed.ID, "test", nil); err != nil {
			t.Fatal(err)
		}
		if _, err := store.db.Exec(`CREATE TRIGGER fail_repository_update BEFORE UPDATE ON repositories BEGIN SELECT RAISE(FAIL, 'injected repository update failure'); END`); err != nil {
			t.Fatal(err)
		}
		session := Session{ID: "slot", WorkspaceID: "workspace", SlotID: "slot", AgentKind: "codex", TokenHash: HashToken("token")}
		if err := store.LeaseReady(ctx, "slot", session); err == nil {
			t.Fatal("lease succeeded despite repository timestamp failure")
		}
	})

	t.Run("release durable job", func(t *testing.T) {
		store := openTestStore(t)
		seedWorkspace(t, store)
		ctx := context.Background()
		session := Session{ID: "slot", WorkspaceID: "workspace", SlotID: "slot", State: "ACTIVE", AgentKind: "codex", TokenHash: HashToken("token")}
		if _, err := store.CreateSlotSession(ctx, Slot{ID: "slot", WorkspaceID: "workspace", Generation: 1, Path: "/wx/slot", State: "LEASED"}, nil, session, ""); err != nil {
			t.Fatal(err)
		}
		if _, err := store.db.Exec(`DROP TABLE jobs`); err != nil {
			t.Fatal(err)
		}
		if _, _, err := store.Release(ctx, "slot", "workspace", "slot"); err == nil {
			t.Fatal("release succeeded without durable jobs table")
		}
		stored, err := store.SessionByID(ctx, "slot")
		if err != nil || stored.State != "ACTIVE" {
			t.Fatalf("release transaction did not roll back: session=%+v err=%v", stored, err)
		}
	})

	for _, operation := range []string{"finish-preparation", "archive", "begin-snapshot"} {
		t.Run(operation, func(t *testing.T) {
			store := openTestStore(t)
			seedWorkspace(t, store)
			ctx := context.Background()
			sessionState, slotState := "STARTING", "PREPARING"
			if operation != "finish-preparation" {
				sessionState, slotState = "RELEASING", "DRAINING"
			}
			session := Session{ID: "slot", WorkspaceID: "workspace", SlotID: "slot", State: sessionState, AgentKind: "codex", TokenHash: HashToken("token")}
			if _, err := store.CreateSlotSession(ctx, Slot{ID: "slot", WorkspaceID: "workspace", Generation: 1, Path: "/wx/slot", State: slotState}, nil, session, ""); err != nil {
				t.Fatal(err)
			}
			if operation == "finish-preparation" {
				if _, err := store.db.Exec(`DROP TABLE sessions`); err != nil {
					t.Fatal(err)
				}
				if _, _, err := store.FinishPreparationWithRelease(ctx, "slot"); err == nil {
					t.Fatal("preparation finish succeeded without sessions table")
				}
				return
			}
			if _, err := store.db.Exec(`CREATE TRIGGER fail_slot_update BEFORE UPDATE ON slots BEGIN SELECT RAISE(FAIL, 'injected slot update failure'); END`); err != nil {
				t.Fatal(err)
			}
			var err error
			if operation == "archive" {
				err = store.MarkArchived(ctx, "slot", "slot", now())
			} else {
				err = store.BeginSnapshot(ctx, "slot", "slot")
			}
			if err == nil {
				t.Fatalf("%s succeeded despite an injected slot update failure", operation)
			}
		})
	}

	t.Run("metadata pruning", func(t *testing.T) {
		store := openTestStore(t)
		if _, err := store.db.Exec(`DROP TABLE events`); err != nil {
			t.Fatal(err)
		}
		if err := store.PruneMetadata(context.Background(), now(), now(), now()); err == nil {
			t.Fatal("metadata pruning succeeded without events table")
		}
	})
}

func TestRecoverJobsReclaimsOnlyExpiredLease(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	job, err := store.CreateJob(ctx, "ENSURE_STANDBY", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ClaimJob(ctx, job.ID, "owner"); err != nil {
		t.Fatal(err)
	}
	jobs, err := store.RecoverJobs(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 0 {
		t.Fatalf("live lease was reclaimed: %+v", jobs)
	}
	if _, err := store.db.Exec(`UPDATE jobs SET lease_expires_at=? WHERE id=?`, time.Now().Add(-time.Minute).UTC().Format(time.RFC3339Nano), job.ID); err != nil {
		t.Fatal(err)
	}
	jobs, err = store.RecoverJobs(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 || jobs[0].ID != job.ID || jobs[0].State != "PENDING" {
		t.Fatalf("expired lease recovery=%+v", jobs)
	}
}

func TestEnsureRecoveryJobsReconstructsInterruptedSlotAndRepositoryWork(t *testing.T) {
	store := openTestStore(t)
	seedWorkspace(t, store)
	ctx := context.Background()
	root := t.TempDir()
	for _, slot := range []struct {
		id, state, sessionState string
	}{
		{id: "prepare", state: "PREPARING", sessionState: "STARTING"},
		{id: "snapshot", state: "SNAPSHOTTING", sessionState: "SNAPSHOTTING"},
		{id: "remove", state: "REMOVING", sessionState: "ARCHIVED"},
	} {
		if _, err := store.db.Exec(`INSERT INTO slots(id,workspace_id,generation,path,state,created_at,updated_at) VALUES(?,?,?,?,?,?,?)`, slot.id, "workspace", 1, filepath.Join(root, slot.id), slot.state, now(), now()); err != nil {
			t.Fatal(err)
		}
		if slot.sessionState != "" {
			if _, err := store.db.Exec(`INSERT INTO sessions(id,workspace_id,slot_id,state,agent_kind,session_token_hash,created_at) VALUES(?,?,?,?,?,?,?)`, slot.id, "workspace", slot.id, slot.sessionState, "codex", HashToken(slot.id), now()); err != nil {
				t.Fatal(err)
			}
		}
	}
	if _, err := store.db.Exec(`INSERT INTO slots(id,workspace_id,generation,path,state,created_at,updated_at) VALUES('retire','workspace',1,?,'RETIRING',?,?)`, filepath.Join(root, "retire"), now(), now()); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`INSERT INTO slot_repositories(slot_id,repository_id,worktree_path,state,requested_ref,base_oid,prepare_fingerprint) VALUES('retire','repository',?,'RETIRING','main','abc','fp')`, filepath.Join(root, "retire", "repo")); err != nil {
		t.Fatal(err)
	}

	jobs, err := store.EnsureRecoveryJobs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"prepare": "PREPARE", "snapshot": "SNAPSHOT", "remove": "REMOVE", "retire": "REMOVE_REPOSITORY"}
	if len(jobs) != len(want) {
		t.Fatalf("recovery jobs=%+v", jobs)
	}
	for _, job := range jobs {
		if want[job.SlotID] != job.Kind {
			t.Fatalf("unexpected recovery job=%+v", job)
		}
		if job.Kind == "REMOVE" && job.SessionID != "remove" {
			t.Fatalf("remove recovery job lost archived session identity: %+v", job)
		}
		if job.Kind == "REMOVE_REPOSITORY" && job.RepositoryID != "repository" {
			t.Fatalf("repository recovery job lost repository identity: %+v", job)
		}
	}
	if duplicate, err := store.EnsureRecoveryJobs(ctx); err != nil || len(duplicate) != 0 {
		t.Fatalf("duplicate recovery jobs=%+v err=%v", duplicate, err)
	}
}

func TestAgentProcessRegistrationAndDependencyDeferral(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	if _, err := store.CreateSlotSession(ctx, Slot{ID: "agent", Path: filepath.Join(t.TempDir(), "agent"), State: "LEASED"}, nil, Session{ID: "agent", SlotID: "agent", State: "ACTIVE", AgentKind: "codex", TokenHash: HashToken("token")}, ""); err != nil {
		t.Fatal(err)
	}
	if err := store.RegisterAgentProcess(ctx, "agent", "token", 0); err == nil {
		t.Fatal("agent process registration accepted a non-positive PID")
	}
	if err := store.RegisterAgentProcess(ctx, "agent", "wrong", os.Getpid()); err == nil {
		t.Fatal("agent process registration accepted an invalid token")
	}
	if err := store.RegisterAgentProcess(ctx, "agent", "token", os.Getpid()); err != nil {
		t.Fatal(err)
	}
	session, err := store.SessionByID(ctx, "agent")
	if err != nil || session.AgentPID != os.Getpid() {
		t.Fatalf("registered agent process=%d err=%v", session.AgentPID, err)
	}
	if _, err := store.db.ExecContext(ctx, `CREATE TRIGGER fail_agent_pid BEFORE UPDATE OF agent_pid ON sessions BEGIN SELECT RAISE(ABORT,'fault'); END`); err != nil {
		t.Fatal(err)
	}
	if err := store.RegisterAgentProcess(ctx, "agent", "token", os.Getpid()); err == nil {
		t.Fatal("agent process registration ignored a storage failure")
	}
	if _, err := store.db.ExecContext(ctx, `DROP TRIGGER fail_agent_pid`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE sessions SET state='ARCHIVED' WHERE id='agent'`); err != nil {
		t.Fatal(err)
	}
	if err := store.RegisterAgentProcess(ctx, "agent", "token", os.Getpid()); err == nil {
		t.Fatal("agent process registration accepted an archived session")
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE sessions SET state='ACTIVE' WHERE id='agent'`); err != nil {
		t.Fatal(err)
	}

	job, err := store.CreateJob(ctx, "RESTORE", "", "agent", "agent")
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := store.ClaimJob(ctx, job.ID, "worker")
	if err != nil || claimed.Attempt != 1 {
		t.Fatalf("claimed job=%+v err=%v", claimed, err)
	}
	if err := store.DeferJob(ctx, job.ID, "wrong-worker", 0, "SNAPSHOT_PENDING"); err == nil {
		t.Fatal("dependency deferral accepted the wrong lease owner")
	}
	if err := store.DeferJob(ctx, job.ID, "worker", 0, "SNAPSHOT_PENDING"); err != nil {
		t.Fatal(err)
	}
	claimed, err = store.ClaimJob(ctx, job.ID, "worker")
	if err != nil || claimed.Attempt != 1 {
		t.Fatalf("dependency wait consumed retry budget: job=%+v err=%v", claimed, err)
	}
	if _, err := store.db.ExecContext(ctx, `CREATE TRIGGER fail_defer_event BEFORE INSERT ON events BEGIN SELECT RAISE(ABORT,'fault'); END`); err != nil {
		t.Fatal(err)
	}
	if err := store.DeferJob(ctx, job.ID, "worker", 0, "SNAPSHOT_PENDING"); err == nil {
		t.Fatal("dependency deferral ignored event persistence failure")
	}
	if _, err := store.db.ExecContext(ctx, `DROP TRIGGER fail_defer_event`); err != nil {
		t.Fatal(err)
	}
	closed := openTestStore(t)
	if err := closed.Close(); err != nil {
		t.Fatal(err)
	}
	if err := closed.DeferJob(ctx, "missing", "worker", 0, "SNAPSHOT_PENDING"); err == nil {
		t.Fatal("dependency deferral succeeded after store closure")
	}
}

func TestJobEventsRecordAttemptsRetriesAndElapsedTime(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	job, err := store.CreateJob(ctx, "PREPARE", "", "slot", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ClaimJob(ctx, job.ID, "owner"); err != nil {
		t.Fatal(err)
	}
	if err := store.RetryJob(ctx, job.ID, "owner", time.Millisecond, "TRANSIENT"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`UPDATE jobs SET not_before=? WHERE id=?`, now(), job.ID); err != nil {
		t.Fatal(err)
	}
	claimed, err := store.ClaimJob(ctx, job.ID, "owner")
	if err != nil {
		t.Fatal(err)
	}
	if claimed.Attempt != 2 {
		t.Fatalf("attempt=%d", claimed.Attempt)
	}
	if err := store.FinishJob(ctx, job.ID, "owner", nil); err != nil {
		t.Fatal(err)
	}
	rows, err := store.db.Query(`SELECT kind,message FROM events WHERE slot_id='slot' ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var events []string
	for rows.Next() {
		var kind, message string
		if err := rows.Scan(&kind, &message); err != nil {
			t.Fatal(err)
		}
		events = append(events, kind+" "+message)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(events, "\n")
	for _, fragment := range []string{"job_started kind=PREPARE attempt=1", "job_retry delay=1ms failure_code=TRANSIENT", "job_started kind=PREPARE attempt=2", "PREPARE state=SUCCEEDED elapsed="} {
		if !strings.Contains(joined, fragment) {
			t.Fatalf("event log missing %q:\n%s", fragment, joined)
		}
	}
}

func TestBindFreshResumeSlotRollsBackAtEveryPersistenceBoundary(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*testing.T, *Store)
		repos  []SlotRepository
	}{
		{name: "parent mapping", mutate: func(t *testing.T, store *Store) {
			if _, err := store.db.Exec(`UPDATE sessions SET state='ACTIVE' WHERE id='parent'`); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "session update", mutate: func(t *testing.T, store *Store) {
			if _, err := store.db.Exec(`UPDATE sessions SET state='ACTIVE' WHERE id='current'`); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "slot update", mutate: func(t *testing.T, store *Store) {
			if _, err := store.db.Exec(`UPDATE slots SET state='FAILED' WHERE id='current'`); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "repository insert", repos: []SlotRepository{{RepositoryID: "missing", WorktreePath: "/wx/current/repo", RequestedRef: "main", BaseOID: "abc", Fingerprint: "fp"}}},
		{name: "lease timestamp", repos: []SlotRepository{{RepositoryID: "repository", WorktreePath: "/wx/current/repo", RequestedRef: "main", BaseOID: "abc", Fingerprint: "fp"}}, mutate: func(t *testing.T, store *Store) {
			if _, err := store.db.Exec(`CREATE TRIGGER fail_fresh_lease_timestamp BEFORE UPDATE OF last_leased_at ON repositories BEGIN SELECT RAISE(ABORT,'fault'); END`); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "job insert", mutate: func(t *testing.T, store *Store) {
			if _, err := store.db.Exec(`CREATE TRIGGER fail_fresh_job BEFORE INSERT ON jobs BEGIN SELECT RAISE(ABORT,'fault'); END`); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			store := openTestStore(t)
			seedWorkspace(t, store)
			ctx := context.Background()
			parent := Session{ID: "parent", WorkspaceID: "workspace", SlotID: "parent", State: "EXPIRED", AgentKind: "codex", AgentSessionID: "agent", TokenHash: HashToken("parent")}
			if _, err := store.CreateSlotSession(ctx, Slot{ID: "parent", WorkspaceID: "workspace", Generation: 1, Path: "/wx/parent", State: "SNAPSHOTTED"}, nil, parent, ""); err != nil {
				t.Fatal(err)
			}
			if _, err := store.db.Exec(`UPDATE sessions SET agent_session_id='agent' WHERE id='parent'`); err != nil {
				t.Fatal(err)
			}
			current := Session{ID: "current", SlotID: "current", State: "UNBOUND", AgentKind: "codex", TokenHash: HashToken("current")}
			if _, err := store.CreateSlotSession(ctx, Slot{ID: "current", Path: "/wx/current", State: "UNBOUND"}, nil, current, ""); err != nil {
				t.Fatal(err)
			}
			if test.mutate != nil {
				test.mutate(t, store)
			}
			if _, err := store.BindFreshResumeSlot(ctx, "current", "parent", "workspace", "agent", 1, test.repos); err == nil {
				t.Fatal("fault-injected fresh resume binding succeeded")
			}
			storedParent, err := store.SessionByID(ctx, "parent")
			if err != nil || storedParent.AgentSessionID != "agent" {
				t.Fatalf("parent mapping was not rolled back: session=%+v err=%v", storedParent, err)
			}
			var jobs int
			if err := store.db.QueryRow(`SELECT count(*) FROM jobs WHERE slot_id='current'`).Scan(&jobs); err != nil || jobs != 0 {
				t.Fatalf("partial fresh resume job count=%d err=%v", jobs, err)
			}
		})
	}
}

func TestBindFreshSessionCommitsOrRollsBackAtomically(t *testing.T) {
	for _, test := range []struct {
		name         string
		currentState string
		trigger      string
		wantError    bool
	}{
		{name: "success", currentState: "STARTING"},
		{name: "state changed", currentState: "UNBOUND", wantError: true},
		{name: "parent update fault", currentState: "STARTING", trigger: `CREATE TRIGGER fail_parent_update BEFORE UPDATE ON sessions WHEN OLD.id='parent' BEGIN SELECT RAISE(ABORT,'fault'); END`, wantError: true},
		{name: "current update fault", currentState: "STARTING", trigger: `CREATE TRIGGER fail_current_update BEFORE UPDATE ON sessions WHEN OLD.id='current' BEGIN SELECT RAISE(ABORT,'fault'); END`, wantError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := openTestStore(t)
			ctx := context.Background()
			parent := Session{ID: "parent", SlotID: "parent", State: "EXPIRED", AgentKind: "codex", TokenHash: HashToken("parent")}
			if _, err := store.CreateSlotSession(ctx, Slot{ID: "parent", Path: "/wx/parent", State: "SNAPSHOTTED"}, nil, parent, ""); err != nil {
				t.Fatal(err)
			}
			if _, err := store.db.Exec(`UPDATE sessions SET agent_session_id='agent' WHERE id='parent'`); err != nil {
				t.Fatal(err)
			}
			current := Session{ID: "current", SlotID: "current", State: test.currentState, AgentKind: "codex", TokenHash: HashToken("current")}
			if _, err := store.CreateSlotSession(ctx, Slot{ID: "current", Path: "/wx/current", State: test.currentState}, nil, current, ""); err != nil {
				t.Fatal(err)
			}
			if test.trigger != "" {
				if _, err := store.db.Exec(test.trigger); err != nil {
					t.Fatal(err)
				}
			}
			err := store.BindFreshSession(ctx, "current", "parent", "agent")
			if (err != nil) != test.wantError {
				t.Fatalf("BindFreshSession error=%v wantError=%v", err, test.wantError)
			}
			storedParent, err := store.SessionByID(ctx, "parent")
			if err != nil {
				t.Fatal(err)
			}
			if test.wantError && storedParent.AgentSessionID != "agent" {
				t.Fatalf("parent mapping was not rolled back: %+v", storedParent)
			}
			if !test.wantError {
				storedCurrent, err := store.SessionByID(ctx, "current")
				if err != nil || storedCurrent.State != "ACTIVE" || storedCurrent.ParentSessionID != "parent" || storedCurrent.AgentSessionID != "agent" || storedParent.AgentSessionID != "" {
					t.Fatalf("fresh binding current=%+v parent=%+v err=%v", storedCurrent, storedParent, err)
				}
			}
		})
	}
}

func TestStatusDiagnosticsFailsAtEachSchemaBoundary(t *testing.T) {
	for _, table := range []string{"workspaces", "sessions", "repositories", "jobs", "snapshots", "quarantined_artifacts"} {
		t.Run(table, func(t *testing.T) {
			store := openTestStore(t)
			if _, err := store.db.Exec(`DROP TABLE ` + table); err != nil {
				t.Fatal(err)
			}
			if _, err := store.StatusDiagnostics(context.Background()); err == nil {
				t.Fatalf("status diagnostics succeeded without %s", table)
			}
		})
	}
}

func TestStatusDiagnosticsRejectsMalformedRows(t *testing.T) {
	for _, test := range []struct {
		name string
		sql  []string
	}{
		{name: "workspace", sql: []string{`DROP TABLE workspaces`, `CREATE VIEW workspaces AS SELECT 'id' AS id,'/root' AS root_path,'bad' AS generation`}},
		{name: "session", sql: []string{`DROP TABLE sessions`, `CREATE VIEW sessions AS SELECT 'id' AS id,NULL AS agent_kind,'ACTIVE' AS state,'now' AS created_at,'slot' AS slot_id`}},
		{name: "repository", sql: []string{`DROP TABLE repositories`, `CREATE VIEW repositories AS SELECT NULL AS id,'/repo' AS main_worktree_path,NULL AS last_leased_at`}},
		{name: "quarantine", sql: []string{`DROP TABLE quarantined_artifacts`, `CREATE VIEW quarantined_artifacts AS SELECT '/path' AS path,NULL AS reason`}},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := openTestStore(t)
			for _, statement := range test.sql {
				if _, err := store.db.Exec(statement); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := store.StatusDiagnostics(context.Background()); err == nil {
				t.Fatal("malformed diagnostic row was accepted")
			}
		})
	}
}

func TestNewJobPersistenceRollsBackOnLateDatabaseFaults(t *testing.T) {
	for _, test := range []struct {
		name    string
		trigger string
		retry   bool
	}{
		{name: "finish update", trigger: `CREATE TRIGGER fail_finish_update BEFORE UPDATE OF state ON jobs WHEN NEW.state='SUCCEEDED' BEGIN SELECT RAISE(ABORT,'fault'); END`},
		{name: "finish ignored update", trigger: `CREATE TRIGGER ignore_finish_update BEFORE UPDATE OF state ON jobs WHEN NEW.state='SUCCEEDED' BEGIN SELECT RAISE(IGNORE); END`},
		{name: "finish event", trigger: `CREATE TRIGGER fail_finish_event BEFORE INSERT ON events BEGIN SELECT RAISE(ABORT,'fault'); END`},
		{name: "retry event", trigger: `CREATE TRIGGER fail_retry_event BEFORE INSERT ON events BEGIN SELECT RAISE(ABORT,'fault'); END`, retry: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := openTestStore(t)
			ctx := context.Background()
			job, err := store.CreateJob(ctx, "PREPARE", "", "slot", "")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := store.ClaimJob(ctx, job.ID, "owner"); err != nil {
				t.Fatal(err)
			}
			if _, err := store.db.Exec(test.trigger); err != nil {
				t.Fatal(err)
			}
			if test.retry {
				err = store.RetryJob(ctx, job.ID, "owner", time.Second, "TRANSIENT")
			} else {
				err = store.FinishJob(ctx, job.ID, "owner", nil)
			}
			if err == nil {
				t.Fatal("fault-injected job transition succeeded")
			}
			var stateName string
			if err := store.db.QueryRow(`SELECT state FROM jobs WHERE id=?`, job.ID).Scan(&stateName); err != nil || stateName != "RUNNING" {
				t.Fatalf("job transition was not rolled back: state=%s err=%v", stateName, err)
			}
		})
	}
	t.Run("invalid start time", func(t *testing.T) {
		store := openTestStore(t)
		ctx := context.Background()
		job, err := store.CreateJob(ctx, "PREPARE", "", "slot", "")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.ClaimJob(ctx, job.ID, "owner"); err != nil {
			t.Fatal(err)
		}
		if _, err := store.db.Exec(`UPDATE jobs SET started_at='invalid' WHERE id=?`, job.ID); err != nil {
			t.Fatal(err)
		}
		if err := store.FinishJob(ctx, job.ID, "owner", nil); err != nil {
			t.Fatal(err)
		}
	})
}

func TestEnsureRecoveryJobsReportsQueryAndInsertFaults(t *testing.T) {
	t.Run("query", func(t *testing.T) {
		store := openTestStore(t)
		if _, err := store.db.Exec(`DROP TABLE slots`); err != nil {
			t.Fatal(err)
		}
		if _, err := store.EnsureRecoveryJobs(context.Background()); err == nil {
			t.Fatal("recovery reconstruction succeeded without slots")
		}
	})
	t.Run("insert", func(t *testing.T) {
		store := openTestStore(t)
		seedWorkspace(t, store)
		if _, err := store.db.Exec(`INSERT INTO slots(id,workspace_id,generation,path,state,created_at,updated_at) VALUES('slot','workspace',1,'/wx/slot','PREPARING',?,?)`, now(), now()); err != nil {
			t.Fatal(err)
		}
		if _, err := store.db.Exec(`CREATE TRIGGER fail_recovery_job BEFORE INSERT ON jobs BEGIN SELECT RAISE(ABORT,'fault'); END`); err != nil {
			t.Fatal(err)
		}
		if _, err := store.EnsureRecoveryJobs(context.Background()); err == nil {
			t.Fatal("recovery reconstruction succeeded despite job insert fault")
		}
	})
}

func TestPruneMetadataReportsLateSchemaFaults(t *testing.T) {
	for _, table := range []string{"sessions", "rpc_idempotency"} {
		t.Run(table, func(t *testing.T) {
			store := openTestStore(t)
			if _, err := store.db.Exec(`DROP TABLE ` + table); err != nil {
				t.Fatal(err)
			}
			if err := store.PruneMetadata(context.Background(), now(), now(), now()); err == nil {
				t.Fatalf("metadata pruning succeeded without %s", table)
			}
		})
	}
}

func TestAdministrativeStateTransitionsAndQueries(t *testing.T) {
	store := openTestStore(t)
	seedWorkspace(t, store)
	ctx := context.Background()
	root := t.TempDir()
	job, err := store.CreateStandby(ctx, Slot{ID: "standby", WorkspaceID: "workspace", Generation: 1, Path: filepath.Join(root, "standby"), State: "PREPARING"}, []SlotRepository{{RepositoryID: "repository", WorktreePath: filepath.Join(root, "standby", "repo"), State: "PREPARING", RequestedRef: "main", BaseOID: "abc", Fingerprint: "fp"}})
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := store.ClaimJob(ctx, job.ID, "owner")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.RenewJob(ctx, claimed.ID, "owner"); err != nil {
		t.Fatal(err)
	}
	if err := store.SetSlotRepositoryState(ctx, "standby", "repository", []string{"PREPARING"}, "READY"); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkReady(ctx, "standby"); err != nil {
		t.Fatal(err)
	}
	if store.StandbyCount(ctx, "workspace") == 0 {
		t.Fatal("ready standby was not counted")
	}
	if slots, err := store.ReadySlots(ctx, "workspace"); err != nil || len(slots) != 1 {
		t.Fatalf("ready slots=%+v err=%v", slots, err)
	}
	if err := store.FinishJob(ctx, claimed.ID, "owner", nil); err != nil {
		t.Fatal(err)
	}
	session := Session{ID: "standby", WorkspaceID: "workspace", SlotID: "standby", State: "ACTIVE", AgentKind: "codex", ClientPID: os.Getpid(), TokenHash: HashToken("token")}
	if err := store.LeaseReady(ctx, "standby", session); err != nil {
		t.Fatal(err)
	}
	if err := store.Heartbeat(ctx, "standby", "token"); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkSessionState(ctx, "standby", []string{"ACTIVE"}, "RELEASING"); err != nil {
		t.Fatal(err)
	}
	if sessions, err := store.ListSessions(ctx, true); err != nil || len(sessions) != 1 || sessions[0].ID != "standby" {
		t.Fatalf("sessions=%+v err=%v", sessions, err)
	}
	if err := store.Ping(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestOwnershipAndCompareAndSwapFailuresAreRejected(t *testing.T) {
	store := openTestStore(t)
	seedWorkspace(t, store)
	ctx := context.Background()
	job, err := store.CreateJob(ctx, "PREPARE", "workspace", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ClaimJob(ctx, job.ID, "owner"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ClaimJob(ctx, job.ID, "owner"); err == nil {
		t.Fatal("running job was claimed twice")
	}
	if err := store.RenewJob(ctx, job.ID, "intruder"); err == nil {
		t.Fatal("job lease was renewed by another owner")
	}
	if err := store.RetryJob(ctx, job.ID, "intruder", time.Second, "retry"); err == nil {
		t.Fatal("job was retried by another owner")
	}
	if err := store.FinishJob(ctx, job.ID, "intruder", nil); err == nil {
		t.Fatal("job was finished by another owner")
	}
	if err := store.FinishJob(ctx, job.ID, "owner", errors.New("expected")); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	session := Session{ID: "active", WorkspaceID: "workspace", SlotID: "active", State: "ACTIVE", AgentKind: "codex", TokenHash: HashToken("token")}
	if _, err := store.CreateSlotSession(ctx, Slot{ID: "active", WorkspaceID: "workspace", Generation: 1, Path: filepath.Join(root, "active"), State: "LEASED"}, nil, session, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Session(ctx, "active", "wrong"); err == nil {
		t.Fatal("wrong session token succeeded")
	}
	if err := store.Heartbeat(ctx, "active", "wrong"); err == nil {
		t.Fatal("wrong heartbeat token succeeded")
	}
	if err := store.BindAgentSession(ctx, "active", "agent-one"); err != nil {
		t.Fatal(err)
	}
	if err := store.BindAgentSession(ctx, "active", "agent-two"); err == nil {
		t.Fatal("agent session was rebound ambiguously")
	}
	if err := store.SetSlotState(ctx, "active", []string{"READY"}, "STALE", "test"); err == nil {
		t.Fatal("slot state CAS mismatch succeeded")
	}
	if _, _, err := store.FinishPreparationWithRelease(ctx, "active"); err == nil {
		t.Fatal("leased slot finished preparation")
	}
	if err := store.MarkSessionState(ctx, "active", []string{"STARTING"}, "ACTIVE"); err == nil {
		t.Fatal("session state CAS mismatch succeeded")
	}
	if err := store.ForgetWorkspace(ctx, "/workspace"); err == nil {
		t.Fatal("active workspace was forgotten")
	}
	if _, changed, err := store.ScheduleRemoval(ctx, "active", "active"); err != nil || changed {
		t.Fatalf("active removal changed=%v err=%v", changed, err)
	}
	if err := store.AddRestoringRepositories(ctx, "active", nil); err == nil {
		t.Fatal("repositories were added to non-restoring slot")
	}
}

func TestDrainArchiveAndForgetAdministrativePaths(t *testing.T) {
	store := openTestStore(t)
	seedWorkspace(t, store)
	ctx := context.Background()
	root := t.TempDir()
	job, err := store.CreateStandby(ctx, Slot{ID: "slot", WorkspaceID: "workspace", Generation: 1, Path: filepath.Join(root, "slot"), State: "READY"}, []SlotRepository{{RepositoryID: "repository", WorktreePath: filepath.Join(root, "slot", "repo"), State: "READY", RequestedRef: "main", BaseOID: "abc", Fingerprint: "fp"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.DrainRoot(ctx, root); err != nil {
		t.Fatal(err)
	}
	if slot, err := store.Slot(ctx, "slot"); err != nil || slot.State != "STALE" {
		t.Fatalf("drained slot=%+v err=%v", slot, err)
	}
	if err := store.SetSlotState(ctx, "slot", []string{"READY", "STALE"}, "ARCHIVED", ""); err != nil {
		t.Fatal(err)
	}
	if err := store.SetSlotState(ctx, "slot", []string{"ARCHIVED"}, "SNAPSHOTTED", ""); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkSlotArchived(ctx, "slot"); err != nil {
		t.Fatal(err)
	}
	claimed, err := store.ClaimJob(ctx, job.ID, "owner")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.FinishJob(ctx, claimed.ID, "owner", nil); err != nil {
		t.Fatal(err)
	}
	if err := store.ForgetWorkspace(ctx, "/workspace"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Workspace(ctx, "workspace"); err == nil {
		t.Fatal("forgotten workspace still exists")
	}
}

func TestForgetWorkspaceRefusesLiveRecoveryMappings(t *testing.T) {
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
				if _, err := store.CreateSlotSession(ctx, Slot{ID: "slot", WorkspaceID: "workspace", Generation: 1, Path: "/workspace/slot", State: "ARCHIVED"}, nil, session, ""); err != nil {
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
				if _, err := store.CreateSlotSession(ctx, Slot{ID: "slot", WorkspaceID: "workspace", Generation: 1, Path: "/workspace/slot", State: "ARCHIVED"}, nil, session, ""); err != nil {
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
				if _, err := store.CreateSlotSession(ctx, Slot{ID: "slot", WorkspaceID: "workspace", Generation: 1, Path: "/workspace/slot", State: "ARCHIVED"}, nil, session, ""); err != nil {
					t.Fatal(err)
				}
				if err := store.SaveWorkspaceSnapshot(ctx, WorkspaceSnapshot{SessionID: "session", ArchivePath: "/workspace/snapshot.tar", SHA256: strings.Repeat("a", 64), Status: "ARCHIVED", CreatedAt: now(), ExpiresAt: FormatTime(time.Now().Add(time.Hour))}); err != nil {
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

func TestSaveSnapshotIsIdempotentButRejectsConflict(t *testing.T) {
	store := openTestStore(t)
	seedWorkspace(t, store)
	ctx := context.Background()
	session := Session{ID: "session", WorkspaceID: "workspace", SlotID: "slot", State: "ACTIVE", AgentKind: "codex", TokenHash: HashToken("token")}
	if _, err := store.CreateSlotSession(ctx, Slot{ID: "slot", WorkspaceID: "workspace", Generation: 1, Path: filepath.Join(t.TempDir(), "slot"), State: "LEASED"}, nil, session, ""); err != nil {
		t.Fatal(err)
	}
	snapshot := Snapshot{ID: "snapshot", SessionID: "session", RepositoryID: "repository", HeadOID: "head", HeadRef: "refs/wx/head", IndexTreeOID: "index", WorktreeOID: "worktree", WorktreeRef: "refs/wx/worktree", Status: "ARCHIVED", CreatedAt: now(), ExpiresAt: time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano)}
	if err := store.SaveSnapshot(ctx, snapshot); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveSnapshot(ctx, snapshot); err != nil {
		t.Fatalf("idempotent save: %v", err)
	}
	snapshot.WorktreeOID = "different"
	if err := store.SaveSnapshot(ctx, snapshot); err == nil {
		t.Fatal("conflicting snapshot was accepted")
	}
}

func TestWorkspaceMembershipChangeAdvancesGenerationAndStalesOldStandby(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	w := discovery.Workspace{ID: "workspace", Root: "/workspace", Kind: "multi_repository", Repositories: []discovery.Repository{{ID: "repository", MainPath: "/workspace/repository", CommonDir: "/workspace/repository/.git", RelativePath: "repository", DefaultBranch: "main"}}}
	generation, err := store.UpsertWorkspaceGeneration(ctx, w)
	if err != nil || generation != 1 {
		t.Fatalf("initial generation=%d err=%v", generation, err)
	}
	job, err := store.CreateStandby(ctx, Slot{ID: "slot", WorkspaceID: "workspace", Generation: generation, Path: filepath.Join(t.TempDir(), "slot"), State: "READY"}, []SlotRepository{{RepositoryID: "repository", WorktreePath: filepath.Join(t.TempDir(), "worktree"), State: "READY", RequestedRef: "main", BaseOID: "abc", Fingerprint: "fingerprint"}})
	if err != nil || job.Kind != "PREPARE" {
		t.Fatalf("create standby job=%+v err=%v", job, err)
	}
	if generation, err = store.UpsertWorkspaceGeneration(ctx, w); err != nil || generation != 1 {
		t.Fatalf("unchanged generation=%d err=%v", generation, err)
	}
	w.Repositories = append(w.Repositories, discovery.Repository{ID: "repository-2", MainPath: "/workspace/repository-2", CommonDir: "/workspace/repository-2/.git", RelativePath: "repository-2", DefaultBranch: "main"})
	if generation, err = store.UpsertWorkspaceGeneration(ctx, w); err != nil || generation != 2 {
		t.Fatalf("changed generation=%d err=%v", generation, err)
	}
	slot, err := store.Slot(ctx, "slot")
	if err != nil || slot.State != "STALE" {
		t.Fatalf("old slot=%+v err=%v", slot, err)
	}
	loaded, err := store.Workspace(ctx, "workspace")
	if err != nil || len(loaded.Repositories) != 2 {
		t.Fatalf("workspace repositories=%d err=%v", len(loaded.Repositories), err)
	}
}

func TestOnlineBackupContainsCommittedRegistry(t *testing.T) {
	store := openTestStore(t)
	seedWorkspace(t, store)
	path, err := store.Backup(context.Background(), 2, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	info, err := filepath.Glob(filepath.Join(filepath.Dir(path), "*.db"))
	if err != nil || len(info) != 1 {
		t.Fatalf("backup files=%v err=%v", info, err)
	}
	backup, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer backup.Close()
	workspace, err := backup.Workspace(context.Background(), "workspace")
	if err != nil || len(workspace.Repositories) != 1 {
		t.Fatalf("backup workspace=%+v err=%v", workspace, err)
	}
}

func TestWALAllowsStatusReadWhileWriteTransactionIsOpen(t *testing.T) {
	store := openTestStore(t)
	seedWorkspace(t, store)
	ctx := context.Background()
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `UPDATE workspaces SET last_seen_at=? WHERE id='workspace'`, now()); err != nil {
		t.Fatal(err)
	}
	readDone := make(chan error, 1)
	go func() {
		_, err := store.Status(ctx)
		readDone <- err
	}()
	select {
	case err := <-readDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("WAL status read blocked behind writer")
	}
}

func TestEverySQLiteConnectionEnforcesPolicyPragmas(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	connections := make([]interface{ Close() error }, 0, 3)
	for range 3 {
		conn, err := store.db.Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		connections = append(connections, conn)
		var foreignKeys, busyTimeout int
		var journalMode string
		if err := conn.QueryRowContext(ctx, `PRAGMA foreign_keys`).Scan(&foreignKeys); err != nil {
			t.Fatal(err)
		}
		if err := conn.QueryRowContext(ctx, `PRAGMA busy_timeout`).Scan(&busyTimeout); err != nil {
			t.Fatal(err)
		}
		if err := conn.QueryRowContext(ctx, `PRAGMA journal_mode`).Scan(&journalMode); err != nil {
			t.Fatal(err)
		}
		if foreignKeys != 1 || busyTimeout != 5000 || journalMode != "wal" {
			t.Fatalf("pragmas foreign_keys=%d busy_timeout=%d journal_mode=%s", foreignKeys, busyTimeout, journalMode)
		}
	}
	for _, conn := range connections {
		_ = conn.Close()
	}
}

func TestCorruptDatabaseIsNotReplacedOrClaimed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	original := []byte("not a sqlite database; preserve for recovery")
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	if store, err := Open(path); err == nil {
		_ = store.Close()
		t.Fatal("corrupt database unexpectedly opened")
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, original) {
		t.Fatalf("corrupt database was modified: %q", after)
	}
}

func TestDamagedSchemaFailsEveryOperationWithoutRecreatingState(t *testing.T) {
	store := openTestStore(t)
	for _, table := range []string{"rpc_idempotency", "quarantined_artifacts", "workspace_snapshots", "snapshots", "jobs", "session_repositories", "sessions", "slot_repositories", "slots", "workspace_repositories", "repositories", "workspaces", "events"} {
		if _, err := store.db.Exec(`DROP TABLE ` + table); err != nil {
			t.Fatalf("drop %s: %v", table, err)
		}
	}
	ctx := context.Background()
	w := discovery.Workspace{ID: "w", Root: "/w", Kind: "repository"}
	slot := Slot{ID: "s", WorkspaceID: "w", Path: "/wx/s", State: "PREPARING"}
	repository := SlotRepository{RepositoryID: "r", WorktreePath: "/wx/s/r", State: "PREPARING"}
	session := Session{ID: "s", WorkspaceID: "w", SlotID: "s", State: "STARTING", AgentKind: "codex", TokenHash: HashToken("token")}
	snapshot := Snapshot{ID: "snapshot", SessionID: "s", RepositoryID: "r"}

	if _, err := store.UpsertWorkspaceGeneration(ctx, w); err == nil {
		t.Error("upsert succeeded against damaged schema")
	}
	if _, err := store.WorkspaceGeneration(ctx, "w"); err == nil {
		t.Error("generation lookup succeeded against damaged schema")
	}
	if _, err := store.CreateJob(ctx, "PREPARE", "w", "s", "s"); err == nil {
		t.Error("job creation succeeded against damaged schema")
	}
	if _, err := store.ClaimJob(ctx, "j", "owner"); err == nil {
		t.Error("job claim succeeded against damaged schema")
	}
	if err := store.RenewJob(ctx, "j", "owner"); err == nil {
		t.Error("job renewal succeeded against damaged schema")
	}
	if err := store.FinishJob(ctx, "j", "owner", nil); err == nil {
		t.Error("job finish succeeded against damaged schema")
	}
	if err := store.RetryJob(ctx, "j", "owner", time.Second, "retry"); err == nil {
		t.Error("job retry succeeded against damaged schema")
	}
	if _, err := store.RecoverJobs(ctx, false); err == nil {
		t.Error("job recovery succeeded against damaged schema")
	}
	if _, _, err := store.ReadySlot(ctx, "w"); err == nil {
		t.Error("ready lookup succeeded against damaged schema")
	}
	if _, err := store.ReadySlots(ctx, "w"); err == nil {
		t.Error("ready list succeeded against damaged schema")
	}
	if _, err := store.CreateSlotSession(ctx, slot, []SlotRepository{repository}, session, "PREPARE"); err == nil {
		t.Error("slot creation succeeded against damaged schema")
	}
	if _, err := store.CreateStandby(ctx, slot, []SlotRepository{repository}); err == nil {
		t.Error("standby creation succeeded against damaged schema")
	}
	for name, operation := range map[string]func() error{
		"lease":              func() error { return store.LeaseReady(ctx, "s", session) },
		"set slot":           func() error { return store.SetSlotState(ctx, "s", []string{"READY"}, "STALE", "test") },
		"finish preparation": func() error { _, _, err := store.FinishPreparationWithRelease(ctx, "s"); return err },
		"session state":      func() error { return store.MarkSessionState(ctx, "s", []string{"ACTIVE"}, "RELEASING") },
		"bind agent":         func() error { return store.BindAgentSession(ctx, "s", "agent") },
		"bind fresh":         func() error { return store.BindFreshSession(ctx, "s", "", "agent") },
		"heartbeat":          func() error { return store.Heartbeat(ctx, "s", "token") },
		"add restore repos":  func() error { return store.AddRestoringRepositories(ctx, "s", []SlotRepository{repository}) },
		"set repository":     func() error { return store.SetSlotRepositoryState(ctx, "s", "r", []string{"READY"}, "COLD") },
		"save snapshot":      func() error { return store.SaveSnapshot(ctx, snapshot) },
		"forget":             func() error { return store.ForgetWorkspace(ctx, "/w") },
		"mark archived":      func() error { return store.MarkArchived(ctx, "s", "s", now()) },
		"begin snapshot":     func() error { return store.BeginSnapshot(ctx, "s", "s") },
		"finish cold":        func() error { return store.FinishColdRepositoryRemoval(ctx, "s", "r") },
		"finish removal":     func() error { return store.FinishRemoval(ctx, "s") },
		"drain root":         func() error { return store.DrainRoot(ctx, "/wx") },
		"expire snapshots":   func() error { return store.ExpireSessionSnapshots(ctx, "s") },
		"prune":              func() error { return store.PruneMetadata(ctx, now(), now(), now()) },
	} {
		if err := operation(); err == nil {
			t.Errorf("%s succeeded against damaged schema", name)
		}
	}
	for name, operation := range map[string]func() error{
		"session":           func() error { _, err := store.Session(ctx, "s", "token"); return err },
		"session by id":     func() error { _, err := store.SessionByID(ctx, "s"); return err },
		"find agent":        func() error { _, err := store.FindByAgentSession(ctx, "codex", "agent"); return err },
		"orphans":           func() error { _, err := store.OrphanCandidates(ctx, now()); return err },
		"slot repos":        func() error { _, err := store.SlotRepositories(ctx, "s"); return err },
		"slot repo":         func() error { _, err := store.SlotRepository(ctx, "s", "r"); return err },
		"slot":              func() error { _, err := store.Slot(ctx, "s"); return err },
		"snapshots":         func() error { _, err := store.Snapshots(ctx, "s"); return err },
		"repository":        func() error { _, err := store.Repository(ctx, "r"); return err },
		"workspace":         func() error { _, err := store.Workspace(ctx, "w"); return err },
		"workspace roots":   func() error { _, err := store.WorkspaceRoots(ctx); return err },
		"status":            func() error { _, err := store.Status(ctx); return err },
		"sessions":          func() error { _, err := store.ListSessions(ctx, true); return err },
		"standby gc":        func() error { _, err := store.StandbyGCCandidates(ctx, now(), 1); return err },
		"cold candidates":   func() error { _, err := store.ColdRepositoryCandidates(ctx, now()); return err },
		"expired snapshots": func() error { _, err := store.ExpiredSnapshots(ctx, now()); return err },
		"gc candidates":     func() error { _, err := store.GCCandidates(ctx, now()); return err },
	} {
		if err := operation(); err == nil {
			t.Errorf("%s succeeded against damaged schema", name)
		}
	}
	if _, err := store.LeaseReadyWithCold(ctx, "s", session); err == nil {
		t.Error("cold lease succeeded against damaged schema")
	}
	if _, err := store.BindResumeSlot(ctx, "s", "parent", "w", "agent", 1, nil); err == nil {
		t.Error("resume binding succeeded against damaged schema")
	}
	if _, _, err := store.Release(ctx, "s", "w", "s"); err == nil {
		t.Error("release succeeded against damaged schema")
	}
	if _, _, err := store.ScheduleColdRepositoryRemoval(ctx, ColdRepositoryCandidate{SlotID: "s", WorkspaceID: "w", RepositoryID: "r"}); err == nil {
		t.Error("cold removal scheduling succeeded against damaged schema")
	}
	if _, _, err := store.ScheduleRemoval(ctx, "s", "s"); err == nil {
		t.Error("removal scheduling succeeded against damaged schema")
	}
}

func TestRPCIdempotencyResultSurvivesStoreRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	params := `{"value":1}`
	resultPayload := []byte(`{"value":"response"}`)
	if _, _, _, execute, err := store.BeginRPCRequest(ctx, "key", "Mutate", params, time.Now().Add(time.Hour)); err != nil || !execute {
		t.Fatalf("begin execute=%v err=%v", execute, err)
	}
	if err := store.CompleteRPCRequest(ctx, "key", "Mutate", params, resultPayload, "", "", time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	var storedResult []byte
	if err := store.db.QueryRow(`SELECT result FROM rpc_idempotency WHERE idempotency_key='key'`).Scan(&storedResult); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(storedResult, resultPayload) {
		t.Fatalf("stored RPC result=%q want=%q", storedResult, resultPayload)
	}
	if _, _, _, execute, err := store.BeginRPCRequest(ctx, "pending", "Mutate", `{}`, time.Now().Add(time.Hour)); err != nil || !execute {
		t.Fatalf("pending begin execute=%v err=%v", execute, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	result, code, message, execute, err := store.BeginRPCRequest(ctx, "key", "Mutate", params, time.Now().Add(time.Hour))
	if err != nil || execute || string(result) != string(resultPayload) || code != "" || message != "" {
		t.Fatalf("result=%s code=%q message=%q execute=%v err=%v", result, code, message, execute, err)
	}
	_, code, _, execute, err = store.BeginRPCRequest(ctx, "key", "Mutate", `{"value":2}`, time.Now().Add(time.Hour))
	if err != nil || execute || code != "IDEMPOTENCY_KEY_REUSE" {
		t.Fatalf("mismatch code=%q execute=%v err=%v", code, execute, err)
	}
	_, code, _, execute, err = store.BeginRPCRequest(ctx, "pending", "Mutate", `{}`, time.Now().Add(time.Hour))
	if err != nil || execute || code != "IDEMPOTENCY_INDETERMINATE" {
		t.Fatalf("pending code=%q execute=%v err=%v", code, execute, err)
	}
	if _, _, _, execute, err := store.BeginRPCRequest(ctx, "expired", "Mutate", `{}`, time.Now().Add(-time.Hour)); err != nil || !execute {
		t.Fatalf("expired begin execute=%v err=%v", execute, err)
	}
	if err := store.PruneMetadata(ctx, FormatTime(time.Now().Add(-time.Hour)), FormatTime(time.Now().Add(-time.Hour)), FormatTime(time.Now().Add(-time.Hour))); err != nil {
		t.Fatal(err)
	}
	var expired int
	if err := store.db.QueryRow(`SELECT count(*) FROM rpc_idempotency WHERE idempotency_key='expired'`).Scan(&expired); err != nil || expired != 0 {
		t.Fatalf("expired idempotency rows=%d err=%v", expired, err)
	}
}

func TestRPCIdempotencyRejectsInvalidReservationTransitions(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	expiry := time.Now().Add(time.Hour)

	if err := store.CompleteRPCRequest(ctx, "missing", "Mutate", `{}`, nil, "", "", expiry); err == nil {
		t.Fatal("completion without a reservation succeeded")
	}
	if _, _, _, execute, err := store.BeginRPCRequest(ctx, "complete", "Mutate", `{}`, expiry); err != nil || !execute {
		t.Fatalf("begin execute=%v err=%v", execute, err)
	}
	if err := store.CompleteRPCRequest(ctx, "complete", "Mutate", `{}`, nil, "EXPECTED", "details", expiry); err != nil {
		t.Fatal(err)
	}
	result, code, message, execute, err := store.BeginRPCRequest(ctx, "complete", "Mutate", `{}`, expiry)
	if err != nil || execute || result != nil || code != "EXPECTED" || message != "details" {
		t.Fatalf("replay result=%v code=%q message=%q execute=%v err=%v", result, code, message, execute, err)
	}
	if err := store.CompleteRPCRequest(ctx, "complete", "Mutate", `{}`, nil, "", "", expiry); err == nil {
		t.Fatal("completed reservation was completed twice")
	}
	if err := store.CompleteRPCRequest(ctx, "complete", "Other", `{}`, nil, "", "", expiry); err == nil {
		t.Fatal("reservation completed with a different method")
	}

	key := "invalid-state"
	if _, _, _, execute, err := store.BeginRPCRequest(ctx, key, "Mutate", `{}`, expiry); err != nil || !execute {
		t.Fatalf("begin execute=%v err=%v", execute, err)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE rpc_idempotency SET state=? WHERE idempotency_key=?`, "UNKNOWN", key); err != nil {
		t.Fatal(err)
	}
	if _, _, _, _, err := store.BeginRPCRequest(ctx, key, "Mutate", `{}`, expiry); err == nil || !strings.Contains(err.Error(), "unknown idempotency reservation state") {
		t.Fatalf("error=%v, want unknown reservation state", err)
	}

	if _, _, _, execute, err := store.BeginRPCRequest(ctx, "expired", "Mutate", `{}`, time.Now().Add(-time.Minute)); err != nil || !execute {
		t.Fatalf("begin expired execute=%v err=%v", execute, err)
	}
	_, code, _, execute, err = store.BeginRPCRequest(ctx, "expired", "Mutate", `{}`, expiry)
	if err != nil || execute || code != "IDEMPOTENCY_EXPIRED" {
		t.Fatalf("expired replay code=%q execute=%v err=%v", code, execute, err)
	}
}

func TestSessionWorkspaceRejectsEmptyMembershipAndMissingSession(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()

	w := discovery.Workspace{ID: "empty-workspace", Root: "/empty", Kind: "repository"}
	if _, err := store.UpsertWorkspaceGeneration(ctx, w); err != nil {
		t.Fatal(err)
	}
	session := Session{ID: "empty-session", WorkspaceID: string(w.ID), SlotID: "empty-slot", State: "ACTIVE", AgentKind: "codex", TokenHash: HashToken("token")}
	if _, err := store.CreateSlotSession(ctx, Slot{ID: session.SlotID, WorkspaceID: string(w.ID), Path: "/empty/slot", State: "LEASED"}, nil, session, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SessionWorkspace(ctx, session.ID); err == nil || !strings.Contains(err.Error(), "no recorded repository") {
		t.Fatalf("session workspace error=%v", err)
	}
	if _, err := store.SessionWorkspace(ctx, "missing-session"); err == nil {
		t.Fatal("missing session workspace lookup succeeded")
	}
}

func TestResumeOrphanAndBackupNontrivialPaths(t *testing.T) {
	store := openTestStore(t)
	seedWorkspace(t, store)
	ctx := context.Background()
	root := t.TempDir()

	parent := Session{ID: "parent", WorkspaceID: "workspace", SlotID: "parent", State: "ACTIVE", AgentKind: "codex", AgentSessionID: "agent", TokenHash: HashToken("parent-token")}
	if _, err := store.CreateSlotSession(ctx, Slot{ID: "parent", WorkspaceID: "workspace", Generation: 1, Path: filepath.Join(root, "parent"), State: "LEASED"}, nil, parent, ""); err != nil {
		t.Fatal(err)
	}
	if err := store.BindAgentSession(ctx, "parent", "agent"); err != nil {
		t.Fatal(err)
	}
	unbound := Session{ID: "child", SlotID: "child", State: "UNBOUND", AgentKind: "codex", TokenHash: HashToken("child-token")}
	if _, err := store.CreateSlotSession(ctx, Slot{ID: "child", Path: filepath.Join(root, "child"), State: "UNBOUND"}, nil, unbound, ""); err != nil {
		t.Fatal(err)
	}
	repositories := []SlotRepository{{RepositoryID: "repository", WorktreePath: filepath.Join(root, "child", "repo"), RequestedRef: "main", BaseOID: "abc", Fingerprint: "fp"}}
	job, err := store.BindResumeSlot(ctx, "child", "parent", "workspace", "agent", 1, repositories)
	if err != nil || job.Kind != "RESTORE" || job.RepositoryID != "" {
		t.Fatalf("resume job=%+v err=%v", job, err)
	}
	child, err := store.SessionByID(ctx, "child")
	if err != nil || child.State != "RESTORING" || child.ParentSessionID != "parent" {
		t.Fatalf("bound child=%+v err=%v", child, err)
	}
	mapped, err := store.FindByAgentSession(ctx, "codex", "agent")
	if err != nil || mapped.ID != "parent" {
		t.Fatalf("agent mapping moved before restore: mapped=%+v err=%v", mapped, err)
	}
	if err := store.AddRestoringRepositories(ctx, "child", repositories); err != nil {
		t.Fatalf("idempotent restore metadata: %v", err)
	}
	if err := store.AddRestoringRepositories(ctx, "child", append(repositories, SlotRepository{})); err == nil {
		t.Fatal("incomplete restore metadata was accepted")
	}
	if _, err := store.BindResumeSlot(ctx, "child", "parent", "workspace", "agent", 1, nil); err == nil {
		t.Fatal("bound resume slot was rebound")
	}
	if _, _, err := store.FinishPreparationWithRelease(ctx, "child"); err != nil {
		t.Fatalf("finish restore: %v", err)
	}
	mapped, err = store.FindByAgentSession(ctx, "codex", "agent")
	if err != nil || mapped.ID != "child" {
		t.Fatalf("agent mapping not moved after restore: mapped=%+v err=%v", mapped, err)
	}

	orphan := Session{ID: "orphan", WorkspaceID: "workspace", SlotID: "orphan", State: "STARTING", AgentKind: "codex", ClientPID: 999999, TokenHash: HashToken("orphan-token")}
	if _, err := store.CreateSlotSession(ctx, Slot{ID: "orphan", WorkspaceID: "workspace", Generation: 1, Path: filepath.Join(root, "orphan"), State: "LEASED"}, nil, orphan, ""); err != nil {
		t.Fatal(err)
	}
	candidates, err := store.OrphanCandidates(ctx, time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano))
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, candidate := range candidates {
		found = found || candidate.ID == "orphan" && candidate.ClientPID == 999999
	}
	if !found {
		t.Fatalf("orphan candidates=%+v", candidates)
	}
	artifacts, err := store.SlotArtifacts(ctx)
	if err != nil || len(artifacts) != 3 {
		t.Fatalf("slot artifacts=%+v err=%v", artifacts, err)
	}
	registeredRepositories, err := store.Repositories(ctx)
	if err != nil || len(registeredRepositories) != 1 || registeredRepositories[0].ID != "repository" {
		t.Fatalf("repositories=%+v err=%v", registeredRepositories, err)
	}
	backupDir := store.path + ".backups"
	if err := os.MkdirAll(filepath.Join(backupDir, "keep-directory"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(backupDir, "ignore.txt"), []byte("not a backup"), 0o600); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if _, err := store.Backup(ctx, 1, 0); err != nil {
			t.Fatal(err)
		}
		time.Sleep(time.Millisecond)
	}
	backups, err := filepath.Glob(filepath.Join(backupDir, "*.db"))
	if err != nil || len(backups) != 1 {
		t.Fatalf("retained backups=%v err=%v", backups, err)
	}
}

func TestColdLeaseRollsBackAtEveryPersistenceBoundary(t *testing.T) {
	tests := []struct {
		name    string
		trigger string
	}{
		{name: "slot update", trigger: `CREATE TRIGGER fail_cold_slot_update BEFORE UPDATE ON slots BEGIN SELECT RAISE(ABORT,'fault'); END`},
		{name: "repository state update", trigger: `CREATE TRIGGER fail_cold_repository_state BEFORE UPDATE ON slot_repositories BEGIN SELECT RAISE(ABORT,'fault'); END`},
		{name: "session insert", trigger: `CREATE TRIGGER fail_cold_session_insert BEFORE INSERT ON sessions BEGIN SELECT RAISE(ABORT,'fault'); END`},
		{name: "lease timestamp", trigger: `CREATE TRIGGER fail_cold_lease_timestamp BEFORE UPDATE OF last_leased_at ON repositories BEGIN SELECT RAISE(ABORT,'fault'); END`},
		{name: "job insert", trigger: `CREATE TRIGGER fail_cold_job_insert BEFORE INSERT ON jobs BEGIN SELECT RAISE(ABORT,'fault'); END`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := openTestStore(t)
			seedWorkspace(t, store)
			ctx := context.Background()
			root := t.TempDir()
			if _, err := store.CreateStandby(ctx, Slot{ID: "cold", WorkspaceID: "workspace", Generation: 1, Path: filepath.Join(root, "cold"), State: "READY"}, []SlotRepository{{RepositoryID: "repository", WorktreePath: filepath.Join(root, "cold", "root"), State: "COLD"}}); err != nil {
				t.Fatal(err)
			}
			if _, err := store.db.ExecContext(ctx, test.trigger); err != nil {
				t.Fatal(err)
			}
			session := Session{ID: "session", WorkspaceID: "workspace", SlotID: "cold", AgentKind: "codex", TokenHash: HashToken("token")}
			if _, err := store.LeaseReadyWithCold(ctx, "cold", session); err == nil {
				t.Fatal("fault-injected cold lease succeeded")
			}
			slot, err := store.Slot(ctx, "cold")
			if err != nil || slot.State != "READY" || slot.OwnerSessionID != "" {
				t.Fatalf("cold lease did not roll back slot: slot=%+v err=%v", slot, err)
			}
			repository, err := store.SlotRepository(ctx, "cold", "repository")
			if err != nil || repository.State != "COLD" {
				t.Fatalf("cold lease did not roll back repository: repository=%+v err=%v", repository, err)
			}
			if _, err := store.SessionByID(ctx, "session"); !errors.Is(err, sql.ErrNoRows) {
				t.Fatalf("cold lease retained a partial session: %v", err)
			}
		})
	}
}

func TestResumeBindingRollsBackAtEveryPersistenceBoundary(t *testing.T) {
	tests := []struct {
		name    string
		trigger string
	}{
		{name: "session update", trigger: `CREATE TRIGGER fail_resume_session_update BEFORE UPDATE ON sessions BEGIN SELECT RAISE(ABORT,'fault'); END`},
		{name: "slot update", trigger: `CREATE TRIGGER fail_resume_slot_update BEFORE UPDATE ON slots BEGIN SELECT RAISE(ABORT,'fault'); END`},
		{name: "repository insert", trigger: `CREATE TRIGGER fail_resume_repository_insert BEFORE INSERT ON slot_repositories BEGIN SELECT RAISE(ABORT,'fault'); END`},
		{name: "lease timestamp", trigger: `CREATE TRIGGER fail_resume_lease_timestamp BEFORE UPDATE OF last_leased_at ON repositories BEGIN SELECT RAISE(ABORT,'fault'); END`},
		{name: "job insert", trigger: `CREATE TRIGGER fail_resume_job_insert BEFORE INSERT ON jobs BEGIN SELECT RAISE(ABORT,'fault'); END`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := openTestStore(t)
			seedWorkspace(t, store)
			ctx := context.Background()
			root := t.TempDir()
			parent := Session{ID: "parent", WorkspaceID: "workspace", SlotID: "parent", State: "ARCHIVED", AgentKind: "codex", AgentSessionID: "agent", TokenHash: HashToken("parent")}
			if _, err := store.CreateSlotSession(ctx, Slot{ID: "parent", WorkspaceID: "workspace", Generation: 1, Path: filepath.Join(root, "parent"), State: "SNAPSHOTTED"}, nil, parent, ""); err != nil {
				t.Fatal(err)
			}
			if err := store.BindAgentSession(ctx, "parent", "agent"); err != nil {
				t.Fatal(err)
			}
			child := Session{ID: "child", SlotID: "child", State: "UNBOUND", AgentKind: "codex", TokenHash: HashToken("child")}
			if _, err := store.CreateSlotSession(ctx, Slot{ID: "child", Path: filepath.Join(root, "child"), State: "UNBOUND"}, nil, child, ""); err != nil {
				t.Fatal(err)
			}
			if _, err := store.db.ExecContext(ctx, test.trigger); err != nil {
				t.Fatal(err)
			}
			repositories := []SlotRepository{{RepositoryID: "repository", WorktreePath: filepath.Join(root, "child", "root"), State: "RESTORING"}}
			if _, err := store.BindResumeSlot(ctx, "child", "parent", "workspace", "agent", 1, repositories); err == nil {
				t.Fatal("fault-injected resume binding succeeded")
			}
			storedChild, err := store.SessionByID(ctx, "child")
			if err != nil || storedChild.State != "UNBOUND" || storedChild.ParentSessionID != "" {
				t.Fatalf("resume binding did not roll back session: session=%+v err=%v", storedChild, err)
			}
			slot, err := store.Slot(ctx, "child")
			if err != nil || slot.State != "UNBOUND" || slot.WorkspaceID != "" {
				t.Fatalf("resume binding did not roll back slot: slot=%+v err=%v", slot, err)
			}
			if repositories, err := store.SlotRepositories(ctx, "child"); err != nil || len(repositories) != 0 {
				t.Fatalf("resume binding retained repository metadata: repositories=%+v err=%v", repositories, err)
			}
		})
	}
}

func TestRestoreActivationPreservesParentMappingAtEveryHandoffFailure(t *testing.T) {
	tests := []struct {
		name    string
		trigger string
	}{
		{name: "parent mapping clear", trigger: `CREATE TRIGGER fail_parent_mapping_clear BEFORE UPDATE OF agent_session_id ON sessions WHEN OLD.id='parent' BEGIN SELECT RAISE(ABORT,'fault'); END`},
		{name: "child mapping set", trigger: `CREATE TRIGGER fail_child_mapping_set BEFORE UPDATE OF agent_session_id ON sessions WHEN OLD.id='child' BEGIN SELECT RAISE(ABORT,'fault'); END`},
		{name: "activation", trigger: `CREATE TRIGGER fail_restore_activation BEFORE UPDATE OF state ON sessions WHEN NEW.state='ACTIVE' BEGIN SELECT RAISE(ABORT,'fault'); END`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := openTestStore(t)
			seedWorkspace(t, store)
			ctx := context.Background()
			root := t.TempDir()
			parent := Session{ID: "parent", WorkspaceID: "workspace", SlotID: "parent", State: "ARCHIVED", AgentKind: "codex", AgentSessionID: "agent", TokenHash: HashToken("parent")}
			if _, err := store.CreateSlotSession(ctx, Slot{ID: "parent", WorkspaceID: "workspace", Generation: 1, Path: filepath.Join(root, "parent"), State: "SNAPSHOTTED"}, nil, parent, ""); err != nil {
				t.Fatal(err)
			}
			if err := store.BindAgentSession(ctx, "parent", "agent"); err != nil {
				t.Fatal(err)
			}
			child := Session{ID: "child", SlotID: "child", State: "UNBOUND", AgentKind: "codex", TokenHash: HashToken("child")}
			if _, err := store.CreateSlotSession(ctx, Slot{ID: "child", Path: filepath.Join(root, "child"), State: "UNBOUND"}, nil, child, ""); err != nil {
				t.Fatal(err)
			}
			if _, err := store.BindResumeSlot(ctx, "child", "parent", "workspace", "agent", 1, nil); err != nil {
				t.Fatal(err)
			}
			if _, err := store.db.ExecContext(ctx, test.trigger); err != nil {
				t.Fatal(err)
			}
			if _, _, err := store.FinishPreparationWithRelease(ctx, "child"); err == nil {
				t.Fatal("fault-injected restore activation succeeded")
			}
			mapped, err := store.FindByAgentSession(ctx, "codex", "agent")
			if err != nil || mapped.ID != "parent" {
				t.Fatalf("restore handoff lost parent mapping: mapped=%+v err=%v", mapped, err)
			}
			storedChild, err := store.SessionByID(ctx, "child")
			if err != nil || storedChild.State != "RESTORING" || storedChild.AgentSessionID != "" {
				t.Fatalf("restore handoff partially activated child: child=%+v err=%v", storedChild, err)
			}
		})
	}
}

func TestRPCIdempotencyPropagatesReservationAndCompletionStorageFaults(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	expiry := time.Now().Add(time.Hour)
	if _, err := store.db.Exec(`CREATE TRIGGER fail_rpc_reservation BEFORE INSERT ON rpc_idempotency BEGIN SELECT RAISE(ABORT,'reservation fault'); END`); err != nil {
		t.Fatal(err)
	}
	if _, _, _, _, err := store.BeginRPCRequest(ctx, "reservation", "Mutate", `{}`, expiry); err == nil {
		t.Fatal("RPC reservation storage fault was ignored")
	}
	if _, err := store.db.Exec(`DROP TRIGGER fail_rpc_reservation`); err != nil {
		t.Fatal(err)
	}
	if _, _, _, execute, err := store.BeginRPCRequest(ctx, "completion", "Mutate", `{}`, expiry); err != nil || !execute {
		t.Fatalf("begin completion execute=%v err=%v", execute, err)
	}
	if _, err := store.db.Exec(`CREATE TRIGGER fail_rpc_completion BEFORE UPDATE ON rpc_idempotency BEGIN SELECT RAISE(ABORT,'completion fault'); END`); err != nil {
		t.Fatal(err)
	}
	if err := store.CompleteRPCRequest(ctx, "completion", "Mutate", `{}`, nil, "", "", expiry); err == nil {
		t.Fatal("RPC completion storage fault was ignored")
	}
	if _, err := store.db.Exec(`DROP TRIGGER fail_rpc_completion`); err != nil {
		t.Fatal(err)
	}

	paramsHash := rpcParamsHash(`{}`)
	if _, err := store.db.Exec(`INSERT INTO rpc_idempotency(idempotency_key,method,params,result,error_code,error_message,completed_at,expires_at,state) VALUES('unknown','Mutate',?,NULL,NULL,NULL,?,?, 'UNKNOWN')`, paramsHash, now(), FormatTime(expiry)); err != nil {
		t.Fatal(err)
	}
	if _, _, _, _, err := store.BeginRPCRequest(ctx, "unknown", "Mutate", `{}`, expiry); err == nil || !strings.Contains(err.Error(), "unknown idempotency reservation state") {
		t.Fatalf("unknown reservation state error=%v", err)
	}

	blockingBackupDirectory := store.path + ".backups"
	if err := os.WriteFile(blockingBackupDirectory, []byte("file"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Backup(ctx, 1, time.Hour); err == nil {
		t.Fatal("backup succeeded through a regular-file backup directory")
	}
}

func TestJobClaimRollsBackWhenClaimedRowOrAuditEventChanges(t *testing.T) {
	for _, test := range []struct {
		name    string
		trigger string
	}{
		{
			name:    "claimed row disappears",
			trigger: `CREATE TRIGGER remove_claimed_job AFTER UPDATE OF state ON jobs WHEN NEW.state='RUNNING' BEGIN DELETE FROM jobs WHERE id=NEW.id; END`,
		},
		{
			name:    "audit event fails",
			trigger: `CREATE TRIGGER fail_claim_event BEFORE INSERT ON events WHEN NEW.kind='job_started' BEGIN SELECT RAISE(ABORT,'event fault'); END`,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := openTestStore(t)
			job, err := store.CreateJob(context.Background(), "PREPARE", "", "", "")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := store.db.Exec(test.trigger); err != nil {
				t.Fatal(err)
			}
			if _, err := store.ClaimJob(context.Background(), job.ID, "worker"); err == nil {
				t.Fatal("fault-injected job claim succeeded")
			}
			var state string
			if err := store.db.QueryRow(`SELECT state FROM jobs WHERE id=?`, job.ID).Scan(&state); err != nil || state != "PENDING" {
				t.Fatalf("failed claim did not roll back job: state=%q err=%v", state, err)
			}
		})
	}
}

func TestReadySlotsRejectsCorruptGenerationType(t *testing.T) {
	store := openTestStore(t)
	seedWorkspace(t, store)
	job, err := store.CreateStandby(context.Background(), Slot{ID: "ready-corrupt", WorkspaceID: "workspace", Generation: 1, Path: "/ready-corrupt", State: "READY"}, nil)
	if err != nil || job.ID == "" {
		t.Fatalf("create standby job=%+v err=%v", job, err)
	}
	if _, err := store.db.Exec(`UPDATE slots SET generation='not-an-integer' WHERE id='ready-corrupt'`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ReadySlots(context.Background(), "workspace"); err == nil {
		t.Fatal("READY slot with corrupt generation type was decoded")
	}
}

func openTestStore(t *testing.T) *Store {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func seedWorkspace(t *testing.T, store *Store) {
	t.Helper()
	w := discovery.Workspace{ID: "workspace", Root: "/workspace", Kind: "repository", Repositories: []discovery.Repository{{ID: "repository", MainPath: "/workspace", CommonDir: "/workspace/.git", DefaultBranch: "main"}}}
	if _, err := store.UpsertWorkspaceGeneration(context.Background(), w); err != nil {
		t.Fatal(err)
	}
}
