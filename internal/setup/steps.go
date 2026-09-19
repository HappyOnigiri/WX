package setup

import (
	"context"
	"errors"
	"os"
	"path/filepath"

	"github.com/HappyOnigiri/WX/internal/config"
	"github.com/HappyOnigiri/WX/internal/diag"
	"github.com/HappyOnigiri/WX/internal/gitx"
	"github.com/HappyOnigiri/WX/internal/hookconfig"
	"github.com/HappyOnigiri/WX/internal/i18n"
	"github.com/HappyOnigiri/WX/internal/launchd"
)

const (
	stepPrerequisites = "prerequisites"
	stepWorktreeRoot  = "worktree_root"
	stepShellPath     = "shell_path"
	stepLaunchAgent   = "launch_agent"
	stepHooksClaude   = "hooks.claude"
	stepHooksCodex    = "hooks.codex"
	stepDaemon        = "daemon"
)

// collectPrerequisites は質問しない情報項目で、以降の項目が前提にしている事実を先に見せる。
func collectPrerequisites(ctx context.Context) Step {
	step := Step{
		ID: stepPrerequisites, Title: message("wx.setup.item.prerequisites"), State: StatePresent, Default: ActionKeep,
		Summary: message("setup.summary.prerequisites"), Detail: message("setup.detail.prerequisites"),
	}
	runner := &gitx.Runner{}
	if _, err := runner.Run(ctx, "", "--version"); err != nil {
		step.State = StateUnknown
		step.Reasons = append(step.Reasons, message("setup.reason.git_unavailable", "Error", err.Error()))
	}
	binary, err := hookconfig.ResolveHookBinary()
	if err != nil {
		step.State = StateUnknown
		step.Reasons = append(step.Reasons, message("setup.reason.hook_binary_unresolved", "Error", err.Error()))
	} else {
		step.Desired = binary
		if step.State == StatePresent {
			step.Detail = message("setup.detail.prerequisites_binary", "Binary", binary)
		}
	}
	step.Reasons = append(step.Reasons, developmentBuildReasons(binary)...)
	return step
}

// developmentBuildReasons は、書き込む command と判定基準の wx が食い違う開発 build を警告する。
// この状態で書くと、書いた直後に divergent と表示される。
func developmentBuildReasons(binary string) []i18n.Message {
	running, err := os.Executable()
	if err != nil || binary == "" {
		return nil
	}
	current, err := hookconfig.CurrentExecutable()
	if err != nil {
		return []i18n.Message{message("setup.reason.running_wx_unknown", "Error", err.Error())}
	}
	resolved, err := filepath.EvalSymlinks(binary)
	if err != nil || resolved == current {
		return nil
	}
	return []i18n.Message{message("setup.reason.development_build", "Running", running, "Binary", binary)}
}

// collectWorktreeRoot は system.storage.worktree_root の記載とディレクトリの実体を見る。
// 実効値は展開・symlink 解決を経るため既定リテラルと一致しない。比較は raw 値と Defaults の生文字列で行う。
func collectWorktreeRoot() Step {
	step := Step{ID: stepWorktreeRoot, Title: message("wx.setup.item.worktree_root"), Desired: config.Defaults().System.Storage.WorktreeRoot}
	path, err := config.Path()
	if err != nil {
		return unknownStep(step, message("setup.reason.config_unreadable", "Error", err.Error()))
	}
	step.Target = path
	raw, err := config.LoadRaw()
	if err != nil {
		return unknownStep(step, message("setup.reason.config_unreadable", "Error", err.Error()))
	}
	step.Current = raw.System.Storage.WorktreeRoot
	if step.Current == "" {
		step.Summary = message("setup.summary.worktree_root_unset", "Default", step.Desired)
		step.Detail = message("setup.detail.worktree_root_unset", "Default", step.Desired)
		// 未記載のときは path そのものを決めてもらう。書かずに済ませる skip は既定値での運用と結果が同じなので置かない。
		step.Options, step.Default = []Action{ActionDefault, ActionManual}, ActionDefault
		step.State = StateAbsent
		return step
	}
	step.Desired = step.Current
	expanded, err := config.ExpandHome(step.Current)
	if err != nil {
		return unknownStep(step, message("setup.reason.config_unreadable", "Error", err.Error()))
	}
	step.Summary = message("setup.summary.worktree_root_path", "Path", expanded)
	step.Detail = message("setup.detail.worktree_root_path", "Path", expanded)
	if result, reason := diag.DiagnosticPathMessage(expanded, os.ModeDir, 0o700); result != "ok" {
		step.State = StateDivergent
		step.Reasons = append(step.Reasons, pathProblem(expanded, result, reason))
		step.Options, step.Default = stepOptions(StateDivergent, worktreeRootActions)
		return step
	}
	step.State = StatePresent
	step.Options, step.Default = stepOptions(StatePresent, worktreeRootActions)
	return step
}

// worktreeRootActions は記載済みの状態にだけ使う。config.yaml から key を消す公開 API が無いため remove を持たない。
// 未記載の状態は install/skip ではなく default/manual を出すため、この一覧を通さない。
var worktreeRootActions = []Action{ActionUpdate, ActionKeep}

// applyWorktreeRoot は raw config へ値を書き、ディレクトリを 0o700 で用意する。
// Save には raw を渡す。実効値を渡すと全既定値が config.yaml へ焼き付き、以後の既定変更が届かなくなる。
func applyWorktreeRoot(ctx context.Context, options Options, step Step, _ Action, value string) error {
	if value == "" {
		value = step.Desired
	}
	raw, err := config.LoadRaw()
	if err != nil {
		return err
	}
	if err := config.SetV2Field(&raw, config.V2ScopeSystem, "", "", "storage."+stepWorktreeRoot, value); err != nil {
		return err
	}
	effective := config.Merge(config.Defaults(), raw)
	if err := config.NormalizePaths(&effective); err != nil {
		return err
	}
	if err := config.Validate(&effective); err != nil {
		return err
	}
	// ディレクトリの準備を先に済ませる。Validate は種別も作成可否も見ないため、
	// 既存の通常ファイルを指す入力でも保存だけが成功し、daemon の次回起動まで壊れたままになる。
	expanded, err := config.ExpandHome(value)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(expanded, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(expanded, 0o700); err != nil {
		return err
	}
	if err := config.Save(raw); err != nil {
		return err
	}
	// 起動済みの daemon には保存済み設定を反映する。未起動を正常扱いにするのは呼び出し側のアダプターの責務で、
	// ここで全エラーを捨てると daemon が変更を拒否しても成功と表示され、旧 root が使われ続ける。
	if options.ReloadConfig != nil {
		if err := options.ReloadConfig(ctx); err != nil {
			return i18n.WrapError(err, "setup.error.daemon_rejected_config", map[string]any{"Path": step.Target, "Error": i18n.ErrorValue(err)})
		}
	}
	return nil
}

// collectLaunchAgent は plist の内容と permission の両方を見る。
// CurrentPlistStatus は byte 比較だけで permission を見ないため、setup が keep と言った直後に doctor が NG を返す矛盾が起きる。
func collectLaunchAgent() Step {
	step := Step{
		ID: stepLaunchAgent, Title: message("wx.setup.item.launch_agent"),
		Summary: message("setup.summary.launch_agent"), Detail: message("setup.detail.launch_agent"),
	}
	path, err := launchd.PlistPath()
	if err != nil {
		return unknownStep(step, message("setup.reason.plist_uncomparable", "Error", err.Error()))
	}
	step.Target = path
	if binary, err := launchd.ResolveBinary(); err == nil {
		step.Desired = binary
	}
	status, statusErr := launchd.CurrentPlistStatus()
	if status == launchd.PlistUnknown && errors.Is(statusErr, os.ErrNotExist) {
		step.State = StateAbsent
		step.Options, step.Default = stepOptions(StateAbsent, allActions)
		return step
	}
	if status == launchd.PlistUnknown {
		if statusErr != nil {
			return unknownStep(step, message("setup.reason.plist_uncomparable_error", "Error", statusErr.Error()))
		}
		return unknownStep(step, message("setup.reason.plist_uncomparable"))
	}
	if permission, reason := diag.DiagnosticPathMessage(path, 0, 0o600); permission != "ok" {
		step.State = StateDivergent
		step.Reasons = append(step.Reasons, pathProblem(path, permission, reason))
		step.Options, step.Default = stepOptions(StateDivergent, allActions)
		return step
	}
	if status == launchd.PlistStale {
		step.State = StateDivergent
		step.Reasons = append(step.Reasons, message("setup.reason.plist_stale"))
		step.Options, step.Default = stepOptions(StateDivergent, allActions)
		return step
	}
	step.State = StatePresent
	step.Options, step.Default = stepOptions(StatePresent, allActions)
	return step
}

var allActions = []Action{ActionInstall, ActionUpdate, ActionKeep, ActionRemove, ActionSkip}

func applyLaunchAgent(ctx context.Context, options Options, action Action) error {
	if action == ActionRemove {
		if options.UninstallLaunchAgent == nil {
			return i18n.NewError("setup.error.launch_agent_remove_unavailable", nil)
		}
		return options.UninstallLaunchAgent(ctx)
	}
	if options.InstallLaunchAgent == nil {
		return i18n.NewError("setup.error.launch_agent_install_unavailable", nil)
	}
	return options.InstallLaunchAgent(ctx)
}

// collectHooks は agent hook 設定を見る。判定と書き込みは internal/hookconfig が同じ受理条件で行う。
func collectHooks(agent string) Step {
	step := Step{ID: "hooks." + agent, Title: message("wx.setup.item.hooks", "Agent", agent)}
	if !hookconfig.AgentInstalled(agent) {
		step.State = StateNotApplicable
		step.Default = ActionKeep
		step.Summary = message("setup.summary.hooks_agent_missing", "Agent", agent)
		step.Detail = message("setup.detail.hooks_agent_missing", "Agent", agent)
		return step
	}
	state, err := hookconfig.Inspect(agent)
	if err != nil {
		return unknownStep(step, message("setup.reason.hook_binary_unresolved", "Error", err.Error()))
	}
	step.Target = state.Path
	step.Current = state.Executable
	step.Reasons = state.Messages()
	if binary, err := hookconfig.ResolveHookBinary(); err == nil {
		step.Desired = binary
	}
	events := hookEventNames()
	detail := []any{"Count", len(events), "Events", joinComma(events), "Path", state.Path, "Agent", agent}
	step.Summary = message("setup.summary.hooks_entries", detail...)
	step.Detail = message("setup.detail.hooks_entries", detail...)
	if command, err := hookconfig.HookCommand("SessionStart"); err == nil {
		// 実際の command を出さないと、何が書き込まれるのか選ぶ前に分からない。
		step.Detail = message("setup.detail.hooks_entries_command", append(detail, "Command", command)...)
	}
	switch state.Status {
	case hookconfig.StatusUnsupported:
		step.State = StateNotApplicable
	case hookconfig.StatusBlocked:
		step.State = StateUnknown
	case hookconfig.StatusAbsent:
		step.State = StateAbsent
	case hookconfig.StatusStale:
		step.State = StateDivergent
	case hookconfig.StatusCurrent:
		step.State = StatePresent
	}
	step.Options, step.Default = stepOptions(step.State, allActions)
	return step
}

// hookEventNames は wx が書き込む event 名を、設定ファイルへ書く順で返す。
func hookEventNames() []string {
	names := make([]string, 0, len(hookconfig.Events()))
	for _, event := range hookconfig.Events() {
		names = append(names, event.Name)
	}
	return names
}

func joinComma(values []string) string {
	out := ""
	for index, value := range values {
		if index > 0 {
			out += ", "
		}
		out += value
	}
	return out
}

// applyHooks は hook エントリを書き、書き換えた実体と控えの path を note として返す。
// 控えは wx の状態ディレクトリへ置くので、出力に出さないと利用者は写しの場所を知る手段がない。
func applyHooks(step Step, action Action) (i18n.Message, error) {
	agent := "claude"
	if step.ID == stepHooksCodex {
		agent = "codex"
	}
	if action == ActionRemove {
		result, err := hookconfig.Remove(agent)
		if err != nil {
			return i18n.Message{}, err
		}
		return hookApplyNote(result), nil
	}
	result, err := hookconfig.Install(agent)
	if err != nil {
		return i18n.Message{}, err
	}
	if result.State.Status != hookconfig.StatusCurrent {
		// 受理されない理由は 1 件に絞らない。書き込んだのに受理されない状況では、
		// 残り全ての finding が次に何を直すかの手掛かりになる。
		if reasons := joinMessages(result.State.Messages()); reasons.ID != "" {
			return i18n.Message{}, messageError("setup.error.hooks_not_ready_reasons", "Path", result.Resolved, "Reason", reasons)
		}
		return i18n.Message{}, messageError("setup.error.hooks_not_ready", "Path", result.Resolved)
	}
	return hookApplyNote(result), nil
}

// hookApplyNote は書き換えた実体と控えの path を 1 行にする。何も書かなかったときは空を返す。
func hookApplyNote(result hookconfig.Result) i18n.Message {
	if !result.Changed {
		return i18n.Message{}
	}
	if result.Backup != "" {
		return message("setup.note.wrote_backup", "Path", result.Resolved, "Backup", result.Backup)
	}
	return message("setup.note.wrote", "Path", result.Resolved)
}

// collectDaemon は socket へ接続するだけで満足せず、Status を 1 回呼んで応答の中身まで確かめる。
func collectDaemon(ctx context.Context, options Options) Step {
	step := Step{
		ID: stepDaemon, Title: message("wx.setup.item.daemon"),
		Summary: message("setup.summary.daemon"), Detail: message("setup.detail.daemon"),
	}
	if options.DaemonStatus == nil {
		return unknownStep(step, message("setup.reason.daemon_unqueryable"))
	}
	responding, err := options.DaemonStatus(ctx)
	switch {
	case err != nil:
		step.State = StateDivergent
		step.Reasons = append(step.Reasons, message("setup.reason.daemon_broken", "Error", err.Error()))
		// 応答するが壊れている daemon は起動依頼では直らないので、別 process へ入れ替える restart を出す。
		step.Options, step.Default = []Action{ActionRestart, ActionKeep}, ActionRestart
	case responding:
		step.State = StatePresent
		step.Default = ActionKeep
	default:
		// 未稼働のときは設定を書く install ではなく start を出す。適用は launchd への起動依頼だけである。
		step.State = StateAbsent
		step.Options, step.Default = []Action{ActionStart, ActionSkip}, ActionStart
	}
	return step
}

// applyDaemon は未稼働なら起動し、壊れている daemon は入れ替える。停止は setup の対象にせず wx daemon stop の仕事とする。
func applyDaemon(ctx context.Context, options Options, action Action) error {
	if action == ActionRestart {
		if options.RestartDaemon == nil {
			return i18n.NewError("setup.error.daemon_restart_unavailable", nil)
		}
		return options.RestartDaemon(ctx)
	}
	if options.StartDaemon == nil {
		return i18n.NewError("setup.error.daemon_start_unavailable", nil)
	}
	return options.StartDaemon(ctx)
}

func unknownStep(step Step, reason i18n.Message) Step {
	step.State = StateUnknown
	step.Reasons = append(step.Reasons, reason)
	step.Options, step.Default = stepOptions(StateUnknown, nil)
	return step
}
