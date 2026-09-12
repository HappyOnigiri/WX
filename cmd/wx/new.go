package main

import (
	"context"
	"os"

	"github.com/spf13/pflag"

	"github.com/HappyOnigiri/WX/internal/i18n"
)

func runNew(ctx context.Context, args []string) int {
	ctx = commandContext(ctx)
	return runNewFrom(ctx, args, "")
}

func runNewFrom(ctx context.Context, args []string, cwd string) int {
	ctx = commandContext(ctx)
	fs := pflag.NewFlagSet("new", pflag.ContinueOnError)
	branches := fs.StringArray("branch", nil, "detached base branch")
	jsonOut := fs.Bool("json", false, "print JSON")
	fs.Usage = func() { commandUsageLanguage(os.Stdout, "new", i18n.LanguageFromContext(ctx)) }
	if code, done := finishFlagParse(fs, "new", args); done {
		return code
	}
	if fs.NArg() != 0 {
		commandUsageLanguage(os.Stderr, "new", i18n.LanguageFromContext(ctx))
		return 2
	}
	client, code := leaseClient()
	if code != 0 {
		return code
	}
	if cwd != "" {
		return client.RunLeaseNewFrom(ctx, cwd, *branches, *jsonOut)
	}
	return client.RunLeaseNew(ctx, *branches, *jsonOut)
}
