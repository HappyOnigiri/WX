package daemon

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/HappyOnigiri/WX/internal/config"
	"github.com/HappyOnigiri/WX/internal/rpc"
	"github.com/HappyOnigiri/WX/internal/state"
)

func TestDaemonCrashProcessHelper(t *testing.T) {
	if os.Getenv("WX_DAEMON_CRASH_HELPER") != "1" {
		return
	}
	if err := Serve(context.Background()); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(0)
}

func TestDaemonProcessCrashMatrix(t *testing.T) {
	requireDaemonIntegration(t)
	for _, boundary := range []string{"preparing", "leased", "snapshot", "snapshot-ref", "remove", "restore"} {
		t.Run(boundary, func(t *testing.T) {
			testDaemonProcessCrashBoundary(t, boundary)
		})
	}
}

type crashDaemonProcess struct {
	command *exec.Cmd
	stderr  bytes.Buffer
}

func testDaemonProcessCrashBoundary(t *testing.T, boundary string) {
	t.Helper()
	home, err := os.MkdirTemp("/tmp", "wx-crash-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(home) })
	t.Setenv("HOME", home)
	repository := filepath.Join(home, "repo")
	initGitRepo(t, repository)
	mainHead := gitOutput(t, repository, "rev-parse", "HEAD")
	mainStatus := gitOutput(t, repository, "status", "--porcelain=v2")
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	gate := filepath.Join(home, "gate")
	if err := os.Mkdir(gate, 0o700); err != nil {
		t.Fatal(err)
	}
	installCrashGitWrapper(t, home, realGit, gate, boundary)
	writeCrashConfig(t, home)

	socket, err := config.SocketPath()
	if err != nil {
		t.Fatal(err)
	}
	client := rpc.Client{Socket: socket, Timeout: 10 * time.Second}
	process := startCrashDaemon(t, client)
	t.Cleanup(func() { stopCrashDaemon(process) })
	storePath, err := config.StatePath()
	if err != nil {
		t.Fatal(err)
	}
	store, err := state.Open(storePath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	if boundary == "preparing" {
		armCrashGate(t, gate)
	}
	lease := resolveCrashLease(t, client, repository)
	if boundary == "preparing" {
		waitCrashGate(t, gate, "entered")
		process = restartAfterCrash(t, process, client, gate)
		waitCrashReady(t, client, lease)
	} else {
		waitCrashReady(t, client, lease)
	}

	expectedRegistration := lease.Path
	switch boundary {
	case "preparing":
		assertCrashSessionState(t, store, lease.SessionID, "ACTIVE")
	case "leased":
		process = restartAfterCrash(t, process, client, "")
		assertCrashSessionState(t, store, lease.SessionID, "ACTIVE")
	case "snapshot", "snapshot-ref":
		writeCrashWorkspace(t, lease.Path)
		armCrashGate(t, gate)
		releaseCrashLease(t, client, lease)
		waitCrashGate(t, gate, "entered")
		process = restartAfterCrash(t, process, client, gate)
		waitCrashSessionState(t, store, lease.SessionID, "ARCHIVED")
		assertCrashSnapshot(t, store, lease.SessionID)
	case "remove":
		releaseCrashLease(t, client, lease)
		waitCrashSessionState(t, store, lease.SessionID, "ARCHIVED")
		armCrashGate(t, gate)
		var result map[string]int
		if err := client.Call(context.Background(), "GC", map[string]bool{"dry_run": false}, &result); err != nil {
			t.Fatal(err)
		}
		waitCrashGate(t, gate, "entered")
		process = restartAfterCrash(t, process, client, gate)
		waitUntil(t, 10*time.Second, func() bool {
			slot, slotErr := store.Slot(context.Background(), lease.SessionID)
			_, pathErr := os.Lstat(lease.Path)
			return slotErr == nil && slot.State == "ARCHIVED" && os.IsNotExist(pathErr)
		})
	case "restore":
		writeCrashWorkspace(t, lease.Path)
		releaseCrashLease(t, client, lease)
		waitCrashSessionState(t, store, lease.SessionID, "ARCHIVED")
		armCrashGate(t, gate)
		var resumed Lease
		if err := client.Call(context.Background(), "Resume", map[string]any{"wx_session_id": lease.SessionID, "agent": "codex", "client_pid": os.Getpid(), "allow_fresh": false}, &resumed); err != nil {
			t.Fatal(err)
		}
		expectedRegistration = resumed.Path
		waitCrashGate(t, gate, "entered")
		process = restartAfterCrash(t, process, client, gate)
		waitCrashReady(t, client, resumed)
		data, err := os.ReadFile(filepath.Join(resumed.Path, "crash-content"))
		if err != nil || string(data) != "preserved\n" {
			t.Fatalf("restored content=%q err=%v", data, err)
		}
	}

	if boundary == "preparing" || boundary == "leased" {
		releaseCrashLease(t, client, lease)
		waitCrashSessionState(t, store, lease.SessionID, "ARCHIVED")
	}
	var status state.Status
	waitUntil(t, 10*time.Second, func() bool {
		var statusErr error
		status, statusErr = store.Status(context.Background())
		return statusErr == nil && status.Jobs == 0
	})
	if status.Quarantined != 0 {
		t.Fatalf("quarantined state after %s crash: %+v", boundary, status)
	}
	if got := gitOutput(t, repository, "rev-parse", "HEAD"); got != mainHead {
		t.Fatalf("main HEAD changed after %s crash: got=%s want=%s", boundary, got, mainHead)
	}
	if got := gitOutput(t, repository, "status", "--porcelain=v2"); got != mainStatus {
		t.Fatalf("main worktree changed after %s crash: got=%q want=%q", boundary, got, mainStatus)
	}
	registrations := gitOutput(t, repository, "worktree", "list", "--porcelain")
	if boundary == "remove" {
		if strings.Contains(registrations, lease.Path) {
			t.Fatalf("removed worktree remains registered after crash: %s", lease.Path)
		}
	} else if boundary != "snapshot" && boundary != "snapshot-ref" && !strings.Contains(registrations, expectedRegistration) {
		t.Fatalf("owned worktree registration was lost after %s crash: %s", boundary, expectedRegistration)
	}
	stopCrashDaemon(process)
}

func writeCrashConfig(t *testing.T, home string) {
	t.Helper()
	path, err := config.Path()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	document := fmt.Sprintf("version: 1\nstorage:\n  worktree_root: %s\npool:\n  warm_per_workspace: 0\n  preparation_concurrency: 1\n  git_concurrency_per_repository: 1\nretention:\n  ended_worktree: 0s\n  recovery_snapshot: 1h\ndiscovery:\n  reconcile_interval: 1h\nreadiness:\n  timeout: 10s\n", filepath.Join(home, "worktrees"))
	if err := os.WriteFile(path, []byte(document), 0o600); err != nil {
		t.Fatal(err)
	}
}

func installCrashGitWrapper(t *testing.T, home, realGit, gate, boundary string) {
	t.Helper()
	pattern := map[string]string{"preparing": " worktree add ", "snapshot": " commit-tree ", "snapshot-ref": " update-ref --create-reflog ", "remove": " worktree remove --force ", "restore": " worktree add "}[boundary]
	bin := filepath.Join(home, "bin")
	if err := os.Mkdir(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	script := `#!/bin/sh
if [ -n "$WX_CRASH_PATTERN" ] && [ -f "$WX_CRASH_GATE/armed" ]; then
  case " $* " in
    *"$WX_CRASH_PATTERN"*)
      if mkdir "$WX_CRASH_GATE/claimed" 2>/dev/null; then
        : > "$WX_CRASH_GATE/entered"
        while [ ! -f "$WX_CRASH_GATE/release" ]; do sleep 0.02; done
        "$WX_REAL_GIT" "$@"
        status=$?
        : > "$WX_CRASH_GATE/completed"
        exit "$status"
      fi
      ;;
  esac
fi
exec "$WX_REAL_GIT" "$@"
`
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WX_REAL_GIT", realGit)
	t.Setenv("WX_CRASH_GATE", gate)
	t.Setenv("WX_CRASH_PATTERN", pattern)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func startCrashDaemon(t *testing.T, client rpc.Client) *crashDaemonProcess {
	t.Helper()
	process := &crashDaemonProcess{}
	process.command = exec.Command(os.Args[0], "-test.run=^TestDaemonCrashProcessHelper$")
	process.command.Env = append(os.Environ(), "WX_DAEMON_CRASH_HELPER=1")
	process.command.Stdin = nil
	process.command.Stdout = nil
	process.command.Stderr = &process.stderr
	if err := process.command.Start(); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, 10*time.Second, func() bool {
		var status map[string]any
		return client.Call(context.Background(), "Status", struct{}{}, &status) == nil
	})
	return process
}

func stopCrashDaemon(process *crashDaemonProcess) {
	if process == nil || process.command == nil || process.command.Process == nil {
		return
	}
	_ = process.command.Process.Kill()
	_ = process.command.Wait()
	process.command.Process = nil
}

func restartAfterCrash(t *testing.T, process *crashDaemonProcess, client rpc.Client, gate string) *crashDaemonProcess {
	t.Helper()
	if err := process.command.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := process.command.Wait(); err == nil {
		t.Fatal("killed daemon exited successfully")
	}
	process.command.Process = nil
	if gate != "" {
		if err := os.WriteFile(filepath.Join(gate, "release"), nil, 0o600); err != nil {
			t.Fatal(err)
		}
		waitCrashGate(t, gate, "completed")
	}
	return startCrashDaemon(t, client)
}

func armCrashGate(t *testing.T, gate string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(gate, "armed"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
}

func waitCrashGate(t *testing.T, gate, name string) {
	t.Helper()
	waitUntil(t, 10*time.Second, func() bool {
		_, err := os.Lstat(filepath.Join(gate, name))
		return err == nil
	})
}

func resolveCrashLease(t *testing.T, client rpc.Client, repository string) Lease {
	t.Helper()
	var lease Lease
	if err := client.Call(context.Background(), "ResolveAndLease", map[string]any{"force_worktree": true, "cwd": repository, "branches": []string{}, "agent": "codex", "client_pid": os.Getpid()}, &lease); err != nil {
		t.Fatal(err)
	}
	return lease
}

func waitCrashReady(t *testing.T, client rpc.Client, lease Lease) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := client.Call(ctx, "WaitReady", map[string]any{"session_id": lease.SessionID, "token": lease.Token, "timeout_ms": 10000}, nil); err != nil {
		t.Fatal(err)
	}
}

func releaseCrashLease(t *testing.T, client rpc.Client, lease Lease) {
	t.Helper()
	if err := client.Call(context.Background(), "Release", map[string]any{"session_id": lease.SessionID, "token": lease.Token, "reason": "client-exit"}, nil); err != nil {
		t.Fatal(err)
	}
}

func writeCrashWorkspace(t *testing.T, root string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, "crash-content"), []byte("preserved\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func waitCrashSessionState(t *testing.T, store *state.Store, sessionID, want string) {
	t.Helper()
	waitUntil(t, 10*time.Second, func() bool {
		session, err := store.SessionByID(context.Background(), sessionID)
		return err == nil && session.State == want
	})
}

func assertCrashSessionState(t *testing.T, store *state.Store, sessionID, want string) {
	t.Helper()
	session, err := store.SessionByID(context.Background(), sessionID)
	if err != nil || session.State != want {
		t.Fatalf("session %s state=%s want=%s err=%v", sessionID, session.State, want, err)
	}
}

func assertCrashSnapshot(t *testing.T, store *state.Store, sessionID string) {
	t.Helper()
	snapshots, err := store.Snapshots(context.Background(), sessionID)
	if err != nil || len(snapshots) != 1 {
		t.Fatalf("session %s snapshots=%d err=%v", sessionID, len(snapshots), err)
	}
	if !strings.HasPrefix(snapshots[0].HeadRef, "refs/wx/recovery/") {
		t.Fatalf("unexpected recovery ref %q", snapshots[0].HeadRef)
	}
}
