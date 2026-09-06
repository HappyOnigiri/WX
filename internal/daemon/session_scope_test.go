package daemon

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/HappyOnigiri/WX/internal/config"
	"github.com/HappyOnigiri/WX/internal/discovery"
	"github.com/HappyOnigiri/WX/internal/state"
)

// 未登録のworkspaceでもcwdの解決結果は返し、登録後はslot pathとsessionが同じIDの下に並ぶことを検査する。
func TestWorkspaceScopeReportsRegistrationSlotPathsAndSessions(t *testing.T) {
	t.Parallel()
	requireDaemonIntegration(t)
	root := t.TempDir()
	repository := filepath.Join(root, "repo")
	initGitRepo(t, repository)
	store, err := state.Open(filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cfg := config.Defaults()
	cfg.Storage.WorktreeRoot = filepath.Join(root, "worktrees")
	m := testManager(t, cfg, store)
	m.git.SetTimeout(10 * time.Second)
	defer m.Close()
	ctx := context.Background()

	unregistered, err := m.WorkspaceScope(ctx, repository)
	if err != nil {
		t.Fatal(err)
	}
	if unregistered.Registered {
		t.Fatalf("unobserved workspace reported as registered: %+v", unregistered)
	}
	if unregistered.Kind != "repository" || filepath.Clean(unregistered.Root) != filepath.Clean(repository) {
		t.Fatalf("unregistered scope kind=%q root=%q", unregistered.Kind, unregistered.Root)
	}
	if len(unregistered.SlotPaths) != 0 || len(unregistered.Sessions) != 0 {
		t.Fatalf("unregistered scope carried state: %+v", unregistered)
	}

	discoverer := discovery.Discoverer{Git: m.git, Config: cfg}
	w, err := discoverer.Resolve(ctx, repository)
	if err != nil {
		t.Fatal(err)
	}
	w = registerTestWorkspace(t, store, w)
	slot := testSlot(t, m, string(w.ID), "scope-slot", 1, "LEASED")
	session := state.Session{ID: "scope-session", WorkspaceID: string(w.ID), SlotID: "scope-slot", State: "ACTIVE", AgentKind: "codex", TokenHash: state.HashToken("scope-session")}
	if _, err := store.CreateSlotSession(ctx, slot, nil, session, ""); err != nil {
		t.Fatal(err)
	}

	registered, err := m.WorkspaceScope(ctx, repository)
	if err != nil {
		t.Fatal(err)
	}
	if !registered.Registered || registered.WorkspaceID != string(w.ID) {
		t.Fatalf("registered scope=%+v, want workspace %s", registered, w.ID)
	}
	if len(registered.SlotPaths) != 1 || filepath.Clean(registered.SlotPaths[0]) != filepath.Clean(slot.Path) {
		t.Fatalf("scope slot paths=%v, want %s", registered.SlotPaths, slot.Path)
	}
	if len(registered.Sessions) != 1 {
		t.Fatalf("scope sessions=%+v, want exactly one", registered.Sessions)
	}
	if got := registered.Sessions[0]; got.ID != "scope-session" || got.Agent != "codex" || got.State != "ACTIVE" {
		t.Fatalf("scope session=%+v", got)
	}
}

// cwdがどのrepositoryにも属さないときは、空のscopeを返さずエラーで止まる。
func TestWorkspaceScopeFailsOutsideAnyRepository(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	store, err := state.Open(filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cfg := config.Defaults()
	cfg.Storage.WorktreeRoot = filepath.Join(root, "worktrees")
	m := testManager(t, cfg, store)
	m.git.SetTimeout(10 * time.Second)
	defer m.Close()

	outside := filepath.Join(root, "outside")
	if err := os.MkdirAll(outside, 0o700); err != nil {
		t.Fatal(err)
	}
	if scope, err := m.WorkspaceScope(context.Background(), outside); err == nil {
		t.Fatalf("scope outside every repository succeeded: %+v", scope)
	}
}
