package cli

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/HappyOnigiri/WX/internal/config"
	"github.com/HappyOnigiri/WX/internal/launchd"
	"github.com/HappyOnigiri/WX/internal/rpc"
	"github.com/HappyOnigiri/WX/internal/testsupport"
)

// fakeLaunchctl は PATH に偽の launchctl を置き、呼び出しを marker へ記録して成功終了する。
// 実際の LaunchAgent に依存せず、ensureDaemon が launchd.Kickstart を呼んだか確認できる。
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
	socket := testsupport.SocketPath(t, "wxd.sock")
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
	// 旧 command 契約は daemon serve ではなく daemon start --foreground を含む plist を書いていた。
	if err := os.WriteFile(plist, []byte("<string>daemon start --foreground</string>\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	client := Client{RPC: rpc.Client{Socket: testsupport.SocketPath(t, "wxd.sock"), Timeout: 200 * time.Millisecond}, Config: config.Defaults()}
	err = client.ensureDaemon(context.Background())
	if err == nil || !strings.Contains(err.Error(), "run wx daemon install") {
		t.Fatalf("stale launch agent guidance error=%v", err)
	}
	if strings.Contains(err.Error(), "run wx doctor") {
		t.Fatalf("stale launch agent still suggested doctor: %v", err)
	}
}

type erroringPingHandler struct{}

func (erroringPingHandler) Handle(_ context.Context, method string, _ json.RawMessage) (any, error) {
	if method == "Ping" {
		return nil, errors.New("ping handler intentionally failed")
	}
	return map[string]bool{"ok": true}, nil
}

// unknownMethodHandler は Ping を知らない旧 daemon を模し、他の method には応答する。
type unknownMethodHandler struct{}

func (unknownMethodHandler) Handle(_ context.Context, method string, _ json.RawMessage) (any, error) {
	if method == "Ping" {
		return nil, errors.New("unknown RPC method")
	}
	return map[string]bool{"ok": true}, nil
}

// countingPingHandler は接続確認の回数を数え、起動 1 回あたりの Ping が 1 度だけであることを確かめる。
type countingPingHandler struct {
	mu    sync.Mutex
	pings int
}

func (h *countingPingHandler) Handle(_ context.Context, method string, _ json.RawMessage) (any, error) {
	if method == "Ping" {
		h.mu.Lock()
		h.pings++
		h.mu.Unlock()
		return map[string]any{"protocol_version": rpc.ProtocolVersion, "degraded": false}, nil
	}
	return nil, errors.New("unexpected method " + method)
}

func (h *countingPingHandler) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.pings
}

func TestEnsureDaemonKeepsAFailingLiveDaemonUnkicked(t *testing.T) {
	marker := fakeLaunchctl(t)
	socket := testsupport.SocketPath(t, "wxd.sock")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	server := &rpc.Server{Socket: socket, Handler: erroringPingHandler{}}
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx) }()
	waitForSocket(t, socket, done)
	client := Client{RPC: rpc.Client{Socket: socket, Timeout: time.Second}, Config: config.Defaults()}
	err := client.ensureDaemon(context.Background())
	if err == nil {
		t.Fatal("ensureDaemon reported success despite the Ping RPC failing")
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

func TestEnsureDaemonKeepsAnOutdatedDaemonUnkicked(t *testing.T) {
	marker := fakeLaunchctl(t)
	socket := testsupport.SocketPath(t, "wxd.sock")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	server := &rpc.Server{Socket: socket, Handler: unknownMethodHandler{}}
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx) }()
	waitForSocket(t, socket, done)
	client := Client{RPC: rpc.Client{Socket: socket, Timeout: time.Second}, Config: config.Defaults()}
	err := client.ensureDaemon(context.Background())
	if err == nil {
		t.Fatal("ensureDaemon reported success against a daemon that does not know Ping")
	}
	if !strings.Contains(err.Error(), "reachable but this request did not complete") {
		t.Fatalf("unexpected error for a daemon without Ping: %v", err)
	}
	if !strings.Contains(err.Error(), "wx daemon restart") {
		t.Fatalf("outdated daemon guidance did not mention a restart: %v", err)
	}
	if _, statErr := os.Stat(marker); !os.IsNotExist(statErr) {
		t.Fatal("launchctl kickstart -k was invoked against a daemon that answered the connection")
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestEnsureDaemonChecksTheConnectionOncePerLaunch(t *testing.T) {
	socket := testsupport.SocketPath(t, "wxd.sock")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	handler := &countingPingHandler{}
	server := &rpc.Server{Socket: socket, Handler: handler}
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx) }()
	waitForSocket(t, socket, done)
	client := Client{RPC: rpc.Client{Socket: socket, Timeout: time.Second}, Config: config.Defaults(), daemonGate: &daemonGate{}}
	// policy 起動・RunAgent・scope 解決が重なる起動を模し、Client を値で複製しても確認が増えないことを確かめる。
	for _, probe := range []Client{client, client, client} {
		if err := probe.ensureDaemon(context.Background()); err != nil {
			t.Fatalf("ensureDaemon against a live daemon: %v", err)
		}
	}
	if got := handler.count(); got != 1 {
		t.Fatalf("connection checks per launch=%d, want 1", got)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestEnsureDaemonReusesTheFirstFailureWithinALaunch(t *testing.T) {
	marker := fakeLaunchctl(t)
	socket := testsupport.SocketPath(t, "wxd.sock")
	client := Client{RPC: rpc.Client{Socket: socket, Timeout: 200 * time.Millisecond}, Config: config.Defaults(), daemonGate: &daemonGate{}}
	first := client.ensureDaemon(context.Background())
	if first == nil {
		t.Fatal("ensureDaemon reported success against a socket nothing is listening on")
	}
	kickstarts := launchctlInvocations(t, marker)
	if second := client.ensureDaemon(context.Background()); second == nil || second.Error() != first.Error() {
		t.Fatalf("second ensureDaemon error=%v, want the first error %v", second, first)
	}
	if got := launchctlInvocations(t, marker); got != kickstarts {
		t.Fatalf("launchctl invocations after the second check=%d, want %d", got, kickstarts)
	}
}

func launchctlInvocations(t *testing.T, marker string) int {
	t.Helper()
	data, err := os.ReadFile(marker)
	if err != nil {
		t.Fatalf("read launchctl marker: %v", err)
	}
	return len(strings.Split(strings.TrimSuffix(string(data), "\n"), "\n"))
}

// launchProbeHandler は起動 1 回で届いた method を順に記録し、lease 取得だけを失敗させる。
// agent を起動しないまま、scope 解決と起動要求をまたぐ RPC の並びを観測できる。
type launchProbeHandler struct {
	mu      sync.Mutex
	methods []string
}

func (h *launchProbeHandler) Handle(_ context.Context, method string, _ json.RawMessage) (any, error) {
	h.mu.Lock()
	h.methods = append(h.methods, method)
	h.mu.Unlock()
	switch method {
	case "Ping":
		return map[string]any{"protocol_version": rpc.ProtocolVersion, "degraded": false}, nil
	case "ResolveAndLease":
		return nil, errors.New("lease intentionally refused")
	default:
		return map[string]bool{"ok": true}, nil
	}
}

func (h *launchProbeHandler) recorded() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.methods...)
}

func TestLaunchChecksTheConnectionOnceAcrossScopeAndLease(t *testing.T) {
	socket := testsupport.SocketPath(t, "wxd.sock")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	handler := &launchProbeHandler{}
	server := &rpc.Server{Socket: socket, Handler: handler}
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx) }()
	waitForSocket(t, socket, done)
	client := Client{RPC: rpc.Client{Socket: socket, Timeout: time.Second}, Config: config.Defaults(), daemonGate: &daemonGate{}}
	if _, err := client.ResolveSessionScope(context.Background(), t.TempDir()); err != nil {
		t.Fatalf("resolve session scope: %v", err)
	}
	if exit := client.runAgent(context.Background(), "claude", nil, nil, false, ""); exit != 1 {
		t.Fatalf("runAgent against a refused lease exit=%d", exit)
	}
	recorded := handler.recorded()
	pings := 0
	for _, method := range recorded {
		if method == "Ping" {
			pings++
		}
	}
	if pings != 1 {
		t.Fatalf("connection checks across scope resolution and launch=%d in %v, want 1", pings, recorded)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
