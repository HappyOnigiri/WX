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
	fn()
	os.Stdout = original
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
	fn()
	os.Stderr = original
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
