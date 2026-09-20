package workspace

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/HappyOnigiri/WX/internal/config"
)

// writeRepositoryFiles は fixture の main worktree へ file を作る。
func writeRepositoryFiles(t *testing.T, source string, files map[string]string) {
	t.Helper()
	for path, content := range files {
		path = filepath.Join(source, path)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// assertRepositoryRuleConflict は、利用者が原因のrepositoryと両manifestを1行で読めることを確かめる。
func assertRepositoryRuleConflict(t *testing.T, err error, source, overlap string) {
	t.Helper()
	if err == nil {
		t.Fatal("conflicting .worktreeinclude and .worktreelink rules were accepted")
	}
	for _, want := range []string{source, ".worktreeinclude and .worktreelink rules conflict", "copy and link rules overlap: " + overlap} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("conflict message %q does not name %q", err.Error(), want)
		}
	}
}

func TestPrepareStagedRejectsOverlappingIncludeAndLinkRules(t *testing.T) {
	t.Parallel()
	source, repo, preparer, _, target := prepareEdgesFixture(t)
	preparer.Config.Storage.CopyMode = config.CopyModeCopy
	writeRepositoryFiles(t, source, map[string]string{
		".gitignore":       "tools/audit/\n",
		".worktreeinclude": "tools/audit\n",
		".worktreelink":    "tools/audit\n",
	})
	gitCommand(t, source, "add", ".")
	gitCommand(t, source, "commit", "-m", "conflicting rules")
	oid := gitOutput(t, source, "rev-parse", "HEAD")
	writeRepositoryFiles(t, source, map[string]string{"tools/audit/report": "audit\n"})
	early := false
	_, err := preparer.PrepareStaged(context.Background(), "slot", []Preparation{{Repository: repo, Target: target, OID: oid}}, nil, func() error {
		early = true
		return nil
	})
	assertRepositoryRuleConflict(t, err, source, "tools/audit and tools/audit")
	if early {
		t.Fatal("preparation reached the early boundary despite conflicting rules")
	}
	if _, err := os.Lstat(filepath.Join(target, "tools", "audit")); !os.IsNotExist(err) {
		t.Fatalf("conflicting path was materialized before the check: %v", err)
	}
}

// TestCopyIncludesRejectsOverlappingRulesBeforeWriting は復元・単発準備の経路を対象にする。
// この経路は copy を先に行うため、検査が include の書き込み前で終わることを実体で確かめる。
func TestCopyIncludesRejectsOverlappingRulesBeforeWriting(t *testing.T) {
	t.Parallel()
	source, repo, preparer, _, target := prepareEdgesFixture(t)
	writeRepositoryFiles(t, source, map[string]string{
		".worktreeinclude": "local\ntools/audit\n",
		".worktreelink":    "tools/audit\n",
		"local/value":      "local\n",
		"tools/audit/repo": "audit\n",
	})
	if err := os.MkdirAll(target, 0o700); err != nil {
		t.Fatal(err)
	}
	assertRepositoryRuleConflict(t, preparer.copyIncludes(repo, target), source, "tools/audit and tools/audit")
	for _, path := range []string{"local", "tools"} {
		if _, err := os.Lstat(filepath.Join(target, path)); !os.IsNotExist(err) {
			t.Fatalf("include %s was written despite conflicting rules: %v", path, err)
		}
	}
}

// TestCopyIncludesRejectsOverlappingRulesWithMissingLinkSource は、link source が欠けて link が省かれる回でも
// 矛盾を通さないことを固定する。省略で準備が成功していた挙動を、明示した矛盾の拒否へ置き換えている。
func TestCopyIncludesRejectsOverlappingRulesWithMissingLinkSource(t *testing.T) {
	t.Parallel()
	source, repo, preparer, _, target := prepareEdgesFixture(t)
	writeRepositoryFiles(t, source, map[string]string{
		".worktreeinclude": "tools\n",
		".worktreelink":    "tools/audit\n",
		"tools/other":      "other\n",
	})
	if err := os.MkdirAll(target, 0o700); err != nil {
		t.Fatal(err)
	}
	assertRepositoryRuleConflict(t, preparer.copyIncludes(repo, target), source, "tools and tools/audit")
}

// TestPrepareStagedKeepsDistinctIncludeAndLinkRules は、default include 名を .worktreelink が所有する設定を
// 衝突と判定しないことを含めて、矛盾のない設定が従来どおり準備できることを確かめる。
func TestPrepareStagedKeepsDistinctIncludeAndLinkRules(t *testing.T) {
	t.Parallel()
	source, repo, preparer, _, target := prepareEdgesFixture(t)
	preparer.Config.Storage.CopyMode = config.CopyModeCopy
	writeRepositoryFiles(t, source, map[string]string{
		".gitignore":       "local\nshared\nAGENTS.local.md\n",
		".worktreeinclude": "local\n",
		".worktreelink":    "shared\nAGENTS.local.md\n",
	})
	gitCommand(t, source, "add", ".")
	gitCommand(t, source, "commit", "-m", "distinct rules")
	oid := gitOutput(t, source, "rev-parse", "HEAD")
	writeRepositoryFiles(t, source, map[string]string{
		"local/value":     "local\n",
		"shared/value":    "shared\n",
		"AGENTS.local.md": "local rules\n",
	})
	if _, err := preparer.PrepareStaged(context.Background(), "slot", []Preparation{{Repository: repo, Target: target, OID: oid}}, nil, func() error { return nil }); err != nil {
		t.Fatalf("distinct rules were rejected: %v", err)
	}
	if content, err := os.ReadFile(filepath.Join(target, "local", "value")); err != nil || string(content) != "local\n" {
		t.Fatalf("include content=%q: %v", content, err)
	}
	for _, path := range []string{"shared", "AGENTS.local.md"} {
		info, err := os.Lstat(filepath.Join(target, path))
		if err != nil || info.Mode()&os.ModeSymlink == 0 {
			t.Fatalf("link %s was not placed as a symlink: %v", path, err)
		}
	}
}

// TestExpandLinkPatternsSeparatesLiteralAndGlob は、.worktreelink の行のうちメタ文字を含む行だけが
// glob 展開され、メタ文字の無い行は不在でもそのまま残ることを固定する。
// literal を glob 経由にすると「欠落なら skip / 準備失敗」という下流の契約が両方壊れる。
func TestExpandLinkPatternsSeparatesLiteralAndGlob(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	for _, dir := range []string{"dir/one", "dir/two", "dir/sub/deep", "local-a", "local-b", "a/b/c", "a/z/c", ".claude/skills/local-x", ".claude/skills/local-y", "viasym-real"} {
		if err := os.MkdirAll(filepath.Join(base, dir), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"q1", "q2", "bar", "car"} {
		if err := os.WriteFile(filepath.Join(base, name), []byte("x\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(filepath.Join(base, "viasym-real"), filepath.Join(base, "viasym")); err != nil {
		t.Fatal(err)
	}
	root, err := OpenPhysicalRoot(base)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	for _, tc := range []struct {
		name     string
		patterns []string
		want     []string
	}{
		{"glob under directory", []string{"dir/*"}, []string{"dir/one", "dir/sub", "dir/two"}},
		{"top level glob", []string{"local-*"}, []string{"local-a", "local-b"}},
		{"trailing slash is cleaned", []string{".claude/skills/local-*/"}, []string{".claude/skills/local-x", ".claude/skills/local-y"}},
		{"single character glob", []string{"q?"}, []string{"q1", "q2"}},
		{"character class", []string{"[cb]ar"}, []string{"bar", "car"}},
		{"intermediate segment glob", []string{"a/*/c"}, []string{"a/b/c", "a/z/c"}},
		{"double star is a single star", []string{"dir/**"}, []string{"dir/one", "dir/sub", "dir/two"}},
		{"glob without matches is empty", []string{"nomatch-*"}, []string{}},
		{"literal without matches survives", []string{"missing-literal"}, []string{"missing-literal"}},
		{"literal trailing slash is cleaned", []string{"missing-literal/"}, []string{"missing-literal"}},
		{"duplicate across glob and literal", []string{"local-*", "local-a"}, []string{"local-a", "local-b"}},
		{"duplicate across two globs", []string{"local-*", "loc*-a"}, []string{"local-a", "local-b"}},
	} {
		if got, err := expandLinkPatternsAt(root, tc.patterns); err != nil || !slices.Equal(got, tc.want) {
			t.Fatalf("%s: got=%v want=%v err=%v", tc.name, got, tc.want, err)
		}
	}
	for _, tc := range []struct {
		name    string
		pattern string
	}{
		{"escape outside the root", "../outside/*"},
		{"absolute pattern", "/abs/*"},
		{"invalid syntax", "["},
		{"symlink ancestor", "viasym/*"},
	} {
		if got, err := expandLinkPatternsAt(root, []string{tc.pattern}); err == nil {
			t.Fatalf("%s: expected an error, got=%v", tc.name, got)
		}
	}
}

// TestCopyIncludesRejectsOverlapFromExpandedLinkGlob は、衝突検査が pattern ではなく展開後の path を見ることを固定する。
// pattern のまま渡すと `dir/*` は `dir/one` と一致せず、include/link が同じ path を所有する矛盾を素通りさせてしまう。
func TestCopyIncludesRejectsOverlapFromExpandedLinkGlob(t *testing.T) {
	t.Parallel()
	source, repo, preparer, _, target := prepareEdgesFixture(t)
	writeRepositoryFiles(t, source, map[string]string{
		".worktreeinclude": "dir/one\n",
		".worktreelink":    "dir/*\n",
		"dir/one/value":    "one\n",
		"dir/two/value":    "two\n",
	})
	if err := os.MkdirAll(target, 0o700); err != nil {
		t.Fatal(err)
	}
	assertRepositoryRuleConflict(t, preparer.copyIncludes(repo, target), source, "dir/one and dir/one")
}
