package cli

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"syscall"

	"github.com/HappyOnigiri/WorktreeX/internal/config"
	"github.com/HappyOnigiri/WorktreeX/internal/discovery"
	"github.com/HappyOnigiri/WorktreeX/internal/gitx"
	"github.com/HappyOnigiri/WorktreeX/internal/i18n"
	"github.com/HappyOnigiri/WorktreeX/internal/rpc"
	"github.com/HappyOnigiri/WorktreeX/internal/tui"
)

type WorktreeOptions struct {
	Force          bool
	Disable        bool
	Select         bool
	SkipOnboarding bool
}

// SelectWorktreePolicy は agent を起動せず、現在の workspace の policy を選択して保存する。
func (c Client) SelectWorktreePolicy(ctx context.Context) int {
	root, rootErr := c.policyRoot(ctx)
	if _, err := c.selectWorktreeMode(ctx, WorktreeOptions{Select: true}, root, rootErr); err != nil {
		cliError(c, err)
		return 1
	}
	// 起動している daemon があれば、次の agent 起動を待たずに保存済み設定を反映する。
	if err := c.RPC.Call(ctx, "ReloadConfig", struct{}{}, nil); err != nil && !rpc.IsConnectError(err) {
		reportStepError(cliLanguage(c), "cli.step.reload_policy", err)
		return 1
	}
	return 0
}

// RunAgentWithPolicy は daemon の起動前に許可を解決し、対象外なら現在のディレクトリで通常起動する。
func (c Client) RunAgentWithPolicy(ctx context.Context, agent string, args, branches []string, fresh bool, options WorktreeOptions) int {
	cwd, err := os.Getwd()
	if err != nil {
		cliError(c, err)
		return 1
	}
	return c.RunAgentWithPolicyFrom(ctx, cwd, agent, args, branches, fresh, options)
}

// RunAgentWithPolicyFrom は TUI が明示した作業元を使い、process 全体の cwd を変更せずに agent を起動する。
func (c Client) RunAgentWithPolicyFrom(ctx context.Context, sourceCWD, agent string, args, branches []string, fresh bool, options WorktreeOptions) int {
	intent := parseResumeIntent(agent, args)
	if fresh && intent.Kind == resumeIntentNone {
		localizer := cliLocalizer(c)
		fmt.Fprintln(os.Stderr, localizer.Localize("cli.error_prefix", nil), localizer.Localize("cli.resume.fresh_required", nil))
		return 2
	}
	// 会話 ID を指定した再開は、起動場所ではなく会話の側で worktree の可否を決める。
	// 記録済み session の復元先は起動場所と無関係で、管理外の会話も当時の workspace の方針に従うのが利用者の期待に近い。
	// worktree の指定を明示した起動はその指定を優先するため、この経路へ入れない。
	if intent.Kind == resumeIntentLookup && !options.Force && !options.Disable && !options.Select {
		return c.runResumeByID(ctx, sourceCWD, agent, args, branches, fresh, intent)
	}
	// workspace root は agent.add_dir の解決キーでもあるため、worktree を作らない経路より先に一度だけ解決する。
	root, rootErr := c.policyRootFrom(ctx, sourceCWD)
	mode, err := c.selectWorktreeMode(ctx, options, root, rootErr)
	if err != nil {
		cliError(c, err)
		return 1
	}
	if mode == "off" {
		if len(branches) > 0 || fresh {
			localizer := cliLocalizer(c)
			fmt.Fprintln(os.Stderr, localizer.Localize("cli.error_prefix", nil), localizer.Localize("cli.resume.branch_needs_worktree", nil))
			return 2
		}
		return runDirectAgentFrom(ctx, sourceCWD, agent, addDirArgs(directAddDirs(c.Config, root), args), c.directHookEnvironment(ctx, sourceCWD, root, rootErr, options))
	}
	c.forceWorktree = options.Force
	// 保存直後の選択を、既に動いている daemon にも lease より先に反映する。
	if err := c.ensureDaemon(ctx); err != nil {
		cliError(c, err)
		return 1
	}
	if err := c.RPC.Call(ctx, "ReloadConfig", struct{}{}, nil); err != nil {
		reportStepError(cliLanguage(c), "cli.step.reload_policy", err)
		return 1
	}
	return c.runAgentResolved(ctx, agent, args, branches, fresh, "", sourceCWD, nil, options.SkipOnboarding)
}

// policyRoot は設定を引くための workspace root を返す。
// リポジトリ内なら main worktree、それ以外は CWD そのものになる。
// 解決に失敗したときは空の root と理由を返し、root を必要としない経路は global 設定のまま進める。
func (c Client) policyRoot(ctx context.Context) (string, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	return c.policyRootFrom(ctx, cwd)
}

func (c Client) policyRootFrom(ctx context.Context, cwd string) (string, error) {
	discoverer := discovery.Discoverer{Git: &gitx.Runner{Timeout: c.Config.System.Discovery.Timeout.Duration}, Config: c.Config}
	return discoverer.PolicyRoot(ctx, cwd)
}

// selectWorktreeMode は worktree の方針を決める。root と rootErr は policyRoot の結果で、
// 方針の決定に root が要る経路だけが rootErr で失敗する。
func (c Client) selectWorktreeMode(ctx context.Context, options WorktreeOptions, root string, rootErr error) (string, error) {
	if options.Disable {
		return "off", nil
	}
	if options.Force {
		return "cold", nil
	}
	if rootErr != nil {
		return "", rootErr
	}
	mode := c.Config.WorktreeMode(root)
	if !options.Select && mode != "ask" {
		return mode, nil
	}
	if !tui.IsTerminal(int(os.Stdin.Fd())) || !tui.IsTerminal(int(os.Stderr.Fd())) {
		return "", i18n.NewError("cli.worktree_needs_terminal", nil)
	}
	initial := 1
	switch mode {
	case "hot":
		initial = 0
	case "off":
		initial = 2
	}
	lang := cliLanguage(c)
	localizer := i18n.New(string(lang))
	mode, err := tui.Select(ctx, os.Stdin, os.Stderr, tui.Selection{
		Title: localizer.Localize("cli.policy.title", nil), Description: "workspace: " + root,
		Initial: initial, ClearOnExit: true, Language: string(lang),
		Options: []tui.Option{
			{Value: "hot", Label: localizer.Localize("cli.policy.hot", nil), Description: localizer.Localize("cli.policy.hot_description", nil)},
			{Value: "cold", Label: localizer.Localize("cli.policy.cold", nil), Description: localizer.Localize("cli.policy.cold_description", nil)},
			{Value: "off", Label: localizer.Localize("cli.policy.off", nil), Description: localizer.Localize("cli.policy.off_description", nil)},
		},
	})
	if err != nil {
		return "", err
	}
	raw, err := config.LoadRaw()
	if err != nil {
		return "", err
	}
	if err := config.SetWorkspaceWorktree(&raw, root, mode); err != nil {
		return "", err
	}
	effective := config.Merge(config.Defaults(), raw)
	if err := config.NormalizePaths(&effective); err != nil {
		return "", err
	}
	if err := config.Validate(&effective); err != nil {
		return "", err
	}
	if err := config.Save(raw); err != nil {
		return "", err
	}
	if mode == "off" {
		if err := c.RPC.Call(ctx, "ReloadConfig", struct{}{}, nil); err != nil && !rpc.IsConnectError(err) {
			return "", i18n.WrapError(err, "cli.policy_reload_failed", map[string]any{"Error": err.Error()})
		}
	}
	return mode, nil
}

// directHookEnvironment は wx -n で起動する agent へ、git worktree add を wx new へ書き換えるための
// 境界と所有者を渡す。保存済み方針が off の起動（-n を付けていない）では何も渡さず、従来どおり素通しさせる。
// 書き換え先の wx new は off / ask の workspace では daemon に拒否されるため、hot / cold でだけ有効にする。
// 境界は policy root ではなく起動元 worktree の toplevel である。policy root は main worktree を指すので、
// linked worktree や wx の slot 内で -n 起動すると cwd の前方一致が常に外れ、無言で素通しになる。
// commentlint:allow-long -- 境界に toplevel を使う理由（policy root だと素通しになる）を残す
func (c Client) directHookEnvironment(ctx context.Context, sourceCWD, root string, rootErr error, options WorktreeOptions) []string {
	// root が解けていないと WorktreeMode はグローバル既定を返す。repository 外で書き換えを有効にすると、
	// 書き換えた先の wx new が daemon 側の discovery で失敗するだけになる。
	if !options.Disable || rootErr != nil {
		return nil
	}
	if mode := c.Config.WorktreeMode(root); mode != "hot" && mode != "cold" {
		return nil
	}
	discoverer := discovery.Discoverer{Git: &gitx.Runner{Timeout: c.Config.System.Discovery.Timeout.Duration}, Config: c.Config}
	toplevel, err := discoverer.Toplevel(ctx, sourceCWD)
	if err != nil || toplevel == "" {
		return nil
	}
	return []string{envDirectRoot + "=" + toplevel, envDirectOwnerPID + "=" + strconv.Itoa(os.Getpid())}
}

func runDirectAgentFrom(ctx context.Context, cwd, agent string, args, env []string) int {
	cmd := exec.CommandContext(ctx, agent, codexNoDaemonArgs(agent, args)...)
	cmd.Dir = cwd
	cmd.Env = childEnvironment(os.Environ(), env)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	foreground := configureAgentProcess(cmd, int(os.Stdin.Fd()))
	signals := make(chan os.Signal, 4)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(signals)
	if err := cmd.Start(); err != nil {
		reportErrorLanguage(i18n.LanguageFromContext(ctx), err)
		return 1
	}
	if foreground {
		defer restoreForeground(int(os.Stdin.Fd()))
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	var err error
	select {
	case err = <-done:
	case sig := <-signals:
		forwardAgentSignal(cmd, sig)
		err = <-done
	}
	if err == nil {
		return 0
	}
	if code, ok := childExitCode(err); ok {
		return code
	}
	reportErrorLanguage(i18n.LanguageFromContext(ctx), err)
	return 1
}
