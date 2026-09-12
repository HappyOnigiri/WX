package main

import (
	"context"
	"os"

	"github.com/spf13/pflag"

	"github.com/HappyOnigiri/WX/internal/i18n"
)

func runRelease(ctx context.Context, args []string) int {
	ctx = commandContext(ctx)
	fs := pflag.NewFlagSet("release", pflag.ContinueOnError)
	discard := fs.Bool("discard", false, "return the lease without saving unfinished work")
	fs.Usage = func() { commandUsageLanguage(os.Stdout, "release", i18n.LanguageFromContext(ctx)) }
	if code, done := finishFlagParse(fs, "release", args); done {
		return code
	}
	if fs.NArg() != 1 {
		commandUsageLanguage(os.Stderr, "release", i18n.LanguageFromContext(ctx))
		return 2
	}
	client, code := leaseClient()
	if code != 0 {
		return code
	}
	return client.RunLeaseRelease(ctx, fs.Arg(0), *discard)
}
