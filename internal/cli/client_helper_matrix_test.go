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
	// restoreForeground brackets its TIOCSPGRP ioctl with a SIGTTOU guard so
	// that handing the terminal back cannot stop wx itself: a background
	// process group attempting TIOCSPGRP is delivered SIGTTOU, whose default
	// action stops the process. With an invalid descriptor the ioctl itself
	// fails and must be swallowed, so the guard's own lifecycle is the only
	// externally observable effect available here.
	//
	// The guard is built on signal.Notify/signal.Stop rather than
	// signal.Ignore/signal.Reset specifically so this is a true bracket:
	// signal.Reset only undoes Notify registrations, it does not clear the
	// disposition signal.Ignore sets, so an Ignore/Reset pair leaves SIGTTOU
	// permanently reported as ignored for the rest of the process's life
	// after the very first call. That is a process-wide leak, and this test
	// runs restoreForeground repeatedly in one process (mirroring `go test
	// -count>1`, which reruns the whole binary without a fresh process) to
	// prove no such state survives a call. No other code in this package
	// touches SIGTTOU, so the order -shuffle picks does not matter.
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
