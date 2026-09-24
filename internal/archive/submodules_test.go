package archive

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/HappyOnigiri/WorktreeX/internal/config"
	"github.com/HappyOnigiri/WorktreeX/internal/discovery"
	"github.com/HappyOnigiri/WorktreeX/internal/domain"
	"github.com/HappyOnigiri/WorktreeX/internal/gitx"
	"github.com/HappyOnigiri/WorktreeX/internal/workspace"
)

// submoduleName と submodulePath は module directory 名と worktree 上の配置を意図的に食い違わせ、
// 検出が name と path を取り違えないことを確かめられるようにする。
const (
	submoduleName = "modules/kid"
	submodulePath = "sub/kid"
)

// submoduleFixture は、ソース repository の linked worktree を slot に見立てた検出用の一式を用意する。
// 子の gitdir が linked worktree 側（`$GIT_DIR/modules/<name>`）に分かれる本番と同じ配置になるため、
// 子で作った commit がソースのローカル module に無い状態を実機どおり再現できる。
func submoduleFixture(t *testing.T) (string, discovery.Repository, *Manager, string) {
	t.Helper()
	root := t.TempDir()
	worktreeRoot := filepath.Join(root, "worktrees")
	mustMkdir(t, worktreeRoot)
	child := filepath.Join(root, "child")
	mustMkdir(t, child)
	initRepository(t, child, "tracked.txt")
	source := filepath.Join(root, "source")
	mustMkdir(t, source)
	initRepository(t, source, "README")
	gitCommand(t, source, "-c", "protocol.file.allow=always", "submodule", "add", "--name", submoduleName, "../child", submodulePath)
	gitCommand(t, source, "commit", "-m", "add submodule")
	head := gitCommand(t, source, "rev-parse", "HEAD")
	worktree := filepath.Join(worktreeRoot, "slot")
	gitCommand(t, source, "worktree", "add", "--detach", worktree, head)
	localModule := filepath.Join(source, ".git", "modules", filepath.FromSlash(submoduleName))
	gitCommand(t, worktree, "-c", "protocol.file.allow=always", "-c", "submodule."+submoduleName+".url="+localModule,
		"submodule", "update", "--init", "--", submodulePath)
	common := gitCommand(t, source, "rev-parse", "--path-format=absolute", "--git-common-dir")
	repo := discovery.Repository{ID: "repository", MainPath: domain.CanonicalPath(source), CommonDir: domain.CanonicalPath(common)}
	cfg := config.Defaults()
	cfg.Storage.WorktreeRoot = worktreeRoot
	runner := &gitx.Runner{Timeout: 30 * time.Second}
	owner, _, err := domain.OpenOwnedRoot(worktreeRoot, worktreeRoot)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = owner.Close() })
	preparer := &workspace.Preparer{Git: runner, Config: cfg, Ownership: allowOwnershipValidator{}, OwnedRoot: owner, RootPath: worktreeRoot}
	return worktree, repo, &Manager{Git: runner, Preparer: preparer, Ownership: allowOwnershipValidator{}}, worktreeRoot
}

func initRepository(t *testing.T, path, file string) {
	t.Helper()
	gitCommand(t, path, "init", "-b", "main")
	gitCommand(t, path, "config", "user.name", "test")
	gitCommand(t, path, "config", "user.email", "test@example.com")
	if err := os.WriteFile(filepath.Join(path, file), []byte("base\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, path, "add", ".")
	gitCommand(t, path, "commit", "-m", "initial")
}

// clean な子は prepare の実体化だけで元に戻るため、capsule を作らず記録も残さない。
// 保存できる作業を持つ子の往復は TestSnapshotAndRestorePreservesSubmoduleWork が、
// 保存できない条件は TestSnapshotRecordsSubmoduleWorkItCannotSave が固定する。
func TestSnapshotReportsNoUnsavedWorkForACleanSubmodule(t *testing.T) {
	worktree, repo, manager, _ := submoduleFixture(t)
	_, unsaved, err := manager.SnapshotWithPersistence(context.Background(), repo, worktree, "session", time.Now().Add(time.Hour), nil)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if len(unsaved) != 0 {
		t.Fatalf("unsaved submodules=%+v, want none", unsaved)
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func commitInSubmodule(t *testing.T, worktree string) {
	t.Helper()
	submodule := filepath.Join(worktree, submodulePath)
	writeFile(t, filepath.Join(submodule, "tracked.txt"), "child work\n")
	gitCommand(t, submodule, "-c", "user.name=test", "-c", "user.email=test@example.com", "commit", "-am", "child work")
}

// porcelain v2 の 3 列目だけを見る。非 submodule の `N...` は無視し、rename 行の path は tab の手前を使う。
func TestStatusSubmoduleReasonsReadsOnlyTheSubmoduleField(t *testing.T) {
	output := strings.Join([]string{
		"1 .M N... 100644 100644 100644 aaa bbb plain.txt",
		"1 .M S.M. 160000 160000 160000 aaa bbb sub/edited",
		"1 .M S..U 160000 160000 160000 aaa bbb sub/untracked",
		"2 R. SC.. 160000 160000 160000 aaa bbb R100 sub/moved\tsub/old",
		"u UU S..U 160000 160000 160000 160000 aaa bbb ccc sub/conflicted",
		"? scratch.txt",
		"",
	}, "\n")
	got := statusSubmoduleReasons(output)
	if _, ok := got["plain.txt"]; ok {
		t.Fatalf("a non-submodule entry was reported: %+v", got)
	}
	for path, want := range map[string]string{
		"sub/edited":     ReasonModified,
		"sub/untracked":  ReasonUntracked,
		"sub/moved":      ReasonCommitMoved,
		"sub/conflicted": ReasonUntracked,
	} {
		if len(got[path]) != 1 || got[path][0] != want {
			t.Fatalf("reasons for %s=%v, want [%s]", path, got[path], want)
		}
	}
}

// 列挙が失敗しても snapshot は成功させ、判定不能として保護側へ倒す。
// 親の保存は既に済んでおり、ここで失敗させると resume の手段まで失うためである。
func TestSnapshotKeepsGoingWhenTheSubmoduleProbeFails(t *testing.T) {
	worktree, repo, manager, _ := submoduleFixture(t)
	installGitFault(t, " config --blob HEAD:.gitmodules", 1)
	_, unsaved, err := manager.SnapshotWithPersistence(context.Background(), repo, worktree, "probe-failure", time.Now().Add(time.Hour), nil)
	if err != nil {
		t.Fatalf("snapshot failed on a submodule probe failure: %v", err)
	}
	if len(unsaved) != 1 || unsaved[0].Path != "" || unsaved[0].Reasons[0] != ReasonUndetermined {
		t.Fatalf("unsaved submodules=%+v, want a single undetermined record", unsaved)
	}
}
