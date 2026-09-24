package main

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"
)

type checkSelection struct {
	name    string
	reasons []string
}

// テストとコンパイルは選ばない。コミット時はfmtと静的検査だけに留め、テストはGitHub Actionsへ任せる。
type selection struct {
	checks  map[string]*checkSelection
	skipped []string
	note    string
}

var checkOrder = []string{
	"fmt-check",
	"check-fast",
	"gitexec-check",
	"fuzz-check",
	"migrations-check",
	"catalog-check",
	"automation-check",
	"docs-check",
	"docs-index-check",
	"shell-check",
	"workflow-check",
	"mod-tidy-check",
	"testlayout-check",
	"lint",
}

var makefileChecks = []string{
	"fmt-check",
	"automation-check",
	"docs-check",
	"shell-check",
	"workflow-check",
	"check-fast",
	"gitexec-check",
	"fuzz-check",
	"catalog-check",
}

func selectChecks(files []changedFile) selection {
	result := selection{checks: map[string]*checkSelection{}}
	if len(files) == 0 {
		result.skipped = append(result.skipped, "staged diff is empty")
		return result
	}
	for _, file := range files {
		path := filepath.ToSlash(file.path)
		reason := fmt.Sprintf("%s: %s", file.status, path)
		matched := false
		if strings.HasSuffix(path, ".go") {
			matched = true
			result.addCheck("fmt-check", reason)
			result.addCheck("check-fast", reason)
			result.addCheck("gitexec-check", reason)
			if strings.HasSuffix(path, "_test.go") {
				result.addCheck("fuzz-check", reason)
			}
		}
		if isMigrationPath(path) {
			matched = true
			result.addCheck("migrations-check", reason)
		}
		if isMarkdownPath(path) {
			matched = true
			result.addCheck("docs-check", reason)
		}
		if isDocsIndexPath(path) {
			matched = true
			result.addCheck("docs-index-check", reason)
		}
		if isShellPath(path) {
			matched = true
			result.addCheck("shell-check", reason)
			result.addCheck("automation-check", reason)
		}
		if isWorkflowPath(path) {
			matched = true
			result.addCheck("workflow-check", reason)
			result.addCheck("automation-check", reason)
			if isNightlyWorkflow(path) {
				result.addCheck("fuzz-check", reason)
			}
		}
		if path == "go.mod" || path == "go.sum" {
			matched = true
			result.addCheck("mod-tidy-check", reason)
		}
		if path == "testlayout-exclusions.txt" {
			matched = true
			result.addCheck("testlayout-check", reason)
		}
		if path == ".golangci.yml" {
			matched = true
			result.addCheck("lint", reason)
		}
		if isMakefilePath(path) {
			matched = true
			for _, check := range makefileChecks {
				result.addCheck(check, reason)
			}
		}
		if isHookToolPath(path) {
			matched = true
			result.addCheck("automation-check", reason)
		}
		if structuralCheck, ok := structuralTool(path); ok {
			matched = true
			result.addCheck(structuralCheck, reason)
		}
		if !matched {
			result.skipped = append(result.skipped, fmt.Sprintf("%s: no matching check", reason))
		}
	}
	return result
}

func (s *selection) addCheck(name, reason string) {
	check := s.checks[name]
	if check == nil {
		check = &checkSelection{name: name}
		s.checks[name] = check
	}
	check.reasons = appendUnique(check.reasons, reason)
}

func (s *selection) sortedChecks() []*checkSelection {
	result := make([]*checkSelection, 0, len(s.checks))
	for _, name := range checkOrder {
		if check := s.checks[name]; check != nil {
			result = append(result, check)
		}
	}
	var rest []*checkSelection
	for name, check := range s.checks {
		found := false
		for _, ordered := range checkOrder {
			if ordered == name {
				found = true
				break
			}
		}
		if !found {
			rest = append(rest, check)
		}
	}
	sort.Slice(rest, func(i, j int) bool { return rest[i].name < rest[j].name })
	return append(result, rest...)
}

func (s selection) empty() bool {
	return len(s.checks) == 0
}

func appendUnique(values []string, value string) []string {
	for _, existing := range values {
		if existing == value {
			return values
		}
	}
	return append(values, value)
}

func isMigrationPath(path string) bool {
	return path == "migrations" || strings.HasPrefix(path, "migrations/")
}

func isMarkdownPath(path string) bool {
	if strings.HasPrefix(path, ".markdownlint") || strings.HasPrefix(path, "tools/markdownlint/") ||
		strings.HasPrefix(path, "tools/checkdoclinks/") {
		return true
	}
	return strings.HasSuffix(path, ".md") || strings.HasSuffix(path, ".markdown")
}

// 一覧の載せ忘れはAGENTS.mdとdocs/の対応でだけ起きるため、Markdown全体には広げない。
func isDocsIndexPath(path string) bool {
	return path == "AGENTS.md" || path == "docs" || strings.HasPrefix(path, "docs/")
}

// hook本体は拡張子を持てないため、scripts/hooks/直下は名前によらずshell扱いにする。
func isShellPath(path string) bool {
	if strings.HasPrefix(path, "scripts/hooks/") {
		return true
	}
	return strings.HasPrefix(path, "scripts/") && strings.HasSuffix(path, ".sh")
}

func isWorkflowPath(path string) bool {
	return strings.HasPrefix(path, ".github/workflows/") || strings.HasPrefix(path, ".github/actions/")
}

func isNightlyWorkflow(path string) bool {
	return filepath.Base(path) == "nightly.yml" || strings.Contains(filepath.Base(path), "nightly")
}

func isMakefilePath(path string) bool {
	return path == "Makefile" || strings.HasSuffix(path, "/Makefile")
}

func isHookToolPath(path string) bool {
	return path == "scripts/hook-check.sh" || strings.HasPrefix(path, "tools/hookcheck/")
}

func structuralTool(path string) (string, bool) {
	for _, name := range []struct {
		prefix string
		check  string
	}{
		{"tools/checkcomments/", "comments-check"},
		{"tools/checktests/", "tests-check"},
		{"tools/checklines/", "lines-check"},
		{"tools/checktestlayout/", "testlayout-check"},
		{"tools/checkfuzz/", "fuzz-check"},
		{"tools/checkgitexec/", "gitexec-check"},
		{"tools/checkmigrations/", "migrations-check"},
		{"tools/checkautomation/", "automation-check"},
		{"tools/checkdocsindex/", "docs-index-check"},
		{"tools/checkcatalog/", "catalog-check"},
	} {
		if strings.HasPrefix(path, name.prefix) {
			return name.check, true
		}
	}
	return "", false
}
