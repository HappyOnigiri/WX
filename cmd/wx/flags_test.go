package main

import (
	"testing"
)

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
