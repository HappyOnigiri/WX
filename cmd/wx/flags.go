package main

import "github.com/spf13/pflag"

type agentFlags struct {
	branches []string
	fresh    bool
}

func parseAgentPrefix(args []string) (agentFlags, string, []string, error) {
	fs := pflag.NewFlagSet("wx", pflag.ContinueOnError)
	fs.SetInterspersed(false)
	var f agentFlags
	fs.StringArrayVar(&f.branches, "branch", nil, "detached base branch")
	fs.BoolVar(&f.fresh, "fresh", false, "continue without recovery state")
	if err := fs.Parse(args); err != nil {
		return f, "", nil, err
	}
	rest := fs.Args()
	if len(rest) == 0 {
		return f, "", nil, pflag.ErrHelp
	}
	return f, rest[0], rest[1:], nil
}
