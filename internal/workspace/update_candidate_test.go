package workspace

import (
	"context"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/HappyOnigiri/WorktreeX/internal/discovery"
	"github.com/HappyOnigiri/WorktreeX/internal/domain"
	"github.com/HappyOnigiri/WorktreeX/internal/gitx"
	"github.com/HappyOnigiri/WorktreeX/internal/state"
)

func TestRejectChangedAttributesDetectsRootAndNestedChanges(t *testing.T) {
	t.Parallel()
	for _, testCase := range []struct {
		name       string
		change     func(t *testing.T, repository string)
		ineligible bool
	}{
		{name: "root added", change: func(t *testing.T, repository string) {
			writeTestFile(t, filepath.Join(repository, ".gitattributes"), "*.txt text eol=crlf\n")
		}, ineligible: true},
		{name: "nested changed", change: func(t *testing.T, repository string) {
			writeTestFile(t, filepath.Join(repository, "sub", ".gitattributes"), "*.txt -text\n")
		}, ineligible: true},
		{name: "nested removed", change: func(t *testing.T, repository string) {
			if err := os.Remove(filepath.Join(repository, "sub", ".gitattributes")); err != nil {
				t.Fatal(err)
			}
		}, ineligible: true},
		{name: "unrelated file only", change: func(t *testing.T, repository string) {
			writeTestFile(t, filepath.Join(repository, "sub", "b.txt"), "changed\n")
		}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			repository := t.TempDir()
			gitCommand(t, repository, "init", "-b", "main")
			gitCommand(t, repository, "config", "user.name", "test")
			gitCommand(t, repository, "config", "user.email", "test@example.com")
			writeTestFile(t, filepath.Join(repository, "a.txt"), "a\n")
			writeTestFile(t, filepath.Join(repository, "sub", "b.txt"), "b\n")
			writeTestFile(t, filepath.Join(repository, "sub", ".gitattributes"), "*.txt text\n")
			gitCommand(t, repository, "add", "-A")
			gitCommand(t, repository, "commit", "-m", "base")
			oldOID := gitOutput(t, repository, "rev-parse", "HEAD")
			testCase.change(t, repository)
			gitCommand(t, repository, "add", "-A")
			gitCommand(t, repository, "commit", "-m", "change")
			newOID := gitOutput(t, repository, "rev-parse", "HEAD")
			preparer := Preparer{Git: &gitx.Runner{Timeout: 30 * time.Second}}
			repo := discovery.Repository{MainPath: domain.CanonicalPath(repository)}
			diff, err := preparer.readUpdateTreeDiff(context.Background(), repo, oldOID, newOID)
			if err != nil {
				t.Fatal(err)
			}
			err = diff.rejectIneligible()
			if testCase.ineligible != errors.Is(err, ErrUpdateIneligible) {
				t.Fatalf("ineligible=%v err=%v", testCase.ineligible, err)
			}
			if !testCase.ineligible && err != nil {
				t.Fatalf("unexpected failure: %v", err)
			}
		})
	}
}

func TestTreeLeavesConflictsIncludeAncestorsAndDescendants(t *testing.T) {
	t.Parallel()
	tree := TreeLeaves{paths: map[string]bool{"cache/file": true, "leaf": true}}
	directories := tree.directories()
	for _, path := range []string{"cache", "cache/file", "leaf", "leaf/child"} {
		if !tree.conflicts(path, directories) {
			t.Fatalf("collision with %s was missed", path)
		}
	}
	for _, path := range []string{"cache-a", "cache/other", "leaf-b"} {
		if tree.conflicts(path, directories) {
			t.Fatalf("unrelated path %s collided", path)
		}
	}
}

// diff-treeの出力から、追加されたpath・要求OIDでのmode・更新不能条件を読み取る。
// rename検出を切った出力は1件につきpathを1つだけ持ち、型変化はDとAの組、または状態Tで現れる。
func TestParseUpdateTreeDiff(t *testing.T) {
	t.Parallel()
	record := func(oldMode, newMode, status, path string) string {
		return ":" + oldMode + " " + newMode + " " + strings.Repeat("1", 40) + " " + strings.Repeat("2", 40) + " " + status + "\x00" + path + "\x00"
	}
	diff, err := parseUpdateTreeDiff(record("100644", "000000", "D", "gone") + record("000000", "100755", "A", "dir/new") + record("100644", "120000", "T", "link") + record("100644", "100644", "M", "sub/.gitattributes/x"))
	if err != nil {
		t.Fatal(err)
	}
	if len(diff.added) != 1 || diff.added[0] != filepath.Join("dir", "new") {
		t.Fatalf("added=%v, want only dir/new", diff.added)
	}
	if diff.newModes["gone"] != "000000" || diff.newModes["dir/new"] != "100755" || diff.newModes["link"] != "120000" {
		t.Fatalf("newModes=%v", diff.newModes)
	}
	if !diff.attributes || diff.modules || diff.gitlinks {
		t.Fatalf("attributes=%v modules=%v gitlinks=%v, want only attributes", diff.attributes, diff.modules, diff.gitlinks)
	}
	gitlink, err := parseUpdateTreeDiff(record("160000", "160000", "M", "sub") + record("100644", "100644", "M", ".gitmodules"))
	if err != nil {
		t.Fatal(err)
	}
	if !gitlink.gitlinks || !gitlink.modules || !errors.Is(gitlink.rejectIneligible(), ErrUpdateIneligible) {
		t.Fatalf("gitlink diff=%+v was not rejected", gitlink)
	}
	truncatedRecord := ":100644 100644 " + strings.Repeat("1", 40) + " " + strings.Repeat("2", 40) + " M"
	for _, malformed := range []string{"garbage\x00path\x00", ":100644 100644 a b\x00path\x00", record("100644", "100644", "M", ""), truncatedRecord} {
		if _, err := parseUpdateTreeDiff(malformed); err == nil {
			t.Fatalf("malformed diff %q was accepted", malformed)
		}
	}
}

// 更新候補の既存配置が壊れている場合は、Git差分の検査より前に不適格として返す。
// testlint:allow-serial -- fixture preparation changes HOME through the shared setup
func TestValidateUpdateCandidateRejectsInvalidRecordedPlacement(t *testing.T) {
	ctx := context.Background()
	f := newSubmoduleFixture(t)
	if err := f.preparer.Prepare(ctx, f.repo, f.target, f.head, testSlotID); err != nil {
		t.Fatal(err)
	}
	previous := []state.Placement{{RelativePath: "missing", Kind: "copy", ContentSHA256: "hash"}}
	err := validateCollisionCandidate(ctx, f.preparer, f.repo, f.target, f.head, f.head, previous, nil)
	if !errors.Is(err, ErrUpdateIneligible) {
		t.Fatalf("invalid recorded placement error=%v, want ErrUpdateIneligible", err)
	}
}

// 要求OIDのtree列挙に失敗した場合は、空のtreeとして配置計画や衝突検査を続けない。
// testlint:allow-serial -- cowFixture が隔離 repository の構築中に HOME を変更する。
func TestListTreeLeavesPropagatesTreeError(t *testing.T) {
	ctx := context.Background()
	p, repo, oid, _ := cowFixture(t)
	treeOID := cowGit(t, string(repo.MainPath), "rev-parse", oid+"^{tree}")
	objectPath := filepath.Join(string(repo.CommonDir), "objects", treeOID[:2], treeOID[2:])
	if err := os.Rename(objectPath, objectPath+".mutation-test"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Rename(objectPath+".mutation-test", objectPath) })
	if _, err := p.ListTreeLeaves(ctx, repo, oid); err == nil {
		t.Fatal("tree enumeration error was ignored")
	}
}

// 衝突候補の配下の untracked/ignored path 列挙に失敗した場合は、不完全な集合で適格と判定しない。
// index を読む他の検査の巻き添えの失敗で成功しないよう、列挙だけを実行する helper を直接呼ぶ。
// testlint:allow-serial -- cowFixture が隔離 repository の構築中に HOME を変更する。
func TestUntrackedPathsUnderPropagatesListingError(t *testing.T) {
	ctx := context.Background()
	p, repo, oid, target := cowFixture(t)
	if err := p.Prepare(ctx, repo, target, oid, testSlotID); err != nil {
		t.Fatal(err)
	}
	identity, err := p.WorktreeIdentity(target)
	if err != nil {
		t.Fatal(err)
	}
	indexPath := cowGit(t, target, "rev-parse", "--path-format=absolute", "--git-path", "index")
	index, err := os.ReadFile(indexPath)
	if err != nil {
		t.Fatal(err)
	}
	var corrupted atomic.Bool
	t.Cleanup(func() {
		if corrupted.Load() {
			_ = os.WriteFile(indexPath, index, 0o600)
		}
	})
	p.Git.SetBeforeRunAtHook(func(args []string) {
		if len(args) < 2 || args[0] != "ls-files" || args[1] != "--others" {
			return
		}
		if err := os.WriteFile(indexPath, []byte("invalid index"), 0o600); err != nil {
			t.Error(err)
			return
		}
		corrupted.Store(true)
	})
	if _, err := p.untrackedPathsUnder(ctx, target, identity, []string{"file"}); err == nil {
		t.Fatal("untracked path enumeration error was ignored")
	}
	if !corrupted.Load() {
		t.Fatal("untracked path enumeration was not exercised")
	}
}

// 衝突候補の列挙は pathspec の上限ごとに分けて全件を調べ、glob 文字を含む path を magic として解釈しない。
// testlint:allow-serial -- cowFixture が隔離 repository の構築中に HOME を変更する。
func TestUntrackedPathsUnderBatchesLiteralPathspecs(t *testing.T) {
	ctx := context.Background()
	p, repo, oid, target := cowFixture(t)
	if err := p.Prepare(ctx, repo, target, oid, testSlotID); err != nil {
		t.Fatal(err)
	}
	identity, err := p.WorktreeIdentity(target)
	if err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(target, "glob-a"), "")
	writeTestFile(t, filepath.Join(target, "last"), "")
	probes := []string{"glob-*"}
	for i := 0; len(probes) < untrackedProbeBatch+1; i++ {
		probes = append(probes, "missing-"+strconv.Itoa(i))
	}
	probes = append(probes, "last")
	calls := 0
	p.Git.SetBeforeRunAtHook(func(args []string) {
		if len(args) > 1 && args[0] == "ls-files" && args[1] == "--others" {
			calls++
		}
	})
	listed, err := p.untrackedPathsUnder(ctx, target, identity, probes)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 2 || len(listed) != 1 || listed[0] != "last" {
		t.Fatalf("calls=%d listed=%v, want the last batch to find only last", calls, listed)
	}
}

// 所有権と tracked clean を確かめる前に、worktree で衝突候補の列挙や index flag の読み取りを走らせない。
// 衝突候補が実在する状態を作り、検査が走れば列挙の Git 起動が必ず観測されるようにする。
// testlint:allow-serial -- cowFixture が隔離 repository の構築中に HOME を変更する。
func TestValidateUpdateCandidateSkipsChecksWhenReadyValidationFails(t *testing.T) {
	ctx := context.Background()
	p, repo, oid, target := cowFixture(t)
	if err := p.Prepare(ctx, repo, target, oid, testSlotID); err != nil {
		t.Fatal(err)
	}
	main := string(repo.MainPath)
	writeTestFile(t, filepath.Join(main, "added"), "tracked\n")
	cowGit(t, main, "add", "added")
	cowGit(t, main, "commit", "-q", "-m", "add a path the standby already has")
	newOID := cowGit(t, main, "rev-parse", "HEAD")
	writeTestFile(t, filepath.Join(target, "added"), "local\n")
	var listed atomic.Bool
	p.Git.SetBeforeRunAtHook(func(args []string) {
		if len(args) > 1 && args[0] == "ls-files" && args[1] == "--others" {
			listed.Store(true)
		}
	})
	if err := validateCollisionCandidate(ctx, p, repo, target, oid, newOID, nil, nil); !errors.Is(err, ErrUpdateIneligible) {
		t.Fatalf("clean standby error=%v, want the collision to be found", err)
	}
	if !listed.Load() {
		t.Fatal("the collision listing was not observed")
	}
	listed.Store(false)
	if err := os.WriteFile(filepath.Join(target, "file"), []byte("dirty\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var started atomic.Bool
	p.Git.SetBeforeRunAtHook(func(args []string) {
		if len(args) > 0 && args[0] == "ls-files" {
			started.Store(true)
		}
	})
	if err := validateCollisionCandidate(ctx, p, repo, target, oid, newOID, nil, nil); !errors.Is(err, ErrTrackedChanges) {
		t.Fatalf("dirty standby error=%v, want ErrTrackedChanges", err)
	}
	if started.Load() {
		t.Fatal("update checks ran before the ready validation succeeded")
	}
}

// 並列の検査は全て完了を待ち、到着順ではなく並び順で最初のエラーを返す。
// 到着順に返すと、同じ状態でも貸出ごとにSTALE化するかどうかが変わり得る。
func TestRunChecksInOrderReturnsFirstErrorByPosition(t *testing.T) {
	t.Parallel()
	first := errors.New("first")
	last := errors.New("last")
	lastReturned := make(chan struct{})
	var middleRan atomic.Bool
	err := runChecksInOrder(context.Background(), []func(context.Context) error{
		func(context.Context) error {
			select {
			case <-lastReturned:
			case <-time.After(10 * time.Second):
				return errors.New("checks did not run concurrently")
			}
			return first
		},
		func(context.Context) error {
			middleRan.Store(true)
			return nil
		},
		func(context.Context) error {
			defer close(lastReturned)
			return last
		},
	})
	if !errors.Is(err, first) {
		t.Fatalf("error=%v, want the first check's error", err)
	}
	if !middleRan.Load() {
		t.Fatal("a check was skipped")
	}
	if err := runChecksInOrder(context.Background(), []func(context.Context) error{func(context.Context) error { return nil }}); err != nil {
		t.Fatalf("passing checks returned %v", err)
	}
}

// 失敗した検査より後ろの順位だけを取り消し、前の順位の検査は取り消さずに最後まで走らせる。
func TestRunChecksInOrderCancelsOnlyLaterChecks(t *testing.T) {
	t.Parallel()
	failed := errors.New("failed")
	laterDone := make(chan struct{})
	var earlierCanceled, laterCanceled atomic.Bool
	err := runChecksInOrder(context.Background(), []func(context.Context) error{
		func(ctx context.Context) error {
			select {
			case <-laterDone:
			case <-time.After(10 * time.Second):
			}
			earlierCanceled.Store(ctx.Err() != nil)
			return nil
		},
		func(context.Context) error { return failed },
		func(ctx context.Context) error {
			defer close(laterDone)
			select {
			case <-ctx.Done():
				laterCanceled.Store(true)
				return ctx.Err()
			case <-time.After(10 * time.Second):
				return nil
			}
		},
	})
	if !errors.Is(err, failed) {
		t.Fatalf("error=%v, want the failed check's error", err)
	}
	if !laterCanceled.Load() {
		t.Fatal("a later check was not canceled")
	}
	if earlierCanceled.Load() {
		t.Fatal("an earlier check was canceled")
	}
}

// 除外指定なしの ls-files --others は、untracked と ignored を別々に列挙した和集合と一致する。
// 衝突候補の列挙（untrackedPathsUnder）は 1 回の起動で済ませるためにこの一致へ依存している。
// testlint:allow-serial -- cowFixture が隔離 repository の構築中に HOME を変更する。
func TestUntrackedListingMatchesUntrackedAndIgnoredUnion(t *testing.T) {
	ctx := context.Background()
	p, repo, oid, target := cowFixture(t)
	if err := p.Prepare(ctx, repo, target, oid, testSlotID); err != nil {
		t.Fatal(err)
	}
	excludes := filepath.Join(t.TempDir(), "excludes")
	if err := os.WriteFile(excludes, []byte("global\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cowGit(t, target, "config", "core.excludesFile", excludes)
	infoExclude := cowGit(t, target, "rev-parse", "--path-format=absolute", "--git-path", "info/exclude")
	if err := os.MkdirAll(filepath.Dir(infoExclude), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(infoExclude, []byte("local\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		".gitignore":       "ignored/\n*.log\nignored-nested/\n",
		"ignored/a":        "",
		"ignored/sub/b":    "",
		"top.log":          "",
		"dir/plain":        "",
		"dir/nested.log":   "",
		"dir/global":       "",
		"global":           "",
		"local":            "",
		"nested/n":         "",
		"ignored-nested/m": "",
		"untracked":        "",
	}
	for name, body := range files {
		path := filepath.Join(target, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(target, "empty"), 0o755); err != nil {
		t.Fatal(err)
	}
	cowGit(t, filepath.Join(target, "nested"), "init", "-q")
	cowGit(t, filepath.Join(target, "ignored-nested"), "init", "-q")

	union, err := p.gitPaths(ctx, target, "ls-files", "--others", "--exclude-standard", "-z")
	if err != nil {
		t.Fatal(err)
	}
	ignored, err := p.gitPaths(ctx, target, "ls-files", "--others", "--ignored", "--exclude-standard", "-z")
	if err != nil {
		t.Fatal(err)
	}
	for path := range ignored {
		union[path] = true
	}
	merged, err := p.gitPaths(ctx, target, "ls-files", "--others", "-z")
	if err != nil {
		t.Fatal(err)
	}
	if !maps.Equal(union, merged) {
		t.Fatalf("ls-files --others=%v, want the union %v", merged, union)
	}
	for _, want := range []string{"ignored/sub/b", "global", "local", "nested", "ignored-nested", "untracked"} {
		if !merged[want] {
			t.Fatalf("ls-files --others=%v, want %s", merged, want)
		}
	}
}

// 正常な worktree では gitPaths が Git の NUL 区切り結果を path 集合へ変換する。
// testlint:allow-serial -- fixture preparation changes HOME through the shared setup
func TestGitPathsReturnsGitEntries(t *testing.T) {
	ctx := context.Background()
	f := newSubmoduleFixture(t)
	if err := f.preparer.Prepare(ctx, f.repo, f.target, f.head, testSlotID); err != nil {
		t.Fatal(err)
	}
	paths, err := f.preparer.gitPaths(ctx, f.target, "ls-files", "-z")
	if err != nil {
		t.Fatal(err)
	}
	if !paths["tracked"] {
		t.Fatalf("gitPaths=%v, want tracked", paths)
	}
}

// flag 付きの path が差分に乗るだけでは弾かない。更新は flag を解除して checkout し、内容を戻す。
// 弾くのは要求OIDで通常 file として残らない場合だけで、そこは flag を張り直す先が無い。
// testlint:allow-serial -- プロセス全体の環境（HOME）を変更するため
func TestValidateUpdateCandidateRejectsOnlyUnrestorableFlaggedIndexPaths(t *testing.T) {
	ctx := context.Background()
	p, repo, _, target := cowFixture(t)
	main := string(repo.MainPath)
	if err := os.WriteFile(filepath.Join(main, "other"), []byte("other\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cowGit(t, main, "add", ".")
	cowGit(t, main, "commit", "-m", "add a tracked file the update leaves alone")
	baseOID := cowGit(t, main, "rev-parse", "HEAD")
	if err := p.Prepare(ctx, repo, target, baseOID, testSlotID); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(main, "file"), []byte(cowBody+"after\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cowGit(t, main, "add", ".")
	cowGit(t, main, "commit", "-m", "rewrite a tracked file")
	newOID := cowGit(t, main, "rev-parse", "HEAD")
	if err := validateCollisionCandidate(ctx, p, repo, target, baseOID, newOID, nil, nil); err != nil {
		t.Fatalf("an update without index flags must stay eligible: %v", err)
	}
	// 差分に乗らない path の flag は checkout を妨げないので、更新は適格なままである。
	cowGit(t, target, "update-index", "--skip-worktree", "other")
	if err := validateCollisionCandidate(ctx, p, repo, target, baseOID, newOID, nil, nil); err != nil {
		t.Fatalf("a flag outside the diff must stay eligible: %v", err)
	}
	cowGit(t, target, "update-index", "--skip-worktree", "file")
	if err := validateCollisionCandidate(ctx, p, repo, target, baseOID, newOID, nil, nil); err != nil {
		t.Fatalf("a restorable flagged path must stay eligible: %v", err)
	}
	cowGit(t, main, "rm", "-q", "file")
	cowGit(t, main, "commit", "-m", "delete the flagged file")
	deletedOID := cowGit(t, main, "rev-parse", "HEAD")
	err := validateCollisionCandidate(ctx, p, repo, target, baseOID, deletedOID, nil, nil)
	if !errors.Is(err, ErrUpdateIneligible) {
		t.Fatalf("flagged path deleted at the requested OID: error=%v, want ErrUpdateIneligible", err)
	}
	if !strings.Contains(err.Error(), "file") {
		t.Fatalf("error=%v, want it to name the path that cannot be restored", err)
	}
}

func (p *Preparer) gitPaths(ctx context.Context, target string, args ...string) (map[string]bool, error) {
	identity, err := p.WorktreeIdentity(target)
	if err != nil {
		return nil, err
	}
	result, err := p.RunGitInWorktree(ctx, target, identity, nil, nil, args...)
	if err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for _, entry := range strings.Split(result.Stdout, "\x00") {
		if entry != "" {
			out[filepath.Clean(entry)] = true
		}
	}
	return out, nil
}
