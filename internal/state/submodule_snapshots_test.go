package state

import (
	"context"
	"testing"
	"time"
)

// seedSubmoduleSnapshot は親 snapshot 1 件と、その子 snapshot 1 件分の行を作る。
func seedSubmoduleSnapshot(t *testing.T, store *Store, slotID string) Snapshot {
	t.Helper()
	ctx := context.Background()
	seedSnapshottedSlot(t, store, slotID)
	snapshot := Snapshot{
		ID: "snapshot-" + slotID, SessionID: "session-" + slotID, RepositoryID: "repository",
		HeadOID: "head", HeadRef: "refs/wx/recovery/head", IndexTreeOID: "index", WorktreeOID: "worktree",
		WorktreeRef: "refs/wx/recovery/worktree", Status: "ARCHIVED", CreatedAt: now(),
		ExpiresAt: FormatTime(time.Now().Add(time.Hour)),
	}
	if err := store.SaveSnapshot(ctx, snapshot); err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func testSubmoduleSnapshot(sessionID, path, capsule string) SubmoduleSnapshot {
	return SubmoduleSnapshot{
		SessionID: sessionID, RepositoryID: "repository", Path: path, Name: "modules/kid",
		HeadOID: "child-head", HeadRef: "refs/heads/work", IndexTreeOID: "child-index", WorktreeTreeOID: "child-worktree",
		CapsuleOID: capsule, CapsuleRef: "refs/wx/recovery/" + sessionID + "/repository/submodule/" + capsule,
	}
}

// 置き換えは 1 repository 分を丸ごと差し替え、読み出しは path 順に返す。
func TestReplaceSubmoduleSnapshotsSwapsTheWholeRepository(t *testing.T) {
	t.Parallel()
	store := openTestStore(t)
	seedWorkspace(t, store)
	ctx := context.Background()
	snapshot := seedSubmoduleSnapshot(t, store, "protected")
	first := []SubmoduleSnapshot{
		testSubmoduleSnapshot(snapshot.SessionID, "sub/two", "capsule2"),
		testSubmoduleSnapshot(snapshot.SessionID, "sub/one", "capsule1"),
	}
	if err := store.ReplaceSubmoduleSnapshots(ctx, snapshot.SessionID, "repository", first); err != nil {
		t.Fatal(err)
	}
	stored, err := store.SubmoduleSnapshots(ctx, snapshot.SessionID, "repository")
	if err != nil {
		t.Fatal(err)
	}
	if len(stored) != 2 || stored[0].Path != "sub/one" || stored[1].Path != "sub/two" {
		t.Fatalf("submodule snapshots=%+v, want both entries in path order", stored)
	}
	if stored[0].HeadRef != "refs/heads/work" || stored[0].CapsuleOID != "capsule1" || stored[0].CreatedAt == "" {
		t.Fatalf("submodule snapshot=%+v, want the recorded columns", stored[0])
	}
	if err := store.ReplaceSubmoduleSnapshots(ctx, snapshot.SessionID, "repository", first[1:]); err != nil {
		t.Fatal(err)
	}
	stored, err = store.SubmoduleSnapshots(ctx, snapshot.SessionID, "repository")
	if err != nil {
		t.Fatal(err)
	}
	if len(stored) != 1 || stored[0].Path != "sub/one" {
		t.Fatalf("submodule snapshots after the replacement=%+v, want only the remaining entry", stored)
	}
}

// 親 snapshot を消すと子の行も落ちる。ref の回収は親と同じ経路で一度に行うためである。
func TestExpiringSnapshotsRemovesSubmoduleSnapshots(t *testing.T) {
	t.Parallel()
	store := openTestStore(t)
	seedWorkspace(t, store)
	ctx := context.Background()
	snapshot := seedSubmoduleSnapshot(t, store, "protected")
	if err := store.ReplaceSubmoduleSnapshots(ctx, snapshot.SessionID, "repository",
		[]SubmoduleSnapshot{testSubmoduleSnapshot(snapshot.SessionID, "sub/one", "capsule1")}); err != nil {
		t.Fatal(err)
	}
	expectations, err := store.SubmoduleRecoveryRefExpectations(ctx, "repository")
	if err != nil {
		t.Fatal(err)
	}
	if len(expectations) != 1 || expectations[0].OID != "capsule1" || expectations[0].Name != "modules/kid" || expectations[0].ExpiresAt != snapshot.ExpiresAt {
		t.Fatalf("expectations=%+v, want the capsule of the archived snapshot", expectations)
	}
	if err := store.ExpireSessionSnapshots(ctx, snapshot.SessionID); err != nil {
		t.Fatal(err)
	}
	stored, err := store.SubmoduleSnapshots(ctx, snapshot.SessionID, "repository")
	if err != nil {
		t.Fatal(err)
	}
	if len(stored) != 0 {
		t.Fatalf("submodule snapshots after expiry=%+v, want none", stored)
	}
}
