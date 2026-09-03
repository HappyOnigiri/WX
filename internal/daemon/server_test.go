package daemon

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/HappyOnigiri/WX/internal/config"
	"github.com/HappyOnigiri/WX/internal/rpc"
)

func TestDaemonRuntimeLockAllowsOnlyOneOwner(t *testing.T) {
	path := t.TempDir() + "/daemon.lock"
	first, err := acquireDaemonLock(path)
	if err != nil {
		t.Fatal(err)
	}
	defer releaseDaemonLock(first)
	second, err := acquireDaemonLock(path)
	if err == nil {
		releaseDaemonLock(second)
		t.Fatal("second daemon acquired the runtime lock")
	}
	if !strings.Contains(err.Error(), "already running") {
		t.Fatalf("second lock error = %v", err)
	}
}

func TestServerPathLevelAndWorktreeRootHelpers(t *testing.T) {
	for value, want := range map[string]slog.Level{
		"debug": slog.LevelDebug,
		"info":  slog.LevelInfo,
		"warn":  slog.LevelWarn,
		"error": slog.LevelError,
	} {
		if got := slogLevel(value); got != want {
			t.Errorf("slogLevel(%q)=%s, want %s", value, got, want)
		}
	}
	releaseDaemonLock(nil)

	root := filepath.Join(t.TempDir(), "worktrees")
	created, createdRoot, err := ensureWorktreeRootDescriptor(root)
	if err != nil || created != root {
		t.Fatalf("create worktree root=%q err=%v", created, err)
	}
	if err := createdRoot.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(root, 0o755); err != nil {
		t.Fatal(err)
	}
	existing, existingRoot, err := ensureWorktreeRootDescriptor(root)
	if err != nil || existing != root {
		t.Fatalf("reuse worktree root=%q err=%v", existing, err)
	}
	if err := existingRoot.Close(); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(root); err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("worktree root permissions=%v err=%v", info, err)
	}
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ensureWorktreeRootDescriptor(file); err == nil {
		t.Fatal("regular file was accepted as worktree root")
	}
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(root, link); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ensureWorktreeRootDescriptor(link); err == nil {
		t.Fatal("symlink was accepted as worktree root")
	}
	if _, _, err := ensureWorktreeRootDescriptor("$UNSUPPORTED/worktrees"); err == nil {
		t.Fatal("unsupported path expansion succeeded")
	}
}

func TestServeStartsRPCAndStopsWithContext(t *testing.T) {
	home, err := os.MkdirTemp("/tmp", "wx-serve-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(home) })
	t.Setenv("HOME", home)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Serve(ctx) }()

	socket, err := config.SocketPath()
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		select {
		case serveErr := <-done:
			t.Fatalf("daemon exited before creating socket: %v", serveErr)
		default:
		}
		if info, statErr := os.Stat(socket); statErr == nil && info.Mode()&os.ModeSocket != 0 {
			break
		}
		if time.Now().After(deadline) {
			cancel()
			t.Fatalf("daemon socket was not created at %s", socket)
		}
		time.Sleep(10 * time.Millisecond)
	}
	client := rpc.Client{Socket: socket, Timeout: time.Second}
	var status map[string]any
	if err := client.Call(context.Background(), "Status", nil, &status); err != nil {
		cancel()
		t.Fatal(err)
	}
	if status["protocol_version"] != float64(rpc.ProtocolVersion) {
		t.Fatalf("status=%v", status)
	}
	if info, err := os.Stat(filepath.Dir(socket)); err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("runtime directory mode=%v err=%v", info, err)
	}
	if info, err := os.Stat(socket); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("socket mode=%v err=%v", info, err)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("daemon did not stop after context cancellation")
	}
}
