package cli

import (
	"errors"
	"os/exec"
	"syscall"
	"testing"
)

func TestChildExitCodePreservesExitStatusAndNormalizesSignal(t *testing.T) {
	if got, ok := childExitCode(nil); !ok || got != 0 {
		t.Fatalf("childExitCode(nil)=(%d, %t), want (0, true)", got, ok)
	}

	tests := []struct {
		name string
		cmd  *exec.Cmd
		want int
	}{
		{name: "ordinary exit", cmd: exec.Command("/bin/sh", "-c", "exit 3"), want: 3},
		{name: "sigterm", cmd: exec.Command("/bin/sh", "-c", "kill -TERM $$"), want: 128 + int(syscall.SIGTERM)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := test.cmd.Run()
			if err == nil {
				t.Fatal("command unexpectedly succeeded")
			}
			got, ok := childExitCode(err)
			if !ok || got != test.want {
				t.Fatalf("childExitCode=(%d, %t), want (%d, true); err=%v", got, ok, test.want, err)
			}
		})
	}
	if got, ok := childExitCode(errors.New("not an exit error")); ok || got != 1 {
		t.Fatalf("childExitCode(non-exit)=(%d, %t), want (1, false)", got, ok)
	}
}
