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
	"strings"
	"syscall"
	"time"

	"github.com/HappyOnigiri/WX/internal/config"
	"github.com/HappyOnigiri/WX/internal/daemon"
	"github.com/HappyOnigiri/WX/internal/domain"
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
	} else if native && !fresh {
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
	if agent == "codex" && native {
		args = codexResumeArgs(args)
	}
	env := append(os.Environ(), "WX_SESSION_ID="+lease.SessionID, "WX_SESSION_TOKEN="+lease.Token, "WX_DAEMON_SOCKET="+c.RPC.Socket, "WX_WORKSPACE_ROOT="+lease.Path, "WX_SOURCE_WORKSPACE="+lease.SourceWorkspace, "WX_READINESS_TIMEOUT="+c.Config.Readiness.Timeout.String())
	if native && !fresh && explicitResume == "" {
		env = append(env, "WX_NATIVE_RESUME=1")
	}
	if explicitResume != "" {
		env = append(env, "WX_EXPLICIT_RESUME=1")
	}
	if fresh {
		env = append(env, "WX_FRESH=1")
	}
	if recoveryDiscarded {
		env = append(env, "WX_RECOVERY_DISCARDED=1")
	}
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
	heartbeatDone := make(chan struct{})
	go func() {
		ticker := time.NewTicker(15 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				heartbeatCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
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
		if cmd.Process != nil {
			_ = syscall.Kill(-cmd.Process.Pid, sig.(syscall.Signal))
		}
		runErr = <-done
	}
	close(heartbeatDone)
	releaseCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	_ = c.RPC.CallWithKey(releaseCtx, "Release", "release:"+lease.SessionID+":client-exit", map[string]any{"session_id": lease.SessionID, "token": lease.Token, "reason": "client-exit"}, nil)
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

func readinessHooksAvailable(agent string) bool {
	home, err := os.UserHomeDir()
	if err != nil {
		return false
	}
	var paths []string
	if agent == "codex" {
		paths = []string{filepath.Join(home, ".codex", "hooks.json")}
	} else {
		paths = []string{filepath.Join(home, ".claude", "settings.json"), filepath.Join(home, ".claude", "settings.local.json")}
	}
	required := map[string]string{
		"SessionStart":     "session-start",
		"UserPromptSubmit": "user-prompt-submit",
		"PreToolUse":       "pre-tool-use",
	}
	found := map[string]bool{}
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil || len(data) > 4<<20 {
			continue
		}
		var document struct {
			Hooks map[string]any `json:"hooks"`
		}
		if json.Unmarshal(data, &document) != nil {
			continue
		}
		for event, command := range required {
			if hookTreeContainsCommand(document.Hooks[event], command) {
				found[event] = true
			}
		}
	}
	for event := range required {
		if !found[event] {
			return false
		}
	}
	return true
}

func hookTreeContainsCommand(value any, event string) bool {
	switch typed := value.(type) {
	case []any:
		for _, child := range typed {
			if hookTreeContainsCommand(child, event) {
				return true
			}
		}
	case map[string]any:
		if disabled, _ := typed["disabled"].(bool); disabled {
			return false
		}
		if command, ok := typed["command"].(string); ok {
			typeName, _ := typed["type"].(string)
			if (typeName == "" || typeName == "command") && isExactWXHookCommand(command, event) {
				return true
			}
		}
		for key, child := range typed {
			if key != "command" && hookTreeContainsCommand(child, event) {
				return true
			}
		}
	}
	return false
}

func isExactWXHookCommand(command, event string) bool {
	fields := strings.Fields(command)
	if len(fields) != 3 {
		return false
	}
	executable := strings.Trim(fields[0], `"'`)
	return filepath.Base(executable) == "wx" && fields[1] == "hook" && fields[2] == event
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
