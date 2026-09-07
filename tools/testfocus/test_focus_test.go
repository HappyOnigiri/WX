// testfocus は scripts/test-focus.sh の引数転送と終了状態を検査するテスト専用パッケージである。
// 実装はshell scriptなので、偽のgoコマンドへ渡った引数を記録して契約を確かめる。
package testfocus

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const (
	scriptPath        = "../../scripts/test-focus.sh"
	daemonTest        = "TestLeaseArchiveAndRestorePreservesGitState"
	fakeGoName        = "fake-go.sh"
	argumentSeparator = "\x00"
)

// fakeGo は受け取った引数をNUL区切りでファイルへ書き、指定の終了コードで終わるgoの代役を作る。
// 記録先と終了コードは環境変数で渡し、shellの引用が壊れていれば引数の境界の違いとして表れる。
func fakeGo(t *testing.T, directory string) string {
	t.Helper()
	path := filepath.Join(directory, fakeGoName)
	source := "#!/bin/sh\n" +
		": > \"$FAKE_GO_ARGS\"\n" +
		"for argument in \"$@\"; do printf '%s\\0' \"$argument\" >> \"$FAKE_GO_ARGS\"; done\n" +
		"exit \"${FAKE_GO_STATUS:-0}\"\n"
	if err := os.WriteFile(path, []byte(source), 0o755); err != nil {
		t.Fatalf("write fake go: %v", err)
	}
	return path
}

type result struct {
	status int
	stderr string
	args   []string
}

// runFocus はscriptを走らせ、偽goが記録した引数と終了状態を返す。
// environmentは PKG=... のような追加の環境変数で、空文字の値は未設定として扱われる。
func runFocus(t *testing.T, environment ...string) result {
	t.Helper()
	directory := t.TempDir()
	goPath := fakeGo(t, directory)
	argsPath := filepath.Join(directory, "args")

	command := exec.Command("/bin/sh", scriptPath)
	command.Env = append(os.Environ(), "GO="+goPath, "FAKE_GO_ARGS="+argsPath, "PKG=", "RUN=", "FAKE_GO_STATUS=")
	command.Env = append(command.Env, environment...)
	var stderr bytes.Buffer
	command.Stderr = &stderr
	command.Stdout = &stderr

	status := 0
	if err := command.Run(); err != nil {
		var exitError *exec.ExitError
		if !errors.As(err, &exitError) {
			t.Fatalf("run script: %v (output %q)", err, stderr.String())
		}
		status = exitError.ExitCode()
	}

	recorded, err := os.ReadFile(argsPath)
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("read recorded arguments: %v", err)
	}
	var args []string
	if trimmed := strings.TrimSuffix(string(recorded), argumentSeparator); trimmed != "" {
		args = strings.Split(trimmed, argumentSeparator)
	}
	return result{status: status, stderr: stderr.String(), args: args}
}

func TestScriptRejectsAMissingPackage(t *testing.T) {
	t.Parallel()
	got := runFocus(t)
	if got.status != 2 {
		t.Fatalf("status=%d, want 2 (output %q)", got.status, got.stderr)
	}
	if len(got.args) != 0 {
		t.Fatalf("go was invoked with %q", got.args)
	}
	for _, want := range []string{"PKG is required", "usage: make test-focus"} {
		if !strings.Contains(got.stderr, want) {
			t.Fatalf("output %q does not mention %q", got.stderr, want)
		}
	}
}

func TestScriptRejectsWiderTargetsThanASinglePackage(t *testing.T) {
	t.Parallel()
	for name, packages := range map[string]string{
		"two packages":     "./internal/daemon ./internal/state",
		"partial wildcard": "./internal/...",
		"absolute path":    "/tmp/repository/internal/daemon",
		"bare import path": "internal/daemon",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got := runFocus(t, "PKG="+packages)
			if got.status != 2 {
				t.Fatalf("status=%d, want 2 (output %q)", got.status, got.stderr)
			}
			if len(got.args) != 0 {
				t.Fatalf("go was invoked with %q", got.args)
			}
		})
	}
}

func TestScriptForwardsThePackageAndRunPattern(t *testing.T) {
	t.Parallel()
	// RUNは正規表現のまま1つの引数へ渡す必要があり、|や*が分割・展開されないことを確かめる。
	pattern := "TestLease.*PreservesGitState|TestRestore"
	got := runFocus(t, "PKG=./internal/daemon", "RUN="+pattern)
	if got.status != 0 {
		t.Fatalf("status=%d, want 0 (output %q)", got.status, got.stderr)
	}
	want := []string{"test", "-count=1", "-shuffle=on", "-run", pattern, "./internal/daemon"}
	if strings.Join(got.args, argumentSeparator) != strings.Join(want, argumentSeparator) {
		t.Fatalf("args=%q, want %q", got.args, want)
	}
	if !strings.Contains(got.stderr, strings.Join(want, " ")) {
		t.Fatalf("output %q does not show the executed arguments", got.stderr)
	}
	if !strings.Contains(got.stderr, "the final gate is make ci") {
		t.Fatalf("output %q does not point at the final gate", got.stderr)
	}
}

func TestScriptRunsTheWholePackageWithoutARunPattern(t *testing.T) {
	t.Parallel()
	for name, packages := range map[string]string{
		"single package":     "./internal/daemon",
		"explicit whole set": "./...",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got := runFocus(t, "PKG="+packages)
			if got.status != 0 {
				t.Fatalf("status=%d, want 0 (output %q)", got.status, got.stderr)
			}
			want := []string{"test", "-count=1", "-shuffle=on", packages}
			if strings.Join(got.args, argumentSeparator) != strings.Join(want, argumentSeparator) {
				t.Fatalf("args=%q, want %q", got.args, want)
			}
			for _, forbidden := range []string{"-run", "-short", "-race"} {
				for _, argument := range got.args {
					if argument == forbidden {
						t.Fatalf("args=%q must not contain %q", got.args, forbidden)
					}
				}
			}
		})
	}
}

// 表示処理が失敗を成功へ変えないことを確かめる。
func TestScriptPropagatesTheGoExitStatus(t *testing.T) {
	t.Parallel()
	got := runFocus(t, "PKG=./internal/daemon", "RUN="+daemonTest, "FAKE_GO_STATUS=3")
	if got.status != 3 {
		t.Fatalf("status=%d, want 3 (output %q)", got.status, got.stderr)
	}
	if !strings.Contains(got.stderr, "the final gate is make ci") {
		t.Fatalf("output %q does not point at the final gate", got.stderr)
	}
}

// Makefileのtargetと計画で約束した呼び出し方が、環境変数だけで完結していることを確かめる。
func TestMakefileExportsTheFocusVariables(t *testing.T) {
	t.Parallel()
	content, err := os.ReadFile("../../Makefile")
	if err != nil {
		t.Fatalf("read Makefile: %v", err)
	}
	for _, want := range []string{"export PKG", "export RUN", "scripts/test-focus.sh"} {
		if !strings.Contains(string(content), want) {
			t.Fatalf("Makefile does not contain %q", want)
		}
	}
}
