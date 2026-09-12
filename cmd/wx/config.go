package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/pflag"

	"github.com/HappyOnigiri/WX/internal/config"
	"github.com/HappyOnigiri/WX/internal/discovery"
	"github.com/HappyOnigiri/WX/internal/gitx"
)

// configEdit は1回の設定更新の指示である。op は set・add・remove・reset のいずれか。
type configEdit struct {
	key   string
	value string
	op    string
}

const (
	configOpSet    = "set"
	configOpAdd    = "add"
	configOpRemove = "remove"
	configOpReset  = "reset"
)

// parseConfigEdit は位置引数を1件の更新指示へ解釈する。
// 引数の個数が合わない場合は ok=false を返し、呼び出し側が usage を出して終了コード2にする。
func parseConfigEdit(args []string) (configEdit, bool) {
	if len(args) < 2 {
		return configEdit{}, false
	}
	switch args[1] {
	case "--add", "--remove":
		if len(args) != 3 {
			return configEdit{}, false
		}
		op := configOpAdd
		if args[1] == "--remove" {
			op = configOpRemove
		}
		return configEdit{key: args[0], value: args[2], op: op}, true
	case "--reset":
		if len(args) != 2 {
			return configEdit{}, false
		}
		return configEdit{key: args[0], op: configOpReset}, true
	default:
		if len(args) != 2 {
			return configEdit{}, false
		}
		return configEdit{key: args[0], value: args[1], op: configOpSet}, true
	}
}

func runConfig(ctx context.Context, args []string) int {
	fs := pflag.NewFlagSet("config", pflag.ContinueOnError)
	workspace := fs.String("workspace", "", "target a workspace-specific setting")
	repository := fs.String("repository", "", "target a repository-specific setting")
	describe := fs.String("describe", "", "describe a setting")
	// 設定値は「-」で始まることもあるため、最初の位置引数（キー）以降はフラグとして扱わない。
	fs.SetInterspersed(false)
	fs.Usage = func() { commandUsage(os.Stdout, "config") }
	if code, done := finishFlagParse(fs, "config", args); done {
		return code
	}
	rest := fs.Args()
	if *workspace != "" && *repository != "" {
		fmt.Fprintln(os.Stderr, "error: --workspace and --repository cannot be combined")
		return 2
	}
	if *describe != "" {
		if len(rest) != 0 {
			commandUsage(os.Stderr, "config")
			return 2
		}
		scope := "global"
		if *workspace != "" {
			scope = config.ScopeWorkspace.String()
		} else if *repository != "" {
			scope = config.ScopeRepository.String()
		}
		return describeConfig(*describe, scope)
	}
	switch {
	case *workspace != "":
		return runScopeConfig(ctx, config.ScopeWorkspace, *workspace, rest)
	case *repository != "":
		return runScopeConfig(ctx, config.ScopeRepository, *repository, rest)
	}
	if len(rest) == 0 {
		return showGlobalConfig()
	}
	edit, ok := parseConfigEdit(rest)
	if !ok {
		commandUsage(os.Stderr, "config")
		return 2
	}
	return executeConfigEdit(ctx, config.EditRequest{Scope: "global", Key: edit.key, Value: edit.value, Operation: config.EditOperation(edit.op)})
}

func describeConfig(key, scope string) int {
	meta, err := config.Describe(key, scope)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	fmt.Printf("%s — %s\n", meta.Key, meta.DisplayName)
	fmt.Printf("  Type: %s\n", meta.Kind)
	fmt.Printf("  Scopes: %s\n", strings.Join(meta.Scopes, ", "))
	if len(meta.Choices) > 0 {
		fmt.Printf("  Choices: %s\n", strings.Join(meta.Choices, ", "))
	}
	fmt.Printf("  %s\n", meta.Description)
	fmt.Printf("  Impact: %s\n", meta.Impact)
	return 0
}

func showGlobalConfig() int {
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
	for _, f := range config.Lists(cfg) {
		fmt.Printf("  %-42s = %s\n", f.Key, f.Value)
	}
	return 0
}

func runScopeConfig(ctx context.Context, scope config.Scope, path string, args []string) int {
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	target, err := resolveConfigScope(ctx, cfg, scope, path)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	if len(args) == 0 {
		fmt.Printf("%s: %s\n", scopeTitle(scope), target)
		for _, f := range config.ScopeFields(cfg, scope, target) {
			fmt.Printf("  %-42s = %s (source: %s)\n", f.Key, f.Value, f.Source)
		}
		return 0
	}
	edit, ok := parseConfigEdit(args)
	if !ok {
		commandUsage(os.Stderr, "config")
		return 2
	}
	return executeConfigEdit(ctx, config.EditRequest{Scope: scope.String(), Target: target, Key: edit.key, Value: edit.value, Operation: config.EditOperation(edit.op)})
}

// executeConfigEdit は共通サービスで preview と保存を連続して行い、CLI の表示契約だけを担当する。
func executeConfigEdit(ctx context.Context, request config.EditRequest) int {
	preview, err := config.PreviewEdit(request)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	if err := config.CommitEdit(preview); err != nil {
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

func scopeTitle(scope config.Scope) string {
	if scope == config.ScopeRepository {
		return "Repository"
	}
	return "Workspace"
}

// resolveConfigScope は指定 path を設定キーと同じ表記の対象へ解決する。
// workspace は repository なら main worktree、それ以外はそのディレクトリ。repository は repository 外を拒否する。
func resolveConfigScope(ctx context.Context, cfg config.Config, scope config.Scope, path string) (string, error) {
	discoverer := discovery.Discoverer{Git: &gitx.Runner{Timeout: cfg.Discovery.Timeout.Duration}, Config: cfg}
	if scope == config.ScopeRepository {
		return discoverer.MainWorktree(ctx, path)
	}
	return discoverer.PolicyRoot(ctx, path)
}
