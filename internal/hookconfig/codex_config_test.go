package hookconfig

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestCodexPolicyFindingsRecognizeConfigBoundaries は config.toml の書式ごとに、本番経路が
// hook を許すか（finding なし）を確認する。解釈できない TOML と明示的な無効化は code で区別する。
func TestCodexPolicyFindingsRecognizeConfigBoundaries(t *testing.T) {
	tests := []struct {
		name string
		data string
		want bool
	}{
		{name: "empty", data: "", want: true},
		{name: "comments", data: "# hooks are enabled\nname = \"wx\" # inline comment", want: true},
		{name: "features table", data: "[features]\nhooks = true\ncodex_hooks = true", want: true},
		{name: "quoted keys", data: "[features]\n\"hooks\" = true\n'codex_hooks' = true", want: true},
		{name: "inline features", data: `features = { hooks = true, codex_hooks = true, nested = { value = "#" } }`, want: true},
		{name: "array of tables", data: "[[features.hooks]]\nname = \"wx\"", want: true},
		{name: "feature disabled", data: "[features]\nhooks = false", want: false},
		{name: "inline feature disabled", data: "features = { hooks = false }", want: false},
		{name: "malformed table", data: "[features", want: false},
		{name: "malformed array table", data: "[[features.hooks]", want: false},
		{name: "bare statement", data: "hooks", want: false},
		{name: "multiline string", data: "name = \"\"\"hooks\"\"\"", want: false},
		{name: "unbalanced inline table", data: "features = { hooks = true", want: false},
		{name: "inline field without value", data: "features = { hooks }", want: false},
		{name: "multiline array", data: "notify = [\n    \"/opt/Codex Client.app/Contents/MacOS/client\",\n    \"turn-ended\",\n]\n\n[features]\nhooks = true", want: true},
		{name: "multiline array before disabled feature", data: "notify = [\n    \"turn-ended\",\n]\n\n[features]\nhooks = false", want: false},
		{name: "multiline array of arrays", data: "matrix = [\n    [1, 2],\n    [3],\n]", want: true},
		{name: "multiline inline table", data: "notify = { command = \"client\",\n    event = \"turn-ended\" }", want: true},
		{name: "unclosed multiline array", data: "notify = [\n    \"turn-ended\",\n\n[features]\nhooks = false", want: false},
		{name: "statement after closed array", data: "notify = [\n    \"turn-ended\",\n] hooks = false", want: false},
		{name: "unopened array close", data: "notify = ]", want: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			writeHookConfigFile(t, filepath.Join(home, ".codex", "config.toml"), test.data)
			findings := codexPolicyFindings()
			if got := len(findings) == 0; got != test.want {
				t.Fatalf("codexPolicyFindings(%q) = %v, want hooks allowed=%v", test.data, findings, test.want)
			}
			if test.want {
				return
			}
			if code := findings[0].Code; code != FindingCodexFeatureOff && code != FindingCodexConfigUnusable {
				t.Fatalf("code=%s for %q", code, test.data)
			}
		})
	}
}

// TestCodexPolicyFindingsEvaluateLocalAndManagedPolicyFiles は policy file ごとの判定を確認する。
// file 不在、home 不在、無効化設定、regular file 以外、読取不能、サイズ超過を含める。
func TestCodexPolicyFindingsEvaluateLocalAndManagedPolicyFiles(t *testing.T) {
	t.Run("no policy files present", func(t *testing.T) {
		t.Setenv("HOME", t.TempDir())
		if findings := codexPolicyFindings(); len(findings) != 0 {
			t.Fatalf("absent policy files should default to enabled: %v", findings)
		}
	})

	t.Run("home unavailable", func(t *testing.T) {
		t.Setenv("HOME", "")
		if len(codexPolicyFindings()) == 0 {
			t.Fatal("unavailable HOME should default to disabled")
		}
	})

	t.Run("user config disables hooks", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		writeHookConfigFile(t, filepath.Join(home, ".codex", "config.toml"), "[features]\nhooks = false\n")
		if len(codexPolicyFindings()) == 0 {
			t.Fatal("disabling user config reported enabled")
		}
	})

	t.Run("user config is not a regular file", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		if err := os.MkdirAll(filepath.Join(home, ".codex", "config.toml"), 0o700); err != nil {
			t.Fatal(err)
		}
		if len(codexPolicyFindings()) == 0 {
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
		if len(codexPolicyFindings()) == 0 {
			t.Fatal("unreadable user config reported enabled")
		}
	})

	t.Run("user config too large", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		writeHookConfigFile(t, filepath.Join(home, ".codex", "config.toml"), strings.Repeat("#", (4<<20)+1))
		if len(codexPolicyFindings()) == 0 {
			t.Fatal("oversized user config reported enabled")
		}
	})
}
