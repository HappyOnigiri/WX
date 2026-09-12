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
	"github.com/HappyOnigiri/WX/internal/i18n"
	"github.com/HappyOnigiri/WX/internal/setup"
)

func runDashboard(ctx context.Context) int {
	ctx = commandContext(ctx)
	cwd, err := os.Getwd()
	if err != nil {
		fmt.Fprintln(os.Stderr, i18n.T(ctx, "common.error", nil)+":", err)
		return 1
	}
	notice := ""
	for {
		// ダッシュボード内で language を変更した操作の後も、次の画面・RPC へ
		// 新しい設定を渡す。起動時の context は commandContext 済みでも更新する。
		ctx = i18n.WithLanguage(ctx, string(config.LoadLanguage()))
		cfg, rawConfig, configErr := config.LoadWithRaw()
		if configErr != nil {
			// 不正設定でも診断や daemon 操作は使えるよう、設定タブだけを既定値で表示する。
			cfg, rawConfig = config.Defaults(), config.Config{}
			notice = i18n.T(ctx, "common.error", nil) + ": Could not load configuration: " + configErr.Error()
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
			fmt.Fprintln(os.Stderr, i18n.T(ctx, "common.error", nil)+": dashboard:", runErr)
			return 1
		}
		code := runDashboardAction(ctx, action)
		if i18n.LanguageFromContext(ctx) == i18n.Japanese {
			notice = fmt.Sprintf("%s が終了しました（終了コード %d）", action.Args[0], code)
		} else {
			notice = fmt.Sprintf("%s finished (exit %d)", action.Args[0], code)
		}
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

// addDashboardEnvironments は daemon に登録済みだが個別設定を持たない workspace と repository も環境一覧へ加える。
// 空の override は表示用の Config だけに置き、設定ファイルへは保存しない。
func addDashboardEnvironments(ctx context.Context, cfg *config.Config) {
	if cfg == nil {
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
	if cfg.Workspaces == nil {
		cfg.Workspaces = map[string]config.Workspace{}
	}
	workspaceDetails, _ := payload["workspace_details"].([]any)
	for _, raw := range workspaceDetails {
		item, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		root, ok := item["root"].(string)
		if ok && root != "" {
			if _, exists := cfg.Workspaces[root]; !exists {
				cfg.Workspaces[root] = config.Workspace{}
			}
		}
	}
	if cfg.Repositories == nil {
		cfg.Repositories = map[string]config.Repository{}
	}
	repositoryDetails, _ := payload["repository_details"].([]any)
	for _, raw := range repositoryDetails {
		item, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		mainPath, ok := item["main_path"].(string)
		if ok && mainPath != "" {
			if _, exists := cfg.Repositories[mainPath]; !exists {
				cfg.Repositories[mainPath] = config.Repository{}
			}
		}
	}
}

// runDashboardInlineAction は端末を引き渡さない CLI 操作を子 process で実行し、TUI の描画先と出力を分離する。
func runDashboardInlineAction(ctx context.Context, action dashboard.Action) (string, int) {
	ctx = commandContext(ctx)
	if len(action.Args) == 0 {
		return "", 0
	}
	binary, err := os.Executable()
	if err != nil {
		return i18n.T(ctx, "common.error", nil) + ": " + err.Error(), 1
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
	return text + i18n.T(ctx, "common.error", nil) + ": " + err.Error(), 1
}

func dashboardStatus(ctx context.Context) (string, error) {
	ctx = commandContext(ctx)
	c, err := rpcClient()
	if err != nil {
		return "", err
	}
	callCtx, cancel := context.WithTimeout(ctx, statusDisplayTimeout)
	defer cancel()
	var payload map[string]any
	if err := c.Call(callCtx, "Status", map[string]string{"language": string(i18n.LanguageFromContext(ctx))}, &payload); err != nil {
		return "", errors.New(rpcErrorMessageLanguage(err, i18n.LanguageFromContext(ctx)))
	}
	var out bytes.Buffer
	printStatusDisplay(&out, payload, false)
	return translateHumanOutput(out.String(), i18n.LanguageFromContext(ctx)), nil
}

func runDashboardAction(ctx context.Context, action dashboard.Action) int {
	if len(action.Args) == 0 {
		return 0
	}
	cwd := filepath.Clean(action.WorkDir)
	if action.WorkDir != "" {
		info, err := os.Stat(cwd)
		if err != nil || !info.IsDir() {
			message := fmt.Sprintf("dashboard target %q is not an accessible directory", action.WorkDir)
			if i18n.LanguageFromContext(ctx) == i18n.Japanese {
				message = fmt.Sprintf("dashboard の対象 %q は利用可能な directory ではありません", action.WorkDir)
			}
			fmt.Fprintln(os.Stderr, i18n.T(ctx, "common.error", nil)+":", message)
			return 1
		}
	}
	command, args := action.Args[0], action.Args[1:]
	switch command {
	case "claude", "codex":
		cfg, err := config.Load()
		if err != nil {
			fmt.Fprintln(os.Stderr, i18n.T(ctx, "common.error", nil)+":", err)
			return 1
		}
		client, err := cli.New(cfg)
		if err != nil {
			fmt.Fprintln(os.Stderr, i18n.T(ctx, "common.error", nil)+":", err)
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
