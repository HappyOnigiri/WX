package main

import (
	"context"
	"fmt"
	"os"

	"github.com/spf13/pflag"

	"github.com/HappyOnigiri/WX/internal/cli"
	"github.com/HappyOnigiri/WX/internal/config"
	"github.com/HappyOnigiri/WX/internal/i18n"
)

func runShell(ctx context.Context, args []string) int {
	ctx = commandContext(ctx)
	return runShellFrom(ctx, args, "")
}

func runShellFrom(ctx context.Context, args []string, cwd string) int {
	ctx = commandContext(ctx)
	fs := pflag.NewFlagSet("shell", pflag.ContinueOnError)
	branches := fs.StringArray("branch", nil, "detached base branch")
	resume := fs.String("resume", "", "restore the worktree of a wx session")
	fs.Usage = func() { commandUsageLanguage(os.Stdout, "shell", i18n.LanguageFromContext(ctx)) }
	if code, done := finishFlagParse(fs, "shell", args); done {
		return code
	}
	if fs.NArg() != 0 {
		commandUsageLanguage(os.Stderr, "shell", i18n.LanguageFromContext(ctx))
		return 2
	}
	if len(*branches) > 0 && *resume != "" {
		message := "--branch and --resume choose different bases; use one of them"
		if i18n.LanguageFromContext(ctx) == i18n.Japanese {
			message = "--branch と --resume は異なる base を選ぶため、どちらか一方を使ってください"
		}
		fmt.Fprintln(os.Stderr, i18n.T(ctx, "common.error", nil)+":", message)
		return 2
	}
	client, code := leaseClient()
	if code != 0 {
		return code
	}
	if cwd != "" {
		return client.RunLeaseShellFrom(ctx, cwd, *branches, *resume)
	}
	return client.RunLeaseShell(ctx, *branches, *resume)
}

// leaseClient は貸出コマンドが共有する設定読み込みと client 生成を行う。
func leaseClient() (cli.Client, int) {
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, i18n.New(config.LoadLanguage()).Localize("common.error", nil)+":", err)
		return cli.Client{}, 1
	}
	client, err := cli.New(cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, i18n.New(config.LoadLanguage()).Localize("common.error", nil)+":", err)
		return cli.Client{}, 1
	}
	return client, 0
}
