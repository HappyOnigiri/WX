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

func TestCreateSlotRootPinsAllocationAcrossRootReplacement(t *testing.T) {
	t.Parallel()
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
	pinnedIdentity, err := descriptorIdentity(pinned)
	if err != nil {
		t.Fatal(err)
	}
	m := &Manager{
		cfg:            func() config.Config { cfg := config.Defaults(); cfg.Storage.WorktreeRoot = root; return cfg }(),
		git:            &gitx.Runner{},
		log:            slog.New(slog.NewTextHandler(io.Discard, nil)),
		roots:          map[string]bool{root: true},
		rootRefs:       map[string]*managedRoot{root: {root: pinned, identity: pinnedIdentity}},
		rootIdentities: map[string]string{root: pinnedIdentity},
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

	relPath, err := slotRelPath("wsp001", "slt001", false)
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, relPath)
	identity, _, err := m.createSlotRoot(target, target)
	if err != nil {
		t.Fatalf("create slot root: %v", err)
	}
	oldTarget := filepath.Join(root+"-old", relPath)
	if info, err := os.Stat(oldTarget); err != nil || !info.IsDir() {
		t.Fatalf("allocation did not remain in pinned root: info=%v err=%v", info, err)
	}
	if _, err := os.Lstat(filepath.Join(outside, "wsp001")); !os.IsNotExist(err) {
		t.Fatalf("allocation escaped into replacement directory: %v", err)
	}
	if got, err := m.ownedDirectoryIdentity(target); err != nil || got != identity {
		t.Fatalf("lease identity got=%q want=%q err=%v", got, identity, err)
	}
}

func TestPrepareQuarantinesUncertainGitAddAfterRootReplacement(t *testing.T) {
	t.Parallel()
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
	w = registerTestWorkspace(t, store, w)
	resolved, err := pool.ResolveBranches(ctx, runner, w, nil)
	if err != nil {
		t.Fatal(err)
	}
	id, err := newSlotID()
	if err != nil {
		t.Fatal(err)
	}
	slot := testSlot(t, m, string(w.ID), id, 1, "PREPARING")
	slotRoot := slot.Path
	repos, err := m.slotRepos(slotRoot, w, resolved, 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	session := state.Session{ID: id, WorkspaceID: string(w.ID), SlotID: id, State: "STARTING", AgentKind: "codex", TokenHash: state.HashToken("token")}
	if _, err := store.CreateSlotSession(ctx, slot, repos, session, "PREPARE"); err != nil {
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
	quarantined, err := store.Slot(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if quarantined.State != "QUARANTINED" {
		t.Fatalf("uncertain add state=%s want QUARANTINED", quarantined.State)
	}
	oldTarget := filepath.Join(oldRoot, slot.RelPath)
	if info, err := os.Stat(oldTarget); err != nil || !info.IsDir() {
		t.Fatalf("quarantined worktree was not preserved: info=%v err=%v", info, err)
	}
	if _, err := os.Lstat(filepath.Join(outside, "workspaces")); !os.IsNotExist(err) {
		t.Fatalf("uncertain add wrote outside root: %v", err)
	}
}
