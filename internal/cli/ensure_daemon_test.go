package cli

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/HappyOnigiri/WX/internal/config"
	"github.com/HappyOnigiri/WX/internal/launchd"
	"github.com/HappyOnigiri/WX/internal/rpc"
)

// fakeLaunchctl installs a launchctl on PATH that records each invocation to
// marker and exits 0 without doing anything real. It lets tests observe
// whether ensureDaemon reached for launchd.Kickstart without actually
// depending on a real LaunchAgent.
func fakeLaunchctl(t *testing.T) (marker string) {
	t.Helper()
	bin := t.TempDir()
	marker = filepath.Join(bin, "kickstart-invoked")
	script := "#!/bin/sh\necho \"$@\" >> \"" + marker + "\"\nexit 0\n"
	if err := os.WriteFile(filepath.Join(bin, "launchctl"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	return marker
}

func TestEnsureDaemonKickstartsWhenNothingIsListening(t *testing.T) {
	marker := fakeLaunchctl(t)
	socket := filepath.Join(t.TempDir(), "wxd.sock")
	client := Client{RPC: rpc.Client{Socket: socket, Timeout: 200 * time.Millisecond}, Config: config.Defaults()}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := client.ensureDaemon(ctx)
	if err == nil {
		t.Fatal("ensureDaemon reported success against a socket nothing is listening on")
	}
	if !strings.Contains(err.Error(), "did not become ready") {
		t.Fatalf("unexpected error after failed kickstart: %v", err)
	}
	if _, statErr := os.Stat(marker); statErr != nil {
		t.Fatalf("launchctl kickstart was not invoked for a missing socket: %v", statErr)
	}
}

func TestEnsureDaemonGuidesInstallForStaleLaunchAgent(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	bin := filepath.Join(home, "bin")
	if err := os.Mkdir(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "wx"), []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "launchctl"), []byte("#!/bin/sh\necho bootstrap-failed >&2\nexit 9\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	plist, err := launchd.PlistPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(plist), 0o700); err != nil {
		t.Fatal(err)
	}
	// The old command contract that motivated this diagnostic wrote a plist
	// containing daemon start --foreground instead of daemon serve.
	if err := os.WriteFile(plist, []byte("<string>daemon start --foreground</string>\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	client := Client{RPC: rpc.Client{Socket: filepath.Join(home, "wxd.sock"), Timeout: 200 * time.Millisecond}, Config: config.Defaults()}
	err = client.ensureDaemon(context.Background())
	if err == nil || !strings.Contains(err.Error(), "run wx daemon install") {
		t.Fatalf("stale launch agent guidance error=%v", err)
	}
	if strings.Contains(err.Error(), "run wx doctor") {
		t.Fatalf("stale launch agent still suggested doctor: %v", err)
	}
}

type erroringStatusHandler struct{}

func (erroringStatusHandler) Handle(_ context.Context, method string, _ json.RawMessage) (any, error) {
	if method == "Status" {
		return nil, errors.New("status handler intentionally failed")
	}
	return map[string]bool{"ok": true}, nil
}

func TestEnsureDaemonDoesNotKickstartALiveDaemonThatFailedToAnswer(t *testing.T) {
	marker := fakeLaunchctl(t)
	socket := filepath.Join(t.TempDir(), "wxd.sock")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	server := &rpc.Server{Socket: socket, Handler: erroringStatusHandler{}}
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx) }()
	waitForPath(t, socket)
	client := Client{RPC: rpc.Client{Socket: socket, Timeout: time.Second}, Config: config.Defaults()}
	err := client.ensureDaemon(context.Background())
	if err == nil {
		t.Fatal("ensureDaemon reported success despite the Status RPC failing")
	}
	if !strings.Contains(err.Error(), "reachable but this request did not complete") {
		t.Fatalf("unexpected error for a live-but-failing daemon: %v", err)
	}
	if _, statErr := os.Stat(marker); !os.IsNotExist(statErr) {
		t.Fatal("launchctl kickstart -k was invoked against a daemon that answered the connection")
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
