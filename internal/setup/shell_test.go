package setup

import (
	"context"
	"errors"
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
	if _, err := Apply(ctx, fixture.options(), step, ActionInstall, ""); err != nil {
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
	if _, err := Apply(ctx, fixture.options(), divergent, ActionUpdate, ""); err != nil {
		t.Fatal(err)
	}
	if got := readSetupFile(t, rc); !strings.HasPrefix(got, "tail\n") || !strings.Contains(got, shellBlockBegin) {
		t.Fatalf("update produced:\n%s", got)
	}
	if _, err := Apply(ctx, fixture.options(), collectShellPath(), ActionRemove, ""); err != nil {
		t.Fatal(err)
	}
	if got := readSetupFile(t, rc); got != "tail\n" {
		t.Fatalf("remove left content behind:\n%q", got)
	}
}

// TestShellPathStaysQuietWhenTheStartupFileAlreadyAddsTheDirectory は、利用者が自分で書いた
// PATH 行を wx の block で重ねないことを確認する。$HOME 表記でも同じ行と見なす。
func TestShellPathStaysQuietWhenTheStartupFileAlreadyAddsTheDirectory(t *testing.T) {
	for _, line := range []string{
		`export PATH="$HOME/.local/bin:$PATH"`,
		`export PATH="${HOME}/.local/bin:$PATH"`,
		`export PATH=~/.local/bin:$PATH`,
	} {
		t.Run(line, func(t *testing.T) {
			fixture := newSetupFixture(t)
			writeSetupFile(t, filepath.Join(fixture.home, ".zshrc"), line+"\n")
			step := collectShellPath()
			if step.State != StatePresent || len(step.Options) != 0 {
				t.Fatalf("startup file already adds the directory=%+v", step)
			}
			if step.Detail.ID != "setup.detail.shell_path_external" {
				t.Fatalf("detail=%+v", step.Detail)
			}
		})
	}
}

// TestShellPathIgnoresTheProcessPathWhenTheStartupFileHasNoLine は、install.sh の案内どおり
// export だけした利用者が present と表示されないことを確認する。その場合 PATH は新しい端末で失われる。
func TestShellPathIgnoresTheProcessPathWhenTheStartupFileHasNoLine(t *testing.T) {
	fixture := newSetupFixture(t)
	binDirectory := filepath.Join(fixture.home, ".local", "bin")
	writeSetupFile(t, filepath.Join(fixture.home, ".zshrc"), "alias ll='ls -l'\n")
	t.Setenv("PATH", binDirectory+":"+os.Getenv("PATH"))
	step := collectShellPath()
	if step.State != StateAbsent {
		t.Fatalf("exported but not persisted=%+v", step)
	}
	if !hasMessageID(step.Reasons, "setup.reason.path_session_only") {
		t.Fatalf("reasons=%v", step.Reasons)
	}
}

// TestShellPathIgnoresCommentedPathLines はコメント行を根拠にしないことを確認する。
func TestShellPathIgnoresCommentedPathLines(t *testing.T) {
	fixture := newSetupFixture(t)
	writeSetupFile(t, filepath.Join(fixture.home, ".zshrc"), `# export PATH="$HOME/.local/bin:$PATH"`+"\n")
	if step := collectShellPath(); step.State != StateAbsent {
		t.Fatalf("commented line=%+v", step)
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
	if step.State != StateUnknown || len(step.Options) != 0 || !hasMessageID(step.Reasons, "setup.reason.startup_symlink") {
		t.Fatalf("symlinked startup file=%+v", step)
	}
	if _, err := Apply(context.Background(), fixture.options(), Step{ID: stepShellPath}, ActionInstall, ""); err == nil {
		t.Fatal("a step without a target was applied")
	}
}

// TestShellBlockQuotesAnUnexpectedDirectory は既定以外の bin directory でも 1 行に収まることを確認する。
func TestShellBlockQuotesAnUnexpectedDirectory(t *testing.T) {
	_ = newSetupFixture(t)
	if got := shellBlock("/opt/wx bin"); !strings.Contains(got, `export PATH="/opt/wx bin":$PATH`) {
		t.Fatalf("block=%q", got)
	}
	if _, found, _ := shellManagedBlock("no markers here"); found {
		t.Fatal("a block was found in unrelated content")
	}
	block, found, terminated := shellManagedBlock(shellBlockBegin + "\nline\n")
	if !found || terminated || block != "" {
		t.Fatalf("unterminated block=%q,%v,%v", block, found, terminated)
	}
}

// TestShellManagedBlockRecognizesBoundaryMarkers は、開始 marker を先頭に置き、
// 終了 marker を直後に続けた最小の block も完全な block として扱うことを確認する。
// これにより begin == 0 と end == len(contents) の両方を検査できる。
func TestShellManagedBlockRecognizesBoundaryMarkers(t *testing.T) {
	contents := shellBlockBegin + shellBlockEnd
	block, found, terminated := shellManagedBlock(contents)
	if !found || !terminated || block != contents {
		t.Fatalf("boundary block=%q,%v,%v", block, found, terminated)
	}
}

// TestShellPathRefusesAnUnterminatedBlock は終了 marker が無いとき、後続の利用者の設定を消さないことを確認する。
func TestShellPathRefusesAnUnterminatedBlock(t *testing.T) {
	fixture := newSetupFixture(t)
	rc := filepath.Join(fixture.home, ".zshrc")
	contents := shellBlockBegin + "\nexport PATH=\"$HOME/.local/bin:$PATH\"\nalias ll='ls -l'\nexport EDITOR=vim\n"
	writeSetupFile(t, rc, contents)

	step := collectShellPath()
	if step.State != StateUnknown || len(step.Options) != 0 {
		t.Fatalf("unterminated block=%+v", step)
	}
	if !strings.Contains(englishJoin(step.Reasons), shellBlockEnd) {
		t.Fatalf("reasons=%v", step.Reasons)
	}
	for _, action := range []Action{ActionUpdate, ActionRemove} {
		target := Step{ID: stepShellPath, Target: rc, Desired: filepath.Join(fixture.home, ".local", "bin")}
		if err := applyShellPath(target, action); err == nil {
			t.Fatalf("%s rewrote a file whose block has no end marker", action)
		}
	}
	if got := readSetupFile(t, rc); got != contents {
		t.Fatalf("the startup file was changed:\n%s", got)
	}
}

// TestDirectoryOnPathRequiresAnExactEntry は PATH の要素全体が一致したときだけ
// directoryOnPath が true を返すことを確認する。先頭以外の要素も含む PATH では、
// 比較を反転しても偶然 true になるため、単一要素の fixture で境界を観測する。
func TestDirectoryOnPathRequiresAnExactEntry(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "bin")
	t.Setenv("PATH", directory)
	if !directoryOnPath(directory) {
		t.Fatal("an exact PATH entry was not found")
	}
	if directoryOnPath(filepath.Dir(directory)) {
		t.Fatal("a path prefix was treated as an exact PATH entry")
	}
}

// TestWriteStartupFileReportsFilesystemBoundaryErrors は atomic rename と親 directory の
// sync が失敗したとき、見かけ上の成功を返さないことを確認する。rename と directory
// open を一時 fixture の fake に差し替えるため、実際の dotfile や shell は変更しない。
func TestWriteStartupFileReportsFilesystemBoundaryErrors(t *testing.T) {
	t.Run("rename", func(t *testing.T) {
		target := t.TempDir()
		renameErr := errors.New("rename refused")
		rename := func(string, string) error { return renameErr }
		if err := writeStartupFileWithOps(filepath.Join(target, ".zshrc"), []byte("data"), 0o600, rename, os.Open); !errors.Is(err, renameErr) {
			t.Fatal("renaming a temporary file over a directory was reported as success")
		}
	})
	t.Run("directory sync", func(t *testing.T) {
		directory := t.TempDir()
		path := filepath.Join(directory, ".zshrc")
		openErr := errors.New("directory open refused")
		open := func(string) (*os.File, error) { return nil, openErr }
		if err := writeStartupFileWithOps(path, []byte("data"), 0o600, os.Rename, open); !errors.Is(err, openErr) {
			t.Fatal("syncing an unreadable parent directory was reported as success")
		}
	})
}

func readSetupFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
