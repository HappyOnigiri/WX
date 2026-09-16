package main

import (
	"context"
	"fmt"
	"os"

	"github.com/spf13/pflag"

	"github.com/HappyOnigiri/WX/internal/diag"
	"github.com/HappyOnigiri/WX/internal/i18n"
)

func runWorktreeSetupCheck(ctx context.Context, args []string) int {
	ctx = commandContext(ctx)
	fs := pflag.NewFlagSet("setup-check", pflag.ContinueOnError)
	fs.Usage = func() { commandUsageLanguage(os.Stdout, "setup-check", i18n.LanguageFromContext(ctx)) }
	if code, done := finishFlagParse(fs, "setup-check", args); done {
		return code
	}
	if fs.NArg() > 1 {
		commandUsageLanguage(os.Stderr, "setup-check", i18n.LanguageFromContext(ctx))
		return 2
	}
	var root string
	if fs.NArg() == 1 {
		root = fs.Arg(0)
	} else {
		var err error
		root, err = os.Getwd()
		if err != nil {
			fmt.Fprintln(os.Stderr, i18n.T(ctx, "common.error", nil)+":", err)
			return 1
		}
	}
	client, code := leaseClient()
	if code != 0 {
		return code
	}
	reply := diag.Resolve(diag.Reply{Findings: client.RunSetupCheck(ctx, root)}, i18n.LanguageFromContext(ctx))
	diag.RenderLanguage(os.Stdout, reply, true, i18n.LanguageFromContext(ctx))
	return diag.ExitCode(reply)
}
