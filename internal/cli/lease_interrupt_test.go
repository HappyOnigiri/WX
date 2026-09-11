package cli

import (
	"context"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// holdInterrupt は test process が SIGINT の既定動作で終了しないようにする。
// 受信登録が 1 つでもある間は既定の終了が起きないため、被験コードの捕捉が外れても test 自体は生き残る。
func holdInterrupt(t *testing.T) {
	t.Helper()
	received := make(chan os.Signal, 1)
	signal.Notify(received, os.Interrupt)
	t.Cleanup(func() { signal.Stop(received) })
}

// blockWaitReadyUntilInterrupt は準備待ちを止め、client が待機に入ったら SIGINT を送る。
func blockWaitReadyUntilInterrupt(t *testing.T, handler *launcherHandler) {
	t.Helper()
	entered := make(chan struct{})
	handler.mu.Lock()
	handler.lease.Ready = false
	handler.waitReadyHook = func(ctx context.Context) error {
		close(entered)
		<-ctx.Done()
		return ctx.Err()
	}
	handler.mu.Unlock()
	go func() {
		<-entered
		_ = syscall.Kill(os.Getpid(), syscall.SIGINT)
	}()
}

// releaseReasons は記録された Release 要求の理由を返す。
func releaseReasons(handler *launcherHandler) []string {
	handler.mu.Lock()
	defer handler.mu.Unlock()
	return append([]string(nil), handler.releaseReasons...)
}

// wx new の準備待ちを Ctrl-C で中断しても、貸出はその場で返却される。
// path 貸出は heartbeat も client_pid も持たず orphan 回収の対象外なので、
// ここで返さないと利用者に path を渡せなかった slot が lease.ttl まで残る。
func TestRunLeaseNewReleasesTheLeaseWhenInterruptedBeforeReady(t *testing.T) {
	client, handler, _, ctx := leaseFixture(t)
	holdInterrupt(t)
	// 中断が効かなければ、この予算いっぱい待たされる。
	client.Config.Readiness.Timeout.Duration = time.Minute
	blockWaitReadyUntilInterrupt(t, handler)
	started := time.Now()
	if exit := client.RunLeaseNew(ctx, nil, true); exit != 1 {
		t.Fatalf("RunLeaseNew exit=%d, want 1 after an interrupt", exit)
	}
	if elapsed := time.Since(started); elapsed >= client.Config.Readiness.Timeout.Duration {
		t.Fatalf("RunLeaseNew waited %v; the interrupt did not stop the wait", elapsed)
	}
	if reasons := releaseReasons(handler); len(reasons) != 1 || reasons[0] != "lease-setup-failed" {
		t.Fatalf("release reasons=%v, want a single lease-setup-failed release", reasons)
	}
}

// wx shell の準備待ちを Ctrl-C で中断すると、agent を起動せずに貸出を返す。
func TestRunLeaseShellReleasesTheLeaseWhenInterruptedBeforeReady(t *testing.T) {
	client, handler, base, ctx := leaseFixture(t)
	holdInterrupt(t)
	shell := filepath.Join(base, "shell")
	started := filepath.Join(base, "shell-started")
	if err := os.WriteFile(shell, []byte("#!/bin/sh\ntouch \""+started+"\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	client.Config.Lease.Shell = shell
	client.Config.Readiness.Timeout.Duration = time.Minute
	blockWaitReadyUntilInterrupt(t, handler)
	if exit := client.RunLeaseShell(ctx, nil, ""); exit != 1 {
		t.Fatalf("RunLeaseShell exit=%d, want 1 after an interrupt", exit)
	}
	if _, err := os.Lstat(started); err == nil {
		t.Fatal("the shell was started although the preparation was interrupted")
	}
	if reasons := releaseReasons(handler); len(reasons) != 1 || reasons[0] != "client-exit" {
		t.Fatalf("release reasons=%v, want a single client-exit release", reasons)
	}
	handler.mu.Lock()
	methods := strings.Join(handler.methods, ",")
	handler.mu.Unlock()
	if strings.Contains(methods, "RegisterAgentProcess") {
		t.Fatalf("methods=%s, want no agent registration after an interrupt", methods)
	}
}
