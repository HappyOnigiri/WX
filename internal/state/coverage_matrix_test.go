package state

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/HappyOnigiri/WX/internal/discovery"
)

func TestBackupRetentionSkipsNonDatabaseEntriesAndPrunesOldGenerations(t *testing.T) {
	store := openTestStore(t)
	seedWorkspace(t, store)
	ctx := context.Background()
	first, err := store.Backup(ctx, 10, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	backupDir := filepath.Dir(first)
	old := filepath.Join(backupDir, "00000000T000000.000000000Z.db")
	if err := os.WriteFile(old, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	oldTime := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(old, oldTime, oldTime); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(backupDir, "notes.txt"), []byte("ignore"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(backupDir, "ignored-dir"), 0o700); err != nil {
		t.Fatal(err)
	}
	second, err := store.Backup(ctx, 1, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if second == first {
		t.Fatal("consecutive backups reused their path")
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatalf("old backup survived retention: %v", err)
	}
	databaseBackups, err := filepath.Glob(filepath.Join(backupDir, "*.db"))
	if err != nil || len(databaseBackups) != 1 || databaseBackups[0] != second {
		t.Fatalf("retained backups=%v err=%v want %q", databaseBackups, err, second)
	}
}

func TestLifecycleCandidateQueriesCoverWarmStaleAndColdTransitions(t *testing.T) {
	store := openTestStore(t)
	seedWorkspace(t, store)
	ctx := context.Background()
	root := t.TempDir()
	ready := Slot{ID: "ready-candidate", WorkspaceID: "workspace", Generation: 1, Path: filepath.Join(root, "ready"), State: "READY"}
	if _, err := store.CreateStandby(ctx, ready, []SlotRepository{{RepositoryID: "repository", WorktreePath: filepath.Join(root, "ready", "repository"), State: "READY", RequestedRef: "main", BaseOID: "head", Fingerprint: "fingerprint"}}); err != nil {
		t.Fatal(err)
	}
	stale := Slot{ID: "stale-candidate", WorkspaceID: "workspace", Generation: 1, Path: filepath.Join(root, "stale"), State: "STALE"}
	if _, err := store.CreateStandby(ctx, stale, nil); err != nil {
		t.Fatal(err)
	}
	if candidates, err := store.StandbyGCCandidates(ctx, FormatTime(time.Now().Add(time.Hour)), 1); err != nil || len(candidates) != 1 || candidates[0].SlotID != stale.ID {
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
	store := openTestStore(t)
	ctx := context.Background()
	w := discovery.Workspace{ID: "workspace", Root: "/workspace", Kind: "multi_repository", Repositories: []discovery.Repository{
		{ID: "never-leased", MainPath: "/workspace/never-leased", CommonDir: "/workspace/never-leased/.git", RelativePath: "never-leased", DefaultBranch: "main"},
		{ID: "stale", MainPath: "/workspace/stale", CommonDir: "/workspace/stale/.git", RelativePath: "stale", DefaultBranch: "main"},
		{ID: "hot", MainPath: "/workspace/hot", CommonDir: "/workspace/hot/.git", RelativePath: "hot", DefaultBranch: "main"},
	}}
	if err := store.UpsertWorkspace(ctx, w); err != nil {
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

// TestCountMetadataCandidatesStopsCountingAlreadyTombstonedSessions pins the
// dry-run accounting to work PruneMetadata would actually perform. Tombstoning
// clears agent_session_id, so an already-tombstoned session is not a candidate
// any more; counting it would make `wx gc --dry-run` report the same non-zero
// total forever while nothing ever changes.
func TestCountMetadataCandidatesStopsCountingAlreadyTombstonedSessions(t *testing.T) {
	store := openTestStore(t)
	seedWorkspace(t, store)
	ctx := context.Background()
	session := Session{ID: "session", WorkspaceID: "workspace", SlotID: "slot", State: "ACTIVE", AgentKind: "codex", TokenHash: HashToken("token")}
	if _, err := store.CreateSlotSession(ctx, Slot{ID: "slot", WorkspaceID: "workspace", Generation: 1, Path: filepath.Join(t.TempDir(), "slot"), State: "LEASED"}, nil, session, ""); err != nil {
		t.Fatal(err)
	}
	expired := FormatTime(time.Now().Add(-time.Hour))
	if _, err := store.db.ExecContext(ctx, `UPDATE sessions SET state='EXPIRED',expires_at=?,agent_session_id='agent' WHERE id=?`, expired, session.ID); err != nil {
		t.Fatal(err)
	}
	// Keep the other three tiers empty so the total is exactly the tombstone.
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

// TestSaveSnapshotBackfillsMissingIndexRecoveryRefButStillRejectsMismatch
// covers the migration 010 upgrade path: a snapshot row committed before the
// index_recovery_ref column existed carries an empty ref, so re-running its
// snapshot job after the upgrade must backfill that one column instead of
// reporting a metadata conflict (which would quarantine the slot). A row whose
// index tree genuinely differs must still be refused.
func TestSaveSnapshotRejectsConflictingIndexTree(t *testing.T) {
	store := openTestStore(t)
	seedWorkspace(t, store)
	ctx := context.Background()
	session := Session{ID: "session", WorkspaceID: "workspace", SlotID: "slot", State: "ACTIVE", AgentKind: "codex", TokenHash: HashToken("token")}
	if _, err := store.CreateSlotSession(ctx, Slot{ID: "slot", WorkspaceID: "workspace", Generation: 1, Path: filepath.Join(t.TempDir(), "slot"), State: "LEASED"}, nil, session, ""); err != nil {
		t.Fatal(err)
	}
	original := Snapshot{ID: "snapshot", SessionID: "session", RepositoryID: "repository", HeadOID: "head", HeadRef: "refs/wx/recovery/session/repository/head", IndexTreeOID: "index", IndexRef: "refs/wx/recovery/session/repository/index", WorktreeOID: "worktree", WorktreeRef: "refs/wx/recovery/session/repository/worktree", Status: "ARCHIVED", CreatedAt: now(), ExpiresAt: FormatTime(time.Now().Add(time.Hour))}
	if err := store.SaveSnapshot(ctx, original); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveSnapshot(ctx, original); err != nil {
		t.Fatalf("re-saving the identical snapshot: %v", err)
	}
	conflicting := original
	conflicting.IndexTreeOID = "different"
	if err := store.SaveSnapshot(ctx, conflicting); err == nil {
		t.Fatal("a snapshot with a different index tree was accepted")
	}
}
