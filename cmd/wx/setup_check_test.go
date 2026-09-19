package main

import (
	"context"
	"testing"
)

func TestRunWorktreeSetupCheckRejectsExtraArguments(t *testing.T) {
	if code := runWorktreeSetupCheck(context.Background(), []string{"one", "two"}); code != 2 {
		t.Fatalf("exit=%d, want 2", code)
	}
}
