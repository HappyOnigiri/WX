package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/HappyOnigiri/WX/internal/cli"
	"github.com/HappyOnigiri/WX/internal/config"
	"github.com/HappyOnigiri/WX/internal/dashboard"
)

func runDashboard(ctx context.Context) int {
	cwd, err := os.Getwd()
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	notice := ""
	for {
		cfg, configErr := config.Load()
		if configErr != nil {
			// 不正設定でも診断や daemon 操作は使えるよう、設定タブだけを既定値で表示する。
			cfg = config.Defaults()
			notice = "設定を読み込めません: " + configErr.Error()
		}
		action, runErr := dashboard.Run(ctx, dashboard.Options{Status: dashboardStatus, CWD: cwd, Config: cfg, Notice: notice})
		if errors.Is(runErr, dashboard.ErrCancelled) {
			return 0
		}
		if runErr != nil {
			fmt.Fprintln(os.Stderr, "error: dashboard:", runErr)
			return 1
		}
		code := runDashboardAction(ctx, action)
		notice = fmt.Sprintf("%s を終了しました（exit %d）", action.Args[0], code)
	}
}

func dashboardStatus(ctx context.Context) (string, error) {
	c, err := rpcClient()
	if err != nil {
		return "", err
	}
	callCtx, cancel := context.WithTimeout(ctx, statusDisplayTimeout)
	defer cancel()
	var payload map[string]any
	if err := c.Call(callCtx, "Status", struct{}{}, &payload); err != nil {
		return "", errors.New(rpcErrorMessage(err))
	}
	var out bytes.Buffer
	printStatusDisplay(&out, payload, false)
	return out.String(), nil
}

func runDashboardAction(ctx context.Context, action dashboard.Action) int {
	if len(action.Args) == 0 {
		return 0
	}
	cwd := filepath.Clean(action.WorkDir)
	if action.WorkDir != "" {
		info, err := os.Stat(cwd)
		if err != nil || !info.IsDir() {
			fmt.Fprintf(os.Stderr, "error: dashboard target %q is not an accessible directory\n", action.WorkDir)
			return 1
		}
	}
	command, args := action.Args[0], action.Args[1:]
	switch command {
	case "claude", "codex":
		cfg, err := config.Load()
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			return 1
		}
		client, err := cli.New(cfg)
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			return 1
		}
		return client.RunAgentWithPolicyFrom(ctx, cwd, command, args, nil, false, cli.WorktreeOptions{})
	case "shell":
		return runShellFrom(ctx, args, cwd)
	case "run":
		return runRunFrom(ctx, args, cwd)
	case "new":
		return runNewFrom(ctx, args, cwd)
	case "bench":
		return runBenchFrom(ctx, args, cwd)
	default:
		return run(ctx, action.Args)
	}
}
