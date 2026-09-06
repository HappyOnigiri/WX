package state

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

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

// TestStatusDiagnosticsReportsWorkspaceLastUsedFromSessions は workspace_details.last_used_at の集計元を固定する。
// workspace 自身の session を数えるため、repository 数に関わらず値が入り、repository 行を共有する別 workspace の貸出では値が入らない。
func TestStatusDiagnosticsReportsWorkspaceLastUsedFromSessions(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	seedWorkspaceRows(t, store, "multi", "/multi", "multi_repository", "multi-a", "/multi/a", "/multi/a/.git", "a")
	seedWorkspaceRows(t, store, "single", "/single", "repository", "single-repo", "/single", "/single/.git", "")
	seedWorkspaceRows(t, store, "unused", "/unused", "repository", "unused-repo", "/unused", "/unused/.git", "")
	if _, err := store.db.ExecContext(ctx, `INSERT INTO repositories(id,main_worktree_path,common_git_dir,default_branch,remote_name,first_seen_at,last_seen_at) VALUES('multi-b','/multi/b','/multi/b/.git','main','',?,?)`, now(), now()); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `INSERT INTO workspace_repositories(workspace_id,repository_id,relative_path,ordinal) VALUES('multi','multi-b','b',1)`); err != nil {
		t.Fatal(err)
	}
	// multi の配下 repository を root とする workspace を足し、repository 行を multi と共有させる。
	if _, err := store.db.ExecContext(ctx, `INSERT INTO workspaces(id,root_path,kind,generation,discovery_state,first_seen_at,last_seen_at,last_reconciled_at) VALUES('shared','/multi/a','repository',1,'READY',?,?,?)`, now(), now(), now()); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `INSERT INTO workspace_repositories(workspace_id,repository_id,relative_path,ordinal) VALUES('shared','multi-a','',0)`); err != nil {
		t.Fatal(err)
	}
	// 貸出は実際の経路で行い、multi には 2 セッションを与えて created_at の最大値が採られることを示す。
	for _, session := range []Session{
		{ID: "multi-older", WorkspaceID: "multi", SlotID: "multi-slot-older", State: "STARTING", AgentKind: "codex", TokenHash: HashToken("older")},
		{ID: "multi-newer", WorkspaceID: "multi", SlotID: "multi-slot-newer", State: "STARTING", AgentKind: "codex", TokenHash: HashToken("newer")},
		{ID: "single-session", WorkspaceID: "single", SlotID: "single-slot", State: "STARTING", AgentKind: "codex", TokenHash: HashToken("single")},
	} {
		slot := Slot{ID: session.SlotID, WorkspaceID: session.WorkspaceID, Generation: 1, RootID: testRootID, RelPath: session.SlotID, State: "PREPARING"}
		if _, err := store.CreateSlotSession(ctx, slot, nil, session, ""); err != nil {
			t.Fatal(err)
		}
	}
	older, newer := FormatTime(time.Now().Add(-2*time.Hour)), FormatTime(time.Now().Add(-time.Hour))
	if _, err := store.db.ExecContext(ctx, `UPDATE sessions SET created_at=CASE id WHEN 'multi-older' THEN ? ELSE ? END WHERE id IN ('multi-older','multi-newer')`, older, newer); err != nil {
		t.Fatal(err)
	}
	diagnostics, err := store.StatusDiagnostics(ctx)
	if err != nil {
		t.Fatal(err)
	}
	lastUsed := map[string]string{}
	repositories := map[string]int{}
	for _, workspace := range diagnostics.Workspaces {
		lastUsed[workspace.ID] = workspace.LastUsedAt
		repositories[workspace.ID] = workspace.Repositories
	}
	if repositories["multi"] != 2 || lastUsed["multi"] != newer {
		t.Fatalf("multi-repository workspace repositories=%d last_used_at=%q, want 2 and %q", repositories["multi"], lastUsed["multi"], newer)
	}
	var singleCreatedAt string
	if err := store.db.QueryRowContext(ctx, `SELECT created_at FROM sessions WHERE id='single-session'`).Scan(&singleCreatedAt); err != nil {
		t.Fatal(err)
	}
	if lastUsed["single"] == "" || lastUsed["single"] != singleCreatedAt {
		t.Fatalf("single-repository workspace last_used_at=%q, want the session value %q", lastUsed["single"], singleCreatedAt)
	}
	// multi の貸出で multi-a の last_leased_at は入るが、それを共有する shared workspace 自身は未使用のままである。
	var sharedLeasedAt string
	if err := store.db.QueryRowContext(ctx, `SELECT COALESCE(last_leased_at,'') FROM repositories WHERE id='multi-a'`).Scan(&sharedLeasedAt); err != nil {
		t.Fatal(err)
	}
	if sharedLeasedAt == "" {
		t.Fatal("multi-a last_leased_at is empty, want the lease timestamp")
	}
	if lastUsed["shared"] != "" {
		t.Fatalf("workspace sharing a leased repository last_used_at=%q, want empty", lastUsed["shared"])
	}
	if lastUsed["unused"] != "" {
		t.Fatalf("never used workspace last_used_at=%q, want empty", lastUsed["unused"])
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
	if _, err := store.QuarantineArtifact(ctx, "test", filepath.Join(root, "artifact"), "TEST"); err != nil {
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
