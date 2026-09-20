package workspace

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
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

func (f *submoduleFixture) commitGitmodules(t *testing.T, content string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(f.repository, ".gitmodules"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, f.repository, "add", ".gitmodules")
	gitCommand(t, f.repository, "commit", "-m", "update gitmodules")
	return gitOutput(t, f.repository, "rev-parse", "HEAD")
}

func assertPreparedEmptyGitmodules(t *testing.T, f *submoduleFixture, content string) {
	t.Helper()
	got, err := os.ReadFile(filepath.Join(f.target, ".gitmodules"))
	if err != nil {
		t.Fatalf("prepared .gitmodules: %v", err)
	}
	if string(got) != content {
		t.Fatalf("prepared .gitmodules=%q, want %q", got, content)
	}
	assertEmptyGitlinkDirectory(t, f.submoduleTarget())
	if strings.Contains(f.logged.String(), "submodule") {
		t.Fatalf("logged=%q, want no warning for an empty submodule definition", f.logged.String())
	}
}

// Git の設定検索が空定義を返す各形式は、通常準備で submodule なしとして成功する。
func TestPrepareTreatsEmptyGitmodulesAsNoSubmodules(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name    string
		content string
	}{
		{name: "zero-byte"},
		{name: "whitespace", content: " \n\t\n"},
		{name: "comments", content: "# no modules\n; still empty\n"},
		{name: "other-keys", content: "[core]\n\tbare = false\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			f := newSubmoduleFixture(t)
			head := f.commitGitmodules(t, test.content)
			if err := f.preparer.Prepare(context.Background(), f.repo, f.target, head, "slot"); err != nil {
				t.Fatal(err)
			}
			assertPreparedEmptyGitmodules(t, f, test.content)
		})
	}
}

// 0-byte の .gitmodules は二段階準備の後半でも空定義として成功する。
func TestPrepareStagedTreatsEmptyGitmodulesAsNoSubmodules(t *testing.T) {
	t.Parallel()
	f := newSubmoduleFixture(t)
	head := f.commitGitmodules(t, "")
	if _, err := f.preparer.PrepareStaged(context.Background(), "slot", []Preparation{{Repository: f.repo, Target: f.target, OID: head}}, nil, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	assertPreparedEmptyGitmodules(t, f, "")
}

// 0-byte の .gitmodules は復元準備でも空定義として成功する。
func TestPrepareForRestoreTreatsEmptyGitmodulesAsNoSubmodules(t *testing.T) {
	t.Parallel()
	f := newSubmoduleFixture(t)
	head := f.commitGitmodules(t, "")
	if err := f.preparer.PrepareForRestore(context.Background(), f.repo, f.target, head, "slot"); err != nil {
		t.Fatal(err)
	}
	assertPreparedEmptyGitmodules(t, f, "")
}

func TestPreparePropagatesInvalidGitmodulesConfig(t *testing.T) {
	t.Parallel()
	f := newSubmoduleFixture(t)
	head := f.commitGitmodules(t, "[submodule \""+submoduleName+"\"]\n\tpath = "+submodulePath+"\n\turl = ../child\n[broken\n")
	err := f.preparer.Prepare(context.Background(), f.repo, f.target, head, "slot")
	var gitErr *gitx.Error
	if !errors.As(err, &gitErr) {
		t.Fatalf("Prepare() error=%v, want the Git config error", err)
	}
	if gitErr.Result.Stderr == "" {
		t.Fatalf("Git config error result=%+v, want diagnostics", gitErr.Result)
	}
}

func TestIsEmptySubmoduleConfigRequiresAConfirmedEmptySearchFailure(t *testing.T) {
	t.Parallel()
	baseError := func(result gitx.Result) error { return &gitx.Error{Result: result} }
	for _, test := range []struct {
		name string
		ctx  context.Context
		err  error
		want bool
	}{
		{name: "empty search", err: baseError(gitx.Result{ExitCode: 1}), want: true},
		{name: "stdout", err: baseError(gitx.Result{ExitCode: 1, Stdout: "unexpected\n"})},
		{name: "stderr", err: baseError(gitx.Result{ExitCode: 1, Stderr: "fatal\n"})},
		{name: "other exit", err: baseError(gitx.Result{ExitCode: 2})},
		{name: "ordinary error", err: errors.New("runner failed")},
		{name: "cancelled context", ctx: emptySubmoduleCancelledContext(), err: baseError(gitx.Result{ExitCode: 1})},
		{name: "deadline context", ctx: emptySubmoduleDeadlineContext(t), err: baseError(gitx.Result{ExitCode: 1})},
		{name: "wrapped cancellation", err: errors.Join(errors.New("runner failed"), context.Canceled)},
		{name: "wrapped deadline", err: errors.Join(errors.New("runner failed"), context.DeadlineExceeded)},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := test.ctx
			if ctx == nil {
				ctx = context.Background()
			}
			if got := isEmptySubmoduleConfig(ctx, test.err); got != test.want {
				t.Fatalf("isEmptySubmoduleConfig()=%t, want %t", got, test.want)
			}
		})
	}
}

func emptySubmoduleCancelledContext() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}

func emptySubmoduleDeadlineContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	t.Cleanup(cancel)
	return ctx
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

// 複数の子は一括 update へまとめ、各子の origin 復元だけを個別に行う。
func TestPrepareMaterializesMultipleSubmodulesInOneUpdate(t *testing.T) {
	t.Parallel()
	f := newSubmoduleFixture(t)
	secondChild := filepath.Join(filepath.Dir(f.child), "second-child")
	if err := os.Mkdir(secondChild, 0o700); err != nil {
		t.Fatal(err)
	}
	initTestRepository(t, secondChild)
	if err := os.WriteFile(filepath.Join(secondChild, "second.txt"), []byte("second\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, secondChild, "add", ".")
	gitCommand(t, secondChild, "commit", "-m", "second initial")
	gitCommand(t, f.repository, "-c", "protocol.file.allow=always", "submodule", "add", "--name", "modules/second", "../second-child", "sub/second")
	gitCommand(t, f.repository, "add", ".")
	gitCommand(t, f.repository, "commit", "-m", "add second submodule")
	f.head = gitOutput(t, f.repository, "rev-parse", "HEAD")
	// 共有 config に submodule の url/active が無い状態でも、update の -c だけで実体化できることを確認する。
	gitCommand(t, f.repository, "config", "--remove-section", "submodule."+submoduleName)
	gitCommand(t, f.repository, "config", "--remove-section", "submodule.modules/second")
	configPath := filepath.Join(string(f.repo.CommonDir), "config")
	f.preparer.submoduleWorkerCount = 1
	f.preparer.Phases = &PhaseTimings{}
	var mu sync.Mutex
	var invoked [][]string
	f.runner.SetBeforeRunAtHook(func(args []string) {
		mu.Lock()
		defer mu.Unlock()
		invoked = append(invoked, append([]string(nil), args...))
	})
	beforeConfig, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.preparer.Prepare(context.Background(), f.repo, f.target, f.head, "slot"); err != nil {
		t.Fatal(err)
	}
	afterConfig, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(beforeConfig, afterConfig) {
		t.Fatalf("source repository config changed:\nbefore:\n%s\nafter:\n%s", beforeConfig, afterConfig)
	}
	for _, item := range []struct {
		path   string
		origin string
		file   string
	}{
		{path: submodulePath, origin: f.child, file: "kid.txt"},
		{path: "sub/second", origin: secondChild, file: "second.txt"},
	} {
		target := filepath.Join(f.target, item.path)
		if _, err := os.Stat(filepath.Join(target, item.file)); err != nil {
			t.Fatalf("submodule %s content: %v", item.path, err)
		}
		if origin := gitOutput(t, target, "remote", "get-url", "origin"); origin != item.origin {
			t.Fatalf("submodule %s origin=%s, want %s", item.path, origin, item.origin)
		}
	}
	updates := 0
	for _, args := range invoked {
		joined := strings.Join(args, " ")
		if !strings.Contains(joined, "submodule update") {
			continue
		}
		updates++
		if !strings.Contains(joined, "--jobs=1") || !strings.Contains(joined, "submodule.modules/kid.url=") || !strings.Contains(joined, "submodule.modules/second.url=") {
			t.Fatalf("bulk submodule update args=%v", args)
		}
		if !strings.Contains(joined, "sub/kid") || !strings.Contains(joined, "sub/second") {
			t.Fatalf("bulk submodule update omitted path: %v", args)
		}
	}
	if updates != 1 {
		t.Fatalf("submodule update invocations=%d, want one; commands=%v", updates, invoked)
	}
	phases := map[string]Phase{}
	for _, phase := range f.preparer.Phases.Phases() {
		phases[phase.Name] = phase
	}
	for _, name := range []string{"submodule.declared", "submodule.eligible", "submodule.inspect", "submodule.materialize", "submodule.origin"} {
		if _, ok := phases[name]; !ok {
			t.Fatalf("phase %q missing from %+v", name, phases)
		}
	}
	if phases["submodule.declared"].Count != 2 || phases["submodule.eligible"].Count != 2 || phases["submodule.inspect"].Count != 2 || phases["submodule.materialize"].Count != 1 || phases["submodule.origin"].Count != 2 {
		t.Fatalf("submodule phase counts=%+v", phases)
	}
}

// origin 復元の書込みが1件でも失敗した場合は、実体化済みの準備を成功扱いにしない。
func TestPrepareFailsWhenSubmoduleOriginRestoreFails(t *testing.T) {
	t.Parallel()
	f := newSubmoduleFixture(t)
	f.preparer.submoduleWorkerCount = 1
	failed := false
	f.runner.SetBeforeRunAtHook(func(args []string) {
		if failed || len(args) < 4 || args[0] != "-C" || args[1] != submodulePath || args[2] != "remote" || args[3] != "set-url" {
			return
		}
		failed = true
		gitCommand(t, filepath.Join(f.target, submodulePath), "config", "--remove-section", "remote.origin")
	})
	err := f.preparer.Prepare(context.Background(), f.repo, f.target, f.head, "slot")
	if err == nil || !strings.Contains(err.Error(), "restore submodule") {
		t.Fatalf("Prepare() error=%v, want origin restore failure", err)
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
	moduleIndexPath := filepath.Join(f.moduleDir(), "index")
	before, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	moduleIndexBefore, err := os.ReadFile(moduleIndexPath)
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
	moduleIndexAfter, err := os.ReadFile(moduleIndexPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(moduleIndexBefore, moduleIndexAfter) {
		t.Fatalf("source submodule index changed")
	}
	if status := gitOutput(t, f.repository, "status", "--porcelain", "--ignore-submodules=none"); status != statusBefore {
		t.Fatalf("source worktree status=%q, want the unchanged %q", status, statusBefore)
	}
}

// 方針を無効にした workspace では submodule 関連の Git を1回も起動しない。
func TestPrepareSkipsSubmodulePhaseWhenDisabled(t *testing.T) {
	t.Parallel()
	f := newSubmoduleFixture(t)
	disabled := false
	f.preparer.Config.RepositoryDefaults.Submodules = &disabled
	var invoked []string
	f.runner.SetBeforeRunAtHook(func(args []string) { invoked = append(invoked, strings.Join(args, " ")) })
	if err := f.preparer.Prepare(context.Background(), f.repo, f.target, f.head, "slot"); err != nil {
		t.Fatal(err)
	}
	for _, command := range invoked {
		// tracked file を展開する checkout の `--no-recurse-submodules` は submodule を触らないための抑止なので、
		// 実体化に関わる起動かどうかの判定から外す。
		probed := strings.ReplaceAll(command, "--no-recurse-submodules", "")
		if strings.Contains(probed, "submodule") || strings.Contains(probed, ".gitmodules") {
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
	f.preparer.Config.Workspaces = map[string]config.Workspace{root: {
		RepositoryDefaults: config.RepositoryDefaults{Submodules: &disabled},
	}}
	enabled, err := f.preparer.submodulesEnabled(f.repo)
	if err != nil {
		t.Fatal(err)
	}
	if enabled {
		t.Fatal("workspace override did not disable submodule materialization")
	}
	f.preparer.Config.RepositoryDefaults.Submodules = &disabled
	f.preparer.Config.Workspaces = map[string]config.Workspace{}
	if enabled, err := f.preparer.submodulesEnabled(f.repo); err != nil || enabled {
		t.Fatalf("global policy enabled=%t err=%v, want false", enabled, err)
	}
}

func TestSubmodulesEnabledUsesRepositoryMemberOverride(t *testing.T) {
	t.Parallel()
	f := newSubmoduleFixture(t)
	root, err := repositoryWorkspaceRoot(f.repo)
	if err != nil {
		t.Fatal(err)
	}
	disabled, enabled := false, true
	relative := f.repo.RelativePath
	if relative == "" {
		relative = "."
	}
	cfg := f.preparer.Config
	cfg.RepositoryDefaults.Submodules = &disabled
	cfg.Workspaces = map[string]config.Workspace{root: {
		RepositoryDefaults: config.RepositoryDefaults{Submodules: &disabled},
		Repositories: map[string]config.Repository{
			relative: {Submodules: &enabled},
		},
	}}
	f.preparer.Config = cfg
	if got, err := f.preparer.submodulesEnabled(f.repo); err != nil || !got {
		t.Fatalf("submodulesEnabled()=%t err=%v, want repository member override true", got, err)
	}
}

func TestRecordSubmoduleOutcomeAddsToConfiguredCollector(t *testing.T) {
	t.Parallel()
	collector := &SubmoduleOutcomes{}
	p := &Preparer{SubmoduleOutcomes: collector}
	repo := discovery.Repository{MainPath: "/repo"}
	p.recordSubmoduleOutcome(repo, submodule{path: "sub/kid"}, SubmoduleActionSkipped, SubmoduleReasonObjectMissing)
	_, items := collector.Snapshot()
	if len(items) != 1 || items[0].Repository != "/repo" || items[0].Reason != SubmoduleReasonObjectMissing {
		t.Fatalf("outcomes=%+v, want one recorded outcome", items)
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
	if _, _, ok := splitSubmoduleKey("submodule..url"); ok {
		t.Fatal("a key with an empty name was accepted")
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
