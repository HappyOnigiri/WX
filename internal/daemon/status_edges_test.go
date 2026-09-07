package daemon

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/HappyOnigiri/WX/internal/hookconfig"
)

func TestDiagnosticFilesystemAndHookChecks(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	regular := filepath.Join(home, "regular")
	if err := os.WriteFile(regular, []byte("1234"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := diagnosticPath(regular, 0, 0o600); got != "ok" {
		t.Fatalf("regular diagnostic=%q", got)
	}
	if got := diagnosticPath(regular, os.ModeDir, 0o700); got != "not a directory" {
		t.Fatalf("directory diagnostic=%q", got)
	}
	if err := os.Chmod(regular, 0o644); err != nil {
		t.Fatal(err)
	}
	if got := diagnosticPath(regular, 0, 0o600); !strings.Contains(got, "permissions") {
		t.Fatalf("permission diagnostic=%q", got)
	}
	link := filepath.Join(home, "link")
	if err := os.Symlink(regular, link); err != nil {
		t.Fatal(err)
	}
	if got := diagnosticPath(link, 0, 0o600); got != "unsafe symlink" {
		t.Fatalf("symlink diagnostic=%q", got)
	}
	if got := diagnosticPath(filepath.Join(home, "missing"), 0, 0o600); !strings.Contains(got, "no such file") {
		t.Fatalf("missing diagnostic=%q", got)
	}
	if got := diagnosticPath("", 0, 0o600); got != "path unavailable" {
		t.Fatalf("empty diagnostic=%q", got)
	}
	if got := diagnosticPath(regular, os.ModeSocket, 0o600); got != "not a Unix socket" {
		t.Fatalf("socket diagnostic=%q", got)
	}
	if got := diagnosticPath(home, 0, 0o700); got != "not a regular file" {
		t.Fatalf("regular-file diagnostic=%q", got)
	}
	executable, err := hookconfig.CurrentExecutable()
	if err != nil {
		t.Fatal(err)
	}
	writeHooks := func(t *testing.T, path, document string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(document), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	canonical := func(event string) string {
		return fmt.Sprintf(`{"type":"command","command":%q}`, executable+" hook "+event)
	}
	valid := fmt.Sprintf(`{"hooks":{"SessionStart":[{"hooks":[%s]}],"UserPromptSubmit":[{"hooks":[%s]}],"PreToolUse":[{"hooks":[%s]}]}}`, canonical("session-start"), canonical("user-prompt-submit"), canonical("pre-tool-use"))
	claudePath := filepath.Join(home, ".claude", "settings.json")
	codexPath := filepath.Join(home, ".codex", "hooks.json")
	for _, test := range []struct {
		name     string
		document string
		want     bool
	}{
		{name: "valid canonical hooks", document: valid, want: true},
		{name: "matcher applies to one event", document: strings.Replace(valid, `[{"hooks":[`, `[{"matcher":"startup","hooks":[`, 1), want: false},
		{name: "disable all hooks", document: strings.Replace(valid, `{"hooks":`, `{"disableAllHooks":true,"hooks":`, 1), want: false},
		{name: "wrong executable", document: strings.ReplaceAll(valid, executable, filepath.Join(home, "other-wx")), want: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			other := filepath.Join(home, "other-wx")
			if test.name == "wrong executable" {
				if err := os.WriteFile(other, []byte("#!/bin/sh\n"), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			writeHooks(t, claudePath, test.document)
			if got := hookconfig.Available("claude"); got != test.want {
				t.Fatalf("claude diagnostic=%v, want %v", got, test.want)
			}
		})
	}
	writeHooks(t, claudePath, valid)
	localPath := filepath.Join(home, ".claude", "settings.local.json")
	writeHooks(t, localPath, strings.Replace(valid, `[{"hooks":[`, `[{"disabled":true,"hooks":[`, 1))
	if hookconfig.Available("claude") {
		t.Fatal("invalid local Claude hooks were ignored in favor of global hooks")
	}
	if err := os.Remove(localPath); err != nil {
		t.Fatal(err)
	}
	if !hookconfig.Available("claude") {
		t.Fatal("valid global Claude hooks were not detected")
	}

	for _, separator := range []string{"\n", "\r"} {
		document := fmt.Sprintf(`{"hooks":{"SessionStart":[{"hooks":[{"type":"command","command":%q}]}],"UserPromptSubmit":[{"hooks":[%s]}],"PreToolUse":[{"hooks":[%s]}]}}`, executable+separator+"hook session-start", canonical("user-prompt-submit"), canonical("pre-tool-use"))
		writeHooks(t, codexPath, document)
		if hookconfig.Available("codex") {
			t.Fatalf("shell separator %q was accepted", separator)
		}
	}
	wrapper := fmt.Sprintf(`{"hooks":{"SessionStart":[{"hooks":[{"type":"command","command":%q}]}],"UserPromptSubmit":[{"hooks":[%s]}],"PreToolUse":[{"hooks":[%s]}]}}`, "sh -c '"+executable+" hook session-start'", canonical("user-prompt-submit"), canonical("pre-tool-use"))
	writeHooks(t, codexPath, wrapper)
	if hookconfig.Available("codex") {
		t.Fatal("shell wrapper was accepted as a canonical readiness hook")
	}
	writeHooks(t, codexPath, valid)
	if !hookconfig.Available("codex") {
		t.Fatal("valid canonical Codex hooks were not detected")
	}
}
