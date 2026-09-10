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
