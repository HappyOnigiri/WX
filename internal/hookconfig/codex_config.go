package hookconfig

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// codexPolicyFindings は有効な hooks.json があっても user hook を無効にし得る Codex 設定を調べる。
// 必要な TOML 部分だけを読み、読み取り不能または構造不正の policy も blocking とする。
// 利用者からは hooks.json 側の問題に見えるため、原因が別ファイルにあることを finding で明示する。
func codexPolicyFindings() []Finding {
	home, err := os.UserHomeDir()
	if err != nil {
		return []Finding{{Code: FindingCodexConfigUnusable, Detail: err.Error(), Blocking: true}}
	}
	for _, path := range []string{filepath.Join(home, ".codex", "config.toml"), "/etc/codex/config.toml"} {
		if _, err := regularHookPath(path); errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			return []Finding{{Code: FindingCodexConfigUnusable, Path: path, Detail: err.Error(), Blocking: true}}
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return []Finding{{Code: FindingCodexConfigUnusable, Path: path, Detail: err.Error(), Blocking: true}}
		}
		if len(data) > maxHookConfigSize {
			return []Finding{{Code: FindingCodexConfigUnusable, Path: path, Detail: "the file is larger than the 4MiB limit", Blocking: true}}
		}
		if enabled, parsable := codexHooksConfigState(data); !parsable {
			return []Finding{{Code: FindingCodexConfigUnusable, Path: path, Detail: "wx cannot interpret this TOML, so Codex hooks are treated as disabled", Blocking: true}}
		} else if !enabled {
			return []Finding{{Code: FindingCodexFeatureOff, Path: path, Detail: "set hooks = true under [features] to let Codex run user hooks", Blocking: true}}
		}
	}
	return nil
}

// codexHooksConfigState は config.toml の [features] hooks を読み、有効かと解釈できたかを返す。
// 解釈できない TOML と明示的な無効化は診断上まったく別の原因なので、呼び出し側が区別できるようにする。
func codexHooksConfigState(data []byte) (enabled, parsable bool) {
	table := ""
	depth := 0
	for _, rawLine := range strings.Split(string(data), "\n") {
		line := strings.TrimSpace(stripTOMLComment(rawLine))
		if line == "" {
			continue
		}
		if strings.Contains(line, "\"\"\"") || strings.Contains(line, "'''") {
			// 小さな parser では multiline string を安全に解釈できない。
			return false, false
		}
		if depth > 0 {
			// 前行で開いた array または inline table の継続行である。
			// [features] key にはなれないため、ここでは bracket depth だけを扱う。
			next, rest, ok := scanTOMLValueDepth(line, depth)
			if !ok || rest != "" {
				return false, false
			}
			depth = next
			continue
		}
		if strings.HasPrefix(line, "[[") {
			if !strings.HasSuffix(line, "]]") {
				return false, false
			}
			// array-of-table entry は TOML として有効だが、単一の [features] table にはならない。
			table = "array:" + strings.TrimSpace(line[2:len(line)-2])
			continue
		}
		if strings.HasPrefix(line, "[") {
			if !strings.HasSuffix(line, "]") {
				return false, false
			}
			table = strings.TrimSpace(line[1 : len(line)-1])
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			// TOML に bare statement はない。不明な形は unavailable とし、不正 config が fast path を有効化しないようにする。
			return false, false
		}
		next, rest, ok := scanTOMLValueDepth(value, 0)
		if !ok || rest != "" {
			return false, false
		}
		// bracket を開いたままの value は次行へ続く。
		// 一行で読めない [features] key は unavailable のままとする。
		depth = next
		key = normalizeTOMLKey(key)
		if table == "" {
			key = strings.ReplaceAll(key, " ", "")
			if key == "features" {
				if !inlineTOMLFeatureTableEnabled(value) {
					return false, true
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
			return false, true
		}
	}
	// 閉じない array または inline table は file の切詰めか不正を示す。
	// 読み飛ばした行に [features] key があり得るため unavailable とする。
	return depth == 0, depth == 0
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
