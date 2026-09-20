package cli

import (
	"errors"
	"os/exec"
	"syscall"
)

// childExitCode は agent の終了状態を CLI の終了コードへ変換する。
// os/exec は signal 終了を -1 と返すため、shell と同じ 128+signal に直す。
func childExitCode(err error) (int, bool) {
	if err == nil {
		return 0, true
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		return 1, false
	}
	if code := exitErr.ExitCode(); code >= 0 {
		return code, true
	}
	if exitErr.ProcessState != nil {
		if status, ok := exitErr.ProcessState.Sys().(syscall.WaitStatus); ok && status.Signaled() {
			return 128 + int(status.Signal()), true
		}
	}
	return 1, true
}
