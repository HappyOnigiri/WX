package state

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/HappyOnigiri/WX/internal/discovery"
)

func TestReadModelsRejectRowsWithUnscannableFields(t *testing.T) {
	tests := []struct {
		name  string
		sql   []string
		check func(*Store) error
	}{
		{
			name: "workspace roots",
			sql: []string{
				"DROP TABLE workspaces",
				"CREATE VIEW workspaces AS SELECT NULL AS root_path",
			},
			check: func(store *Store) error {
				_, err := store.WorkspaceRoots(context.Background())
				return err
			},
		},
		{
			name: "slot artifacts",
			sql: []string{
				"DROP TABLE slots",
				"CREATE VIEW slots AS SELECT NULL AS id,NULL AS path,NULL AS state",
			},
			check: func(store *Store) error {
				_, err := store.SlotArtifacts(context.Background())
				return err
			},
		},
		{
			name: "repositories",
			sql: []string{
				"DROP TABLE repositories",
				"CREATE VIEW repositories AS SELECT NULL AS id,NULL AS main_worktree_path,NULL AS common_git_dir,NULL AS default_branch",
			},
			check: func(store *Store) error {
				_, err := store.Repositories(context.Background())
				return err
			},
		},
		{
			name: "snapshots",
			sql: []string{
				"DROP TABLE snapshots",
				"CREATE VIEW snapshots AS SELECT NULL AS id,'session' AS session_id,'repository' AS repository_id,'head' AS head_oid,'head-ref' AS head_recovery_ref,'index' AS index_tree_oid,'tree' AS worktree_snapshot_oid,'tree-ref' AS worktree_recovery_ref,'ARCHIVED' AS status,'created' AS created_at,'expires' AS expires_at",
			},
			check: func(store *Store) error {
				_, err := store.Snapshots(context.Background(), "session")
				return err
			},
		},
		{
			name: "slot repositories",
			sql: []string{
				"DROP TABLE slot_repositories",
				"CREATE VIEW slot_repositories AS SELECT NULL AS repository_id,'/worktree' AS worktree_path,'READY' AS state,'main' AS requested_ref,'head' AS base_oid,'fingerprint' AS prepare_fingerprint",
			},
			check: func(store *Store) error {
				_, err := store.SlotRepositories(context.Background(), "slot")
				return err
			},
		},
		{
			name: "orphan candidates",
			sql: []string{
				"DROP TABLE sessions",
				"CREATE VIEW sessions AS SELECT NULL AS id,NULL AS workspace_id,'slot' AS slot_id,'ACTIVE' AS state,'created' AS created_at,NULL AS last_heartbeat_at,NULL AS client_pid,NULL AS agent_pid",
			},
			check: func(store *Store) error {
				_, err := store.OrphanCandidates(context.Background(), "later")
				return err
			},
		},
		{
			name: "cold repository candidates",
			sql: []string{
				"DROP TABLE slots",
				"CREATE VIEW slots AS SELECT 'slot' AS id,'workspace' AS workspace_id,NULL AS owner_session_id,'READY' AS state",
				"DROP TABLE slot_repositories",
				"CREATE VIEW slot_repositories AS SELECT 'slot' AS slot_id,'repository' AS repository_id,NULL AS worktree_path,'READY' AS state",
				"DROP TABLE repositories",
				"CREATE VIEW repositories AS SELECT 'repository' AS id,NULL AS last_leased_at",
			},
			check: func(store *Store) error {
				_, err := store.ColdRepositoryCandidates(context.Background(), "later")
				return err
			},
		},
		{
			name: "standby GC candidates",
			sql: []string{
				"DROP TABLE slots",
				"CREATE VIEW slots AS SELECT NULL AS id,'workspace' AS workspace_id,'/slot' AS path,'READY' AS state,NULL AS owner_session_id,NULL AS ready_at,'created' AS created_at",
				"DROP TABLE slot_repositories",
				"CREATE VIEW slot_repositories AS SELECT 'slot' AS slot_id,'repository' AS repository_id",
				"DROP TABLE repositories",
				"CREATE VIEW repositories AS SELECT 'repository' AS id,NULL AS last_leased_at",
			},
			check: func(store *Store) error {
				_, err := store.StandbyGCCandidates(context.Background(), "later", 1)
				return err
			},
		},
		{
			name: "expired snapshots",
			sql: []string{
				"DROP TABLE snapshots",
				"CREATE VIEW snapshots AS SELECT NULL AS id,'session' AS session_id,'repository' AS repository_id,'head' AS head_oid,'head-ref' AS head_recovery_ref,'index' AS index_tree_oid,'tree' AS worktree_snapshot_oid,'tree-ref' AS worktree_recovery_ref,'ARCHIVED' AS status,'created' AS created_at,'expired' AS expires_at",
				"DROP TABLE sessions",
				"CREATE VIEW sessions AS SELECT 'session' AS id,'slot' AS slot_id,NULL AS parent_session_id,'ARCHIVED' AS state",
				"DROP TABLE slots",
				"CREATE VIEW slots AS SELECT 'slot' AS id,'ARCHIVED' AS state",
			},
			check: func(store *Store) error {
				_, err := store.ExpiredSnapshots(context.Background(), "later")
				return err
			},
		},
		{
			name: "GC candidates",
			sql: []string{
				"DROP TABLE slots",
				"CREATE VIEW slots AS SELECT 'slot' AS id,'SNAPSHOTTED' AS state,NULL AS path",
				"DROP TABLE sessions",
				"CREATE VIEW sessions AS SELECT 'session' AS id,'slot' AS slot_id,'archived' AS archived_at",
			},
			check: func(store *Store) error {
				_, err := store.GCCandidates(context.Background(), "later")
				return err
			},
		},
		{
			name: "workspace repositories",
			sql: []string{
				"DROP TABLE workspaces",
				"CREATE VIEW workspaces AS SELECT 'workspace' AS id,'/workspace' AS root_path,'repository' AS kind",
				"DROP TABLE workspace_repositories",
				"CREATE VIEW workspace_repositories AS SELECT 'workspace' AS workspace_id,'repository' AS repository_id,'.' AS relative_path,0 AS ordinal",
				"DROP TABLE repositories",
				"CREATE VIEW repositories AS SELECT 'repository' AS id,NULL AS main_worktree_path,'/common' AS common_git_dir,'main' AS default_branch",
			},
			check: func(store *Store) error {
				_, err := store.Workspace(context.Background(), "workspace")
				return err
			},
		},
		{
			name: "session summaries",
			sql: []string{
				"DROP TABLE sessions",
				"CREATE VIEW sessions AS SELECT NULL AS id,NULL AS workspace_id,'ACTIVE' AS state,'codex' AS agent_kind,NULL AS agent_session_id,'created' AS created_at,NULL AS archived_at,NULL AS expires_at",
			},
			check: func(store *Store) error {
				_, err := store.ListSessions(context.Background(), true)
				return err
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := openTestStore(t)
			for _, statement := range test.sql {
				if _, err := store.db.Exec(statement); err != nil {
					t.Fatal(err)
				}
			}
			if err := test.check(store); err == nil {
				t.Fatal("unscannable database row was accepted")
			}
		})
	}
}

func TestSessionWorkspaceRejectsMissingHistoricalMembership(t *testing.T) {
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

func TestStatusDiagnosticsRejectsUnscannableRows(t *testing.T) {
	tests := []struct {
		name string
		sql  []string
	}{
		{
			name: "workspace",
			sql: []string{
				"DROP TABLE workspaces",
				"CREATE VIEW workspaces AS SELECT NULL AS id,'/workspace' AS root_path,1 AS generation",
			},
		},
		{
			name: "session",
			sql: []string{
				"DROP TABLE sessions",
				"CREATE VIEW sessions AS SELECT NULL AS id,'codex' AS agent_kind,'ACTIVE' AS state,'created' AS created_at,'slot' AS slot_id",
			},
		},
		{
			name: "repository",
			sql: []string{
				"DROP TABLE repositories",
				"CREATE VIEW repositories AS SELECT NULL AS id,'/repository' AS main_worktree_path,NULL AS last_leased_at",
			},
		},
		{
			name: "quarantine",
			sql: []string{
				"DROP TABLE slots",
				"CREATE VIEW slots AS SELECT NULL AS id,'/slot' AS path,'QUARANTINED' AS state,NULL AS failure_code,NULL AS workspace_id,NULL AS owner_session_id,NULL AS ready_at,'created' AS created_at",
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := openTestStore(t)
			for _, statement := range test.sql {
				if _, err := store.db.Exec(statement); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := store.StatusDiagnostics(context.Background()); err == nil {
				t.Fatal("unscannable diagnostics row was accepted")
			}
		})
	}
}

func TestClosedStoreOperationsFailClosed(t *testing.T) {
	store := openTestStore(t)
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	requireError := func(name string, err error) {
		t.Helper()
		if err == nil {
			t.Fatalf("closed store operation %s succeeded", name)
		}
	}
	w := discovery.Workspace{
		ID: "workspace", Root: "/workspace", Kind: "repository",
		Repositories: []discovery.Repository{{ID: "repository", MainPath: "/workspace", CommonDir: "/workspace/.git", RelativePath: ".", DefaultBranch: "main"}},
	}
	workspaceID := string(w.ID)
	workspaceRoot := string(w.Root)
	slot := Slot{ID: "slot", WorkspaceID: "workspace", Generation: 1, RootID: testRootID, RelPath: "workspace/slot", State: "READY"}
	session := Session{ID: "session", WorkspaceID: "workspace", SlotID: "slot", State: "ACTIVE", AgentKind: "codex", TokenHash: HashToken("token")}

	_, _, _, _, err := store.BeginRPCRequest(ctx, "key", "method", "{}", time.Now().Add(time.Hour))
	requireError("BeginRPCRequest", err)
	requireError("CompleteRPCRequest", store.CompleteRPCRequest(ctx, "key", "method", "{}", nil, "", "", time.Now().Add(time.Hour)))
	_, err = store.Backup(ctx, 1, time.Hour)
	requireError("Backup", err)
	_, err = store.CanonicalWorkspace(ctx, w)
	requireError("CanonicalWorkspace", err)
	_, _, err = store.UpsertWorkspaceGeneration(ctx, w)
	requireError("UpsertWorkspaceGeneration", err)
	_, err = store.WorkspaceGeneration(ctx, workspaceID)
	requireError("WorkspaceGeneration", err)
	_, err = store.CreateJob(ctx, "PREPARE", workspaceID, slot.ID, session.ID)
	requireError("CreateJob", err)
	_, err = store.ClaimJob(ctx, "job", "owner")
	requireError("ClaimJob", err)
	requireError("RenewJob", store.RenewJob(ctx, "job", "owner"))
	requireError("FinishJob", store.FinishJob(ctx, "job", "owner", nil))
	requireError("RetryJob", store.RetryJob(ctx, "job", "owner", time.Second, "code"))
	requireError("DeferJob", store.DeferJob(ctx, "job", "owner", time.Second, "code"))
	_, err = store.RecoverJobs(ctx, false)
	requireError("RecoverJobs", err)
	_, err = store.EnsureRecoveryJobs(ctx)
	requireError("EnsureRecoveryJobs", err)
	_, _, err = store.ReadySlot(ctx, workspaceID)
	requireError("ReadySlot", err)
	_, err = store.ReadySlots(ctx, workspaceID)
	requireError("ReadySlots", err)
	if store.StandbyCount(ctx, workspaceID) != 0 {
		t.Fatal("closed store reported standby data")
	}
	_, err = store.CreateSlotSession(ctx, slot, nil, session, "PREPARE")
	requireError("CreateSlotSession", err)
	_, err = store.CreateStandby(ctx, slot, nil)
	requireError("CreateStandby", err)
	requireError("LeaseReady", store.LeaseReady(ctx, slot.ID, session))
	_, err = store.LeaseReadyWithCold(ctx, slot.ID, session)
	requireError("LeaseReadyWithCold", err)
	requireError("SetSlotState", store.SetSlotState(ctx, slot.ID, []string{"READY"}, "STALE", "code"))
	requireError("ResetPreparationForRetry", store.ResetPreparationForRetry(ctx, slot.ID))
	requireError("MarkReady", store.MarkReady(ctx, slot.ID))
	_, _, finishPreparationErr := store.FinishPreparationWithRelease(ctx, slot.ID)
	requireError("FinishPreparation", finishPreparationErr)
	_, _, err = store.FinishPreparationWithRelease(ctx, slot.ID)
	requireError("FinishPreparationWithRelease", err)
	requireError("MarkSessionState", store.MarkSessionState(ctx, session.ID, []string{"ACTIVE"}, "RELEASING"))
	_, err = store.Session(ctx, session.ID, "token")
	requireError("Session", err)
	_, err = store.SessionByID(ctx, session.ID)
	requireError("SessionByID", err)
	requireError("RegisterAgentProcess", store.RegisterAgentProcess(ctx, session.ID, "token", 1))
	requireError("BindAgentSession", store.BindAgentSession(ctx, session.ID, "agent"))
	requireError("BindFreshSession", store.BindFreshSession(ctx, session.ID, "parent", "agent"))
	_, err = store.BindFreshResumeSlot(ctx, session.ID, "parent", workspaceID, "agent", 1, nil)
	requireError("BindFreshResumeSlot", err)
	_, err = store.FindByAgentSession(ctx, "codex", "agent")
	requireError("FindByAgentSession", err)
	requireError("Heartbeat", store.Heartbeat(ctx, session.ID, "token"))
	_, err = store.OrphanCandidates(ctx, "before")
	requireError("OrphanCandidates", err)
	_, err = store.BindResumeSlot(ctx, session.ID, "parent", workspaceID, "agent", 1, nil)
	requireError("BindResumeSlot", err)
	_, err = store.SlotRepositories(ctx, slot.ID)
	requireError("SlotRepositories", err)
	requireError("AddRestoringRepositories", store.AddRestoringRepositories(ctx, slot.ID, nil))
	_, err = store.SlotRepository(ctx, slot.ID, "repository")
	requireError("SlotRepository", err)
	requireError("SetSlotRepositoryState", store.SetSlotRepositoryState(ctx, slot.ID, "repository", []string{"READY"}, "COLD"))
	_, err = store.Slot(ctx, slot.ID)
	requireError("Slot", err)
	requireError("SaveSnapshot", store.SaveSnapshot(ctx, Snapshot{ID: "snapshot", SessionID: session.ID, RepositoryID: "repository"}))
	_, err = store.Snapshots(ctx, session.ID)
	requireError("Snapshots", err)
	requireError("SaveWorkspaceSnapshot", store.SaveWorkspaceSnapshot(ctx, WorkspaceSnapshot{SessionID: session.ID}))
	_, _, err = store.WorkspaceSnapshot(ctx, session.ID)
	requireError("WorkspaceSnapshot", err)
	_, err = store.Repository(ctx, "repository")
	requireError("Repository", err)
	_, err = store.Workspace(ctx, workspaceID)
	requireError("Workspace", err)
	_, err = store.WorkspaceByRoot(ctx, workspaceRoot)
	requireError("WorkspaceByRoot", err)
	_, err = store.SessionWorkspace(ctx, session.ID)
	requireError("SessionWorkspace", err)
	_, err = store.WorkspaceRoots(ctx)
	requireError("WorkspaceRoots", err)
	_, err = store.ListSessions(ctx, false)
	requireError("ListSessions", err)
	requireError("ForgetWorkspace", store.ForgetWorkspace(ctx, workspaceRoot))
	_, err = store.Status(ctx)
	requireError("Status", err)
	_, err = store.StatusDiagnostics(ctx)
	requireError("StatusDiagnostics", err)
	requireError("MarkArchived", store.MarkArchived(ctx, session.ID, slot.ID, "expiry"))
	requireError("BeginSnapshot", store.BeginSnapshot(ctx, session.ID, slot.ID))
	_, _, err = store.Release(ctx, session.ID, workspaceID, slot.ID)
	requireError("Release", err)
	_, err = store.SlotArtifacts(ctx)
	requireError("SlotArtifacts", err)
	requireError("QuarantineMissingSlot", store.QuarantineMissingSlot(ctx, slot.ID, "reason"))
	requireError("QuarantineArtifact", store.QuarantineArtifact(ctx, "slot", slot.Path, "reason"))
	requireError("QuarantineMissingRecoveryRef", store.QuarantineMissingRecoveryRef(ctx, "refs/wx/missing"))
	_, err = store.Repositories(ctx)
	requireError("Repositories", err)
	_, err = store.ColdRepositoryCandidates(ctx, "before")
	requireError("ColdRepositoryCandidates", err)
	_, _, err = store.ScheduleColdRepositoryRemoval(ctx, ColdRepositoryCandidate{SlotID: slot.ID, WorkspaceID: workspaceID, RepositoryID: "repository"})
	requireError("ScheduleColdRepositoryRemoval", err)
	requireError("FinishColdRepositoryRemoval", store.FinishColdRepositoryRemoval(ctx, slot.ID, "repository"))
	_, err = store.StandbyGCCandidates(ctx, "before", 1)
	requireError("StandbyGCCandidates", err)
	_, _, err = store.ScheduleRemoval(ctx, slot.ID, session.ID)
	requireError("ScheduleRemoval", err)
	requireError("FinishRemoval", store.FinishRemoval(ctx, slot.ID))
	requireError("PruneRoots", store.PruneRoots(ctx))
	_, err = store.ExpiredSnapshots(ctx, "before")
	requireError("ExpiredSnapshots", err)
	requireError("ExpireSessionSnapshots", store.ExpireSessionSnapshots(ctx, session.ID))
	requireError("PruneMetadata", store.PruneMetadata(ctx, "failed", "events", "tombstones"))
	_, err = store.GCCandidates(ctx, "before")
	requireError("GCCandidates", err)
	requireError("MarkSlotArchived", store.MarkSlotArchived(ctx, slot.ID))
}

func TestMissingLifecycleRowsFailClosed(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	missing := "missing-lifecycle-row"
	session := Session{ID: missing, WorkspaceID: "missing-workspace", SlotID: missing, State: "ACTIVE", AgentKind: "codex", TokenHash: HashToken("token")}
	requireError := func(name string, err error) {
		t.Helper()
		if err == nil {
			t.Errorf("missing-row operation %s succeeded", name)
		}
	}

	if err := store.RegisterAgentProcess(ctx, missing, "token", 0); err == nil {
		t.Error("non-positive agent PID was accepted")
	}

	_, err := store.ClaimJob(ctx, missing, "worker")
	requireError("ClaimJob", err)
	requireError("RenewJob", store.RenewJob(ctx, missing, "worker"))
	requireError("FinishJob", store.FinishJob(ctx, missing, "worker", nil))
	requireError("RetryJob", store.RetryJob(ctx, missing, "worker", time.Second, "retry"))
	requireError("DeferJob", store.DeferJob(ctx, missing, "worker", time.Second, "defer"))
	requireError("LeaseReady", store.LeaseReady(ctx, missing, session))
	_, err = store.LeaseReadyWithCold(ctx, missing, session)
	requireError("LeaseReadyWithCold", err)
	requireError("SetSlotState", store.SetSlotState(ctx, missing, []string{"READY"}, "STALE", "missing"))
	requireError("MarkReady", store.MarkReady(ctx, missing))
	_, _, finishPreparationMissingErr := store.FinishPreparationWithRelease(ctx, missing)
	requireError("FinishPreparation", finishPreparationMissingErr)
	_, _, err = store.FinishPreparationWithRelease(ctx, missing)
	requireError("FinishPreparationWithRelease", err)
	requireError("MarkSessionState", store.MarkSessionState(ctx, missing, []string{"ACTIVE"}, "RELEASING"))
	_, err = store.Session(ctx, missing, "token")
	requireError("Session", err)
	_, err = store.SessionByID(ctx, missing)
	requireError("SessionByID", err)
	if err := store.RegisterAgentProcess(ctx, missing, "token", 1); err == nil {
		t.Error("agent process was registered for a missing session")
	}
	requireError("BindAgentSession", store.BindAgentSession(ctx, missing, "agent"))
	requireError("BindFreshSession", store.BindFreshSession(ctx, missing, "parent", "agent"))
	_, err = store.BindFreshResumeSlot(ctx, missing, "parent", "workspace", "agent", 1, nil)
	requireError("BindFreshResumeSlot", err)
	_, err = store.FindByAgentSession(ctx, "codex", "agent")
	requireError("FindByAgentSession", err)
	requireError("Heartbeat", store.Heartbeat(ctx, missing, "token"))
	_, err = store.BindResumeSlot(ctx, missing, "parent", "workspace", "agent", 1, nil)
	requireError("BindResumeSlot", err)
	_, err = store.SlotRepositories(ctx, missing)
	if err != nil {
		t.Errorf("missing SlotRepositories returned error: %v", err)
	}
	requireError("AddRestoringRepositories", store.AddRestoringRepositories(ctx, missing, nil))
	_, err = store.SlotRepository(ctx, missing, "repository")
	requireError("SlotRepository", err)
	requireError("SetSlotRepositoryState", store.SetSlotRepositoryState(ctx, missing, "repository", []string{"READY"}, "COLD"))
	_, err = store.Slot(ctx, missing)
	requireError("Slot", err)
	_, err = store.Repository(ctx, "repository")
	requireError("Repository", err)
	_, err = store.Workspace(ctx, "workspace")
	requireError("Workspace", err)
	_, err = store.WorkspaceByRoot(ctx, "/missing-workspace")
	requireError("WorkspaceByRoot", err)
	_, err = store.SessionWorkspace(ctx, missing)
	requireError("SessionWorkspace", err)
	requireError("ForgetWorkspace", store.ForgetWorkspace(ctx, "/missing-workspace"))
	requireError("MarkArchived", store.MarkArchived(ctx, missing, missing, "expiry"))
	requireError("BeginSnapshot", store.BeginSnapshot(ctx, missing, missing))
	_, _, err = store.Release(ctx, missing, "workspace", missing)
	requireError("Release", err)
	requireError("QuarantineMissingSlot", store.QuarantineMissingSlot(ctx, missing, "missing"))
	_, _, err = store.ScheduleRemoval(ctx, missing, missing)
	requireError("ScheduleRemoval", err)
	requireError("FinishRemoval", store.FinishRemoval(ctx, missing))
	requireError("ExpireSessionSnapshots", store.ExpireSessionSnapshots(ctx, missing))
	requireError("MarkSlotArchived", store.MarkSlotArchived(ctx, missing))
}

func TestFinishPreparationSchedulesSnapshotAfterRelease(t *testing.T) {
	store := openTestStore(t)
	seedWorkspace(t, store)
	ctx := context.Background()
	session := Session{ID: "releasing", WorkspaceID: "workspace", SlotID: "releasing", State: "RELEASING", AgentKind: "codex", TokenHash: HashToken("token")}
	if _, err := store.CreateSlotSession(ctx, Slot{ID: "releasing", WorkspaceID: "workspace", Generation: 1, RootID: testRootID, RelPath: "workspace/releasing", State: "PREPARING"}, nil, session, ""); err != nil {
		t.Fatal(err)
	}
	job, scheduled, err := store.FinishPreparationWithRelease(ctx, session.SlotID)
	if err != nil || !scheduled || job.Kind != "SNAPSHOT" || job.WorkspaceID != "workspace" {
		t.Fatalf("finish preparation job=%+v scheduled=%v err=%v", job, scheduled, err)
	}
	slot, err := store.Slot(ctx, session.SlotID)
	if err != nil || slot.State != "DRAINING" {
		t.Fatalf("slot after release preparation=%+v err=%v", slot, err)
	}
}

func TestReleaseUnboundSessionSchedulesRemoval(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	session := Session{ID: "unbound", SlotID: "unbound", State: "UNBOUND", AgentKind: "codex", TokenHash: HashToken("token")}
	if _, err := store.CreateSlotSession(ctx, Slot{ID: session.SlotID, State: "UNBOUND", RootID: testRootID, RelPath: filepath.Join("_unbound", session.SlotID)}, nil, session, ""); err != nil {
		t.Fatal(err)
	}
	job, changed, err := store.Release(ctx, session.ID, "", session.SlotID)
	if err != nil || !changed || job.Kind != "REMOVE" {
		t.Fatalf("unbound release job=%+v changed=%v err=%v", job, changed, err)
	}
	storedSession, err := store.SessionByID(ctx, session.ID)
	if err != nil || storedSession.State != "EXPIRED" {
		t.Fatalf("session after unbound release=%+v err=%v", storedSession, err)
	}
	storedSlot, err := store.Slot(ctx, session.SlotID)
	if err != nil || storedSlot.State != "REMOVING" || storedSlot.OwnerSessionID != "" {
		t.Fatalf("slot after unbound release=%+v err=%v", storedSlot, err)
	}
}

func TestQuarantineMissingRecoveryRefQuarantinesDurableMappings(t *testing.T) {
	store := openTestStore(t)
	seedWorkspace(t, store)
	ctx := context.Background()
	session := Session{ID: "recover", WorkspaceID: "workspace", SlotID: "recover", State: "ACTIVE", AgentKind: "codex", TokenHash: HashToken("token")}
	if _, err := store.CreateSlotSession(ctx, Slot{ID: session.SlotID, WorkspaceID: "workspace", Generation: 1, RootID: testRootID, RelPath: filepath.Join("workspace", session.SlotID), State: "LEASED"}, nil, session, ""); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveSnapshot(ctx, Snapshot{ID: "snapshot", SessionID: session.ID, RepositoryID: "repository", HeadOID: "head", HeadRef: "refs/wx/recovery/missing", IndexTreeOID: "index", WorktreeOID: "tree", WorktreeRef: "refs/wx/recovery/worktree", Status: "ARCHIVED", CreatedAt: now(), ExpiresAt: FormatTime(time.Now().Add(time.Hour))}); err != nil {
		t.Fatal(err)
	}
	if err := store.QuarantineMissingRecoveryRef(ctx, "refs/wx/recovery/missing"); err != nil {
		t.Fatal(err)
	}
	var snapshotStatus, sessionState, slotState, failureCode string
	if err := store.db.QueryRowContext(ctx, `SELECT status FROM snapshots WHERE id='snapshot'`).Scan(&snapshotStatus); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRowContext(ctx, `SELECT state FROM sessions WHERE id='recover'`).Scan(&sessionState); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRowContext(ctx, `SELECT state,failure_code FROM slots WHERE id='recover'`).Scan(&slotState, &failureCode); err != nil {
		t.Fatal(err)
	}
	if snapshotStatus != "QUARANTINED" || sessionState != "QUARANTINED" || slotState != "QUARANTINED" || failureCode != "RECOVERY_REF_MISSING" {
		t.Fatalf("quarantine states snapshot=%q session=%q slot=%q code=%q", snapshotStatus, sessionState, slotState, failureCode)
	}
}

func TestRegistryReadModelsReturnCommittedLifecycleRows(t *testing.T) {
	store := openTestStore(t)
	seedWorkspace(t, store)
	ctx := context.Background()
	root := t.TempDir()
	if _, err := store.CreateStandby(ctx, Slot{ID: "ready", WorkspaceID: "workspace", Generation: 1, RootID: testRootID, RelPath: "workspace/ready", State: "READY"}, []SlotRepository{{RepositoryID: "repository", WorktreePath: filepath.Join(root, "ready", "repository"), State: "READY", RequestedRef: "main", BaseOID: "head", Fingerprint: "fingerprint"}}); err != nil {
		t.Fatal(err)
	}
	if workspace, err := store.WorkspaceByRoot(ctx, "/workspace"); err != nil || workspace.ID != "workspace" || len(workspace.Repositories) != 1 {
		t.Fatalf("workspace by root=%+v err=%v", workspace, err)
	}
	if roots, err := store.WorkspaceRoots(ctx); err != nil || len(roots) != 1 || roots[0] != "/workspace" {
		t.Fatalf("workspace roots=%v err=%v", roots, err)
	}
	if ready, found, err := store.ReadySlot(ctx, "workspace"); err != nil || !found || ready.ID != "ready" {
		t.Fatalf("ready slot=%+v found=%v err=%v", ready, found, err)
	}
	if repositories, err := store.SlotRepositories(ctx, "ready"); err != nil || len(repositories) != 1 || repositories[0].RepositoryID != "repository" {
		t.Fatalf("slot repositories=%+v err=%v", repositories, err)
	}
	if artifacts, err := store.SlotArtifacts(ctx); err != nil || len(artifacts) != 1 || artifacts[0].ID != "ready" {
		t.Fatalf("slot artifacts=%+v err=%v", artifacts, err)
	}
	if repositories, err := store.Repositories(ctx); err != nil || len(repositories) != 1 || repositories[0].ID != "repository" {
		t.Fatalf("repositories=%+v err=%v", repositories, err)
	}
	if candidates, err := store.ColdRepositoryCandidates(ctx, FormatTime(time.Now().Add(time.Hour))); err != nil || len(candidates) != 1 || candidates[0].RepositoryID != "repository" {
		t.Fatalf("cold candidates=%+v err=%v", candidates, err)
	}
	if candidates, err := store.StandbyGCCandidates(ctx, now(), 0); err != nil || len(candidates) != 1 || candidates[0].SlotID != "ready" {
		t.Fatalf("standby GC candidates=%+v err=%v", candidates, err)
	}
	if value, err := TokenHex(); err != nil || len(value) != 64 {
		t.Fatalf("token hex=%q err=%v", value, err)
	}

	session := Session{ID: "archived", WorkspaceID: "workspace", SlotID: "archived", State: "ARCHIVED", AgentKind: "codex", TokenHash: HashToken("token")}
	if _, err := store.CreateSlotSession(ctx, Slot{ID: session.SlotID, WorkspaceID: "workspace", Generation: 1, RootID: testRootID, RelPath: filepath.Join("workspace", session.SlotID), State: "ARCHIVED"}, nil, session, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE sessions SET archived_at=?,expires_at=? WHERE id=?`, FormatTime(time.Now().Add(-time.Hour)), FormatTime(time.Now().Add(-time.Minute)), session.ID); err != nil {
		t.Fatal(err)
	}
	snapshot := Snapshot{ID: "archived-snapshot", SessionID: session.ID, RepositoryID: "repository", HeadOID: "head", HeadRef: "refs/wx/recovery/head", IndexTreeOID: "index", WorktreeOID: "tree", WorktreeRef: "refs/wx/recovery/worktree", Status: "ARCHIVED", CreatedAt: now(), ExpiresAt: FormatTime(time.Now().Add(-time.Minute))}
	if err := store.SaveSnapshot(ctx, snapshot); err != nil {
		t.Fatal(err)
	}
	if snapshots, err := store.Snapshots(ctx, session.ID); err != nil || len(snapshots) != 1 || snapshots[0].ID != snapshot.ID {
		t.Fatalf("snapshots=%+v err=%v", snapshots, err)
	}
	if expired, err := store.ExpiredSnapshots(ctx, FormatTime(time.Now())); err != nil || len(expired) != 1 || expired[0].ID != snapshot.ID {
		t.Fatalf("expired snapshots=%+v err=%v", expired, err)
	}
}

func TestLeaseReadyWithColdPromotesRepositoriesAndStartsPreparation(t *testing.T) {
	store := openTestStore(t)
	seedWorkspace(t, store)
	ctx := context.Background()
	root := t.TempDir()
	if _, err := store.CreateStandby(ctx, Slot{ID: "cold-slot", WorkspaceID: "workspace", Generation: 1, RootID: testRootID, RelPath: "workspace/cold-slot", State: "READY"}, []SlotRepository{{RepositoryID: "repository", WorktreePath: filepath.Join(root, "cold-slot", "repository"), State: "COLD", RequestedRef: "main", BaseOID: "head", Fingerprint: "fingerprint"}}); err != nil {
		t.Fatal(err)
	}
	session := Session{ID: "cold-session", WorkspaceID: "workspace", SlotID: "cold-slot", State: "ACTIVE", AgentKind: "codex", TokenHash: HashToken("token")}
	job, err := store.LeaseReadyWithCold(ctx, "cold-slot", session)
	if err != nil || job.Kind != "PREPARE" || job.SessionID != session.ID {
		t.Fatalf("cold lease job=%+v err=%v", job, err)
	}
	slot, err := store.Slot(ctx, "cold-slot")
	if err != nil || slot.State != "PREPARING" || slot.OwnerSessionID != session.ID {
		t.Fatalf("cold lease slot=%+v err=%v", slot, err)
	}
	repository, err := store.SlotRepository(ctx, "cold-slot", "repository")
	if err != nil || repository.State != "PREPARING" {
		t.Fatalf("cold lease repository=%+v err=%v", repository, err)
	}
	started, err := store.SessionByID(ctx, session.ID)
	if err != nil || started.State != "STARTING" {
		t.Fatalf("cold lease session=%+v err=%v", started, err)
	}
	rows, err := store.db.QueryContext(ctx, `SELECT repository_id,relative_path,ordinal FROM session_repositories WHERE session_id=?`, session.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	if !rows.Next() {
		t.Fatal("cold lease did not preserve current repository membership")
	}
	var repositoryID, relativePath string
	var ordinal int
	if err := rows.Scan(&repositoryID, &relativePath, &ordinal); err != nil {
		t.Fatal(err)
	}
	if repositoryID != "repository" || relativePath != "" || ordinal != 0 {
		t.Fatalf("cold lease membership=%q %q %d", repositoryID, relativePath, ordinal)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
}

func TestStatePersistenceFaultsRemainFailClosed(t *testing.T) {
	t.Run("restore membership copy", func(t *testing.T) {
		store := openTestStore(t)
		ctx := context.Background()
		parent := Session{ID: "parent", SlotID: "parent", State: "ARCHIVED", AgentKind: "codex", TokenHash: HashToken("parent")}
		if _, err := store.CreateSlotSession(ctx, Slot{ID: "parent", State: "SNAPSHOTTED", RootID: testRootID, RelPath: "_unbound/parent"}, nil, parent, ""); err != nil {
			t.Fatal(err)
		}
		if _, err := store.db.Exec(`DROP TABLE session_repositories`); err != nil {
			t.Fatal(err)
		}
		child := Session{ID: "child", SlotID: "child", ParentSessionID: parent.ID, State: "RESTORING", AgentKind: "codex", TokenHash: HashToken("child")}
		if _, err := store.CreateSlotSession(ctx, Slot{ID: "child", State: "RESTORING", RootID: testRootID, RelPath: "_unbound/child"}, nil, child, "RESTORE"); err == nil {
			t.Fatal("restore session succeeded without session repository storage")
		}
	})

	t.Run("current membership copy", func(t *testing.T) {
		store := openTestStore(t)
		seedWorkspace(t, store)
		if _, err := store.db.Exec(`DROP TABLE session_repositories`); err != nil {
			t.Fatal(err)
		}
		session := Session{ID: "current", WorkspaceID: "workspace", SlotID: "current", State: "STARTING", AgentKind: "codex", TokenHash: HashToken("current")}
		if _, err := store.CreateSlotSession(context.Background(), Slot{ID: "current", WorkspaceID: "workspace", State: "PREPARING", RootID: testRootID, RelPath: "workspace/current"}, nil, session, ""); err == nil {
			t.Fatal("session succeeded without current membership storage")
		}
	})

	t.Run("ready lease membership", func(t *testing.T) {
		store := openTestStore(t)
		seedWorkspace(t, store)
		ctx := context.Background()
		if _, err := store.CreateStandby(ctx, Slot{ID: "ready-lease", WorkspaceID: "workspace", Generation: 1, RootID: testRootID, RelPath: "workspace/ready-lease", State: "READY"}, []SlotRepository{{RepositoryID: "repository", WorktreePath: "/wx/ready-lease/repository", State: "READY"}}); err != nil {
			t.Fatal(err)
		}
		if _, err := store.db.Exec(`DROP TABLE session_repositories`); err != nil {
			t.Fatal(err)
		}
		session := Session{ID: "ready-session", WorkspaceID: "workspace", SlotID: "ready-lease", State: "ACTIVE", AgentKind: "codex", TokenHash: HashToken("ready")}
		if err := store.LeaseReady(ctx, "ready-lease", session); err == nil {
			t.Fatal("ready lease succeeded without membership storage")
		}
	})

	t.Run("cold lease membership", func(t *testing.T) {
		store := openTestStore(t)
		seedWorkspace(t, store)
		ctx := context.Background()
		if _, err := store.CreateStandby(ctx, Slot{ID: "cold-lease", WorkspaceID: "workspace", Generation: 1, RootID: testRootID, RelPath: "workspace/cold-lease", State: "READY"}, []SlotRepository{{RepositoryID: "repository", WorktreePath: "/wx/cold-lease/repository", State: "COLD"}}); err != nil {
			t.Fatal(err)
		}
		if _, err := store.db.Exec(`DROP TABLE session_repositories`); err != nil {
			t.Fatal(err)
		}
		session := Session{ID: "cold-session-fault", WorkspaceID: "workspace", SlotID: "cold-lease", State: "ACTIVE", AgentKind: "codex", TokenHash: HashToken("cold")}
		if _, err := store.LeaseReadyWithCold(ctx, "cold-lease", session); err == nil {
			t.Fatal("cold lease succeeded without membership storage")
		}
	})

	t.Run("retry slot update", func(t *testing.T) {
		store := openTestStore(t)
		seedWorkspace(t, store)
		if _, err := store.CreateStandby(context.Background(), Slot{ID: "retry", WorkspaceID: "workspace", Generation: 1, RootID: testRootID, RelPath: "workspace/retry", State: "FAILED"}, []SlotRepository{{RepositoryID: "repository", WorktreePath: "/wx/retry/repository", State: "PREPARE_RUNNING"}}); err != nil {
			t.Fatal(err)
		}
		if _, err := store.db.Exec(`CREATE TRIGGER fail_retry_slot BEFORE UPDATE OF state ON slots WHEN OLD.id='retry' BEGIN SELECT RAISE(ABORT,'fault'); END`); err != nil {
			t.Fatal(err)
		}
		if err := store.ResetPreparationForRetry(context.Background(), "retry"); err == nil {
			t.Fatal("retry succeeded despite slot transition fault")
		}
	})

	t.Run("retry repository update", func(t *testing.T) {
		store := openTestStore(t)
		seedWorkspace(t, store)
		if _, err := store.CreateStandby(context.Background(), Slot{ID: "retry", WorkspaceID: "workspace", Generation: 1, RootID: testRootID, RelPath: "workspace/retry", State: "FAILED"}, []SlotRepository{{RepositoryID: "repository", WorktreePath: "/wx/retry/repository", State: "PREPARE_RUNNING"}}); err != nil {
			t.Fatal(err)
		}
		if _, err := store.db.Exec(`DROP TABLE slot_repositories`); err != nil {
			t.Fatal(err)
		}
		if err := store.ResetPreparationForRetry(context.Background(), "retry"); err == nil {
			t.Fatal("retry succeeded without repository metadata")
		}
	})

	t.Run("finish preparation slot transition", func(t *testing.T) {
		store := openTestStore(t)
		seedWorkspace(t, store)
		ctx := context.Background()
		session := Session{ID: "preparing", WorkspaceID: "workspace", SlotID: "preparing", State: "STARTING", AgentKind: "codex", TokenHash: HashToken("preparing")}
		if _, err := store.CreateSlotSession(ctx, Slot{ID: "preparing", WorkspaceID: "workspace", Generation: 1, RootID: testRootID, RelPath: "workspace/preparing", State: "PREPARING"}, nil, session, ""); err != nil {
			t.Fatal(err)
		}
		if _, err := store.db.Exec(`CREATE TRIGGER fail_finish_slot BEFORE UPDATE OF state ON slots WHEN OLD.id='preparing' BEGIN SELECT RAISE(ABORT,'fault'); END`); err != nil {
			t.Fatal(err)
		}
		if _, _, err := store.FinishPreparationWithRelease(ctx, "preparing"); err == nil {
			t.Fatal("finish preparation succeeded despite slot transition fault")
		}
	})

	t.Run("finish preparation stale slot", func(t *testing.T) {
		store := openTestStore(t)
		seedWorkspace(t, store)
		ctx := context.Background()
		session := Session{ID: "stale-preparing", WorkspaceID: "workspace", SlotID: "stale-preparing", State: "STARTING", AgentKind: "codex", TokenHash: HashToken("stale")}
		if _, err := store.CreateSlotSession(ctx, Slot{ID: "stale-preparing", WorkspaceID: "workspace", Generation: 1, RootID: testRootID, RelPath: "workspace/stale-preparing", State: "FAILED"}, nil, session, ""); err != nil {
			t.Fatal(err)
		}
		if _, _, err := store.FinishPreparationWithRelease(ctx, "stale-preparing"); err == nil {
			t.Fatal("finish preparation accepted a non-preparing slot")
		}
	})

	t.Run("finish restore pending mapping", func(t *testing.T) {
		store := openTestStore(t)
		seedWorkspace(t, store)
		ctx := context.Background()
		parent := Session{ID: "restore-parent", WorkspaceID: "workspace", SlotID: "restore-parent", State: "EXPIRED", AgentKind: "codex", AgentSessionID: "agent", TokenHash: HashToken("parent")}
		if _, err := store.CreateSlotSession(ctx, Slot{ID: parent.ID, WorkspaceID: "workspace", Generation: 1, RootID: testRootID, RelPath: filepath.Join("workspace", parent.ID), State: "SNAPSHOTTED"}, nil, parent, ""); err != nil {
			t.Fatal(err)
		}
		if _, err := store.db.Exec(`UPDATE sessions SET agent_session_id='agent' WHERE id=?`, parent.ID); err != nil {
			t.Fatal(err)
		}
		child := Session{ID: "restore-child", WorkspaceID: "workspace", SlotID: "restore-child", ParentSessionID: parent.ID, State: "RESTORING", AgentKind: "codex", TokenHash: HashToken("child")}
		if _, err := store.CreateSlotSession(ctx, Slot{ID: child.ID, WorkspaceID: "workspace", Generation: 1, RootID: testRootID, RelPath: filepath.Join("workspace", child.ID), State: "RESTORING"}, nil, child, ""); err != nil {
			t.Fatal(err)
		}
		if _, err := store.db.Exec(`UPDATE sessions SET pending_agent_session_id='agent' WHERE id='restore-child'`); err != nil {
			t.Fatal(err)
		}
		if _, err := store.db.Exec(`CREATE TRIGGER ignore_restore_activation BEFORE UPDATE ON sessions WHEN OLD.id='restore-child' AND NEW.pending_agent_session_id IS NULL BEGIN SELECT RAISE(IGNORE); END`); err != nil {
			t.Fatal(err)
		}
		if _, _, err := store.FinishPreparationWithRelease(ctx, child.SlotID); err == nil {
			t.Fatal("restore activation succeeded after its mapping changed")
		}
	})

	t.Run("finish release job insert", func(t *testing.T) {
		store := openTestStore(t)
		seedWorkspace(t, store)
		ctx := context.Background()
		session := Session{ID: "release-finish", WorkspaceID: "workspace", SlotID: "release-finish", State: "RELEASING", AgentKind: "codex", TokenHash: HashToken("release")}
		if _, err := store.CreateSlotSession(ctx, Slot{ID: session.SlotID, WorkspaceID: "workspace", Generation: 1, RootID: testRootID, RelPath: filepath.Join("workspace", session.SlotID), State: "PREPARING"}, nil, session, ""); err != nil {
			t.Fatal(err)
		}
		if _, err := store.db.Exec(`CREATE TRIGGER fail_finish_job BEFORE INSERT ON jobs WHEN NEW.kind='SNAPSHOT' BEGIN SELECT RAISE(ABORT,'fault'); END`); err != nil {
			t.Fatal(err)
		}
		if _, _, err := store.FinishPreparationWithRelease(ctx, session.SlotID); err == nil {
			t.Fatal("release finish succeeded despite snapshot job fault")
		}
	})

	t.Run("release job insert", func(t *testing.T) {
		store := openTestStore(t)
		seedWorkspace(t, store)
		ctx := context.Background()
		session := Session{ID: "release-job", WorkspaceID: "workspace", SlotID: "release-job", State: "ACTIVE", AgentKind: "codex", TokenHash: HashToken("release")}
		if _, err := store.CreateSlotSession(ctx, Slot{ID: session.SlotID, WorkspaceID: "workspace", Generation: 1, RootID: testRootID, RelPath: filepath.Join("workspace", session.SlotID), State: "LEASED"}, nil, session, ""); err != nil {
			t.Fatal(err)
		}
		if _, err := store.db.Exec(`CREATE TRIGGER fail_release_job BEFORE INSERT ON jobs WHEN NEW.kind='SNAPSHOT' BEGIN SELECT RAISE(ABORT,'fault'); END`); err != nil {
			t.Fatal(err)
		}
		if _, _, err := store.Release(ctx, session.ID, session.WorkspaceID, session.SlotID); err == nil {
			t.Fatal("release succeeded despite snapshot job fault")
		}
	})

	t.Run("agent parent mapping update", func(t *testing.T) {
		store := openTestStore(t)
		ctx := context.Background()
		parent := Session{ID: "agent-parent", SlotID: "agent-parent", State: "EXPIRED", AgentKind: "codex", AgentSessionID: "agent", TokenHash: HashToken("parent")}
		if _, err := store.CreateSlotSession(ctx, Slot{ID: parent.ID, State: "SNAPSHOTTED", RootID: testRootID, RelPath: filepath.Join("_unbound", parent.ID)}, nil, parent, ""); err != nil {
			t.Fatal(err)
		}
		if _, err := store.db.Exec(`UPDATE sessions SET agent_session_id='agent' WHERE id=?`, parent.ID); err != nil {
			t.Fatal(err)
		}
		child := Session{ID: "agent-child", SlotID: "agent-child", ParentSessionID: parent.ID, State: "STARTING", AgentKind: "codex", TokenHash: HashToken("child")}
		if _, err := store.CreateSlotSession(ctx, Slot{ID: child.ID, State: "UNBOUND", RootID: testRootID, RelPath: filepath.Join("_unbound", child.ID)}, nil, child, ""); err != nil {
			t.Fatal(err)
		}
		if _, err := store.db.Exec(`CREATE TRIGGER fail_agent_parent BEFORE UPDATE ON sessions WHEN OLD.id='agent-parent' BEGIN SELECT RAISE(ABORT,'fault'); END`); err != nil {
			t.Fatal(err)
		}
		if err := store.BindAgentSession(ctx, child.ID, "agent"); err == nil {
			t.Fatal("agent binding succeeded despite parent mapping fault")
		}
	})

	t.Run("agent session update", func(t *testing.T) {
		store := openTestStore(t)
		ctx := context.Background()
		session := Session{ID: "agent-current", SlotID: "agent-current", State: "STARTING", AgentKind: "codex", TokenHash: HashToken("current")}
		if _, err := store.CreateSlotSession(ctx, Slot{ID: session.ID, State: "UNBOUND", RootID: testRootID, RelPath: filepath.Join("_unbound", session.ID)}, nil, session, ""); err != nil {
			t.Fatal(err)
		}
		if _, err := store.db.Exec(`CREATE TRIGGER fail_agent_current BEFORE UPDATE ON sessions WHEN OLD.id='agent-current' BEGIN SELECT RAISE(ABORT,'fault'); END`); err != nil {
			t.Fatal(err)
		}
		if err := store.BindAgentSession(ctx, session.ID, "agent"); err == nil {
			t.Fatal("agent binding succeeded despite session mapping fault")
		}
	})

	t.Run("heartbeat update", func(t *testing.T) {
		store := openTestStore(t)
		ctx := context.Background()
		session := Session{ID: "heartbeat", SlotID: "heartbeat", State: "STARTING", AgentKind: "codex", TokenHash: HashToken("token")}
		if _, err := store.CreateSlotSession(ctx, Slot{ID: session.ID, State: "UNBOUND", RootID: testRootID, RelPath: filepath.Join("_unbound", session.ID)}, nil, session, ""); err != nil {
			t.Fatal(err)
		}
		if _, err := store.db.Exec(`CREATE TRIGGER fail_heartbeat BEFORE UPDATE ON sessions WHEN OLD.id='heartbeat' BEGIN SELECT RAISE(ABORT,'fault'); END`); err != nil {
			t.Fatal(err)
		}
		if err := store.Heartbeat(ctx, session.ID, "token"); err == nil {
			t.Fatal("heartbeat succeeded despite durable update fault")
		}
	})
}

func TestStateLifecycleFaultsRemainFailClosed(t *testing.T) {
	newFreshResumeState := func(t *testing.T) (*Store, context.Context) {
		t.Helper()
		store := openTestStore(t)
		seedWorkspace(t, store)
		ctx := context.Background()
		parent := Session{ID: "resume-parent", WorkspaceID: "workspace", SlotID: "resume-parent", State: "EXPIRED", AgentKind: "codex", TokenHash: HashToken("parent")}
		if _, err := store.CreateSlotSession(ctx, Slot{ID: parent.ID, WorkspaceID: "workspace", Generation: 1, RootID: testRootID, RelPath: filepath.Join("workspace", parent.ID), State: "SNAPSHOTTED"}, nil, parent, ""); err != nil {
			t.Fatal(err)
		}
		if _, err := store.db.Exec(`UPDATE sessions SET agent_session_id='agent' WHERE id=?`, parent.ID); err != nil {
			t.Fatal(err)
		}
		current := Session{ID: "resume-current", SlotID: "resume-current", State: "UNBOUND", AgentKind: "codex", TokenHash: HashToken("current")}
		if _, err := store.CreateSlotSession(ctx, Slot{ID: current.ID, State: "UNBOUND", RootID: testRootID, RelPath: filepath.Join("_unbound", current.ID)}, nil, current, ""); err != nil {
			t.Fatal(err)
		}
		return store, ctx
	}

	t.Run("fresh resume parent update", func(t *testing.T) {
		store, ctx := newFreshResumeState(t)
		if _, err := store.db.Exec(`CREATE TRIGGER fail_fresh_parent BEFORE UPDATE ON sessions WHEN OLD.id='resume-parent' BEGIN SELECT RAISE(ABORT,'fault'); END`); err != nil {
			t.Fatal(err)
		}
		if _, err := store.BindFreshResumeSlot(ctx, "resume-current", "resume-parent", "workspace", "agent", 1, nil); err == nil {
			t.Fatal("fresh resume succeeded despite parent mapping fault")
		}
	})

	t.Run("fresh resume session update", func(t *testing.T) {
		store, ctx := newFreshResumeState(t)
		if _, err := store.db.Exec(`CREATE TRIGGER fail_fresh_session BEFORE UPDATE ON sessions WHEN OLD.id='resume-current' BEGIN SELECT RAISE(ABORT,'fault'); END`); err != nil {
			t.Fatal(err)
		}
		if _, err := store.BindFreshResumeSlot(ctx, "resume-current", "resume-parent", "workspace", "agent", 1, nil); err == nil {
			t.Fatal("fresh resume succeeded despite session mapping fault")
		}
	})

	t.Run("fresh resume slot update", func(t *testing.T) {
		store, ctx := newFreshResumeState(t)
		if _, err := store.db.Exec(`CREATE TRIGGER fail_fresh_slot BEFORE UPDATE ON slots WHEN OLD.id='resume-current' BEGIN SELECT RAISE(ABORT,'fault'); END`); err != nil {
			t.Fatal(err)
		}
		if _, err := store.BindFreshResumeSlot(ctx, "resume-current", "resume-parent", "workspace", "agent", 1, nil); err == nil {
			t.Fatal("fresh resume succeeded despite slot transition fault")
		}
	})

	t.Run("fresh resume membership", func(t *testing.T) {
		store, ctx := newFreshResumeState(t)
		if _, err := store.db.Exec(`DROP TABLE session_repositories`); err != nil {
			t.Fatal(err)
		}
		if _, err := store.BindFreshResumeSlot(ctx, "resume-current", "resume-parent", "workspace", "agent", 1, nil); err == nil {
			t.Fatal("fresh resume succeeded without membership storage")
		}
	})

	t.Run("resume membership copy", func(t *testing.T) {
		store, ctx := newFreshResumeState(t)
		if _, err := store.db.Exec(`DROP TABLE session_repositories`); err != nil {
			t.Fatal(err)
		}
		if _, err := store.BindResumeSlot(ctx, "resume-current", "resume-parent", "workspace", "agent", 1, nil); err == nil {
			t.Fatal("resume succeeded without membership storage")
		}
	})

	t.Run("restoring repository count", func(t *testing.T) {
		store := openTestStore(t)
		ctx := context.Background()
		if _, err := store.CreateSlotSession(ctx, Slot{ID: "restoring", State: "RESTORING", RootID: testRootID, RelPath: "_unbound/restoring"}, nil, Session{ID: "restoring", SlotID: "restoring", State: "RESTORING", AgentKind: "codex", TokenHash: HashToken("restoring")}, ""); err != nil {
			t.Fatal(err)
		}
		if _, err := store.db.Exec(`DROP TABLE slot_repositories`); err != nil {
			t.Fatal(err)
		}
		if err := store.AddRestoringRepositories(ctx, "restoring", []SlotRepository{{RepositoryID: "missing", WorktreePath: "/wx/restoring/missing"}}); err == nil {
			t.Fatal("restoring repository metadata succeeded without storage")
		}
	})

	t.Run("restoring repository insert", func(t *testing.T) {
		store := openTestStore(t)
		ctx := context.Background()
		if _, err := store.CreateSlotSession(ctx, Slot{ID: "restoring", State: "RESTORING", RootID: testRootID, RelPath: "_unbound/restoring"}, nil, Session{ID: "restoring", SlotID: "restoring", State: "RESTORING", AgentKind: "codex", TokenHash: HashToken("restoring")}, ""); err != nil {
			t.Fatal(err)
		}
		if _, err := store.db.Exec(`CREATE TRIGGER fail_restoring_repo BEFORE INSERT ON slot_repositories BEGIN SELECT RAISE(ABORT,'fault'); END`); err != nil {
			t.Fatal(err)
		}
		if err := store.AddRestoringRepositories(ctx, "restoring", []SlotRepository{{RepositoryID: "missing", WorktreePath: "/wx/restoring/missing"}}); err == nil {
			t.Fatal("restoring repository metadata succeeded despite insert fault")
		}
	})

	t.Run("snapshot metadata read", func(t *testing.T) {
		store := openTestStore(t)
		ctx := context.Background()
		seedWorkspace(t, store)
		session := Session{ID: "snapshot-read", WorkspaceID: "workspace", SlotID: "snapshot-read", State: "ACTIVE", AgentKind: "codex", TokenHash: HashToken("snapshot")}
		if _, err := store.CreateSlotSession(ctx, Slot{ID: session.SlotID, WorkspaceID: "workspace", Generation: 1, RootID: testRootID, RelPath: filepath.Join("workspace", session.SlotID), State: "LEASED"}, nil, session, ""); err != nil {
			t.Fatal(err)
		}
		if _, err := store.db.Exec(`CREATE TRIGGER ignore_snapshot_insert BEFORE INSERT ON snapshots BEGIN SELECT RAISE(IGNORE); END`); err != nil {
			t.Fatal(err)
		}
		snapshot := Snapshot{ID: "snapshot-read", SessionID: session.ID, RepositoryID: "repository", HeadOID: "head", HeadRef: "refs/wx/head", IndexTreeOID: "index", WorktreeOID: "tree", WorktreeRef: "refs/wx/tree", Status: "ARCHIVED", CreatedAt: now(), ExpiresAt: now()}
		if err := store.SaveSnapshot(ctx, snapshot); err == nil {
			t.Fatal("snapshot save accepted an ignored durable insert")
		}
	})

	t.Run("workspace snapshot metadata read", func(t *testing.T) {
		store := openTestStore(t)
		ctx := context.Background()
		if _, err := store.CreateSlotSession(ctx, Slot{ID: "workspace-snapshot", State: "ARCHIVED", RootID: testRootID, RelPath: "_unbound/workspace-snapshot"}, nil, Session{ID: "workspace-snapshot", SlotID: "workspace-snapshot", State: "EXPIRED", AgentKind: "codex", TokenHash: HashToken("snapshot")}, ""); err != nil {
			t.Fatal(err)
		}
		if _, err := store.db.Exec(`CREATE TRIGGER ignore_workspace_snapshot_insert BEFORE INSERT ON workspace_snapshots BEGIN SELECT RAISE(IGNORE); END`); err != nil {
			t.Fatal(err)
		}
		snapshot := WorkspaceSnapshot{SessionID: "workspace-snapshot", RootID: testRootID, RelPath: "_recovery/workspace-snapshots/snapshot.tar", SHA256: strings.Repeat("a", 64), Status: "ARCHIVED", CreatedAt: now(), ExpiresAt: now()}
		if err := store.SaveWorkspaceSnapshot(ctx, snapshot); err == nil {
			t.Fatal("workspace snapshot save accepted an ignored durable insert")
		}
	})
}

func TestForgetWorkspaceStopsAtEveryDurableBoundary(t *testing.T) {
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

func TestQuarantineRecoveryRefStopsAtDurableBoundaries(t *testing.T) {
	t.Run("query", func(t *testing.T) {
		store := openTestStore(t)
		if _, err := store.db.Exec(`DROP TABLE snapshots`); err != nil {
			t.Fatal(err)
		}
		if err := store.QuarantineMissingRecoveryRef(context.Background(), "refs/wx/missing"); err == nil {
			t.Fatal("quarantine succeeded without snapshot storage")
		}
	})

	store := openTestStore(t)
	seedWorkspace(t, store)
	ctx := context.Background()
	session := Session{ID: "quarantine", WorkspaceID: "workspace", SlotID: "quarantine", State: "ACTIVE", AgentKind: "codex", TokenHash: HashToken("quarantine")}
	if _, err := store.CreateSlotSession(ctx, Slot{ID: session.SlotID, WorkspaceID: "workspace", Generation: 1, RootID: testRootID, RelPath: filepath.Join("workspace", session.SlotID), State: "LEASED"}, nil, session, ""); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveSnapshot(ctx, Snapshot{ID: "quarantine-snapshot", SessionID: session.ID, RepositoryID: "repository", HeadOID: "head", HeadRef: "refs/wx/missing", IndexTreeOID: "index", WorktreeOID: "tree", WorktreeRef: "refs/wx/tree", Status: "ARCHIVED", CreatedAt: now(), ExpiresAt: now()}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`CREATE TRIGGER fail_quarantine_snapshot BEFORE UPDATE ON snapshots BEGIN SELECT RAISE(ABORT,'fault'); END`); err != nil {
		t.Fatal(err)
	}
	if err := store.QuarantineMissingRecoveryRef(ctx, "refs/wx/missing"); err == nil {
		t.Fatal("quarantine succeeded despite snapshot update fault")
	}
}

func TestStatusDiagnosticsAndGarbageCollectionCandidatesExposeRows(t *testing.T) {
	store := openTestStore(t)
	seedWorkspace(t, store)
	ctx := context.Background()
	root := t.TempDir()
	session := Session{ID: "snapshot", WorkspaceID: "workspace", SlotID: "snapshot", State: "ARCHIVED", AgentKind: "codex", TokenHash: HashToken("token")}
	if _, err := store.CreateSlotSession(ctx, Slot{ID: "snapshot", WorkspaceID: "workspace", Generation: 1, RootID: testRootID, RelPath: "workspace/snapshot", State: "SNAPSHOTTED"}, nil, session, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE sessions SET archived_at=? WHERE id=?`, FormatTime(time.Now().Add(-time.Hour)), session.ID); err != nil {
		t.Fatal(err)
	}
	job, err := store.CreateJob(ctx, "PREPARE", "workspace", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.QuarantineArtifact(ctx, "test", filepath.Join(root, "artifact"), "TEST"); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveSnapshot(ctx, Snapshot{ID: "snapshot-row", SessionID: session.ID, RepositoryID: "repository", HeadOID: "head", HeadRef: "refs/wx/recovery/head", IndexTreeOID: "index", WorktreeOID: "tree", WorktreeRef: "refs/wx/recovery/worktree", Status: "ARCHIVED", CreatedAt: now(), ExpiresAt: FormatTime(time.Now().Add(time.Hour))}); err != nil {
		t.Fatal(err)
	}
	diagnostics, err := store.StatusDiagnostics(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(diagnostics.Workspaces) != 1 || len(diagnostics.Sessions) != 1 || len(diagnostics.Repositories) != 1 || diagnostics.Jobs.Pending < 1 || diagnostics.Snapshots.Count != 1 || len(diagnostics.Quarantine) != 1 {
		t.Fatalf("diagnostics=%+v", diagnostics)
	}
	if jobs, err := store.RecoverJobs(ctx, true); err != nil || len(jobs) == 0 || jobs[0].ID != job.ID {
		t.Fatalf("reclaimed jobs=%+v err=%v", jobs, err)
	}
	candidates, err := store.GCCandidates(ctx, FormatTime(time.Now().Add(time.Hour)))
	if err != nil || len(candidates) != 1 || candidates[0].SlotID != "snapshot" || candidates[0].SessionID != session.ID {
		t.Fatalf("GC candidates=%+v err=%v", candidates, err)
	}
}

func TestStoreMutationsFailClosedWhenContextIsCanceled(t *testing.T) {
	store := openTestStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	workspace := discovery.Workspace{ID: "canceled", Root: "/canceled", Kind: "repository", Repositories: []discovery.Repository{{ID: "repository", CommonDir: "/canceled/repository.git"}}}
	session := Session{ID: "canceled", WorkspaceID: "canceled", SlotID: "canceled", State: "STARTING", AgentKind: "codex"}
	job := Job{ID: "canceled", Kind: "PREPARE", State: "PENDING"}
	operations := map[string]func() error{
		"ping": func() error { return store.Ping(ctx) },
		"begin RPC": func() error {
			_, _, _, _, err := store.BeginRPCRequest(ctx, "canceled", "method", `{}`, time.Now().Add(time.Hour))
			return err
		},
		"complete RPC": func() error {
			return store.CompleteRPCRequest(ctx, "canceled", "method", `{}`, nil, "", "", time.Now().Add(time.Hour))
		},
		"backup":               func() error { _, err := store.Backup(ctx, 1, time.Hour); return err },
		"canonical workspace":  func() error { _, err := store.CanonicalWorkspace(ctx, workspace); return err },
		"upsert generation":    func() error { _, _, err := store.UpsertWorkspaceGeneration(ctx, workspace); return err },
		"create job":           func() error { _, err := store.CreateJob(ctx, job.Kind, "", "", ""); return err },
		"claim job":            func() error { _, err := store.ClaimJob(ctx, job.ID, "owner"); return err },
		"renew job":            func() error { return store.RenewJob(ctx, job.ID, "owner") },
		"finish job":           func() error { return store.FinishJob(ctx, job.ID, "owner", nil) },
		"retry job":            func() error { return store.RetryJob(ctx, job.ID, "owner", time.Second, "CANCELED") },
		"defer job":            func() error { return store.DeferJob(ctx, job.ID, "owner", time.Second, "CANCELED") },
		"recover jobs":         func() error { _, err := store.RecoverJobs(ctx, false); return err },
		"ensure recovery jobs": func() error { _, err := store.EnsureRecoveryJobs(ctx); return err },
		"ready slot":           func() error { _, _, err := store.ReadySlot(ctx, "canceled"); return err },
		"ready slots":          func() error { _, err := store.ReadySlots(ctx, "canceled"); return err },
		"create slot session": func() error {
			_, err := store.CreateSlotSession(ctx, Slot{ID: session.SlotID, State: "PREPARING", RootID: testRootID, RelPath: filepath.Join("_unbound", session.SlotID)}, nil, session, "")
			return err
		},
		"create standby": func() error {
			_, err := store.CreateStandby(ctx, Slot{ID: "canceled", State: "PREPARING", RootID: testRootID, RelPath: "_unbound/canceled"}, nil)
			return err
		},
		"lease ready":           func() error { return store.LeaseReady(ctx, "canceled", session) },
		"lease ready with cold": func() error { _, err := store.LeaseReadyWithCold(ctx, "canceled", session); return err },
		"set slot state":        func() error { return store.SetSlotState(ctx, "canceled", []string{"READY"}, "FAILED", "CANCELED") },
		"reset preparation":     func() error { return store.ResetPreparationForRetry(ctx, "canceled") },
		"mark ready":            func() error { return store.MarkReady(ctx, "canceled") },
		"finish preparation":    func() error { _, _, err := store.FinishPreparationWithRelease(ctx, "canceled"); return err },
		"mark session state":    func() error { return store.MarkSessionState(ctx, "canceled", []string{"STARTING"}, "ACTIVE") },
		"session":               func() error { _, err := store.Session(ctx, "canceled", "token"); return err },
		"session by ID":         func() error { _, err := store.SessionByID(ctx, "canceled"); return err },
		"register agent":        func() error { return store.RegisterAgentProcess(ctx, "canceled", "token", 1) },
		"bind agent":            func() error { return store.BindAgentSession(ctx, "canceled", "agent") },
		"bind fresh":            func() error { return store.BindFreshSession(ctx, "canceled", "", "agent") },
		"bind fresh resume": func() error {
			_, err := store.BindFreshResumeSlot(ctx, "canceled", "", "workspace", "agent", 1, nil)
			return err
		},
		"find agent": func() error { _, err := store.FindByAgentSession(ctx, "codex", "agent"); return err },
		"heartbeat":  func() error { return store.Heartbeat(ctx, "canceled", "token") },
		"orphans":    func() error { _, err := store.OrphanCandidates(ctx, now()); return err },
		"bind resume": func() error {
			_, err := store.BindResumeSlot(ctx, "canceled", "parent", "workspace", "agent", 1, nil)
			return err
		},
		"slot repositories":          func() error { _, err := store.SlotRepositories(ctx, "canceled"); return err },
		"add restoring repositories": func() error { return store.AddRestoringRepositories(ctx, "canceled", nil) },
		"slot repository":            func() error { _, err := store.SlotRepository(ctx, "canceled", "repository"); return err },
		"set slot repository": func() error {
			return store.SetSlotRepositoryState(ctx, "canceled", "repository", []string{"READY"}, "COLD")
		},
		"slot": func() error { _, err := store.Slot(ctx, "canceled"); return err },
		"save snapshot": func() error {
			return store.SaveSnapshot(ctx, Snapshot{ID: "canceled", SessionID: "canceled", RepositoryID: "repository"})
		},
		"snapshots":               func() error { _, err := store.Snapshots(ctx, "canceled"); return err },
		"save workspace snapshot": func() error { return store.SaveWorkspaceSnapshot(ctx, WorkspaceSnapshot{SessionID: "canceled"}) },
		"workspace snapshot":      func() error { _, _, err := store.WorkspaceSnapshot(ctx, "canceled"); return err },
		"repository":              func() error { _, err := store.Repository(ctx, "repository"); return err },
		"workspace":               func() error { _, err := store.Workspace(ctx, "canceled"); return err },
		"workspace by root":       func() error { _, err := store.WorkspaceByRoot(ctx, "/canceled"); return err },
		"session workspace":       func() error { _, err := store.SessionWorkspace(ctx, "canceled"); return err },
		"workspace roots":         func() error { _, err := store.WorkspaceRoots(ctx); return err },
		"list sessions":           func() error { _, err := store.ListSessions(ctx, true); return err },
		"forget workspace":        func() error { return store.ForgetWorkspace(ctx, "/canceled") },
		"status":                  func() error { _, err := store.Status(ctx); return err },
		"status diagnostics":      func() error { _, err := store.StatusDiagnostics(ctx); return err },
		"mark archived":           func() error { return store.MarkArchived(ctx, "canceled", "canceled", now()) },
		"begin snapshot":          func() error { return store.BeginSnapshot(ctx, "canceled", "canceled") },
		"release":                 func() error { _, _, err := store.Release(ctx, "canceled", "", "canceled"); return err },
		"slot artifacts":          func() error { _, err := store.SlotArtifacts(ctx); return err },
		"quarantine slot":         func() error { return store.QuarantineMissingSlot(ctx, "canceled", "CANCELED") },
		"quarantine artifact":     func() error { return store.QuarantineArtifact(ctx, "test", "/canceled", "CANCELED") },
		"quarantine recovery ref": func() error { return store.QuarantineMissingRecoveryRef(ctx, "refs/wx/canceled") },
		"repositories":            func() error { _, err := store.Repositories(ctx); return err },
		"cold candidates":         func() error { _, err := store.ColdRepositoryCandidates(ctx, now()); return err },
		"schedule cold removal": func() error {
			_, _, err := store.ScheduleColdRepositoryRemoval(ctx, ColdRepositoryCandidate{SlotID: "canceled", WorkspaceID: "canceled", RepositoryID: "repository"})
			return err
		},
		"finish cold removal":      func() error { return store.FinishColdRepositoryRemoval(ctx, "canceled", "repository") },
		"standby GC candidates":    func() error { _, err := store.StandbyGCCandidates(ctx, now(), 1); return err },
		"schedule removal":         func() error { _, _, err := store.ScheduleRemoval(ctx, "canceled", "canceled"); return err },
		"finish removal":           func() error { return store.FinishRemoval(ctx, "canceled") },
		"prune roots":              func() error { return store.PruneRoots(ctx) },
		"expired snapshots":        func() error { _, err := store.ExpiredSnapshots(ctx, now()); return err },
		"expire session snapshots": func() error { return store.ExpireSessionSnapshots(ctx, "canceled") },
		"prune metadata":           func() error { return store.PruneMetadata(ctx, now(), now(), now()) },
		"GC candidates":            func() error { _, err := store.GCCandidates(ctx, now()); return err },
		"mark slot archived":       func() error { return store.MarkSlotArchived(ctx, "canceled") },
	}
	for name, operation := range operations {
		if err := operation(); err == nil {
			t.Errorf("%s ignored canceled context", name)
		}
	}
}

func TestPreparationRetryAndOwnerStateBranches(t *testing.T) {
	ctx := context.Background()
	t.Run("retry failed preparation", func(t *testing.T) {
		store := openTestStore(t)
		seedWorkspace(t, store)
		root := t.TempDir()
		if _, err := store.CreateStandby(ctx, Slot{ID: "retry", WorkspaceID: "workspace", Generation: 1, RootID: testRootID, RelPath: "workspace/retry", State: "FAILED"}, []SlotRepository{{RepositoryID: "repository", WorktreePath: filepath.Join(root, "retry", "repository"), State: "PREPARE_RUNNING"}}); err != nil {
			t.Fatal(err)
		}
		if err := store.ResetPreparationForRetry(ctx, "retry"); err != nil {
			t.Fatal(err)
		}
		slot, err := store.Slot(ctx, "retry")
		if err != nil || slot.State != "PREPARING" {
			t.Fatalf("retried slot=%+v err=%v", slot, err)
		}
		repository, err := store.SlotRepository(ctx, "retry", "repository")
		if err != nil || repository.State != "PREPARING" {
			t.Fatalf("retried repository=%+v err=%v", repository, err)
		}
	})

	t.Run("finish unowned preparation", func(t *testing.T) {
		store := openTestStore(t)
		seedWorkspace(t, store)
		if _, err := store.CreateStandby(ctx, Slot{ID: "unowned", WorkspaceID: "workspace", Generation: 1, RootID: testRootID, RelPath: "workspace/unowned", State: "PREPARING"}, nil); err != nil {
			t.Fatal(err)
		}
		job, scheduled, err := store.FinishPreparationWithRelease(ctx, "unowned")
		if err != nil || scheduled || job.ID != "" {
			t.Fatalf("unowned finish job=%+v scheduled=%v err=%v", job, scheduled, err)
		}
		slot, err := store.Slot(ctx, "unowned")
		if err != nil || slot.State != "READY" {
			t.Fatalf("unowned slot=%+v err=%v", slot, err)
		}
	})

	t.Run("finish starting owner", func(t *testing.T) {
		store := openTestStore(t)
		seedWorkspace(t, store)
		session := Session{ID: "starting", WorkspaceID: "workspace", SlotID: "starting", State: "STARTING", AgentKind: "codex", TokenHash: HashToken("token")}
		if _, err := store.CreateSlotSession(ctx, Slot{ID: "starting", WorkspaceID: "workspace", Generation: 1, RootID: testRootID, RelPath: "workspace/starting", State: "PREPARING"}, nil, session, "PREPARE"); err != nil {
			t.Fatal(err)
		}
		job, scheduled, err := store.FinishPreparationWithRelease(ctx, "starting")
		if err != nil || scheduled || job.ID != "" {
			t.Fatalf("starting finish job=%+v scheduled=%v err=%v", job, scheduled, err)
		}
		slot, err := store.Slot(ctx, "starting")
		if err != nil || slot.State != "LEASED" {
			t.Fatalf("starting slot=%+v err=%v", slot, err)
		}
		stored, err := store.SessionByID(ctx, session.ID)
		if err != nil || stored.State != "ACTIVE" {
			t.Fatalf("starting session=%+v err=%v", stored, err)
		}
	})

	t.Run("finish restoring without pending mapping", func(t *testing.T) {
		store := openTestStore(t)
		seedWorkspace(t, store)
		session := Session{ID: "restoring", WorkspaceID: "workspace", SlotID: "restoring", State: "RESTORING", AgentKind: "codex", TokenHash: HashToken("token")}
		if _, err := store.CreateSlotSession(ctx, Slot{ID: "restoring", WorkspaceID: "workspace", Generation: 1, RootID: testRootID, RelPath: "workspace/restoring", State: "RESTORING"}, nil, session, "RESTORE"); err != nil {
			t.Fatal(err)
		}
		job, scheduled, err := store.FinishPreparationWithRelease(ctx, "restoring")
		if err != nil || scheduled || job.ID != "" {
			t.Fatalf("restoring finish job=%+v scheduled=%v err=%v", job, scheduled, err)
		}
		slot, err := store.Slot(ctx, "restoring")
		if err != nil || slot.State != "LEASED" {
			t.Fatalf("restoring slot=%+v err=%v", slot, err)
		}
		stored, err := store.SessionByID(ctx, session.ID)
		if err != nil || stored.State != "ACTIVE" {
			t.Fatalf("restoring session=%+v err=%v", stored, err)
		}
	})

	t.Run("finish owner in unexpected state", func(t *testing.T) {
		store := openTestStore(t)
		seedWorkspace(t, store)
		session := Session{ID: "unexpected", WorkspaceID: "workspace", SlotID: "unexpected", State: "STARTING", AgentKind: "codex", TokenHash: HashToken("token")}
		if _, err := store.CreateSlotSession(ctx, Slot{ID: "unexpected", WorkspaceID: "workspace", Generation: 1, RootID: testRootID, RelPath: "workspace/unexpected", State: "PREPARING"}, nil, session, "PREPARE"); err != nil {
			t.Fatal(err)
		}
		if err := store.MarkSessionState(ctx, session.ID, []string{"STARTING"}, "SNAPSHOTTING"); err != nil {
			t.Fatal(err)
		}
		if _, _, err := store.FinishPreparationWithRelease(ctx, "unexpected"); err == nil {
			t.Fatal("finish preparation with a SNAPSHOTTING owner session must fail")
		}
		slot, err := store.Slot(ctx, "unexpected")
		if err != nil || slot.State != "PREPARING" {
			t.Fatalf("unexpected slot=%+v err=%v", slot, err)
		}
	})
}

func TestColdRemovalCompletionAndAdministrativeQueries(t *testing.T) {
	store := openTestStore(t)
	seedWorkspace(t, store)
	ctx := context.Background()
	repository := SlotRepository{RepositoryID: "repository", DirName: "repository", State: "READY", RequestedRef: "main", BaseOID: "head", Fingerprint: "fingerprint"}
	job, err := store.CreateStandby(ctx, Slot{ID: "cold", WorkspaceID: "workspace", Generation: 1, RootID: testRootID, RelPath: "workspace/cold", State: "READY"}, []SlotRepository{repository})
	if err != nil {
		t.Fatal(err)
	}
	if _, changed, err := store.ScheduleColdRepositoryRemoval(ctx, ColdRepositoryCandidate{SlotID: "cold", WorkspaceID: "workspace", RepositoryID: "repository", WorktreePath: repository.WorktreePath}); err != nil || !changed {
		t.Fatalf("cold removal scheduling changed=%v err=%v", changed, err)
	}
	if err := store.FinishColdRepositoryRemoval(ctx, "cold", "repository"); err != nil {
		t.Fatal(err)
	}
	finished, err := store.SlotRepository(ctx, "cold", "repository")
	if err != nil || finished.State != "COLD" {
		t.Fatalf("finished cold repository=%+v err=%v", finished, err)
	}
	slot, err := store.Slot(ctx, "cold")
	if err != nil || slot.State != "READY" {
		t.Fatalf("finished cold slot=%+v err=%v", slot, err)
	}
	if claimed, err := store.ClaimJob(ctx, job.ID, "worker"); err != nil {
		t.Fatal(err)
	} else if err := store.FinishJob(ctx, claimed.ID, "worker", nil); err != nil {
		t.Fatal(err)
	}

	removal, changed, err := store.ScheduleRemoval(ctx, "cold", "")
	if err != nil || !changed || removal.Kind != "REMOVE" || removal.WorkspaceID != "workspace" {
		t.Fatalf("schedule removal job=%+v changed=%v err=%v", removal, changed, err)
	}
	removed, err := store.Slot(ctx, "cold")
	if err != nil || removed.State != "REMOVING" || removed.OwnerSessionID != "" {
		t.Fatalf("scheduled removal slot=%+v err=%v", removed, err)
	}
	if err := store.FinishRemoval(ctx, "cold"); err != nil {
		t.Fatal(err)
	}
	archived, err := store.Slot(ctx, "cold")
	if err != nil || archived.State != "ARCHIVED" {
		t.Fatalf("finished removal slot=%+v err=%v", archived, err)
	}
}

func TestRestoringRepositoryMetadataAndSnapshotExpiryBoundaries(t *testing.T) {
	store := openTestStore(t)
	seedWorkspace(t, store)
	ctx := context.Background()
	root := t.TempDir()
	if _, err := store.CreateSlotSession(ctx, Slot{ID: "restore-meta", State: "RESTORING", RootID: testRootID, RelPath: "_unbound/restore-meta"}, nil, Session{ID: "restore-meta", SlotID: "restore-meta", State: "RESTORING", AgentKind: "codex", TokenHash: HashToken("token")}, ""); err != nil {
		t.Fatal(err)
	}
	repos := []SlotRepository{{RepositoryID: "repository", WorktreePath: filepath.Join(root, "restore-meta", "repository"), RequestedRef: "main", BaseOID: "head", Fingerprint: "fingerprint"}}
	if err := store.AddRestoringRepositories(ctx, "restore-meta", repos); err != nil {
		t.Fatal(err)
	}
	if err := store.AddRestoringRepositories(ctx, "restore-meta", repos); err != nil {
		t.Fatalf("idempotent restoring metadata: %v", err)
	}
	if err := store.AddRestoringRepositories(ctx, "restore-meta", nil); err == nil {
		t.Fatal("incomplete restoring metadata was accepted")
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE sessions SET state='EXPIRED' WHERE id='restore-meta'`); err != nil {
		t.Fatal(err)
	}
	if err := store.ExpireSessionSnapshots(ctx, "restore-meta"); err != nil {
		t.Fatal(err)
	}
}

func TestReadAndRestoreStateBoundaries(t *testing.T) {
	store := openTestStore(t)
	seedWorkspace(t, store)
	ctx := context.Background()
	if _, found, err := store.ReadySlot(ctx, "missing"); err != nil || found {
		t.Fatalf("missing ready slot=%+v found=%v err=%v", Slot{}, found, err)
	}
	parent := Session{ID: "parent-copy", WorkspaceID: "workspace", SlotID: "parent-copy", State: "ARCHIVED", AgentKind: "codex", TokenHash: HashToken("parent")}
	parentRepo := SlotRepository{RepositoryID: "repository", DirName: "repository", State: "READY", RequestedRef: "main", BaseOID: "head", Fingerprint: "fingerprint"}
	if _, err := store.CreateSlotSession(ctx, Slot{ID: parent.SlotID, WorkspaceID: "workspace", Generation: 1, RootID: testRootID, RelPath: filepath.Join("workspace", parent.SlotID), State: "SNAPSHOTTED"}, []SlotRepository{parentRepo}, parent, ""); err != nil {
		t.Fatal(err)
	}
	child := Session{ID: "child-copy", WorkspaceID: "workspace", SlotID: "child-copy", ParentSessionID: parent.ID, State: "STARTING", AgentKind: "codex", TokenHash: HashToken("child")}
	if _, err := store.CreateSlotSession(ctx, Slot{ID: child.SlotID, WorkspaceID: "workspace", Generation: 1, RootID: testRootID, RelPath: filepath.Join("workspace", child.SlotID), State: "PREPARING"}, nil, child, "RESTORE"); err != nil {
		t.Fatal(err)
	}
	if repositories, err := store.SlotRepositories(ctx, child.SlotID); err != nil || len(repositories) != 0 {
		// Restore は所属情報を session テーブルへ複製するが、slot のメタデータは
		// prepare 段階でリポジトリを実体化するまで空のままである。
		t.Fatalf("child slot repositories=%v err=%v", repositories, err)
	}
	if err := store.BindAgentSession(ctx, child.ID, "agent-copy"); err != nil {
		t.Fatalf("bind child agent: %v", err)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE sessions SET state='RESTORING',parent_session_id=NULL,pending_agent_session_id='pending' WHERE id='child-copy'`); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.FinishPreparationWithRelease(ctx, child.SlotID); err == nil || !strings.Contains(err.Error(), "no parent") {
		t.Fatalf("pending mapping without parent error=%v", err)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE sessions SET parent_session_id='parent-copy' WHERE id='child-copy'`); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.FinishPreparationWithRelease(ctx, child.SlotID); err == nil || !strings.Contains(err.Error(), "mapping changed") {
		t.Fatalf("pending mapping with missing parent error=%v", err)
	}
}

func TestStandbyGCKeepsWarmSlotsAndReportsStaleRows(t *testing.T) {
	store := openTestStore(t)
	seedWorkspace(t, store)
	ctx := context.Background()
	for _, id := range []string{"warm-a", "warm-b", "stale"} {
		if _, err := store.CreateStandby(ctx, Slot{ID: id, WorkspaceID: "workspace", Generation: 1, RootID: testRootID, RelPath: filepath.Join("workspace", id), State: "READY"}, nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.SetSlotState(ctx, "stale", []string{"READY"}, "STALE", "TEST"); err != nil {
		t.Fatal(err)
	}
	candidates, err := store.StandbyGCCandidates(ctx, now(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 2 {
		t.Fatalf("GC candidates=%+v, want one warm eviction and stale slot", candidates)
	}
	foundStale := false
	for _, candidate := range candidates {
		foundStale = foundStale || candidate.SlotID == "stale"
	}
	if !foundStale {
		t.Fatalf("stale candidate missing from %+v", candidates)
	}
}

func TestReleaseAndForgetRefuseDurableStateThatChangedUnderTheCaller(t *testing.T) {
	ctx := context.Background()

	t.Run("release defers while a slot is preparing", func(t *testing.T) {
		store := openTestStore(t)
		seedWorkspace(t, store)
		session := Session{ID: "changed-release", WorkspaceID: "workspace", SlotID: "changed-release", State: "ACTIVE", AgentKind: "codex", TokenHash: HashToken("token")}
		if _, err := store.CreateSlotSession(ctx, Slot{ID: session.SlotID, WorkspaceID: "workspace", Generation: 1, RootID: testRootID, RelPath: filepath.Join("workspace", session.SlotID), State: "PREPARING"}, nil, session, ""); err != nil {
			t.Fatal(err)
		}
		job, changed, err := store.Release(ctx, session.ID, "workspace", session.SlotID)
		if err != nil {
			t.Fatal(err)
		}
		if changed || job.ID != "" {
			t.Fatalf("release scheduled unexpectedly: job=%+v changed=%v", job, changed)
		}
		stored, err := store.SessionByID(ctx, session.ID)
		if err != nil {
			t.Fatal(err)
		}
		if stored.State != "RELEASING" {
			t.Fatalf("release state=%q", stored.State)
		}
	})

	t.Run("release rejects an unexpected slot state", func(t *testing.T) {
		store := openTestStore(t)
		seedWorkspace(t, store)
		session := Session{ID: "changed-release-ready", WorkspaceID: "workspace", SlotID: "changed-release-ready", State: "ACTIVE", AgentKind: "codex", TokenHash: HashToken("token")}
		if _, err := store.CreateSlotSession(ctx, Slot{ID: session.SlotID, WorkspaceID: "workspace", Generation: 1, RootID: testRootID, RelPath: filepath.Join("workspace", session.SlotID), State: "READY"}, nil, session, ""); err != nil {
			t.Fatal(err)
		}
		if _, _, err := store.Release(ctx, session.ID, "workspace", session.SlotID); err == nil || !strings.Contains(err.Error(), "cannot be released") {
			t.Fatalf("release accepted unexpected slot state: %v", err)
		}
	})

	t.Run("forget waits for pending jobs", func(t *testing.T) {
		store := openTestStore(t)
		seedWorkspace(t, store)
		if _, err := store.CreateJob(ctx, "REMOVE", "workspace", "", ""); err != nil {
			t.Fatal(err)
		}
		if err := store.ForgetWorkspace(ctx, "/workspace"); err == nil || !strings.Contains(err.Error(), "pending recovery jobs") {
			t.Fatalf("forget ignored pending job: %v", err)
		}
	})
}

func TestScheduleColdRepositoryRemovalPropagatesTransactionFaults(t *testing.T) {
	ctx := context.Background()
	newColdSlot := func(t *testing.T, store *Store, id string) {
		t.Helper()
		if _, err := store.CreateStandby(ctx, Slot{ID: id, WorkspaceID: "workspace", Generation: 1, RootID: testRootID, RelPath: filepath.Join("workspace", id), State: "READY"}, []SlotRepository{{RepositoryID: "repository", WorktreePath: "/wx/" + id + "/repository", State: "READY", BaseOID: "head"}}); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("slot transition fault", func(t *testing.T) {
		store := openTestStore(t)
		seedWorkspace(t, store)
		newColdSlot(t, store, "cold-slot-fault")
		if _, err := store.db.Exec(`CREATE TRIGGER fail_cold_slot BEFORE UPDATE OF state ON slots WHEN OLD.id='cold-slot-fault' BEGIN SELECT RAISE(ABORT,'fault'); END`); err != nil {
			t.Fatal(err)
		}
		if _, _, err := store.ScheduleColdRepositoryRemoval(ctx, ColdRepositoryCandidate{SlotID: "cold-slot-fault", WorkspaceID: "workspace", RepositoryID: "repository", WorktreePath: "/wx/cold-slot-fault/repository"}); err == nil {
			t.Fatal("cold repository removal succeeded despite a slot transition fault")
		}
	})

	t.Run("job insertion fault", func(t *testing.T) {
		store := openTestStore(t)
		seedWorkspace(t, store)
		newColdSlot(t, store, "cold-job-fault")
		if _, err := store.db.Exec(`CREATE TRIGGER fail_cold_job BEFORE INSERT ON jobs WHEN NEW.kind='REMOVE_REPOSITORY' BEGIN SELECT RAISE(ABORT,'fault'); END`); err != nil {
			t.Fatal(err)
		}
		if _, _, err := store.ScheduleColdRepositoryRemoval(ctx, ColdRepositoryCandidate{SlotID: "cold-job-fault", WorkspaceID: "workspace", RepositoryID: "repository", WorktreePath: "/wx/cold-job-fault/repository"}); err == nil {
			t.Fatal("cold repository removal succeeded despite a job insertion fault")
		}
		if slot, err := store.Slot(ctx, "cold-job-fault"); err != nil || slot.State != "READY" {
			t.Fatalf("rolled-back cold removal slot=%+v err=%v", slot, err)
		}
	})
}

func TestReleasePropagatesTransactionFaults(t *testing.T) {
	ctx := context.Background()

	t.Run("draining slot update fault", func(t *testing.T) {
		store := openTestStore(t)
		seedWorkspace(t, store)
		session := Session{ID: "release-drain-fault", WorkspaceID: "workspace", SlotID: "release-drain-fault", State: "ACTIVE", AgentKind: "codex", TokenHash: HashToken("release-drain-fault")}
		if _, err := store.CreateSlotSession(ctx, Slot{ID: "release-drain-fault", WorkspaceID: "workspace", Generation: 1, RootID: testRootID, RelPath: "workspace/release-drain-fault", State: "LEASED"}, nil, session, ""); err != nil {
			t.Fatal(err)
		}
		if _, err := store.db.Exec(`CREATE TRIGGER fail_release_drain BEFORE UPDATE OF state ON slots WHEN OLD.id='release-drain-fault' BEGIN SELECT RAISE(ABORT,'fault'); END`); err != nil {
			t.Fatal(err)
		}
		if _, _, err := store.Release(ctx, session.ID, session.WorkspaceID, session.SlotID); err == nil {
			t.Fatal("release succeeded despite a draining slot transition fault")
		}
	})

	t.Run("unbound session removal job insertion fault", func(t *testing.T) {
		store := openTestStore(t)
		seedWorkspace(t, store)
		session := Session{ID: "release-unbound-fault", WorkspaceID: "workspace", SlotID: "release-unbound-fault", State: "UNBOUND", AgentKind: "codex", TokenHash: HashToken("release-unbound-fault")}
		if _, err := store.CreateSlotSession(ctx, Slot{ID: "release-unbound-fault", WorkspaceID: "workspace", Generation: 1, RootID: testRootID, RelPath: "workspace/release-unbound-fault", State: "UNBOUND"}, nil, session, ""); err != nil {
			t.Fatal(err)
		}
		if _, err := store.db.Exec(`CREATE TRIGGER fail_release_unbound_job BEFORE INSERT ON jobs WHEN NEW.kind='REMOVE' BEGIN SELECT RAISE(ABORT,'fault'); END`); err != nil {
			t.Fatal(err)
		}
		if _, _, err := store.Release(ctx, session.ID, session.WorkspaceID, session.SlotID); err == nil {
			t.Fatal("release succeeded despite an unbound removal job insertion fault")
		}
	})

	t.Run("unbound session expiry fault", func(t *testing.T) {
		store := openTestStore(t)
		seedWorkspace(t, store)
		session := Session{ID: "release-unbound-session-fault", WorkspaceID: "workspace", SlotID: "release-unbound-session-fault", State: "UNBOUND", AgentKind: "codex", TokenHash: HashToken("release-unbound-session-fault")}
		if _, err := store.CreateSlotSession(ctx, Slot{ID: "release-unbound-session-fault", WorkspaceID: "workspace", Generation: 1, RootID: testRootID, RelPath: "workspace/release-unbound-session-fault", State: "UNBOUND"}, nil, session, ""); err != nil {
			t.Fatal(err)
		}
		if _, err := store.db.Exec(`CREATE TRIGGER fail_release_unbound_session BEFORE UPDATE OF state ON sessions WHEN OLD.id='release-unbound-session-fault' BEGIN SELECT RAISE(ABORT,'fault'); END`); err != nil {
			t.Fatal(err)
		}
		if _, _, err := store.Release(ctx, session.ID, session.WorkspaceID, session.SlotID); err == nil {
			t.Fatal("release succeeded despite an unbound session expiry fault")
		}
	})

	t.Run("unbound slot removal fault", func(t *testing.T) {
		store := openTestStore(t)
		seedWorkspace(t, store)
		session := Session{ID: "release-unbound-slot-fault", WorkspaceID: "workspace", SlotID: "release-unbound-slot-fault", State: "UNBOUND", AgentKind: "codex", TokenHash: HashToken("release-unbound-slot-fault")}
		if _, err := store.CreateSlotSession(ctx, Slot{ID: "release-unbound-slot-fault", WorkspaceID: "workspace", Generation: 1, RootID: testRootID, RelPath: "workspace/release-unbound-slot-fault", State: "UNBOUND"}, nil, session, ""); err != nil {
			t.Fatal(err)
		}
		if _, err := store.db.Exec(`CREATE TRIGGER fail_release_unbound_slot BEFORE UPDATE OF state ON slots WHEN OLD.id='release-unbound-slot-fault' AND NEW.state='REMOVING' BEGIN SELECT RAISE(ABORT,'fault'); END`); err != nil {
			t.Fatal(err)
		}
		if _, _, err := store.Release(ctx, session.ID, session.WorkspaceID, session.SlotID); err == nil {
			t.Fatal("release succeeded despite an unbound slot removal fault")
		}
	})
}

func TestScheduleRemovalPropagatesJobInsertionFault(t *testing.T) {
	store := openTestStore(t)
	seedWorkspace(t, store)
	ctx := context.Background()
	if _, err := store.CreateStandby(ctx, Slot{ID: "schedule-removal-fault", WorkspaceID: "workspace", Generation: 1, RootID: testRootID, RelPath: "workspace/schedule-removal-fault", State: "READY"}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`CREATE TRIGGER fail_schedule_removal_job BEFORE INSERT ON jobs WHEN NEW.kind='REMOVE' AND NEW.slot_id='schedule-removal-fault' BEGIN SELECT RAISE(ABORT,'fault'); END`); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.ScheduleRemoval(ctx, "schedule-removal-fault", ""); err == nil {
		t.Fatal("schedule removal succeeded despite a job insertion fault")
	}
	if slot, err := store.Slot(ctx, "schedule-removal-fault"); err != nil || slot.State != "READY" {
		t.Fatalf("rolled-back schedule removal slot=%+v err=%v", slot, err)
	}
}

func TestEnsureRecoveryJobsPropagatesJobInsertionFault(t *testing.T) {
	store := openTestStore(t)
	seedWorkspace(t, store)
	ctx := context.Background()
	if _, err := store.db.Exec(`INSERT INTO slots(id,workspace_id,generation,root_id,rel_path,state,created_at,updated_at) VALUES('recovery-fault','workspace',1,?,'workspace/recovery-fault','PREPARING',?,?)`, testRootID, now(), now()); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`CREATE TRIGGER fail_recovery_job BEFORE INSERT ON jobs WHEN NEW.kind='PREPARE' BEGIN SELECT RAISE(ABORT,'fault'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.EnsureRecoveryJobs(ctx); err == nil {
		t.Fatal("recovery job reconstruction succeeded despite a job insertion fault")
	}
	var count int
	if err := store.db.QueryRow(`SELECT count(*) FROM jobs`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("failed recovery reconstruction left partial jobs: %d", count)
	}
}

func TestUpsertWorkspaceGenerationPropagatesMembershipTransactionFaults(t *testing.T) {
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

func TestQuarantineMissingRecoveryRefPropagatesTransactionFaults(t *testing.T) {
	ctx := context.Background()
	newSnapshotFixture := func(t *testing.T, id string) *Store {
		t.Helper()
		store := openTestStore(t)
		seedWorkspace(t, store)
		session := Session{ID: id, WorkspaceID: "workspace", SlotID: id, State: "ACTIVE", AgentKind: "codex", TokenHash: HashToken(id)}
		if _, err := store.CreateSlotSession(ctx, Slot{ID: id, WorkspaceID: "workspace", Generation: 1, RootID: testRootID, RelPath: filepath.Join("workspace", id), State: "LEASED"}, nil, session, ""); err != nil {
			t.Fatal(err)
		}
		if err := store.SaveSnapshot(ctx, Snapshot{ID: id, SessionID: id, RepositoryID: "repository", HeadOID: "head", HeadRef: "refs/wx/recovery/" + id, IndexTreeOID: "index", WorktreeOID: "tree", WorktreeRef: "refs/wx/recovery/" + id + "-worktree", Status: "ARCHIVED", CreatedAt: now(), ExpiresAt: FormatTime(time.Now().Add(time.Hour))}); err != nil {
			t.Fatal(err)
		}
		return store
	}

	t.Run("session quarantine fault", func(t *testing.T) {
		store := newSnapshotFixture(t, "quarantine-session-fault")
		if _, err := store.db.Exec(`CREATE TRIGGER fail_quarantine_session BEFORE UPDATE OF state ON sessions WHEN OLD.id='quarantine-session-fault' BEGIN SELECT RAISE(ABORT,'fault'); END`); err != nil {
			t.Fatal(err)
		}
		if err := store.QuarantineMissingRecoveryRef(ctx, "refs/wx/recovery/quarantine-session-fault"); err == nil {
			t.Fatal("recovery ref quarantine succeeded despite a session update fault")
		}
		if session, err := store.SessionByID(ctx, "quarantine-session-fault"); err != nil || session.State != "ACTIVE" {
			t.Fatalf("rolled-back quarantine session=%+v err=%v", session, err)
		}
	})

	t.Run("slot quarantine fault", func(t *testing.T) {
		store := newSnapshotFixture(t, "quarantine-slot-fault")
		if _, err := store.db.Exec(`CREATE TRIGGER fail_quarantine_slot BEFORE UPDATE OF state ON slots WHEN OLD.id='quarantine-slot-fault' BEGIN SELECT RAISE(ABORT,'fault'); END`); err != nil {
			t.Fatal(err)
		}
		if err := store.QuarantineMissingRecoveryRef(ctx, "refs/wx/recovery/quarantine-slot-fault"); err == nil {
			t.Fatal("recovery ref quarantine succeeded despite a slot update fault")
		}
		if slot, err := store.Slot(ctx, "quarantine-slot-fault"); err != nil || slot.State != "LEASED" {
			t.Fatalf("rolled-back quarantine slot=%+v err=%v", slot, err)
		}
	})
}

func TestCreateSlotSessionPropagatesLastLeasedAtFault(t *testing.T) {
	store := openTestStore(t)
	seedWorkspace(t, store)
	ctx := context.Background()
	if _, err := store.db.Exec(`CREATE TRIGGER fail_last_leased_at BEFORE UPDATE OF last_leased_at ON repositories WHEN OLD.id='repository' BEGIN SELECT RAISE(ABORT,'fault'); END`); err != nil {
		t.Fatal(err)
	}
	session := Session{ID: "leased-at-fault", WorkspaceID: "workspace", SlotID: "leased-at-fault", State: "STARTING", AgentKind: "codex", TokenHash: HashToken("leased-at-fault")}
	if _, err := store.CreateSlotSession(ctx, Slot{ID: "leased-at-fault", WorkspaceID: "workspace", Generation: 1, RootID: testRootID, RelPath: "workspace/leased-at-fault", State: "PREPARING"}, nil, session, ""); err == nil {
		t.Fatal("slot session creation succeeded despite a last_leased_at update fault")
	}
	if _, err := store.Slot(ctx, "leased-at-fault"); err == nil {
		t.Fatal("rolled-back slot session left a durable slot row")
	}
}

func TestFinishColdRepositoryRemovalPropagatesSlotReadyFault(t *testing.T) {
	store := openTestStore(t)
	seedWorkspace(t, store)
	ctx := context.Background()
	if _, err := store.CreateStandby(ctx, Slot{ID: "cold-ready-fault", WorkspaceID: "workspace", Generation: 1, RootID: testRootID, RelPath: "workspace/cold-ready-fault", State: "RETIRING"}, []SlotRepository{{RepositoryID: "repository", WorktreePath: "/wx/cold-ready-fault/repository", State: "RETIRING", BaseOID: "head"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`CREATE TRIGGER fail_cold_ready BEFORE UPDATE OF state ON slots WHEN OLD.id='cold-ready-fault' AND NEW.state='READY' BEGIN SELECT RAISE(ABORT,'fault'); END`); err != nil {
		t.Fatal(err)
	}
	if err := store.FinishColdRepositoryRemoval(ctx, "cold-ready-fault", "repository"); err == nil {
		t.Fatal("cold repository finish succeeded despite a slot-ready transition fault")
	}
	if slot, err := store.Slot(ctx, "cold-ready-fault"); err != nil || slot.State != "RETIRING" {
		t.Fatalf("rolled-back cold finish slot=%+v err=%v", slot, err)
	}
}

func TestReopeningAnExistingDatabaseSkipsAppliedMigrations(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	first, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	seedWorkspace(t, first)
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	// 同じデータベースを再オープンし、全 migration が適用済みなら init が
	// 再実行を省略する schema_migrations の短絡経路を検証する。
	second, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = second.Close() }()
	if _, err := second.Workspace(context.Background(), "workspace"); err != nil {
		t.Fatalf("data from before reopen is unavailable: %v", err)
	}
}

func TestFinishPreparationWithReleasePropagatesSnapshotJobFault(t *testing.T) {
	store := openTestStore(t)
	seedWorkspace(t, store)
	ctx := context.Background()
	session := Session{ID: "releasing-fault", WorkspaceID: "workspace", SlotID: "releasing-fault", State: "RELEASING", AgentKind: "codex", TokenHash: HashToken("releasing-fault")}
	if _, err := store.CreateSlotSession(ctx, Slot{ID: "releasing-fault", WorkspaceID: "workspace", Generation: 1, RootID: testRootID, RelPath: "workspace/releasing-fault", State: "PREPARING"}, nil, session, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`CREATE TRIGGER fail_finish_preparation_snapshot BEFORE INSERT ON jobs WHEN NEW.kind='SNAPSHOT' AND NEW.slot_id='releasing-fault' BEGIN SELECT RAISE(ABORT,'fault'); END`); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.FinishPreparationWithRelease(ctx, session.SlotID); err == nil {
		t.Fatal("finish preparation succeeded despite a snapshot job insertion fault")
	}
	if slot, err := store.Slot(ctx, session.SlotID); err != nil || slot.State != "PREPARING" {
		t.Fatalf("rolled-back finish preparation slot=%+v err=%v", slot, err)
	}
}

func TestCreateStandbyPropagatesJobInsertionFault(t *testing.T) {
	store := openTestStore(t)
	seedWorkspace(t, store)
	ctx := context.Background()
	if _, err := store.db.Exec(`CREATE TRIGGER fail_create_standby_job BEFORE INSERT ON jobs WHEN NEW.kind='PREPARE' AND NEW.slot_id='standby-job-fault' BEGIN SELECT RAISE(ABORT,'fault'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateStandby(ctx, Slot{ID: "standby-job-fault", WorkspaceID: "workspace", Generation: 1, RootID: testRootID, RelPath: "workspace/standby-job-fault", State: "PREPARING"}, nil); err == nil {
		t.Fatal("create standby succeeded despite a job insertion fault")
	}
	if _, err := store.Slot(ctx, "standby-job-fault"); err == nil {
		t.Fatal("rolled-back standby creation left a durable slot row")
	}
}

func TestLeaseReadyPropagatesTransactionFaults(t *testing.T) {
	ctx := context.Background()

	t.Run("repository lease timestamp fault", func(t *testing.T) {
		store := openTestStore(t)
		seedWorkspace(t, store)
		if _, err := store.CreateStandby(ctx, Slot{ID: "lease-ready-fault", WorkspaceID: "workspace", Generation: 1, RootID: testRootID, RelPath: "workspace/lease-ready-fault", State: "READY"}, nil); err != nil {
			t.Fatal(err)
		}
		if _, err := store.db.Exec(`CREATE TRIGGER fail_lease_ready_repo BEFORE UPDATE OF last_leased_at ON repositories WHEN OLD.id='repository' BEGIN SELECT RAISE(ABORT,'fault'); END`); err != nil {
			t.Fatal(err)
		}
		session := Session{ID: "lease-ready-fault", WorkspaceID: "workspace", State: "STARTING", AgentKind: "codex", TokenHash: HashToken("lease-ready-fault")}
		if err := store.LeaseReady(ctx, "lease-ready-fault", session); err == nil {
			t.Fatal("lease ready succeeded despite a repository lease timestamp fault")
		}
		if slot, err := store.Slot(ctx, "lease-ready-fault"); err != nil || slot.State != "READY" {
			t.Fatalf("rolled-back lease slot=%+v err=%v", slot, err)
		}
	})
}

func TestDeferJobPropagatesUpdateFault(t *testing.T) {
	store := openTestStore(t)
	seedWorkspace(t, store)
	ctx := context.Background()
	job, err := store.CreateJob(ctx, "RESTORE", "workspace", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ClaimJob(ctx, job.ID, "worker"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(fmt.Sprintf(`CREATE TRIGGER fail_defer_job BEFORE UPDATE OF state ON jobs WHEN OLD.id='%s' AND NEW.state='PENDING' BEGIN SELECT RAISE(ABORT,'fault'); END`, job.ID)); err != nil {
		t.Fatal(err)
	}
	if err := store.DeferJob(ctx, job.ID, "worker", 0, "SNAPSHOT_PENDING"); err == nil {
		t.Fatal("job deferral succeeded despite an update fault")
	}
}

func TestRecoverJobsPropagatesReclaimUpdateFault(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	job, err := store.CreateJob(ctx, "ENSURE_STANDBY", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ClaimJob(ctx, job.ID, "owner"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`UPDATE jobs SET lease_expires_at=? WHERE id=?`, time.Now().Add(-time.Minute).UTC().Format(time.RFC3339Nano), job.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`CREATE TRIGGER fail_recover_jobs BEFORE UPDATE OF state ON jobs WHEN NEW.state='PENDING' BEGIN SELECT RAISE(ABORT,'fault'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.RecoverJobs(ctx, false); err == nil {
		t.Fatal("job recovery succeeded despite a reclaim update fault")
	}
}

func TestSaveWorkspaceSnapshotPropagatesInsertionFault(t *testing.T) {
	store := openTestStore(t)
	seedWorkspace(t, store)
	ctx := context.Background()
	if _, err := store.CreateSlotSession(ctx, Slot{ID: "workspace-snapshot-fault", State: "ARCHIVED", RootID: testRootID, RelPath: "_unbound/workspace-snapshot-fault"}, nil, Session{ID: "workspace-snapshot-fault", SlotID: "workspace-snapshot-fault", State: "ARCHIVED", AgentKind: "codex", TokenHash: HashToken("workspace-snapshot-fault")}, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`CREATE TRIGGER fail_workspace_snapshot_insert BEFORE INSERT ON workspace_snapshots BEGIN SELECT RAISE(ABORT,'fault'); END`); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveWorkspaceSnapshot(ctx, WorkspaceSnapshot{SessionID: "workspace-snapshot-fault", RootID: testRootID, RelPath: "_recovery/workspace-snapshots/fault.tar", SHA256: "sha", Status: "ARCHIVED", CreatedAt: now(), ExpiresAt: now()}); err == nil {
		t.Fatal("workspace snapshot save succeeded despite an insertion fault")
	}
}

func TestExpireSessionSnapshotsPropagatesDeletionFaults(t *testing.T) {
	ctx := context.Background()

	t.Run("repository snapshot deletion fault", func(t *testing.T) {
		store := openTestStore(t)
		seedWorkspace(t, store)
		if _, err := store.CreateSlotSession(ctx, Slot{ID: "expire-fault", State: "ARCHIVED", RootID: testRootID, RelPath: "_unbound/expire-fault"}, nil, Session{ID: "expire-fault", SlotID: "expire-fault", State: "ARCHIVED", AgentKind: "codex", TokenHash: HashToken("expire-fault")}, ""); err != nil {
			t.Fatal(err)
		}
		if _, err := store.db.Exec(`INSERT INTO snapshots(id,session_id,repository_id,head_oid,head_recovery_ref,index_tree_oid,worktree_snapshot_oid,worktree_recovery_ref,status,created_at,expires_at) VALUES ('snap-fault','expire-fault','repository','head','refs/wx/head','tree','worktree','refs/wx/worktree','ARCHIVED',?,?)`, now(), now()); err != nil {
			t.Fatal(err)
		}
		if _, err := store.db.Exec(`CREATE TRIGGER fail_expire_snapshot BEFORE DELETE ON snapshots WHEN OLD.session_id='expire-fault' BEGIN SELECT RAISE(ABORT,'fault'); END`); err != nil {
			t.Fatal(err)
		}
		if err := store.ExpireSessionSnapshots(ctx, "expire-fault"); err == nil {
			t.Fatal("session snapshot expiry succeeded despite a repository snapshot deletion fault")
		}
	})

	t.Run("workspace snapshot deletion fault", func(t *testing.T) {
		store := openTestStore(t)
		seedWorkspace(t, store)
		if _, err := store.CreateSlotSession(ctx, Slot{ID: "expire-ws-fault", State: "ARCHIVED", RootID: testRootID, RelPath: "_unbound/expire-ws-fault"}, nil, Session{ID: "expire-ws-fault", SlotID: "expire-ws-fault", State: "ARCHIVED", AgentKind: "codex", TokenHash: HashToken("expire-ws-fault")}, ""); err != nil {
			t.Fatal(err)
		}
		if err := store.SaveWorkspaceSnapshot(ctx, WorkspaceSnapshot{SessionID: "expire-ws-fault", RootID: testRootID, RelPath: "_recovery/workspace-snapshots/expire.tar", SHA256: "sha", Status: "ARCHIVED", CreatedAt: now(), ExpiresAt: now()}); err != nil {
			t.Fatal(err)
		}
		if _, err := store.db.Exec(`CREATE TRIGGER fail_expire_workspace_snapshot BEFORE DELETE ON workspace_snapshots WHEN OLD.session_id='expire-ws-fault' BEGIN SELECT RAISE(ABORT,'fault'); END`); err != nil {
			t.Fatal(err)
		}
		if err := store.ExpireSessionSnapshots(ctx, "expire-ws-fault"); err == nil {
			t.Fatal("session snapshot expiry succeeded despite a workspace snapshot deletion fault")
		}
	})
}
