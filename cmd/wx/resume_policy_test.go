package main

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/HappyOnigiri/WorktreeX/internal/config"
	"github.com/HappyOnigiri/WorktreeX/internal/daemon"
	"github.com/HappyOnigiri/WorktreeX/internal/domain"
	"github.com/HappyOnigiri/WorktreeX/internal/rpc"
	"github.com/HappyOnigiri/WorktreeX/internal/testsupport"
)

// resumeLeaseBoundaryHandler は wx run の引数解析から Resume RPC、lease command の
// 起動・返却までを実 socket 越しに通すための最小 daemon である。
type resumeLeaseBoundaryHandler struct {
	mu      sync.Mutex
	methods []string
	lease   daemon.Lease
}

func (h *resumeLeaseBoundaryHandler) Handle(_ context.Context, method string, _ json.RawMessage) (any, error) {
	h.mu.Lock()
	h.methods = append(h.methods, method)
	h.mu.Unlock()
	if method == "Resume" || method == "ResolveAndLease" {
		return h.lease, nil
	}
	return map[string]any{"ok": true}, nil
}

func (h *resumeLeaseBoundaryHandler) count(method string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	count := 0
	for _, called := range h.methods {
		if called == method {
			count++
		}
	}
	return count
}

// TestRunCommandExplicitResumeIgnoresCallerOffPolicy は実際の wx run の CLI boundary で、
// 許可 workspace からの resume、無関係な off workspace からの resume、新規貸出の拒否を
// 同じ daemon に対して確認する。明示 resume は caller の新規貸出 policy ではなく、
// daemon の Resume が session の存在・種別・所有権を検証するための入口である。
// commentlint:allow-long -- 実際の CLI 境界と3つの policy 経路を同じ socket で検査するため
func TestRunCommandExplicitResumeIgnoresCallerOffPolicy(t *testing.T) {
	home, err := os.MkdirTemp("/tmp", "wx-resume-policy-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(home) })
	home, err = filepath.EvalSymlinks(home)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)

	allowed := makeGitRepository(t, filepath.Join(home, "allowed"))
	blocked := makeGitRepository(t, filepath.Join(home, "blocked"))
	storageRoot := filepath.Join(home, "worktrees")
	leasePath := filepath.Join(storageRoot, "resumed-slot")
	if err := os.MkdirAll(leasePath, 0o700); err != nil {
		t.Fatal(err)
	}
	rootDirectory, identity, err := domain.OpenOwnedDirectory(storageRoot, leasePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := rootDirectory.Close(); err != nil {
		t.Fatal(err)
	}

	cfg := config.Defaults()
	cfg.Version = 2
	cfg.System.Storage.WorktreeRoot = storageRoot
	cfg.Storage.WorktreeRoot = storageRoot
	cfg.System.Lease.Shell = "/usr/bin/true"
	cfg.Lease.Shell = "/usr/bin/true"
	cfg.WorkspaceDefaults.Worktree = "cold"
	cfg.Workspaces = map[string]config.Workspace{
		allowed: {Worktree: "cold"},
		blocked: {Worktree: "off"},
	}
	if err := config.Save(cfg); err != nil {
		t.Fatal(err)
	}

	handler := &resumeLeaseBoundaryHandler{lease: daemon.Lease{
		SessionID:       "resumed-session",
		Token:           "resume-token",
		Path:            leasePath,
		RootIdentity:    identity,
		SourceWorkspace: allowed,
		Ready:           true,
	}}
	socket, err := config.SocketPath()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	server := &rpc.Server{Socket: socket, Handler: handler}
	go func() { done <- server.Serve(ctx) }()
	testsupport.WaitForSocket(t, socket, done)
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	}()

	// 許可された workspace からの明示 resume は従来どおり成功する。
	if exit := runRunFrom(ctx, []string{"--resume", "resumed-session", "--", "true"}, allowed); exit != 0 {
		t.Fatalf("allowed workspace explicit resume exit=%d, want 0", exit)
	}
	if got := handler.count("Resume"); got != 1 {
		t.Fatalf("allowed workspace Resume calls=%d, want 1", got)
	}

	// caller が無関係な off workspace でも、指定 session の Resume へ到達して成功する。
	if exit := runRunFrom(ctx, []string{"--resume", "resumed-session", "--", "true"}, blocked); exit != 0 {
		t.Fatalf("unrelated off workspace explicit resume exit=%d, want 0", exit)
	}
	if got := handler.count("Resume"); got != 2 {
		t.Fatalf("off workspace explicit Resume calls=%d, want 2", got)
	}
	// wx shell は同じ runLeaseFrom の境界を通るため、こちらも caller policy を見ずに復元する。
	if exit := runShellFrom(ctx, []string{"--resume", "resumed-session"}, blocked); exit != 0 {
		t.Fatalf("unrelated off workspace shell resume exit=%d, want 0", exit)
	}
	if got := handler.count("Resume"); got != 3 {
		t.Fatalf("off workspace shell Resume calls=%d, want 3", got)
	}

	// resume の無い新規貸出には、従来どおり caller workspace の off policy を適用する。
	before := handler.count("ResolveAndLease")
	stderr := captureStderr(t, func() {
		if exit := runRunFrom(ctx, []string{"--", "true"}, blocked); exit != 2 {
			t.Fatalf("off workspace new lease exit=%d, want 2", exit)
		}
	})
	if got := handler.count("ResolveAndLease"); got != before {
		t.Fatalf("off workspace new lease ResolveAndLease calls=%d, want %d", got, before)
	}
	if !strings.Contains(stderr, "configured not to use a worktree") {
		t.Fatalf("off workspace new lease stderr=%q", stderr)
	}
}

func makeGitRepository(t *testing.T, path string) string {
	t.Helper()
	if err := os.MkdirAll(path, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"init", "-b", "main"},
		{"config", "user.name", "wx test"},
		{"config", "user.email", "wx-test@example.com"},
	} {
		runGit(t, path, args...)
	}
	if err := os.WriteFile(filepath.Join(path, "tracked"), []byte("base\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, path, "add", "tracked")
	runGit(t, path, "commit", "-m", "initial")
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	command := exec.Command("git", args...)
	command.Dir = dir
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, output)
	}
}
