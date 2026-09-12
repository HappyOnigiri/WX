package state

import (
	"context"
	"path/filepath"
	"testing"
)

func TestRecordSlotRepositoryIdentityRequiresAnExistingRow(t *testing.T) {
	t.Parallel()
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

func TestRestoringRepositoryMetadataAndSnapshotExpiryBoundaries(t *testing.T) {
	t.Parallel()
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
