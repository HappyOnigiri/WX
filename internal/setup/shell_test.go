package setup

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestShellPathAddsAndRemovesTheManagedBlock(t *testing.T) {
	fixture := newSetupFixture(t)
	ctx := context.Background()
	rc := filepath.Join(fixture.home, ".zshrc")
	// 末尾改行が無いファイルへそのまま追記すると最終行が壊れる。
	writeSetupFile(t, rc, "alias ll='ls -l'")

	steps, err := Collect(ctx, fixture.options())
	if err != nil {
		t.Fatal(err)
	}
	step := stepByID(t, steps, stepShellPath)
	if step.State != StateAbsent || step.Target != rc {
		t.Fatalf("shell path=%+v", step)
	}
	if err := Apply(ctx, fixture.options(), step, ActionInstall, ""); err != nil {
		t.Fatal(err)
	}
	contents := readSetupFile(t, rc)
	if !strings.HasPrefix(contents, "alias ll='ls -l'\n"+shellBlockBegin) {
		t.Fatalf("the existing line was damaged:\n%s", contents)
	}
	if !strings.Contains(contents, `export PATH="$HOME/.local/bin:$PATH"`) {
		t.Fatalf("the PATH line was not written:\n%s", contents)
	}
	if collectShellPath().State != StatePresent {
		t.Fatal("the managed block was not recognized")
	}

	// 中身を書き換えると divergent になり、update で書き戻せる。
	writeSetupFile(t, rc, shellBlockBegin+"\nexport PATH=/somewhere:$PATH\n"+shellBlockEnd+"\ntail\n")
	divergent := collectShellPath()
	if divergent.State != StateDivergent {
		t.Fatalf("edited block=%+v", divergent)
	}
	if err := Apply(ctx, fixture.options(), divergent, ActionUpdate, ""); err != nil {
		t.Fatal(err)
	}
	if got := readSetupFile(t, rc); !strings.HasPrefix(got, "tail\n") || !strings.Contains(got, shellBlockBegin) {
		t.Fatalf("update produced:\n%s", got)
	}
	if err := Apply(ctx, fixture.options(), collectShellPath(), ActionRemove, ""); err != nil {
		t.Fatal(err)
	}
	if got := readSetupFile(t, rc); got != "tail\n" {
		t.Fatalf("remove left content behind:\n%q", got)
	}
}

func TestShellPathStaysQuietWhenTheDirectoryIsAlreadyOnPath(t *testing.T) {
	fixture := newSetupFixture(t)
	binDirectory := filepath.Join(fixture.home, ".local", "bin")
	t.Setenv("PATH", binDirectory+":"+os.Getenv("PATH"))
	step := collectShellPath()
	if step.State != StatePresent || len(step.Options) != 0 {
		t.Fatalf("already on PATH=%+v", step)
	}
	if !strings.Contains(step.Detail, "already on PATH") {
		t.Fatalf("detail=%q", step.Detail)
	}
}

func TestShellPathRefusesUnknownShellsAndManagedFiles(t *testing.T) {
	fixture := newSetupFixture(t)
	t.Setenv("SHELL", "/usr/bin/fish")
	if step := collectShellPath(); step.State != StateUnknown || len(step.Options) != 0 {
		t.Fatalf("unknown shell=%+v", step)
	}
	t.Setenv("SHELL", "/bin/zsh")
	// dotfile 管理下の起動ファイルは書かず、案内だけを出す。
	managed := filepath.Join(fixture.home, "dotfiles", "zshrc")
	writeSetupFile(t, managed, "")
	rc := filepath.Join(fixture.home, ".zshrc")
	if err := os.Symlink(managed, rc); err != nil {
		t.Fatal(err)
	}
	step := collectShellPath()
	if step.State != StateUnknown || len(step.Options) != 0 || !strings.Contains(strings.Join(step.Reasons, " "), "symlink") {
		t.Fatalf("symlinked startup file=%+v", step)
	}
	if err := Apply(context.Background(), fixture.options(), Step{ID: stepShellPath}, ActionInstall, ""); err == nil {
		t.Fatal("a step without a target was applied")
	}
}

// TestShellBlockQuotesAnUnexpectedDirectory は既定以外の bin directory でも 1 行に収まることを確認する。
func TestShellBlockQuotesAnUnexpectedDirectory(t *testing.T) {
	_ = newSetupFixture(t)
	if got := shellBlock("/opt/wx bin"); !strings.Contains(got, `export PATH="/opt/wx bin":$PATH`) {
		t.Fatalf("block=%q", got)
	}
	if _, found := shellManagedBlock("no markers here"); found {
		t.Fatal("a block was found in unrelated content")
	}
	if block, found := shellManagedBlock(shellBlockBegin + "\nline\n"); !found || !strings.HasSuffix(block, "line\n") {
		t.Fatalf("unterminated block=%q,%v", block, found)
	}
}

func readSetupFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
