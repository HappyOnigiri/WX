package gitx

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestMutationRunEnvInputSanitizesGitEnvironmentAndKeepsStdin(t *testing.T) {
	bin := t.TempDir()
	fakeGit := filepath.Join(bin, "git")
	script := "#!/bin/sh\nIFS= read -r value\nprintf '%s|%s|%s' \"$GIT_DIR\" \"$WX_MUTATION_VALUE\" \"$value\"\n"
	if err := os.WriteFile(fakeGit, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	t.Setenv("GIT_DIR", "/unrelated/.git")
	result, err := (&Runner{}).RunEnvInput(
		context.Background(), t.TempDir(), []string{"WX_MUTATION_VALUE=explicit"}, []byte("from-stdin\n"), "status",
	)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := strings.TrimSpace(result.Stdout), "|explicit|from-stdin"; got != want {
		t.Fatalf("sanitized env and stdin=%q, want %q", got, want)
	}
}

func TestMutationRunnerTimeoutReturnsTheContextDeadline(t *testing.T) {
	bin := t.TempDir()
	fakeGit := filepath.Join(bin, "git")
	if err := os.WriteFile(fakeGit, []byte("#!/bin/sh\nwhile :; do :; done\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	_, err := (&Runner{Timeout: 60 * time.Millisecond}).Run(context.Background(), t.TempDir(), "status")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("timeout error=%v, want context deadline", err)
	}
}

func TestMutationLockRetryBackoffGrowsBeforeNextAttempt(t *testing.T) {
	bin := t.TempDir()
	fakeGit := filepath.Join(bin, "git")
	markerDir := t.TempDir()
	script := `#!/bin/sh
: > "$WX_MUTATION_MARKER_DIR/start-$$"
while [ ! -e "$WX_MUTATION_MARKER_DIR/release-$$" ]; do /bin/sleep 0.001; done
printf 'fatal: could not lock index.lock\n' >&2
exit 1
`
	if err := os.WriteFile(fakeGit, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	t.Setenv("WX_MUTATION_MARKER_DIR", markerDir)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := (&Runner{}).Run(ctx, t.TempDir(), "status")
		done <- err
	}()
	seen := make(map[string]bool)
	first := waitForMutationMarker(t, markerDir, seen)
	releaseMutationMarker(t, markerDir, first)
	second := waitForMutationMarker(t, markerDir, seen)
	releaseMutationMarker(t, markerDir, second)
	thirdStarted := time.Now()
	third := waitForMutationMarker(t, markerDir, seen)
	if elapsed := time.Since(thirdStarted); elapsed < 35*time.Millisecond {
		t.Fatalf("second lock backoff=%s, want the growing delay before the third attempt", elapsed)
	}
	releaseMutationMarker(t, markerDir, third)
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("lock retry after cancellation=%v, want context cancellation", err)
	}
}

func waitForMutationMarker(t *testing.T, directory string, seen map[string]bool) os.DirEntry {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		entries, err := os.ReadDir(directory)
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range entries {
			if strings.HasPrefix(entry.Name(), "start-") && !seen[entry.Name()] {
				seen[entry.Name()] = true
				return entry
			}
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("did not see another lock attempt")
	return nil
}

func releaseMutationMarker(t *testing.T, directory string, marker os.DirEntry) {
	t.Helper()
	pid := strings.TrimPrefix(marker.Name(), "start-")
	if err := os.WriteFile(filepath.Join(directory, "release-"+pid), nil, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestMutationResolveRefChecksRemoteAfterMissingLocalRef(t *testing.T) {
	repo := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q", "-b", "main"},
		{"config", "user.name", "mutation"},
		{"config", "user.email", "mutation@example.invalid"},
	} {
		cmd := newGitxCommand(repo, args...)
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, output)
		}
	}
	if err := os.WriteFile(filepath.Join(repo, "tracked"), []byte("content\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "tracked"}, {"commit", "-qm", "initial"}} {
		cmd := newGitxCommand(repo, args...)
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, output)
		}
	}
	want := gitOutput(t, repo, "rev-parse", "HEAD")
	if output, err := newGitxCommand(repo, "update-ref", "-d", "refs/heads/main").CombinedOutput(); err != nil {
		t.Fatalf("delete local ref: %v: %s", err, output)
	}
	if output, err := newGitxCommand(repo, "update-ref", "refs/remotes/origin/main", want).CombinedOutput(); err != nil {
		t.Fatalf("create remote ref: %v: %s", err, output)
	}
	got, found, err := ResolveRef(context.Background(), &Runner{}, repo, "main")
	if err != nil || !found || got != want {
		t.Fatalf("remote ref=(%q,%v,%v), want (%q,true,nil)", got, found, err, want)
	}
}

func TestMutationResolveRefReportsCancellationAfterBothRefsAreMissing(t *testing.T) {
	bin := t.TempDir()
	fakeGit := filepath.Join(bin, "git")
	if err := os.WriteFile(fakeGit, []byte("#!/bin/sh\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	ctx := &mutationErrAfterContext{Context: context.Background(), cancelAt: 5}
	ref, found, err := ResolveRef(ctx, &Runner{}, t.TempDir(), "missing")
	if !errors.Is(err, context.Canceled) || found || ref != "" {
		t.Fatalf("missing refs after cancellation=(%q,%v,%v), want empty,false,canceled", ref, found, err)
	}
}

func TestMutationWithHeldLockCopiesTheMapWithoutMutatingParentContext(t *testing.T) {
	parent := context.WithValue(context.Background(), heldLocksKey{}, map[string]bool{"first": true})
	child := withHeldLock(parent, "second")
	if holdsLock(parent, "second") || !holdsLock(parent, "first") {
		t.Fatal("withHeldLock changed the parent context")
	}
	if !holdsLock(child, "first") || !holdsLock(child, "second") {
		t.Fatal("child context did not contain both held keys")
	}
	parent.Value(heldLocksKey{}).(map[string]bool)["third"] = true
	if holdsLock(child, "third") {
		t.Fatal("child context shares the parent map")
	}
}

func newGitxCommand(dir string, args ...string) *exec.Cmd {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	return cmd
}

type mutationErrAfterContext struct {
	context.Context
	calls    atomic.Int32
	cancelAt int32
}

func (c *mutationErrAfterContext) Done() <-chan struct{} { return nil }

func (c *mutationErrAfterContext) Err() error {
	if c.calls.Add(1) >= c.cancelAt {
		return context.Canceled
	}
	return nil
}
