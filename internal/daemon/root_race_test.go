package daemon

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/HappyOnigiri/WX/internal/config"
	"github.com/HappyOnigiri/WX/internal/discovery"
	"github.com/HappyOnigiri/WX/internal/domain"
	"github.com/HappyOnigiri/WX/internal/gitx"
	"github.com/HappyOnigiri/WX/internal/pool"
	"github.com/HappyOnigiri/WX/internal/state"
)

// NEW-1: allocation, and the identity later attached to ready/native leases,
// stay in the daemon-held root inode when its pathname is replaced at the
// exact allocation barrier.
func TestCreateSlotRootPinsAllocationAcrossRootReplacement(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "worktrees")
	outside := filepath.Join(parent, "outside")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(outside, 0o700); err != nil {
		t.Fatal(err)
	}
	pinned, _, err := domain.OpenOwnedRoot(root, root)
	if err != nil {
		t.Fatal(err)
	}
	m := &Manager{
		cfg:         func() config.Config { cfg := config.Defaults(); cfg.Storage.WorktreeRoot = root; return cfg }(),
		git:         &gitx.Runner{},
		log:         slog.New(slog.NewTextHandler(io.Discard, nil)),
		roots:       map[string]bool{root: true},
		rootHandles: map[string]*os.Root{root: pinned},
	}
	m.beforeSlotRootCreate = func() {
		old := root + "-old"
		if err := os.Rename(root, old); err != nil {
			t.Fatalf("replace configured root: %v", err)
		}
		if err := os.Symlink(outside, root); err != nil {
			t.Fatalf("install replacement root: %v", err)
		}
	}
	t.Cleanup(m.Close)

	target := filepath.Join(root, "workspaces", "workspace", "slots", "slot", "root")
	identity, err := m.createSlotRoot(target)
	if err != nil {
		t.Fatalf("create slot root: %v", err)
	}
	oldTarget := filepath.Join(root+"-old", "workspaces", "workspace", "slots", "slot", "root")
	if info, err := os.Stat(oldTarget); err != nil || !info.IsDir() {
		t.Fatalf("allocation did not remain in pinned root: info=%v err=%v", info, err)
	}
	if _, err := os.Lstat(filepath.Join(outside, "workspaces")); !os.IsNotExist(err) {
		t.Fatalf("allocation escaped into replacement directory: %v", err)
	}
	if got, err := m.leaseRootIdentity(target); err != nil || got != identity {
		t.Fatalf("lease identity got=%q want=%q err=%v", got, identity, err)
	}
}

// NEW-2: when the post-add ownership/registration proof fails, the manager
// must quarantine the slot while retaining the descriptor-created worktree.
func TestPrepareQuarantinesUncertainGitAddAfterRootReplacement(t *testing.T) {
	base := t.TempDir()
	repository := filepath.Join(base, "repository")
	root := filepath.Join(base, "worktrees")
	outside := filepath.Join(base, "outside")
	initGitRepo(t, repository)
	if err := os.Mkdir(outside, 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := config.Defaults()
	cfg.Storage.WorktreeRoot = root
	cfg.Pool.WarmPerWorkspace = 0
	cfg.Readiness.Timeout.Duration = 5 * time.Second
	store, err := state.Open(filepath.Join(base, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	m := testManager(t, cfg, store)
	t.Cleanup(m.Close)
	ctx := context.Background()
	runner := m.git
	discoverer := discovery.Discoverer{Git: runner, Config: cfg}
	w, err := discoverer.Resolve(ctx, repository)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertWorkspace(ctx, w); err != nil {
		t.Fatal(err)
	}
	resolved, err := pool.ResolveBranches(ctx, runner, w, nil)
	if err != nil {
		t.Fatal(err)
	}
	id, err := domain.NewID()
	if err != nil {
		t.Fatal(err)
	}
	slotRoot, err := m.slotRoot(string(w.ID), id, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.createSlotRoot(slotRoot); err != nil {
		t.Fatal(err)
	}
	repos, err := m.slotRepos(slotRoot, w, resolved, 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	session := state.Session{ID: id, WorkspaceID: string(w.ID), SlotID: id, State: "STARTING", AgentKind: "codex", TokenHash: state.HashToken("token")}
	if _, err := store.CreateSlotSession(ctx, state.Slot{ID: id, WorkspaceID: string(w.ID), Generation: 1, Path: slotRoot, State: "PREPARING"}, repos, session, "PREPARE"); err != nil {
		t.Fatal(err)
	}
	oldRoot := root + "-old"
	runner.SetBeforeRunAtHook(func(args []string) {
		if len(args) < 3 || args[0] != "--git-dir" || !strings.Contains(strings.Join(args, " "), " worktree add ") {
			return
		}
		if err := os.Rename(root, oldRoot); err != nil {
			t.Fatalf("replace configured root: %v", err)
		}
		if err := os.Symlink(outside, root); err != nil {
			t.Fatalf("install replacement root: %v", err)
		}
	})
	if err := m.prepareSlot(ctx, id, w, resolved, repos); err == nil {
		t.Fatal("uncertain descriptor-bound Git add unexpectedly succeeded")
	}
	slot, err := store.Slot(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if slot.State != "QUARANTINED" {
		t.Fatalf("uncertain add state=%s want QUARANTINED", slot.State)
	}
	oldTarget := filepath.Join(oldRoot, "workspaces", string(w.ID), "slots", id, "root")
	if info, err := os.Stat(oldTarget); err != nil || !info.IsDir() {
		t.Fatalf("quarantined worktree was not preserved: info=%v err=%v", info, err)
	}
	if _, err := os.Lstat(filepath.Join(outside, "workspaces")); !os.IsNotExist(err) {
		t.Fatalf("uncertain add wrote outside root: %v", err)
	}
}
