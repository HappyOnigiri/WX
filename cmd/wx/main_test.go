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
	"github.com/HappyOnigiri/WX/internal/daemon"
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
	// /tmp 配下の短い HOME で Unix socket path を sun_path の長さ上限内に収める。
	// t.TempDir() の深い path では収まらない。
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
	if got := payload["schema_version"]; got != float64(6) {
		t.Fatalf("schema_version=%v, want 6", got)
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

func TestRunGCReturnsNonZeroForPendingReport(t *testing.T) {
	home, err := os.MkdirTemp("/tmp", "wx-gc-result-")
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
	server := &rpc.Server{
		Socket: socket,
		Handler: commandHandler{gcResult: &daemon.GCResult{
			Candidates: 2,
			Pending:    1,
			Reasons:    []daemon.GCReason{{Target: "worktree slot", Status: "pending", Reason: "ownership could not be proven"}},
		}},
	}
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx) }()
	for deadline := time.Now().Add(time.Second); ; {
		if _, err := os.Lstat(socket); err == nil {
			break
		}
		select {
		case serveErr := <-done:
			t.Fatalf("RPC server failed at %s: %v", socket, serveErr)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("RPC server did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	var code int
	stdout := captureStdout(t, func() {
		stderr := captureStderr(t, func() { code = runGC(ctx, nil) })
		if !strings.Contains(stderr, "ownership could not be proven") {
			t.Fatalf("GC reason missing from stderr: %q", stderr)
		}
	})
	if code != 1 {
		t.Fatalf("runGC exit=%d, want 1 for pending report", code)
	}
	for _, field := range []string{"candidates: 2", "scheduled: 0", "pending: 1", "failed: 0"} {
		if !strings.Contains(stdout, field) {
			t.Fatalf("GC output=%q missing %q", stdout, field)
		}
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// captureStdout は fn 中の os.Stdout を差し替え、書込み内容を返す。
// この package の test は並列実行しないため、process 全体の差し替えでも安全である。
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	original := os.Stdout
	os.Stdout = write
	// t.Fatal/t.Fatalf は runtime.Goexit で後続処理を飛ばすため、defer で os.Stdout を復元する。
	// 閉じた pipe へのリダイレクトを test binary の残りへ残さない。
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

// TestEverySubcommandHasAUniformPflagContract は全 subcommand の --help と未知 flag の終了コード・出力先を確認する。
// 入力は command 固有の処理より前に pflag が拒否するため、daemon や filesystem には触れない。
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
			// ContinueOnError の pflag はエラーを書かず、usage だけでは拒否した引数が分からない。
			// 診断には typo と未対応 option を区別できる flag 名を含める。
			if !strings.Contains(stderr, "--this-flag-does-not-exist") {
				t.Fatalf("%s unrecognized-flag stderr=%q does not name the rejected flag", c.name, stderr)
			}
		})
	}
}

// captureStderr は fn 中の os.Stderr を差し替え、書込み内容を返す。
// この package の test は並列実行しないため、process 全体の差し替えでも安全である。
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

func TestWorktreeFlagsAndPassThrough(t *testing.T) {
	for _, flag := range []string{"--worktree", "--no-worktree", "--select-worktree", "-w", "-n", "-s"} {
		f, name, args, err := parseAgentPrefix([]string{flag, "claude", "--worktree", "two words"})
		if err != nil || name != "claude" || len(args) != 2 || args[0] != "--worktree" || args[1] != "two words" {
			t.Fatalf("flag=%s name=%s args=%v err=%v", flag, name, args, err)
		}
		if !(f.worktree || f.noWorktree || f.selectWorktree) {
			t.Fatal("flag was not retained")
		}
	}
	for _, args := range [][]string{{"--worktree", "--no-worktree", "codex"}, {"--select-worktree", "--worktree", "codex"}, {"--no-worktree", "--select-worktree", "codex"}, {"-w", "-n", "codex"}} {
		if _, _, _, err := parseAgentPrefix(args); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
	f, name, args, err := parseAgentPrefix([]string{"-s"})
	if err != nil || !f.selectWorktree || name != "" || len(args) != 0 {
		t.Fatalf("select-only invocation: flags=%+v name=%q args=%v err=%v", f, name, args, err)
	}
}
