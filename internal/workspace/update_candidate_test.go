package workspace

import (
	"context"
	"errors"
	"maps"
	"os"
	"path/filepath"
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
			err := preparer.rejectChangedAttributes(context.Background(), repo, oldOID, newOID)
			if testCase.ineligible != errors.Is(err, ErrUpdateIneligible) {
				t.Fatalf("ineligible=%v err=%v", testCase.ineligible, err)
			}
			if !testCase.ineligible && err != nil {
				t.Fatalf("unexpected failure: %v", err)
			}
		})
	}
}

func TestPathsConflictAnyIncludesAncestors(t *testing.T) {
	t.Parallel()
	if !pathsConflictAny("cache", map[string]bool{"cache/file": true}) {
		t.Fatal("ancestor collision was missed")
	}
	if pathsConflictAny("cache-a", map[string]bool{"cache-b": true}) {
		t.Fatal("unrelated paths collided")
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
	err := f.preparer.ValidateUpdateCandidate(ctx, f.repo, f.target, f.head, f.head, previous, nil)
	if !errors.Is(err, ErrUpdateIneligible) {
		t.Fatalf("invalid recorded placement error=%v, want ErrUpdateIneligible", err)
	}
}

// 更新候補の tracked path 列挙に失敗した場合は、空の tree として衝突検査を続けない。
// 検査は並列に走るため、退避した object は戻さずに終える。他の検査が巻き添えで失敗しても、エラーが返ることは変わらない。
// testlint:allow-serial -- cowFixture が隔離 repository の構築中に HOME を変更する。
func TestValidateUpdateCandidatePropagatesTrackedPathError(t *testing.T) {
	ctx := context.Background()
	p, repo, oid, target := cowFixture(t)
	if err := p.Prepare(ctx, repo, target, oid, testSlotID); err != nil {
		t.Fatal(err)
	}
	treeOID := cowGit(t, string(repo.MainPath), "rev-parse", oid+"^{tree}")
	objectPath := filepath.Join(string(repo.CommonDir), "objects", treeOID[:2], treeOID[2:])
	backupPath := objectPath + ".mutation-test"
	var moved atomic.Bool
	t.Cleanup(func() {
		if moved.Load() {
			_ = os.Rename(backupPath, objectPath)
		}
	})
	p.Git.SetBeforeRunAtHook(func(args []string) {
		if strings.Join(args, "\x00") != strings.Join([]string{"ls-tree", "-r", "--name-only", "-z", oid}, "\x00") {
			return
		}
		if err := os.Rename(objectPath, backupPath); err != nil {
			t.Error(err)
			return
		}
		moved.Store(true)
	})
	if err := p.ValidateUpdateCandidate(ctx, repo, target, oid, oid, nil, nil); err == nil {
		t.Fatal("tracked path enumeration error was ignored")
	}
	if !moved.Load() {
		t.Fatal("tracked path enumeration was not exercised")
	}
}

// 更新候補の untracked/ignored path 列挙に失敗した場合は、不完全な集合で適格と判定しない。
// testlint:allow-serial -- cowFixture が隔離 repository の構築中に HOME を変更する。
func TestValidateUpdateCandidatePropagatesUntrackedPathError(t *testing.T) {
	ctx := context.Background()
	p, repo, oid, target := cowFixture(t)
	if err := p.Prepare(ctx, repo, target, oid, testSlotID); err != nil {
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
		if strings.Join(args, "\x00") != "ls-files\x00--others\x00-z" {
			return
		}
		if err := os.WriteFile(indexPath, []byte("invalid index"), 0o600); err != nil {
			t.Error(err)
			return
		}
		corrupted.Store(true)
	})
	if err := p.ValidateUpdateCandidate(ctx, repo, target, oid, oid, nil, nil); err == nil {
		t.Fatal("untracked path enumeration error was ignored")
	}
	if !corrupted.Load() {
		t.Fatal("untracked path enumeration was not exercised")
	}
}

// 所有権と tracked clean を確かめる前に、worktree で並列の検査を走らせない。
// testlint:allow-serial -- cowFixture が隔離 repository の構築中に HOME を変更する。
func TestValidateUpdateCandidateSkipsChecksWhenReadyValidationFails(t *testing.T) {
	ctx := context.Background()
	p, repo, oid, target := cowFixture(t)
	if err := p.Prepare(ctx, repo, target, oid, testSlotID); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "file"), []byte("dirty\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var started atomic.Bool
	p.Git.SetBeforeRunAtHook(func(args []string) {
		if len(args) > 0 && (args[0] == "ls-tree" || args[0] == "ls-files") {
			started.Store(true)
		}
	})
	if err := p.ValidateUpdateCandidate(ctx, repo, target, oid, oid, nil, nil); !errors.Is(err, ErrTrackedChanges) {
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
	err := runChecksInOrder([]func() error{
		func() error {
			<-lastReturned
			return first
		},
		func() error {
			middleRan.Store(true)
			return nil
		},
		func() error {
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
	if err := runChecksInOrder([]func() error{func() error { return nil }}); err != nil {
		t.Fatalf("passing checks returned %v", err)
	}
}

// 除外指定なしの ls-files --others は、untracked と ignored を別々に列挙した和集合と一致する。
// ValidateUpdateCandidate は 1 回の走査で済ませるためにこの一致へ依存している。
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
	if err := p.ValidateUpdateCandidate(ctx, repo, target, baseOID, newOID, nil, nil); err != nil {
		t.Fatalf("an update without index flags must stay eligible: %v", err)
	}
	// 差分に乗らない path の flag は checkout を妨げないので、更新は適格なままである。
	cowGit(t, target, "update-index", "--skip-worktree", "other")
	if err := p.ValidateUpdateCandidate(ctx, repo, target, baseOID, newOID, nil, nil); err != nil {
		t.Fatalf("a flag outside the diff must stay eligible: %v", err)
	}
	cowGit(t, target, "update-index", "--skip-worktree", "file")
	if err := p.ValidateUpdateCandidate(ctx, repo, target, baseOID, newOID, nil, nil); err != nil {
		t.Fatalf("a restorable flagged path must stay eligible: %v", err)
	}
	cowGit(t, main, "rm", "-q", "file")
	cowGit(t, main, "commit", "-m", "delete the flagged file")
	deletedOID := cowGit(t, main, "rev-parse", "HEAD")
	err := p.ValidateUpdateCandidate(ctx, repo, target, baseOID, deletedOID, nil, nil)
	if !errors.Is(err, ErrUpdateIneligible) {
		t.Fatalf("flagged path deleted at the requested OID: error=%v, want ErrUpdateIneligible", err)
	}
	if !strings.Contains(err.Error(), "file") {
		t.Fatalf("error=%v, want it to name the path that cannot be restored", err)
	}
}
