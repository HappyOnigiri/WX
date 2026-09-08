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

// TestTargetPathResolvesPerAgentPrecedenceAndFailures は agent ごとの file layout と Claude の local-settings 優先を確認する。
// unsafe local settings、未対応 agent、home directory 不在も確認する。
func TestTargetPathResolvesPerAgentPrecedenceAndFailures(t *testing.T) {
	t.Run("codex present", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		hooks := filepath.Join(home, ".codex", "hooks.json")
		writeHookConfigFile(t, hooks, "{}")
		path, err := TargetPath("codex")
		if err != nil || path != hooks {
			t.Fatalf("TargetPath(codex)=%v,%v want %s", path, err, hooks)
		}
	})

	t.Run("codex missing", func(t *testing.T) {
		_, _ = hookTestHome(t)
		state, err := Inspect("codex")
		if err != nil {
			t.Fatal(err)
		}
		if state.Status != StatusAbsent || len(state.Blocking()) != 0 {
			t.Fatalf("missing codex hooks file=%s; reasons=%v", state.Status, state.Reasons())
		}
	})

	t.Run("claude local settings take precedence", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		local := filepath.Join(home, ".claude", "settings.local.json")
		writeHookConfigFile(t, local, "{}")
		writeHookConfigFile(t, filepath.Join(home, ".claude", "settings.json"), "{}")
		path, err := TargetPath("claude")
		if err != nil || path != local {
			t.Fatalf("TargetPath(claude)=%v,%v want local settings", path, err)
		}
	})

	t.Run("claude falls back to settings.json", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		shared := filepath.Join(home, ".claude", "settings.json")
		writeHookConfigFile(t, shared, "{}")
		path, err := TargetPath("claude")
		if err != nil || path != shared {
			t.Fatalf("TargetPath(claude)=%v,%v want shared settings", path, err)
		}
	})

	// dotfile を symlink 方式(GNU Stow等)で管理する運用では settings.json 自体が symlink になる。
	// 拒否すると readiness hook が静かに未設定扱いになるため、regular file を指す symlink は許可する。
	t.Run("claude settings.json is a symlink to a regular file", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		managed := filepath.Join(home, "dotfiles", "claude-settings.json")
		writeHookConfigFile(t, managed, "{}")
		shared := filepath.Join(home, ".claude", "settings.json")
		if err := os.MkdirAll(filepath.Dir(shared), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(managed, shared); err != nil {
			t.Fatal(err)
		}
		path, err := TargetPath("claude")
		if err != nil || path != shared {
			t.Fatalf("TargetPath(claude)=%v,%v want symlinked shared settings", path, err)
		}
	})

	t.Run("claude local settings unsafe", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		if err := os.MkdirAll(filepath.Join(home, ".claude", "settings.local.json"), 0o700); err != nil {
			t.Fatal(err)
		}
		if _, err := TargetPath("claude"); err == nil {
			t.Fatal("directory masquerading as local settings was accepted")
		}
	})

	t.Run("unknown agent", func(t *testing.T) {
		t.Setenv("HOME", t.TempDir())
		if _, err := TargetPath("unknown"); err == nil {
			t.Fatal("unknown agent resolved to a path")
		}
		state, err := Inspect("unknown")
		if err != nil {
			t.Fatal(err)
		}
		if state.Status != StatusUnsupported || !hasFinding(state, FindingUnsupportedAgent) {
			t.Fatalf("unknown agent=%s; reasons=%v", state.Status, state.Reasons())
		}
	})

	t.Run("home unavailable", func(t *testing.T) {
		t.Setenv("HOME", "")
		if _, err := TargetPath("codex"); err == nil {
			t.Fatal("missing HOME resolved to a path")
		}
	})
}

// TestAvailableEvaluatesFullReadinessContractPerAgent は制御した HOME で Available を end-to-end に確認する。
// 未対応 agent、無効化 policy、両 agent の有効 contract、空・サイズ超過・不正な hook file を含める。
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

	t.Run("claude readiness contract satisfied via symlinked settings.json", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		managed := filepath.Join(home, "dotfiles", "claude-settings.json")
		writeHookConfigFile(t, managed, validDocument)
		shared := filepath.Join(home, ".claude", "settings.json")
		if err := os.MkdirAll(filepath.Dir(shared), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(managed, shared); err != nil {
			t.Fatal(err)
		}
		if !Available("claude") {
			t.Fatal("valid claude readiness contract via symlinked settings.json reported unavailable")
		}
	})
}

// TestInspectEventSkipsNonCommandDisabledAndAsyncHooks は inspectEvent の hook 単位の絞り込みを確認する。
// non-command、disabled、async の重複と、有効 group より前の不正 group を飛ばす。
func TestInspectEventSkipsNonCommandDisabledAndAsyncHooks(t *testing.T) {
	executable, err := CurrentExecutable()
	if err != nil {
		t.Fatal(err)
	}
	valid := readinessHookCommand{Type: "command", Command: executable + " hook session-start"}
	for _, test := range []struct {
		name   string
		groups []readinessHookGroup
	}{
		{
			name:   "non-command hook is skipped",
			groups: []readinessHookGroup{{Matcher: json.RawMessage(`"*"`), Hooks: []readinessHookCommand{{Type: "prompt"}, valid}}},
		},
		{
			name: "disabled duplicate is skipped",
			groups: []readinessHookGroup{{Matcher: json.RawMessage(`"*"`), Hooks: []readinessHookCommand{
				{Type: "command", Command: executable + " hook session-start", Disabled: json.RawMessage("true")}, valid,
			}}},
		},
		{
			name: "async duplicate is skipped",
			groups: []readinessHookGroup{{Matcher: json.RawMessage(`"*"`), Hooks: []readinessHookCommand{
				{Type: "command", Command: executable + " hook session-start", Async: json.RawMessage("true")}, valid,
			}}},
		},
		{
			name: "invalid group is skipped before a valid one",
			groups: []readinessHookGroup{
				{Disabled: json.RawMessage("true"), Hooks: []readinessHookCommand{valid}},
				{Matcher: json.RawMessage(`"*"`), Hooks: []readinessHookCommand{valid}},
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			matched, _ := inspectEvent(test.groups, "session-start", "SessionStart", executable)
			if !matched {
				t.Fatal("the valid hook was not found")
			}
		})
	}
}
