package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode"

	"golang.org/x/sys/unix"

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
	if fresh && (!native || explicitResume != "") {
		fmt.Fprintln(os.Stderr, "error: --fresh is only valid for an agent-native resume")
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
	// #nosec G702 -- agent is selected by the fixed claude/codex CLI subcommands;
	// arguments are passed directly to exec without shell interpretation.
	cmd := exec.CommandContext(ctx, agent, args...)
	cmd.Dir = lease.Path
	cmd.Env = env
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
	paths, ok := readinessHookPaths(agent)
	if !ok {
		return false
	}
	if agent == "codex" && !codexHooksEnabled() {
		return false
	}
	executable, err := currentWXExecutable()
	if err != nil {
		return false
	}
	required := map[string]string{
		"SessionStart":     "session-start",
		"UserPromptSubmit": "user-prompt-submit",
		"PreToolUse":       "pre-tool-use",
	}
	for _, path := range paths {
		data, readErr := os.ReadFile(path)
		if readErr != nil || len(data) == 0 || len(data) > 4<<20 {
			return false
		}
		if readinessHookDocumentMatches(data, required, executable) {
			return true
		}
		return false
	}
	return false
}

// codexHooksEnabled checks local Codex feature and managed-hook policy files
// which can disable user hooks, including an otherwise valid hooks.json. The
// detector only needs this small, stable part of TOML; unreadable or
// structurally malformed policy is treated as unavailable.
func codexHooksEnabled() bool {
	home, err := os.UserHomeDir()
	if err != nil {
		return false
	}
	configs := []struct {
		path        string
		requirement bool
	}{
		{path: filepath.Join(home, ".codex", "config.toml")},
		{path: "/etc/codex/config.toml"},
		{path: "/etc/codex/requirements.toml", requirement: true},
		{path: "/etc/codex/managed_config.toml", requirement: true},
	}
	for _, config := range configs {
		if _, err := regularHookPath(config.path); errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			return false
		}
		data, err := os.ReadFile(config.path)
		if err != nil || len(data) > 4<<20 || !codexHooksConfigEnabled(data, config.requirement) {
			return false
		}
	}
	return true
}

func codexHooksConfigEnabled(data []byte, requirement bool) bool {
	table := ""
	for _, rawLine := range strings.Split(string(data), "\n") {
		line := strings.TrimSpace(stripTOMLComment(rawLine))
		if line == "" {
			continue
		}
		if strings.Contains(line, "\"\"\"") || strings.Contains(line, "'''") {
			// A small parser cannot safely reason about multiline strings.
			return false
		}
		if strings.HasPrefix(line, "[[") {
			if !strings.HasSuffix(line, "]]") {
				return false
			}
			// Array-of-table entries are valid TOML (and common for hooks),
			// but never represent the singular [features] table.
			table = "array:" + strings.TrimSpace(line[2:len(line)-2])
			continue
		}
		if strings.HasPrefix(line, "[") {
			if !strings.HasSuffix(line, "]") {
				return false
			}
			table = strings.TrimSpace(line[1 : len(line)-1])
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			// TOML permits no bare statements. Treat an unrecognised shape as
			// unavailable so a malformed config cannot enable the fast path.
			return false
		}
		key = normalizeTOMLKey(key)
		if table == "" {
			if requirement && key == "allow_managed_hooks_only" {
				value = strings.TrimSpace(value)
				if value != "false" {
					return false
				}
				continue
			}
			key = strings.ReplaceAll(key, " ", "")
			if key == "features" {
				if !inlineTOMLFeatureTableEnabled(value) {
					return false
				}
				continue
			}
			if key != "features.hooks" && key != "features.codex_hooks" {
				continue
			}
		} else if table != "features" || (key != "hooks" && key != "codex_hooks") {
			continue
		}
		value = strings.TrimSpace(value)
		if value != "true" {
			return false
		}
	}
	return true
}

func inlineTOMLFeatureTableEnabled(raw string) bool {
	raw = strings.TrimSpace(raw)
	if len(raw) < 2 || raw[0] != '{' || raw[len(raw)-1] != '}' {
		return false
	}
	fields, ok := splitTOMLInlineFields(raw[1 : len(raw)-1])
	if !ok {
		return false
	}
	for _, field := range fields {
		key, value, ok := strings.Cut(field, "=")
		if !ok {
			return false
		}
		key = normalizeTOMLKey(key)
		if key != "hooks" && key != "codex_hooks" {
			continue
		}
		if strings.TrimSpace(value) != "true" {
			return false
		}
	}
	return true
}

func splitTOMLInlineFields(raw string) ([]string, bool) {
	var fields []string
	start := 0
	var quote byte
	depth := 0
	for index := 0; index < len(raw); index++ {
		char := raw[index]
		if quote != 0 {
			if char == quote && (quote != '"' || index == 0 || raw[index-1] != '\\') {
				quote = 0
			}
			continue
		}
		switch char {
		case '\'', '"':
			quote = char
		case '{', '[':
			depth++
		case '}', ']':
			if depth == 0 {
				return nil, false
			}
			depth--
		case ',':
			if depth == 0 {
				fields = append(fields, strings.TrimSpace(raw[start:index]))
				start = index + 1
			}
		}
	}
	if quote != 0 || depth != 0 {
		return nil, false
	}
	if tail := strings.TrimSpace(raw[start:]); tail != "" {
		fields = append(fields, tail)
	}
	return fields, true
}

func normalizeTOMLKey(key string) string {
	key = strings.TrimSpace(key)
	if len(key) >= 2 && ((key[0] == '"' && key[len(key)-1] == '"') || (key[0] == '\'' && key[len(key)-1] == '\'')) {
		return key[1 : len(key)-1]
	}
	return key
}

func stripTOMLComment(line string) string {
	var quote byte
	for index := 0; index < len(line); index++ {
		char := line[index]
		if quote != 0 {
			if char == quote && (quote != '"' || index == 0 || line[index-1] != '\\') {
				quote = 0
			}
			continue
		}
		if char == '\'' || char == '"' {
			quote = char
			continue
		}
		if char == '#' {
			return line[:index]
		}
	}
	return line
}

func readinessHookPaths(agent string) ([]string, bool) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, false
	}
	switch agent {
	case "codex":
		path, err := regularHookPath(filepath.Join(home, ".codex", "hooks.json"))
		return []string{path}, err == nil
	case "claude":
		local := filepath.Join(home, ".claude", "settings.local.json")
		if _, err := regularHookPath(local); err == nil {
			// Claude's local settings have higher precedence than settings.json.
			// Do not merge two files when the effective hook set is ambiguous.
			return []string{local}, true
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, false
		}
		path, err := regularHookPath(filepath.Join(home, ".claude", "settings.json"))
		return []string{path}, err == nil
	default:
		return nil, false
	}
}

func regularHookPath(path string) (string, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return "", fmt.Errorf("hook configuration is not a regular file: %s", path)
	}
	return path, nil
}

type readinessHookGroup struct {
	Matcher  json.RawMessage        `json:"matcher"`
	Disabled json.RawMessage        `json:"disabled"`
	Hooks    []readinessHookCommand `json:"hooks"`
}

type readinessHookCommand struct {
	Type                   string          `json:"type"`
	Command                string          `json:"command"`
	Disabled               json.RawMessage `json:"disabled"`
	Async                  json.RawMessage `json:"async"`
	Once                   json.RawMessage `json:"once"`
	Timeout                json.RawMessage `json:"timeout"`
	StatusMessage          json.RawMessage `json:"statusMessage"`
	AdditionalContextLimit json.RawMessage `json:"additionalContextLimit"`
}

func readinessHookDocumentMatches(data []byte, required map[string]string, executable string) bool {
	var document map[string]json.RawMessage
	if decodeJSON(data, &document) != nil {
		return false
	}
	if disabled, valid := boolOption(document["disableAllHooks"]); !valid || disabled {
		return false
	}
	hooksRaw, ok := document["hooks"]
	if !ok {
		return false
	}
	var hooks map[string]json.RawMessage
	if decodeJSON(hooksRaw, &hooks) != nil {
		return false
	}
	for event, command := range required {
		eventRaw, ok := hooks[event]
		if !ok {
			return false
		}
		var groups []readinessHookGroup
		if decodeStrictJSON(eventRaw, &groups) != nil || !readinessHookGroupsMatch(groups, command, event, executable) {
			return false
		}
	}
	return true
}

func readinessHookGroupsMatch(groups []readinessHookGroup, command, event, executable string) bool {
	for _, group := range groups {
		if !readinessHookGroupValid(group) {
			continue
		}
		for _, hook := range group.Hooks {
			if hook.Type != "command" {
				continue
			}
			disabled, _ := boolOption(hook.Disabled)
			if disabled {
				continue
			}
			async, _ := boolOption(hook.Async)
			once, _ := boolOption(hook.Once)
			if async || once {
				continue
			}
			if isExactWXHookCommandForExecutable(hook.Command, command, executable) {
				return true
			}
		}
	}
	return false
}

func readinessHookGroupValid(group readinessHookGroup) bool {
	disabled, valid := boolOption(group.Disabled)
	if !valid || disabled {
		return false
	}
	if !matcherAppliesToEveryEvent(group.Matcher) || len(group.Hooks) == 0 {
		return false
	}
	for _, hook := range group.Hooks {
		if !readinessHookCommandValid(hook) {
			return false
		}
	}
	return true
}

func readinessHookCommandValid(hook readinessHookCommand) bool {
	if hook.Type != "command" && hook.Type != "prompt" && hook.Type != "agent" {
		return false
	}
	if hook.Type == "command" && hook.Command == "" {
		return false
	}
	for _, raw := range []json.RawMessage{hook.Disabled, hook.Async, hook.Once} {
		if _, valid := boolOption(raw); !valid {
			return false
		}
	}
	if !optionalNumberValid(hook.Timeout) || !optionalStringValid(hook.StatusMessage) || !optionalIntegerValid(hook.AdditionalContextLimit) {
		return false
	}
	return true
}

func matcherAppliesToEveryEvent(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return true
	}
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return false
	}
	var matcher string
	if json.Unmarshal(raw, &matcher) != nil {
		return false
	}
	switch matcher {
	case "", "*", ".*", "^.*$", "^.+$":
		return true
	default:
		return false
	}
}

func boolOption(raw json.RawMessage) (bool, bool) {
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return false, len(raw) == 0
	}
	var value bool
	if err := json.Unmarshal(raw, &value); err != nil {
		return false, false
	}
	return value, true
}

func optionalStringValid(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return true
	}
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return false
	}
	var value string
	return json.Unmarshal(raw, &value) == nil
}

func optionalNumberValid(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return true
	}
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return false
	}
	var value float64
	return json.Unmarshal(raw, &value) == nil
}

func optionalIntegerValid(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return true
	}
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return false
	}
	var value int
	return json.Unmarshal(raw, &value) == nil
}

func decodeJSON(data []byte, value any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := decoder.Decode(value); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("multiple JSON values")
		}
		return err
	}
	return nil
}

func decodeStrictJSON(data []byte, value any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("multiple JSON values")
		}
		return err
	}
	return nil
}

func isExactWXHookCommand(command, event string) bool {
	executable, err := currentWXExecutable()
	return err == nil && isExactWXHookCommandForExecutable(command, event, executable)
}

func isExactWXHookCommandForExecutable(command, event, executable string) bool {
	fields, ok := splitHookCommand(command)
	if !ok || len(fields) != 3 || fields[1] != "hook" || fields[2] != event {
		return false
	}
	// A quoted variable or tilde is literal to the shell. Reject it instead of
	// treating the token as an expansion and claiming a gate exists.
	if strings.ContainsAny(command, "'\"") && (strings.Contains(fields[0], "$HOME") || strings.HasPrefix(fields[0], "~")) {
		return false
	}
	commandExecutable, ok := resolveHookExecutable(fields[0])
	if !ok {
		return false
	}
	return sameExecutable(commandExecutable, executable)
}

func splitHookCommand(command string) ([]string, bool) {
	var fields []string
	var field strings.Builder
	var quote byte
	escaped := false
	started := false
	for i := 0; i < len(command); i++ {
		char := command[i]
		// Newlines and carriage returns are shell command separators, not
		// ordinary argument whitespace. Reject them even inside quotes so a
		// multiline command can never be mistaken for a synchronous wx hook.
		if char == '\n' || char == '\r' {
			return nil, false
		}
		if escaped {
			field.WriteByte(char)
			escaped = false
			started = true
			continue
		}
		if quote != 0 {
			if char == quote {
				quote = 0
				continue
			}
			if quote == '"' && char == '\\' {
				escaped = true
				continue
			}
			field.WriteByte(char)
			started = true
			continue
		}
		switch {
		case char == '\\':
			escaped = true
			started = true
		case char == '\'' || char == '"':
			quote = char
			started = true
		case unicode.IsSpace(rune(char)):
			if started {
				fields = append(fields, field.String())
				field.Reset()
				started = false
			}
		case strings.ContainsRune("|;&><`", rune(char)):
			return nil, false
		default:
			field.WriteByte(char)
			started = true
		}
	}
	if quote != 0 || escaped {
		return nil, false
	}
	if started {
		fields = append(fields, field.String())
	}
	return fields, true
}

func resolveHookExecutable(value string) (string, bool) {
	if value == "wx" {
		path, err := exec.LookPath(value)
		if err != nil {
			return "", false
		}
		value = path
	} else {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", false
		}
		switch {
		case value == "$HOME":
			value = home
		case strings.HasPrefix(value, "$HOME/"):
			value = filepath.Join(home, strings.TrimPrefix(value, "$HOME/"))
		case value == "~":
			value = home
		case strings.HasPrefix(value, "~/"):
			value = filepath.Join(home, strings.TrimPrefix(value, "~/"))
		}
		if !filepath.IsAbs(value) {
			return "", false
		}
	}
	canonical, err := filepath.EvalSymlinks(value)
	if err != nil {
		return "", false
	}
	info, err := os.Stat(canonical)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&0o111 == 0 {
		return "", false
	}
	return canonical, true
}

func currentWXExecutable() (string, error) {
	path, err := os.Executable()
	if err != nil {
		return "", err
	}
	canonical, ok := resolveHookExecutable(path)
	if !ok {
		return "", errors.New("current wx executable is not a regular executable")
	}
	return canonical, nil
}

func sameExecutable(left, right string) bool {
	leftInfo, leftErr := os.Stat(left)
	rightInfo, rightErr := os.Stat(right)
	return leftErr == nil && rightErr == nil && os.SameFile(leftInfo, rightInfo)
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
