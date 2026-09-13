package daemon

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func initGitRepo(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o700); err != nil {
		t.Fatal(err)
	}
	gitRun(t, path, "init", "-b", "main")
	gitRun(t, path, "config", "user.name", "test")
	gitRun(t, path, "config", "user.email", "test@example.com")
	if err := os.WriteFile(filepath.Join(path, "tracked.txt"), []byte("base\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitRun(t, path, "add", ".")
	gitRun(t, path, "commit", "-m", "initial")
}

// daemonSubmoduleName と daemonSubmodulePath は module directory 名と worktree 上の配置を意図的に食い違わせる。
const (
	daemonSubmoduleName = "modules/kid"
	daemonSubmodulePath = "sub/kid"
)

// initGitRepoWithSubmodule は相対 path のローカル submodule を1件持つ repository を作る。
// upstream の child は repository の兄弟に置くため、`.gitmodules` の url は `../child` になる。
func initGitRepoWithSubmodule(t *testing.T, path string) {
	t.Helper()
	child := filepath.Join(filepath.Dir(path), "child")
	initGitRepo(t, child)
	initGitRepo(t, path)
	gitRun(t, path, "-c", "protocol.file.allow=always", "submodule", "add", "--name", daemonSubmoduleName, "../child", daemonSubmodulePath)
	gitRun(t, path, "add", ".")
	gitRun(t, path, "commit", "-m", "add submodule")
}

// conflictInSubmodule は貸出中の子の index に未解消の衝突を残す。
// wx は未解消 index の子を capsule へ保存できないため、保護が必要な状態を作る最短の手順である。
func conflictInSubmodule(t *testing.T, worktree string) {
	t.Helper()
	child := filepath.Join(worktree, daemonSubmodulePath)
	base := gitOutput(t, child, "rev-parse", "HEAD")
	gitRun(t, child, "checkout", "-b", "theirs")
	writeSubmoduleFile(t, child, "theirs\n")
	gitRun(t, child, "-c", "user.name=test", "-c", "user.email=test@example.com", "commit", "-am", "theirs")
	gitRun(t, child, "checkout", "-b", "ours", base)
	writeSubmoduleFile(t, child, "ours\n")
	gitRun(t, child, "-c", "user.name=test", "-c", "user.email=test@example.com", "commit", "-am", "ours")
	command := exec.Command("git", "-c", "user.name=test", "-c", "user.email=test@example.com", "merge", "theirs")
	command.Dir = child
	if out, err := command.CombinedOutput(); err == nil {
		t.Fatalf("the merge in the submodule did not conflict:\n%s", out)
	}
}

func writeSubmoduleFile(t *testing.T, child, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(child, "tracked.txt"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func gitRun(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func gitOutput(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}
