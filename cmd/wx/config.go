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
	system := fs.Bool("system", false, "target system settings (config v2)")
	workspaceDefaults := fs.Bool("workspace-defaults", false, "target workspace defaults (config v2)")
	repositoryDefaults := fs.Bool("repository-defaults", false, "target repository defaults (config v2)")
	describe := fs.String("describe", "", "describe a setting")
	// 設定値は「-」で始まることもあるため、最初の位置引数（キー）以降はフラグとして扱わない。
	fs.SetInterspersed(false)
	fs.Usage = func() { commandUsageLanguage(os.Stdout, "config", i18n.LanguageFromContext(ctx)) }
	if code, done := finishFlagParse(fs, "config", args); done {
		return code
	}
	rest := fs.Args()
	raw, rawErr := config.LoadRaw()
	v2 := rawErr == nil && raw.V2()
	// 配布スクリプトと外部ツールが設定言語を取得する機械契約。現在の実効値だけを
	// 1 行で返し、Config の人間向け一覧や reload の案内を混ぜない。
	// scope を明示しない読み取りなので、v1 と v2 のどちらの設定でも同じ出力にする。
	if *describe == "" && !*system && !*workspaceDefaults && !*repositoryDefaults &&
		*workspace == "" && *repository == "" && len(rest) == 1 && rest[0] == "language" {
		effective, _, err := config.LoadWithRaw()
		if err != nil {
			lang := i18n.LanguageFromContext(ctx)
			fmt.Fprintln(os.Stderr, i18n.New(string(lang)).Localize("common.error", nil)+":", i18n.LocalizeError(err, lang))
			return 1
		}
		_, _ = fmt.Fprintln(os.Stdout, effective.DisplayLanguage())
		return 0
	}
	if v2 || *system || *workspaceDefaults || *repositoryDefaults {
		return runV2Config(ctx, *system, *workspaceDefaults, *repositoryDefaults, *workspace, *repository, *describe, rest)
	}
	if *workspace != "" && *repository != "" {
		fmt.Fprintln(os.Stderr, i18n.T(ctx, "common.error", nil)+":", i18n.T(ctx, "wx.config.scope_conflict", nil))
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

// runV2Config は config v2 の明示的な scope 構文を処理する。
// Repository 設定は常に workspace root と相対 membership path の組で指定し、
// main path をキーにした global map は受け付けない。
func runV2Config(ctx context.Context, system, workspaceDefaults, repositoryDefaults bool, workspacePath, repositoryPath, describe string, rest []string) int {
	if system && (workspaceDefaults || repositoryDefaults || workspacePath != "" || repositoryPath != "") {
		fmt.Fprintln(os.Stderr, "error: --system cannot be combined with another config scope")
		return 2
	}
	if workspaceDefaults && (repositoryDefaults || workspacePath != "" || repositoryPath != "") {
		fmt.Fprintln(os.Stderr, "error: --workspace-defaults cannot be combined with another config scope")
		return 2
	}
	if repositoryPath != "" && workspacePath == "" {
		fmt.Fprintln(os.Stderr, "error: --repository requires --workspace <root> in config v2")
		return 2
	}
	if repositoryPath != "" && repositoryDefaults {
		fmt.Fprintln(os.Stderr, "error: --repository and --repository-defaults cannot be combined")
		return 2
	}

	cfg, raw, err := config.LoadWithRaw()
	if err != nil {
		// 新規環境では明示的な v2 scope の表示・編集を既定値で続けられる。
		// 既存ファイルが壊れている場合はエラーとして扱う。
		path, pathErr := config.Path()
		if pathErr != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			return 1
		}
		if _, statErr := os.Stat(path); statErr != nil && os.IsNotExist(statErr) {
			cfg, raw = config.DefaultsV2(), config.Config{}
		} else {
			fmt.Fprintln(os.Stderr, "error:", err)
			return 1
		}
	}
	if !cfg.V2() {
		cfg = config.DefaultsV2()
	}

	scope, target, rel, nestedRepositoryDefaults, code := resolveV2Scope(ctx, cfg, system, workspaceDefaults, repositoryDefaults, workspacePath, repositoryPath)
	if code != 0 {
		return code
	}
	if describe != "" {
		if len(rest) != 0 {
			commandUsage(os.Stderr, "config")
			return 2
		}
		describeScope := scope
		if describeScope == "" {
			describeScope = ""
		}
		if nestedRepositoryDefaults {
			describe = "repository_defaults." + describe
		}
		return describeConfig(describe, describeScope)
	}
	if scope == "" {
		if len(rest) == 0 {
			return showV2GlobalConfig(cfg, raw)
		}
		fmt.Fprintln(os.Stderr, "error: config v2 requires an explicit scope (--system, --workspace-defaults, --repository-defaults, or --workspace)")
		return 2
	}
	if len(rest) == 0 {
		if scope == config.V2ScopeWorkspace {
			if nestedRepositoryDefaults {
				fmt.Printf("Repository defaults: %s\n", target)
			} else {
				fmt.Printf("Workspace: %s\n", target)
			}
		} else {
			fmt.Printf("%s:\n", v2ScopeTitle(scope))
		}
		fields := config.V2Fields(cfg, raw, scope, target, rel)
		if nestedRepositoryDefaults {
			filtered := make([]config.ScopeField, 0, len(fields))
			for _, field := range fields {
				if strings.HasPrefix(field.Key, "repository_defaults.") {
					filtered = append(filtered, field)
				}
			}
			fields = filtered
		}
		for _, field := range fields {
			fmt.Printf("  %-42s = %s (source: %s)\n", field.Key, field.Value, field.Source)
		}
		return 0
	}
	edit, ok := parseConfigEdit(rest)
	if !ok {
		commandUsage(os.Stderr, "config")
		return 2
	}
	if nestedRepositoryDefaults {
		edit.key = "repository_defaults." + edit.key
	}
	return executeConfigEdit(ctx, config.EditRequest{Scope: scope, Target: target, Repository: rel, V2: true, Key: edit.key, Value: edit.value, Operation: config.EditOperation(edit.op)})
}

func resolveV2Scope(ctx context.Context, cfg config.Config, system, workspaceDefaults, repositoryDefaults bool, workspacePath, repositoryPath string) (scope, target, rel string, nestedRepositoryDefaults bool, code int) {
	switch {
	case system:
		return config.V2ScopeSystem, "", "", false, 0
	case workspaceDefaults:
		return config.V2ScopeWorkspaceDefaults, "", "", false, 0
	case repositoryDefaults:
		if workspacePath == "" {
			return config.V2ScopeRepositoryDefaults, "", "", false, 0
		}
		root, err := resolveConfigScope(ctx, cfg, config.ScopeWorkspace, workspacePath)
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			return "", "", "", false, 1
		}
		return config.V2ScopeWorkspace, root, "", true, 0
	case repositoryPath != "":
		root, err := resolveConfigScope(ctx, cfg, config.ScopeWorkspace, workspacePath)
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			return "", "", "", false, 1
		}
		relative, err := config.NormalizeRepositoryRelative(repositoryPath)
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			return "", "", "", false, 2
		}
		configuredMulti := len(cfg.Workspaces[root].Repositories) > 1
		if !configuredMulti {
			discoverer := discovery.Discoverer{Git: &gitx.Runner{Timeout: cfg.Discovery.Timeout.Duration}, Config: cfg}
			workspace, err := discoverer.Resolve(ctx, root)
			if err != nil {
				fmt.Fprintln(os.Stderr, "error:", err)
				return "", "", "", false, 1
			}
			configuredMulti = len(workspace.Repositories) > 1
		}
		if !configuredMulti {
			fmt.Fprintln(os.Stderr, "error: individual repository settings require a multi-repository workspace")
			return "", "", "", false, 2
		}
		return config.V2ScopeRepository, root, relative, false, 0
	case workspacePath != "":
		root, err := resolveConfigScope(ctx, cfg, config.ScopeWorkspace, workspacePath)
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			return "", "", "", false, 1
		}
		return config.V2ScopeWorkspace, root, "", false, 0
	default:
		return "", "", "", false, 0
	}
}

func v2ScopeTitle(scope string) string {
	switch scope {
	case config.V2ScopeSystem:
		return "System"
	case config.V2ScopeWorkspaceDefaults:
		return "Workspace defaults"
	case config.V2ScopeRepositoryDefaults:
		return "Repository defaults"
	case config.V2ScopeRepository:
		return "Repository"
	default:
		return "Configuration"
	}
}

func describeConfig(key, scope string) int {
	meta, err := config.Describe(key, scope)
	if err != nil {
		lang := localizedUsageLanguage()
		fmt.Fprintln(os.Stderr, i18n.New(string(lang)).Localize("common.error", nil)+":", i18n.LocalizeError(err, lang))
		return 1
	}
	lang := localizedUsageLanguage()
	r := newTextRenderer(os.Stdout, lang)
	r.raw(meta.Key + " — " + configKeyText(r, meta.Key, "name", meta.DisplayName))
	r.field(2, "config.describe.type", string(meta.Kind))
	r.field(2, "config.describe.scopes", strings.Join(meta.Scopes, ", "))
	if len(meta.Choices) > 0 {
		r.field(2, "config.describe.choices", strings.Join(meta.Choices, ", "))
	}
	r.raw("  " + configKeyText(r, meta.Key, "description", meta.Description))
	r.field(2, "config.describe.impact", configKeyText(r, meta.Key, "impact", meta.Impact))
	return 0
}

// configKeyText は設定キーの散文を動的 ID で引き、カタログに無いキーは英語の原文をそのまま返す。
// 全キーの散文を訳す前でも、訳のあるキーだけが訳文になる。
func configKeyText(r *textRenderer, key, kind, fallback string) string {
	return r.LocalizeOr("config."+key+"."+kind, fallback)
}

func showGlobalConfig() int {
	cfg, raw, err := config.LoadWithRaw()
	if err != nil {
		lang := i18n.Normalize(config.LoadLanguage())
		fmt.Fprintln(os.Stderr, i18n.New(string(lang)).Localize("common.error", nil)+":", i18n.LocalizeError(err, lang))
		return 1
	}
	if cfg.V2() {
		return showV2GlobalConfig(cfg, raw)
	}
	path, _ := config.Path()
	r := newTextRenderer(os.Stdout, localizedUsageLanguage())
	r.line("config.show.path", map[string]any{"Path": path})
	// language は未記載でも英語という実効値を返すため、Fields の疎な表示除外とは別に出す。
	r.raw(fmt.Sprintf("  %-42s = %s", "language", cfg.DisplayLanguage()))
	for _, f := range config.Fields(cfg) {
		if f.Key == "language" {
			continue
		}
		r.raw(fmt.Sprintf("  %-42s = %s", f.Key, f.Value))
	}
	for _, f := range config.Lists(cfg) {
		r.raw(fmt.Sprintf("  %-42s = %s", f.Key, f.Value))
	}
	return 0
}

func showV2GlobalConfig(cfg, raw config.Config) int {
	path, _ := config.Path()
	fmt.Println("Config:", path)
	for _, scope := range []string{config.V2ScopeSystem, config.V2ScopeWorkspaceDefaults, config.V2ScopeRepositoryDefaults} {
		fmt.Println(v2ScopeTitle(scope) + ":")
		for _, field := range config.V2Fields(cfg, raw, scope, "", "") {
			fmt.Printf("  %-42s = %s (source: %s)\n", field.Key, field.Value, field.Source)
		}
	}
	if len(cfg.Workspaces) > 0 {
		fmt.Printf("  workspaces = %d\n", len(cfg.Workspaces))
	}
	return 0
}

func runScopeConfig(ctx context.Context, scope config.Scope, path string, args []string) int {
	cfg, err := config.Load()
	if err != nil {
		lang := i18n.Normalize(config.LoadLanguage())
		fmt.Fprintln(os.Stderr, i18n.New(string(lang)).Localize("common.error", nil)+":", i18n.LocalizeError(err, lang))
		return 1
	}
	target, err := resolveConfigScope(ctx, cfg, scope, path)
	if err != nil {
		lang := i18n.Normalize(config.LoadLanguage())
		fmt.Fprintln(os.Stderr, i18n.New(string(lang)).Localize("common.error", nil)+":", i18n.LocalizeError(err, lang))
		return 1
	}
	if len(args) == 0 {
		r := newTextRenderer(os.Stdout, localizedUsageLanguage())
		r.line("config.show.scope", map[string]any{"Title": scopeTitle(scope), "Target": target})
		for _, f := range config.ScopeFields(cfg, scope, target) {
			r.raw(fmt.Sprintf("  %-42s = %s (%s)", f.Key, f.Value, r.Localize("config.show.source", map[string]any{"Value": f.Source})))
		}
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
		fmt.Fprintln(os.Stderr, i18n.New(string(lang)).Localize("common.error", nil)+":", i18n.LocalizeError(err, lang))
		return 1
	}
	if err := config.CommitEdit(preview); err != nil {
		lang := i18n.LanguageFromContext(ctx)
		fmt.Fprintln(os.Stderr, i18n.New(string(lang)).Localize("common.error", nil)+":", i18n.LocalizeError(err, lang))
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
