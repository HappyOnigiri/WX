package main

import (
	"reflect"
	"strings"
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

func TestAgentBranchArgumentsAreExtracted(t *testing.T) {
	tests := []struct {
		name     string
		args     []string
		branches []string
		agent    string
		passed   []string
	}{
		{
			name:     "space separated after agent",
			args:     []string{"claude", "--branch", "feature/api", "--resume"},
			branches: []string{"feature/api"},
			agent:    "claude",
			passed:   []string{"--resume"},
		},
		{
			name:     "equals after agent",
			args:     []string{"codex", "exec", "--branch=repo=feature/api", "--model", "o3"},
			branches: []string{"repo=feature/api"},
			agent:    "codex",
			passed:   []string{"exec", "--model", "o3"},
		},
		{
			name:     "mixed prefix and suffix",
			args:     []string{"--branch", "base", "codex", "--branch=repo=one", "exec", "--branch", "two", "--branch=repo=three"},
			branches: []string{"base", "repo=one", "two", "repo=three"},
			agent:    "codex",
			passed:   []string{"exec"},
		},
		{
			name:     "double dash preserves suffix",
			args:     []string{"codex", "exec", "--branch", "before", "--", "--branch", "after", "value"},
			branches: []string{"before"},
			agent:    "codex",
			passed:   []string{"exec", "--", "--branch", "after", "value"},
		},
		{
			name:     "empty equals value follows pflag",
			args:     []string{"claude", "--branch="},
			branches: []string{""},
			agent:    "claude",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			f, agent, passed, err := parseAgentPrefix(test.args)
			if err != nil {
				t.Fatal(err)
			}
			if agent != test.agent || !reflect.DeepEqual(passed, test.passed) || !reflect.DeepEqual(f.branches, test.branches) {
				t.Fatalf("agent=%q passed=%v branches=%v, want agent=%q passed=%v branches=%v", agent, passed, f.branches, test.agent, test.passed, test.branches)
			}
		})
	}
}

func TestAgentBranchArgumentValidationAndBoundary(t *testing.T) {
	if _, _, _, err := parseAgentPrefix([]string{"codex", "--branch"}); err == nil || err.Error() != "flag needs an argument: --branch" {
		t.Fatalf("missing post-agent branch error=%v", err)
	}
	_, agent, passed, err := parseAgentPrefix([]string{"codex", "--", "--branch", "after"})
	if err != nil || agent != "codex" || strings.Join(passed, "|") != "--|--branch|after" {
		t.Fatalf("boundary parse agent=%q passed=%v err=%v", agent, passed, err)
	}
}
