package archive

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/HappyOnigiri/WX/internal/gitx"
)

// 停止中 rebase は working tree が clean なので snapshot の clean 短絡を通る。
// 制御ファイルは tree にも index にも現れないため、短絡側でも別経路で採取できていることを固定する。
func TestSnapshotRecordsStoppedRebaseState(t *testing.T) {
	repository, repo, manager, _ := archiveFixture(t)
	commitFile(t, repository, "second", "2\n")
	commitFile(t, repository, "third", "3\n")
	stopRebaseAtEdit(t, repository)
	if status := gitCommand(t, repository, "status", "--porcelain=v1"); status != "" {
		t.Fatalf("edit stop must leave a clean worktree, got:\n%s", status)
	}
	expires := time.Now().Add(time.Hour)
	snapshot, err := manager.SnapshotWithPersistence(context.Background(), repo, repository, "rebase", expires, nil)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.GitStateOID == "" || snapshot.GitStateRef != "refs/wx/recovery/rebase/repository/gitstate" {
		t.Fatalf("snapshot did not record rebase state: %+v", snapshot)
	}
	if got := gitCommand(t, repository, "rev-parse", "--verify", snapshot.GitStateRef); got != snapshot.GitStateOID {
		t.Fatalf("rebase state ref=%s want %s", got, snapshot.GitStateOID)
	}
	paths := strings.Split(gitCommand(t, repository, "ls-tree", "-r", "--name-only", snapshot.GitStateOID), "\n")
	for _, want := range []string{"ORIG_HEAD", "REBASE_HEAD", "rebase-merge/git-rebase-todo", "rebase-merge/interactive", "rebase-merge/onto", "rebase-merge/orig-head", "rebase-merge/stopped-sha"} {
		if !slices.Contains(paths, want) {
			t.Fatalf("rebase state tree is missing %s: %v", want, paths)
		}
	}
	// 復元後の HEAD から到達できない commit は、保護 commit の親に並んでいる場合だけ GC を越えて残る。
	parents := strings.Fields(gitCommand(t, repository, "rev-list", "--parents", "-n", "1", snapshot.GitStateOID))[1:]
	for _, name := range []string{"rebase-merge/onto", "rebase-merge/orig-head", "rebase-merge/stopped-sha"} {
		want := readGitDirFile(t, repository, name)
		if !slices.Contains(parents, want) {
			t.Fatalf("%s (%s) is not protected by a parent: %v", name, want, parents)
		}
	}
	if !slices.Contains(parents, snapshot.HeadOID) {
		t.Fatalf("stopped HEAD is not among the parents: %v", parents)
	}
	if !slices.IsSorted(parents) {
		t.Fatalf("parents must be lexicographically ordered to keep the commit OID stable: %v", parents)
	}
	// SNAPSHOT job の再実行は同じ OID を出す必要がある。出ないと SaveSnapshot の ON CONFLICT が不一致で失敗する。
	replayed, err := manager.SnapshotWithPersistence(context.Background(), repo, repository, "rebase", expires, nil)
	if err != nil || replayed.GitStateOID != snapshot.GitStateOID {
		t.Fatalf("replayed rebase state=%q want %q err=%v", replayed.GitStateOID, snapshot.GitStateOID, err)
	}
}

// 進行中 rebase が無い session は、従来の snapshot と同じ 3 本の ref だけを公開する。
func TestSnapshotWithoutRebaseRecordsNoGitState(t *testing.T) {
	repository, repo, manager, _ := archiveFixture(t)
	snapshot, err := manager.SnapshotWithPersistence(context.Background(), repo, repository, "plain", time.Now().Add(time.Hour), nil)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.GitStateOID != "" || snapshot.GitStateRef != "" {
		t.Fatalf("snapshot recorded rebase state without a rebase: %+v", snapshot)
	}
	if targets := recoveryRefTargets(snapshot); len(targets) != 3 {
		t.Fatalf("recovery ref targets=%v", targets)
	}
	refs := gitCommand(t, repository, "for-each-ref", "--format=%(refname)", "refs/wx/recovery/")
	if strings.Contains(refs, "gitstate") {
		t.Fatalf("published an unexpected rebase state ref:\n%s", refs)
	}
}

// 復元は書き戻しの前に必ず削除する。再利用された slot が前の貸出の進行情報を引き継ぐと、
// snapshot に無い rebase が復元先で続行可能に見えてしまう。
func TestRestoreGitStateClearsStaleRebaseState(t *testing.T) {
	repository, _, _, _ := archiveFixture(t)
	commitFile(t, repository, "second", "2\n")
	stopRebaseAtEdit(t, repository)
	value, run := directGitAccess(t, repository)
	if err := restoreGitState(value, run, ""); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"rebase-merge", "ORIG_HEAD", "REBASE_HEAD"} {
		if _, err := os.Lstat(filepath.Join(repository, ".git", name)); !os.IsNotExist(err) {
			t.Fatalf("stale %s survived the restore: %v", name, err)
		}
	}
}

// apply backend の停止は未解消 index を伴うため本変更の対象外だが、path 集合の取りこぼしは静かな欠損になる。
// 制御ファイルを直接置いて、rebase-apply/ 配下も採取・復元されることだけを固定する。
func TestGitStateRoundTripCoversApplyBackendPaths(t *testing.T) {
	repository, _, _, _ := archiveFixture(t)
	head := gitCommand(t, repository, "rev-parse", "HEAD")
	applyDir := filepath.Join(repository, ".git", "rebase-apply")
	mustMkdir(t, applyDir)
	for name, content := range map[string]string{"next": "1\n", "last": "1\n", "orig-head": head + "\n", "onto": head + "\n", "applying": ""} {
		if err := os.WriteFile(filepath.Join(applyDir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	value, run := directGitAccess(t, repository)
	root, err := openGitStateRoot(value)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	entries, err := collectGitState(root)
	if err != nil {
		t.Fatal(err)
	}
	tree, err := writeGitStateTree(run, entries)
	if err != nil || tree == "" {
		t.Fatalf("tree=%q err=%v", tree, err)
	}
	if err := os.RemoveAll(applyDir); err != nil {
		t.Fatal(err)
	}
	if err := restoreGitState(value, run, tree); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(filepath.Join(applyDir, "orig-head")); err != nil || string(data) != head+"\n" {
		t.Fatalf("rebase-apply/orig-head=%q err=%v", data, err)
	}
	// 空ファイルも状態を表すため、内容の長さに関わらず復元されなければならない。
	if info, err := os.Lstat(filepath.Join(applyDir, "applying")); err != nil || info.Size() != 0 {
		t.Fatalf("rebase-apply/applying=%v err=%v", info, err)
	}
}

// directGitAccess は pin 済み worktree を経由しない Git 実行を返し、gitstate 単体の往復だけを検査できるようにする。
func directGitAccess(t *testing.T, dir string) (func([]string, ...string) (string, error), gitRunner) {
	t.Helper()
	runner := &gitx.Runner{Timeout: 10 * time.Second}
	run := func(env []string, input []byte, args ...string) (gitx.Result, error) {
		return runner.RunEnvInput(context.Background(), dir, env, input, args...)
	}
	value := func(env []string, args ...string) (string, error) {
		result, err := run(env, nil, args...)
		if err != nil {
			return "", err
		}
		return strings.TrimSpace(result.Stdout), nil
	}
	return value, run
}

func commitFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, dir, "add", name)
	gitCommand(t, dir, "commit", "-m", name)
}

// stopRebaseAtEdit は先頭の pick を edit に書き換えた `rebase -i` を走らせ、HEAD~1 の位置で停止させる。
func stopRebaseAtEdit(t *testing.T, dir string) {
	t.Helper()
	editor := filepath.Join(t.TempDir(), "sequence-editor")
	script := "#!/bin/sh\nawk 'NR==1{sub(/^pick/,\"edit\")}1' \"$1\" > \"$1.wx\" && mv \"$1.wx\" \"$1\"\n"
	if err := os.WriteFile(editor, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	command := exec.Command("git", "rebase", "-i", "HEAD~1")
	command.Dir = dir
	command.Env = append(os.Environ(), "GIT_SEQUENCE_EDITOR="+editor)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("start interactive rebase: %v\n%s", err, output)
	}
	if _, err := os.Lstat(filepath.Join(dir, ".git", "rebase-merge")); err != nil {
		t.Fatalf("interactive rebase did not stop: %v", err)
	}
}

func readGitDirFile(t *testing.T, dir, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, ".git", name))
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(data))
}
