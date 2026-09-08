package daemon

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/HappyOnigiri/WX/internal/config"
	"github.com/HappyOnigiri/WX/internal/diag"
	"github.com/HappyOnigiri/WX/internal/state"
)

func TestActiveRootAndRootIDForPathFailClosedWithoutARegisteredGeneration(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	store, err := state.Open(filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	cfg := config.Defaults()
	cfg.Storage.WorktreeRoot = filepath.Join(root, "worktrees")
	manager := testManager(t, cfg, store)
	t.Cleanup(manager.Close)

	rootPath, rootID, err := manager.activeRoot()
	if err != nil || rootID == "" || rootPath != filepath.Clean(cfg.Storage.WorktreeRoot) {
		t.Fatalf("active root path=%q id=%q err=%v", rootPath, rootID, err)
	}
	if _, resolvedID, err := manager.rootIDForPath(filepath.Join(rootPath, "wsp001", "slt001")); err != nil || resolvedID != rootID {
		t.Fatalf("root id for slot path=%q err=%v, want %q", resolvedID, err, rootID)
	}
	if _, _, err := manager.rootIDForPath(filepath.Join(t.TempDir(), "elsewhere")); !errors.Is(err, state.ErrOwnership) {
		t.Fatalf("root id outside every root err=%v", err)
	}

	manager.mu.Lock()
	delete(manager.rootIDs, rootPath)
	manager.mu.Unlock()
	if _, _, err := manager.activeRoot(); !errors.Is(err, state.ErrOwnership) {
		t.Fatalf("active root without a generation err=%v", err)
	}
	if _, _, err := manager.rootIDForPath(filepath.Join(rootPath, "wsp001", "slt001")); !errors.Is(err, state.ErrOwnership) {
		t.Fatalf("root id without a generation err=%v", err)
	}
}

func TestRetryRootGenerationRecoversWithoutRestart(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	base := t.TempDir()
	store, err := state.Open(filepath.Join(base, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	cfg := config.Defaults()
	rootPath := filepath.Join(base, "worktrees")
	cfg.Storage.WorktreeRoot = rootPath
	manager := testManager(t, cfg, store)
	t.Cleanup(manager.Close)

	// 起動時にidentityを読めなかった状態を作る。
	manager.mu.Lock()
	manager.rootIDs = map[string]string{}
	manager.rootIdentities = map[string]string{}
	manager.mu.Unlock()
	manager.registerRootGeneration(ctx, rootPath, "")
	if _, _, err := manager.activeRoot(); !errors.Is(err, state.ErrOwnership) {
		t.Fatalf("activeRoot error=%v, want an ownership failure", err)
	}
	registration := doctorProblem(t, manager.Doctor(ctx), diag.CheckWorktreeRootRegistration)
	if !strings.Contains(registration.Action, "retries") {
		t.Fatalf("doctor worktree root registration action=%q, want the failure to read as recoverable", registration.Action)
	}

	manager.retryRootGeneration(ctx)

	manager.mu.RLock()
	remaining := manager.rootError
	manager.mu.RUnlock()
	if remaining != "" {
		t.Fatalf("root error survived a successful retry: %q", remaining)
	}
	gotRoot, gotID, err := manager.activeRoot()
	if err != nil || gotID == "" {
		t.Fatalf("activeRoot root=%q id=%q err=%v, want the re-registered generation", gotRoot, gotID, err)
	}
	manager.mu.RLock()
	pinned := manager.rootIdentities[filepath.Clean(rootPath)]
	manager.mu.RUnlock()
	if pinned == "" {
		t.Fatal("retry left the worktree root identity unpinned")
	}
}

func TestRetryRootGenerationLogsRepeatedFailuresOnce(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	base := t.TempDir()
	store, err := state.Open(filepath.Join(base, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	blocker := filepath.Join(base, "blocker")
	if err := os.WriteFile(blocker, []byte("not a directory\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := config.Defaults()
	rootPath := filepath.Join(blocker, "worktrees")
	cfg.Storage.WorktreeRoot = rootPath
	manager := testManager(t, cfg, store)
	t.Cleanup(manager.Close)
	logs := &strings.Builder{}
	manager.log = slog.New(slog.NewTextHandler(logs, nil))
	manager.setRootError("startup registration failed")

	manager.retryRootGeneration(ctx)
	manager.retryRootGeneration(ctx)

	if got := strings.Count(logs.String(), "retry worktree root generation registration failed"); got != 1 {
		t.Fatalf("retry failure log count=%d, want 1 for a repeated reason:\n%s", got, logs.String())
	}
	manager.mu.RLock()
	remaining := manager.rootError
	manager.mu.RUnlock()
	if remaining == "" || remaining == "startup registration failed" {
		t.Fatalf("root error=%q, want the current retry failure", remaining)
	}
	if _, _, err := manager.activeRoot(); !errors.Is(err, state.ErrOwnership) {
		t.Fatalf("activeRoot error=%v, want the failure to persist", err)
	}

	// 理由が変われば再度記録する。
	manager.setRootError("another reason")
	manager.mu.Lock()
	manager.rootRetryLogged = "another reason"
	manager.mu.Unlock()
	manager.retryRootGeneration(ctx)
	if got := strings.Count(logs.String(), "retry worktree root generation registration failed"); got != 2 {
		t.Fatalf("retry failure log count=%d, want a second entry for a new reason:\n%s", got, logs.String())
	}
}

func TestRootForPathChoosesMostSpecificOverlappingRoot(t *testing.T) {
	t.Parallel()
	parent := t.TempDir()
	outer := filepath.Join(parent, "outer")
	inner := filepath.Join(outer, "inner")
	path := filepath.Join(inner, "workspace", "slot")
	m := &Manager{roots: map[string]bool{outer: true, inner: true}}

	for range 100 {
		got, ok := m.rootForPath(path)
		if !ok || got != inner {
			t.Fatalf("rootForPath(%q)=%q,%v want nested root %q", path, got, ok, inner)
		}
	}
}
