package cli

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReadinessHooksRequireCommandsUnderMatchingEvents(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	path := filepath.Join(home, ".codex", "hooks.json")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	valid := `{"hooks":{"SessionStart":[{"hooks":[{"type":"command","command":"/usr/local/bin/wx hook session-start"}]}],"UserPromptSubmit":[{"hooks":[{"type":"command","command":"wx hook user-prompt-submit"}]}],"PreToolUse":[{"hooks":[{"type":"command","command":"wx hook pre-tool-use"}]}]}}`
	if err := os.WriteFile(path, []byte(valid), 0600); err != nil {
		t.Fatal(err)
	}
	if !readinessHooksAvailable("codex") {
		t.Fatal("structurally valid hooks were not detected")
	}

	wrongEvent := `{"hooks":{"SessionStart":[{"hooks":[{"type":"command","command":"wx hook session-start"},{"type":"command","command":"wx hook user-prompt-submit"},{"type":"command","command":"wx hook pre-tool-use"}]}]}}`
	if err := os.WriteFile(path, []byte(wrongEvent), 0600); err != nil {
		t.Fatal(err)
	}
	if readinessHooksAvailable("codex") {
		t.Fatal("commands under the wrong events enabled asynchronous startup")
	}
}

func TestReadinessHooksRejectDisabledSubstringAndMalformedConfigurations(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	path := filepath.Join(home, ".codex", "hooks.json")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	cases := []string{
		`{"description":"wx hook session-start wx hook user-prompt-submit wx hook pre-tool-use"}`,
		`{"hooks":{"SessionStart":[{"disabled":true,"hooks":[{"type":"command","command":"wx hook session-start"}]}],"UserPromptSubmit":[{"hooks":[{"type":"command","command":"wx hook user-prompt-submit"}]}],"PreToolUse":[{"hooks":[{"type":"command","command":"wx hook pre-tool-use"}]}]}}`,
		`{"hooks":`,
		`{"hooks":{"SessionStart":[{"hooks":[{"type":"command","command":"wx hook session-start && false"}]}],"UserPromptSubmit":[],"PreToolUse":[]}}`,
	}
	for _, document := range cases {
		if err := os.WriteFile(path, []byte(document), 0600); err != nil {
			t.Fatal(err)
		}
		if readinessHooksAvailable("codex") {
			t.Fatalf("unsafe hook configuration was accepted: %s", document)
		}
	}
}
