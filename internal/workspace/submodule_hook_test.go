package workspace

import (
	"bytes"
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
)

// 適格外の子を含む一括準備でも、post-checkout hook がその子を再初期化しない。
func TestPrepareBulkSubmodulesKeepsIneligibleChildEmpty(t *testing.T) {
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
	gitCommand(t, f.repository, "config", "--remove-section", "submodule."+submoduleName)
	gitCommand(t, f.repository, "config", "--remove-section", "submodule.modules/second")
	if err := os.RemoveAll(filepath.Join(string(f.repo.CommonDir), "modules", "modules", "second")); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(string(f.repo.CommonDir), "config")
	beforeConfig, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	f.preparer.submoduleWorkerCount = 1
	if err := f.preparer.Prepare(context.Background(), f.repo, f.target, f.head, "slot"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(f.target, submodulePath, "kid.txt")); err != nil {
		t.Fatalf("eligible submodule content: %v", err)
	}
	assertEmptyGitlinkDirectory(t, filepath.Join(f.target, "sub", "second"))
	afterConfig, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(beforeConfig, afterConfig) {
		t.Fatalf("source repository config changed:\nbefore:\n%s\nafter:\n%s", beforeConfig, afterConfig)
	}
}

type postCheckoutHookFixture struct {
	preparer      *Preparer
	repo          discovery.Repository
	target        string
	commonModules string
	runner        *gitx.Runner
}

func newPostCheckoutHookFixture(t *testing.T) *postCheckoutHookFixture {
	t.Helper()
	root := t.TempDir()
	target := filepath.Join(root, "target")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, target, "init", "-b", "main")
	common := filepath.Join(root, "source-common")
	commonModules := filepath.Join(common, "modules")
	if err := os.MkdirAll(commonModules, 0o700); err != nil {
		t.Fatal(err)
	}
	owner, _, err := domain.OpenOwnedRoot(root, root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = owner.Close() })
	cfg := config.Defaults()
	cfg.Storage.WorktreeRoot = root
	runner := &gitx.Runner{Timeout: 5 * time.Second}
	return &postCheckoutHookFixture{
		preparer: &Preparer{
			Git: runner, Config: cfg, OwnedRoot: owner, RootPath: root,
			Notices: &PrepareNotices{},
		},
		repo:          discovery.Repository{MainPath: domain.CanonicalPath(target), CommonDir: domain.CanonicalPath(common)},
		target:        target,
		commonModules: commonModules,
		runner:        runner,
	}
}

func (f *postCheckoutHookFixture) run(t *testing.T, result submodulePhaseResult) []string {
	t.Helper()
	var invoked [][]string
	f.runner.SetBeforeRunAtHook(func(args []string) { invoked = append(invoked, append([]string(nil), args...)) })
	if err := f.preparer.runPostCheckoutWithSubmodules(context.Background(), f.repo, f.target, "", "abc", result); err != nil {
		t.Fatalf("runPostCheckoutWithSubmodules() error=%v", err)
	}
	if len(invoked) != 1 {
		t.Fatalf("hook invocations=%d, want one: %v", len(invoked), invoked)
	}
	return invoked[0]
}

func hasHookArg(args []string, want string) bool {
	for _, arg := range args {
		if arg == want {
			return true
		}
	}
	return false
}

// eligible の件数は protocol と active path の選択を変えるため、増減を strict に検証する。
func TestRunPostCheckoutWithSubmodulesCountsEligibleChildren(t *testing.T) {
	t.Parallel()
	f := newPostCheckoutHookFixture(t)
	args := f.run(t, submodulePhaseResult{
		enabled: true,
		declared: []submodule{
			{name: "first", path: "sub/first"},
			{name: "second", path: "sub/second"},
		},
		decisions: []submoduleProbe{{eligible: true}, {eligible: false}},
	})
	for _, want := range []string{
		"-c", "protocol.file.allow=always", "-c", "submodule.active=:(top,literal)sub/first",
		"-c", "submodule.first.url=" + filepath.Join(f.commonModules, "first"),
		"-c", "submodule.first.active=true", "-c", "submodule.second.active=false",
	} {
		if !hasHookArg(args, want) {
			t.Fatalf("hook args=%v, missing %q", args, want)
		}
	}
	if hasHookArg(args, "submodule.active=:(top,exclude)**") {
		t.Fatalf("eligible child was replaced by the all-excluded active rule: %v", args)
	}
}

// 宣言はあるが適格な子が 0 件なら、hook の無指定 update を全 path 除外へ固定する。
func TestRunPostCheckoutWithSubmodulesExcludesAllWhenNoChildIsEligible(t *testing.T) {
	t.Parallel()
	f := newPostCheckoutHookFixture(t)
	args := f.run(t, submodulePhaseResult{
		enabled:   true,
		declared:  []submodule{{name: "first", path: "sub/first"}},
		decisions: []submoduleProbe{{eligible: false}},
	})
	if !hasHookArg(args, "submodule.active=:(top,exclude)**") {
		t.Fatalf("hook args=%v, want all paths excluded", args)
	}
	if hasHookArg(args, "protocol.file.allow=always") {
		t.Fatalf("hook args=%v, want no file protocol for zero eligible children", args)
	}
}

func TestRunPostCheckoutWithSubmodulesPropagatesHookFailure(t *testing.T) {
	t.Parallel()
	f := newPostCheckoutHookFixture(t)
	hook := filepath.Join(f.target, ".git", "hooks", "post-checkout")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\nexit 17\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	err := f.preparer.runPostCheckoutWithSubmodules(context.Background(), f.repo, f.target, "", "abc", submodulePhaseResult{
		enabled:   true,
		declared:  []submodule{{name: "first", path: "sub/first"}},
		decisions: []submoduleProbe{{eligible: false}},
	})
	if err == nil {
		t.Fatal("post-checkout hook failure was ignored")
	}
}

// decisions が宣言より短い場合も、残りの子を安全に省略して hook を実行する。
func TestRunPostCheckoutWithSubmodulesAllowsMissingDecisionAtExactBoundary(t *testing.T) {
	t.Parallel()
	f := newPostCheckoutHookFixture(t)
	args := f.run(t, submodulePhaseResult{
		enabled:   true,
		declared:  []submodule{{name: "first", path: "sub/first"}},
		decisions: nil,
	})
	if !hasHookArg(args, "submodule.active=:(top,exclude)**") {
		t.Fatalf("hook args=%v, want all paths excluded when decision is missing", args)
	}
}

// 宣言の末尾に判定が無い場合は、その子の URL を hook へ渡さず走査を止める。
func TestRunPostCheckoutWithSubmodulesStopsAtMissingDecision(t *testing.T) {
	t.Parallel()
	f := newPostCheckoutHookFixture(t)
	args := f.run(t, submodulePhaseResult{
		enabled: true,
		declared: []submodule{
			{name: "first", path: "sub/first"},
			{name: "missing", path: "sub/missing"},
		},
		decisions: []submoduleProbe{{eligible: true}},
	})
	if !hasHookArg(args, "submodule.first.url="+filepath.Join(f.commonModules, "first")) || hasHookArg(args, "submodule.missing.url="+filepath.Join(f.commonModules, "missing")) {
		t.Fatalf("hook args=%v, want only the decided child", args)
	}
	if !strings.Contains(strings.Join(args, " "), "hook run --ignore-missing post-checkout") {
		t.Fatalf("hook args=%v, missing post-checkout invocation", args)
	}
}
