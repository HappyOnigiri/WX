package hookconfig

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestHookConfigJSONValidationBoundaries(t *testing.T) {
	for _, test := range []struct {
		name  string
		raw   json.RawMessage
		want  bool
		valid bool
	}{
		{name: "missing", raw: nil, want: false, valid: true},
		{name: "null", raw: json.RawMessage("null"), want: false},
		{name: "boolean", raw: json.RawMessage("true"), want: true, valid: true},
		{name: "invalid boolean", raw: json.RawMessage(`"true"`), want: false},
	} {
		t.Run("bool/"+test.name, func(t *testing.T) {
			got, valid := boolOption(test.raw)
			if got != test.want || valid != test.valid {
				t.Fatalf("boolOption(%s) = (%v, %v), want (%v, %v)", test.raw, got, valid, test.want, test.valid)
			}
		})
	}

	for _, test := range []struct {
		name string
		fn   func(json.RawMessage) bool
	}{
		{name: "string", fn: optionalStringValid},
		{name: "number", fn: optionalNumberValid},
		{name: "integer", fn: optionalIntegerValid},
	} {
		t.Run(test.name, func(t *testing.T) {
			if !test.fn(nil) || !test.fn(json.RawMessage(`"value"`)) && test.name == "string" || !test.fn(json.RawMessage("1")) && test.name != "string" {
				t.Fatalf("valid %s option rejected", test.name)
			}
			if test.fn(json.RawMessage("null")) {
				t.Fatalf("null %s option accepted", test.name)
			}
			if test.name == "string" && test.fn(json.RawMessage("1")) {
				t.Fatal("number accepted as string option")
			}
			if test.name == "number" && test.fn(json.RawMessage(`"1"`)) {
				t.Fatal("string accepted as number option")
			}
			if test.name == "integer" && test.fn(json.RawMessage("1.5")) {
				t.Fatal("fraction accepted as integer option")
			}
		})
	}

	for _, test := range []struct {
		name string
		raw  json.RawMessage
		want bool
	}{
		{name: "missing matcher", raw: nil, want: true},
		{name: "wildcard", raw: json.RawMessage(`"*"`), want: true},
		{name: "regex wildcard", raw: json.RawMessage(`"^.*$"`), want: true},
		{name: "null matcher", raw: json.RawMessage("null"), want: false},
		{name: "specific matcher", raw: json.RawMessage(`"SessionStart"`), want: false},
		{name: "non-string matcher", raw: json.RawMessage("1"), want: false},
	} {
		t.Run("matcher/"+test.name, func(t *testing.T) {
			if got := matcherAppliesToEveryEvent(test.raw); got != test.want {
				t.Fatalf("matcherAppliesToEveryEvent(%s) = %v, want %v", test.raw, got, test.want)
			}
		})
	}
}

func TestHookConfigStrictJSONAndGroupValidation(t *testing.T) {
	var document map[string]json.RawMessage
	if err := decodeJSON([]byte(`{"ok":true}`), &document); err != nil {
		t.Fatal(err)
	}
	if err := decodeJSON([]byte(`{"ok":true}{"extra":true}`), &document); err == nil {
		t.Fatal("multiple JSON values accepted")
	}
	if err := decodeJSON([]byte(`{"ok":true} trailing`), &document); err == nil {
		t.Fatal("trailing malformed JSON accepted")
	}
	var group readinessHookGroup
	if err := decodeStrictJSON([]byte(`{"matcher":"*","hooks":[]}`), &group); err != nil {
		t.Fatal(err)
	}
	if err := decodeStrictJSON([]byte(`{"unknown":true}`), &group); err == nil {
		t.Fatal("unknown strict JSON field accepted")
	}

	validCommand := readinessHookCommand{Type: "command", Command: "/bin/wx hook SessionStart", Disabled: json.RawMessage("false"), Async: json.RawMessage("false"), Once: json.RawMessage("false"), Timeout: json.RawMessage("1"), StatusMessage: json.RawMessage(`"ready"`), AdditionalContextLimit: json.RawMessage("1")}
	if !inspectCommand(validCommand) {
		t.Fatal("valid command hook rejected")
	}
	for _, invalid := range []readinessHookCommand{
		{Type: "unknown"},
		{Type: "command"},
		{Type: "command", Command: "/bin/wx", Disabled: json.RawMessage(`"false"`)},
		{Type: "command", Command: "/bin/wx", Timeout: json.RawMessage("null")},
		{Type: "command", Command: "/bin/wx", AdditionalContextLimit: json.RawMessage("1.5")},
	} {
		if inspectCommand(invalid) {
			t.Fatalf("invalid command hook accepted: %+v", invalid)
		}
	}
	if valid, _ := inspectGroup(readinessHookGroup{Matcher: json.RawMessage(`"*"`), Hooks: []readinessHookCommand{validCommand}}); !valid {
		t.Fatal("valid hook group rejected")
	}
	for _, invalid := range []readinessHookGroup{
		{Disabled: json.RawMessage("true"), Hooks: []readinessHookCommand{validCommand}},
		{Matcher: json.RawMessage("null"), Hooks: []readinessHookCommand{validCommand}},
		{Matcher: json.RawMessage(`"*"`)},
	} {
		if valid, findings := inspectGroup(invalid); valid || len(findings) == 0 {
			t.Fatalf("invalid hook group accepted: %+v", invalid)
		}
	}
}

func TestInspectDocumentRejectsMalformedAndDisabledHooks(t *testing.T) {
	required := map[string]string{"SessionStart": "session-start"}
	executable, err := CurrentExecutable()
	if err != nil {
		t.Fatal(err)
	}
	valid := `{"disableAllHooks":false,"hooks":{"SessionStart":[{"matcher":"*","hooks":[{"type":"command","command":"` + executable + ` hook session-start","disabled":false,"async":false,"once":false}]}]}}`
	if !documentMatchesEvery(t, valid, required, executable) {
		t.Fatal("valid readiness hook document rejected")
	}
	for _, data := range []string{
		"not json",
		`{"disableAllHooks":true}`,
		`{"disableAllHooks":false}`,
		`{"disableAllHooks":false,"hooks":{"SessionStart":"wrong"}}`,
		`{"disableAllHooks":false,"hooks":{"SessionStart":[{"matcher":"*","hooks":[{"type":"command","command":"` + executable + ` hook session-start","async":true}]}]}}`,
	} {
		if documentMatchesEvery(t, data, required, executable) {
			t.Fatalf("malformed or disabled readiness document accepted: %s", data)
		}
	}
	if _, ok := splitTOMLInlineFields(`hooks = true, nested = { value = "x" }, list = [1, 2]`); !ok {
		t.Fatal("balanced TOML inline fields rejected")
	}
	if inlineTOMLFeatureTableEnabled(`{nested = { value = 1 ] }}`) {
		t.Fatal("unbalanced nested TOML feature table accepted")
	}
	for _, raw := range []string{`hooks = "unterminated`, `nested = { value = 1`, `nested = { value = 1 ] }`} {
		if _, ok := splitTOMLInlineFields(raw); ok {
			t.Fatalf("malformed TOML inline fields accepted: %s", raw)
		}
	}
	if normalizeTOMLKey(`"hooks"`) != "hooks" || normalizeTOMLKey(" plain ") != "plain" {
		t.Fatal("TOML key normalization failed")
	}
	if stripTOMLComment(`value = "#not-comment" # comment`) != `value = "#not-comment" ` {
		t.Fatal("TOML comment stripping changed quoted content")
	}
}

func TestInspectDocumentRequiresEachConfiguredEvent(t *testing.T) {
	executable, err := CurrentExecutable()
	if err != nil {
		t.Fatal(err)
	}
	data := `{"disableAllHooks":false,"hooks":{"SessionStart":[{"matcher":"*","hooks":[{"type":"command","command":"` + executable + ` hook SessionStart","disabled":false,"async":false,"once":false}]}]}}`
	required := map[string]string{"SessionStart": "SessionStart", "UserPromptSubmit": "UserPromptSubmit"}
	if documentMatchesEvery(t, data, required, executable) {
		t.Fatal("readiness hook document missing a required event was accepted")
	}
}

// documentMatchesEvery は inspectDocument の report から、required の全 event が受理されたかを返す。
// 受理判定の本番経路をそのまま呼び、テスト側で bool へ畳む。
func documentMatchesEvery(t *testing.T, data string, required map[string]string, executable string) bool {
	t.Helper()
	report := inspectDocument([]byte(data), required, executable)
	if report.blocked {
		return false
	}
	for event := range required {
		if !report.matched[event] {
			return false
		}
	}
	return true
}

func TestRegularHookPathRejectsUnsafeEntries(t *testing.T) {
	root := t.TempDir()
	regular := filepath.Join(root, "hooks.json")
	if err := os.WriteFile(regular, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if path, err := regularHookPath(regular); err != nil || path != regular {
		t.Fatalf("regular hook path=%q err=%v", path, err)
	}
	if _, err := regularHookPath(filepath.Join(root, "missing")); !os.IsNotExist(err) {
		t.Fatalf("missing hook path err=%v", err)
	}
	directory := filepath.Join(root, "directory")
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := regularHookPath(directory); err == nil {
		t.Fatal("directory accepted as hook configuration")
	}
	// symlink 自体は dotfile 管理(GNU Stow等)で使われるため、最終的な参照先が regular file なら許可する。
	symlinkToRegular := filepath.Join(root, "symlink-to-regular")
	if err := os.Symlink(regular, symlinkToRegular); err != nil {
		t.Fatal(err)
	}
	if path, err := regularHookPath(symlinkToRegular); err != nil || path != symlinkToRegular {
		t.Fatalf("symlink to regular file rejected: path=%q err=%v", path, err)
	}
	symlinkToDirectory := filepath.Join(root, "symlink-to-directory")
	if err := os.Symlink(directory, symlinkToDirectory); err != nil {
		t.Fatal(err)
	}
	if _, err := regularHookPath(symlinkToDirectory); err == nil {
		t.Fatal("symlink to directory accepted as hook configuration")
	}
	danglingSymlink := filepath.Join(root, "dangling-symlink")
	if err := os.Symlink(filepath.Join(root, "does-not-exist"), danglingSymlink); err != nil {
		t.Fatal(err)
	}
	if _, err := regularHookPath(danglingSymlink); !os.IsNotExist(err) {
		t.Fatalf("dangling symlink err=%v", err)
	}
	if got := stripTOMLComment(`key = 'quote # value'`); got != `key = 'quote # value'` {
		t.Fatalf("comment marker inside single quotes was stripped: %q", got)
	}
}
