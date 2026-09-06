package hookconfig

import (
	"os"
	"path/filepath"
	"testing"
)

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

func TestResolveHookExecutableRejectsBareWXWhenUnavailable(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	if resolved, ok := resolveHookExecutable("wx"); ok || resolved != "" {
		t.Fatalf("unavailable bare wx executable resolved to %q, ok=%v", resolved, ok)
	}
}

// TestResolveHookExecutableExpandsHomeAndTildeVariants は resolveHookExecutable の $HOME と ~ の展開経路を確認する。
// os.UserHomeDir 自体が失敗する場合も含める。
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
