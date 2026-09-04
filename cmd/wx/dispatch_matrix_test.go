package main

import (
	"context"
	"testing"
)

func TestCommandDispatchRejectsMalformedAndUnavailableRequests(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	cases := []struct {
		name string
		args []string
		want int
	}{
		{name: "empty", want: 2},
		{name: "unknown", args: []string{"not-a-command"}, want: 2},
		{name: "unknown agent", args: []string{"editor"}, want: 2},
		{name: "agent flag parse", args: []string{"--not-a-flag"}, want: 2},
		{name: "status extra argument", args: []string{"status", "extra"}, want: 2},
		{name: "status unavailable", args: []string{"status"}, want: 1},
		{name: "gc extra argument", args: []string{"gc", "extra"}, want: 2},
		{name: "gc unavailable", args: []string{"gc"}, want: 1},
		{name: "config wrong arity", args: []string{"config", "a", "b", "c"}, want: 2},
		{name: "config invalid field", args: []string{"config", "unknown.field", "value"}, want: 1},
		{name: "resume missing id", args: []string{"resume"}, want: 2},
		{name: "resume invalid agent", args: []string{"resume", "session", "editor"}, want: 2},
		{name: "daemon wrong arity", args: []string{"daemon"}, want: 2},
		{name: "daemon unknown action", args: []string{"daemon", "unknown"}, want: 2},
		{name: "daemon restart extra argument", args: []string{"daemon", "restart", "extra"}, want: 2},
		{name: "daemon stop extra argument", args: []string{"daemon", "stop", "extra"}, want: 2},
		{name: "daemon start extra argument", args: []string{"daemon", "start", "extra"}, want: 2},
		// --foreground is only meaningful for start; accepting it silently
		// elsewhere would make wx daemon stop --foreground look like a mode
		// rather than the misuse it is.
		{name: "daemon stop rejects foreground", args: []string{"daemon", "stop", "--foreground"}, want: 2},
		{name: "daemon restart rejects foreground", args: []string{"daemon", "restart", "--foreground"}, want: 2},
		{name: "daemon install rejects foreground", args: []string{"daemon", "install", "--foreground"}, want: 2},
		{name: "daemon restart unavailable", args: []string{"daemon", "restart"}, want: 1},
		// A cancelled context reaches the RPC client before it can classify the
		// missing socket as a connect error, so stop reports the failure rather
		// than the "already stopped" it answers with a live context.
		{name: "daemon stop unavailable", args: []string{"daemon", "stop"}, want: 1},
		{name: "daemon start unavailable", args: []string{"daemon", "start"}, want: 1},
		{name: "hook missing event", args: []string{"hook"}, want: 2},
		{name: "sessions extra argument", args: []string{"sessions", "extra"}, want: 2},
		{name: "sessions unavailable", args: []string{"sessions"}, want: 1},
		{name: "forget missing path", args: []string{"forget"}, want: 2},
		{name: "forget unavailable", args: []string{"forget", "/tmp/workspace"}, want: 1},
		{name: "agent daemon unavailable", args: []string{"codex"}, want: 1},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			if got := run(ctx, test.args); got != test.want {
				t.Fatalf("run(%v)=%d, want %d", test.args, got, test.want)
			}
		})
	}
}

func TestConfigCommandDisplaysDefaultsAndHelp(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if got := runConfig(context.Background(), []string{"--help"}); got != 0 {
		t.Fatalf("config help exit=%d", got)
	}
	if got := runConfig(context.Background(), nil); got != 0 {
		t.Fatalf("config display exit=%d", got)
	}
}
