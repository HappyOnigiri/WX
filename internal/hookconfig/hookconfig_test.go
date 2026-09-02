package hookconfig

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestCodexHooksConfigEnabledBoundaries(t *testing.T) {
	tests := []struct {
		name        string
		data        string
		requirement bool
		want        bool
	}{
		{name: "empty", data: "", want: true},
		{name: "comments", data: "# hooks are enabled\nname = \"wx\" # inline comment", want: true},
		{name: "features table", data: "[features]\nhooks = true\ncodex_hooks = true", want: true},
		{name: "quoted keys", data: "[features]\n\"hooks\" = true\n'codex_hooks' = true", want: true},
		{name: "inline features", data: `features = { hooks = true, codex_hooks = true, nested = { value = "#" } }`, want: true},
		{name: "array of tables", data: "[[features.hooks]]\nname = \"wx\"", want: true},
		{name: "managed hooks disabled", data: "allow_managed_hooks_only = false", requirement: true, want: true},
		{name: "managed hooks required", data: "allow_managed_hooks_only = true", requirement: true, want: false},
		{name: "feature disabled", data: "[features]\nhooks = false", want: false},
		{name: "inline feature disabled", data: "features = { hooks = false }", want: false},
		{name: "malformed table", data: "[features", want: false},
		{name: "malformed array table", data: "[[features.hooks]", want: false},
		{name: "bare statement", data: "hooks", want: false},
		{name: "multiline string", data: "name = \"\"\"hooks\"\"\"", want: false},
		{name: "unbalanced inline table", data: "features = { hooks = true", want: false},
		{name: "inline field without value", data: "features = { hooks }", want: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := CodexHooksConfigEnabled([]byte(test.data), test.requirement); got != test.want {
				t.Fatalf("CodexHooksConfigEnabled(%q, %v) = %v, want %v", test.data, test.requirement, got, test.want)
			}
		})
	}
}

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

func TestHookConfigCommandParsingAndExecutableIdentity(t *testing.T) {
	for _, test := range []struct {
		name string
		cmd  string
		want bool
	}{
		{name: "plain command", cmd: "/bin/wx hook SessionStart", want: true},
		{name: "quoted executable", cmd: `"/bin/wx" hook SessionStart`, want: true},
		{name: "escaped space", cmd: `/tmp/wx\ binary hook SessionStart`, want: true},
		{name: "single quoted home literal", cmd: `'$HOME/wx' hook SessionStart`, want: true},
		{name: "newline", cmd: "/bin/wx hook\nSessionStart", want: false},
		{name: "pipeline", cmd: "/bin/wx | hook SessionStart", want: false},
		{name: "unterminated quote", cmd: `"/bin/wx hook SessionStart`, want: false},
		{name: "dangling escape", cmd: `/bin/wx\`, want: false},
		{name: "backtick substitution", cmd: "\"`uname`\" hook SessionStart", want: false},
		{name: "invalid double quote escape", cmd: `"/bin/wx\q" hook SessionStart`, want: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			fields, ok := splitHookCommand(test.cmd)
			if ok != test.want {
				t.Fatalf("splitHookCommand(%q) ok=%v, want %v; fields=%v", test.cmd, ok, test.want, fields)
			}
			if test.want && len(fields) != 3 {
				t.Fatalf("splitHookCommand(%q) fields=%v, want 3 fields", test.cmd, fields)
			}
		})
	}

	executable, err := CurrentExecutable()
	if err != nil {
		t.Fatal(err)
	}
	if !isExactWXHookCommandForExecutable(executable+" hook SessionStart", "SessionStart", executable) {
		t.Fatal("current executable command was not recognized")
	}
	for _, command := range []string{
		executable + " hook UserPromptSubmit",
		executable + " run SessionStart",
		"/definitely/missing/wx hook SessionStart",
	} {
		if isExactWXHookCommandForExecutable(command, "SessionStart", executable) {
			t.Fatalf("unsafe or mismatched command accepted: %q", command)
		}
	}
	if resolved, ok := resolveHookExecutable(executable); !ok || resolved != executable {
		t.Fatalf("resolveHookExecutable(%q) = %q, %v", executable, resolved, ok)
	}
	if _, ok := resolveHookExecutable("relative/wx"); ok {
		t.Fatal("relative hook executable accepted")
	}
	if sameExecutable(executable, filepath.Join(os.TempDir(), "missing-wx-executable")) {
		t.Fatal("missing executable considered identical")
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
	if !readinessHookCommandValid(validCommand) {
		t.Fatal("valid command hook rejected")
	}
	for _, invalid := range []readinessHookCommand{
		{Type: "unknown"},
		{Type: "command"},
		{Type: "command", Command: "/bin/wx", Disabled: json.RawMessage(`"false"`)},
		{Type: "command", Command: "/bin/wx", Timeout: json.RawMessage("null")},
		{Type: "command", Command: "/bin/wx", AdditionalContextLimit: json.RawMessage("1.5")},
	} {
		if readinessHookCommandValid(invalid) {
			t.Fatalf("invalid command hook accepted: %+v", invalid)
		}
	}
	if !readinessHookGroupValid(readinessHookGroup{Matcher: json.RawMessage(`"*"`), Hooks: []readinessHookCommand{validCommand}}) {
		t.Fatal("valid hook group rejected")
	}
	for _, invalid := range []readinessHookGroup{
		{Disabled: json.RawMessage("true"), Hooks: []readinessHookCommand{validCommand}},
		{Matcher: json.RawMessage("null"), Hooks: []readinessHookCommand{validCommand}},
		{Matcher: json.RawMessage(`"*"`)},
	} {
		if readinessHookGroupValid(invalid) {
			t.Fatalf("invalid hook group accepted: %+v", invalid)
		}
	}
}

func TestReadinessHookDocumentRejectsMalformedAndDisabledHooks(t *testing.T) {
	required := map[string]string{"SessionStart": "session-start"}
	executable, err := CurrentExecutable()
	if err != nil {
		t.Fatal(err)
	}
	valid := `{"disableAllHooks":false,"hooks":{"SessionStart":[{"matcher":"*","hooks":[{"type":"command","command":"` + executable + ` hook session-start","disabled":false,"async":false,"once":false}]}]}}`
	if !readinessHookDocumentMatches([]byte(valid), required, executable) {
		t.Fatal("valid readiness hook document rejected")
	}
	for _, data := range []string{
		"not json",
		`{"disableAllHooks":true}`,
		`{"disableAllHooks":false}`,
		`{"disableAllHooks":false,"hooks":{"SessionStart":"wrong"}}`,
		`{"disableAllHooks":false,"hooks":{"SessionStart":[{"matcher":"*","hooks":[{"type":"command","command":"` + executable + ` hook session-start","async":true}]}]}}`,
	} {
		if readinessHookDocumentMatches([]byte(data), required, executable) {
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
	symlink := filepath.Join(root, "symlink")
	if err := os.Symlink(regular, symlink); err != nil {
		t.Fatal(err)
	}
	if _, err := regularHookPath(symlink); err == nil {
		t.Fatal("symlink accepted as hook configuration")
	}
	if got := stripTOMLComment(`key = 'quote # value'`); got != `key = 'quote # value'` {
		t.Fatalf("comment marker inside single quotes was stripped: %q", got)
	}
}
