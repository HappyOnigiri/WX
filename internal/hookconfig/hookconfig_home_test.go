package hookconfig

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeHookConfigFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
}

func validReadinessDocument(t *testing.T, executable string) string {
	t.Helper()
	required := map[string]string{"SessionStart": "session-start", "UserPromptSubmit": "user-prompt-submit", "PreToolUse": "pre-tool-use"}
	hooks := map[string]any{}
	for event, command := range required {
		hooks[event] = []map[string]any{{
			"matcher": "*",
			"hooks": []map[string]any{{
				"type": "command", "command": executable + " hook " + command,
				"disabled": false, "async": false, "once": false,
			}},
		}}
	}
	data, err := json.Marshal(map[string]any{"disableAllHooks": false, "hooks": hooks})
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// TestReadinessHookPathsResolvesPerAgentPrecedenceAndFailures exercises every
// reachable branch of readinessHookPaths: the per-agent file layout, Claude's
// local-settings precedence over settings.json, the unsafe-local-settings
// short circuit, an unrecognised agent, and a missing home directory.
func TestReadinessHookPathsResolvesPerAgentPrecedenceAndFailures(t *testing.T) {
	t.Run("codex present", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		hooks := filepath.Join(home, ".codex", "hooks.json")
		writeHookConfigFile(t, hooks, "{}")
		paths, ok := readinessHookPaths("codex")
		if !ok || len(paths) != 1 || paths[0] != hooks {
			t.Fatalf("readinessHookPaths(codex)=%v,%v want [%s],true", paths, ok, hooks)
		}
	})

	t.Run("codex missing", func(t *testing.T) {
		t.Setenv("HOME", t.TempDir())
		if _, ok := readinessHookPaths("codex"); ok {
			t.Fatal("missing codex hooks file reported available")
		}
	})

	t.Run("claude local settings take precedence", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		local := filepath.Join(home, ".claude", "settings.local.json")
		shared := filepath.Join(home, ".claude", "settings.json")
		writeHookConfigFile(t, local, "{}")
		writeHookConfigFile(t, shared, "{}")
		paths, ok := readinessHookPaths("claude")
		if !ok || len(paths) != 1 || paths[0] != local {
			t.Fatalf("readinessHookPaths(claude)=%v,%v want local settings", paths, ok)
		}
	})

	t.Run("claude falls back to settings.json", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		shared := filepath.Join(home, ".claude", "settings.json")
		writeHookConfigFile(t, shared, "{}")
		paths, ok := readinessHookPaths("claude")
		if !ok || len(paths) != 1 || paths[0] != shared {
			t.Fatalf("readinessHookPaths(claude)=%v,%v want shared settings", paths, ok)
		}
	})

	t.Run("claude local settings unsafe", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		local := filepath.Join(home, ".claude", "settings.local.json")
		if err := os.MkdirAll(local, 0o700); err != nil {
			t.Fatal(err)
		}
		if _, ok := readinessHookPaths("claude"); ok {
			t.Fatal("directory masquerading as local settings was accepted")
		}
	})

	t.Run("unknown agent", func(t *testing.T) {
		t.Setenv("HOME", t.TempDir())
		if _, ok := readinessHookPaths("unknown"); ok {
			t.Fatal("unknown agent reported available")
		}
	})

	t.Run("home unavailable", func(t *testing.T) {
		t.Setenv("HOME", "")
		if _, ok := readinessHookPaths("codex"); ok {
			t.Fatal("missing HOME reported available")
		}
	})
}

// TestCodexHooksEnabledEvaluatesLocalAndManagedPolicyFiles exercises
// codexHooksEnabled's file-by-file evaluation: absent policy files (default
// enabled), a missing home directory, a disabling user config, a config path
// that is not a regular file, an unreadable config, and an oversized config.
func TestCodexHooksEnabledEvaluatesLocalAndManagedPolicyFiles(t *testing.T) {
	t.Run("no policy files present", func(t *testing.T) {
		t.Setenv("HOME", t.TempDir())
		if !codexHooksEnabled() {
			t.Fatal("absent policy files should default to enabled")
		}
	})

	t.Run("home unavailable", func(t *testing.T) {
		t.Setenv("HOME", "")
		if codexHooksEnabled() {
			t.Fatal("unavailable HOME should default to disabled")
		}
	})

	t.Run("user config disables hooks", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		writeHookConfigFile(t, filepath.Join(home, ".codex", "config.toml"), "[features]\nhooks = false\n")
		if codexHooksEnabled() {
			t.Fatal("disabling user config reported enabled")
		}
	})

	t.Run("user config is not a regular file", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		if err := os.MkdirAll(filepath.Join(home, ".codex", "config.toml"), 0o700); err != nil {
			t.Fatal(err)
		}
		if codexHooksEnabled() {
			t.Fatal("directory masquerading as user config reported enabled")
		}
	})

	t.Run("user config unreadable", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		path := filepath.Join(home, ".codex", "config.toml")
		writeHookConfigFile(t, path, "[features]\nhooks = true\n")
		if err := os.Chmod(path, 0); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(path, 0o600) })
		if codexHooksEnabled() {
			t.Fatal("unreadable user config reported enabled")
		}
	})

	t.Run("user config too large", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		writeHookConfigFile(t, filepath.Join(home, ".codex", "config.toml"), strings.Repeat("#", (4<<20)+1))
		if codexHooksEnabled() {
			t.Fatal("oversized user config reported enabled")
		}
	})
}

// TestAvailableEvaluatesFullReadinessContractPerAgent drives Available end to
// end through a controlled $HOME, covering: an unrecognised agent, a policy
// that disables Codex hooks, a satisfied readiness contract for both agents,
// an empty hook file, an oversized hook file, and a document that does not
// satisfy the contract.
func TestAvailableEvaluatesFullReadinessContractPerAgent(t *testing.T) {
	executable, err := CurrentExecutable()
	if err != nil {
		t.Fatal(err)
	}
	validDocument := validReadinessDocument(t, executable)

	t.Run("unknown agent", func(t *testing.T) {
		t.Setenv("HOME", t.TempDir())
		if Available("unrecognized-agent") {
			t.Fatal("unrecognized agent reported available")
		}
	})

	t.Run("codex hooks file missing", func(t *testing.T) {
		t.Setenv("HOME", t.TempDir())
		if Available("codex") {
			t.Fatal("missing codex hooks file reported available")
		}
	})

	t.Run("codex feature disabled by policy", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		writeHookConfigFile(t, filepath.Join(home, ".codex", "hooks.json"), validDocument)
		writeHookConfigFile(t, filepath.Join(home, ".codex", "config.toml"), "[features]\nhooks = false\n")
		if Available("codex") {
			t.Fatal("codex hooks reported available despite disabling policy")
		}
	})

	t.Run("codex readiness contract satisfied", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		writeHookConfigFile(t, filepath.Join(home, ".codex", "hooks.json"), validDocument)
		if !Available("codex") {
			t.Fatal("valid codex readiness contract reported unavailable")
		}
	})

	t.Run("codex hooks file empty", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		writeHookConfigFile(t, filepath.Join(home, ".codex", "hooks.json"), "")
		if Available("codex") {
			t.Fatal("empty codex hooks file reported available")
		}
	})

	t.Run("codex hooks file too large", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		writeHookConfigFile(t, filepath.Join(home, ".codex", "hooks.json"), strings.Repeat(" ", (4<<20)+1))
		if Available("codex") {
			t.Fatal("oversized codex hooks file reported available")
		}
	})

	t.Run("codex hooks document does not satisfy contract", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		writeHookConfigFile(t, filepath.Join(home, ".codex", "hooks.json"), `{"disableAllHooks":false,"hooks":{}}`)
		if Available("codex") {
			t.Fatal("incomplete codex hooks document reported available")
		}
	})

	t.Run("claude readiness contract satisfied", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		writeHookConfigFile(t, filepath.Join(home, ".claude", "settings.json"), validDocument)
		if !Available("claude") {
			t.Fatal("valid claude readiness contract reported unavailable")
		}
	})
}

// TestReadinessHookGroupsMatchSkipsNonCommandDisabledAndAsyncHooks exercises
// the per-hook filtering inside readinessHookGroupsMatch: a non-command hook
// ahead of the matching one, a disabled duplicate, an async duplicate, and an
// invalid group preceding a valid one.
func TestReadinessHookGroupsMatchSkipsNonCommandDisabledAndAsyncHooks(t *testing.T) {
	executable, err := CurrentExecutable()
	if err != nil {
		t.Fatal(err)
	}
	valid := readinessHookCommand{Type: "command", Command: executable + " hook session-start"}

	t.Run("non-command hook is skipped", func(t *testing.T) {
		groups := []readinessHookGroup{{Matcher: json.RawMessage(`"*"`), Hooks: []readinessHookCommand{{Type: "prompt"}, valid}}}
		if !readinessHookGroupsMatch(groups, "session-start", "SessionStart", executable) {
			t.Fatal("group with a leading non-command hook was rejected")
		}
	})

	t.Run("disabled duplicate is skipped", func(t *testing.T) {
		groups := []readinessHookGroup{{Matcher: json.RawMessage(`"*"`), Hooks: []readinessHookCommand{
			{Type: "command", Command: executable + " hook session-start", Disabled: json.RawMessage("true")}, valid,
		}}}
		if !readinessHookGroupsMatch(groups, "session-start", "SessionStart", executable) {
			t.Fatal("group with a disabled duplicate hook was rejected")
		}
	})

	t.Run("async duplicate is skipped", func(t *testing.T) {
		groups := []readinessHookGroup{{Matcher: json.RawMessage(`"*"`), Hooks: []readinessHookCommand{
			{Type: "command", Command: executable + " hook session-start", Async: json.RawMessage("true")}, valid,
		}}}
		if !readinessHookGroupsMatch(groups, "session-start", "SessionStart", executable) {
			t.Fatal("group with an async duplicate hook was rejected")
		}
	})

	t.Run("invalid group is skipped before a valid one", func(t *testing.T) {
		groups := []readinessHookGroup{
			{Disabled: json.RawMessage("true"), Hooks: []readinessHookCommand{valid}},
			{Matcher: json.RawMessage(`"*"`), Hooks: []readinessHookCommand{valid}},
		}
		if !readinessHookGroupsMatch(groups, "session-start", "SessionStart", executable) {
			t.Fatal("valid group after a disabled group was rejected")
		}
	})
}

// TestResolveHookExecutableExpandsHomeAndTildeVariants exercises every
// $HOME/~ expansion branch in resolveHookExecutable, plus the case where
// os.UserHomeDir itself fails.
func TestResolveHookExecutableExpandsHomeAndTildeVariants(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	binary := filepath.Join(home, "bin", "wx")
	if err := os.MkdirAll(filepath.Dir(binary), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(binary, []byte("#!/bin/sh\n"), 0o700); err != nil {
		t.Fatal(err)
	}

	for _, test := range []struct {
		name  string
		value string
	}{
		{name: "$HOME prefix", value: "$HOME/bin/wx"},
		{name: "tilde prefix", value: "~/bin/wx"},
	} {
		t.Run(test.name, func(t *testing.T) {
			resolved, ok := resolveHookExecutable(test.value)
			if !ok || resolved != binary {
				t.Fatalf("resolveHookExecutable(%q) = %q,%v want %q,true", test.value, resolved, ok, binary)
			}
		})
	}

	t.Run("bare $HOME is not an executable", func(t *testing.T) {
		if _, ok := resolveHookExecutable("$HOME"); ok {
			t.Fatal("bare $HOME expanding to a directory was accepted as an executable")
		}
	})

	t.Run("bare tilde is not an executable", func(t *testing.T) {
		if _, ok := resolveHookExecutable("~"); ok {
			t.Fatal("bare ~ expanding to a directory was accepted as an executable")
		}
	})

	t.Run("home unavailable", func(t *testing.T) {
		t.Setenv("HOME", "")
		if _, ok := resolveHookExecutable("~/bin/wx"); ok {
			t.Fatal("tilde path resolved despite missing HOME")
		}
	})
}
