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

func runBench(ctx context.Context, args []string) int {
	ctx = commandContext(ctx)
	return runBenchFrom(ctx, args, "")
}

func runBenchFrom(ctx context.Context, args []string, cwd string) int {
	ctx = commandContext(ctx)
	fs := pflag.NewFlagSet("bench", pflag.ContinueOnError)
	runs := fs.Int("runs", 1, "number of measured leases")
	branches := fs.StringArray("branch", nil, "detached base branch")
	reuse := fs.Bool("reuse", false, "measure what the pool returns instead of forcing a cold start")
	configs := fs.StringArray("config", nil, "preparation settings to compare (repeatable)")
	sweep := fs.Bool("sweep", false, "compare the standard set of preparation settings")
	jsonOut := fs.Bool("json", false, "print JSON")
	fs.Usage = func() { commandUsageLanguage(os.Stdout, "bench", i18n.LanguageFromContext(ctx)) }
	if code, done := finishFlagParse(fs, "bench", args); done {
		return code
	}
	if fs.NArg() != 0 {
		commandUsageLanguage(os.Stderr, "bench", i18n.LanguageFromContext(ctx))
		return 2
	}
	overrides, code := benchConfigsLanguage(*sweep, *configs, i18n.LanguageFromContext(ctx))
	if code != 0 {
		return code
	}
	client, code := leaseClient()
	if code != 0 {
		return code
	}
	opts := cli.BenchOptions{Runs: *runs, Branches: *branches, Reuse: *reuse, JSON: *jsonOut, Configs: overrides}
	if cwd != "" {
		return client.RunBenchFrom(ctx, cwd, opts)
	}
	return client.RunBench(ctx, opts)
}

func benchConfigsLanguage(sweep bool, specs []string, lang i18n.Language) ([]config.PrepareOverride, int) {
	// --sweep は測る設定の並び全体を指すため、--config を足すと並びの意味が二通りになる。
	if sweep {
		if len(specs) > 0 {
			_, _ = fmt.Fprintln(os.Stderr, i18n.New(string(lang)).Localize("common.error", nil)+": --sweep cannot be combined with --config; --sweep already names every setting it measures")
			return nil, 2
		}
		return cli.BenchSweepConfigs(), 0
	}
	out := make([]config.PrepareOverride, 0, len(specs))
	for _, spec := range specs {
		override, err := config.ParsePrepareOverride(spec)
		if err != nil {
			_, _ = fmt.Fprintln(os.Stderr, i18n.New(string(lang)).Localize("common.error", nil)+": --config "+spec+": "+err.Error())
			return nil, 2
		}
		out = append(out, override)
	}
	return out, 0
}
