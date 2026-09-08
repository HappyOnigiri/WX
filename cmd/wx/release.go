package main

import (
	"context"
	"os"

	"github.com/spf13/pflag"
)

func runRelease(ctx context.Context, args []string) int {
	fs := pflag.NewFlagSet("release", pflag.ContinueOnError)
	discard := fs.Bool("discard", false, "return the lease without saving unfinished work")
	fs.Usage = func() { commandUsage(os.Stdout, "release") }
	if code, done := finishFlagParse(fs, "release", args); done {
		return code
	}
	if fs.NArg() != 1 {
		commandUsage(os.Stderr, "release")
		return 2
	}
	client, code := leaseClient()
	if code != 0 {
		return code
	}
	return client.RunLeaseRelease(ctx, fs.Arg(0), *discard)
}
