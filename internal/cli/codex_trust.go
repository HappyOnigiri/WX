package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/BurntSushi/toml"

	"github.com/HappyOnigiri/WorktreeX/internal/daemon"
)

const codexTrustConfigMaxSize = 4 << 20

// codexProjectConfig は trust 判定に必要な project 設定だけを受け取る。
// 他の project 設定を wx が解釈する必要はなく、未知の項目は Codex に任せる。
type codexProjectConfig struct {
	TrustLevel string `toml:"trust_level"`
}

type codexTrustConfig struct {
	Projects map[string]codexProjectConfig `toml:"projects"`
}

// codexNoDaemonArgs は Codex の起動を共有 daemon から切り離す。
// trust 設定の継承可否とは独立して適用する。
func codexNoDaemonArgs(agent string, args []string) []string {
	if agent != "codex" {
		return args
	}
	for _, arg := range args {
		if arg == "--" {
			break
		}
		if arg == "--no-daemon" {
			return args
		}
	}
	return append([]string{"--no-daemon"}, args...)
}

// codexTrustArgs は、明示的に trusted とされた source workspace だけを、
// multi-repository の lease root へプロセス限定で継承する argv を返す。
// 設定を読めない場合や利用者が trust の解決方法を指定した場合は、元の argv を返す。
func codexTrustArgs(agent, leaseKind string, lease daemon.Lease, args []string) []string {
	if agent != "codex" || leaseKind != "" || len(lease.RepositoryDirs) == 0 || lease.SourceWorkspace == "" || lease.Path == "" {
		return args
	}
	if codexArgsControlTrust(args) || !codexSourceWorkspaceTrusted(lease.SourceWorkspace) {
		return args
	}
	override, ok := codexTrustInlineOverride(lease.SourceWorkspace, lease.Path)
	if !ok {
		return args
	}
	return append([]string{"-c", override}, args...)
}

// codexSourceWorkspaceTrusted は CODEX_HOME を Codex と同じ優先順位で解決し、
// 通常ファイルかつ上限内の config.toml だけを解析する。判定不能時は継承しない。
func codexSourceWorkspaceTrusted(sourceWorkspace string) bool {
	path, ok := codexConfigPath()
	if !ok {
		return false
	}
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() > codexTrustConfigMaxSize {
		return false
	}
	data, err := os.ReadFile(path)
	if err != nil || len(data) > codexTrustConfigMaxSize {
		return false
	}
	var cfg codexTrustConfig
	if _, err := toml.Decode(string(data), &cfg); err != nil {
		return false
	}
	project, ok := cfg.Projects[sourceWorkspace]
	return ok && project.TrustLevel == "trusted"
}

func codexConfigPath() (string, bool) {
	codexHome := os.Getenv("CODEX_HOME")
	if codexHome == "" {
		home, err := os.UserHomeDir()
		if err != nil || home == "" {
			return "", false
		}
		codexHome = filepath.Join(home, ".codex")
	}
	if filepath.IsAbs(codexHome) {
		return filepath.Join(codexHome, "config.toml"), true
	}
	absolute, err := filepath.Abs(codexHome)
	if err != nil {
		return "", false
	}
	return filepath.Join(absolute, "config.toml"), true
}

// codexTrustInlineOverride は path を文字列連結せず TOML basic string として
// inline table にし、同じ値を parser へ戻して生成結果を検証する。
func codexTrustInlineOverride(sourceWorkspace, leaseRoot string) (string, bool) {
	paths := []string{sourceWorkspace}
	if leaseRoot != sourceWorkspace {
		paths = append(paths, leaseRoot)
	}

	var builder strings.Builder
	builder.WriteString("projects={")
	for index, path := range paths {
		encoded, ok := codexTOMLBasicString(path)
		if !ok {
			return "", false
		}
		if index > 0 {
			builder.WriteByte(',')
		}
		builder.WriteString(encoded)
		builder.WriteString("={trust_level=\"trusted\"}")
	}
	builder.WriteByte('}')
	override := builder.String()

	var parsed codexTrustConfig
	if _, err := toml.Decode(override, &parsed); err != nil {
		return "", false
	}
	if len(parsed.Projects) != len(paths) {
		return "", false
	}
	for _, path := range paths {
		project, ok := parsed.Projects[path]
		if !ok || project.TrustLevel != "trusted" {
			return "", false
		}
	}
	return override, true
}

// codexTOMLBasicString は TOML の basic string を生成する。
// OS path の空白・引用符・バックスラッシュを保持し、制御文字は TOML の escape にする。
func codexTOMLBasicString(value string) (string, bool) {
	if !utf8.ValidString(value) {
		return "", false
	}
	var builder strings.Builder
	builder.WriteByte('"')
	for _, char := range value {
		switch char {
		case '\\':
			builder.WriteString(`\\`)
		case '"':
			builder.WriteString(`\"`)
		case '\b':
			builder.WriteString(`\b`)
		case '\t':
			builder.WriteString(`\t`)
		case '\n':
			builder.WriteString(`\n`)
		case '\f':
			builder.WriteString(`\f`)
		case '\r':
			builder.WriteString(`\r`)
		default:
			if char < 0x20 || (char >= 0x7f && char <= 0x9f) {
				fmt.Fprintf(&builder, `\u%04X`, char)
			} else {
				builder.WriteRune(char)
			}
		}
	}
	builder.WriteByte('"')
	return builder.String(), true
}

// codexArgsControlTrust は利用者の profile または projects 設定を優先するため、
// 追加の process override を抑止する。-- より後ろは prompt なので option として読まない。
func codexArgsControlTrust(args []string) bool {
	for index := 0; index < len(args); index++ {
		arg := args[index]
		if arg == "--" {
			return false
		}
		switch {
		case arg == "-p" || arg == "--profile" || strings.HasPrefix(arg, "-p=") || strings.HasPrefix(arg, "--profile="):
			return true
		case arg == "-c" || arg == "--config":
			if index+1 >= len(args) {
				return false
			}
			index++
			if codexConfigValueControlsProjects(args[index]) {
				return true
			}
		case strings.HasPrefix(arg, "-c="):
			if codexConfigValueControlsProjects(strings.TrimPrefix(arg, "-c=")) {
				return true
			}
		case strings.HasPrefix(arg, "--config="):
			if codexConfigValueControlsProjects(strings.TrimPrefix(arg, "--config=")) {
				return true
			}
		}
	}
	return false
}

// codexConfigValueControlsProjects は key=value の key の第一要素だけを確認する。
// bare/quoted/dotted key のいずれでも projects を先頭に持つ指定を検出する。
func codexConfigValueControlsProjects(value string) bool {
	key, _, ok := strings.Cut(value, "=")
	if !ok {
		return false
	}
	key = strings.TrimSpace(key)
	if key == "" {
		return false
	}
	if key[0] == '"' || key[0] == '\'' {
		quote := key[0]
		quoted := key[1:]
		end := strings.IndexByte(quoted, quote)
		if end == -1 {
			return false
		}
		key = quoted[:end]
	} else if dot := strings.IndexByte(key, '.'); dot != -1 {
		key = key[:dot]
	}
	return strings.TrimSpace(key) == "projects"
}
