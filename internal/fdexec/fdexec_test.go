package fdexec

import (
	"context"
	"os"
	"testing"
)

func TestStartClosesDirectoryFDAfterFchdir(t *testing.T) {
	directory, err := os.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = directory.Close() }()

	cmd, err := Start(context.Background(), "", directory, os.Environ(), "sh", "-c", "test ! -e /dev/fd/3")
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Run(); err != nil {
		t.Fatalf("descriptor-bound child inherited FD 3: %v", err)
	}
}
