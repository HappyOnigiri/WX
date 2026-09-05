package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"syscall"

	"github.com/HappyOnigiri/WX/internal/config"
	"github.com/HappyOnigiri/WX/internal/discovery"
	"github.com/HappyOnigiri/WX/internal/gitx"
	"github.com/HappyOnigiri/WX/internal/rpc"
	"github.com/HappyOnigiri/WX/internal/tui"
)

type WorktreeOptions struct {
	Force   bool
	Disable bool
	Select  bool
}

// SelectWorktreePolicy は agent を起動せず、現在の workspace の policy を選択して保存する。
func (c Client) SelectWorktreePolicy(ctx context.Context) int {
	if _, err := c.selectWorktreeMode(ctx, WorktreeOptions{Select: true}); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	// 起動している daemon があれば、次の agent 起動を待たずに保存済み設定を反映する。
	if err := c.RPC.Call(ctx, "ReloadConfig", struct{}{}, nil); err != nil && !rpc.IsConnectError(err) {
		fmt.Fprintln(os.Stderr, "error: reload worktree policy:", err)
		return 1
	}
	return 0
}

// RunAgentWithPolicy は daemon の起動前に許可を解決し、対象外なら現在のディレクトリで通常起動する。
func (c Client) RunAgentWithPolicy(ctx context.Context, agent string, args, branches []string, fresh bool, options WorktreeOptions) int {
	if err := validateFreshResume(fresh, isNativeResume(agent, args), ""); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 2
	}
	mode, err := c.selectWorktreeMode(ctx, options)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	if mode == "off" {
		if len(branches) > 0 || fresh {
			fmt.Fprintln(os.Stderr, "error: --branch and --fresh require a worktree")
			return 2
		}
		return runDirectAgent(ctx, agent, args)
	}
	c.forceWorktree = options.Force
	// 保存直後の選択を、既に動いている daemon にも lease より先に反映する。
	if err := c.ensureDaemon(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	if err := c.RPC.Call(ctx, "ReloadConfig", struct{}{}, nil); err != nil {
		fmt.Fprintln(os.Stderr, "error: reload worktree policy:", err)
		return 1
	}
	return c.RunAgent(ctx, agent, args, branches, fresh, "")
}

func (c Client) selectWorktreeMode(ctx context.Context, options WorktreeOptions) (string, error) {
	if options.Disable {
		return "off", nil
	}
	if options.Force {
		return "cold", nil
	}
	cwd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	discoverer := discovery.Discoverer{Git: &gitx.Runner{Timeout: c.Config.Discovery.Timeout.Duration}, Config: c.Config}
	root, err := discoverer.PolicyRoot(ctx, cwd)
	if err != nil {
		return "", err
	}
	mode := c.Config.WorktreeMode(root)
	if !options.Select && mode != "ask" {
		return mode, nil
	}
	if !isTerminal(int(os.Stdin.Fd())) || !isTerminal(int(os.Stderr.Fd())) {
		return "", errors.New("worktree policy requires a terminal; use wx --worktree or wx --no-worktree, or configure worktree.undefined")
	}
	initial := 1
	switch mode {
	case "hot":
		initial = 0
	case "off":
		initial = 2
	}
	mode, err = tui.Select(ctx, os.Stdin, os.Stderr, tui.Selection{
		Title: "Worktree policy", Description: "workspace: " + root, Initial: initial,
		Options: []tui.Option{
			{Value: "hot", Label: "Hot standby", Description: "keep a worktree ready for faster launches"},
			{Value: "cold", Label: "Cold start", Description: "create a worktree when launching an agent"},
			{Value: "off", Label: "No worktree", Description: "run the agent in the current directory"},
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
			return "", fmt.Errorf("policy saved but daemon reload failed: %w", err)
		}
	}
	return mode, nil
}

func runDirectAgent(ctx context.Context, agent string, args []string) int {
	cmd := exec.CommandContext(ctx, agent, args...)
	cmd.Env = childEnvironment(os.Environ(), nil)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	foreground := configureAgentProcess(cmd, int(os.Stdin.Fd()))
	signals := make(chan os.Signal, 4)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(signals)
	if err := cmd.Start(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
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
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return exit.ExitCode()
	}
	fmt.Fprintln(os.Stderr, "error:", err)
	return 1
}
