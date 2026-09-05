package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/HappyOnigiri/WX/internal/config"
	"github.com/HappyOnigiri/WX/internal/daemon"
	"github.com/HappyOnigiri/WX/internal/domain"
	"github.com/HappyOnigiri/WX/internal/fdexec"
	"github.com/HappyOnigiri/WX/internal/hookconfig"
	"github.com/HappyOnigiri/WX/internal/launchd"
	"github.com/HappyOnigiri/WX/internal/rpc"
)

type Client struct {
	RPC           rpc.Client
	Config        config.Config
	forceWorktree bool

	// beforeAgentStart は lease directory descriptor を開いた後に lexical root を置換する test 用 barrier。
	// production client では nil のままにし、子 process は fdexec 経由で起動する。
	beforeAgentStart func()
}

// defaultDiscoveryBudget は config.Defaults().Discovery.Timeout と同じ値。
// config.Load を経ない呼び出しでも discovery RPC を固定の短い client timeout に落とさないための下限である。
const defaultDiscoveryBudget = 30 * time.Second

// discoveryTimeout は repository discovery を行う RPC の制限時間を返す。
// daemon の discovery.timeout に余裕を足し、予定どおり進む大規模 root の探索を client 側で中断しない。
func (c Client) discoveryTimeout() time.Duration {
	budget := c.Config.Discovery.Timeout.Duration
	if budget <= 0 {
		budget = defaultDiscoveryBudget
	}
	return budget + 10*time.Second
}

func New(cfg config.Config) (Client, error) {
	socket, err := config.SocketPath()
	if err != nil {
		return Client{}, err
	}
	// この client の RPC は再起動をまたぐ lease、agent 登録、heartbeat、release に使う。
	// ConnectRetry は送信前の接続失敗だけを再試行するため、実行中の要求は重複しない。予算は hook と同じ 2 秒である。
	return Client{RPC: rpc.Client{Socket: socket, Timeout: 5 * time.Second, ConnectRetry: 2 * time.Second}, Config: cfg}, nil
}

// ensureDaemon は daemon への接続を確認し、未待受時だけ launchd で起動する。
// 大きな root の Status は遅延し得るため、応答遅延だけで kickstart すると他 session を処理中の daemon を終了させる。
func (c Client) ensureDaemon(ctx context.Context) error {
	var status map[string]any
	statusCtx, cancel := context.WithTimeout(ctx, c.discoveryTimeout())
	err := c.RPC.Call(statusCtx, "Status", struct{}{}, &status)
	cancel()
	if err == nil {
		return nil
	}
	if !rpc.IsConnectError(err) {
		return fmt.Errorf("wx daemon is reachable but this request did not complete (%w); refusing to restart a socket that may still be serving other sessions, run wx doctor", err)
	}
	if err := launchd.Kickstart(ctx); err != nil {
		return daemonRecoveryError("wx daemon is unavailable", err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if err := c.RPC.Call(ctx, "Status", struct{}{}, &status); err == nil {
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return daemonRecoveryError("wx daemon did not become ready", nil)
}

// daemonRecoveryError は通常は doctor を案内し、現在の executable と plist が不一致なら daemon install を案内する。
// plist を検査できなくても、元の接続失敗を別の失敗に置き換えない。
func daemonRecoveryError(prefix string, cause error) error {
	hint := "run wx doctor"
	if status, err := launchd.CurrentPlistStatus(); err == nil && status == launchd.PlistStale {
		hint = "LaunchAgent plist is stale; run wx daemon install"
	}
	if cause != nil {
		return fmt.Errorf("%s (%w); %s", prefix, cause, hint)
	}
	return errors.New(prefix + "; " + hint)
}

func (c Client) runAgent(ctx context.Context, agent string, args, branches []string, fresh bool, explicitResume string) int {
	return c.runAgentFrom(ctx, agent, args, branches, fresh, explicitResume, "")
}

func (c Client) runAgentFrom(ctx context.Context, agent string, args, branches []string, fresh bool, explicitResume, sourceCWD string) int {
	if err := c.ensureDaemon(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	cwd, err := os.Getwd()
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	if sourceCWD != "" {
		cwd = sourceCWD
	}
	intent := parseResumeIntent(agent, args)
	if err := validateResumeOptions(intent, explicitResume, fresh, branches); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 2
	}
	target, resuming, err := c.resolveResume(ctx, agent, cwd, intent, explicitResume)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	recoveryDiscarded := fresh
	var lease daemon.Lease
	method := "ResolveAndLease"
	params := any(map[string]any{"cwd": cwd, "branches": branches, "agent": agent, "client_pid": os.Getpid(), "force_worktree": c.forceWorktree})
	if resuming {
		if target.WXSessionID != "" {
			var status resumeStatus
			if err := c.RPC.Call(ctx, "ResumeStatus", map[string]string{"wx_session_id": target.WXSessionID}, &status); err != nil {
				fmt.Fprintln(os.Stderr, "error:", err)
				return 1
			}
			if agent == "" {
				agent = status.Agent
			}
			target.Agent = agent
			if target.AgentSessionID == "" {
				target.AgentSessionID = status.AgentSessionID
			}
			if !fresh && status.Expired {
				if !confirmExpiredResume(target.WXSessionID) {
					fmt.Fprintln(os.Stderr, "resume cancelled; no workspace was created")
					return 1
				}
				fresh = true
				recoveryDiscarded = true
			}
			method = "Resume"
			params = map[string]any{"wx_session_id": target.WXSessionID, "agent": agent, "client_pid": os.Getpid(), "agent_session_id": target.AgentSessionID, "fresh": fresh, "branches": branches}
		} else {
			if target.CWD == "" {
				fmt.Fprintln(os.Stderr, "error: selected conversation has no working directory")
				return 1
			}
			params = map[string]any{"cwd": target.CWD, "branches": branches, "agent": agent, "client_pid": os.Getpid(), "force_worktree": c.forceWorktree}
		}
	}
	hooksReady := hookconfig.Available(agent)
	operationKey, err := domain.NewID()
	if err != nil {
		fmt.Fprintln(os.Stderr, "error: create operation identity:", err)
		return 1
	}
	// ResolveAndLease と Resume は daemon 側で repository discovery を同期実行する。
	// cold な複数 repository root でも、discovery.timeout 内の探索を client 側の既定 timeout で中断しない。
	budget := c.discoveryTimeout()
	if method == "Resume" && c.Config.Readiness.Timeout.Duration > 0 {
		budget = c.Config.Readiness.Timeout.Duration
	}
	leaseCtx, cancelLease := context.WithTimeout(ctx, budget)
	defer cancelLease()
	if err := c.RPC.CallWithKey(leaseCtx, method, "launch:"+operationKey, params, &lease); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	defer func() {
		releaseCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = c.RPC.CallWithKey(releaseCtx, "Release", "release:"+lease.SessionID+":client-exit", map[string]any{"session_id": lease.SessionID, "token": lease.Token, "reason": "client-exit"}, nil)
	}()
	// wx clear --all の終了要求は heartbeat と agent 登録の応答で届く。
	// 要求を受けたら agent の process group へ一度だけ SIGTERM を送り、停止を確認してから daemon へ応答する。
	terminator := &agentTerminator{}
	heartbeatDone := make(chan struct{})
	go c.watchTermination(ctx, lease, terminator, heartbeatDone)
	defer close(heartbeatDone)
	defer terminator.confirm(c, lease)
	if resuming {
		rest := intent.Rest
		if explicitResume != "" {
			rest = args
		}
		args = resumeArgs(agent, target.AgentSessionID, lease.Path, rest)
	}
	envOverrides := []string{"WX_SESSION_ID=" + lease.SessionID, "WX_SESSION_TOKEN=" + lease.Token, "WX_DAEMON_SOCKET=" + c.RPC.Socket, "WX_WORKSPACE_ROOT=" + lease.Path, "WX_SOURCE_WORKSPACE=" + lease.SourceWorkspace, "WX_READINESS_TIMEOUT=" + c.Config.Readiness.Timeout.String(), "WX_SOURCE_CWD=" + cwd}
	if recoveryDiscarded {
		envOverrides = append(envOverrides, "WX_RECOVERY_DISCARDED=1")
	}
	env := childEnvironment(os.Environ(), envOverrides)
	// 通常起動は hook があれば preparation と重ねる。
	// 会話の再開は復元と ID の移譲を完了してから agent を起動する。
	if !lease.Ready && (resuming || !hooksReady) {
		waitCtx, cancel := context.WithTimeout(ctx, c.Config.Readiness.Timeout.Duration)
		err = c.RPC.Call(waitCtx, "WaitReady", map[string]any{"session_id": lease.SessionID, "token": lease.Token, "timeout_ms": int(c.Config.Readiness.Timeout.Milliseconds())}, nil)
		cancel()
		if err != nil {
			fmt.Fprintln(os.Stderr, "error: workspace preparation:", err)
			return 1
		}
	}
	// 起動前・準備待ちの間に終了要求が届いていたら、agent を起動せずにそのまま応答する。
	if terminator.requested() {
		fmt.Fprintln(os.Stderr, "wx clear asked this session to stop before the agent started")
		return 1
	}
	leaseDirectory, err := openLeaseDirectory(c.Config, lease)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error: pin workspace CWD:", err)
		return 1
	}
	defer func() { _ = leaseDirectory.Close() }()
	if c.beforeAgentStart != nil {
		c.beforeAgentStart()
	}
	// descriptor-bound trampoline は子 process の exec(2) 直前に fchdir(2) する。
	// lexical wx root が rename・symlink・実体置換されても agent CWD は lease inode を指す。
	helper, helperErr := os.Executable()
	if helperErr != nil {
		fmt.Fprintln(os.Stderr, "error: locate wx descriptor helper:", helperErr)
		return 1
	}
	cmd, err := fdexec.Start(ctx, helper, leaseDirectory, env, append([]string{agent}, args...)...)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error: prepare agent:", err)
		return 1
	}
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	foreground := configureAgentProcess(cmd, int(os.Stdin.Fd()))
	signals := make(chan os.Signal, 4)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(signals)
	done := make(chan error, 1)
	if err := cmd.Start(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	if foreground {
		defer restoreForeground(int(os.Stdin.Fd()))
	}
	registerCtx, registerCancel := context.WithTimeout(context.Background(), 2*time.Second)
	var registered terminationEnvelope
	registerErr := c.RPC.CallWithKey(registerCtx, "RegisterAgentProcess", "agent-process:"+lease.SessionID+":"+strconv.Itoa(cmd.Process.Pid), map[string]any{"session_id": lease.SessionID, "token": lease.Token, "agent_pid": cmd.Process.Pid}, &registered)
	registerCancel()
	if registerErr != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		fmt.Fprintln(os.Stderr, "error: register agent process:", registerErr)
		return 1
	}
	terminator.adopt(cmd)
	terminator.request(registered.Terminate)
	go func() { done <- cmd.Wait() }()
	var runErr error
	select {
	case runErr = <-done:
	case sig := <-signals:
		forwardAgentSignal(cmd, sig)
		runErr = <-done
	}
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

func configureAgentProcess(cmd *exec.Cmd, ttyFD int) bool {
	foreground := isTerminal(ttyFD)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Foreground: foreground, Ctty: ttyFD}
	return foreground
}

func forwardAgentSignal(cmd *exec.Cmd, sig os.Signal) {
	if cmd.Process == nil {
		return
	}
	unixSignal, ok := sig.(syscall.Signal)
	if !ok || syscall.Kill(-cmd.Process.Pid, unixSignal) != nil {
		_ = cmd.Process.Signal(sig)
	}
}

func restoreForeground(ttyFD int) {
	// TIOCSPGRP は background process group に SIGTTOU を送り、wx 自身を停止させ得る。
	// signal.Ignore は Reset 後も残るため使わず、Notify/Stop で ioctl 中だけ受信し、channel は読まない。
	sigttou := make(chan os.Signal, 1)
	signal.Notify(sigttou, syscall.SIGTTOU)
	_ = unix.IoctlSetPointerInt(ttyFD, unix.TIOCSPGRP, syscall.Getpgrp())
	signal.Stop(sigttou)
}

func openLeaseDirectory(cfg config.Config, lease daemon.Lease) (*os.File, error) {
	root, err := config.ExpandHome(cfg.Storage.WorktreeRoot)
	if err != nil {
		return nil, err
	}
	if !domain.IsWithin(root, lease.Path) && lease.RootIdentity == "" {
		// 旧 test/in-process RPC handler は daemon の durable inode identity を持たない。
		// この互換経路も symlink 成分を拒否し、Darwin の /tmp alias を canonicalize 後に root descriptor 経由で開く。
		canonical, err := domain.Canonicalize(lease.Path)
		if err != nil {
			return nil, err
		}
		volumeRoot := filepath.VolumeName(string(canonical)) + string(filepath.Separator)
		directory, _, err := domain.OpenOwnedDirectory(volumeRoot, string(canonical))
		if err != nil {
			return nil, err
		}
		return directory, nil
	}
	directory, identity, err := domain.OpenOwnedDirectory(root, lease.Path)
	if err != nil {
		return nil, err
	}
	if lease.RootIdentity != "" && identity != lease.RootIdentity {
		_ = directory.Close()
		return nil, fmt.Errorf("lease root identity changed (expected %s, got %s)", lease.RootIdentity, identity)
	}
	return directory, nil
}

var wxChildEnvironmentKeys = map[string]struct{}{
	"WX_SESSION_ID":         {},
	"WX_SESSION_TOKEN":      {},
	"WX_DAEMON_SOCKET":      {},
	"WX_WORKSPACE_ROOT":     {},
	"WX_SOURCE_WORKSPACE":   {},
	"WX_READINESS_TIMEOUT":  {},
	"WX_SOURCE_CWD":         {},
	"WX_RECOVERY_DISCARDED": {},
}

func childEnvironment(base, overrides []string) []string {
	env := make([]string, 0, len(base)+len(overrides))
	for _, entry := range base {
		key, _, ok := strings.Cut(entry, "=")
		if ok {
			if _, internal := wxChildEnvironmentKeys[key]; internal {
				continue
			}
		}
		env = append(env, entry)
	}
	return append(env, overrides...)
}

func confirmExpiredResume(sessionID string) bool {
	info, err := os.Stdin.Stat()
	if err != nil || info.Mode()&os.ModeCharDevice == 0 {
		fmt.Fprintf(os.Stderr, "wx session %s has no recovery snapshot; refusing fresh-base resume without an interactive confirmation\n", sessionID)
		return false
	}
	fmt.Fprintf(os.Stderr, "wx session %s has no recovery snapshot. Continue the conversation in a new workspace from the current base? [y/N] ", sessionID)
	answer, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && len(answer) == 0 {
		return false
	}
	answer = strings.ToLower(strings.TrimSpace(answer))
	return answer == "y" || answer == "yes"
}
