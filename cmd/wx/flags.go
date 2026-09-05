package main

import (
	"errors"

	"github.com/spf13/pflag"
)

type agentFlags struct {
	branches       []string
	fresh          bool
	worktree       bool
	noWorktree     bool
	selectWorktree bool
}

func parseAgentPrefix(args []string) (agentFlags, string, []string, error) {
	fs := pflag.NewFlagSet("wx", pflag.ContinueOnError)
	fs.SetInterspersed(false)
	var f agentFlags
	fs.StringArrayVar(&f.branches, "branch", nil, "detached base branch")
	fs.BoolVar(&f.fresh, "fresh", false, "continue without recovery state")
	fs.BoolVarP(&f.worktree, "worktree", "w", false, "create a worktree for this invocation")
	fs.BoolVarP(&f.noWorktree, "no-worktree", "n", false, "run in the current directory for this invocation")
	fs.BoolVarP(&f.selectWorktree, "select-worktree", "s", false, "select and save this workspace policy again")
	if err := fs.Parse(args); err != nil {
		return f, "", nil, err
	}
	if (f.worktree && f.noWorktree) || (f.selectWorktree && (f.worktree || f.noWorktree)) {
		return f, "", nil, errors.New("--worktree, --no-worktree, and --select-worktree are mutually exclusive")
	}
	rest := fs.Args()
	if len(rest) == 0 {
		if f.selectWorktree {
			return f, "", nil, nil
		}
		return f, "", nil, pflag.ErrHelp
	}
	return f, rest[0], rest[1:], nil
}
