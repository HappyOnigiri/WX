package main

import (
	"context"
	"fmt"
	"os"

	"github.com/spf13/pflag"
)

func runRun(ctx context.Context, args []string) int {
	fs := pflag.NewFlagSet("run", pflag.ContinueOnError)
	// コマンドの argv をそのまま渡すため、wx のオプションは先頭だけで解釈する。
	fs.SetInterspersed(false)
	branches := fs.StringArray("branch", nil, "detached base branch")
	resume := fs.String("resume", "", "restore the worktree of a wx session")
	fs.Usage = func() { commandUsage(os.Stdout, "run") }
	if code, done := finishFlagParse(fs, "run", args); done {
		return code
	}
	if fs.NArg() == 0 {
		commandUsage(os.Stderr, "run")
		return 2
	}
	if len(*branches) > 0 && *resume != "" {
		fmt.Fprintln(os.Stderr, "error: --branch and --resume choose different bases; use one of them")
		return 2
	}
	client, code := leaseClient()
	if code != 0 {
		return code
	}
	return client.RunLeaseCommand(ctx, fs.Args(), *branches, *resume)
}
