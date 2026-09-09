package main

import (
	"context"
	"fmt"
	"os"
	"strconv"

	"github.com/spf13/pflag"

	"github.com/HappyOnigiri/WX/internal/config"
	"github.com/HappyOnigiri/WX/internal/discovery"
	"github.com/HappyOnigiri/WX/internal/gitx"
)

func runConfig(ctx context.Context, args []string) int {
	fs := pflag.NewFlagSet("config", pflag.ContinueOnError)
	workspace := fs.String("workspace", "", "target a workspace-specific setting")
	// 設定値は「-」で始まることもあるため、最初の位置引数（キー）以降はフラグとして扱わない。
	fs.SetInterspersed(false)
	fs.Usage = func() { commandUsage(os.Stdout, "config") }
	if code, done := finishFlagParse(fs, "config", args); done {
		return code
	}
	rest := fs.Args()
	if *workspace != "" {
		return runWorkspaceConfig(ctx, *workspace, rest)
	}
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
		fmt.Printf("  %-42s = %q\n", "readiness.early_paths", cfg.Readiness.EarlyPaths)
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

func runWorkspaceConfig(ctx context.Context, path string, args []string) int {
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	root, err := resolveConfigWorkspace(ctx, cfg, path)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	if len(args) == 0 {
		count, overridden := cfg.WarmCountForWorkspace(root)
		countSource := "global"
		if overridden {
			countSource = "workspace"
		}
		reuse, reuseOverridden := cfg.ReuseStandbyForWorkspace(root)
		reuseSource := "global"
		if reuseOverridden {
			reuseSource = "workspace"
		}
		fmt.Printf("Workspace: %s\n", root)
		fmt.Printf("  warm_count = %d (source: %s)\n", count, countSource)
		fmt.Printf("  reuse_standby = %t (source: %s)\n", reuse, reuseSource)
		return 0
	}
	if len(args) != 2 || (args[0] != "warm_count" && args[0] != "reuse_standby") {
		commandUsage(os.Stderr, "config")
		return 2
	}
	raw, err := config.LoadRaw()
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	if args[0] == "warm_count" {
		switch args[1] {
		case "--reset":
			err = config.ResetWorkspaceWarmCount(&raw, root)
		default:
			count, parseErr := strconv.Atoi(args[1])
			if parseErr != nil {
				err = fmt.Errorf("warm_count must be an integer: %w", parseErr)
			} else {
				err = config.SetWorkspaceWarmCount(&raw, root, count)
			}
		}
	} else {
		switch args[1] {
		case "--reset":
			err = config.ResetWorkspaceReuseStandby(&raw, root)
		default:
			enabled, parseErr := strconv.ParseBool(args[1])
			if parseErr != nil {
				err = fmt.Errorf("reuse_standby must be true or false: %w", parseErr)
			} else {
				err = config.SetWorkspaceReuseStandby(&raw, root, enabled)
			}
		}
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

func resolveConfigWorkspace(ctx context.Context, cfg config.Config, path string) (string, error) {
	discoverer := discovery.Discoverer{Git: &gitx.Runner{Timeout: cfg.Discovery.Timeout.Duration}, Config: cfg}
	return discoverer.PolicyRoot(ctx, path)
}
