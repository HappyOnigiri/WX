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

func TestGitErrorSurvivesUnavailableDetailDirectory(t *testing.T) {
	bin := t.TempDir()
	fakeGit := filepath.Join(bin, "git")
	if err := os.WriteFile(fakeGit, []byte("#!/bin/sh\nprintf 'failure\n' >&2\nexit 7\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	detailPath := filepath.Join(t.TempDir(), "detail-blocker")
	if err := os.WriteFile(detailPath, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	if _, err := (&Runner{DetailDir: detailPath}).Run(context.Background(), t.TempDir(), "status"); err == nil {
		t.Fatal("Git failure was hidden when detail directory was unavailable")
	}
}

func TestErrorFormattingUsesFallbackAndFirstArgument(t *testing.T) {
	tests := []struct {
		name string
		err  *Error
		want string
	}{
		{
			name: "fallback",
			err:  &Error{Result: Result{ExitCode: -1}, FailureID: "fallback-id"},
			want: "git command failed with exit -1 (failure fallback-id)",
		},
		{
			name: "first argument",
			err:  &Error{Args: []string{"status", "--short"}, Result: Result{ExitCode: 7}, FailureID: "status-id"},
			want: "git status failed with exit 7 (failure status-id)",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := test.err.Error(); got != test.want {
				t.Fatalf("Error()=%q want=%q", got, test.want)
			}
		})
	}
}

func TestRunEnvInputPassesEnvironmentAndStdin(t *testing.T) {
	bin := t.TempDir()
	fakeGit := filepath.Join(bin, "git")
	if err := os.WriteFile(fakeGit, []byte("#!/bin/sh\nIFS= read -r value\nprintf '%s|%s' \"$WX_GIT_TEST_VALUE\" \"$value\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	runner := &Runner{}
	result, err := runner.RunEnvInput(
		context.Background(),
		t.TempDir(),
		[]string{"WX_GIT_TEST_VALUE=from-env"},
		[]byte("from-stdin\n"),
		"status",
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.Stdout != "from-env|from-stdin" || result.Stderr != "" || result.ExitCode != 0 {
		t.Fatalf("result=%+v", result)
	}
}

func TestRunRetriesLockConflictExactlyFourTimes(t *testing.T) {
	bin := t.TempDir()
	fakeGit := filepath.Join(bin, "git")
	counter := filepath.Join(bin, "counter")
	script := `#!/bin/sh
count=0
if [ -f "$WX_GIT_TEST_COUNTER" ]; then
  IFS= read -r count < "$WX_GIT_TEST_COUNTER"
fi
count=$((count + 1))
printf '%s\n' "$count" > "$WX_GIT_TEST_COUNTER"
printf 'fatal: could not lock index.lock\n' >&2
exit 1
`
	if err := os.WriteFile(fakeGit, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	t.Setenv("WX_GIT_TEST_COUNTER", counter)
	runner := &Runner{}
	result, err := runner.Run(context.Background(), t.TempDir(), "status")
	var gitError *Error
	if !errors.As(err, &gitError) {
		t.Fatalf("error=%v", err)
	}
	if result.ExitCode != 1 || !strings.Contains(result.Stderr, "could not lock") {
		t.Fatalf("result=%+v", result)
	}
	data, err := os.ReadFile(counter)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(data)); got != "4" {
		t.Fatalf("attempts=%s want=4", got)
	}
}
