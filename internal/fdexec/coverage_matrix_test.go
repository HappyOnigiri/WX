package fdexec

import (
	"context"
	"os"
	"testing"
)

func TestStartRejectsInvalidDescriptorAndCommandInputs(t *testing.T) {
	directory, err := os.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = directory.Close() }()

	for _, test := range []struct {
		name   string
		fd     *os.File
		helper string
		argv   []string
	}{
		{name: "nil descriptor", helper: "sh", argv: []string{"sh"}},
		{name: "missing executable", fd: directory, helper: "sh", argv: []string{"wx-command-that-does-not-exist"}},
		{name: "missing helper", fd: directory, helper: "wx-helper-that-does-not-exist", argv: []string{"sh"}},
		{name: "empty argv", fd: directory, helper: "sh"},
		{name: "empty executable", fd: directory, helper: "sh", argv: []string{""}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := Start(context.Background(), test.helper, test.fd, os.Environ(), test.argv...); err == nil {
				t.Fatal("invalid descriptor command was accepted")
			}
		})
	}

	cmd, err := Start(context.Background(), os.Args[0], directory, os.Environ(), "sh", "-c", "exit 0")
	if err != nil {
		t.Fatalf("relative helper: %v", err)
	}
	if err := cmd.Run(); err != nil {
		t.Fatalf("relative helper command: %v", err)
	}
}

func TestHandleRejectsNormalAndIncompleteInvocations(t *testing.T) {
	for _, args := range [][]string{nil, {}, {"wx"}, {Command}} {
		if handled, code := Handle(args); handled || code != 0 {
			t.Fatalf("Handle(%v)=%v,%d", args, handled, code)
		}
	}
}
