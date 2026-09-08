package main

import (
	"context"
	"os"

	"github.com/spf13/pflag"
)

func runNew(ctx context.Context, args []string) int {
	fs := pflag.NewFlagSet("new", pflag.ContinueOnError)
	branches := fs.StringArray("branch", nil, "detached base branch")
	jsonOut := fs.Bool("json", false, "print JSON")
	fs.Usage = func() { commandUsage(os.Stdout, "new") }
	if code, done := finishFlagParse(fs, "new", args); done {
		return code
	}
	if fs.NArg() != 0 {
		commandUsage(os.Stderr, "new")
		return 2
	}
	client, code := leaseClient()
	if code != 0 {
		return code
	}
	return client.RunLeaseNew(ctx, *branches, *jsonOut)
}
