package setup

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/HappyOnigiri/WX/internal/config"
	"github.com/HappyOnigiri/WX/internal/diag"
	"github.com/HappyOnigiri/WX/internal/gitx"
	"github.com/HappyOnigiri/WX/internal/hookconfig"
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
	step := Step{ID: stepPrerequisites, Title: "Prerequisites", State: StatePresent, Default: ActionKeep, Detail: "checked without changing anything"}
	runner := &gitx.Runner{}
	if _, err := runner.Run(ctx, "", "--version"); err != nil {
		step.State = StateUnknown
		step.Reasons = append(step.Reasons, "git is unavailable: "+err.Error())
	}
	binary, err := hookconfig.ResolveHookBinary()
	if err != nil {
		step.State = StateUnknown
		step.Reasons = append(step.Reasons, err.Error())
	} else {
		step.Desired = binary
	}
	step.Reasons = append(step.Reasons, developmentBuildReasons(binary)...)
	return step
}

// developmentBuildReasons は、書き込む command と判定基準の wx が食い違う開発 build を警告する。
// この状態で書くと、書いた直後に divergent と表示される。
func developmentBuildReasons(binary string) []string {
	running, err := os.Executable()
	if err != nil || binary == "" {
		return nil
	}
	current, err := hookconfig.CurrentExecutable()
	if err != nil {
		return []string{"the running wx cannot be identified: " + err.Error()}
	}
	resolved, err := filepath.EvalSymlinks(binary)
	if err != nil || resolved == current {
		return nil
	}
	return []string{fmt.Sprintf("this wx runs from %s but hooks would name %s; install wx first so both agree", running, binary)}
}

// collectWorktreeRoot は storage.worktree_root の記載とディレクトリの実体を見る。
// 実効値は展開・symlink 解決を経るため既定リテラルと一致しない。比較は raw 値と Defaults の生文字列で行う。
func collectWorktreeRoot() Step {
	step := Step{ID: stepWorktreeRoot, Title: "Worktree root", Desired: config.Defaults().Storage.WorktreeRoot}
	path, err := config.Path()
	if err != nil {
		return unknownStep(step, err.Error())
	}
	step.Target = path
	raw, err := config.LoadRaw()
	if err != nil {
		return unknownStep(step, err.Error())
	}
	step.Current = raw.Storage.WorktreeRoot
	if step.Current == "" {
		step.Detail = "storage.worktree_root is not written; wx would use " + step.Desired
		step.Options, step.Default = stepOptions(StateAbsent, worktreeRootActions)
		step.State = StateAbsent
		return step
	}
	step.Desired = step.Current
	expanded, err := config.ExpandHome(step.Current)
	if err != nil {
		return unknownStep(step, err.Error())
	}
	step.Detail = expanded
	if result := diag.DiagnosticPath(expanded, os.ModeDir, 0o700); result != "ok" {
		step.State = StateDivergent
		step.Reasons = append(step.Reasons, expanded+": "+result)
		step.Options, step.Default = stepOptions(StateDivergent, worktreeRootActions)
		return step
	}
	step.State = StatePresent
	step.Options, step.Default = stepOptions(StatePresent, worktreeRootActions)
	return step
}

// worktreeRootActions は config.yaml から key を消す公開 API が無いため remove を持たない。
var worktreeRootActions = []Action{ActionInstall, ActionUpdate, ActionKeep, ActionSkip}

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
	if err := config.SetField(&raw, "storage."+stepWorktreeRoot, value); err != nil {
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
			return fmt.Errorf("%s was saved but the running daemon rejected it: %w", step.Target, err)
		}
	}
	return nil
}

// collectLaunchAgent は plist の内容と permission の両方を見る。
// CurrentPlistStatus は byte 比較だけで permission を見ないため、setup が keep と言った直後に doctor が NG を返す矛盾が起きる。
func collectLaunchAgent() Step {
	step := Step{ID: stepLaunchAgent, Title: "LaunchAgent", Detail: "starts the wx daemon at login"}
	path, err := launchd.PlistPath()
	if err != nil {
		return unknownStep(step, err.Error())
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
		reason := "unable to compare the LaunchAgent plist"
		if statusErr != nil {
			reason += ": " + statusErr.Error()
		}
		return unknownStep(step, reason)
	}
	if permission := diag.DiagnosticPath(path, 0, 0o600); permission != "ok" {
		step.State = StateDivergent
		step.Reasons = append(step.Reasons, path+": "+permission)
		step.Options, step.Default = stepOptions(StateDivergent, allActions)
		return step
	}
	if status == launchd.PlistStale {
		step.State = StateDivergent
		step.Reasons = append(step.Reasons, "the installed plist does not match what this wx would write")
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
			return errors.New("removing the LaunchAgent is not available here")
		}
		return options.UninstallLaunchAgent(ctx)
	}
	if options.InstallLaunchAgent == nil {
		return errors.New("installing the LaunchAgent is not available here")
	}
	return options.InstallLaunchAgent(ctx)
}

// collectHooks は agent hook 設定を見る。判定と書き込みは internal/hookconfig が同じ受理条件で行う。
func collectHooks(agent string) Step {
	step := Step{ID: "hooks." + agent, Title: "Agent hooks (" + agent + ")"}
	if !hookconfig.AgentInstalled(agent) {
		step.State = StateNotApplicable
		step.Default = ActionKeep
		step.Detail = agent + " is not on PATH, so wx does not configure its hooks"
		return step
	}
	state, err := hookconfig.Inspect(agent)
	if err != nil {
		return unknownStep(step, err.Error())
	}
	step.Target = state.Path
	step.Current = state.Executable
	step.Reasons = state.Reasons()
	if binary, err := hookconfig.ResolveHookBinary(); err == nil {
		step.Desired = binary
	}
	step.Detail = fmt.Sprintf("%s in %s", hookEventSummary(), state.Path)
	if command, err := hookconfig.HookCommand("SessionStart"); err == nil {
		// 選択肢の説明に実際の command を出さないと、何が書き込まれるのか選ぶ前に分からない。
		step.Detail += ", such as " + command
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

// hookEventSummary は書き込む event 名を 1 行にする。
func hookEventSummary() string {
	names := make([]string, 0, len(hookconfig.Events()))
	for _, event := range hookconfig.Events() {
		names = append(names, event.Name)
	}
	return fmt.Sprintf("%d wx entries (%s)", len(names), joinComma(names))
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
func applyHooks(step Step, action Action) (string, error) {
	agent := "claude"
	if step.ID == stepHooksCodex {
		agent = "codex"
	}
	if action == ActionRemove {
		result, err := hookconfig.Remove(agent)
		if err != nil {
			return "", err
		}
		return hookApplyNote(result), nil
	}
	result, err := hookconfig.Install(agent)
	if err != nil {
		return "", err
	}
	if result.State.Status != hookconfig.StatusCurrent {
		return "", fmt.Errorf("%s was written but the readiness contract is still not satisfied: %v", result.Resolved, result.State.Reasons())
	}
	return hookApplyNote(result), nil
}

// hookApplyNote は書き換えた実体と控えの path を 1 行にする。何も書かなかったときは空を返す。
func hookApplyNote(result hookconfig.Result) string {
	if !result.Changed {
		return ""
	}
	note := "wrote " + result.Resolved
	if result.Backup != "" {
		note += "; backup at " + result.Backup
	}
	return note
}

// collectDaemon は socket へ接続するだけで満足せず、Status を 1 回呼んで応答の中身まで確かめる。
func collectDaemon(ctx context.Context, options Options) Step {
	step := Step{ID: stepDaemon, Title: "Daemon", Detail: "wx daemon answers the local socket"}
	if options.DaemonStatus == nil {
		return unknownStep(step, "the daemon cannot be queried here")
	}
	responding, err := options.DaemonStatus(ctx)
	switch {
	case err != nil:
		step.State = StateDivergent
		step.Reasons = append(step.Reasons, "the daemon answered but the request failed: "+err.Error())
	case responding:
		step.State = StatePresent
	default:
		step.State = StateAbsent
	}
	step.Options, step.Default = stepOptions(step.State, daemonActions)
	return step
}

// daemonActions は daemon の停止を setup の対象にしない。停止は wx daemon stop の仕事である。
var daemonActions = []Action{ActionInstall, ActionUpdate, ActionKeep, ActionSkip}

func applyDaemon(ctx context.Context, options Options, _ Action) error {
	if options.StartDaemon == nil {
		return errors.New("starting the daemon is not available here")
	}
	return options.StartDaemon(ctx)
}

func unknownStep(step Step, reason string) Step {
	step.State = StateUnknown
	step.Reasons = append(step.Reasons, reason)
	step.Options, step.Default = stepOptions(StateUnknown, nil)
	return step
}
