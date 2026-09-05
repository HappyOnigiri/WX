package cli

import (
	"errors"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/HappyOnigiri/WX/internal/config"
	"github.com/HappyOnigiri/WX/internal/daemon"
	"github.com/HappyOnigiri/WX/internal/domain"
)

func TestOpenLeaseDirectoryPinsAndValidatesRootIdentity(t *testing.T) {
	rawRoot := t.TempDir()
	rootPath, err := domain.Canonicalize(rawRoot)
	if err != nil {
		t.Fatal(err)
	}
	root := string(rootPath)
	workspace := filepath.Join(root, "workspace")
	if err := os.Mkdir(workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := config.Defaults()
	cfg.Storage.WorktreeRoot = root
	_, identity, err := domain.OpenOwnedDirectory(root, workspace)
	if err != nil {
		t.Fatal(err)
	}

	opened, err := openLeaseDirectory(cfg, daemon.Lease{Path: workspace, RootIdentity: identity})
	if err != nil {
		t.Fatalf("open valid lease: %v", err)
	}
	if err := opened.Close(); err != nil {
		t.Fatal(err)
	}
	if opened, err := openLeaseDirectory(cfg, daemon.Lease{Path: workspace, RootIdentity: "wrong-identity"}); err == nil {
		_ = opened.Close()
		t.Fatal("lease with wrong root identity was accepted")
	}
	if opened, err := openLeaseDirectory(cfg, daemon.Lease{Path: workspace}); err != nil {
		t.Fatalf("identity-less in-root lease: %v", err)
	} else if err := opened.Close(); err != nil {
		t.Fatal(err)
	}

	rawOutside := t.TempDir()
	outsideRoot, err := domain.Canonicalize(rawOutside)
	if err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(string(outsideRoot), "outside")
	if err := os.Mkdir(outside, 0o700); err != nil {
		t.Fatal(err)
	}
	if opened, err := openLeaseDirectory(cfg, daemon.Lease{Path: outside}); err != nil {
		t.Fatalf("identity-less compatibility lease: %v", err)
	} else if err := opened.Close(); err != nil {
		t.Fatal(err)
	}
	if opened, err := openLeaseDirectory(cfg, daemon.Lease{Path: outside, RootIdentity: identity}); err == nil {
		_ = opened.Close()
		t.Fatal("identity-bearing outside lease was accepted")
	}

	link := filepath.Join(root, "link")
	if err := os.Symlink(workspace, link); err != nil {
		t.Fatal(err)
	}
	if opened, err := openLeaseDirectory(cfg, daemon.Lease{Path: link}); err == nil {
		_ = opened.Close()
		t.Fatal("symlink lease was accepted")
	}
	invalid := cfg
	invalid.Storage.WorktreeRoot = "$UNSUPPORTED"
	if opened, err := openLeaseDirectory(invalid, daemon.Lease{Path: workspace}); err == nil {
		_ = opened.Close()
		t.Fatal("unsupported root expansion was accepted")
	}
}

func TestForwardAgentSignalHandlesMissingProcessAndProcessGroup(t *testing.T) {
	// 未起動 command には Process がないため、signal 転送は dereference せず return する。
	forwardAgentSignal(&exec.Cmd{}, syscall.SIGTERM)

	// configureAgentProcess は子を専用 process group に置き、shell は grandchild（sleep）を残す。
	// 終了 signal の一致で group への配送を確認し、回帰を package 全体の timeout にしない。
	cmd := exec.Command("/bin/sh", "-c", "sleep 300 & wait")
	configureAgentProcess(cmd, -1)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	forwardAgentSignal(cmd, syscall.SIGTERM)
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	select {
	case err := <-exited:
		var exit *exec.ExitError
		if !errors.As(err, &exit) {
			t.Fatalf("agent process exited without a signal status: %v", err)
		}
		status, ok := exit.Sys().(syscall.WaitStatus)
		if !ok || !status.Signaled() || status.Signal() != syscall.SIGTERM {
			t.Fatalf("agent process was not terminated by SIGTERM: %v", err)
		}
	case <-time.After(30 * time.Second):
		_ = cmd.Process.Kill()
		<-exited
		t.Fatal("forwardAgentSignal did not deliver SIGTERM to the agent process group")
	}
}

func TestRestoreForegroundIgnoresInvalidTerminalDescriptor(t *testing.T) {
	// restoreForeground は TIOCSPGRP 中の SIGTTOU で wx 自身が停止しないようにする。
	// signal.Notify/Stop は signal.Ignore と異なり process-wide な無視状態を残さないため、繰り返し呼び出して確認する。
	for i := 0; i < 3; i++ {
		if signal.Ignored(syscall.SIGTTOU) {
			t.Fatalf("iteration %d: SIGTTOU was already ignored before restoreForeground ran", i)
		}
		restoreForeground(-1)
		if signal.Ignored(syscall.SIGTTOU) {
			t.Fatalf("iteration %d: restoreForeground leaked a permanent SIGTTOU-ignored disposition", i)
		}
	}
}
