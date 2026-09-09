package main

import (
	"context"
	"os"

	"github.com/spf13/pflag"
)

func runBench(ctx context.Context, args []string) int {
	fs := pflag.NewFlagSet("bench", pflag.ContinueOnError)
	runs := fs.Int("runs", 1, "number of measured leases")
	branches := fs.StringArray("branch", nil, "detached base branch")
	reuse := fs.Bool("reuse", false, "measure what the pool returns instead of forcing a cold start")
	jsonOut := fs.Bool("json", false, "print JSON")
	fs.Usage = func() { commandUsage(os.Stdout, "bench") }
	if code, done := finishFlagParse(fs, "bench", args); done {
		return code
	}
	if fs.NArg() != 0 {
		commandUsage(os.Stderr, "bench")
		return 2
	}
	client, code := leaseClient()
	if code != 0 {
		return code
	}
	return client.RunBench(ctx, *runs, *branches, *reuse, *jsonOut)
}
