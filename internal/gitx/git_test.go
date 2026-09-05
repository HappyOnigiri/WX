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
		// race 有効の全 package coverage では負荷の高い CI runner で process 起動が遅れ得る。
		// watchdog は通常の millisecond 経路より十分長くする。
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

func TestRunClassifiesConfirmedNonRepositorySeparatelyFromExecutionFailure(t *testing.T) {
	for _, test := range []struct {
		name, message string
		class         ErrorClass
		notRepo       bool
	}{
		{name: "non repository", message: "fatal: not a git repository", class: ErrorClassNotRepository, notRepo: true},
		{name: "invalid configuration", message: "fatal: cannot read configuration", class: ErrorClassExecution},
	} {
		t.Run(test.name, func(t *testing.T) {
			bin := t.TempDir()
			fakeGit := filepath.Join(bin, "git")
			script := "#!/bin/sh\nprintf '%s\\n' \"" + test.message + "\" >&2\nexit 128\n"
			if err := os.WriteFile(fakeGit, []byte(script), 0o700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", bin)

			_, err := (&Runner{}).Run(context.Background(), t.TempDir(), "rev-parse", "--show-toplevel")
			var gitError *Error
			if !errors.As(err, &gitError) {
				t.Fatalf("error=%v", err)
			}
			if gitError.Class != test.class {
				t.Fatalf("class=%q want %q", gitError.Class, test.class)
			}
			if got := errors.Is(err, ErrNotRepository); got != test.notRepo {
				t.Fatalf("errors.Is(ErrNotRepository)=%v want %v", got, test.notRepo)
			}
		})
	}
}

func TestRunPreservesGitExecutableFailureCause(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	_, err := (&Runner{}).Run(context.Background(), t.TempDir(), "rev-parse", "--show-toplevel")
	var gitError *Error
	if !errors.As(err, &gitError) {
		t.Fatalf("error=%v", err)
	}
	if gitError.Class != ErrorClassExecution {
		t.Fatalf("class=%q", gitError.Class)
	}
	if !errors.Is(err, exec.ErrNotFound) {
		t.Fatalf("error=%v does not preserve executable-not-found cause", err)
	}
}

func TestRunClassifiesTimeoutAsExecutionFailure(t *testing.T) {
	bin := t.TempDir()
	fakeGit := filepath.Join(bin, "git")
	if err := os.WriteFile(fakeGit, []byte("#!/bin/sh\nwhile :; do :; done\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)

	runner := &Runner{Timeout: 100 * time.Millisecond}
	_, err := runner.Run(context.Background(), t.TempDir(), "rev-parse", "--show-toplevel")
	var gitError *Error
	if !errors.As(err, &gitError) {
		t.Fatalf("error=%v", err)
	}
	if gitError.Class != ErrorClassExecution || errors.Is(err, ErrNotRepository) {
		t.Fatalf("class=%q err=%v", gitError.Class, err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("timeout cause was lost: %v", err)
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

// TestSanitizedEnvironCoversEveryLocalGitEnvironmentVariable は denylist を Git の repository-scoped 定義と一致させる。
// Git upgrade の変数追加は、child Git が別 worktree・index・object store を読む穴になる前にこの test で失敗させる。
func TestSanitizedEnvironCoversEveryLocalGitEnvironmentVariable(t *testing.T) {
	// これらの変数を設定する前に実際の Git へ問い合わせ、query 自体が影響を受けないようにする。
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

// TestExplicitEnvOverridesSanitizedDenylistOnBothPaths は archive が明示する一時 GIT_INDEX_FILE を保持する。
// exec.Cmd と fdexec trampoline の両方で、sanitization 後に渡す値が優先されることを確認する。
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
