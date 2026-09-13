package main

import (
	"context"
	"fmt"
	"os"

	"github.com/spf13/pflag"

	"github.com/HappyOnigiri/WX/internal/i18n"
)

func runRun(ctx context.Context, args []string) int {
	ctx = commandContext(ctx)
	return runRunFrom(ctx, args, "")
}

func runRunFrom(ctx context.Context, args []string, cwd string) int {
	ctx = commandContext(ctx)
	fs := pflag.NewFlagSet("run", pflag.ContinueOnError)
	// コマンドの argv をそのまま渡すため、wx のオプションは先頭だけで解釈する。
	fs.SetInterspersed(false)
	branches := fs.StringArray("branch", nil, "detached base branch")
	resume := fs.String("resume", "", "restore the worktree of a wx session")
	fs.Usage = func() { commandUsageLanguage(os.Stdout, "run", i18n.LanguageFromContext(ctx)) }
	if code, done := finishFlagParse(fs, "run", args); done {
		return code
	}
	if fs.NArg() == 0 {
		commandUsageLanguage(os.Stderr, "run", i18n.LanguageFromContext(ctx))
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
		return client.RunLeaseCommandFrom(ctx, cwd, fs.Args(), *branches, *resume)
	}
	return client.RunLeaseCommand(ctx, fs.Args(), *branches, *resume)
}
