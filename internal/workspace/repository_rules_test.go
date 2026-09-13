package workspace

import (
	"context"
	"os"
	"path/filepath"
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
