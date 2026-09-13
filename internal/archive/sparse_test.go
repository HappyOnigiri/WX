package archive

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestSnapshotAndRestoreCarryWorkOutsideTheSparseCone は、sparse 範囲外に作った file が snapshot に入り、
// sparse 条件を保ったまま復元されることを確かめる。
// 以前は範囲外の実体があるだけで保存の add が失敗し、作業ごと archive へ進めなくなっていた。
func TestSnapshotAndRestoreCarryWorkOutsideTheSparseCone(t *testing.T) {
	repository, repo, manager, worktreeRoot := archiveFixture(t)
	for path, content := range map[string]string{"inside/kept": "kept\n", "outside/dropped": "dropped\n"} {
		path = filepath.Join(repository, path)
		mustMkdir(t, filepath.Dir(path))
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	gitCommand(t, repository, "add", ".")
	gitCommand(t, repository, "commit", "-m", "sparse fixtures")
	gitCommand(t, repository, "sparse-checkout", "set", "inside")
	if _, err := os.Stat(filepath.Join(repository, "outside", "dropped")); !os.IsNotExist(err) {
		t.Fatalf("sparse-checkout left the out-of-cone file: %v", err)
	}
	// 範囲外に作った作業。範囲内の編集と取り違えないよう、範囲内は触らない。
	mustMkdir(t, filepath.Join(repository, "outside"))
	if err := os.WriteFile(filepath.Join(repository, "outside", "new"), []byte("out of cone\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	snapshot, _, err := manager.SnapshotWithPersistence(context.Background(), repo, repository, "sparse", time.Now().Add(time.Hour), nil)
	if err != nil {
		t.Fatalf("snapshot with work outside the sparse cone: %v", err)
	}
	if blob := gitCommand(t, repository, "show", snapshot.WorktreeOID+":outside/new"); blob != "out of cone" {
		t.Fatalf("snapshot did not record the out-of-cone work: %q", blob)
	}
	// 範囲外の tracked path は実体を持てないので、HEAD の内容のまま残らなければならない。
	if blob := gitCommand(t, repository, "show", snapshot.WorktreeOID+":outside/dropped"); blob != "dropped" {
		t.Fatalf("snapshot rewrote an out-of-cone tracked path: %q", blob)
	}
	target := filepath.Join(worktreeRoot, "restore", "root")
	pointAtSlot(t, manager, worktreeRoot, target)
	if err := manager.Restore(context.Background(), repo, target, "restore-slot", snapshot); err != nil {
		t.Fatalf("restore a snapshot holding work outside the sparse cone: %v", err)
	}
	if data, err := os.ReadFile(filepath.Join(target, "outside", "new")); err != nil || string(data) != "out of cone\n" {
		t.Fatalf("restored out-of-cone work=%q err=%v", data, err)
	}
	if enabled := gitCommand(t, target, "config", "--default", "false", "--type=bool", "--get", "core.sparseCheckout"); enabled != "true" {
		t.Fatalf("restored worktree lost its sparse configuration: %q", enabled)
	}
	if _, err := os.Stat(filepath.Join(target, "outside", "dropped")); !os.IsNotExist(err) {
		t.Fatalf("restore materialized an out-of-cone path that held no work: %v", err)
	}
	if listing := gitCommand(t, target, "ls-files", "-v", "outside/dropped"); listing != "S outside/dropped" {
		t.Fatalf("restore cleared skip-worktree for a path that held no work: %q", listing)
	}
	// 復元した範囲外の作業を commit して続きを書き、もう一巡できることを確かめる。
	// 一度実体化した path は、HEAD に載って復元先で skip-worktree が立ち直しても worktree の内容が正になる。
	gitCommand(t, target, "add", "--sparse", "outside/new")
	gitCommand(t, target, "commit", "-m", "out of cone")
	if err := os.WriteFile(filepath.Join(target, "outside", "new"), []byte("still out of cone\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	second, _, err := manager.SnapshotWithPersistence(context.Background(), repo, target, "sparse-again", time.Now().Add(time.Hour), nil)
	if err != nil {
		t.Fatalf("snapshot the restored out-of-cone work: %v", err)
	}
	if blob := gitCommand(t, target, "show", second.WorktreeOID+":outside/new"); blob != "still out of cone" {
		t.Fatalf("second snapshot did not record the out-of-cone edit: %q", blob)
	}
	again := filepath.Join(worktreeRoot, "restore-again", "root")
	pointAtSlot(t, manager, worktreeRoot, again)
	if err := manager.Restore(context.Background(), repo, again, "restore-again-slot", second); err != nil {
		t.Fatalf("restore the second snapshot: %v", err)
	}
	if data, err := os.ReadFile(filepath.Join(again, "outside", "new")); err != nil || string(data) != "still out of cone\n" {
		t.Fatalf("second restore of out-of-cone work=%q err=%v", data, err)
	}
	// 実体を戻した path に skip-worktree が残ると、以後 git は worktree の内容を見なくなり編集が保存されない。
	if listing := gitCommand(t, again, "ls-files", "-v", "outside/new"); listing != "H outside/new" {
		t.Fatalf("restore left skip-worktree on a materialized path: %q", listing)
	}
}
