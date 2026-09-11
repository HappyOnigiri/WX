package main

import (
	"context"
	"fmt"
	"os"

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
	raw, err := config.LoadRaw()
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	if err := applyGlobalEdit(&raw, edit); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	return saveConfig(ctx, raw)
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

func applyGlobalEdit(raw *config.Config, edit configEdit) error {
	switch edit.op {
	case configOpAdd:
		return config.AppendList(raw, edit.key, edit.value)
	case configOpRemove:
		return config.RemoveList(raw, edit.key, edit.value)
	case configOpReset:
		// scalar と list で未設定へ戻す経路が違うため、キーの種別で振り分ける。
		if config.IsListKey(edit.key) {
			return config.ResetList(raw, edit.key)
		}
		return config.ResetField(raw, edit.key)
	default:
		return config.SetField(raw, edit.key, edit.value)
	}
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
	raw, err := config.LoadRaw()
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	if err := applyScopeEdit(&raw, scope, target, edit); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	return saveConfig(ctx, raw)
}

func applyScopeEdit(raw *config.Config, scope config.Scope, target string, edit configEdit) error {
	switch edit.op {
	case configOpAdd:
		return config.AppendScopeList(raw, scope, target, edit.key, edit.value)
	case configOpRemove:
		return config.RemoveScopeList(raw, scope, target, edit.key, edit.value)
	case configOpReset:
		if config.IsScopeListKey(scope, edit.key) {
			return config.ResetScopeList(raw, scope, target, edit.key)
		}
		return config.ResetScopeField(raw, scope, target, edit.key)
	default:
		return config.SetScopeField(raw, scope, target, edit.key, edit.value)
	}
}

// saveConfig は更新した raw 設定を検証してから保存し、動いている daemon へ反映を促す。
func saveConfig(ctx context.Context, raw config.Config) int {
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
