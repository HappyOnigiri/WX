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

func TestNullStringAndTokenBoundaries(t *testing.T) {
	if nullString("") != nil || nullString("value") != "value" {
		t.Fatal("nullString boundary failed")
	}
	first, err := TokenHex()
	if err != nil || len(first) != 64 {
		t.Fatalf("token=%q err=%v", first, err)
	}
	second, err := TokenHex()
	if err != nil || len(second) != 64 || first == second {
		t.Fatalf("second token=%q err=%v", second, err)
	}
}
