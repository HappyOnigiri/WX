// Package hookconfig evaluates whether the configured agent hooks provide the
// synchronous readiness contract required by wx.
package hookconfig

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"unicode"
)

// Available reports whether the effective hook configuration for agent has a
// valid synchronous wx readiness hook for every required event. Any missing,
// malformed, disabled, ambiguous, or unsafe configuration is unavailable.
func Available(agent string) bool {
	paths, ok := readinessHookPaths(agent)
	if !ok {
		return false
	}
	if agent == "codex" && !codexHooksEnabled() {
		return false
	}
	executable, err := CurrentExecutable()
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

// CodexHooksConfigEnabled evaluates the small subset of TOML policy that can
// disable user hooks. It is exported for the CLI's focused parser tests.
func CodexHooksConfigEnabled(data []byte, requirement bool) bool {
	return codexHooksConfigEnabled(data, requirement)
}

// CurrentExecutable returns the canonical executable identity of the running
// wx process.
func CurrentExecutable() (string, error) {
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

// IsExactWXHookCommand reports whether command is the current wx executable's
// synchronous hook invocation for event.
func IsExactWXHookCommand(command, event string) bool {
	executable, err := CurrentExecutable()
	return err == nil && isExactWXHookCommandForExecutable(command, event, executable)
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

func isExactWXHookCommandForExecutable(command, event, executable string) bool {
	fields, ok := splitHookCommand(command)
	if !ok || len(fields) != 3 || fields[0].literalExpansion || fields[1].value != "hook" || fields[2].value != event {
		return false
	}
	commandExecutable, ok := resolveHookExecutable(fields[0].value)
	if !ok {
		return false
	}
	return sameExecutable(commandExecutable, executable)
}

type hookCommandField struct {
	value            string
	literalExpansion bool
}

func splitHookCommand(command string) ([]hookCommandField, bool) {
	var fields []hookCommandField
	var field strings.Builder
	var quote byte
	escaped := false
	started := false
	literalExpansion := false
	for i := 0; i < len(command); i++ {
		char := command[i]
		// Newlines and carriage returns are shell command separators, not
		// ordinary argument whitespace. Reject them even inside quotes so a
		// multiline command can never be mistaken for a synchronous wx hook.
		if char == '\n' || char == '\r' {
			return nil, false
		}
		if escaped {
			if (char == '~' && field.Len() == 0) || (char == '$' && strings.HasPrefix(command[i:], "$HOME")) {
				literalExpansion = true
			}
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
			if quote == '"' && char == '`' {
				// Backticks are command substitution in double quotes. Do not
				// classify a command whose executable depends on shell execution.
				return nil, false
			}
			if (char == '~' && field.Len() == 0) || (quote == '\'' && char == '$' && strings.HasPrefix(command[i:], "$HOME")) {
				literalExpansion = true
			}
			if quote == '"' && char == '\\' {
				if i+1 >= len(command) || !strings.ContainsRune("$`\"\\", rune(command[i+1])) {
					// Inside double quotes, backslash only escapes $, `, ", \\,
					// or a newline. Treating other escapes as removed would
					// mistake a non-existent shell path for the wx executable.
					return nil, false
				}
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
				fields = append(fields, hookCommandField{value: field.String(), literalExpansion: literalExpansion})
				field.Reset()
				started = false
				literalExpansion = false
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
		fields = append(fields, hookCommandField{value: field.String(), literalExpansion: literalExpansion})
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

func sameExecutable(left, right string) bool {
	leftInfo, leftErr := os.Stat(left)
	rightInfo, rightErr := os.Stat(right)
	return leftErr == nil && rightErr == nil && os.SameFile(leftInfo, rightInfo)
}
