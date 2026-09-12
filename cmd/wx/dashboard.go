package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/HappyOnigiri/WX/internal/cli"
	"github.com/HappyOnigiri/WX/internal/config"
	"github.com/HappyOnigiri/WX/internal/dashboard"
	"github.com/HappyOnigiri/WX/internal/setup"
)

func runDashboard(ctx context.Context) int {
	cwd, err := os.Getwd()
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	notice := ""
	for {
		cfg, rawConfig, configErr := config.LoadWithRaw()
		if configErr != nil {
			// 不正設定でも診断や daemon 操作は使えるよう、設定タブだけを既定値で表示する。
			cfg, rawConfig = config.Defaults(), config.Config{}
			notice = "Could not load configuration: " + configErr.Error()
		}
		addDashboardEnvironments(ctx, &cfg)
		steps, _ := setup.Collect(ctx, setupOptions())
		action, runErr := dashboard.Run(ctx, dashboard.Options{
			Status: dashboardStatus, CWD: cwd, Config: cfg, RawConfig: rawConfig, Setup: steps, Notice: notice,
			Execute: runDashboardInlineAction, Refresh: refreshDashboardState,
		})
		if errors.Is(runErr, dashboard.ErrCancelled) {
			return 0
		}
		if runErr != nil {
			fmt.Fprintln(os.Stderr, "error: dashboard:", runErr)
			return 1
		}
		code := runDashboardAction(ctx, action)
		notice = fmt.Sprintf("%s finished (exit %d)", action.Args[0], code)
	}
}

func refreshDashboardState(ctx context.Context) (config.Config, config.Config, []setup.Step, error) {
	cfg, rawConfig, err := config.LoadWithRaw()
	if err != nil {
		return config.Config{}, config.Config{}, nil, err
	}
	addDashboardEnvironments(ctx, &cfg)
	steps, err := setup.Collect(ctx, setupOptions())
	return cfg, rawConfig, steps, err
}

// addDashboardEnvironments は設定ファイルに書かれた workspace へ daemon 側の登録状況を重ねる。
// 環境一覧の対象は設定済みの workspace だけに保ち、貸出のたびに増える一時ディレクトリや
// 実体の消えた登録を設定対象として並べない。補う値は表示用で、設定ファイルへは保存しない。
func addDashboardEnvironments(ctx context.Context, cfg *config.Config) {
	if cfg == nil || len(cfg.Workspaces) == 0 {
		return
	}
	c, err := rpcClient()
	if err != nil {
		return
	}
	callCtx, cancel := context.WithTimeout(ctx, statusDisplayTimeout)
	defer cancel()
	var payload map[string]any
	if err := c.Call(callCtx, "Status", struct{}{}, &payload); err != nil {
		return
	}
	markDashboardRegistrations(cfg, payload)
}

// markDashboardRegistrations は Status 応答を設定済み workspace へ重ねる。
// daemon への接続を伴わない純粋な合成として分け、表示対象の決め方をテストで固定する。
func markDashboardRegistrations(cfg *config.Config, payload map[string]any) {
	if !cfg.V2() {
		return
	}
	workspaceDetails, _ := payload["workspace_details"].([]any)
	for _, raw := range workspaceDetails {
		item, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		root, _ := item["root"].(string)
		if root == "" {
			continue
		}
		workspace, configured := cfg.Workspaces[root]
		if !configured {
			continue
		}
		workspace.Discovered = true
		// membership は個別設定を書くまで設定ファイルに現れないため、設定済み workspace の配下に限って daemon の一覧から補う。
		members, _ := item["repositories"].([]any)
		if len(members) == 0 {
			members, _ = item["repository_memberships"].([]any)
		}
		for _, rawMembership := range members {
			membership, ok := rawMembership.(map[string]any)
			if !ok {
				continue
			}
			relative, _ := membership["relative_path"].(string)
			if relative == "" {
				continue
			}
			if workspace.Repositories == nil {
				workspace.Repositories = map[string]config.Repository{}
			}
			repository := workspace.Repositories[relative]
			repository.Discovered = true
			workspace.Repositories[relative] = repository
		}
		cfg.Workspaces[root] = workspace
	}
}

// runDashboardInlineAction は端末を引き渡さない CLI 操作を子 process で実行し、TUI の描画先と出力を分離する。
func runDashboardInlineAction(ctx context.Context, action dashboard.Action) (string, int) {
	if len(action.Args) == 0 {
		return "", 0
	}
	binary, err := os.Executable()
	if err != nil {
		return "error: " + err.Error(), 1
	}
	command := exec.CommandContext(ctx, binary, action.Args...)
	if action.WorkDir != "" {
		command.Dir = action.WorkDir
	}
	output, err := command.CombinedOutput()
	text := strings.TrimSpace(string(output))
	if err == nil {
		return text, 0
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return text, exitErr.ExitCode()
	}
	if text != "" {
		text += "\n"
	}
	return text + "error: " + err.Error(), 1
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
