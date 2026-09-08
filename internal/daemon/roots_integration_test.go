package daemon

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestWorktreeRootChangeKeepsExistingSessionsAndPlacesNewOnesInTheNewRoot(t *testing.T) {
	requireDaemonIntegration(t)
	f := runningManagerFixture(t, func(s *managerFixtureSetup) {
		t.Setenv("HOME", s.Root)
		s.Config.Storage.WorktreeRoot = filepath.Join(s.Root, "worktrees-old")
		s.Config.Pool.WarmPerWorkspace = 0
		s.Config.Discovery.ReconcileInterval.Duration = time.Hour
		writeWorktreeRootConfig(t, s.Root, s.Config.Storage.WorktreeRoot)
	})
	store, m := f.Store, f.Manager
	home := f.Root
	oldRoot := f.Config.Storage.WorktreeRoot
	newRoot := filepath.Join(home, "worktrees-new")
	repo := filepath.Join(home, "repo")
	initGitRepo(t, repo)
	ctx := context.Background()

	existing, err := m.ResolveAndLease(ctx, repo, nil, "codex", os.Getpid(), leaseAttrs{})
	if err != nil {
		t.Fatal(err)
	}
	if err := waitReady(ctx, m, 20*time.Second, existing.SessionID, existing.Token); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(existing.Path, oldRoot+string(filepath.Separator)) {
		t.Fatalf("first lease path=%q, want it under %q", existing.Path, oldRoot)
	}
	existingSlot, err := store.Slot(ctx, existing.SessionID)
	if err != nil {
		t.Fatal(err)
	}

	writeWorktreeRootConfig(t, home, newRoot)
	if err := m.ReloadConfig(); err != nil {
		t.Fatal(err)
	}

	afterReload, err := store.Slot(ctx, existing.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if afterReload.RootID != existingSlot.RootID || afterReload.Path != existingSlot.Path {
		t.Fatalf("existing slot moved: before=%+v after=%+v", existingSlot, afterReload)
	}
	if afterReload.State != existingSlot.State || afterReload.FailureCode != "" {
		t.Fatalf("existing slot was drained: state=%s failure_code=%s", afterReload.State, afterReload.FailureCode)
	}
	if info, err := os.Lstat(existing.Path); err != nil || !info.IsDir() {
		t.Fatalf("existing worktree disappeared: info=%v err=%v", info, err)
	}
	if status := gitOutput(t, existing.Path, "status", "--porcelain"); status != "" {
		t.Fatalf("existing worktree is no longer usable: %q", status)
	}

	fresh, err := m.ResolveAndLease(ctx, repo, nil, "codex", os.Getpid(), leaseAttrs{})
	if err != nil {
		t.Fatal(err)
	}
	if err := waitReady(ctx, m, 20*time.Second, fresh.SessionID, fresh.Token); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(fresh.Path, newRoot+string(filepath.Separator)) {
		t.Fatalf("lease after the root change=%q, want it under %q", fresh.Path, newRoot)
	}
	freshSlot, err := store.Slot(ctx, fresh.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if freshSlot.RootID == existingSlot.RootID {
		t.Fatalf("new slot reused the retired root generation %q", existingSlot.RootID)
	}
	roots, err := store.Roots(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(roots) != 2 {
		t.Fatalf("registered roots=%+v, want the retired generation kept alongside the active one", roots)
	}
}

func writeWorktreeRootConfig(t *testing.T, home, root string) {
	t.Helper()
	path := filepath.Join(home, ".config", "wx", "config.yaml")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	document := fmt.Sprintf("version: 1\nstorage:\n  worktree_root: %s\npool:\n  warm_per_workspace: 0\ndiscovery:\n  reconcile_interval: 1h\n", root)
	if err := os.WriteFile(path, []byte(document), 0o600); err != nil {
		t.Fatal(err)
	}
}
