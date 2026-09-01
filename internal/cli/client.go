package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/HappyOnigiri/WX/internal/config"
	"github.com/HappyOnigiri/WX/internal/daemon"
	"github.com/HappyOnigiri/WX/internal/launchd"
	"github.com/HappyOnigiri/WX/internal/rpc"
)

type Client struct {
	RPC    rpc.Client
	Config config.Config
}

func New(cfg config.Config) (Client, error) {
	socket, err := config.SocketPath()
	if err != nil {
		return Client{}, err
	}
	return Client{RPC: rpc.Client{Socket: socket, Timeout: 5 * time.Second}, Config: cfg}, nil
}
func (c Client) ensureDaemon(ctx context.Context) error {
	var status map[string]any
	if err := c.RPC.Call(ctx, "Status", struct{}{}, &status); err == nil {
		return nil
	}
	if err := launchd.Kickstart(); err != nil {
		return fmt.Errorf("wx daemon is unavailable (%v); run wx doctor", err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if err := c.RPC.Call(ctx, "Status", struct{}{}, &status); err == nil {
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return errors.New("wx daemon did not become ready; run wx doctor")
}

func (c Client) RunAgent(ctx context.Context, agent string, args, branches []string, fresh bool, explicitResume string) int {
	if err := c.ensureDaemon(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	cwd, err := os.Getwd()
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	native := isNativeResume(agent, args)
	var lease daemon.Lease
	method := "ResolveAndLease"
	params := any(map[string]any{"cwd": cwd, "branches": branches, "agent": agent, "client_pid": os.Getpid()})
	if explicitResume != "" {
		method = "Resume"
		params = map[string]any{"wx_session_id": explicitResume, "agent": agent, "client_pid": os.Getpid()}
	} else if native && !fresh {
		method = "AllocateResumeSlot"
		params = map[string]any{"agent": agent, "client_pid": os.Getpid()}
	}
	if err := c.RPC.Call(ctx, method, params, &lease); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	if agent == "codex" && native {
		args = codexResumeArgs(args)
	}
	env := append(os.Environ(), "WX_SESSION_ID="+lease.SessionID, "WX_SESSION_TOKEN="+lease.Token, "WX_DAEMON_SOCKET="+c.RPC.Socket, "WX_WORKSPACE_ROOT="+lease.Path, "WX_SOURCE_WORKSPACE="+lease.SourceWorkspace, "WX_READINESS_TIMEOUT="+c.Config.Readiness.Timeout.String())
	if native && !fresh && explicitResume == "" {
		env = append(env, "WX_NATIVE_RESUME=1")
	}
	if fresh {
		env = append(env, "WX_FRESH=1")
	}
	// Without global hooks, a normal cold session must not observe an unfinished workspace.
	if !native && !lease.Ready {
		waitCtx, cancel := context.WithTimeout(ctx, c.Config.Readiness.Timeout.Duration)
		err = c.RPC.Call(waitCtx, "WaitReady", map[string]any{"session_id": lease.SessionID, "token": lease.Token, "timeout_ms": int(c.Config.Readiness.Timeout.Milliseconds())}, nil)
		cancel()
		if err != nil {
			fmt.Fprintln(os.Stderr, "error: workspace preparation:", err)
			return 1
		}
	}
	cmd := exec.Command(agent, args...)
	cmd.Dir = lease.Path
	cmd.Env = env
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	signals := make(chan os.Signal, 4)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(signals)
	done := make(chan error, 1)
	if err := cmd.Start(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	go func() { done <- cmd.Wait() }()
	var runErr error
	select {
	case runErr = <-done:
	case sig := <-signals:
		if cmd.Process != nil {
			_ = syscall.Kill(-cmd.Process.Pid, sig.(syscall.Signal))
		}
		runErr = <-done
	}
	releaseCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	_ = c.RPC.Call(releaseCtx, "Release", map[string]any{"session_id": lease.SessionID, "token": lease.Token, "reason": "client-exit"}, nil)
	cancel()
	if runErr == nil {
		return 0
	}
	var exit *exec.ExitError
	if errors.As(runErr, &exit) {
		return exit.ExitCode()
	}
	fmt.Fprintln(os.Stderr, "error:", runErr)
	return 1
}
func isNativeResume(agent string, args []string) bool {
	if agent == "codex" {
		return len(args) > 0 && args[0] == "resume"
	}
	if agent == "claude" {
		for _, a := range args {
			if a == "--resume" || strings.HasPrefix(a, "--resume=") {
				return true
			}
		}
	}
	return false
}
func codexResumeArgs(args []string) []string {
	hasTarget := false
	for _, a := range args[1:] {
		if a == "--last" || a == "--all" || (!strings.HasPrefix(a, "-") && a != "") {
			hasTarget = true
		}
	}
	if hasTarget {
		return args
	}
	return append(args, "--all")
}
