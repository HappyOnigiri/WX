package gitx

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRunStopsLockConflictRetryWhenContextExpires(t *testing.T) {
	bin := t.TempDir()
	fakeGit := filepath.Join(bin, "git")
	marker := filepath.Join(bin, "invoked")
	if err := os.WriteFile(fakeGit, []byte("#!/bin/sh\n: > \"$WX_GIT_TEST_MARKER\"\nprintf 'fatal: could not lock index.lock\\n' >&2\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	t.Setenv("WX_GIT_TEST_MARKER", marker)
	runner := &Runner{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		// Race-enabled all-package coverage can delay process startup on loaded CI
		// runners, so keep this watchdog well above the normal millisecond path.
		deadline := time.Now().Add(10 * time.Second)
		for {
			if _, err := os.Stat(marker); err == nil {
				time.Sleep(10 * time.Millisecond)
				cancel()
				return
			}
			if time.Now().After(deadline) {
				cancel()
				return
			}
			time.Sleep(time.Millisecond)
		}
	}()
	result, err := runner.Run(ctx, t.TempDir(), "status")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("lock retry error=%v result=%+v", err, result)
	}
	if result.ExitCode != 1 || result.Stderr == "" {
		t.Fatalf("lock retry result=%+v", result)
	}
}

func TestGitErrorRedactsStderrAndWritesOwnerOnlyDetail(t *testing.T) {
	bin := t.TempDir()
	fakeGit := filepath.Join(bin, "git")
	if err := os.WriteFile(fakeGit, []byte("#!/bin/sh\nprintf 'secret-token-from-hook\\n' >&2\nexit 7\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	details := filepath.Join(t.TempDir(), "details")
	runner := &Runner{DetailDir: details}
	_, err := runner.Run(context.Background(), t.TempDir(), "status", "credential-path")
	var gitError *Error
	if !errors.As(err, &gitError) {
		t.Fatalf("error=%v", err)
	}
	if strings.Contains(err.Error(), "secret-token") || strings.Contains(err.Error(), "credential-path") {
		t.Fatalf("public error leaked detail: %v", err)
	}
	detailPath := filepath.Join(details, gitError.FailureID+".log")
	data, err := os.ReadFile(detailPath)
	if err != nil || !strings.Contains(string(data), "secret-token-from-hook") || !strings.Contains(string(data), "credential-path") {
		t.Fatalf("detail=%q err=%v", data, err)
	}
	if info, err := os.Stat(detailPath); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("detail mode=%v err=%v", info, err)
	}
}
