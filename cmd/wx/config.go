package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/pflag"

	"github.com/HappyOnigiri/WX/internal/config"
	"github.com/HappyOnigiri/WX/internal/discovery"
	"github.com/HappyOnigiri/WX/internal/gitx"
	"github.com/HappyOnigiri/WX/internal/i18n"
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
	ctx = commandContext(ctx)
	fs := pflag.NewFlagSet("config", pflag.ContinueOnError)
	workspace := fs.String("workspace", "", "target a workspace-specific setting")
	repository := fs.String("repository", "", "target a repository-specific setting")
	describe := fs.String("describe", "", "describe a setting")
	// 設定値は「-」で始まることもあるため、最初の位置引数（キー）以降はフラグとして扱わない。
	fs.SetInterspersed(false)
	fs.Usage = func() { commandUsageLanguage(os.Stdout, "config", i18n.LanguageFromContext(ctx)) }
	if code, done := finishFlagParse(fs, "config", args); done {
		return code
	}
	rest := fs.Args()
	if *workspace != "" && *repository != "" {
		message := "--workspace and --repository cannot be combined"
		if i18n.LanguageFromContext(ctx) == i18n.Japanese {
			message = "--workspace と --repository は併用できません"
		}
		fmt.Fprintln(os.Stderr, i18n.T(ctx, "common.error", nil)+":", message)
		return 2
	}
	if *describe != "" {
		if len(rest) != 0 {
			commandUsageLanguage(os.Stderr, "config", i18n.LanguageFromContext(ctx))
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
	// 配布スクリプトと外部ツールが設定言語を取得する機械契約。現在の実効値だけを
	// 1 行で返し、Config の人間向け一覧や reload の案内を混ぜない。
	if *workspace == "" && *repository == "" && len(rest) == 1 && rest[0] == "language" {
		effective, _, err := config.LoadWithRaw()
		if err != nil {
			lang := i18n.LanguageFromContext(ctx)
			fmt.Fprintln(os.Stderr, i18n.New(string(lang)).Localize("common.error", nil)+":", localizeError(err, lang))
			return 1
		}
		_, _ = fmt.Fprintln(os.Stdout, effective.DisplayLanguage())
		return 0
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
		commandUsageLanguage(os.Stderr, "config", i18n.LanguageFromContext(ctx))
		return 2
	}
	return executeConfigEdit(ctx, config.EditRequest{Scope: "global", Key: edit.key, Value: edit.value, Operation: config.EditOperation(edit.op)})
}

func describeConfig(key, scope string) int {
	meta, err := config.Describe(key, scope)
	if err != nil {
		lang := localizedUsageLanguage()
		fmt.Fprintln(os.Stderr, i18n.New(string(lang)).Localize("common.error", nil)+":", localizeError(err, lang))
		return 1
	}
	lang := localizedUsageLanguage()
	name := meta.DisplayName
	if meta.Key == "language" && lang == i18n.Japanese {
		name = i18n.New(string(lang)).Localize("config.display_name", nil)
	}
	description := translateHumanOutput(meta.Description, lang)
	impact := translateHumanOutput(meta.Impact, lang)
	var rendered bytes.Buffer
	fmt.Fprintf(&rendered, "%s — %s\n", meta.Key, name)
	fmt.Fprintf(&rendered, "  Type: %s\n", meta.Kind)
	fmt.Fprintf(&rendered, "  Scopes: %s\n", strings.Join(meta.Scopes, ", "))
	if len(meta.Choices) > 0 {
		fmt.Fprintf(&rendered, "  Choices: %s\n", strings.Join(meta.Choices, ", "))
	}
	fmt.Fprintf(&rendered, "  %s\n", description)
	fmt.Fprintf(&rendered, "  Impact: %s\n", impact)
	fmt.Print(translateHumanOutput(rendered.String(), lang))
	return 0
}

func showGlobalConfig() int {
	cfg, err := config.Load()
	if err != nil {
		lang := i18n.Normalize(config.LoadLanguage())
		fmt.Fprintln(os.Stderr, i18n.New(string(lang)).Localize("common.error", nil)+":", localizeError(err, lang))
		return 1
	}
	path, _ := config.Path()
	var rendered bytes.Buffer
	fmt.Fprintln(&rendered, "Config:", path)
	// language は未記載でも英語という実効値を返すため、Fields の疎な表示除外とは別に出す。
	fmt.Fprintf(&rendered, "  %-42s = %s\n", "language", cfg.DisplayLanguage())
	for _, f := range config.Fields(cfg) {
		if f.Key == "language" {
			continue
		}
		fmt.Fprintf(&rendered, "  %-42s = %s\n", f.Key, f.Value)
	}
	for _, f := range config.Lists(cfg) {
		fmt.Fprintf(&rendered, "  %-42s = %s\n", f.Key, f.Value)
	}
	fmt.Print(translateHumanOutput(rendered.String(), localizedUsageLanguage()))
	return 0
}

func runScopeConfig(ctx context.Context, scope config.Scope, path string, args []string) int {
	cfg, err := config.Load()
	if err != nil {
		lang := i18n.Normalize(config.LoadLanguage())
		fmt.Fprintln(os.Stderr, i18n.New(string(lang)).Localize("common.error", nil)+":", localizeError(err, lang))
		return 1
	}
	target, err := resolveConfigScope(ctx, cfg, scope, path)
	if err != nil {
		lang := i18n.Normalize(config.LoadLanguage())
		fmt.Fprintln(os.Stderr, i18n.New(string(lang)).Localize("common.error", nil)+":", localizeError(err, lang))
		return 1
	}
	if len(args) == 0 {
		var rendered bytes.Buffer
		fmt.Fprintf(&rendered, "%s: %s\n", scopeTitle(scope), target)
		for _, f := range config.ScopeFields(cfg, scope, target) {
			fmt.Fprintf(&rendered, "  %-42s = %s (source: %s)\n", f.Key, f.Value, f.Source)
		}
		fmt.Print(translateHumanOutput(rendered.String(), localizedUsageLanguage()))
		return 0
	}
	edit, ok := parseConfigEdit(args)
	if !ok {
		commandUsageLanguage(os.Stderr, "config", i18n.LanguageFromContext(ctx))
		return 2
	}
	return executeConfigEdit(ctx, config.EditRequest{Scope: scope.String(), Target: target, Key: edit.key, Value: edit.value, Operation: config.EditOperation(edit.op)})
}

// executeConfigEdit は共通サービスで preview と保存を連続して行い、CLI の表示契約だけを担当する。
func executeConfigEdit(ctx context.Context, request config.EditRequest) int {
	ctx = commandContext(ctx)
	preview, err := config.PreviewEdit(request)
	if err != nil {
		lang := i18n.LanguageFromContext(ctx)
		fmt.Fprintln(os.Stderr, i18n.New(string(lang)).Localize("common.error", nil)+":", localizeError(err, lang))
		return 1
	}
	if err := config.CommitEdit(preview); err != nil {
		lang := i18n.LanguageFromContext(ctx)
		fmt.Fprintln(os.Stderr, i18n.New(string(lang)).Localize("common.error", nil)+":", localizeError(err, lang))
		return 1
	}
	c, _ := rpcClient()
	if err := c.Call(ctx, "ReloadConfig", struct{}{}, nil); err != nil {
		lang := i18n.Normalize(config.LoadLanguage())
		message := i18n.New(string(lang)).Localize("common.saved_pending", nil)
		fmt.Printf("%s: %s\n", message, rpcErrorMessageLanguage(err, lang))
	} else {
		fmt.Println(i18n.New(config.LoadLanguage()).Localize("common.saved", nil))
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
