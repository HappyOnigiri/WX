package main

import (
	"bytes"
	"strings"
	"testing"
)

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
