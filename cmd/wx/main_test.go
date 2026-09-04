package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/HappyOnigiri/WX/internal/config"
	"github.com/HappyOnigiri/WX/internal/rpc"
)

func TestRunConfigRejectsUnnormalizablePathBeforeSave(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	loop := filepath.Join(home, "loop")
	if err := os.Symlink(loop, loop); err != nil {
		t.Fatal(err)
	}

	if code := runConfig(context.Background(), []string{"storage.worktree_root", loop}); code != 1 {
		t.Fatalf("runConfig exit=%d want=1", code)
	}
	path, err := config.Path()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("invalid config was persisted: %v", err)
	}
}

type multiKeyStatusHandler struct{}

func (multiKeyStatusHandler) Handle(_ context.Context, method string, _ json.RawMessage) (any, error) {
	return map[string]any{"zeta": 1, "alpha": 2, "mu": 3}, nil
}

func TestRunRPCDisplaySortsHumanReadableOutputByKey(t *testing.T) {
	// A short /tmp-rooted HOME keeps the Unix socket path under sun_path's
	// length limit; t.TempDir()'s deeper path does not (see the same pattern
	// in help_test.go's TestCommandDispatchAgainstRPCBoundary).
	home, err := os.MkdirTemp("/tmp", "wx-status-sort-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(home) })
	t.Setenv("HOME", home)
	socket, err := config.SocketPath()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	server := &rpc.Server{Socket: socket, Handler: multiKeyStatusHandler{}}
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx) }()
	for deadline := time.Now().Add(time.Second); ; {
		if _, err := os.Lstat(socket); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("RPC server did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	stdout := captureStdout(t, func() {
		if code := runRPCDisplay(ctx, "Status", nil); code != 0 {
			t.Fatalf("runRPCDisplay exit=%d", code)
		}
	})
	wantOrder := []string{"alpha", "mu", "zeta"}
	lastIndex := -1
	for _, key := range wantOrder {
		index := strings.Index(stdout, key)
		if index < 0 {
			t.Fatalf("output missing key %q: %q", key, stdout)
		}
		if index < lastIndex {
			t.Fatalf("keys were not printed in sorted order: %q", stdout)
		}
		lastIndex = index
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestRunDoctorFallsBackToLocalChecksWhenDaemonCannotConnect(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	stdout := captureStdout(t, func() {
		if code := runDoctor(context.Background(), []string{"--json"}); code != 1 {
			t.Fatalf("runDoctor exit=%d, want 1 when daemon is unavailable", code)
		}
	})
	var payload map[string]any
	if err := json.Unmarshal([]byte(stdout), &payload); err != nil {
		t.Fatalf("local doctor output is not JSON: %v\n%s", err, stdout)
	}
	if got := payload["schema_version"]; got != float64(5) {
		t.Fatalf("schema_version=%v, want 5", got)
	}
	checks, ok := payload["checks"].(map[string]any)
	if !ok {
		t.Fatalf("checks has wrong type: %T", payload["checks"])
	}
	for _, key := range []string{"config", "git", "socket", "state_database", "launch_agent", "worktree_root", "hooks", "sqlite", "worktree_registration", "artifact_ownership", "daemon"} {
		if _, ok := checks[key]; !ok {
			t.Fatalf("local doctor checks missing %q: %v", key, checks)
		}
	}
	if got := checks["sqlite"]; got == "ok" {
		t.Fatal("local doctor reported sqlite as available without a daemon")
	}
	registration, ok := checks["worktree_registration"].(map[string]any)
	if !ok || registration["checked"] != float64(0) || registration["error"] == nil {
		t.Fatalf("local registration placeholder=%v", checks["worktree_registration"])
	}
	artifacts, ok := checks["artifact_ownership"].(map[string]any)
	if !ok {
		t.Fatalf("local artifact placeholder=%v", checks["artifact_ownership"])
	}
	if errors, ok := artifacts["errors"].([]any); !ok || len(errors) == 0 {
		t.Fatalf("local artifact errors=%v", artifacts["errors"])
	}
	if daemon, ok := checks["daemon"].(string); !ok || !strings.Contains(daemon, "connect") {
		t.Fatalf("local daemon reason=%v", checks["daemon"])
	}
}

// captureStdout redirects os.Stdout for the duration of fn and returns what
// was written. Tests in this package do not run in parallel, so the process-
// wide swap is safe.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	original := os.Stdout
	os.Stdout = write
	// A deferred restore (rather than a plain assignment after fn()) keeps
	// os.Stdout from staying redirected to a now-closed pipe for the rest of
	// the test binary if fn calls t.Fatal/t.Fatalf: those call
	// runtime.Goexit, which unwinds through defers but skips any code after
	// fn() that isn't one.
	defer func() { os.Stdout = original }()
	fn()
	if err := write.Close(); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(read); err != nil {
		t.Fatal(err)
	}
	if err := read.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.String()
}

func TestRunHookHelpPrintsUsageAndExitsTwo(t *testing.T) {
	for _, args := range [][]string{{"--help"}, {"-h"}} {
		stderr := captureStderr(t, func() {
			if code := runHook(context.Background(), args); code != 2 {
				t.Fatalf("runHook(%v) exit=%d want=2", args, code)
			}
		})
		if !strings.Contains(stderr, "Usage: wx hook") {
			t.Fatalf("runHook(%v) stderr=%q missing usage", args, stderr)
		}
	}
}

func TestRunHookRejectsUnknownFlag(t *testing.T) {
	captureStderr(t, func() {
		if code := runHook(context.Background(), []string{"--not-a-flag", "session-start"}); code != 2 {
			t.Fatalf("runHook exit=%d want=2", code)
		}
	})
}

// TestEverySubcommandHasAUniformPflagContract verifies, for all 9 wx
// subcommands, that each owns a pflag.NewFlagSet(..., pflag.ContinueOnError)
// rather than a hand-rolled len(args) check: --help/-h prints usage and
// exits with that command's existing contract (stdout+0 for every command
// except hook, which is invoked by agent hook configuration rather than
// typed interactively and so keeps its misuse-style stderr+2), and an
// unrecognized flag is rejected with usage on stderr and a non-zero exit
// rather than being silently accepted or misparsed as a positional
// argument. None of these paths reach the daemon or touch the filesystem:
// pflag rejects the input before any command-specific logic runs.
func TestEverySubcommandHasAUniformPflagContract(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	type contract struct {
		name         string
		run          func(ctx context.Context, args []string) int
		helpExit     int
		helpOnStdout bool
	}
	contracts := []contract{
		{name: "status", run: func(ctx context.Context, args []string) int { return runRPCDisplay(ctx, "Status", args) }, helpExit: 0, helpOnStdout: true},
		{name: "doctor", run: func(ctx context.Context, args []string) int { return runRPCDisplay(ctx, "Doctor", args) }, helpExit: 0, helpOnStdout: true},
		{name: "gc", run: runGC, helpExit: 0, helpOnStdout: true},
		{name: "sessions", run: runSessions, helpExit: 0, helpOnStdout: true},
		{name: "config", run: runConfig, helpExit: 0, helpOnStdout: true},
		{name: "resume", run: runResume, helpExit: 0, helpOnStdout: true},
		{name: "daemon", run: runDaemon, helpExit: 0, helpOnStdout: true},
		{name: "forget", run: runForget, helpExit: 0, helpOnStdout: true},
		{name: "hook", run: runHook, helpExit: 2, helpOnStdout: false},
	}
	for _, c := range contracts {
		t.Run(c.name, func(t *testing.T) {
			for _, helpFlag := range []string{"--help", "-h"} {
				var code int
				var output string
				if c.helpOnStdout {
					output = captureStdout(t, func() { code = c.run(context.Background(), []string{helpFlag}) })
				} else {
					output = captureStderr(t, func() { code = c.run(context.Background(), []string{helpFlag}) })
				}
				if code != c.helpExit {
					t.Fatalf("%s %s exit=%d want=%d", c.name, helpFlag, code, c.helpExit)
				}
				if want := "Usage: wx " + c.name; !strings.Contains(output, want) {
					t.Fatalf("%s %s output=%q missing %q", c.name, helpFlag, output, want)
				}
			}
			var code int
			stderr := captureStderr(t, func() {
				code = c.run(context.Background(), []string{"--this-flag-does-not-exist"})
			})
			if code == 0 {
				t.Fatalf("%s accepted an unrecognized flag", c.name)
			}
			if want := "Usage: wx " + c.name; !strings.Contains(stderr, want) {
				t.Fatalf("%s unrecognized-flag stderr=%q missing %q", c.name, stderr, want)
			}
			// The usage block alone does not say which argument was rejected,
			// and pflag writes nothing itself under ContinueOnError, so the
			// diagnostic has to name the flag or the user has no way to tell a
			// typo from an unsupported option.
			if !strings.Contains(stderr, "--this-flag-does-not-exist") {
				t.Fatalf("%s unrecognized-flag stderr=%q does not name the rejected flag", c.name, stderr)
			}
		})
	}
}

// captureStderr redirects os.Stderr for the duration of fn and returns what
// was written. Tests in this package do not run in parallel, so the process-
// wide swap is safe.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	original := os.Stderr
	os.Stderr = write
	defer func() { os.Stderr = original }()
	fn()
	if err := write.Close(); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(read); err != nil {
		t.Fatal(err)
	}
	if err := read.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.String()
}
