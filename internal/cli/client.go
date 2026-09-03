package cli

import (
	"bufio"
	"context"
	"encoding/json"
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
	RPC    rpc.Client
	Config config.Config

	// beforeAgentStart is a deterministic test barrier used to replace the
	// lexical root after the lease directory descriptor is opened. Production
	// clients leave it nil; the child still starts through fdexec below.
	beforeAgentStart func()
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
	if err := launchd.Kickstart(ctx); err != nil {
		return fmt.Errorf("wx daemon is unavailable (%w); run wx doctor", err)
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
	if err := validateFreshResume(fresh, native, explicitResume); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 2
	}
	recoveryDiscarded := false
	var lease daemon.Lease
	method := "ResolveAndLease"
	params := any(map[string]any{"cwd": cwd, "branches": branches, "agent": agent, "client_pid": os.Getpid()})
	if explicitResume != "" {
		var status struct {
			Expired bool `json:"expired"`
		}
		if err := c.RPC.Call(ctx, "ResumeStatus", map[string]string{"wx_session_id": explicitResume}, &status); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			return 1
		}
		if status.Expired {
			if !confirmExpiredResume(explicitResume) {
				fmt.Fprintln(os.Stderr, "resume cancelled; no workspace was created")
				return 1
			}
			recoveryDiscarded = true
		}
		method = "Resume"
		params = map[string]any{"wx_session_id": explicitResume, "agent": agent, "client_pid": os.Getpid(), "allow_fresh": recoveryDiscarded}
	} else if native {
		method = "AllocateResumeSlot"
		params = map[string]any{"agent": agent, "client_pid": os.Getpid()}
	}
	hooksReady := readinessHooksAvailable(agent)
	if native && explicitResume == "" && !hooksReady {
		fmt.Fprintln(os.Stderr, "error: native resume requires global wx SessionStart, UserPromptSubmit, and PreToolUse hooks; use wx resume <wx-session-id> for the safe foreground fallback")
		return 1
	}
	operationKey, err := domain.NewID()
	if err != nil {
		fmt.Fprintln(os.Stderr, "error: create operation identity:", err)
		return 1
	}
	if err := c.RPC.CallWithKey(ctx, method, "launch:"+operationKey, params, &lease); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	defer func() {
		releaseCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = c.RPC.CallWithKey(releaseCtx, "Release", "release:"+lease.SessionID+":client-exit", map[string]any{"session_id": lease.SessionID, "token": lease.Token, "reason": "client-exit"}, nil)
	}()
	if agent == "codex" && native {
		args = codexResumeArgs(args)
	}
	encodedBranches, marshalErr := json.Marshal(branches)
	if marshalErr != nil {
		fmt.Fprintln(os.Stderr, "error: encode branch selection:", marshalErr)
		return 1
	}
	envOverrides := []string{"WX_SESSION_ID=" + lease.SessionID, "WX_SESSION_TOKEN=" + lease.Token, "WX_DAEMON_SOCKET=" + c.RPC.Socket, "WX_WORKSPACE_ROOT=" + lease.Path, "WX_SOURCE_WORKSPACE=" + lease.SourceWorkspace, "WX_READINESS_TIMEOUT=" + c.Config.Readiness.Timeout.String(), "WX_SOURCE_CWD=" + cwd, "WX_BRANCHES_JSON=" + string(encodedBranches)}
	if native && explicitResume == "" {
		envOverrides = append(envOverrides, "WX_NATIVE_RESUME=1")
	}
	if explicitResume != "" {
		envOverrides = append(envOverrides, "WX_EXPLICIT_RESUME=1")
	}
	if fresh {
		envOverrides = append(envOverrides, "WX_FRESH=1")
	}
	if recoveryDiscarded {
		envOverrides = append(envOverrides, "WX_RECOVERY_DISCARDED=1")
	}
	env := childEnvironment(os.Environ(), envOverrides)
	// When hooks are installed, preparation overlaps agent startup and prompt entry.
	// Otherwise normal starts and explicit resumes safely wait in the foreground.
	if !lease.Ready && !hooksReady {
		waitCtx, cancel := context.WithTimeout(ctx, c.Config.Readiness.Timeout.Duration)
		err = c.RPC.Call(waitCtx, "WaitReady", map[string]any{"session_id": lease.SessionID, "token": lease.Token, "timeout_ms": int(c.Config.Readiness.Timeout.Milliseconds())}, nil)
		cancel()
		if err != nil {
			fmt.Fprintln(os.Stderr, "error: workspace preparation:", err)
			return 1
		}
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
	// The descriptor-bound trampoline calls fchdir(2) in the child immediately
	// before exec(2). This keeps agent CWD on the lease inode across a rename or
	// symlink/physical replacement of the lexical wx root.
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
	registerErr := c.RPC.CallWithKey(registerCtx, "RegisterAgentProcess", "agent-process:"+lease.SessionID+":"+strconv.Itoa(cmd.Process.Pid), map[string]any{"session_id": lease.SessionID, "token": lease.Token, "agent_pid": cmd.Process.Pid}, nil)
	registerCancel()
	if registerErr != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		fmt.Fprintln(os.Stderr, "error: register agent process:", registerErr)
		return 1
	}
	heartbeatDone := make(chan struct{})
	go func() {
		ticker := time.NewTicker(15 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				heartbeatCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
				_ = c.RPC.Call(heartbeatCtx, "Heartbeat", map[string]string{"session_id": lease.SessionID, "token": lease.Token}, nil)
				cancel()
			case <-heartbeatDone:
				return
			}
		}
	}()
	go func() { done <- cmd.Wait() }()
	var runErr error
	select {
	case runErr = <-done:
	case sig := <-signals:
		forwardAgentSignal(cmd, sig)
		runErr = <-done
	}
	close(heartbeatDone)
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

func validateFreshResume(fresh, native bool, explicitResume string) error {
	if fresh && (!native || explicitResume != "") {
		return errors.New("--fresh is only valid for an agent-native resume")
	}
	return nil
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
	signal.Ignore(syscall.SIGTTOU)
	_ = unix.IoctlSetPointerInt(ttyFD, unix.TIOCSPGRP, syscall.Getpgrp())
	signal.Reset(syscall.SIGTTOU)
}

func openLeaseDirectory(cfg config.Config, lease daemon.Lease) (*os.File, error) {
	root, err := config.ExpandHome(cfg.Storage.WorktreeRoot)
	if err != nil {
		return nil, err
	}
	if !domain.IsWithin(root, lease.Path) && lease.RootIdentity == "" {
		// Test-only/in-process RPC handlers from older callers do not carry the
		// daemon's durable inode identity. Keep that compatibility path physical
		// and fail closed on symlink components. Canonicalize first so Darwin's
		// /tmp -> /private/tmp alias is not rejected as an unsafe component, then
		// open through the filesystem-root descriptor. Daemon-issued leases always
		// carry RootIdentity and never take this compatibility branch.
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
	"WX_BRANCHES_JSON":      {},
	"WX_NATIVE_RESUME":      {},
	"WX_EXPLICIT_RESUME":    {},
	"WX_FRESH":              {},
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

func readinessHooksAvailable(agent string) bool {
	return hookconfig.Available(agent)
}

func codexHooksConfigEnabled(data []byte, requirement bool) bool {
	return hookconfig.CodexHooksConfigEnabled(data, requirement)
}

func currentWXExecutable() (string, error) {
	return hookconfig.CurrentExecutable()
}

func isExactWXHookCommand(command, event string) bool {
	return hookconfig.IsExactWXHookCommand(command, event)
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
