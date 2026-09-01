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
	"testing"
	"time"

	"github.com/HappyOnigiri/WX/internal/config"
	"github.com/HappyOnigiri/WX/internal/rpc"
)

type commandHandler struct{}

func (commandHandler) Handle(_ context.Context, method string, _ json.RawMessage) (any, error) {
	switch method {
	case "Sessions":
		return []map[string]any{{"id": "session", "state": "ACTIVE", "agent": "codex"}}, nil
	case "GC":
		return map[string]int{"candidates": 0}, nil
	default:
		return map[string]any{"ok": true}, nil
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
	socket, err := config.SocketPath()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	server := &rpc.Server{Socket: socket, Handler: commandHandler{}}
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
		{"doctor"},
		{"gc", "--dry-run"},
		{"sessions", "--all", "--json"},
		{"forget", home},
		{"config"},
		{"config", "logging.level", "warn"},
	} {
		if exit := run(ctx, args); exit != 0 {
			t.Fatalf("run(%v) exit=%d", args, exit)
		}
	}
	for _, args := range [][]string{{}, {"unknown"}, {"gc", "extra"}, {"sessions", "extra"}, {"forget"}, {"resume"}, {"daemon", "unknown"}, {"hook"}} {
		if exit := run(ctx, args); exit != 2 {
			t.Fatalf("misuse run(%v) exit=%d", args, exit)
		}
	}
	for _, args := range [][]string{{"--help"}, {"--version"}, {"status", "--help"}, {"daemon", "--help"}} {
		if exit := run(ctx, args); exit != 0 {
			t.Fatalf("informational run(%v) exit=%d", args, exit)
		}
	}
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

func TestEveryPublicSubcommandHasSpecificHelp(t *testing.T) {
	for _, command := range []string{"status", "doctor", "gc", "sessions", "config", "resume", "forget", "daemon"} {
		t.Run(command, func(t *testing.T) {
			var output bytes.Buffer
			commandUsage(&output, command)
			if want := "Usage: wx " + command; !strings.HasPrefix(output.String(), want) {
				t.Fatalf("help=%q, want prefix %q", output.String(), want)
			}
		})
	}
}
