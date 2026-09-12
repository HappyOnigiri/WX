package workspace

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/HappyOnigiri/WX/internal/config"
	"github.com/HappyOnigiri/WX/internal/discovery"
	"github.com/HappyOnigiri/WX/internal/domain"
	"github.com/HappyOnigiri/WX/internal/gitx"
)

// submoduleName と submodulePath は name と worktree 上の配置が一致せず、name に `/` を含む形を常に通す。
// module directory は name 由来、worktree 操作は path 由来なので、混同した実装をここで落とす。
const (
	submoduleName = "modules/kid"
	submodulePath = "sub/kid"
)

type submoduleFixture struct {
	repository string
	child      string
	repo       discovery.Repository
	preparer   *Preparer
	head       string
	target     string
	logged     *bytes.Buffer
	runner     *gitx.Runner
}

func newSubmoduleFixture(t *testing.T) *submoduleFixture {
	t.Helper()
	root := t.TempDir()
	child := filepath.Join(root, "child")
	if err := os.Mkdir(child, 0o700); err != nil {
		t.Fatal(err)
	}
	initTestRepository(t, child)
	if err := os.WriteFile(filepath.Join(child, "kid.txt"), []byte("kid\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, child, "add", ".")
	gitCommand(t, child, "commit", "-m", "child initial")

	repository := filepath.Join(root, "repository")
	if err := os.Mkdir(repository, 0o700); err != nil {
		t.Fatal(err)
	}
	initTestRepository(t, repository)
	if err := os.WriteFile(filepath.Join(repository, "tracked"), []byte("base\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// 相対 path の submodule として足す。`file://` は使わず、ローカル path 直指定の clone だけを通す。
	gitCommand(t, repository, "-c", "protocol.file.allow=always", "submodule", "add", "--name", submoduleName, "../child", submodulePath)
	gitCommand(t, repository, "add", ".")
	gitCommand(t, repository, "commit", "-m", "add submodule")
	return newSubmodulePreparer(t, root, repository, child)
}

func newSubmodulePreparer(t *testing.T, root, repository, child string) *submoduleFixture {
	t.Helper()
	head := gitOutput(t, repository, "rev-parse", "HEAD")
	common := gitOutput(t, repository, "rev-parse", "--path-format=absolute", "--git-common-dir")
	repo := discovery.Repository{ID: "repository", MainPath: domain.CanonicalPath(repository), CommonDir: domain.CanonicalPath(common)}
	worktreeRoot := filepath.Join(root, "worktrees")
	if err := os.Mkdir(worktreeRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := config.Defaults()
	cfg.Storage.WorktreeRoot = worktreeRoot
	owner, _, err := domain.OpenOwnedRoot(worktreeRoot, worktreeRoot)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = owner.Close() })
	logged := &bytes.Buffer{}
	runner := &gitx.Runner{Timeout: 30 * time.Second}
	slotPath := filepath.Join(worktreeRoot, testSlotRelPath)
	preparer := &Preparer{
		Git: runner, Config: cfg, Ownership: allowOwnershipValidator{},
		SlotPath: slotPath, OwnedRoot: owner, RootPath: worktreeRoot,
		RootID: testRootID, SlotRelPath: testSlotRelPath,
		Log: slog.New(slog.NewTextHandler(logged, &slog.HandlerOptions{Level: slog.LevelWarn})),
	}
	return &submoduleFixture{
		repository: repository, child: child, repo: repo, preparer: preparer,
		head: head, target: filepath.Join(slotPath, testRepositoryID), logged: logged, runner: runner,
	}
}

func initTestRepository(t *testing.T, directory string) {
	t.Helper()
	gitCommand(t, directory, "init", "-b", "main")
	gitCommand(t, directory, "config", "user.name", "test")
	gitCommand(t, directory, "config", "user.email", "test@example.com")
}

func (f *submoduleFixture) moduleDir() string {
	return filepath.Join(string(f.repo.CommonDir), "modules", submoduleName)
}

func (f *submoduleFixture) submoduleTarget() string {
	return filepath.Join(f.target, submodulePath)
}

// 実体化した submodule は内容・HEAD・origin が揃い、親は tracked-clean のまま残る。
func TestPrepareMaterializesSubmodule(t *testing.T) {
	t.Parallel()
	f := newSubmoduleFixture(t)
	gitlink := submoduleGitlink(t, f.repository, f.head)
	if err := f.preparer.Prepare(context.Background(), f.repo, f.target, f.head, "slot"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(f.submoduleTarget(), "kid.txt")); err != nil {
		t.Fatalf("submodule content: %v", err)
	}
	if head := gitOutput(t, f.submoduleTarget(), "rev-parse", "HEAD"); head != gitlink {
		t.Fatalf("submodule HEAD=%s, want gitlink %s", head, gitlink)
	}
	// origin をローカル module のままにすると `git push` が main の `.git/modules` に入る。
	origin := gitOutput(t, f.submoduleTarget(), "remote", "get-url", "origin")
	if origin != f.child {
		t.Fatalf("submodule origin=%s, want the upstream %s", origin, f.child)
	}
	if status := gitOutput(t, f.target, "status", "--porcelain", "--ignore-submodules=none"); status != "" {
		t.Fatalf("prepared worktree status=%q, want clean", status)
	}
	// objects はローカル clone の hardlink で共有され、共有 .git 側のディスクを増やさない。
	if info, err := os.Stat(filepath.Join(f.target, ".git")); err != nil {
		t.Fatalf("worktree gitdir link: %v %v", info, err)
	}
}

// restore 経路でも実体化する。単発準備と staged 準備で結線が別なため、両方を通す。
func TestPrepareForRestoreMaterializesSubmodule(t *testing.T) {
	t.Parallel()
	f := newSubmoduleFixture(t)
	gitlink := submoduleGitlink(t, f.repository, f.head)
	if err := f.preparer.PrepareForRestore(context.Background(), f.repo, f.target, f.head, "slot"); err != nil {
		t.Fatal(err)
	}
	if head := gitOutput(t, f.submoduleTarget(), "rev-parse", "HEAD"); head != gitlink {
		t.Fatalf("restored submodule HEAD=%s, want gitlink %s", head, gitlink)
	}
}

// ローカル module が無い場合は書き込む前に省略し、準備は成功して空ディレクトリのまま残る。
func TestPrepareSkipsSubmoduleWithoutLocalModule(t *testing.T) {
	t.Parallel()
	f := newSubmoduleFixture(t)
	if err := os.RemoveAll(f.moduleDir()); err != nil {
		t.Fatal(err)
	}
	if err := f.preparer.Prepare(context.Background(), f.repo, f.target, f.head, "slot"); err != nil {
		t.Fatal(err)
	}
	assertEmptyGitlinkDirectory(t, f.submoduleTarget())
	if !strings.Contains(f.logged.String(), "submodule has no local module") {
		t.Fatalf("logged=%q, want a skip warning for the missing local module", f.logged.String())
	}
}

// gitlink OID がローカル module に無い場合も書き込む前に省略する。
// 実体化を始めてしまうと親が ` M <path>` の dirty で残り、その gitdir は wx が消せない場所にできる。
func TestPrepareSkipsSubmoduleWithMissingCommit(t *testing.T) {
	t.Parallel()
	f := newSubmoduleFixture(t)
	// child だけを進め、その commit を親の gitlink に据える。module clone は取得していないので解決できない。
	if err := os.WriteFile(filepath.Join(f.child, "kid.txt"), []byte("ahead\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, f.child, "add", ".")
	gitCommand(t, f.child, "commit", "-m", "child ahead")
	ahead := gitOutput(t, f.child, "rev-parse", "HEAD")
	gitCommand(t, f.repository, "update-index", "--cacheinfo", "160000,"+ahead+","+submodulePath)
	gitCommand(t, f.repository, "commit", "-m", "advance gitlink")
	head := gitOutput(t, f.repository, "rev-parse", "HEAD")
	if err := f.preparer.Prepare(context.Background(), f.repo, f.target, head, "slot"); err != nil {
		t.Fatal(err)
	}
	assertEmptyGitlinkDirectory(t, f.submoduleTarget())
	if status := gitOutput(t, f.target, "status", "--porcelain", "--ignore-submodules=none"); status != "" {
		t.Fatalf("prepared worktree status=%q, want clean instead of a dirty gitlink", status)
	}
	if !strings.Contains(f.logged.String(), "submodule commit is missing") {
		t.Fatalf("logged=%q, want a skip warning for the missing commit", f.logged.String())
	}
}

// 準備はソースリポジトリの共有 config と main worktree を変更しない。
// `-c submodule.<name>.url=` を与えた `submodule update --init` が共有 config へ何も書かないことに依存しているため、
// 上流の挙動が変わったらここで落とす。
func TestPrepareLeavesSourceRepositoryUnchanged(t *testing.T) {
	t.Parallel()
	f := newSubmoduleFixture(t)
	configPath := filepath.Join(string(f.repo.CommonDir), "config")
	before, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	statusBefore := gitOutput(t, f.repository, "status", "--porcelain", "--ignore-submodules=none")
	if err := f.preparer.Prepare(context.Background(), f.repo, f.target, f.head, "slot"); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("source repository config changed:\nbefore:\n%s\nafter:\n%s", before, after)
	}
	if status := gitOutput(t, f.repository, "status", "--porcelain", "--ignore-submodules=none"); status != statusBefore {
		t.Fatalf("source worktree status=%q, want the unchanged %q", status, statusBefore)
	}
}

// 方針を無効にした workspace では submodule 関連の Git を1回も起動しない。
func TestPrepareSkipsSubmodulePhaseWhenDisabled(t *testing.T) {
	t.Parallel()
	f := newSubmoduleFixture(t)
	f.preparer.Config.Worktree.Submodules = false
	var invoked []string
	f.runner.SetBeforeRunAtHook(func(args []string) { invoked = append(invoked, strings.Join(args, " ")) })
	if err := f.preparer.Prepare(context.Background(), f.repo, f.target, f.head, "slot"); err != nil {
		t.Fatal(err)
	}
	for _, command := range invoked {
		if strings.Contains(command, "submodule") || strings.Contains(command, ".gitmodules") {
			t.Fatalf("git command %q ran while submodules were disabled", command)
		}
	}
	assertEmptyGitlinkDirectory(t, f.submoduleTarget())
}

// workspace 個別の上書きが global 方針より優先される。
func TestSubmodulesForWorkspaceOverride(t *testing.T) {
	t.Parallel()
	f := newSubmoduleFixture(t)
	root, err := repositoryWorkspaceRoot(f.repo)
	if err != nil {
		t.Fatal(err)
	}
	disabled := false
	f.preparer.Config.Workspaces = map[string]config.Workspace{root: {Submodules: &disabled}}
	enabled, err := f.preparer.submodulesEnabled(f.repo)
	if err != nil {
		t.Fatal(err)
	}
	if enabled {
		t.Fatal("workspace override did not disable submodule materialization")
	}
	f.preparer.Config.Worktree.Submodules = false
	f.preparer.Config.Workspaces = map[string]config.Workspace{}
	if enabled, err := f.preparer.submodulesEnabled(f.repo); err != nil || enabled {
		t.Fatalf("global policy enabled=%t err=%v, want false", enabled, err)
	}
}

// .gitmodules から取り出す name と path は、module directory と worktree の外を指す値を拒否する。
func TestParseSubmoduleConfigRejectsUnsafeNamesAndPaths(t *testing.T) {
	t.Parallel()
	for _, entries := range []string{
		"submodule.../escape.path sub\nsubmodule.../escape.url ../child\n",
		"submodule./absolute.path sub\nsubmodule./absolute.url ../child\n",
		"submodule.kid/.git.path sub\nsubmodule.kid/.git.url ../child\n",
		"submodule..GIT/kid.path sub\nsubmodule..GIT/kid.url ../child\n",
		"submodule.kid.path ../escape\nsubmodule.kid.url ../child\n",
		"submodule.kid.path /etc\nsubmodule.kid.url ../child\n",
	} {
		if modules, err := parseSubmoduleConfig(entries); err == nil {
			t.Fatalf("parseSubmoduleConfig(%q)=%+v, want rejection", entries, modules)
		}
	}
}

// nested name は末尾の属性名だけを剥がして取り出し、`/` や `.` を含む name を壊さない。
func TestParseSubmoduleConfigKeepsNestedNames(t *testing.T) {
	t.Parallel()
	modules, err := parseSubmoduleConfig("submodule.modules/kid.v2.path sub/kid\nsubmodule.modules/kid.v2.url ../child\nsubmodule.other.branch main\n")
	if err != nil {
		t.Fatal(err)
	}
	if len(modules) != 1 || modules[0].name != "modules/kid.v2" || modules[0].path != "sub/kid" || modules[0].url != "../child" {
		t.Fatalf("modules=%+v, want the nested name kept intact", modules)
	}
	// 部分一致で弾くと正当な名前まで準備失敗になるため、`.git` は成分単位でだけ拒否する。
	if modules, err := parseSubmoduleConfig("submodule.libs/mylib.github.path sub\nsubmodule.libs/mylib.github.url ../child\n"); err != nil || len(modules) != 1 {
		t.Fatalf("modules=%+v err=%v, want a name that merely contains .git to be accepted", modules, err)
	}
	if _, _, ok := splitSubmoduleKey("submodule.path"); ok {
		t.Fatal("a key without a name was accepted")
	}
	if _, _, ok := splitSubmoduleKey("core.bare"); ok {
		t.Fatal("a non-submodule key was accepted")
	}
}

func submoduleGitlink(t *testing.T, repository, oid string) string {
	t.Helper()
	entry := gitOutput(t, repository, "ls-tree", oid, "--", submodulePath)
	fields := strings.Fields(entry)
	if len(fields) < 3 || fields[0] != "160000" {
		t.Fatalf("ls-tree entry=%q, want a gitlink", entry)
	}
	return fields[2]
}

func assertEmptyGitlinkDirectory(t *testing.T, path string) {
	t.Helper()
	entries, err := os.ReadDir(path)
	if err != nil {
		t.Fatalf("gitlink directory: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("gitlink directory %s has %d entries, want the empty directory checkout leaves", path, len(entries))
	}
}

// `.gitmodules` に url が無い entry は実体化しない。clone 後に origin を戻す先が無いためである。
func TestPrepareSkipsSubmoduleWithoutURL(t *testing.T) {
	t.Parallel()
	f := newSubmoduleFixture(t)
	if err := os.WriteFile(filepath.Join(f.repository, ".gitmodules"), []byte("[submodule \""+submoduleName+"\"]\n\tpath = "+submodulePath+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, f.repository, "add", ".gitmodules")
	gitCommand(t, f.repository, "commit", "-m", "drop submodule url")
	head := gitOutput(t, f.repository, "rev-parse", "HEAD")
	if err := f.preparer.Prepare(context.Background(), f.repo, f.target, head, "slot"); err != nil {
		t.Fatal(err)
	}
	assertEmptyGitlinkDirectory(t, f.submoduleTarget())
	if !strings.Contains(f.logged.String(), "submodule has no url in .gitmodules") {
		t.Fatalf("logged=%q, want a skip warning for the missing url", f.logged.String())
	}
}

// ローカル module に origin が無い場合も、戻し先が定まらないため実体化しない。
func TestPrepareSkipsSubmoduleWithoutModuleOrigin(t *testing.T) {
	t.Parallel()
	f := newSubmoduleFixture(t)
	gitCommand(t, f.moduleDir(), "--git-dir=.", "remote", "remove", "origin")
	if err := f.preparer.Prepare(context.Background(), f.repo, f.target, f.head, "slot"); err != nil {
		t.Fatal(err)
	}
	assertEmptyGitlinkDirectory(t, f.submoduleTarget())
	if !strings.Contains(f.logged.String(), "submodule local module has no origin url") {
		t.Fatalf("logged=%q, want a skip warning for the missing module origin", f.logged.String())
	}
}
