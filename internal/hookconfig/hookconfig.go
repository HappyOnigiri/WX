// Package hookconfig は設定済み agent hook が wx に必要な同期 readiness 契約を満たすか判定する。
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

// Available は agent の有効 hook 設定が必須 event すべてに有効な同期 wx readiness hook を持つか返す。
// 欠落、不正、無効化、曖昧、安全でない設定は unavailable とする。
func Available(agent string) bool {
	path, ok := readinessHookPaths(agent)
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
	data, readErr := os.ReadFile(path)
	if readErr != nil || len(data) == 0 || len(data) > 4<<20 {
		return false
	}
	return readinessHookDocumentMatches(data, required, executable)
}

// CurrentExecutable は実行中 wx process の canonical executable identity を返す。
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

// codexHooksEnabled は有効な hooks.json があっても user hook を無効にし得る Codex 設定を調べる。
// 必要な TOML 部分だけを読み、読み取り不能または構造不正の policy は unavailable とする。
func codexHooksEnabled() bool {
	home, err := os.UserHomeDir()
	if err != nil {
		return false
	}
	for _, path := range []string{filepath.Join(home, ".codex", "config.toml"), "/etc/codex/config.toml"} {
		if _, err := regularHookPath(path); errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			return false
		}
		data, err := os.ReadFile(path)
		if err != nil || len(data) > 4<<20 || !codexHooksConfigEnabled(data) {
			return false
		}
	}
	return true
}

func codexHooksConfigEnabled(data []byte) bool {
	table := ""
	depth := 0
	for _, rawLine := range strings.Split(string(data), "\n") {
		line := strings.TrimSpace(stripTOMLComment(rawLine))
		if line == "" {
			continue
		}
		if strings.Contains(line, "\"\"\"") || strings.Contains(line, "'''") {
			// 小さな parser では multiline string を安全に解釈できない。
			return false
		}
		if depth > 0 {
			// 前行で開いた array または inline table の継続行である。
			// [features] key にはなれないため、ここでは bracket depth だけを扱う。
			next, rest, ok := scanTOMLValueDepth(line, depth)
			if !ok || rest != "" {
				return false
			}
			depth = next
			continue
		}
		if strings.HasPrefix(line, "[[") {
			if !strings.HasSuffix(line, "]]") {
				return false
			}
			// array-of-table entry は TOML として有効だが、単一の [features] table にはならない。
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
			// TOML に bare statement はない。不明な形は unavailable とし、不正 config が fast path を有効化しないようにする。
			return false
		}
		next, rest, ok := scanTOMLValueDepth(value, 0)
		if !ok || rest != "" {
			return false
		}
		// bracket を開いたままの value は次行へ続く。
		// 一行で読めない [features] key は unavailable のままとする。
		depth = next
		key = normalizeTOMLKey(key)
		if table == "" {
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
	// 閉じない array または inline table は file の切詰めか不正を示す。
	// 読み飛ばした行に [features] key があり得るため unavailable とする。
	return depth == 0
}

// scanTOMLValueDepth は value の1行を走査し、末尾で開いている bracket depth を返す。
// rest は閉じた bracket 後の文字列で、quote または bracket が不均衡なら ok は false となる。
func scanTOMLValueDepth(line string, depth int) (int, string, bool) {
	var quote byte
	for index := 0; index < len(line); index++ {
		char := line[index]
		if quote != 0 {
			if char == quote && (quote != '"' || index == 0 || line[index-1] != '\\') {
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
				return 0, "", false
			}
			depth--
			if depth == 0 {
				return 0, strings.TrimSpace(line[index+1:]), true
			}
		}
	}
	if quote != 0 {
		return 0, "", false
	}
	return depth, "", true
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

func readinessHookPaths(agent string) (string, bool) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", false
	}
	switch agent {
	case "codex":
		path, err := regularHookPath(filepath.Join(home, ".codex", "hooks.json"))
		return path, err == nil
	case "claude":
		local := filepath.Join(home, ".claude", "settings.local.json")
		if _, err := regularHookPath(local); err == nil {
			// Claude の local settings は settings.json より優先される。
			// 有効な hook set が曖昧になるため、二つの file を merge しない。
			return local, true
		} else if !errors.Is(err, os.ErrNotExist) {
			return "", false
		}
		path, err := regularHookPath(filepath.Join(home, ".claude", "settings.json"))
		return path, err == nil
	default:
		return "", false
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
		// newline と carriage return は通常の引数空白ではなく shell command separator である。
		// quote 内でも拒否し、multiline command を同期 wx hook と誤認させない。
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
				// double quote 内の backtick は command substitution である。
				// executable が shell 実行に依存する command は分類しない。
				return nil, false
			}
			if (char == '~' && field.Len() == 0) || (quote == '\'' && char == '$' && strings.HasPrefix(command[i:], "$HOME")) {
				literalExpansion = true
			}
			if quote == '"' && char == '\\' {
				if i+1 >= len(command) || !strings.ContainsRune("$`\"\\", rune(command[i+1])) {
					// double quote 内で backslash が escape できるのは $、`、"、\\、newline だけである。
					// 他の escape を除去すると、存在しない shell path を wx executable と誤認する。
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
