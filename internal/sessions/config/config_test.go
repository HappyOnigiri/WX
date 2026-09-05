package config

import "testing"

func TestDefaultsAndSessionPaths(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cfg := Defaults()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("defaults are invalid: %v", err)
	}
	if got := cfg.SessionPaths("claude"); len(got) != 1 || got[0] != "~/.claude/projects" {
		t.Fatalf("Claude sessions = %v", got)
	}
	if got := cfg.SessionPaths("codex"); len(got) != 1 || got[0] != "~/.codex/sessions" {
		t.Fatalf("Codex sessions = %v", got)
	}
	if got := cfg.SessionPaths("cursor"); got != nil {
		t.Fatalf("Cursor sessions = %v, want nil", got)
	}
}

func TestZeroAndNilConfigUsePathDefaults(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	var zero Config
	if got := zero.SessionPaths("claude"); len(got) != 1 || got[0] != "~/.claude/projects" {
		t.Fatalf("zero Claude sessions = %v", got)
	}
	if err := zero.Validate(); err != nil {
		t.Fatalf("zero config validation failed: %v", err)
	}

	var nilConfig *Config
	if got := nilConfig.SessionPaths("codex"); len(got) != 1 || got[0] != "~/.codex/sessions" {
		t.Fatalf("nil Codex sessions = %v", got)
	}
	if err := nilConfig.Validate(); err == nil {
		t.Fatal("nil config validation unexpectedly succeeded")
	}
}

func TestCustomPathsAndUnsupportedTools(t *testing.T) {
	cfg := Config{Paths: PathsConfig{Claude: ToolPathsConfig{Sessions: []string{"~/custom"}}}}
	if got := cfg.SessionPaths("claude"); len(got) != 1 || got[0] != "~/custom" {
		t.Fatalf("custom Claude sessions = %v", got)
	}
	if got := cfg.SessionPaths("codex"); len(got) != 1 || got[0] != "~/.codex/sessions" {
		t.Fatalf("unset Codex sessions = %v", got)
	}
	if got := cfg.SessionPaths("copilot"); got != nil {
		t.Fatalf("Copilot sessions = %v, want nil", got)
	}
}
