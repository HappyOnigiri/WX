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
	// An unstarted command has no Process; forwarding must return rather than
	// dereference it.
	forwardAgentSignal(&exec.Cmd{}, syscall.SIGTERM)

	// configureAgentProcess puts the child in its own process group, and the
	// shell keeps a grandchild (`sleep`) alive in it, so the child only dies
	// if the signal reached the group. Asserting the exact termination signal
	// is what proves delivery: without it a forwardAgentSignal regression
	// would leave the child running and surface as a package-wide timeout
	// rather than as this test failing.
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
	// restoreForeground brackets its TIOCSPGRP ioctl with signal.Ignore of
	// SIGTTOU so that handing the terminal back cannot stop wx itself. With an
	// invalid descriptor the ioctl fails and must be swallowed, and installing
	// that guard is the only externally observable effect the call has without
	// a controlling terminal, so assert it rather than only that the call did
	// not panic. Go's signal.Reset undoes Notify registrations, not
	// signal.Ignore, so the ignore disposition is what remains afterwards;
	// checking the precondition keeps the assertion about this call. No other
	// code in this package touches SIGTTOU, so the order -shuffle picks does
	// not matter.
	if signal.Ignored(syscall.SIGTTOU) {
		t.Fatal("SIGTTOU was already ignored before restoreForeground ran")
	}
	restoreForeground(-1)
	if !signal.Ignored(syscall.SIGTTOU) {
		t.Fatal("restoreForeground returned without installing its SIGTTOU guard")
	}
}
