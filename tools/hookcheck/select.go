package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

type checkSelection struct {
	name    string
	reasons []string
}

type testSelection struct {
	packages []string
	reasons  []string
	countOne bool
}

type selection struct {
	checks    map[string]*checkSelection
	tests     map[string]*testSelection
	compile   bool
	compileBy []string
	skipped   []string
	note      string
}

var checkOrder = []string{
	"fmt-check",
	"check-fast",
	"gitexec-check",
	"fuzz-check",
	"migrations-check",
	"docs-check",
	"shell-check",
	"workflow-check",
	"mod-tidy-check",
	"testlayout-check",
	"lint",
}

var makefileChecks = []string{
	"fmt-check",
	"docs-check",
	"shell-check",
	"workflow-check",
	"check-fast",
	"gitexec-check",
	"fuzz-check",
}

func selectChecks(root string, files []changedFile) (selection, error) {
	result := selection{
		checks: map[string]*checkSelection{},
		tests:  map[string]*testSelection{},
	}
	if len(files) == 0 {
		result.skipped = append(result.skipped, "staged diff is empty")
		return result, nil
	}
	for _, file := range files {
		path := filepath.ToSlash(file.path)
		reason := fmt.Sprintf("%s: %s", file.status, path)
		matched := false
		isGo := strings.HasSuffix(path, ".go")
		if isGo {
			matched = true
			result.addCheck("fmt-check", reason)
			result.addCheck("check-fast", reason)
			result.addCheck("gitexec-check", reason)
			if strings.HasSuffix(path, "_test.go") {
				result.addCheck("fuzz-check", reason)
			}
			if err := result.addFilePackage(root, file, reason, isTestdataPath(path)); err != nil {
				return result, err
			}
		}
		if isTestdataPath(path) {
			matched = true
			if !isGo {
				if err := result.addFilePackage(root, file, reason, true); err != nil {
					return result, err
				}
			}
		}
		if isMigrationPath(path) {
			matched = true
			result.addCheck("migrations-check", reason)
			result.addTest("./internal/state", reason, false)
		}
		if isMarkdownPath(path) {
			matched = true
			result.addCheck("docs-check", reason)
		}
		if isShellPath(path) {
			matched = true
			result.addCheck("shell-check", reason)
			result.addScriptTest(path, reason)
		}
		if isWorkflowPath(path) {
			matched = true
			result.addCheck("workflow-check", reason)
			if isNightlyWorkflow(path) {
				result.addCheck("fuzz-check", reason)
			}
		}
		if path == "go.mod" || path == "go.sum" {
			matched = true
			result.addCheck("mod-tidy-check", reason)
			result.compile = true
			result.compileBy = appendUnique(result.compileBy, reason)
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
			result.compile = true
			result.compileBy = appendUnique(result.compileBy, reason)
			result.addTest("./tools/hookcheck", reason, false)
		}
		if isHookToolPath(path) {
			matched = true
			result.addTest("./tools/hookcheck", reason, false)
		}
		if structuralCheck, ok := structuralTool(path); ok {
			matched = true
			result.addCheck(structuralCheck, reason)
		}
		if matched && !isGo && !isTestdataPath(path) && isStructuralToolPath(path) {
			if pkg := structuralPackage(path); pkg != "" {
				result.addTest(pkg, reason, false)
			}
		}
		if !matched {
			result.skipped = append(result.skipped, fmt.Sprintf("%s: no matching check", reason))
		}
	}
	return result, nil
}

func (s *selection) addCheck(name, reason string) {
	check := s.checks[name]
	if check == nil {
		check = &checkSelection{name: name}
		s.checks[name] = check
	}
	check.reasons = appendUnique(check.reasons, reason)
}

func (s *selection) addTest(pkg, reason string, countOne bool) {
	test := s.tests[pkg]
	if test == nil {
		test = &testSelection{packages: []string{pkg}}
		s.tests[pkg] = test
	}
	test.reasons = appendUnique(test.reasons, reason)
	test.countOne = test.countOne || countOne
}

func (s *selection) addFilePackage(root string, file changedFile, reason string, testdata bool) error {
	directory := filepath.Dir(filepath.FromSlash(file.path))
	if testdata {
		var found bool
		directory, found = findOwnerPackage(root, directory)
		if !found {
			if file.status == "D" {
				s.skipped = append(s.skipped, fmt.Sprintf("%s: testdata owner package no longer exists", reason))
				return nil
			}
			directory = testdataOwnerDirectory(directory)
		}
	} else if !directoryHasGoFiles(filepath.Join(root, directory)) {
		if file.status == "D" {
			s.skipped = append(s.skipped, fmt.Sprintf("%s: package was deleted", reason))
			return nil
		}
	}
	pkg, err := packagePath(root, directory)
	if err != nil {
		return fmt.Errorf("resolve package for %s: %w", file.path, err)
	}
	s.addTest(pkg, reason, false)
	return nil
}

func (s *selection) addScriptTest(path, reason string) {
	switch {
	case path == "scripts/test-focus.sh":
		s.addTest("./tools/testfocus", reason, true)
	case path == "scripts/test-darwin.sh":
		s.addTest("./tools/testdarwin", reason, true)
	case path == "scripts/build-release.sh", path == "scripts/install.sh", path == "scripts/uninstall.sh":
		s.addTest("./tools/testrelease", reason, true)
	}
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

func (s *selection) sortedTests() []*testSelection {
	groups := map[bool]*testSelection{}
	packages := make([]string, 0, len(s.tests))
	for pkg := range s.tests {
		packages = append(packages, pkg)
	}
	sort.Strings(packages)
	for _, pkg := range packages {
		test := s.tests[pkg]
		group := groups[test.countOne]
		if group == nil {
			group = &testSelection{countOne: test.countOne}
			groups[test.countOne] = group
		}
		group.packages = append(group.packages, pkg)
		for _, reason := range test.reasons {
			group.reasons = appendUnique(group.reasons, reason)
		}
	}
	result := make([]*testSelection, 0, len(groups))
	for _, group := range groups {
		result = append(result, group)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].countOne != result[j].countOne {
			return !result[i].countOne
		}
		return strings.Join(result[i].packages, " ") < strings.Join(result[j].packages, " ")
	})
	return result
}

func (s selection) empty() bool {
	return len(s.checks) == 0 && len(s.tests) == 0 && !s.compile
}

func appendUnique(values []string, value string) []string {
	for _, existing := range values {
		if existing == value {
			return values
		}
	}
	return append(values, value)
}

func isTestdataPath(path string) bool {
	for _, part := range strings.Split(path, "/") {
		if part == "testdata" {
			return true
		}
	}
	return false
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
	} {
		if strings.HasPrefix(path, name.prefix) {
			return name.check, true
		}
	}
	return "", false
}

func isStructuralToolPath(path string) bool {
	_, ok := structuralTool(path)
	return ok
}

func structuralPackage(path string) string {
	for _, prefix := range []string{
		"tools/checkcomments/",
		"tools/checktests/",
		"tools/checklines/",
		"tools/checktestlayout/",
		"tools/checkfuzz/",
		"tools/checkgitexec/",
		"tools/checkmigrations/",
	} {
		if strings.HasPrefix(path, prefix) {
			return "./" + strings.TrimSuffix(prefix, "/")
		}
	}
	return ""
}

func findOwnerPackage(root, directory string) (string, bool) {
	parts := strings.Split(filepath.ToSlash(directory), "/")
	for index, part := range parts {
		if part != "testdata" {
			continue
		}
		owner := filepath.FromSlash(strings.Join(parts[:index], "/"))
		if owner == "" {
			owner = "."
		}
		return owner, directoryHasGoFiles(filepath.Join(root, owner))
	}
	for {
		if directoryHasGoFiles(filepath.Join(root, directory)) {
			return directory, true
		}
		parent := filepath.Dir(directory)
		if parent == directory {
			break
		}
		directory = parent
	}
	return "", false
}

func testdataOwnerDirectory(directory string) string {
	parts := strings.Split(filepath.ToSlash(directory), "/")
	for index, part := range parts {
		if part == "testdata" {
			owner := strings.Join(parts[:index], "/")
			if owner == "" {
				return "."
			}
			return filepath.FromSlash(owner)
		}
	}
	return directory
}

func directoryHasGoFiles(directory string) bool {
	entries, err := os.ReadDir(directory)
	if err != nil {
		return false
	}
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".go") {
			return true
		}
	}
	return false
}

func packagePath(root, directory string) (string, error) {
	relative, err := filepath.Rel(root, filepath.Join(root, directory))
	if err != nil {
		return "", err
	}
	if relative == "." {
		return ".", nil
	}
	return "./" + filepath.ToSlash(relative), nil
}
