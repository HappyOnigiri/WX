package fdexec

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// TestStartResolvesRelativeCommandInsideDescriptorDirectory は、呼び出し元の CWD に無く
// descriptor のディレクトリにだけある相対パスのコマンドを起動できることを確かめる。
// wx run -- ./scripts/test.sh のように、貸出先の worktree にだけあるコマンドの経路である。
func TestStartResolvesRelativeCommandInsideDescriptorDirectory(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	script := filepath.Join(root, "only-here.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	directory, err := os.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = directory.Close() }()

	if _, err := os.Stat("./only-here.sh"); err == nil {
		t.Fatal("the command must not be reachable from the caller's CWD")
	}
	cmd, err := Start(context.Background(), os.Args[0], directory, os.Environ(), "./only-here.sh")
	if err != nil {
		t.Fatalf("relative command inside the descriptor directory: %v", err)
	}
	if err := cmd.Run(); err != nil {
		t.Fatalf("run relative command inside the descriptor directory: %v", err)
	}
}

// TestStartReportsMissingCommandFromTheChild は、解決を子へ移した後も
// 存在しないコマンドが失敗として伝わることを確かめる。
func TestStartReportsMissingCommandFromTheChild(t *testing.T) {
	t.Parallel()
	directory, err := os.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = directory.Close() }()

	cmd, err := Start(context.Background(), os.Args[0], directory, os.Environ(), "./wx-command-that-does-not-exist")
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = nil
	if err := cmd.Run(); err == nil {
		t.Fatal("a missing command was reported as a success")
	}
}

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
