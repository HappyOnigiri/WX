package main

import (
	"strings"
	"testing"
)

func TestRetryCommandReplacesRunAndRestrictsPackages(t *testing.T) {
	command, _, err := retryCommand(config{
		Profile:  "race-daemon-1",
		RepoRoot: t.TempDir(),
		Command:  []string{"go", "test", "-run", "^OldRoot$", "-shuffle=on", "./internal/daemon", "./internal/state"},
	}, "./internal/daemon", []string{"TestFirst", "TestSecond"}, "123", 0)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := strings.Join(command, " "), "go test -json -run=^(TestFirst|TestSecond)$ -shuffle=123 ./internal/daemon"; got != want {
		t.Fatalf("retry command=%q, want %q", got, want)
	}
}

func TestRetryCommandReplacesSeparatedRunFlag(t *testing.T) {
	command, _, err := retryCommand(config{
		Profile:  "race-state-0",
		RepoRoot: t.TempDir(),
		Command:  []string{"go", "test", "-run", "^OldRoot$", "./internal/state"},
	}, "./internal/state", []string{"TestA"}, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(command, " ")
	if !strings.Contains(joined, "-run=^(TestA)$") || strings.Contains(joined, "OldRoot") {
		t.Fatalf("retry command=%q did not replace -run", joined)
	}
}
