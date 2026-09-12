package state

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestWorkspaceDiagnosticJSONIncludesMembershipContext(t *testing.T) {
	t.Parallel()
	item := WorkspaceDiagnostic{
		ID: "workspace", Root: "/workspace", Kind: "multi_repository", Generation: 3,
		Repositories: 2, RepositoryMemberships: []WorkspaceRepositoryMembership{{ID: "repo", MainPath: "/src/repo", RelativePath: "frontend"}},
	}
	data, err := json.Marshal(item)
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal(data, &payload); err != nil {
		t.Fatal(err)
	}
	if payload["kind"] != "multi_repository" || payload["repositories"] != float64(2) {
		t.Fatalf("workspace JSON=%v", payload)
	}
	members, ok := payload["repository_memberships"].([]any)
	if !ok || len(members) != 1 {
		t.Fatalf("workspace memberships=%v", payload["repository_memberships"])
	}
	member, ok := members[0].(map[string]any)
	if !ok || member["id"] != "repo" || member["main_path"] != "/src/repo" || member["relative_path"] != "frontend" {
		t.Fatalf("membership JSON=%v", members[0])
	}
}

func TestStatusDiagnosticsFailsAtEachSchemaBoundary(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	// この test の session は ARCHIVED だけなので、一覧は空で集計側に 1 件入る。
	if len(diagnostics.Workspaces) != 1 || len(diagnostics.Sessions) != 0 || len(diagnostics.Repositories) != 1 || diagnostics.Jobs.Pending < 1 || diagnostics.Snapshots.Count != 1 || len(diagnostics.Quarantine) != 1 {
		t.Fatalf("diagnostics=%+v", diagnostics)
	}
	if diagnostics.ArchivedSessions.Count != 1 || diagnostics.ArchivedSessions.EarliestArchivedAt == "" {
		t.Fatalf("archived sessions=%+v", diagnostics.ArchivedSessions)
	}
	if jobs, err := store.RecoverJobs(ctx, true); err != nil || len(jobs) == 0 || jobs[0].ID != job.ID {
		t.Fatalf("reclaimed jobs=%+v err=%v", jobs, err)
	}
	candidates, err := store.GCCandidates(ctx, FormatTime(time.Now().Add(time.Hour)), constantBefore(FormatTime(time.Now().Add(time.Hour))))
	if err != nil || len(candidates) != 1 || candidates[0].SlotID != "snapshot" || candidates[0].SessionID != session.ID {
		t.Fatalf("GC candidates=%+v err=%v", candidates, err)
	}
}

// TestListSlotsReturnsLiveSlotsWithRepositories は既定の一覧の範囲と REPO 列の元になる値を固定する。
// 実体が残る slot（ARCHIVED 以外）を全て返し、multi-repository slot は dir_name 順に main worktree path を並べる。
func TestListSlotsReturnsLiveSlotsWithRepositories(t *testing.T) {
	t.Parallel()
	store := openTestStore(t)
	ctx := context.Background()
	seedWorkspaceRows(t, store, "multi", "/multi", "multi_repository", "multi-a", "/multi/a", "/multi/a/.git", "a")
	if _, err := store.db.ExecContext(ctx, `INSERT INTO repositories(id,main_worktree_path,common_git_dir,default_branch,remote_name,first_seen_at,last_seen_at) VALUES('multi-b','/multi/b','/multi/b/.git','main','',?,?)`, now(), now()); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `INSERT INTO workspace_repositories(workspace_id,repository_id,relative_path,ordinal) VALUES('multi','multi-b','b',1)`); err != nil {
		t.Fatal(err)
	}
	repositories := []SlotRepository{
		{RepositoryID: "multi-b", DirName: "b", State: "READY", RequestedRef: "main", BaseOID: "head", Fingerprint: "fingerprint"},
		{RepositoryID: "multi-a", DirName: "a", State: "READY", RequestedRef: "main", BaseOID: "head", Fingerprint: "fingerprint"},
	}
	session := Session{ID: "snapshotted", WorkspaceID: "multi", SlotID: "snapshotted", State: "ARCHIVED", AgentKind: "codex", TokenHash: HashToken("token")}
	if _, err := store.CreateSlotSession(ctx, Slot{ID: session.SlotID, WorkspaceID: "multi", Generation: 1, RootID: testRootID, RelPath: "multi/snapshotted", State: "SNAPSHOTTED"}, repositories, session, ""); err != nil {
		t.Fatal(err)
	}
	// slot_repositories が未登録の slot も行として残し、REPO を空のまま返す。
	if _, err := store.CreateSlotSession(ctx, Slot{ID: "allocating", WorkspaceID: "multi", Generation: 1, RootID: testRootID, RelPath: "multi/allocating", State: "ALLOCATING"}, nil, Session{ID: "allocating", WorkspaceID: "multi", SlotID: "allocating", State: "STARTING", AgentKind: "codex", TokenHash: HashToken("allocating")}, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `INSERT INTO slots(id,workspace_id,generation,root_id,rel_path,state,created_at,updated_at) VALUES('archived','multi',1,?,'multi/archived','ARCHIVED',?,?)`, testRootID, now(), now()); err != nil {
		t.Fatal(err)
	}
	slots, err := store.ListSlots(ctx, false)
	if err != nil || len(slots) != 2 {
		t.Fatalf("live slot list=%+v err=%v", slots, err)
	}
	if slots[0].SlotID != "allocating" || len(slots[0].Repositories) != 0 {
		t.Fatalf("allocating slot=%+v", slots[0])
	}
	if slots[1].SlotID != "snapshotted" || !slices.Equal(slots[1].Repositories, []string{"/multi/a", "/multi/b"}) {
		t.Fatalf("snapshotted slot=%+v", slots[1])
	}
}

// 準備失敗で止まった補充は、停止行だけでは原因を説明できないため、失敗した job の理由を併せて返す。
func TestStandbyReplenishmentDiagnosticsCarryTheFailedJobCause(t *testing.T) {
	t.Parallel()
	store := openTestStore(t)
	seedWorkspace(t, store)
	ctx := context.Background()
	createSessionSlot(t, store, "standby", "STARTING", "PREPARING")
	job, err := store.CreateJob(ctx, "PREPARE", "workspace", "standby", "")
	if err != nil {
		t.Fatal(err)
	}
	failJob(t, store, job, errors.New("copy /src/AGENTS.md to /slot/AGENTS.md: permission denied"), "PREPARE_FAILED", "/logs/prepare.log")
	if err := store.SuspendReplenish(ctx, "workspace", SuspendReplenishReasonStandbyFailure, job.ID); err != nil {
		t.Fatal(err)
	}
	diagnostics, err := store.StandbyReplenishmentDiagnostics(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(diagnostics) != 1 {
		t.Fatalf("standby diagnostics=%+v", diagnostics)
	}
	item := diagnostics[0]
	if item.FailureCode != "PREPARE_FAILED" || item.DetailPath != "/logs/prepare.log" {
		t.Fatalf("standby diagnostic=%+v", item)
	}
	if item.FailureMessage != "copy /src/AGENTS.md to /slot/AGENTS.md: permission denied" {
		t.Fatalf("standby failure message=%q", item.FailureMessage)
	}
	// `wx clear` による停止は job を指さないため、失敗情報を持たない。
	if err := store.ResumeReplenish(ctx, "workspace"); err != nil {
		t.Fatal(err)
	}
	if err := store.SuspendReplenish(ctx, "workspace", SuspendReplenishReasonClean, "run-1"); err != nil {
		t.Fatal(err)
	}
	diagnostics, err = store.StandbyReplenishmentDiagnostics(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(diagnostics) != 1 || diagnostics[0].FailureCode != "" || diagnostics[0].FailureMessage != "" {
		t.Fatalf("standby diagnostics after wx clear=%+v", diagnostics)
	}
}

// 再試行待ちの error_code と成功した job は失敗の確定ではないため、失敗情報として引き継がない。
func TestStandbyReplenishmentDiagnosticsIgnoreJobsThatHaveNotFailed(t *testing.T) {
	t.Parallel()
	store := openTestStore(t)
	seedWorkspace(t, store)
	ctx := context.Background()
	createSessionSlot(t, store, "standby", "STARTING", "PREPARING")
	job, err := store.CreateJob(ctx, "PREPARE", "workspace", "standby", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SuspendReplenish(ctx, "workspace", SuspendReplenishReasonStandbyFailure, job.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ClaimJob(ctx, job.ID, "test"); err != nil {
		t.Fatal(err)
	}
	if err := store.RetryJob(ctx, job.ID, "test", 0, "DEPENDENCY_PENDING"); err != nil {
		t.Fatal(err)
	}
	diagnostics, err := store.StandbyReplenishmentDiagnostics(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(diagnostics) != 1 {
		t.Fatalf("standby diagnostics=%+v", diagnostics)
	}
	if diagnostics[0].FailureCode != "" || diagnostics[0].FailureMessage != "" || diagnostics[0].DetailPath != "" {
		t.Fatalf("standby diagnostic while retrying=%+v", diagnostics[0])
	}
	if _, err := store.ClaimJob(ctx, job.ID, "test"); err != nil {
		t.Fatal(err)
	}
	if err := store.FinishJob(ctx, job.ID, "test", nil); err != nil {
		t.Fatal(err)
	}
	diagnostics, err = store.StandbyReplenishmentDiagnostics(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(diagnostics) != 1 {
		t.Fatalf("standby diagnostics=%+v", diagnostics)
	}
	if diagnostics[0].FailureCode != "" || diagnostics[0].FailureMessage != "" || diagnostics[0].DetailPath != "" {
		t.Fatalf("standby diagnostic after success=%+v", diagnostics[0])
	}
}

// wx slots --json は貸出の種別・期限・親 session を行に載せる。
// slot を手放した session の行（--all）でも同じ列が読めることを併せて確認する。
func TestListSlotsCarryLeaseAttributes(t *testing.T) {
	t.Parallel()
	store := openTestStore(t)
	seedWorkspace(t, store)
	ctx := context.Background()
	expiry := FormatTime(time.Now().Add(72 * time.Hour))
	seedLease(t, store, "owner", LeaseKindShell, "", "", 0)
	seedLease(t, store, "child", LeaseKindPath, expiry, "owner", 0)
	slots, err := store.ListSlots(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]SlotSummary{}
	for _, slot := range slots {
		byID[slot.SlotID] = slot
	}
	if got := byID["child"]; got.LeaseKind != LeaseKindPath || got.LeaseExpiresAt != expiry || got.LeaseOwnerSessionID != "owner" {
		t.Fatalf("child lease row=%+v", got)
	}
	if got := byID["owner"]; got.LeaseKind != LeaseKindShell || got.LeaseExpiresAt != "" || got.LeaseOwnerSessionID != "" {
		t.Fatalf("owner lease row=%+v", got)
	}
	// slot を手放した session も同じ列を返す。
	if _, err := store.db.ExecContext(ctx, `UPDATE slots SET owner_session_id=NULL WHERE id='child'`); err != nil {
		t.Fatal(err)
	}
	all, err := store.ListSlots(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, slot := range all {
		if slot.SlotID == "" && slot.SessionID == "child" {
			found = slot.LeaseKind == LeaseKindPath && slot.LeaseOwnerSessionID == "owner"
		}
	}
	if !found {
		t.Fatalf("detached lease session row missing lease columns: %+v", all)
	}
}

// TestStatusDiagnosticsSplitsArchivedSessionsFromTheList は Sessions 診断の範囲を固定する。
// 終端でない state は名前を問わず一覧に残り、ARCHIVED は集計だけに、EXPIRED はどちらにも出ない。
func TestStatusDiagnosticsSplitsArchivedSessionsFromTheList(t *testing.T) {
	t.Parallel()
	store := openTestStore(t)
	seedWorkspace(t, store)
	ctx := context.Background()
	liveStates := []string{"STARTING", "ACTIVE", "RESTORING", "UNBOUND", "RELEASING", "SNAPSHOTTING", "QUARANTINED"}
	older, newer := FormatTime(time.Now().Add(-2*time.Hour)), FormatTime(time.Now().Add(-time.Hour))
	earlyExpiry, lateExpiry := FormatTime(time.Now().Add(time.Hour)), FormatTime(time.Now().Add(2*time.Hour))
	type seed struct{ id, sessionState, archivedAt, expiresAt string }
	seeds := []seed{
		{id: "archived-older", sessionState: "ARCHIVED", archivedAt: older, expiresAt: earlyExpiry},
		{id: "archived-newer", sessionState: "ARCHIVED", archivedAt: newer, expiresAt: lateExpiry},
		{id: "expired", sessionState: "EXPIRED"},
	}
	for _, sessionState := range liveStates {
		seeds = append(seeds, seed{id: "live-" + sessionState, sessionState: sessionState})
	}
	for _, item := range seeds {
		session := Session{ID: item.id, WorkspaceID: "workspace", SlotID: item.id, State: "STARTING", AgentKind: "codex", TokenHash: HashToken(item.id)}
		slot := Slot{ID: item.id, WorkspaceID: "workspace", Generation: 1, RootID: testRootID, RelPath: "workspace/" + item.id, State: "PREPARING"}
		if _, err := store.CreateSlotSession(ctx, slot, nil, session, ""); err != nil {
			t.Fatal(err)
		}
		if _, err := store.db.ExecContext(ctx, `UPDATE sessions SET state=?,archived_at=NULLIF(?,''),expires_at=NULLIF(?,'') WHERE id=?`, item.sessionState, item.archivedAt, item.expiresAt, item.id); err != nil {
			t.Fatal(err)
		}
	}
	diagnostics, err := store.StatusDiagnostics(ctx)
	if err != nil {
		t.Fatal(err)
	}
	listed := make([]string, 0, len(diagnostics.Sessions))
	for _, item := range diagnostics.Sessions {
		listed = append(listed, item.State)
	}
	slices.Sort(listed)
	want := slices.Clone(liveStates)
	slices.Sort(want)
	if !slices.Equal(listed, want) {
		t.Fatalf("listed session states=%v, want %v", listed, want)
	}
	archived := diagnostics.ArchivedSessions
	if archived.Count != 2 || archived.EarliestArchivedAt != older || archived.LatestExpiresAt != lateExpiry {
		t.Fatalf("archived sessions=%+v, want count 2 with earliest %q and latest expiry %q", archived, older, lateExpiry)
	}
}

// TestStatusDiagnosticsFailsWhenArchivedSessionColumnsAreMissing は、一覧が引けても集計が引けない DB を失敗として扱うことを固定する。
func TestStatusDiagnosticsFailsWhenArchivedSessionColumnsAreMissing(t *testing.T) {
	t.Parallel()
	store := openTestStore(t)
	for _, statement := range []string{
		`DROP TABLE sessions`,
		`CREATE VIEW sessions AS SELECT 'id' AS id,'codex' AS agent_kind,'ACTIVE' AS state,'created' AS created_at,'slot' AS slot_id`,
	} {
		if _, err := store.db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.StatusDiagnostics(context.Background()); err == nil {
		t.Fatal("diagnostics succeeded without the archived session columns")
	}
}

// TestStatusDiagnosticsSeparatesDiscardedJobsFromFailures は、state='FAILED' の job を error_code で 2 つに分けて数えることを固定する。
// 取り消した予定 job は記録として FAILED のまま残るため、集計で除かないと対処が必要な失敗の件数が読めない。
func TestStatusDiagnosticsSeparatesDiscardedJobsFromFailures(t *testing.T) {
	t.Parallel()
	store := openTestStore(t)
	ctx := context.Background()
	for _, job := range []struct{ id, state, code string }{
		{id: "discarded-snapshot", state: "FAILED", code: JobErrorCodeDiscarded},
		{id: "discarded-remove", state: "FAILED", code: JobErrorCodeDiscarded},
		{id: "failed-with-code", state: "FAILED", code: "JOB_FAILED"},
		// error_code を持たない FAILED も失敗として数える。取り消しだけが除外の対象である。
		{id: "failed-without-code", state: "FAILED"},
		{id: "queued", state: "PENDING"},
		{id: "running", state: "RUNNING"},
		{id: "succeeded", state: "SUCCEEDED"},
	} {
		if _, err := store.db.ExecContext(ctx, `INSERT INTO jobs(id,kind,state,attempt,not_before,error_code) VALUES(?,'SNAPSHOT',?,0,NULL,?)`, job.id, job.state, nullString(job.code)); err != nil {
			t.Fatal(err)
		}
	}
	diagnostics, err := store.StatusDiagnostics(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := JobDiagnostic{Pending: 1, Running: 1, Failed: 2, Discarded: 2}
	if diagnostics.Jobs != want {
		t.Fatalf("job diagnostics=%+v, want %+v", diagnostics.Jobs, want)
	}
}

// 補充計画の失敗は停止行を残さないため、最新の失敗を job 行から拾って報告する。
func TestUnresolvedStandbyPlanFailuresReportsTheLatestFailure(t *testing.T) {
	t.Parallel()
	store := openTestStore(t)
	seedWorkspace(t, store)
	ctx := context.Background()
	job, err := store.CreateJob(ctx, "ENSURE_STANDBY", "workspace", "", "")
	if err != nil {
		t.Fatal(err)
	}
	failJob(t, store, job, errors.New(`unsafe .worktreeinclude pattern "../escape"`), "JOB_FAILED", "/logs/ensure.log")
	failures, err := store.UnresolvedStandbyPlanFailures(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(failures) != 1 {
		t.Fatalf("plan failures=%+v, want the failed ENSURE_STANDBY", failures)
	}
	item := failures[0]
	if item.Reason != StandbyReplenishReasonPlanFailure || item.WorkspaceID != "workspace" || item.Root != "/workspace" {
		t.Fatalf("plan failure=%+v", item)
	}
	if item.Detail != job.ID || item.FailureCode != "JOB_FAILED" || item.DetailPath != "/logs/ensure.log" || item.FailedAt == "" {
		t.Fatalf("plan failure job fields=%+v", item)
	}
	if !strings.Contains(item.FailureMessage, "unsafe .worktreeinclude pattern") {
		t.Fatalf("plan failure message=%q, want the recorded reason", item.FailureMessage)
	}
	// 2 度続けて失敗しても、報告するのは最新の 1 件だけにする。
	second, err := store.CreateJob(ctx, "ENSURE_STANDBY", "workspace", "", "")
	if err != nil {
		t.Fatal(err)
	}
	failJob(t, store, second, errors.New("still unsafe"), "JOB_FAILED", "")
	failures, err = store.UnresolvedStandbyPlanFailures(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(failures) != 1 || failures[0].Detail != second.ID {
		t.Fatalf("plan failures after a second failure=%+v", failures)
	}
}

// 後続の補充計画が動いていれば、残った FAILED 行は未解消として報告しない。
func TestUnresolvedStandbyPlanFailuresExcludesResolvedPlans(t *testing.T) {
	t.Parallel()
	store := openTestStore(t)
	seedWorkspace(t, store)
	ctx := context.Background()
	job, err := store.CreateJob(ctx, "ENSURE_STANDBY", "workspace", "", "")
	if err != nil {
		t.Fatal(err)
	}
	failJob(t, store, job, errors.New("transient failure"), "JOB_FAILED", "")
	pending, err := store.CreateJob(ctx, "ENSURE_STANDBY", "workspace", "", "")
	if err != nil {
		t.Fatal(err)
	}
	failures, err := store.UnresolvedStandbyPlanFailures(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(failures) != 0 {
		t.Fatalf("plan failures while a later plan is pending=%+v", failures)
	}
	if _, err := store.ClaimJob(ctx, pending.ID, "test"); err != nil {
		t.Fatal(err)
	}
	if err := store.FinishJob(ctx, pending.ID, "test", nil); err != nil {
		t.Fatal(err)
	}
	failures, err = store.UnresolvedStandbyPlanFailures(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(failures) != 0 {
		t.Fatalf("plan failures after a later plan succeeded=%+v", failures)
	}
}

// `wx clear` などが取り消した job は失敗ではないので、報告にも解消の判定にも使わない。
func TestUnresolvedStandbyPlanFailuresIgnoresDiscardedJobs(t *testing.T) {
	t.Parallel()
	store := openTestStore(t)
	seedWorkspace(t, store)
	ctx := context.Background()
	discarded, err := store.CreateJob(ctx, "ENSURE_STANDBY", "workspace", "", "")
	if err != nil {
		t.Fatal(err)
	}
	failJob(t, store, discarded, errors.New("discarded by wx clear"), JobErrorCodeDiscarded, "")
	failures, err := store.UnresolvedStandbyPlanFailures(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(failures) != 0 {
		t.Fatalf("plan failures for a discarded job=%+v", failures)
	}
	failed, err := store.CreateJob(ctx, "ENSURE_STANDBY", "workspace", "", "")
	if err != nil {
		t.Fatal(err)
	}
	failJob(t, store, failed, errors.New("unsafe manifest"), "JOB_FAILED", "")
	later, err := store.CreateJob(ctx, "ENSURE_STANDBY", "workspace", "", "")
	if err != nil {
		t.Fatal(err)
	}
	failJob(t, store, later, errors.New("discarded by wx clear"), JobErrorCodeDiscarded, "")
	failures, err = store.UnresolvedStandbyPlanFailures(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(failures) != 1 || failures[0].Detail != failed.ID {
		t.Fatalf("plan failures after a discarded job=%+v", failures)
	}
}
