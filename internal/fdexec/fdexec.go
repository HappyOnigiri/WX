// Package fdexec は descriptor に束縛したカレントディレクトリで process を起動する。
// os/exec の Cmd.Dir は path しか受けないため、fork 後・exec 前に fchdir(2) する。
package fdexec

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"golang.org/x/sys/unix"
)

const (
	Command = "__wx_exec_at_fd"
	FD      = 3
)

// Start は子 process で fd のディレクトリへ移動して argv[0] を直接 exec する command を作る。
// helper は wx（または本 package を組み込んだ実行ファイル）で、子の開始前に解決する。
func Start(ctx context.Context, helper string, fd *os.File, env []string, argv ...string) (*exec.Cmd, error) {
	if fd == nil {
		return nil, fmt.Errorf("descriptor-bound command requires a directory")
	}
	if len(argv) == 0 || argv[0] == "" {
		return nil, fmt.Errorf("descriptor-bound command requires an executable")
	}
	if helper == "" {
		var err error
		helper, err = os.Executable()
		if err != nil {
			return nil, fmt.Errorf("locate descriptor exec helper: %w", err)
		}
	}
	if !filepath.IsAbs(helper) {
		resolved, err := exec.LookPath(helper)
		if err != nil {
			return nil, fmt.Errorf("locate descriptor exec helper %q: %w", helper, err)
		}
		helper = resolved
	}
	executable, err := exec.LookPath(argv[0])
	if err != nil {
		return nil, fmt.Errorf("locate descriptor command %q: %w", argv[0], err)
	}
	childArgs := make([]string, 0, len(argv)+1)
	childArgs = append(childArgs, Command, executable)
	childArgs = append(childArgs, argv[1:]...)
	cmd := exec.CommandContext(ctx, helper, childArgs...)
	// helper は安全な固定ディレクトリで開始し、要求 process の実行前に fd へ移動する。
	cmd.Dir = string(filepath.Separator)
	cmd.Env = env
	cmd.ExtraFiles = []*os.File{fd}
	return cmd, nil
}

// Handle は隠し trampoline command を実行する。通常の wx 呼び出しでは handled=false を返す。
// init hook により、生成 main を持つ Go test binary からも descriptor 束縛を検証できる。
func Handle(args []string) (handled bool, exitCode int) {
	if len(args) < 2 || args[0] != Command {
		return false, 0
	}
	if err := unix.Fchdir(FD); err != nil {
		fmt.Fprintf(os.Stderr, "wx descriptor exec: fchdir: %v\n", err)
		return true, 1
	}
	// ディレクトリ FD は子の CWD 設定だけに使う。要求 process へ渡さず、長寿命 agent の間も保持しない。
	if err := unix.Close(FD); err != nil {
		fmt.Fprintf(os.Stderr, "wx descriptor exec: close directory fd: %v\n", err)
		return true, 1
	}
	if err := unix.Exec(args[1], args[1:], os.Environ()); err != nil {
		fmt.Fprintf(os.Stderr, "wx descriptor exec: exec %s: %v\n", args[1], err)
		return true, 1
	}
	return true, 1
}

func init() {
	if handled, code := Handle(os.Args[1:]); handled {
		os.Exit(code)
	}
}
