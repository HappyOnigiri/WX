// Package fdexec starts a process with a descriptor-bound current directory.
// Go's os/exec API accepts only a pathname for Cmd.Dir; the small exec
// trampoline below closes that pathname race by calling fchdir(2) after fork
// and before exec(2).
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

// Start creates a command which changes to fd's directory in the child and
// then execs argv[0] directly.  helper must be the wx executable (or another
// executable importing this package); it is resolved before the child starts.
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
	// The helper starts in a harmless, stable directory and switches to fd
	// before executing the requested process.  No user-controlled path is used
	// for this pre-exec chdir.
	cmd.Dir = string(filepath.Separator)
	cmd.Env = env
	cmd.ExtraFiles = []*os.File{fd}
	return cmd, nil
}

// Handle executes the hidden trampoline command.  It returns handled=false
// for normal wx invocations.  The init hook is intentional: Go test binaries
// have a generated main and therefore cannot call cmd/wx's main dispatcher,
// but they still need to exercise descriptor-bound commands.
func Handle(args []string) (handled bool, exitCode int) {
	if len(args) < 2 || args[0] != Command {
		return false, 0
	}
	if err := unix.Fchdir(FD); err != nil {
		fmt.Fprintf(os.Stderr, "wx descriptor exec: fchdir: %v\n", err)
		return true, 1
	}
	// The directory FD is only needed to establish the child's CWD. Do not
	// expose it to the requested process or retain it for the lifetime of a
	// long-running agent.
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
