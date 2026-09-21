package archive

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// forceAddedIgnoredFixture は、`git add -f` で index に載せた ignored path を持つ worktree を作る。
// staged 内容と作業ツリー内容を別にして、両方が snapshot と restore を通るかを見分けられるようにする。
func forceAddedIgnoredFixture(t *testing.T, repository string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(repository, ".gitignore"), []byte("generated/\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, repository, "add", ".gitignore")
	gitCommand(t, repository, "commit", "-m", "ignore generated")
	mustMkdir(t, filepath.Join(repository, "generated"))
	target := filepath.Join(repository, "generated", "keep.txt")
	if err := os.WriteFile(target, []byte("staged\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, repository, "add", "-f", "generated/keep.txt")
	if err := os.WriteFile(target, []byte("working\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestSnapshotAndRestoreKeepForceAddedIgnoredWork は、ignore 規則に一致する path を `git add -f` した後の
// 未 staged 編集が release/resume を越えて残ることを確かめる。
// 一時 index を HEAD だけで初期化していた頃は、この path が `add -A` の ignore 判定で落ちて作業内容が消えていた。
func TestSnapshotAndRestoreKeepForceAddedIgnoredWork(t *testing.T) {
	repository, repo, manager, worktreeRoot := archiveFixture(t)
	forceAddedIgnoredFixture(t, repository)
	snapshot, _, err := manager.SnapshotWithPersistence(context.Background(), repo, repository, "force-added", time.Now().Add(time.Hour), nil)
	if err != nil {
		t.Fatalf("snapshot a force-added ignored path: %v", err)
	}
	if blob := gitCommand(t, repository, "show", snapshot.WorktreeOID+":generated/keep.txt"); blob != "working" {
		t.Fatalf("snapshot did not record the unstaged work: %q", blob)
	}
	if blob := gitCommand(t, repository, "show", snapshot.IndexTreeOID+":generated/keep.txt"); blob != "staged" {
		t.Fatalf("snapshot did not record the staged content: %q", blob)
	}
	target := filepath.Join(worktreeRoot, "restore", "root")
	pointAtSlot(t, manager, worktreeRoot, target)
	if err := manager.Restore(context.Background(), repo, target, "restore-slot", snapshot, nil); err != nil {
		t.Fatalf("restore a snapshot holding a force-added ignored path: %v", err)
	}
	if data, err := os.ReadFile(filepath.Join(target, "generated", "keep.txt")); err != nil || string(data) != "working\n" {
		t.Fatalf("restored working tree content=%q err=%v", data, err)
	}
	if blob := gitCommand(t, target, "show", ":generated/keep.txt"); blob != "staged" {
		t.Fatalf("restored index content=%q", blob)
	}
}

// TestSnapshotKeepsForceAddedIgnoredWorkWithSparseIndex は、sparse index を有効にした worktree でも
// HEAD に無い index entry の持ち込みが通ることを確かめる。
// 一時 index は cone 設定を引き継ぐため、entry の持ち込みが sparse の逸脱として弾かれると保存全体が止まる。
func TestSnapshotKeepsForceAddedIgnoredWorkWithSparseIndex(t *testing.T) {
	repository, repo, manager, _ := archiveFixture(t)
	mustMkdir(t, filepath.Join(repository, "inside"))
	if err := os.WriteFile(filepath.Join(repository, "inside", "kept"), []byte("kept\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repository, ".gitignore"), []byte("inside/generated/\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, repository, "add", "inside/kept", ".gitignore")
	gitCommand(t, repository, "commit", "-m", "sparse fixture")
	gitCommand(t, repository, "config", "index.sparse", "true")
	gitCommand(t, repository, "sparse-checkout", "set", "--cone", "inside")
	// ignored path は cone の内側に置く。cone の外は `git add -f` 自体が sparse の逸脱として拒まれる。
	mustMkdir(t, filepath.Join(repository, "inside", "generated"))
	ignored := filepath.Join(repository, "inside", "generated", "keep.txt")
	if err := os.WriteFile(ignored, []byte("staged\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, repository, "add", "-f", "inside/generated/keep.txt")
	if err := os.WriteFile(ignored, []byte("working\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// cone 内に新しく staged した path も持ち込みの対象になるので、同じ snapshot で一緒に確かめる。
	if err := os.WriteFile(filepath.Join(repository, "inside", "added"), []byte("added\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, repository, "add", "inside/added")
	snapshot, _, err := manager.SnapshotWithPersistence(context.Background(), repo, repository, "force-added-sparse", time.Now().Add(time.Hour), nil)
	if err != nil {
		t.Fatalf("snapshot a force-added ignored path with a sparse index: %v", err)
	}
	if blob := gitCommand(t, repository, "show", snapshot.WorktreeOID+":inside/generated/keep.txt"); blob != "working" {
		t.Fatalf("snapshot did not record the unstaged work: %q", blob)
	}
	if blob := gitCommand(t, repository, "show", snapshot.WorktreeOID+":inside/added"); blob != "added" {
		t.Fatalf("snapshot did not record the newly staged path: %q", blob)
	}
}

// TestSnapshotRecordsRemovalOfForceAddedIgnoredPath は、staged 後に作業ツリーから消した ignored path が
// 「index にはあるが作業ツリーには無い」状態のまま復元されることを確かめる。
// 一時 index へ index の entry を持ち込む以上、作業ツリーの削除も同じ経路で記録されなければならない。
func TestSnapshotRecordsRemovalOfForceAddedIgnoredPath(t *testing.T) {
	repository, repo, manager, worktreeRoot := archiveFixture(t)
	forceAddedIgnoredFixture(t, repository)
	if err := os.Remove(filepath.Join(repository, "generated", "keep.txt")); err != nil {
		t.Fatal(err)
	}
	snapshot, _, err := manager.SnapshotWithPersistence(context.Background(), repo, repository, "force-added-removed", time.Now().Add(time.Hour), nil)
	if err != nil {
		t.Fatalf("snapshot a removed force-added ignored path: %v", err)
	}
	if err := gitCommandExpectFailure(repository, "show", snapshot.WorktreeOID+":generated/keep.txt"); err == nil {
		t.Fatal("snapshot recorded a path that no longer exists in the working tree")
	}
	target := filepath.Join(worktreeRoot, "restore", "root")
	pointAtSlot(t, manager, worktreeRoot, target)
	if err := manager.Restore(context.Background(), repo, target, "restore-slot", snapshot, nil); err != nil {
		t.Fatalf("restore a snapshot holding a removed force-added ignored path: %v", err)
	}
	if _, err := os.Stat(filepath.Join(target, "generated", "keep.txt")); !os.IsNotExist(err) {
		t.Fatalf("restore materialized a path the snapshot recorded as deleted: %v", err)
	}
	if blob := gitCommand(t, target, "show", ":generated/keep.txt"); blob != "staged" {
		t.Fatalf("restored index content=%q", blob)
	}
}
