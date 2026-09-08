package setup

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// shell の起動ファイルに書き込むブロックの目印。update と remove を可能にするために付ける。
const (
	shellBlockBegin = "# wx: managed by wx setup"
	shellBlockEnd   = "# wx: end"
)

// shellBlock は marker で囲んだ追記内容を返す。展開されない形で書き、利用者の他の PATH 設定を壊さない。
func shellBlock(binDirectory string) string {
	line := `export PATH="$HOME/.local/bin:$PATH"`
	if binDirectory != defaultBinDirectory() {
		line = fmt.Sprintf("export PATH=%q:$PATH", binDirectory)
	}
	return shellBlockBegin + "\n" + line + "\n" + shellBlockEnd + "\n"
}

func defaultBinDirectory() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".local", "bin")
}

// collectShellPath は $SHELL から起動ファイルを選び、wx のブロックの有無と内容を見る。
// 起動ファイル自体が symlink のことがあるため（dotfile 管理）、書き込みはせず unknown として案内だけを出す。
func collectShellPath() Step {
	step := Step{ID: stepShellPath, Title: "Shell PATH"}
	binDirectory := defaultBinDirectory()
	if binDirectory == "" {
		return unknownStep(step, "the home directory cannot be resolved")
	}
	step.Desired = binDirectory
	path, err := shellStartupFile()
	if err != nil {
		return unknownStep(step, err.Error())
	}
	step.Target = path
	step.Detail = "adds " + binDirectory + " to PATH for new terminals"
	info, err := os.Lstat(path)
	switch {
	case err == nil && info.Mode()&os.ModeSymlink != 0:
		return unknownStep(step, path+" is a symlink; take it out of dotfile management or add the line yourself")
	case err == nil && !info.Mode().IsRegular():
		return unknownStep(step, path+" is not a regular file")
	case err != nil && !errors.Is(err, os.ErrNotExist):
		return unknownStep(step, err.Error())
	}
	contents := ""
	if err == nil {
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return unknownStep(step, readErr.Error())
		}
		contents = string(data)
	}
	block, found, terminated := shellManagedBlock(contents)
	switch {
	case found && !terminated:
		return unknownStep(step, path+" has "+shellBlockBegin+" without "+shellBlockEnd+"; restore the missing end marker or remove the block yourself")
	case found && block == shellBlock(binDirectory):
		step.State = StatePresent
	case found:
		step.State = StateDivergent
		step.Reasons = append(step.Reasons, "the block managed by wx does not match what wx would write")
	case directoryOnPath(binDirectory):
		// 別の手段で PATH に入っているので、wx が起動ファイルを触る理由がない。
		step.State = StatePresent
		step.Detail = binDirectory + " is already on PATH"
		step.Options, step.Default = stepOptions(StatePresent, []Action{ActionKeep})
		return step
	default:
		step.State = StateAbsent
	}
	step.Options, step.Default = stepOptions(step.State, allActions)
	return step
}

// shellStartupFile は $SHELL の basename から、対話 shell が読む起動ファイルを選ぶ。
func shellStartupFile() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	switch filepath.Base(os.Getenv("SHELL")) {
	case "zsh":
		return filepath.Join(home, ".zshrc"), nil
	case "bash":
		return filepath.Join(home, ".bash_profile"), nil
	default:
		return "", errors.New("wx cannot tell which startup file this shell reads; add the PATH line yourself")
	}
}

// shellManagedBlock は marker で囲まれた wx のブロックを返す。
// terminated は終了 marker が見つかったかを表す。開始 marker だけのファイルで末尾までを wx のものと見なすと、
// 後から書かれた利用者の設定を update・remove が消してしまうため、範囲を確定できないことを呼び出し側へ伝える。
func shellManagedBlock(contents string) (block string, found bool, terminated bool) {
	begin := strings.Index(contents, shellBlockBegin)
	if begin < 0 {
		return "", false, false
	}
	offset := strings.Index(contents[begin:], shellBlockEnd)
	if offset < 0 {
		return "", true, false
	}
	end := begin + offset + len(shellBlockEnd)
	if end < len(contents) && contents[end] == '\n' {
		end++
	}
	return contents[begin:end], true, true
}

// directoryOnPath は現在のプロセスの PATH に directory が含まれるかを返す。
func directoryOnPath(directory string) bool {
	for _, entry := range filepath.SplitList(os.Getenv("PATH")) {
		if entry == directory {
			return true
		}
	}
	return false
}

// applyShellPath は marker ブロックを置き換える、または取り除く。
// 末尾改行が無いファイルへそのまま追記すると最終行が壊れるため、追記前に補う。
func applyShellPath(step Step, action Action) error {
	path := step.Target
	if path == "" {
		return errors.New("the shell startup file is unknown")
	}
	contents := ""
	data, err := os.ReadFile(path)
	switch {
	case err == nil:
		contents = string(data)
	case !errors.Is(err, os.ErrNotExist):
		return err
	}
	block, found, terminated := shellManagedBlock(contents)
	switch {
	case found && !terminated:
		// 収集時に unknown で弾く状態だが、その後にファイルが変わっていることもあるので書く前に確かめる。
		return errors.New(path + " has " + shellBlockBegin + " without " + shellBlockEnd + "; wx cannot tell where its block ends")
	case found:
		contents = strings.Replace(contents, block, "", 1)
	}
	if action != ActionRemove {
		if contents != "" && !strings.HasSuffix(contents, "\n") {
			contents += "\n"
		}
		contents += shellBlock(step.Desired)
	}
	mode := os.FileMode(0o600)
	if info, err := os.Stat(path); err == nil {
		mode = info.Mode().Perm()
	}
	return os.WriteFile(path, []byte(contents), mode)
}
