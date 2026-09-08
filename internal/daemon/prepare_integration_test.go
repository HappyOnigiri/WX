package daemon

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/HappyOnigiri/WX/internal/config"
	"github.com/HappyOnigiri/WX/internal/domain"
	"github.com/HappyOnigiri/WX/internal/state"
)

func TestMultiRepositoryBundleAndRootRules(t *testing.T) {
	t.Parallel()
	requireDaemonIntegration(t)
	f := runningManagerFixture(t, func(s *managerFixtureSetup) {
		s.Config.Worktree.Undefined = "hot"
		s.Config.Pool.WarmPerWorkspace = 1
		s.Config.Discovery.ReconcileInterval.Duration = time.Hour
		s.Config.Retention.EndedWorktree.Duration = 0
		s.Config.Workspaces = map[string]config.Workspace{s.Root: {Link: []string{"audit"}}}
	})
	store, m := f.Store, f.Manager
	root := f.Root
	initGitRepo(t, filepath.Join(root, "service"))
	initGitRepo(t, filepath.Join(root, "web"))
	if err := os.WriteFile(filepath.Join(root, "AGENTS.md"), []byte("root rules\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "audit"), 0o700); err != nil {
		t.Fatal(err)
	}
	lease, err := m.ResolveAndLease(context.Background(), root, nil, "codex", 1, leaseAttrs{})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := m.WaitReady(ctx, lease.SessionID, lease.Token); err != nil {
		details, detailsErr := store.StatusDiagnostics(context.Background())
		t.Fatalf("wait for multi-repository bundle: %v; diagnostics=%+v diagnostics_error=%v", err, details, detailsErr)
	}
	for _, name := range []string{"service", "web"} {
		path := filepath.Join(lease.Path, name)
		if got := gitOutput(t, path, "rev-parse", "--abbrev-ref", "HEAD"); got != "HEAD" {
			t.Fatalf("%s branch=%s", name, got)
		}
	}
	data, err := os.ReadFile(filepath.Join(lease.Path, "AGENTS.md"))
	if err != nil || string(data) != "root rules\n" {
		t.Fatalf("root rules=%q err=%v", data, err)
	}
	info, err := os.Lstat(filepath.Join(lease.Path, "audit"))
	if err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("audit link=%v err=%v", info, err)
	}
	activeRepos, err := store.SlotRepositories(context.Background(), lease.SessionID)
	if err != nil || len(activeRepos) != 2 {
		t.Fatalf("active repository count=%d err=%v", len(activeRepos), err)
	}
	initGitRepo(t, filepath.Join(root, "api"))
	m.reconcileRegistry(context.Background())
	updated, err := store.WorkspaceByRoot(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	generation, err := store.WorkspaceGeneration(context.Background(), string(updated.ID))
	if err != nil || generation != 2 {
		t.Fatalf("updated generation=%d err=%v", generation, err)
	}
	activeRepos, err = store.SlotRepositories(context.Background(), lease.SessionID)
	if err != nil || len(activeRepos) != 2 {
		t.Fatalf("active session membership changed: count=%d err=%v", len(activeRepos), err)
	}
	waitUntil(t, 10*time.Second, func() bool {
		ready, ok, _ := store.ReadySlot(context.Background(), string(updated.ID))
		if !ok || ready.Generation != 2 {
			return false
		}
		repos, _ := store.SlotRepositories(context.Background(), ready.ID)
		return len(repos) == 3
	})
	if _, err := m.GC(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	var coldSlot state.Slot
	waitUntil(t, 10*time.Second, func() bool {
		ready, ok, _ := store.ReadySlot(context.Background(), string(updated.ID))
		if !ok || ready.Generation != 2 {
			return false
		}
		repositories, _ := store.SlotRepositories(context.Background(), ready.ID)
		states := map[string]string{}
		for _, repository := range repositories {
			states[repository.RepositoryID] = repository.State
		}
		apiID := string(updated.Repositories[0].ID)
		for _, repository := range updated.Repositories {
			if repository.RelativePath == "api" {
				apiID = string(repository.ID)
			}
		}
		if states[apiID] != "COLD" {
			return false
		}
		for _, repository := range updated.Repositories {
			if repository.RelativePath != "api" && states[string(repository.ID)] != "READY" {
				return false
			}
		}
		coldSlot = ready
		return true
	})
	coldLease, err := m.ResolveAndLease(context.Background(), root, nil, "codex", os.Getpid(), leaseAttrs{})
	if err != nil {
		t.Fatal(err)
	}
	if coldLease.SessionID != coldSlot.ID || coldLease.Ready {
		t.Fatalf("cold bundle lease=%+v slot=%+v", coldLease, coldSlot)
	}
	// 上のctxはこの時点までの経過時間も食っているため、この待機には独自の予算を与える。
	coldWait, coldCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer coldCancel()
	if err := m.WaitReady(coldWait, coldLease.SessionID, coldLease.Token); err != nil {
		t.Fatal(err)
	}
	for _, repository := range updated.Repositories {
		if _, err := os.Stat(filepath.Join(coldLease.Path, repository.RelativePath, ".git")); err != nil {
			t.Fatalf("repository %s was not rematerialized: %v", repository.RelativePath, err)
		}
	}
	if err := os.WriteFile(filepath.Join(lease.Path, "AGENTS.md"), []byte("session-specific rules\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(lease.Path, "notes"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(lease.Path, "notes", "todo.txt"), []byte("preserve root state\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := m.Release(context.Background(), lease.SessionID, lease.Token, "test"); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, 10*time.Second, func() bool {
		session, sessionErr := store.SessionByID(context.Background(), lease.SessionID)
		return sessionErr == nil && session.State == "ARCHIVED"
	})
	if _, err := m.GC(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, 10*time.Second, func() bool {
		slot, slotErr := store.Slot(context.Background(), lease.SessionID)
		_, pathErr := os.Lstat(lease.Path)
		return slotErr == nil && slot.State == "ARCHIVED" && os.IsNotExist(pathErr)
	})
	rootSnapshot, found, err := store.WorkspaceSnapshot(context.Background(), lease.SessionID)
	if err != nil || !found {
		t.Fatalf("workspace root snapshot found=%v snapshot=%+v err=%v", found, rootSnapshot, err)
	}
	resumeCtx, resumeCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer resumeCancel()
	resumed, err := m.Resume(resumeCtx, lease.SessionID, "codex", os.Getpid(), false)
	if err != nil {
		t.Fatal(err)
	}
	if err := waitReady(context.Background(), m, 10*time.Second, resumed.SessionID, resumed.Token); err != nil {
		t.Fatal(err)
	}
	assertWorkspaceTestFile(t, filepath.Join(resumed.Path, "AGENTS.md"), "session-specific rules\n")
	assertWorkspaceTestFile(t, filepath.Join(resumed.Path, "notes", "todo.txt"), "preserve root state\n")
	if _, err := os.Lstat(filepath.Join(resumed.Path, "api")); !os.IsNotExist(err) {
		t.Fatalf("repository added after the archived session leaked into Resume: %v", err)
	}
	if info, err := os.Lstat(filepath.Join(resumed.Path, "audit")); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("shared root link was not rematerialized: info=%v err=%v", info, err)
	}
}

func assertWorkspaceTestFile(t *testing.T, path, expected string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil || string(data) != expected {
		t.Fatalf("workspace file %s=%q err=%v", path, data, err)
	}
}

func TestMultiRepositorySiblingLinkedWorktreesAcquireASession(t *testing.T) {
	requireDaemonIntegration(t)
	f := runningManagerFixture(t, func(s *managerFixtureSetup) {
		t.Setenv("HOME", s.Root)
		s.Config.Pool.WarmPerWorkspace = 0
		s.Config.Discovery.ReconcileInterval.Duration = time.Hour
	})
	store, m := f.Store, f.Manager
	bundle := filepath.Join(f.Root, "bundle")
	server := filepath.Join(bundle, "server")
	client := filepath.Join(bundle, "client")
	initGitRepo(t, server)
	initGitRepo(t, client)
	for _, name := range []string{"server-feature", "server-hotfix"} {
		gitRun(t, server, "worktree", "add", "--detach", filepath.Join(bundle, name))
	}
	for _, name := range []string{"a", "b"} {
		gitRun(t, server, "worktree", "add", "--detach", filepath.Join(bundle, "worktrees", "server-"+name))
	}
	ctx := context.Background()

	lease, err := m.ResolveAndLease(ctx, bundle, nil, "codex", os.Getpid(), leaseAttrs{})
	if err != nil {
		t.Fatalf("multi-repository lease with sibling linked worktrees: %v", err)
	}
	if err := waitReady(ctx, m, 30*time.Second, lease.SessionID, lease.Token); err != nil {
		t.Fatal(err)
	}
	repos, err := store.SlotRepositories(ctx, lease.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if len(repos) != 2 {
		names := make([]string, 0, len(repos))
		for _, repository := range repos {
			names = append(names, repository.DirName)
		}
		t.Fatalf("slot repositories=%v, want exactly the two distinct repositories", names)
	}
	for _, repository := range repos {
		worktree := filepath.Join(lease.Path, repository.DirName)
		if info, err := os.Lstat(filepath.Join(worktree, ".git")); err != nil || info.Mode()&os.ModeSymlink != 0 {
			t.Fatalf("repository %s was not checked out at %s: info=%v err=%v", repository.RepositoryID, worktree, info, err)
		}
	}
	w, err := store.SessionWorkspace(ctx, lease.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	for _, repository := range w.Repositories {
		located, canonicalErr := domain.Canonicalize(filepath.Join(string(w.Root), repository.RelativePath))
		if canonicalErr != nil {
			t.Fatal(canonicalErr)
		}
		if located != repository.MainPath {
			t.Fatalf("repository %s kept relative path %q resolving to %s, not its main worktree %s", repository.ID, repository.RelativePath, located, repository.MainPath)
		}
	}
}
