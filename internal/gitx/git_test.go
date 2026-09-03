package gitx

import (
	"context"
	"errors"
	"os"
	"os/exec"
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

func TestRunStripsInheritedRepositoryScopedGitEnvironment(t *testing.T) {
	bin := t.TempDir()
	fakeGit := filepath.Join(bin, "git")
	if err := os.WriteFile(fakeGit, []byte("#!/bin/sh\nprintf 'dir=%s work=%s index=%s obj=%s key=%s\\n' \"$GIT_DIR\" \"$GIT_WORK_TREE\" \"$GIT_INDEX_FILE\" \"$GIT_OBJECT_DIRECTORY\" \"$GIT_CONFIG_KEY_0\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	t.Setenv("GIT_DIR", "/unrelated/.git")
	t.Setenv("GIT_WORK_TREE", "/unrelated/worktree")
	t.Setenv("GIT_INDEX_FILE", "/unrelated/index")
	t.Setenv("GIT_OBJECT_DIRECTORY", "/unrelated/objects")
	t.Setenv("GIT_CONFIG_KEY_0", "unrelated.key")
	runner := &Runner{}
	result, err := runner.Run(context.Background(), t.TempDir(), "status")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(result.Stdout) != "dir= work= index= obj= key=" {
		t.Fatalf("inherited Git-scoped environment leaked into child: %q", result.Stdout)
	}
}

func TestRunAtStripsInheritedRepositoryScopedGitEnvironment(t *testing.T) {
	bin := t.TempDir()
	fakeGit := filepath.Join(bin, "git")
	if err := os.WriteFile(fakeGit, []byte("#!/bin/sh\nprintf 'dir=%s index=%s\\n' \"$GIT_DIR\" \"$GIT_INDEX_FILE\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	t.Setenv("GIT_DIR", "/unrelated/.git")
	t.Setenv("GIT_INDEX_FILE", "/unrelated/index")
	dirFile, err := os.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer dirFile.Close()
	wxExe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	runner := &Runner{FDHelper: wxExe}
	result, err := runner.RunAt(context.Background(), dirFile, nil, nil, "status")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(result.Stdout) != "dir= index=" {
		t.Fatalf("inherited Git-scoped environment leaked into descriptor-bound child: %q", result.Stdout)
	}
}

// TestSanitizedEnvironCoversEveryLocalGitEnvironmentVariable pins the
// denylist to Git's own definition of "repository-scoped" instead of to a
// hand-maintained list. scripts/hook-check.sh unsets exactly
// `git rev-parse --local-env-vars` for the same reason, so any name Git
// reports and gitEnvDenylist omits is a hole through which an inherited
// variable reaches a child Git and can silently change which worktree,
// index, object store, or config a snapshot reads. A Git upgrade that adds a
// name fails here rather than reopening that hole unnoticed.
func TestSanitizedEnvironCoversEveryLocalGitEnvironmentVariable(t *testing.T) {
	// Ask the real Git before any of these variables are set, so the query
	// itself cannot be perturbed by them.
	output, err := exec.Command("git", "rev-parse", "--local-env-vars").Output()
	if err != nil {
		t.Fatalf("git rev-parse --local-env-vars: %v", err)
	}
	names := strings.Fields(string(output))
	if len(names) == 0 {
		t.Fatal("git reported no local environment variables")
	}
	for _, name := range names {
		t.Setenv(name, "/unrelated/"+name)
	}
	sanitized := sanitizedEnviron()
	for _, name := range names {
		for _, kv := range sanitized {
			if got, _, _ := strings.Cut(kv, "="); got == name {
				t.Errorf("%s is repository-scoped per git rev-parse --local-env-vars but survived sanitization", name)
			}
		}
	}
}

// TestExplicitEnvOverridesSanitizedDenylistOnBothPaths guards the property
// internal/archive depends on: archive passes its temporary GIT_INDEX_FILE
// explicitly, and sanitizing the inherited one must not defeat that.
// exec.Cmd de-duplicates Env keeping the last occurrence of a key, and the
// descriptor-bound path re-execs through a trampoline, so assert the
// override survives on both paths rather than assuming last-wins carries
// through fdexec.
func TestExplicitEnvOverridesSanitizedDenylistOnBothPaths(t *testing.T) {
	bin := t.TempDir()
	fakeGit := filepath.Join(bin, "git")
	if err := os.WriteFile(fakeGit, []byte("#!/bin/sh\nprintf 'index=%s\\n' \"$GIT_INDEX_FILE\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	wxExe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	t.Setenv("GIT_INDEX_FILE", "/unrelated/index")
	const want = "index=/wx/owned/index"
	env := []string{"GIT_INDEX_FILE=/wx/owned/index"}
	runner := &Runner{FDHelper: wxExe}

	result, err := runner.RunEnv(context.Background(), t.TempDir(), env, "write-tree")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(result.Stdout) != want {
		t.Fatalf("explicit GIT_INDEX_FILE lost on the path-based command: %q want %q", result.Stdout, want)
	}

	dirFile, err := os.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = dirFile.Close() }()
	result, err = runner.RunAt(context.Background(), dirFile, env, nil, "write-tree")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(result.Stdout) != want {
		t.Fatalf("explicit GIT_INDEX_FILE lost on the descriptor-bound command: %q want %q", result.Stdout, want)
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

func TestIsDetachedDistinguishesBranchAndDetachedHeads(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "tracked"), []byte("base\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitTestCommand(t, root, "init", "-b", "main")
	gitTestCommand(t, root, "config", "user.name", "test")
	gitTestCommand(t, root, "config", "user.email", "test@example.com")
	gitTestCommand(t, root, "add", "tracked")
	gitTestCommand(t, root, "commit", "-m", "initial")
	runner := &Runner{Timeout: time.Second}
	if IsDetached(context.Background(), runner, root) {
		t.Fatal("branch checkout was reported as detached")
	}
	head := gitTestOutput(t, root, "rev-parse", "HEAD")
	gitTestCommand(t, root, "checkout", "--detach", head)
	if !IsDetached(context.Background(), runner, root) {
		t.Fatal("detached checkout was reported as a branch")
	}
}

func gitTestCommand(t *testing.T, directory string, args ...string) {
	t.Helper()
	command := exec.Command("git", args...)
	command.Dir = directory
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, output)
	}
}

func gitTestOutput(t *testing.T, directory string, args ...string) string {
	t.Helper()
	command := exec.Command("git", args...)
	command.Dir = directory
	output, err := command.Output()
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(output))
}
