package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseDiffHandlesNULPaths(t *testing.T) {
	t.Parallel()
	files, err := parseDiff("M\x00a file\x00A\x00line\nfile\x00D\x00old\x00")
	if err != nil {
		t.Fatalf("parseDiff: %v", err)
	}
	if len(files) != 3 {
		t.Fatalf("got %d files, want 3", len(files))
	}
	if files[1].path != "line\nfile" || files[2].status != "D" {
		t.Fatalf("files=%#v", files)
	}
}

func TestParseDiffRejectsIncompleteRecord(t *testing.T) {
	t.Parallel()
	if _, err := parseDiff("M\x00"); err == nil {
		t.Fatal("incomplete record accepted")
	}
}

func TestSelectChecksForMixedChanges(t *testing.T) {
	t.Parallel()
	selected := selectChecks([]changedFile{
		{status: "M", path: "internal/state/state.go"},
		{status: "M", path: "README.md"},
		{status: "M", path: "migrations/004.sql"},
		{status: "M", path: "scripts/test-focus.sh"},
		{status: "M", path: ".github/workflows/nightly.yml"},
		{status: "M", path: "Makefile"},
	})
	for _, name := range []string{"fmt-check", "check-fast", "gitexec-check", "docs-check", "migrations-check", "shell-check", "workflow-check", "fuzz-check"} {
		if selected.checks[name] == nil {
			t.Errorf("missing check %s", name)
		}
	}
}

func TestSelectChecksCoversHookSources(t *testing.T) {
	t.Parallel()
	selected := selectChecks([]changedFile{{status: "A", path: "scripts/hooks/pre-commit"}})
	if selected.checks["shell-check"] == nil {
		t.Fatalf("checks=%#v, want shell-check for a hook without a .sh suffix", selected.checks)
	}
}

// テストはGitHub Actionsへ任せるため、Goやtestdataの変更でもテストとコンパイルを選ばない。
func TestSelectChecksNeverSelectsTests(t *testing.T) {
	t.Parallel()
	selected := selectChecks([]changedFile{
		{status: "M", path: "internal/daemon/manager.go"},
		{status: "M", path: "internal/agent/testdata/input.txt"},
		{status: "M", path: "go.mod"},
		{status: "M", path: "Makefile"},
		{status: "M", path: "tools/checkcomments/main.go"},
	})
	var output bytes.Buffer
	printPlan(&output, selected)
	if strings.Contains(output.String(), "go test") {
		t.Fatalf("plan selected tests: %q", output.String())
	}
	if !strings.Contains(output.String(), "internal/agent/testdata/input.txt: no matching check") {
		t.Fatalf("plan=%q, want testdata-only change reported as unchecked", output.String())
	}
}

func TestSelectChecksReportsUnknownFiles(t *testing.T) {
	t.Parallel()
	selected := selectChecks([]changedFile{{status: "M", path: "LICENSE"}})
	if !selected.empty() || len(selected.skipped) != 1 {
		t.Fatalf("selection=%#v", selected)
	}
	var output bytes.Buffer
	printPlan(&output, selected)
	if !strings.Contains(output.String(), "no matching check") {
		t.Fatalf("plan=%q", output.String())
	}
}

func TestSelectChecksRunsDocsForCustomMarkdownRules(t *testing.T) {
	t.Parallel()
	selected := selectChecks([]changedFile{{status: "M", path: "tools/checkdoclinks/wx014.mjs"}})
	if selected.checks["docs-check"] == nil {
		t.Fatalf("checks=%#v", selected.checks)
	}
}

func TestCleanEnvironmentRemovesRepositoryGitVariables(t *testing.T) {
	t.Setenv("GIT_DIR", "/wrong/repository")
	t.Setenv("GIT_INDEX_FILE", "/wrong/index")
	t.Setenv("GIT_AUTHOR_NAME", "kept")
	values := cleanEnvironment()
	joined := strings.Join(values, "\n")
	if strings.Contains(joined, "GIT_DIR=/wrong/repository") || strings.Contains(joined, "GIT_INDEX_FILE=/wrong/index") {
		t.Fatalf("repository environment leaked: %q", joined)
	}
	if !strings.Contains(joined, "GIT_AUTHOR_NAME=kept") {
		t.Fatalf("non-repository Git variable was removed: %q", joined)
	}
}

func TestPrintPlanUsesStableCommands(t *testing.T) {
	t.Parallel()
	selected := selectChecks([]changedFile{{status: "M", path: "internal/state/state.go"}})
	selected.note = "note"
	var output bytes.Buffer
	printPlan(&output, selected)
	text := output.String()
	if !strings.Contains(text, "command: make fmt-check") || !strings.Contains(text, "command: make check-fast") {
		t.Fatalf("plan=%q", text)
	}
}

func TestRunCLIRequiresResolvedPaths(t *testing.T) {
	t.Parallel()
	if err := runCLI(context.Background(), nil, &bytes.Buffer{}, &bytes.Buffer{}); err == nil {
		t.Fatal("missing paths accepted")
	}
}

func writeTestFile(t *testing.T, root, relative, contents string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(relative))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
}
