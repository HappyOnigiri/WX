// Package config は会話の再開に使う履歴パス設定を提供する。
package config

import (
	"fmt"
)

// Config は wx 設定の sessions セクションを保持する。
type Config struct {
	Paths PathsConfig `yaml:"paths,omitempty"`
}

// PathsConfig は対応ツールごとのセッション走査先を保持する。
type PathsConfig struct {
	Claude ToolPathsConfig `yaml:"claude,omitempty"`
	Codex  ToolPathsConfig `yaml:"codex,omitempty"`
}

// ToolPathsConfig は一つのツールのセッション走査先を保持する。
type ToolPathsConfig struct {
	Sessions []string `yaml:"sessions,omitempty"`
}

// Defaults は sessions セクションの既定値を返す。
func Defaults() Config {
	return Config{
		Paths: PathsConfig{
			Claude: ToolPathsConfig{Sessions: []string{"~/.claude/projects"}},
			Codex:  ToolPathsConfig{Sessions: []string{"~/.codex/sessions"}},
		},
	}
}

// Validate は sessions 設定の値を検証する。
// 履歴パスは読み取り時に展開し、設定ではユーザー表記を保つ。
func (c *Config) Validate() error {
	if c == nil {
		return fmt.Errorf("sessions config is nil")
	}
	return nil
}

// SessionPaths はツールのセッション走査先を返す。
func (c *Config) SessionPaths(tool string) []string {
	if c == nil {
		return Defaults().toolPaths(tool).Sessions
	}
	paths := c.toolPaths(tool).Sessions
	if len(paths) > 0 {
		return paths
	}
	return Defaults().toolPaths(tool).Sessions
}

func (c Config) toolPaths(tool string) ToolPathsConfig {
	switch tool {
	case "claude":
		return c.Paths.Claude
	case "codex":
		return c.Paths.Codex
	default:
		return ToolPathsConfig{}
	}
}
