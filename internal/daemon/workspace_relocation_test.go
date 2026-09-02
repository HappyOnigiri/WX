package daemon

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/HappyOnigiri/WX/internal/config"
	"github.com/HappyOnigiri/WX/internal/discovery"
	"github.com/HappyOnigiri/WX/internal/domain"
	"github.com/HappyOnigiri/WX/internal/gitx"
	"github.com/HappyOnigiri/WX/internal/state"
)

func TestMainWorktreeRelocationReusesRegisteredWorkspace(t *testing.T) {
	root := t.TempDir()
	oldMain := filepath.Join(root, "old-main")
	newMain := filepath.Join(root, "new-main")
	common := filepath.Join(root, "common.git")
	if err := os.MkdirAll(newMain, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(common, 0o700); err != nil {
		t.Fatal(err)
	}
	fakeBin := filepath.Join(root, "bin")
	if err := os.MkdirAll(fakeBin, 0o700); err != nil {
		t.Fatal(err)
	}
	fakeGit := `#!/bin/sh
case "$1 $2" in
  "rev-parse --show-toplevel")
    printf '%s\n' "$WX_RELOCATION_MAIN"
    exit 0
    ;;
  "worktree list")
    printf 'worktree %s\0' "$WX_RELOCATION_MAIN"
    exit 0
    ;;
esac
if [ "$1" = "rev-parse" ] && [ "$2" = "--path-format=absolute" ] && [ "$3" = "--git-common-dir" ]; then
  printf '%s\n' "$WX_RELOCATION_COMMON"
  exit 0
fi
if [ "$1" = "rev-parse" ] && [ "$2" = "--verify" ]; then
  printf 'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\n'
  exit 0
fi
exit 1
`
	if err := os.WriteFile(filepath.Join(fakeBin, "git"), []byte(fakeGit), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("WX_RELOCATION_MAIN", newMain)
	t.Setenv("WX_RELOCATION_COMMON", common)

	store, err := state.Open(filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cfg := config.Defaults()
	cfg.Storage.WorktreeRoot = filepath.Join(root, "worktrees")
	cfg.Pool.WarmPerWorkspace = 0
	cfg.Readiness.Timeout.Duration = time.Second
	m := testManager(t, cfg, store)
	defer m.Close()
	// The fake Git path is exercised by coverage and race instrumentation; leave
	// enough room for process startup while keeping the production timeout intact.
	m.git.SetTimeout(5 * time.Second)
	ctx := context.Background()
	oldWorkspaceID := domain.WorkspaceID("old-workspace-id")
	repositoryID := domain.RepositoryID(domain.StableID(common))
	registered := discovery.Workspace{
		ID: oldWorkspaceID, Root: domain.CanonicalPath(oldMain), Kind: "repository",
		Repositories: []discovery.Repository{{
			ID: repositoryID, MainPath: domain.CanonicalPath(oldMain), CommonDir: domain.CanonicalPath(common), RelativePath: ".", DefaultBranch: "main",
		}},
	}
	if err := store.UpsertWorkspace(ctx, registered); err != nil {
		t.Fatal(err)
	}
	oldSlotPath := filepath.Join(cfg.Storage.WorktreeRoot, "workspaces", string(oldWorkspaceID), "slots", "old-slot", "root")
	if err := os.MkdirAll(oldSlotPath, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateStandby(ctx, state.Slot{ID: "old-slot", WorkspaceID: string(oldWorkspaceID), Generation: 1, Path: oldSlotPath, State: "FAILED"}, nil); err != nil {
		t.Fatal(err)
	}

	// A restarted manager only has the old root from SQLite. It must recover the
	// moved main worktree from Git's common directory and update that same row.
	discoverer := discovery.Discoverer{Git: m.git, Config: m.Config()}
	if _, resolveErr := m.resolveRegisteredWorkspace(ctx, oldMain, &discoverer); resolveErr != nil {
		t.Fatalf("common-directory recovery failed: %v", resolveErr)
	}
	m.reconcileRegistry(ctx)
	roots, err := store.WorkspaceRoots(ctx)
	if err != nil || len(roots) != 1 || roots[0] != newMain {
		t.Fatalf("registered roots=%v err=%v, want only relocated main path", roots, err)
	}
	updated, err := store.Workspace(ctx, string(oldWorkspaceID))
	if err != nil {
		t.Fatal(err)
	}
	if updated.Root != domain.CanonicalPath(newMain) || updated.Repositories[0].MainPath != domain.CanonicalPath(newMain) {
		t.Fatalf("updated workspace=%+v, want existing identity at new path", updated)
	}
	if slot, err := store.Slot(ctx, "old-slot"); err != nil || slot.WorkspaceID != string(oldWorkspaceID) {
		t.Fatalf("old slot=%+v err=%v, relocation duplicated or detached the pool", slot, err)
	}
	if status, err := store.Status(ctx); err != nil || status.Workspaces != 1 {
		t.Fatalf("status=%+v err=%v, want one workspace", status, err)
	}

	// A request through the new path must resolve to the same workspace ID and
	// therefore continue using the existing workspaces/<id>/slots namespace.
	lease, err := m.ResolveAndLease(ctx, newMain, nil, "codex", os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	wantPrefix := filepath.Join(cfg.Storage.WorktreeRoot, "workspaces", string(oldWorkspaceID), "slots") + string(os.PathSeparator)
	if !strings.HasPrefix(lease.Path, wantPrefix) {
		t.Fatalf("new-path lease=%q, want existing workspace namespace %q", lease.Path, wantPrefix)
	}
	leasedSession, err := store.SessionByID(ctx, lease.SessionID)
	if err != nil || leasedSession.WorkspaceID != string(oldWorkspaceID) {
		t.Fatalf("new-path session=%+v err=%v, want existing workspace identity", leasedSession, err)
	}

	// A fresh native resume after restart uses the relocated workspace row and
	// must allocate into that same namespace as well.
	parent := state.Session{ID: "expired-parent", WorkspaceID: string(oldWorkspaceID), SlotID: "expired-parent", State: "EXPIRED", AgentKind: "codex", TokenHash: state.HashToken("parent")}
	if _, err := store.CreateSlotSession(ctx, state.Slot{ID: parent.SlotID, WorkspaceID: parent.WorkspaceID, Generation: 1, Path: filepath.Join(cfg.Storage.WorktreeRoot, "workspaces", string(oldWorkspaceID), "slots", parent.SlotID, "root"), State: "ARCHIVED"}, nil, parent, ""); err != nil {
		t.Fatal(err)
	}
	resumed, err := m.Resume(ctx, parent.ID, "codex", os.Getpid(), true)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(resumed.Path, wantPrefix) {
		t.Fatalf("resumed lease=%q, want existing workspace namespace %q", resumed.Path, wantPrefix)
	}
	resumedSession, err := store.SessionByID(ctx, resumed.SessionID)
	if err != nil || resumedSession.WorkspaceID != string(oldWorkspaceID) {
		t.Fatalf("resumed session=%+v err=%v, want existing workspace identity", resumedSession, err)
	}
}

func TestMainWorktreeRelocationWithGitRegistryPreservesIdentityAndSessions(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source")
	common := filepath.Join(root, "common.git")
	oldMain := filepath.Join(root, "old-main")
	newMain := filepath.Join(root, "new-main")
	initGitRepo(t, source)
	gitRun(t, root, "clone", "--bare", source, common)
	gitRun(t, common, "worktree", "add", oldMain, "main")

	cfg := config.Defaults()
	cfg.Storage.WorktreeRoot = filepath.Join(root, "worktrees")
	cfg.Pool.WarmPerWorkspace = 0
	cfg.Readiness.Timeout.Duration = time.Second
	runner := &gitx.Runner{Timeout: 5 * time.Second}
	discoverer := discovery.Discoverer{Git: runner, Config: cfg}
	ctx := context.Background()
	discovered, err := discoverer.Resolve(ctx, oldMain)
	if err != nil {
		t.Fatal(err)
	}
	if discovered.Repositories[0].CommonDir != discoveryPath(common) {
		t.Fatalf("discovered common directory=%q, want %q", discovered.Repositories[0].CommonDir, common)
	}

	store, err := state.Open(filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	// Seed the pre-common-directory identity used by an older database. The
	// repository common directory is real, so the post-move reconciliation must
	// retain this ID and its existing slot namespace.
	legacy := discovered
	legacy.ID = domain.WorkspaceID(domain.StableID(string(discovered.Root)))
	if err := store.UpsertWorkspace(ctx, legacy); err != nil {
		t.Fatal(err)
	}
	m := testManager(t, cfg, store)
	defer m.Close()
	oldSlotPath := filepath.Join(cfg.Storage.WorktreeRoot, "workspaces", string(legacy.ID), "slots", "old-slot", "root")
	if err := os.MkdirAll(oldSlotPath, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateStandby(ctx, state.Slot{ID: "old-slot", WorkspaceID: string(legacy.ID), Generation: 1, Path: oldSlotPath, State: "FAILED"}, nil); err != nil {
		t.Fatal(err)
	}

	gitRun(t, common, "worktree", "move", oldMain, newMain)
	if _, err := os.Stat(oldMain); !os.IsNotExist(err) {
		t.Fatalf("old main worktree still exists: %v", err)
	}
	if _, err := os.Stat(newMain); err != nil {
		t.Fatal(err)
	}

	if _, err := m.resolveRegisteredWorkspace(ctx, oldMain, &discoverer); err != nil {
		t.Fatalf("common-directory recovery failed: %v", err)
	}
	m.reconcileRegistry(ctx)
	updated, err := store.Workspace(ctx, string(legacy.ID))
	if err != nil {
		t.Fatal(err)
	}
	if updated.ID != legacy.ID || updated.Root != discoveryPath(newMain) || updated.Repositories[0].MainPath != discoveryPath(newMain) {
		t.Fatalf("relocated workspace=%+v, want legacy identity at new main path", updated)
	}
	if _, err := store.Workspace(ctx, string(discovered.ID)); err == nil {
		t.Fatalf("relocation created a duplicate common-derived workspace %q", discovered.ID)
	}
	if slot, err := store.Slot(ctx, "old-slot"); err != nil || slot.WorkspaceID != string(legacy.ID) {
		t.Fatalf("relocated slot=%+v err=%v", slot, err)
	}

	lease, err := m.ResolveAndLease(ctx, newMain, nil, "codex", os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	wantPrefix := filepath.Join(cfg.Storage.WorktreeRoot, "workspaces", string(legacy.ID), "slots") + string(os.PathSeparator)
	if !strings.HasPrefix(lease.Path, wantPrefix) {
		t.Fatalf("new-path lease=%q, want existing workspace namespace %q", lease.Path, wantPrefix)
	}
	leasedSession, err := store.SessionByID(ctx, lease.SessionID)
	if err != nil || leasedSession.WorkspaceID != string(legacy.ID) {
		t.Fatalf("new-path session=%+v err=%v, want existing workspace identity", leasedSession, err)
	}

	parent := state.Session{ID: "expired-parent", WorkspaceID: string(legacy.ID), SlotID: "expired-parent", State: "EXPIRED", AgentKind: "codex", TokenHash: state.HashToken("parent")}
	if _, err := store.CreateSlotSession(ctx, state.Slot{ID: parent.SlotID, WorkspaceID: parent.WorkspaceID, Generation: 1, Path: filepath.Join(cfg.Storage.WorktreeRoot, "workspaces", string(legacy.ID), "slots", parent.SlotID, "root"), State: "ARCHIVED"}, nil, parent, ""); err != nil {
		t.Fatal(err)
	}
	resumed, err := m.Resume(ctx, parent.ID, "codex", os.Getpid(), true)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(resumed.Path, wantPrefix) {
		t.Fatalf("resumed lease=%q, want existing workspace namespace %q", resumed.Path, wantPrefix)
	}
	resumedSession, err := store.SessionByID(ctx, resumed.SessionID)
	if err != nil || resumedSession.WorkspaceID != string(legacy.ID) {
		t.Fatalf("resumed session=%+v err=%v, want existing workspace identity", resumedSession, err)
	}
}
