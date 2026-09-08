package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/HappyOnigiri/WX/internal/config"
)

// assertLeaseCommandExits は貸出コマンドを 1 回実行し、終了コードを確かめる。
// 引数検査は daemon へ接続する前に済むため、中断済み context でも誤用は 2 で終える。
func assertLeaseCommandExits(t *testing.T, name string, run func(context.Context, []string) int, args []string, want int) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got := run(ctx, args); got != want {
		t.Fatalf("wx %s %v exit=%d, want %d", name, args, got, want)
	}
}

// assertLeaseCommandHelp は --help が stdout へ専用の help を出して 0 で終えることを確かめる。
func assertLeaseCommandHelp(t *testing.T, name string, run func(context.Context, []string) int) {
	t.Helper()
	output := captureStdout(t, func() {
		if got := run(context.Background(), []string{"--help"}); got != 0 {
			t.Fatalf("wx %s --help exit=%d", name, got)
		}
	})
	if !strings.HasPrefix(output, "Usage: wx "+name) {
		t.Fatalf("wx %s --help output=%q", name, output)
	}
}

func TestShellCommandValidatesArgumentsAndHelp(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	assertLeaseCommandHelp(t, "shell", runShell)
	assertLeaseCommandExits(t, "shell", runShell, []string{"extra"}, 2)
	assertLeaseCommandExits(t, "shell", runShell, []string{"--worktree"}, 2)
	// --branch と --resume は異なる基点を選ぶため、同時指定を受け付けない。
	assertLeaseCommandExits(t, "shell", runShell, []string{"--branch", "main", "--resume", "session"}, 2)
}

// leaseClient は設定の読み込みに失敗したら失敗（1）を返し、client を渡さない。
func TestLeaseClientReportsConfigurationFailures(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	path, err := config.Path()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("unknown: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, code := leaseClient(); code != 1 {
		t.Fatalf("leaseClient code=%d, want 1", code)
	}
}
