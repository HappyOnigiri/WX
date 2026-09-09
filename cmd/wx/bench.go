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
	fs := pflag.NewFlagSet("bench", pflag.ContinueOnError)
	runs := fs.Int("runs", 1, "number of measured leases")
	branches := fs.StringArray("branch", nil, "detached base branch")
	reuse := fs.Bool("reuse", false, "measure what the pool returns instead of forcing a cold start")
	configs := fs.StringArray("config", nil, "preparation settings to compare (repeatable)")
	jsonOut := fs.Bool("json", false, "print JSON")
	fs.Usage = func() { commandUsage(os.Stdout, "bench") }
	if code, done := finishFlagParse(fs, "bench", args); done {
		return code
	}
	if fs.NArg() != 0 {
		commandUsage(os.Stderr, "bench")
		return 2
	}
	overrides, code := benchOverrides(*configs)
	if code != 0 {
		return code
	}
	client, code := leaseClient()
	if code != 0 {
		return code
	}
	return client.RunBench(ctx, cli.BenchOptions{Runs: *runs, Branches: *branches, Reuse: *reuse, JSON: *jsonOut, Configs: overrides})
}

// benchOverrides は --config の指定を貸出要求へ載せる上書きへ直す。
// 値の誤りは daemon へ送る前に引数エラーで終える。測定を1回でも走らせると standby が退役するためである。
func benchOverrides(specs []string) ([]config.PrepareOverride, int) {
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
