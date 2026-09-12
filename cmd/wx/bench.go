package main

import (
	"context"
	"fmt"
	"os"

	"github.com/spf13/pflag"

	"github.com/HappyOnigiri/WX/internal/cli"
	"github.com/HappyOnigiri/WX/internal/config"
)

func runBench(ctx context.Context, args []string) int {
	return runBenchFrom(ctx, args, "")
}

func runBenchFrom(ctx context.Context, args []string, cwd string) int {
	fs := pflag.NewFlagSet("bench", pflag.ContinueOnError)
	runs := fs.Int("runs", 1, "number of measured leases")
	branches := fs.StringArray("branch", nil, "detached base branch")
	reuse := fs.Bool("reuse", false, "measure what the pool returns instead of forcing a cold start")
	configs := fs.StringArray("config", nil, "preparation settings to compare (repeatable)")
	sweep := fs.Bool("sweep", false, "compare the standard set of preparation settings")
	jsonOut := fs.Bool("json", false, "print JSON")
	fs.Usage = func() { commandUsage(os.Stdout, "bench") }
	if code, done := finishFlagParse(fs, "bench", args); done {
		return code
	}
	if fs.NArg() != 0 {
		commandUsage(os.Stderr, "bench")
		return 2
	}
	overrides, code := benchConfigs(*sweep, *configs)
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

// benchConfigs は --sweep と --config の指定を貸出要求へ載せる上書きの並びへ直す。
// 値の誤りは daemon へ送る前に引数エラーで終える。測定を1回でも走らせると standby が退役するためである。
func benchConfigs(sweep bool, specs []string) ([]config.PrepareOverride, int) {
	// --sweep は測る設定の並び全体を指すため、--config を足すと並びの意味が二通りになる。
	if sweep {
		if len(specs) > 0 {
			_, _ = fmt.Fprintln(os.Stderr, "error: --sweep cannot be combined with --config; --sweep already names every setting it measures")
			return nil, 2
		}
		return cli.BenchSweepConfigs(), 0
	}
	out := make([]config.PrepareOverride, 0, len(specs))
	for _, spec := range specs {
		override, err := config.ParsePrepareOverride(spec)
		if err != nil {
			_, _ = fmt.Fprintln(os.Stderr, "error: --config "+spec+": "+err.Error())
			return nil, 2
		}
		out = append(out, override)
	}
	return out, 0
}
