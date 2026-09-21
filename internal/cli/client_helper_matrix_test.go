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
	cfg.System.Storage.WorktreeRoot = root
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
	invalid.System.Storage.WorktreeRoot = "$UNSUPPORTED"
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

// root reload 後も daemon は旧 root の slot を寿命まで貸し出す。応答が示した root 世代を
// 起点にしないと、その貸出は現行設定の ownership root の外として起動前に拒否される。
// identity の照合は root 世代を跨いでも効き続ける。
func TestOpenLeaseDirectoryUsesTheLeaseRootGeneration(t *testing.T) {
	rawRoot := t.TempDir()
	base, err := domain.Canonicalize(rawRoot)
	if err != nil {
		t.Fatal(err)
	}
	oldRoot := filepath.Join(string(base), "root-old")
	newRoot := filepath.Join(string(base), "root-new")
	retained := filepath.Join(oldRoot, "workspace")
	if err := os.MkdirAll(retained, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(newRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	opened, identity, err := domain.OpenOwnedDirectory(oldRoot, retained)
	if err != nil {
		t.Fatal(err)
	}
	if err := opened.Close(); err != nil {
		t.Fatal(err)
	}
	cfg := config.Defaults()
	cfg.System.Storage.WorktreeRoot = newRoot
	lease := daemon.Lease{Path: retained, RootPath: oldRoot, RootIdentity: identity}
	retainedOpen, err := openLeaseDirectory(cfg, lease)
	if err != nil {
		t.Fatalf("open a lease retained under the retired root: %v", err)
	}
	if err := retainedOpen.Close(); err != nil {
		t.Fatal(err)
	}
	lease.RootIdentity = "vol:0:forged"
	if forged, err := openLeaseDirectory(cfg, lease); err == nil {
		_ = forged.Close()
		t.Fatal("the lease root generation skipped the identity check")
	}
	outside := daemon.Lease{Path: filepath.Join(string(base), "outside"), RootPath: oldRoot, RootIdentity: identity}
	if escaped, err := openLeaseDirectory(cfg, outside); err == nil {
		_ = escaped.Close()
		t.Fatal("a path outside the lease root generation was accepted")
	}
}
