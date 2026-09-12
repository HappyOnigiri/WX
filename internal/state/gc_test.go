package state

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/HappyOnigiri/WX/internal/discovery"
)

func TestColdRemovalSchedulingIsIdempotent(t *testing.T) {
	t.Parallel()
	store := openTestStore(t)
	seedWorkspace(t, store)
	ctx := context.Background()
	repository := SlotRepository{RepositoryID: "repository", DirName: "repository", State: "READY", RequestedRef: "main", BaseOID: "head", Fingerprint: "fp"}
	job, err := store.CreateStandby(ctx, Slot{ID: "ready", WorkspaceID: "workspace", Generation: 1, RootID: testRootID, RelPath: "workspace/ready", State: "PREPARING"}, []SlotRepository{repository})
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

func TestPruneMetadataReportsLateSchemaFaults(t *testing.T) {
	t.Parallel()
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

func TestColdRemovalCompletionAndAdministrativeQueries(t *testing.T) {
	t.Parallel()
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
	if _, err := store.FinishRemoval(ctx, "cold"); err != nil {
		t.Fatal(err)
	}
	archived, err := store.Slot(ctx, "cold")
	if err != nil || archived.State != "ARCHIVED" {
		t.Fatalf("finished removal slot=%+v err=%v", archived, err)
	}
}

func TestStandbyGCKeepsWarmSlotsAndReportsStaleRows(t *testing.T) {
	t.Parallel()
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
	candidates, err := store.StandbyGCCandidates(ctx, constantWarm(1))
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

func TestStandbyGCUsesWorkspaceWarmOverrides(t *testing.T) {
	t.Parallel()
	store := openTestStore(t)
	seedWorkspace(t, store)
	seedWorkspaceRows(t, store, "zero", "/zero", "repository", "zero-repository", "/zero", "/zero/.git", "")
	seedWorkspaceRows(t, store, "positive", "/positive", "repository", "positive-repository", "/positive", "/positive/.git", "")
	ctx := context.Background()
	for _, slot := range []Slot{
		{ID: "zero-slot", WorkspaceID: "workspace", Generation: 1, RootID: testRootID, RelPath: "workspace/zero-slot", State: "READY"},
		{ID: "positive-a", WorkspaceID: "positive", Generation: 1, RootID: testRootID, RelPath: "positive/a", State: "READY"},
		{ID: "positive-b", WorkspaceID: "positive", Generation: 1, RootID: testRootID, RelPath: "positive/b", State: "READY"},
		{ID: "zero-override", WorkspaceID: "zero", Generation: 1, RootID: testRootID, RelPath: "zero/override", State: "READY"},
	} {
		if _, err := store.CreateStandby(ctx, slot, nil); err != nil {
			t.Fatal(err)
		}
	}
	candidates, err := store.StandbyGCCandidates(ctx, warmByRoot(0, map[string]int{"/positive": 1, "/zero": 0}))
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, candidate := range candidates {
		seen[candidate.SlotID] = true
	}
	if !seen["zero-slot"] || !seen["zero-override"] || !seen["positive-a"] || seen["positive-b"] {
		t.Fatalf("workspace-specific GC candidates=%v", seen)
	}
}

func TestColdRepositoryCandidatesUseWorkspaceWarmOverrides(t *testing.T) {
	t.Parallel()
	store := openTestStore(t)
	seedWorkspace(t, store)
	seedWorkspaceRows(t, store, "positive", "/positive", "repository", "positive-repository", "/positive", "/positive/.git", "")
	ctx := context.Background()
	for _, slot := range []struct {
		id, workspace, repository string
	}{
		{"global-zero", "workspace", "repository"},
		{"override-positive", "positive", "positive-repository"},
	} {
		if _, err := store.CreateStandby(ctx, Slot{ID: slot.id, WorkspaceID: slot.workspace, Generation: 1, RootID: testRootID, RelPath: slot.workspace + "/" + slot.id, State: "READY"}, []SlotRepository{{RepositoryID: slot.repository, DirName: "repository", State: "READY", BaseOID: "head"}}); err != nil {
			t.Fatal(err)
		}
	}
	candidates, err := store.ColdRepositoryCandidatesForWarm(ctx, FormatTime(time.Now().Add(time.Hour)), warmByRoot(0, map[string]int{"/positive": 1}), constantBefore(FormatTime(time.Now().Add(time.Hour))))
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 1 || candidates[0].SlotID != "override-positive" {
		t.Fatalf("cold candidates=%+v", candidates)
	}
}

func TestScheduleColdRepositoryRemovalPropagatesTransactionFaults(t *testing.T) {
	t.Parallel()
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

func TestScheduleRemovalPropagatesJobInsertionFault(t *testing.T) {
	t.Parallel()
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

func TestFinishColdRepositoryRemovalPropagatesSlotReadyFault(t *testing.T) {
	t.Parallel()
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

func TestLifecycleCandidateQueriesCoverWarmStaleAndColdTransitions(t *testing.T) {
	t.Parallel()
	store := openTestStore(t)
	seedWorkspace(t, store)
	ctx := context.Background()
	root := t.TempDir()
	ready := Slot{ID: "ready-candidate", WorkspaceID: "workspace", Generation: 1, RootID: testRootID, RelPath: "workspace/ready-candidate", State: "READY"}
	if _, err := store.CreateStandby(ctx, ready, []SlotRepository{{RepositoryID: "repository", WorktreePath: filepath.Join(root, "ready", "repository"), State: "READY", RequestedRef: "main", BaseOID: "head", Fingerprint: "fingerprint"}}); err != nil {
		t.Fatal(err)
	}
	stale := Slot{ID: "stale-candidate", WorkspaceID: "workspace", Generation: 1, RootID: testRootID, RelPath: "workspace/stale-candidate", State: "STALE"}
	if _, err := store.CreateStandby(ctx, stale, nil); err != nil {
		t.Fatal(err)
	}
	if candidates, err := store.StandbyGCCandidates(ctx, constantWarm(1)); err != nil || len(candidates) != 1 || candidates[0].SlotID != stale.ID {
		t.Fatalf("standby candidates=%+v err=%v", candidates, err)
	}
	if candidates, err := store.ColdRepositoryCandidates(ctx, FormatTime(time.Now().Add(time.Hour))); err != nil || len(candidates) != 1 || candidates[0].SlotID != ready.ID {
		t.Fatalf("cold candidates=%+v err=%v", candidates, err)
	}
	job, changed, err := store.ScheduleColdRepositoryRemoval(ctx, ColdRepositoryCandidate{SlotID: ready.ID, WorkspaceID: "workspace", RepositoryID: "repository", WorktreePath: filepath.Join(root, "ready", "repository")})
	if err != nil || !changed || job.Kind != "REMOVE_REPOSITORY" {
		t.Fatalf("schedule cold removal job=%+v changed=%v err=%v", job, changed, err)
	}
	if err := store.FinishColdRepositoryRemoval(ctx, ready.ID, "repository"); err != nil {
		t.Fatal(err)
	}
	stored, err := store.Slot(ctx, ready.ID)
	if err != nil || stored.State != "READY" {
		t.Fatalf("finished cold removal slot=%+v err=%v", stored, err)
	}
}

func TestHotRepositoryIDsExcludesNeverLeasedAndStaleRepositories(t *testing.T) {
	t.Parallel()
	store := openTestStore(t)
	ctx := context.Background()
	w := discovery.Workspace{ID: "workspace", Root: "/workspace", Kind: "multi_repository", Repositories: []discovery.Repository{
		{ID: "never-leased", MainPath: "/workspace/never-leased", CommonDir: "/workspace/never-leased/.git", RelativePath: "never-leased", DefaultBranch: "main"},
		{ID: "stale", MainPath: "/workspace/stale", CommonDir: "/workspace/stale/.git", RelativePath: "stale", DefaultBranch: "main"},
		{ID: "hot", MainPath: "/workspace/hot", CommonDir: "/workspace/hot/.git", RelativePath: "hot", DefaultBranch: "main"},
	}}
	if _, _, err := store.UpsertWorkspaceGeneration(ctx, w); err != nil {
		t.Fatal(err)
	}
	hotBefore := FormatTime(time.Now().Add(-time.Hour))
	if _, err := store.db.ExecContext(ctx, `UPDATE repositories SET last_leased_at=? WHERE id='stale'`, FormatTime(time.Now().Add(-2*time.Hour))); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE repositories SET last_leased_at=? WHERE id='hot'`, FormatTime(time.Now())); err != nil {
		t.Fatal(err)
	}
	hot, err := store.HotRepositoryIDs(ctx, hotBefore)
	if err != nil {
		t.Fatal(err)
	}
	if hot["never-leased"] {
		t.Fatal("a repository with no recorded lease was reported hot")
	}
	if hot["stale"] {
		t.Fatal("a repository last leased before the cutoff was reported hot")
	}
	if !hot["hot"] {
		t.Fatal("a repository leased after the cutoff was not reported hot")
	}
}

// TestCountMetadataCandidatesStopsCountingAlreadyTombstonedSessions は、dry-run の集計が
// PruneMetadata の実処理と一致することを固定する。tombstone 化で agent_session_id が消えるため、
// 処理済み session を数えると `wx gc --dry-run` が変化しない非ゼロ値を報告し続ける。
func TestCountMetadataCandidatesStopsCountingAlreadyTombstonedSessions(t *testing.T) {
	t.Parallel()
	store := openTestStore(t)
	seedWorkspace(t, store)
	ctx := context.Background()
	session := Session{ID: "session", WorkspaceID: "workspace", SlotID: "slot", State: "ACTIVE", AgentKind: "codex", TokenHash: HashToken("token")}
	if _, err := store.CreateSlotSession(ctx, Slot{ID: "slot", WorkspaceID: "workspace", Generation: 1, RootID: testRootID, RelPath: "workspace/slot", State: "LEASED"}, nil, session, ""); err != nil {
		t.Fatal(err)
	}
	expired := FormatTime(time.Now().Add(-time.Hour))
	if _, err := store.db.ExecContext(ctx, `UPDATE sessions SET state='EXPIRED',expires_at=?,agent_session_id='agent' WHERE id=?`, expired, session.ID); err != nil {
		t.Fatal(err)
	}
	// 他の3層を空にして、合計をこの tombstone だけにする。
	past := FormatTime(time.Now().Add(-24 * time.Hour))
	tombstoneBefore := FormatTime(time.Now())
	count, err := store.CountMetadataCandidates(ctx, past, past, tombstoneBefore)
	if err != nil || count != 1 {
		t.Fatalf("candidate count before pruning=%d err=%v, want 1", count, err)
	}
	if err := store.PruneMetadata(ctx, past, past, tombstoneBefore); err != nil {
		t.Fatal(err)
	}
	count, err = store.CountMetadataCandidates(ctx, past, past, tombstoneBefore)
	if err != nil || count != 0 {
		t.Fatalf("candidate count after pruning=%d err=%v, want 0", count, err)
	}
}

// 貸出が進行中でlast_leased_atがまだ無いrepositoryも hot として返す。
// 使用中のrepositoryをcoldと判定すると、補充が COLD の待機枠を作り、次の貸出が cold start になるためである。
func TestHotRepositoryIDsIncludesInFlightLease(t *testing.T) {
	t.Parallel()
	store := openTestStore(t)
	seedWorkspace(t, store)
	ctx := context.Background()
	hotBefore := FormatTime(time.Now().UTC())
	hot, err := store.HotRepositoryIDs(ctx, hotBefore)
	if err != nil || hot["repository"] {
		t.Fatalf("repository without a lease was hot: hot=%+v err=%v", hot, err)
	}
	slot := Slot{ID: "slot", WorkspaceID: "workspace", Generation: 1, RootID: testRootID, RelPath: "workspace/slot", OwnerSessionID: "session"}
	if err := store.ReserveSlot(ctx, slot); err != nil {
		t.Fatal(err)
	}
	hot, err = store.HotRepositoryIDs(ctx, hotBefore)
	if err != nil || hot["repository"] {
		t.Fatalf("reservation without repositories was hot: hot=%+v err=%v", hot, err)
	}
	if err := store.ConfirmSlotCreation(ctx, slot.ID, "identity"); err != nil {
		t.Fatal(err)
	}
	repository := SlotRepository{RepositoryID: "repository", DirName: "repository", State: "PREPARING", RequestedRef: "main", BaseOID: "head", Fingerprint: "fp"}
	session := Session{ID: "session", WorkspaceID: "workspace", SlotID: slot.ID, State: "STARTING", AgentKind: "codex", TokenHash: HashToken("token")}
	if _, err := store.RegisterReservedSlotSession(ctx, slot.ID, []SlotRepository{repository}, session, "PREPARING", "PREPARE"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE repositories SET last_leased_at=NULL WHERE id='repository'`); err != nil {
		t.Fatal(err)
	}
	hot, err = store.HotRepositoryIDs(ctx, hotBefore)
	if err != nil || !hot["repository"] {
		t.Fatalf("repository with an in-flight lease was cold: hot=%+v err=%v", hot, err)
	}
}

// constantWarm は root によらず同じ待機枠数を返す。
func constantWarm(n int) func(string) int { return func(string) int { return n } }

// warmByRoot は root ごとの待機枠数を返し、指定の無い root には既定値を返す。
func warmByRoot(base int, overrides map[string]int) func(string) int {
	return func(root string) int {
		if n, ok := overrides[root]; ok {
			return n
		}
		return base
	}
}

// constantBefore は root によらず同じ cutoff を返す。
func constantBefore(before string) func(string) string { return func(string) string { return before } }

// 終了 worktree の候補は workspace ごとの cutoff で決める。
// SQL には最短の保持期間から作った緩い floor だけを置き、長い保持期間の slot は Go 側で落とす。
func TestGCCandidatesApplyPerWorkspaceCutoffs(t *testing.T) {
	t.Parallel()
	store := openTestStore(t)
	seedWorkspace(t, store)
	seedWorkspaceRows(t, store, "long", "/long", "repository", "long-repository", "/long", "/long/.git", "")
	ctx := context.Background()
	archivedAt := FormatTime(time.Now().Add(-30 * time.Minute))
	for _, row := range []struct{ slot, workspace string }{{"short-slot", "workspace"}, {"long-slot", "long"}, {"orphan-slot", ""}} {
		workspaceID := any(row.workspace)
		if row.workspace == "" {
			workspaceID = nil
		}
		if _, err := store.db.ExecContext(ctx, `INSERT INTO slots(id,workspace_id,generation,root_id,rel_path,state,created_at,updated_at) VALUES(?,?,1,?,?,'SNAPSHOTTED',?,?)`, row.slot, workspaceID, testRootID, "x/"+row.slot, now(), now()); err != nil {
			t.Fatal(err)
		}
		if _, err := store.db.ExecContext(ctx, `INSERT INTO sessions(id,workspace_id,slot_id,state,agent_kind,client_pid,session_token_hash,created_at,archived_at) VALUES(?,?,?,'ARCHIVED','codex',1,x'00',?,?)`, "session-"+row.slot, "workspace", row.slot, now(), archivedAt); err != nil {
			t.Fatal(err)
		}
	}
	floor := FormatTime(time.Now().Add(-time.Minute))
	before := func(root string) string {
		if root == "/long" {
			return FormatTime(time.Now().Add(-time.Hour))
		}
		return floor
	}
	candidates, err := store.GCCandidates(ctx, floor, before)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, candidate := range candidates {
		seen[candidate.SlotID] = true
	}
	// workspace 未紐付けの slot は global 値で判定する。
	if !seen["short-slot"] || !seen["orphan-slot"] || seen["long-slot"] {
		t.Fatalf("candidates=%v, want the longer retention respected", seen)
	}
}

// COLD 化の判定も workspace ごとの cutoff で行う。
func TestColdRepositoryCandidatesApplyPerWorkspaceCutoffs(t *testing.T) {
	t.Parallel()
	store := openTestStore(t)
	seedWorkspace(t, store)
	seedWorkspaceRows(t, store, "long", "/long", "repository", "long-repository", "/long", "/long/.git", "")
	ctx := context.Background()
	leasedAt := FormatTime(time.Now().Add(-30 * time.Minute))
	for _, repository := range []string{"repository", "long-repository"} {
		if _, err := store.db.ExecContext(ctx, `UPDATE repositories SET last_leased_at=? WHERE id=?`, leasedAt, repository); err != nil {
			t.Fatal(err)
		}
	}
	for _, row := range []struct{ slot, workspace, repository string }{
		{"short-slot", "workspace", "repository"},
		{"long-slot", "long", "long-repository"},
	} {
		if _, err := store.CreateStandby(ctx, Slot{ID: row.slot, WorkspaceID: row.workspace, Generation: 1, RootID: testRootID, RelPath: row.workspace + "/" + row.slot, State: "READY"}, []SlotRepository{{RepositoryID: row.repository, DirName: "repository", State: "READY", BaseOID: "head"}}); err != nil {
			t.Fatal(err)
		}
	}
	floor := FormatTime(time.Now().Add(-time.Minute))
	hotBefore := func(root string) string {
		if root == "/long" {
			return FormatTime(time.Now().Add(-time.Hour))
		}
		return floor
	}
	candidates, err := store.ColdRepositoryCandidatesForWarm(ctx, floor, constantWarm(1), hotBefore)
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 1 || candidates[0].SlotID != "short-slot" {
		t.Fatalf("cold candidates=%+v, want only the shorter retention", candidates)
	}
}
