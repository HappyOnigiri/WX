package main

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestValidateQueueRequiresMax(t *testing.T) {
	path := filepath.Join(t.TempDir(), "workflow.yml")
	write := func(value string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(value), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("name: test\nconcurrency:\n  group: x\n  queue: max\n")
	if err := validateQueue(path); err != nil {
		t.Fatal(err)
	}
	write("name: test\nconcurrency:\n  group: x\n  queue: one\n")
	if err := validateQueue(path); err == nil {
		t.Fatal("invalid queue accepted")
	}
}

func TestAllowedQueueDiagnosticOnlyAtPinnedLine(t *testing.T) {
	line := queueLine(queueWorkflow)
	if line < 1 {
		t.Fatal("queue line was not found")
	}
	if !allowedQueueDiagnostic(".github/workflows/report-flaky-tests.yml:" + strconv.Itoa(line) + ":3: unexpected key \"queue\"") {
		t.Fatal("pinned queue diagnostic was not accepted")
	}
	if allowedQueueDiagnostic(".github/workflows/report-flaky-tests.yml:" + strconv.Itoa(line+1) + ":3: unexpected key \"queue\"") {
		t.Fatal("wrong-line queue diagnostic was accepted")
	}
}

func TestRunActionlintFiltersOnlyQueueDiagnostic(t *testing.T) {
	path := filepath.Join(t.TempDir(), "actionlint")
	line := queueLine(queueWorkflow)
	if line < 1 {
		t.Fatal("queue line was not found")
	}
	script := "#!/bin/sh\necho '.github/workflows/report-flaky-tests.yml:" + strconv.Itoa(line) + ":3: unexpected key \"queue\"'\nexit 1\n"
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	var output strings.Builder
	if err := runActionlint(context.Background(), path, &output); err != nil {
		t.Fatal(err)
	}
}

// workflow_runのworkflows:に並ぶ名前は、GitHubが一致しないものを黙って無視する。
func TestValidateTriggerNamesRequiresAnExistingWorkflowName(t *testing.T) {
	dir := t.TempDir()
	write := func(name, value string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(value), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("ci.yml", "name: CI\non:\n  push:\n")
	write("hunt.yml", "name: Flake Hunt\non:\n  workflow_dispatch:\n")
	write("notes.txt", "not a workflow")
	reporter := filepath.Join(dir, "report.yml")
	write("report.yml", "name: Report\non:\n  workflow_run:\n    workflows: [CI, Flake Hunt]\n    types: [completed]\n")
	if err := validateTriggerNames(dir, reporter); err != nil {
		t.Fatal(err)
	}
	write("report.yml", "name: Report\non:\n  workflow_run:\n    workflows: [CI, Flake Hunting]\n    types: [completed]\n")
	err := validateTriggerNames(dir, reporter)
	if err == nil || !strings.Contains(err.Error(), "Flake Hunting") {
		t.Fatalf("error=%v", err)
	}
	// workflow_runを持たないworkflowは検査の対象にならない。
	write("report.yml", "name: Report\non:\n  push:\n")
	if err := validateTriggerNames(dir, reporter); err != nil {
		t.Fatal(err)
	}
}

// このリポジトリの実際のworkflowが、名前の一致という条件を満たしていることを確かめる。
func TestRepositoryWorkflowTriggersResolve(t *testing.T) {
	root := repositoryRoot(t)
	dir := filepath.Join(root, ".github", "workflows")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".yml" {
			continue
		}
		if err := validateTriggerNames(dir, filepath.Join(dir, entry.Name())); err != nil {
			t.Fatal(err)
		}
	}
}

func repositoryRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("repository root was not found")
		}
		dir = parent
	}
}
