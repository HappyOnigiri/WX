package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/HappyOnigiri/WX/internal/config"
	"github.com/HappyOnigiri/WX/internal/daemon"
	"github.com/HappyOnigiri/WX/internal/rpc"
)

type commandHandler struct{ workspace string }

func (h commandHandler) Handle(_ context.Context, method string, _ json.RawMessage) (any, error) {
	switch method {
	case "Sessions":
		return []map[string]any{{"id": "session", "state": "ACTIVE", "agent": "codex"}}, nil
	case "GC":
		return map[string]int{"candidates": 0}, nil
	case "ResolveAndLease", "Resume":
		return daemon.Lease{SessionID: "session", Token: "token", Path: h.workspace, SourceWorkspace: h.workspace, Ready: true}, nil
	case "ResumeStatus":
		return map[string]bool{"expired": false}, nil
	default:
		return map[string]any{"ok": true}, nil
	}
}

// runCapturingStderr runs one wx invocation with os.Stderr redirected, so a
// test can assert on the guidance a failure printed as well as its exit code.
func runCapturingStderr(t *testing.T, args []string) (int, string) {
	t.Helper()
	exit := 0
	stderr := captureStderr(t, func() { exit = run(context.Background(), args) })
	return exit, stderr
}

// TestDaemonStartAndRestartTellAnOperatorToInstallTheLaunchAgent covers the
// branch the other daemon lifecycle tests miss: the one that answers with the
// stub launchctl's failure, the others cancel their context so launchctl never
// runs and the output stays empty.
func TestDaemonStartAndRestartTellAnOperatorToInstallTheLaunchAgent(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	bin := filepath.Join(home, "bin")
	if err := os.Mkdir(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "launchctl"), []byte("#!/bin/sh\necho 'Could not find service' >&2\nexit 3\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	// No daemon is listening under this HOME, so RequestRestart fails to
	// connect and the command falls back to launchctl.
	for _, action := range []string{"restart", "start"} {
		exit, stderr := runCapturingStderr(t, []string{"daemon", action})
		if exit != 1 {
			t.Fatalf("daemon %s exit=%d, want 1", action, exit)
		}
		if !strings.Contains(stderr, "run wx daemon install to register the LaunchAgent first") {
			t.Fatalf("daemon %s stderr does not carry the install guidance:\n%s", action, stderr)
		}
	}
}

// TestDaemonStopReportsAnAlreadyStoppedDaemon is the idempotent half of the
// stop contract: nothing is listening, so there is nothing to wait for and the
// wanted state already holds.
func TestDaemonStopReportsAnAlreadyStoppedDaemon(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if exit := run(context.Background(), []string{"daemon", "stop"}); exit != 0 {
		t.Fatalf("daemon stop against a stopped daemon exit=%d, want 0", exit)
	}
}

// lifecycleHandler answers a lifecycle RPC with a gate snapshot and then tells
// the test to take the server down, which is what the real daemon does once
// its idle gate opens.
type lifecycleHandler struct {
	pid     int
	stopped chan struct{}
	once    sync.Once
}

func (h *lifecycleHandler) Handle(_ context.Context, method string, _ json.RawMessage) (any, error) {
	switch method {
	case "RequestStop", "RequestRestart":
		// The signal is deferred so this response still reaches the client:
		// a real daemon closes its listener after answering, not during.
		h.once.Do(func() { time.AfterFunc(100*time.Millisecond, func() { close(h.stopped) }) })
		return map[string]any{"pid": h.pid, "inflight_requests": 0, "queued_jobs": 0, "quiet_period_remaining_ms": 0}, nil
	case "Status":
		return map[string]any{"pid": h.pid}, nil
	default:
		return map[string]any{"ok": true}, nil
	}
}

func serveUntilCanceled(t *testing.T, socket string, handler rpc.Handler) (context.CancelFunc, <-chan error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	server := &rpc.Server{Socket: socket, Handler: handler}
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx) }()
	for deadline := time.Now().Add(2 * time.Second); ; {
		if daemonListening(context.Background(), socket) {
			break
		}
		if time.Now().After(deadline) {
			cancel()
			t.Fatal("RPC server did not start")
		}
		time.Sleep(5 * time.Millisecond)
	}
	return cancel, done
}

// TestDaemonStopAndRestartWaitForTheDaemonToReachTheRequestedState is the
// point of making these commands synchronous: neither may report success while
// the daemon it addressed is still the one answering the socket.
func TestDaemonStopAndRestartWaitForTheDaemonToReachTheRequestedState(t *testing.T) {
	home, err := os.MkdirTemp("/tmp", "wxl-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(home) })
	t.Setenv("HOME", home)
	// A short budget keeps a regression from stalling the suite for a minute.
	restore := daemonWaitTimeout
	daemonWaitTimeout = 5 * time.Second
	t.Cleanup(func() { daemonWaitTimeout = restore })
	socket, err := config.SocketPath()
	if err != nil {
		t.Fatal(err)
	}

	stopHandler := &lifecycleHandler{pid: 4242, stopped: make(chan struct{})}
	cancelStop, stopDone := serveUntilCanceled(t, socket, stopHandler)
	stopExit := make(chan int, 1)
	go func() { stopExit <- run(context.Background(), []string{"daemon", "stop"}) }()
	<-stopHandler.stopped
	cancelStop()
	if err := <-stopDone; err != nil {
		t.Fatal(err)
	}
	if exit := <-stopExit; exit != 0 {
		t.Fatalf("daemon stop exit=%d, want 0", exit)
	}
	if daemonListening(context.Background(), socket) {
		t.Fatal("daemon stop reported success while the socket was still answering")
	}

	// The restart is the same wait plus the pid comparison: the replacement has
	// to be a different process, not merely a socket that came back.
	restartHandler := &lifecycleHandler{pid: 4242, stopped: make(chan struct{})}
	cancelRestart, restartDone := serveUntilCanceled(t, socket, restartHandler)
	restartExit := make(chan int, 1)
	go func() { restartExit <- run(context.Background(), []string{"daemon", "restart"}) }()
	<-restartHandler.stopped
	cancelRestart()
	if err := <-restartDone; err != nil {
		t.Fatal(err)
	}
	replacementCancel, replacementDone := serveUntilCanceled(t, socket, &lifecycleHandler{pid: 9999, stopped: make(chan struct{})})
	exit := <-restartExit
	replacementCancel()
	if err := <-replacementDone; err != nil {
		t.Fatal(err)
	}
	if exit != 0 {
		t.Fatalf("daemon restart exit=%d, want 0", exit)
	}
}

// TestDaemonLifecycleTimeoutsNameWhatTheGateWasWaitingFor keeps the failure
// path actionable: a daemon that never goes away must produce the gate reason
// the request came back with, not just a bare timeout.
func TestDaemonLifecycleTimeoutsNameWhatTheGateWasWaitingFor(t *testing.T) {
	home, err := os.MkdirTemp("/tmp", "wxl-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(home) })
	t.Setenv("HOME", home)
	restore := daemonWaitTimeout
	daemonWaitTimeout = 200 * time.Millisecond
	t.Cleanup(func() { daemonWaitTimeout = restore })
	socket, err := config.SocketPath()
	if err != nil {
		t.Fatal(err)
	}
	// This server answers the request and then keeps serving, which is what a
	// daemon whose gate never opens looks like from outside.
	cancel, done := serveUntilCanceled(t, socket, busyHandler{})
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}()
	for _, test := range []struct{ action, want string }{
		{action: "stop", want: "2 job(s) were still queued"},
		{action: "restart", want: "2 job(s) were still queued"},
	} {
		exit, stderr := runCapturingStderr(t, []string{"daemon", test.action})
		if exit != 1 {
			t.Fatalf("daemon %s exit=%d, want 1", test.action, exit)
		}
		if !strings.Contains(stderr, test.want) {
			t.Fatalf("daemon %s stderr does not name the gate reason:\n%s", test.action, stderr)
		}
	}
}

// TestDaemonRestartRefusesADaemonLaunchdDoesNotManage keeps the command from
// waiting out its whole budget for a replacement that is never coming: a
// daemon started with start --foreground answers the request but never
// kickstarts itself.
func TestDaemonRestartRefusesADaemonLaunchdDoesNotManage(t *testing.T) {
	home, err := os.MkdirTemp("/tmp", "wxl-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(home) })
	t.Setenv("HOME", home)
	// A budget long enough that a regression waiting on it would be obvious.
	restore := daemonWaitTimeout
	daemonWaitTimeout = 30 * time.Second
	t.Cleanup(func() { daemonWaitTimeout = restore })
	socket, err := config.SocketPath()
	if err != nil {
		t.Fatal(err)
	}
	cancel, done := serveUntilCanceled(t, socket, unmanagedHandler{})
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}()
	started := time.Now()
	exit, stderr := runCapturingStderr(t, []string{"daemon", "restart"})
	if exit != 1 {
		t.Fatalf("daemon restart exit=%d, want 1", exit)
	}
	if !strings.Contains(stderr, "not managed by launchd") {
		t.Fatalf("daemon restart stderr does not name the reason:\n%s", stderr)
	}
	if elapsed := time.Since(started); elapsed >= daemonWaitTimeout {
		t.Fatalf("daemon restart waited %s before refusing", elapsed)
	}
}

// TestDaemonStartCallsBackAPendingStop covers the branch that makes start
// idempotent for real: a socket that answers is not enough when the daemon
// behind it is still holding a stop nobody called back.
func TestDaemonStartCallsBackAPendingStop(t *testing.T) {
	home, err := os.MkdirTemp("/tmp", "wxl-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(home) })
	t.Setenv("HOME", home)
	socket, err := config.SocketPath()
	if err != nil {
		t.Fatal(err)
	}
	cancel, done := serveUntilCanceled(t, socket, cancellingStartHandler{})
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}()
	out := captureStdout(t, func() {
		if exit := run(context.Background(), []string{"daemon", "start"}); exit != 0 {
			t.Fatalf("daemon start exit=%d, want 0", exit)
		}
	})
	if !strings.Contains(out, "cancelled the pending stop") {
		t.Fatalf("daemon start did not report the cancelled stop:\n%s", out)
	}
}

// TestDaemonStopRefusesADaemonThatIsAlreadyRestarting keeps a refused request
// from being read as an accepted one: the daemon answered that it is heading
// somewhere else, so waiting for the socket to go quiet would only spend the
// budget.
func TestDaemonStopRefusesADaemonThatIsAlreadyRestarting(t *testing.T) {
	home, err := os.MkdirTemp("/tmp", "wxl-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(home) })
	t.Setenv("HOME", home)
	restore := daemonWaitTimeout
	daemonWaitTimeout = 30 * time.Second
	t.Cleanup(func() { daemonWaitTimeout = restore })
	socket, err := config.SocketPath()
	if err != nil {
		t.Fatal(err)
	}
	cancel, done := serveUntilCanceled(t, socket, conflictHandler{})
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}()
	started := time.Now()
	exit, stderr := runCapturingStderr(t, []string{"daemon", "stop"})
	if exit != 1 {
		t.Fatalf("daemon stop exit=%d, want 1", exit)
	}
	if !strings.Contains(stderr, "already restarting") {
		t.Fatalf("daemon stop stderr does not name the conflict:\n%s", stderr)
	}
	if elapsed := time.Since(started); elapsed >= daemonWaitTimeout {
		t.Fatalf("daemon stop waited %s before refusing", elapsed)
	}
}

type conflictHandler struct{}

func (conflictHandler) Handle(_ context.Context, method string, _ json.RawMessage) (any, error) {
	if method == "RequestStop" {
		return map[string]any{"pid": 1234, "conflict": true, "stop_pending": false, "restart_pending": true}, nil
	}
	return map[string]any{"ok": true, "pid": 1234}, nil
}

type cancellingStartHandler struct{}

func (cancellingStartHandler) Handle(_ context.Context, method string, _ json.RawMessage) (any, error) {
	if method == "RequestStart" {
		return map[string]any{"pid": 1234, "stop_pending": false, "stop_cancelled": true}, nil
	}
	return map[string]any{"ok": true, "pid": 1234}, nil
}

type unmanagedHandler struct{}

func (unmanagedHandler) Handle(_ context.Context, method string, _ json.RawMessage) (any, error) {
	switch method {
	case "RequestRestart":
		return map[string]any{"pid": 1234, "launchd_managed": false, "inflight_requests": 0, "queued_jobs": 0, "quiet_period_remaining_ms": 0}, nil
	default:
		return map[string]any{"ok": true, "pid": 1234}, nil
	}
}

type busyHandler struct{}

func (busyHandler) Handle(_ context.Context, method string, _ json.RawMessage) (any, error) {
	switch method {
	case "RequestStop", "RequestRestart":
		return map[string]any{"pid": 1234, "inflight_requests": 0, "queued_jobs": 2, "quiet_period_remaining_ms": 5000}, nil
	default:
		return map[string]any{"ok": true, "pid": 1234}, nil
	}
}

func TestTopUsageContract(t *testing.T) {
	var b bytes.Buffer
	topUsage(&b)
	got := b.String()
	for _, want := range []string{"Usage: wx", "Global options:", "Commands:", "claude [arguments", "daemon install|uninstall"} {
		if !strings.Contains(got, want) {
			t.Errorf("help missing %q:\n%s", want, got)
		}
	}
}

func TestBinaryHelpVersionAndMisuseContracts(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "wx")
	build := exec.Command("go", "build", "-o", binary, ".")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build wx: %v\n%s", err, output)
	}
	tests := []struct {
		args      []string
		exit      int
		contains  string
		useStderr bool
	}{
		{args: []string{"--help"}, contains: "Usage: wx"},
		{args: []string{"--version"}, contains: "wx version"},
		{args: []string{"doctor", "--help"}, contains: "Usage: wx doctor"},
		{args: []string{"unknown-command"}, exit: 2, contains: "unknown command or agent", useStderr: true},
		{args: nil, exit: 2, contains: "Usage: wx", useStderr: true},
	}
	for _, test := range tests {
		command := exec.Command(binary, test.args...)
		var stdout, stderr bytes.Buffer
		command.Stdout, command.Stderr = &stdout, &stderr
		err := command.Run()
		exit := 0
		var failure *exec.ExitError
		if errors.As(err, &failure) {
			exit = failure.ExitCode()
		} else if err != nil {
			t.Fatal(err)
		}
		output := stdout.String()
		if test.useStderr {
			output = stderr.String()
		}
		if exit != test.exit || !strings.Contains(output, test.contains) {
			t.Fatalf("wx %v: exit=%d stdout=%q stderr=%q", test.args, exit, stdout.String(), stderr.String())
		}
	}
}

func TestCommandDispatchAgainstRPCBoundary(t *testing.T) {
	home, err := os.MkdirTemp("/tmp", "wxh-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(home) })
	t.Setenv("HOME", home)
	bin := filepath.Join(home, "bin")
	if err := os.Mkdir(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "codex"), []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"wx", "launchctl"} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	socket, err := config.SocketPath()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	server := &rpc.Server{Socket: socket, Handler: commandHandler{workspace: home}}
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
	for _, args := range [][]string{
		{"status", "--json"},
		{"status"},
		{"doctor"},
		{"doctor", "--json"},
		{"gc", "--dry-run"},
		{"sessions", "--all", "--json"},
		{"sessions"},
		{"forget", home},
		{"config"},
		{"config", "logging.level", "warn"},
		{"codex", "exec"},
		{"resume", "session", "codex"},
		{"daemon", "install"},
		{"daemon", "uninstall"},
		// start short-circuits on the listening socket without going near
		// launchctl. stop and restart are not in this table on purpose: both
		// wait for this very server to go away, which it does not do here, so
		// they get their own fixture in
		// TestDaemonStopAndRestartWaitForTheDaemonToReachTheRequestedState.
		{"daemon", "start"},
	} {
		if exit := run(ctx, args); exit != 0 {
			t.Fatalf("run(%v) exit=%d", args, exit)
		}
	}
	for _, args := range [][]string{{}, {"unknown"}, {"--unknown", "codex"}, {"status", "extra"}, {"status", "--unknown"}, {"gc", "extra"}, {"sessions", "extra"}, {"forget"}, {"resume"}, {"resume", "session", "invalid"}, {"daemon", "unknown"}, {"hook"}, {"--fresh", "codex"}} {
		if exit := run(ctx, args); exit != 2 {
			t.Fatalf("misuse run(%v) exit=%d", args, exit)
		}
	}
	for _, args := range [][]string{{"--help"}, {"--version"}, {"status", "--help"}, {"daemon", "--help"}} {
		if exit := run(ctx, args); exit != 0 {
			t.Fatalf("informational run(%v) exit=%d", args, exit)
		}
	}
	t.Setenv("WX_SESSION_ID", "session")
	t.Setenv("WX_SESSION_TOKEN", "token")
	t.Setenv("WX_DAEMON_SOCKET", socket)
	if exit := run(ctx, []string{"hook", "unknown"}); exit != 1 {
		t.Fatalf("unknown hook exit=%d", exit)
	}
	oldBuildMeta := buildMeta
	buildMeta = ""
	if versionString() != version {
		t.Fatalf("version without metadata=%q", versionString())
	}
	buildMeta = oldBuildMeta
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestAgentArgumentsPassThrough(t *testing.T) {
	f, agent, args, err := parseAgentPrefix([]string{"--branch", "feature", "codex", "exec", "--model", "x"})
	if err != nil {
		t.Fatal(err)
	}
	if agent != "codex" || strings.Join(args, "|") != "exec|--model|x" || len(f.branches) != 1 {
		t.Fatalf("agent=%s args=%v flags=%+v", agent, args, f)
	}
}

func TestCommandBackendAndConfigurationFailuresReturnNonzero(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	ctx := context.Background()
	for _, args := range [][]string{
		{"status"},
		{"gc", "--dry-run"},
		{"sessions", "--all"},
		{"forget", home},
	} {
		if exit := run(ctx, args); exit != 1 {
			t.Fatalf("missing daemon run(%v) exit=%d", args, exit)
		}
	}
	for _, args := range [][]string{
		{"config", "unknown.key", "value"},
		{"config", "logging.level", "verbose"},
	} {
		if exit := run(ctx, args); exit != 1 {
			t.Fatalf("invalid config run(%v) exit=%d", args, exit)
		}
	}
	configPath, err := config.Path()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(configPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, []byte("unknown: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"config"},
		{"codex"},
		{"resume", "session", "codex"},
	} {
		if exit := run(ctx, args); exit != 1 {
			t.Fatalf("invalid persisted config run(%v) exit=%d", args, exit)
		}
	}
	t.Setenv("WX_SESSION_ID", "session")
	t.Setenv("WX_SESSION_TOKEN", "")
	if exit := run(ctx, []string{"hook", "session-start"}); exit != 1 {
		t.Fatalf("incomplete hook environment exit=%d", exit)
	}
}

func TestEveryPublicSubcommandHasSpecificHelp(t *testing.T) {
	for _, command := range []string{"status", "doctor", "gc", "sessions", "config", "resume", "forget", "daemon"} {
		t.Run(command, func(t *testing.T) {
			var output bytes.Buffer
			commandUsage(&output, command)
			if want := "Usage: wx " + command; !strings.HasPrefix(output.String(), want) {
				t.Fatalf("help=%q, want prefix %q", output.String(), want)
			}
			if strings.Count(output.String(), "\n") < 2 {
				t.Fatalf("command help lacks a description: %q", output.String())
			}
		})
	}
}

func TestAgentPrefixAndCommandUsageFallbacks(t *testing.T) {
	if _, _, _, err := parseAgentPrefix(nil); err == nil {
		t.Fatal("missing agent command was accepted")
	}
	var output strings.Builder
	commandUsage(&output, "not-a-command")
	if !strings.Contains(output.String(), "Usage: wx") {
		t.Fatalf("unknown command usage=%q", output.String())
	}
}
