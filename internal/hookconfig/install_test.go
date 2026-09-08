package hookconfig

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// hookTestHome は制御した HOME と、PATH 上の wx（テスト binary への symlink）を用意する。
// EvalSymlinks の後に os.SameFile で比較されるため、symlink でも実行中 binary との一致を満たせる。
func hookTestHome(t *testing.T) (home, binary string) {
	t.Helper()
	home = t.TempDir()
	t.Setenv("HOME", home)
	directory := t.TempDir()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	binary = filepath.Join(directory, "wx")
	if err := os.Symlink(executable, binary); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", directory)
	return home, binary
}

func TestInstallWritesAcceptedEntriesAndIsIdempotent(t *testing.T) {
	for _, agent := range []string{"claude", "codex"} {
		t.Run(agent, func(t *testing.T) {
			home, binary := hookTestHome(t)
			result, err := Install(agent)
			if err != nil {
				t.Fatal(err)
			}
			if !result.Changed || result.State.Status != StatusCurrent {
				t.Fatalf("install result=%+v reasons=%v", result, result.State.Reasons())
			}
			if !Available(agent) {
				t.Fatal("readiness contract is not available right after install")
			}
			data, err := os.ReadFile(result.Path)
			if err != nil {
				t.Fatal(err)
			}
			for _, event := range Events() {
				if !strings.Contains(string(data), binary+" hook "+event.Subcommand) {
					t.Fatalf("%s is missing the %s command:\n%s", result.Path, event.Name, data)
				}
			}
			if strings.Contains(string(data), `"matcher"`) {
				t.Fatalf("wx wrote a matcher key:\n%s", data)
			}
			// 2 回目は同じ内容になるため、書かずに Changed=false を返す。
			again, err := Install(agent)
			if err != nil {
				t.Fatal(err)
			}
			if again.Changed || again.State.Status != StatusCurrent {
				t.Fatalf("second install changed the file: %+v", again)
			}
			if agent == "claude" && result.Backup != "" {
				t.Fatalf("a backup was taken for a new file: %s", result.Backup)
			}
			_ = home
		})
	}
}

// realisticSettings は実機の settings.json に近い seed で、正規形（2 space・末尾改行）で書く。
const realisticSettings = `{
  "model": "opus",
  "includeCoAuthoredBy": false,
  "permissions": {
    "allow": [
      "Bash(git status:*)"
    ]
  },
  "hooks": {
    "PreToolUse": [
      {
        "matcher": "Bash",
        "hooks": [
          {
            "type": "command",
            "command": "/bin/echo audit",
            "timeout": 5
          }
        ]
      }
    ],
    "SubagentStart": [
      {
        "hooks": [
          {
            "type": "command",
            "command": "/bin/echo subagent"
          }
        ]
      }
    ]
  },
  "statusLine": {
    "type": "command",
    "command": "/bin/echo status"
  }
}
`

func TestInstallPreservesOtherSettingsAndRemoveRestoresThem(t *testing.T) {
	_, _ = hookTestHome(t)
	path, err := TargetPath("claude")
	if err != nil {
		t.Fatal(err)
	}
	writeHookConfigFile(t, path, realisticSettings)
	result, err := Install("claude")
	if err != nil {
		t.Fatal(err)
	}
	if !Available("claude") || result.State.Status != StatusCurrent {
		t.Fatalf("install did not produce an accepted contract: %v", result.State.Reasons())
	}
	if result.Backup == "" {
		t.Fatal("no backup was taken for an existing claude settings file")
	}
	installed := readTestFile(t, path)
	for _, fragment := range []string{
		`"model": "opus"`, `"includeCoAuthoredBy": false`, `"Bash(git status:*)"`,
		`"matcher": "Bash"`, `"command": "/bin/echo audit"`, `"timeout": 5`, `"command": "/bin/echo subagent"`,
	} {
		if !strings.Contains(installed, fragment) {
			t.Fatalf("install changed an unrelated value %s:\n%s", fragment, installed)
		}
	}
	if got := topLevelKeyOrder(installed); got != "model includeCoAuthoredBy permissions hooks statusLine" {
		t.Fatalf("top-level key order changed: %s", got)
	}
	removed, err := Remove("claude")
	if err != nil {
		t.Fatal(err)
	}
	if !removed.Changed {
		t.Fatal("remove reported no change")
	}
	if got := readTestFile(t, path); got != realisticSettings {
		t.Fatalf("remove did not restore the seed byte for byte:\n%s", got)
	}
	if Available("claude") {
		t.Fatal("the readiness contract survived remove")
	}
}

func TestRemovePrunesOnlyWXEntriesAtEveryLevel(t *testing.T) {
	_, binary := hookTestHome(t)
	path, err := TargetPath("codex")
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name, seed, want string
	}{
		{
			name: "another hook in the same event is kept",
			seed: `{"hooks":{"SessionStart":[{"hooks":[{"type":"command","command":"BINARY hook session-start"}]},{"hooks":[{"type":"command","command":"/bin/echo other"}]}]}}`,
			want: `"command": "/bin/echo other"`,
		},
		{
			name: "a shared group keeps its other hooks",
			seed: `{"hooks":{"SessionStart":[{"hooks":[{"type":"command","command":"BINARY hook session-start"},{"type":"command","command":"/bin/echo other"}]}]}}`,
			want: `"command": "/bin/echo other"`,
		},
		{
			name: "an emptied event key is dropped",
			seed: `{"other":1,"hooks":{"SessionStart":[{"hooks":[{"type":"command","command":"BINARY hook session-start"}]}]}}`,
			want: `{
  "other": 1
}
`,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			writeHookConfigFile(t, path, strings.ReplaceAll(test.seed, "BINARY", binary))
			if _, err := Remove("codex"); err != nil {
				t.Fatal(err)
			}
			got := readTestFile(t, path)
			if !strings.Contains(got, test.want) {
				t.Fatalf("remove produced:\n%s\nwant it to contain:\n%s", got, test.want)
			}
			if strings.Contains(got, "hook session-start") {
				t.Fatalf("a wx entry survived remove:\n%s", got)
			}
			if strings.Contains(got, `"SessionStart": []`) || strings.Contains(got, `"hooks": {}`) {
				t.Fatalf("remove left an empty container behind:\n%s", got)
			}
		})
	}
}

// TestInstallKeepsRecordedCommandsThatResolveToTheSameBinary は install.sh の通常更新で
// 書き換えが起きないことを確認する。destination の inode は毎回変わるが path は不変である。
func TestInstallKeepsRecordedCommandsThatResolveToTheSameBinary(t *testing.T) {
	home, binary := hookTestHome(t)
	path, err := TargetPath("codex")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Install("codex"); err != nil {
		t.Fatal(err)
	}
	before := readTestFile(t, path)
	// 同じ path のまま binary の実体を差し替える。install.sh の mv -f と同じ形である。
	replacement := filepath.Join(home, "replacement-wx")
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	copyTestFile(t, executable, replacement)
	if err := os.Remove(binary); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(replacement, binary); err != nil {
		t.Fatal(err)
	}
	state, err := Inspect("codex")
	if err != nil {
		t.Fatal(err)
	}
	if state.Status != StatusStale {
		t.Fatalf("a different binary at the same path was not reported stale: %v", state.Reasons())
	}
	if err := os.Remove(binary); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(executable, binary); err != nil {
		t.Fatal(err)
	}
	again, err := Install("codex")
	if err != nil {
		t.Fatal(err)
	}
	if again.Changed || readTestFile(t, path) != before {
		t.Fatal("an unchanged installation rewrote the recorded commands")
	}
}

// TestInstallRepairsWXEntriesTheReadSideRejects は、判定側が却下する任意項目が付いた wx エントリを
// install が書き直せることを確認する。書き換えを skip すると setup から永久に修復できなくなる。
func TestInstallRepairsWXEntriesTheReadSideRejects(t *testing.T) {
	for _, option := range []string{`"async": true`, `"disabled": true`, `"once": true`, `"timeout": null`} {
		t.Run(option, func(t *testing.T) {
			_, binary := hookTestHome(t)
			path, err := TargetPath("codex")
			if err != nil {
				t.Fatal(err)
			}
			seed := `{"hooks":{"SessionStart":[{"hooks":[{"type":"command","command":"BINARY hook session-start",OPTION}]}]}}`
			seed = strings.ReplaceAll(seed, "BINARY", binary)
			writeHookConfigFile(t, path, strings.ReplaceAll(seed, "OPTION", option))
			state, err := Inspect("codex")
			if err != nil {
				t.Fatal(err)
			}
			if state.Status == StatusCurrent {
				t.Fatalf("the read side accepted %s, so this seed proves nothing", option)
			}
			result, err := Install("codex")
			if err != nil {
				t.Fatal(err)
			}
			if !result.Changed {
				t.Fatalf("install skipped a wx entry the read side rejects: %+v", result)
			}
			if result.State.Status != StatusCurrent || !Available("codex") {
				t.Fatalf("install did not repair the entry: %v", result.State.Reasons())
			}
			if got := readTestFile(t, path); strings.Contains(got, strings.SplitN(option, ":", 2)[0]) {
				t.Fatalf("the rejected option survived install:\n%s", got)
			}
		})
	}
}

// TestInstallKeepsTheIndentOfTheExistingFile は、4 space の設定ファイルへ install しても
// 無関係な行が 2 space へ整形されないことを確認する。
func TestInstallKeepsTheIndentOfTheExistingFile(t *testing.T) {
	_, _ = hookTestHome(t)
	path, err := TargetPath("codex")
	if err != nil {
		t.Fatal(err)
	}
	writeHookConfigFile(t, path, "{\n    \"model\": \"opus\",\n    \"permissions\": {\n        \"allow\": []\n    }\n}\n")
	if _, err := Install("codex"); err != nil {
		t.Fatal(err)
	}
	got := readTestFile(t, path)
	for _, line := range []string{"\n    \"model\": \"opus\",", "\n    \"permissions\": {", "\n        \"allow\": []"} {
		if !strings.Contains(got, line) {
			t.Fatalf("install reindented an unrelated line %q:\n%s", line, got)
		}
	}
}

func TestInstallRefusesUnsafeTargetsWithoutWriting(t *testing.T) {
	for _, test := range []struct {
		name, seed string
		directory  bool
	}{
		{name: "malformed JSON", seed: "{"},
		{name: "duplicate keys", seed: `{"hooks":{},"hooks":{}}`},
		{name: "top level array", seed: `[]`},
		{name: "empty file", seed: ""},
		{name: "directory", directory: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, _ = hookTestHome(t)
			path, err := TargetPath("codex")
			if err != nil {
				t.Fatal(err)
			}
			if test.directory {
				if err := os.MkdirAll(path, 0o700); err != nil {
					t.Fatal(err)
				}
			} else {
				writeHookConfigFile(t, path, test.seed)
			}
			if _, err := Install("codex"); err == nil {
				t.Fatal("an unsafe target was accepted")
			}
			if test.directory {
				return
			}
			if got := readTestFile(t, path); got != test.seed {
				t.Fatalf("the rejected target was modified:\n%s", got)
			}
		})
	}
}

func TestInstallFollowsSymlinksAndReportsTheResolvedPath(t *testing.T) {
	home, _ := hookTestHome(t)
	path, err := TargetPath("codex")
	if err != nil {
		t.Fatal(err)
	}
	managed := filepath.Join(home, "dotfiles", "hooks.json")
	writeHookConfigFile(t, managed, "{}\n")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(managed, path); err != nil {
		t.Fatal(err)
	}
	result, err := Install("codex")
	if err != nil {
		t.Fatal(err)
	}
	resolvedManaged, err := filepath.EvalSymlinks(managed)
	if err != nil {
		t.Fatal(err)
	}
	if result.Resolved != resolvedManaged {
		t.Fatalf("resolved=%q want %q", result.Resolved, resolvedManaged)
	}
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("the link was replaced by a regular file: %v %v", info, err)
	}
	if !strings.Contains(readTestFile(t, managed), "hook session-start") {
		t.Fatal("the symlink target was not updated")
	}
}

func TestInstallRejectsMissingWXOnPathWithoutWriting(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("PATH", t.TempDir())
	if _, err := Install("codex"); err == nil {
		t.Fatal("install succeeded without wx on PATH")
	}
	path, err := TargetPath("codex")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("install created %s despite failing: %v", path, err)
	}
}

func readTestFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func copyTestFile(t *testing.T, source, destination string) {
	t.Helper()
	data, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(destination, data, 0o700); err != nil {
		t.Fatal(err)
	}
}

// topLevelKeyOrder は整形済み JSON の最上位キーを出現順に並べる。
func topLevelKeyOrder(document string) string {
	var keys []string
	for _, line := range strings.Split(document, "\n") {
		if !strings.HasPrefix(line, `  "`) {
			continue
		}
		if key, _, found := strings.Cut(strings.TrimPrefix(line, `  "`), `"`); found {
			keys = append(keys, key)
		}
	}
	return strings.Join(keys, " ")
}
