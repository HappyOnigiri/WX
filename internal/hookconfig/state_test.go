package hookconfig

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// managedDocument は Install が書くのと同じ 4 event の文書を返す。
func managedDocument(binary string) string {
	var groups []string
	for _, event := range Events() {
		groups = append(groups, `"`+event.Name+`":[{"hooks":[{"type":"command","command":"`+binary+` hook `+event.Subcommand+`"}]}]`)
	}
	return `{"hooks":{` + strings.Join(groups, ",") + `}}`
}

// TestInspectReportsEveryRejectionReason は finding code ごとに seed を 1 行持ち、
// 併せて Available と StatusCurrent が乖離していないことを確認する。
func TestInspectReportsEveryRejectionReason(t *testing.T) {
	for _, test := range []struct {
		name   string
		seed   func(binary string) string
		want   Status
		code   FindingCode
		absent bool
	}{
		{name: "installed", seed: managedDocument, want: StatusCurrent},
		{name: "no file", absent: true, want: StatusAbsent},
		{name: "no hooks object", seed: func(string) string { return `{"model":"opus"}` }, want: StatusAbsent, code: FindingHooksMissing},
		{
			name: "one event missing",
			seed: func(binary string) string {
				return strings.Replace(managedDocument(binary), `"PreToolUse"`, `"PreToolUseTypo"`, 1)
			},
			want: StatusStale, code: FindingEventMissing,
		},
		{
			name: "unknown field in a required event",
			seed: func(binary string) string {
				return strings.Replace(managedDocument(binary), `"type":"command"`, `"type":"command","description":"x"`, 1)
			},
			want: StatusStale, code: FindingEventUnknownField,
		},
		{
			name: "specific matcher rejects the group",
			seed: func(binary string) string {
				return strings.Replace(managedDocument(binary), `[{"hooks"`, `[{"matcher":"SessionStart","hooks"`, 1)
			},
			want: StatusStale, code: FindingGroupRejected,
		},
		{
			name: "async hooks do not gate",
			seed: func(binary string) string {
				return strings.Replace(managedDocument(binary), `hook session-start"`, `hook session-start","async":true`, 1)
			},
			want: StatusStale, code: FindingCommandSkipped,
		},
		{
			name: "another binary",
			seed: func(binary string) string {
				return strings.Replace(managedDocument(binary), binary+" hook session-start", "/bin/echo hook session-start", 1)
			},
			want: StatusStale, code: FindingCommandOtherBinary,
		},
		{
			name: "disableAllHooks blocks the whole file",
			seed: func(binary string) string {
				return strings.Replace(managedDocument(binary), `{"hooks"`, `{"disableAllHooks":true,"hooks"`, 1)
			},
			want: StatusBlocked, code: FindingAllHooksDisabled,
		},
		{name: "malformed JSON", seed: func(string) string { return "{" }, want: StatusBlocked, code: FindingTargetUnparsable},
		{name: "duplicate keys", seed: func(string) string { return `{"hooks":{},"hooks":{}}` }, want: StatusBlocked, code: FindingTargetDuplicateKey},
		{name: "empty file", seed: func(string) string { return "" }, want: StatusBlocked, code: FindingTargetEmpty},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, binary := hookTestHome(t)
			path, err := TargetPath("codex")
			if err != nil {
				t.Fatal(err)
			}
			if !test.absent {
				writeHookConfigFile(t, path, test.seed(binary))
			}
			state, err := Inspect("codex")
			if err != nil {
				t.Fatal(err)
			}
			if state.Status != test.want {
				t.Fatalf("status=%s want %s; reasons=%v", state.Status, test.want, state.Reasons())
			}
			if test.code != "" && !hasFinding(state, test.code) {
				t.Fatalf("finding %s is missing; reasons=%v", test.code, state.Reasons())
			}
			if got := Available("codex"); got != (state.Status == StatusCurrent) {
				t.Fatalf("Available=%v but status=%s; reasons=%v", got, state.Status, state.Reasons())
			}
		})
	}
}

func TestInspectReportsCodexPolicyAndClaudeShadowing(t *testing.T) {
	t.Run("codex feature disabled", func(t *testing.T) {
		home, binary := hookTestHome(t)
		path, err := TargetPath("codex")
		if err != nil {
			t.Fatal(err)
		}
		writeHookConfigFile(t, path, managedDocument(binary))
		writeHookConfigFile(t, filepath.Join(home, ".codex", "config.toml"), "[features]\nhooks = false\n")
		state, err := Inspect("codex")
		if err != nil {
			t.Fatal(err)
		}
		if state.Status != StatusBlocked || !hasFinding(state, FindingCodexFeatureOff) {
			t.Fatalf("status=%s reasons=%v", state.Status, state.Reasons())
		}
		if Available("codex") {
			t.Fatal("a disabled Codex hooks feature was reported available")
		}
	})

	t.Run("codex config unparsable", func(t *testing.T) {
		home, binary := hookTestHome(t)
		path, err := TargetPath("codex")
		if err != nil {
			t.Fatal(err)
		}
		writeHookConfigFile(t, path, managedDocument(binary))
		writeHookConfigFile(t, filepath.Join(home, ".codex", "config.toml"), "value = \"\"\"\nmultiline\n\"\"\"\n")
		state, err := Inspect("codex")
		if err != nil {
			t.Fatal(err)
		}
		if !hasFinding(state, FindingCodexConfigUnusable) {
			t.Fatalf("reasons=%v", state.Reasons())
		}
	})

	t.Run("claude local settings shadow settings.json", func(t *testing.T) {
		home, binary := hookTestHome(t)
		writeHookConfigFile(t, filepath.Join(home, ".claude", "settings.json"), managedDocument(binary))
		writeHookConfigFile(t, filepath.Join(home, ".claude", "settings.local.json"), "{}")
		state, err := Inspect("claude")
		if err != nil {
			t.Fatal(err)
		}
		if state.Path != filepath.Join(home, ".claude", "settings.local.json") {
			t.Fatalf("path=%s", state.Path)
		}
		if state.Shadowed == "" || !hasFinding(state, FindingLocalSettingsShadow) {
			t.Fatalf("shadowing was not reported: %v", state.Reasons())
		}
		if Available("claude") {
			t.Fatal("a shadowed settings.json was reported available")
		}
	})

	t.Run("unsupported agent", func(t *testing.T) {
		t.Setenv("HOME", t.TempDir())
		state, err := Inspect("editor")
		if err != nil {
			t.Fatal(err)
		}
		if state.Status != StatusUnsupported || Available("editor") {
			t.Fatalf("status=%s", state.Status)
		}
	})
}

func TestInspectReportsDotfileManagedTargets(t *testing.T) {
	home, binary := hookTestHome(t)
	managed := filepath.Join(home, "dotfiles", "hooks.json")
	writeHookConfigFile(t, managed, managedDocument(binary))
	if err := os.MkdirAll(filepath.Join(home, "dotfiles", ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	path, err := TargetPath("codex")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(managed, path); err != nil {
		t.Fatal(err)
	}
	state, err := Inspect("codex")
	if err != nil {
		t.Fatal(err)
	}
	if state.Status != StatusCurrent {
		t.Fatalf("status=%s reasons=%v", state.Status, state.Reasons())
	}
	if !hasFinding(state, FindingTargetSymlink) || !hasFinding(state, FindingTargetInRepository) {
		t.Fatalf("dotfile management was not reported: %v", state.Reasons())
	}
}

func TestEventsCoverTheRequiredReadinessContract(t *testing.T) {
	required := 0
	for _, event := range Events() {
		if event.Required {
			required++
		}
		if event.Name == "SessionEnd" && event.Required {
			t.Fatal("SessionEnd must stay outside the required set to keep existing installs working")
		}
	}
	if required != 3 {
		t.Fatalf("required events=%d, want 3", required)
	}
	if Status(99).String() != "unknown" {
		t.Fatal("an unknown status must be reported as unknown")
	}
}

func hasFinding(state State, code FindingCode) bool {
	for _, finding := range state.Findings {
		if finding.Code == code {
			return true
		}
	}
	return false
}
