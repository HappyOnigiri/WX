package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"

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
	forwardAgentSignal(&exec.Cmd{}, syscall.SIGTERM)
	cmd := exec.Command("/bin/sh", "-c", "trap 'exit 0' TERM; while :; do sleep 1; done")
	configureAgentProcess(cmd, -1)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	forwardAgentSignal(cmd, syscall.SIGTERM)
	_ = cmd.Wait()
}

func TestRestoreForegroundIgnoresInvalidTerminalDescriptor(t *testing.T) {
	restoreForeground(-1)
}
