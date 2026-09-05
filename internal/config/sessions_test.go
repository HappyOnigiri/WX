package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestSessionsNestedPresenceAndListMerge(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	path, err := Path()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	document := "sessions:\n" +
		"  paths:\n    claude:\n      sessions: []\n    codex:\n      sessions:\n        - ~/custom-codex\n" +
		"discovery:\n  exclude: []\n"
	if err := os.WriteFile(path, []byte(document), 0o600); err != nil {
		t.Fatal(err)
	}
	raw, err := LoadRaw()
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"sessions.paths", "discovery.exclude"} {
		if !raw.present[key] {
			t.Errorf("present[%q] = false", key)
		}
	}
	if raw.present["sessions.paths.claude.sessions"] {
		t.Fatal("sessions.paths descendants must be handled by list traversal")
	}
	effective := Merge(Defaults(), raw)
	if got := effective.Sessions.Paths.Claude.Sessions; got == nil || len(got) != 0 {
		t.Fatalf("empty Claude session path list = %v, want an explicit empty list", got)
	}
	if got := effective.Sessions.Paths.Codex.Sessions; len(got) != 1 || got[0] != "~/custom-codex" {
		t.Fatalf("custom Codex session paths = %v", got)
	}
	if len(effective.Discovery.Exclude) != 0 {
		t.Fatalf("empty discovery exclude = %v", effective.Discovery.Exclude)
	}
	data, err := yaml.Marshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	for _, fragment := range []string{"sessions:", "claude:", "sessions: []", "codex:", "- ~/custom-codex", "exclude: []"} {
		if !strings.Contains(text, fragment) {
			t.Errorf("marshaled config missing %q:\n%s", fragment, text)
		}
	}
}

func TestSessionsListOperationsPreserveUserNotation(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	var cfg Config
	if err := AppendList(&cfg, "sessions.paths.claude.sessions", "~/custom"); err != nil {
		t.Fatal(err)
	}
	if !cfg.present["sessions.paths"] {
		t.Fatal("AppendList did not mark sessions.paths present")
	}
	if got := cfg.Sessions.Paths.Claude.Sessions[len(cfg.Sessions.Paths.Claude.Sessions)-1]; got != "~/custom" {
		t.Fatalf("stored path = %q, want user notation", got)
	}
	if err := AppendList(&cfg, "sessions.paths.claude.sessions", filepath.Join(home, "custom")); err == nil {
		t.Fatal("duplicate path in another notation was accepted")
	}
	if err := RemoveList(&cfg, "sessions.paths.claude.sessions", filepath.Join(home, "custom")); err != nil {
		t.Fatal(err)
	}
	if got := cfg.Sessions.Paths.Claude.Sessions; len(got) != 1 || got[0] != "~/.claude/projects" {
		t.Fatalf("remaining session paths = %v", got)
	}
	if err := ResetList(&cfg, "sessions.paths.claude.sessions"); err != nil {
		t.Fatal(err)
	}
	if got := cfg.Sessions.SessionPaths("claude"); len(got) != 1 || got[0] != "~/.claude/projects" {
		t.Fatalf("reset session paths = %v", got)
	}
	if err := AppendList(&cfg, "sessions.paths.codex.sessions", "/tmp/sessions"); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"sessions.paths.cursor.sessions", "sessions.paths.claude.plans", "sessions.paths.codex.plans"} {
		if err := AppendList(&cfg, key, "/tmp/unsupported"); err == nil {
			t.Fatalf("unsupported list key %q was accepted", key)
		}
	}
}

func TestSessionsRemovedSettingsAreRejected(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	path, err := Path()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("sessions:\n  index:\n    refresh_ttl_seconds: 0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadRaw(); err == nil || !strings.Contains(err.Error(), "field index") {
		t.Fatalf("removed sessions setting error=%v", err)
	}
}

func TestSessionsValidationIsIncludedInEffectiveConfig(t *testing.T) {
	cfg := Defaults()
	if err := Validate(&cfg); err != nil {
		t.Fatalf("default sessions config was rejected: %v", err)
	}
}
