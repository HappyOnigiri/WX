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
	root := t.TempDir()
	writeTestFile(t, root, "internal/state/state.go", "package state\n")
	writeTestFile(t, root, "internal/daemon/daemon.go", "package daemon\n")
	writeTestFile(t, root, "tools/hookcheck/main.go", "package main\n")
	writeTestFile(t, root, "internal/agent/agent.go", "package agent\n")
	selected, err := selectChecks(root, []changedFile{
		{status: "M", path: "internal/state/state.go"},
		{status: "M", path: "README.md"},
		{status: "M", path: "migrations/004.sql"},
		{status: "M", path: "scripts/test-focus.sh"},
		{status: "M", path: ".github/workflows/nightly.yml"},
		{status: "M", path: "Makefile"},
	})
	if err != nil {
		t.Fatalf("selectChecks: %v", err)
	}
	for _, name := range []string{"fmt-check", "check-fast", "gitexec-check", "docs-check", "migrations-check", "shell-check", "workflow-check", "fuzz-check"} {
		if selected.checks[name] == nil {
			t.Errorf("missing check %s", name)
		}
	}
	if !selected.compile {
		t.Error("Makefile change did not select compile check")
	}
	if selected.tests["./internal/state"] == nil || selected.tests["./tools/testfocus"] == nil {
		t.Fatalf("tests=%#v", selected.tests)
	}
	if !selected.tests["./tools/testfocus"].countOne {
		t.Error("test-focus validation test should disable result cache")
	}
	if len(selected.sortedTests()) != 2 {
		t.Fatalf("sortedTests=%#v, want regular and count-one groups", selected.sortedTests())
	}
}

func TestSelectChecksSkipsDeletedPackage(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	selected, err := selectChecks(root, []changedFile{{status: "D", path: "internal/gone/gone.go"}})
	if err != nil {
		t.Fatalf("selectChecks: %v", err)
	}
	if len(selected.tests) != 0 {
		t.Fatalf("tests=%#v, want no tests for deleted package", selected.tests)
	}
	if !strings.Contains(strings.Join(selected.skipped, "\n"), "package was deleted") {
		t.Fatalf("skipped=%v", selected.skipped)
	}
}

func TestSelectChecksTreatsRenameAsDeleteAndAdd(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeTestFile(t, root, "internal/newpkg/new.go", "package newpkg\n")
	selected, err := selectChecks(root, []changedFile{
		{status: "D", path: "internal/oldpkg/old.go"},
		{status: "A", path: "internal/newpkg/new.go"},
	})
	if err != nil {
		t.Fatalf("selectChecks: %v", err)
	}
	if selected.tests["./internal/newpkg"] == nil || len(selected.tests) != 1 {
		t.Fatalf("tests=%#v", selected.tests)
	}
	if !strings.Contains(strings.Join(selected.skipped, "\n"), "package was deleted") {
		t.Fatalf("skipped=%v", selected.skipped)
	}
}

func TestSelectChecksFindsTestdataOwner(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeTestFile(t, root, "internal/agent/agent.go", "package agent\n")
	selected, err := selectChecks(root, []changedFile{{status: "M", path: "internal/agent/testdata/input.txt"}})
	if err != nil {
		t.Fatalf("selectChecks: %v", err)
	}
	if selected.tests["./internal/agent"] == nil {
		t.Fatalf("tests=%#v", selected.tests)
	}
}

func TestSelectChecksReportsUnknownFiles(t *testing.T) {
	t.Parallel()
	selected, err := selectChecks(t.TempDir(), []changedFile{{status: "M", path: "LICENSE"}})
	if err != nil {
		t.Fatalf("selectChecks: %v", err)
	}
	if !selected.empty() || len(selected.skipped) != 1 {
		t.Fatalf("selection=%#v", selected)
	}
	var output bytes.Buffer
	printPlan(&output, selected)
	if !strings.Contains(output.String(), "no matching check") {
		t.Fatalf("plan=%q", output.String())
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
	root := t.TempDir()
	writeTestFile(t, root, "internal/state/state.go", "package state\n")
	selected, err := selectChecks(root, []changedFile{{status: "M", path: "internal/state/state.go"}})
	if err != nil {
		t.Fatalf("selectChecks: %v", err)
	}
	selected.note = "note"
	var output bytes.Buffer
	printPlan(&output, selected)
	text := output.String()
	if !strings.Contains(text, "command: make fmt-check") || !strings.Contains(text, "go test -short ./internal/state") {
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
