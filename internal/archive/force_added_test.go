package archive

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/HappyOnigiri/WX/internal/gitx"
)

// directoryGit は指定 directory で動く value/run を作る。snapshot 経路と同じ形で一時 index を渡せる。
func directoryGit(dir string) (gitValueFunc, gitRunFunc) {
	runner := &gitx.Runner{Timeout: 30 * time.Second}
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

// worktreeTreeForTest は addWorktreeContents だけを通した worktree tree を作る。
func worktreeTreeForTest(t *testing.T, repo string) (gitValueFunc, string) {
	t.Helper()
	value, run := directoryGit(repo)
	head, err := value(nil, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	tmp, cleanup, err := temporaryIndex("force added", ".wx-force-added-index-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cleanup)
	env := []string{"GIT_INDEX_FILE=" + tmp}
	if _, err := run(env, nil, "read-tree", head); err != nil {
		t.Fatal(err)
	}
	if err := addWorktreeContents(value, run, env); err != nil {
		t.Fatalf("add worktree contents: %v", err)
	}
	tree, err := value(env, "write-tree")
	if err != nil {
		t.Fatal(err)
	}
	return value, tree
}

// index に登録された ignored file は worktree の最新内容で tree に載り、
// 登録されていない ignored file と worktree から消えた path は載らない。
func TestAddWorktreeContentsKeepsForceAddedIgnoredFiles(t *testing.T) {
	repo := t.TempDir()
	initRepository(t, repo, "tracked.txt")
	writeFile(t, filepath.Join(repo, ".gitignore"), "generated/\n")
	gitCommand(t, repo, "add", ".gitignore")
	gitCommand(t, repo, "commit", "-m", "ignore generated")
	mustMkdir(t, filepath.Join(repo, "generated"))
	writeFile(t, filepath.Join(repo, "generated", "keep.txt"), "staged\n")
	gitCommand(t, repo, "add", "-f", "generated/keep.txt")
	writeFile(t, filepath.Join(repo, "generated", "keep.txt"), "working\n")
	writeFile(t, filepath.Join(repo, "generated", "scratch.txt"), "throwaway\n")
	if err := os.Remove(filepath.Join(repo, "tracked.txt")); err != nil {
		t.Fatal(err)
	}

	value, tree := worktreeTreeForTest(t, repo)
	listing, err := value(nil, "ls-tree", "-r", "--name-only", tree)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Split(listing, "\n")
	want := []string{".gitignore", "generated/keep.txt"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("worktree tree paths=%v, want %v", got, want)
	}
	content, err := value(nil, "cat-file", "-p", tree+":generated/keep.txt")
	if err != nil {
		t.Fatal(err)
	}
	if content != "working" {
		t.Fatalf("force-added ignored file content=%q, want the unstaged working copy", content)
	}
}

// 取りこぼしが無い worktree では追加の add を走らせず、tree は従来と同じになる。
func TestAddWorktreeContentsLeavesAnOrdinaryWorktreeUnchanged(t *testing.T) {
	repo := t.TempDir()
	initRepository(t, repo, "tracked.txt")
	writeFile(t, filepath.Join(repo, "tracked.txt"), "edited\n")
	writeFile(t, filepath.Join(repo, "scratch.txt"), "note\n")

	value, tree := worktreeTreeForTest(t, repo)
	listing, err := value(nil, "ls-tree", "-r", "--name-only", tree)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := strings.Split(listing, "\n"), []string{"scratch.txt", "tracked.txt"}; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("worktree tree paths=%v, want %v", got, want)
	}
}

// nulPathSet は末尾の区切りが生む空要素を落とし、改行を含む path をそのまま保つ。
func TestNULPathSetIgnoresTheTrailingSeparator(t *testing.T) {
	got := nulPathSet("a.txt\x00dir/with\nnewline.txt\x00")
	if len(got) != 2 {
		t.Fatalf("paths=%v, want two entries", got)
	}
	if _, ok := got["dir/with\nnewline.txt"]; !ok {
		t.Fatalf("paths=%v, want the newline path kept verbatim", got)
	}
}
