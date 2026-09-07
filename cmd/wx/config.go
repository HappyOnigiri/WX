package main

import (
	"context"
	"fmt"
	"os"

	"github.com/spf13/pflag"

	"github.com/HappyOnigiri/WX/internal/config"
)

func runConfig(ctx context.Context, args []string) int {
	fs := pflag.NewFlagSet("config", pflag.ContinueOnError)
	// 設定値は「-」で始まることもあるため、最初の位置引数（キー）以降はフラグとして扱わない。
	fs.SetInterspersed(false)
	fs.Usage = func() { commandUsage(os.Stdout, "config") }
	if code, done := finishFlagParse(fs, "config", args); done {
		return code
	}
	rest := fs.Args()
	if len(rest) == 0 {
		cfg, err := config.Load()
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			return 1
		}
		path, _ := config.Path()
		fmt.Println("Config:", path)
		for _, f := range config.Fields(cfg) {
			fmt.Printf("  %-42s = %s\n", f.Key, f.Value)
		}
		return 0
	}
	if len(rest) < 2 {
		commandUsage(os.Stderr, "config")
		return 2
	}
	raw, err := config.LoadRaw()
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	key := rest[0]
	switch rest[1] {
	case "--add":
		if len(rest) != 3 {
			commandUsage(os.Stderr, "config")
			return 2
		}
		err = config.AppendList(&raw, key, rest[2])
	case "--remove":
		if len(rest) != 3 {
			commandUsage(os.Stderr, "config")
			return 2
		}
		err = config.RemoveList(&raw, key, rest[2])
	case "--reset":
		if len(rest) != 2 {
			commandUsage(os.Stderr, "config")
			return 2
		}
		err = config.ResetList(&raw, key)
	default:
		if len(rest) != 2 {
			commandUsage(os.Stderr, "config")
			return 2
		}
		err = config.SetField(&raw, key, rest[1])
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	effective := config.Merge(config.Defaults(), raw)
	if err := config.NormalizePaths(&effective); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	if err := config.Validate(&effective); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	if err := config.Save(raw); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	c, _ := rpcClient()
	if err := c.Call(ctx, "ReloadConfig", struct{}{}, nil); err != nil {
		fmt.Printf("saved; daemon reload pending: %s\n", rpcErrorMessage(err))
	} else {
		fmt.Println("saved and reloaded")
	}
	return 0
}
