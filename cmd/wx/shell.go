package main

import (
	"context"
	"fmt"
	"os"

	"github.com/spf13/pflag"

	"github.com/HappyOnigiri/WX/internal/cli"
	"github.com/HappyOnigiri/WX/internal/config"
)

func runShell(ctx context.Context, args []string) int {
	fs := pflag.NewFlagSet("shell", pflag.ContinueOnError)
	branches := fs.StringArray("branch", nil, "detached base branch")
	resume := fs.String("resume", "", "restore the worktree of a wx session")
	fs.Usage = func() { commandUsage(os.Stdout, "shell") }
	if code, done := finishFlagParse(fs, "shell", args); done {
		return code
	}
	if fs.NArg() != 0 {
		commandUsage(os.Stderr, "shell")
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
	return client.RunLeaseShell(ctx, *branches, *resume)
}

// leaseClient は貸出コマンドが共有する設定読み込みと client 生成を行う。
func leaseClient() (cli.Client, int) {
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return cli.Client{}, 1
	}
	client, err := cli.New(cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return cli.Client{}, 1
	}
	return client, 0
}
