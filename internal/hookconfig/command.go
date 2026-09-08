package hookconfig

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"unicode"
)

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

// ResolveHookBinary は hook command に書く wx executable の絶対パスを返す。
// PATH 解決に頼る `wx` や `~` / `$HOME` は書かない。hook を実行するのは agent の PATH と展開規則であり、
// GUI 起動の agent では解決に失敗しても Available は true のままになり、未準備 worktree で agent が動き出す。
func ResolveHookBinary() (string, error) {
	binary, err := exec.LookPath("wx")
	if err != nil {
		// os.Executable へ退避すると、開発 build や go test の一時 binary を利用者の dotfile へ焼き付けてしまう。
		return "", errors.New("wx is not on PATH; install it to ~/.local/bin and add that directory to PATH first")
	}
	absolute, err := filepath.Abs(binary)
	if err != nil {
		return "", err
	}
	if _, ok := resolveHookExecutable(absolute); !ok {
		return "", fmt.Errorf("%s is not a regular executable", absolute)
	}
	return absolute, nil
}

// HookCommand は event に対して設定ファイルへ書く command 文字列を返す。
// 受理条件はちょうど 3 token の `<絶対パス> hook <subcommand>` なので、
// quote や shell 演算子を含む executable path は書かずに拒否する。
func HookCommand(event string) (string, error) {
	binary, err := ResolveHookBinary()
	if err != nil {
		return "", err
	}
	return hookCommandFor(binary, event)
}

func hookCommandFor(binary, event string) (string, error) {
	subcommand := ""
	for _, candidate := range Events() {
		if candidate.Name == event {
			subcommand = candidate.Subcommand
		}
	}
	if subcommand == "" {
		return "", fmt.Errorf("unknown hook event %q", event)
	}
	command := binary + " hook " + subcommand
	fields, ok := splitHookCommand(command)
	if !ok || len(fields) != 3 || fields[0].value != binary {
		return "", fmt.Errorf("the wx executable path cannot be written as a hook command: %s", binary)
	}
	return command, nil
}
