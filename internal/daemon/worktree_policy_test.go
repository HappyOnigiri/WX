package daemon

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/HappyOnigiri/WX/internal/config"
	"github.com/HappyOnigiri/WX/internal/discovery"
	"github.com/HappyOnigiri/WX/internal/state"
)

func TestLeasePolicyAndStandbyPermissions(t *testing.T) {
	requireDaemonIntegration(t)
	for _, mode := range []string{"ask", "off", "cold", "hot"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			repo := filepath.Join(root, "repo")
			initGitRepo(t, repo)
			cfg := config.Defaults()
			cfg.Storage.WorktreeRoot = filepath.Join(root, "worktrees")
			cfg.Worktree.Undefined = mode
			store, err := state.Open(filepath.Join(root, "state.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = store.Close() }()
			m := testManager(t, cfg, store)
			defer m.Close()
			ctx := context.Background()
			discoverer := discovery.Discoverer{Git: m.git, Config: cfg}
			w, err := discoverer.Resolve(ctx, repo)
			if err != nil {
				t.Fatal(err)
			}
			w = registerTestWorkspace(t, store, w)
			if err := m.ensureStandby(ctx, w); err != nil {
				t.Fatal(err)
			}
			count := store.StandbyCount(ctx, string(w.ID))
			if (mode == "hot" && count != 1) || (mode != "hot" && count != 0) {
				t.Fatalf("standby=%d", count)
			}
			lease, err := m.leaseWithPolicy(ctx, repo, nil, "codex", os.Getpid(), false)
			if mode == "off" || mode == "ask" {
				if err == nil {
					t.Fatal("unapproved creation accepted")
				}
				lease, err = m.leaseWithPolicy(ctx, repo, nil, "codex", os.Getpid(), true)
			}
			if err != nil || lease.SessionID == "" {
				t.Fatalf("lease=%+v err=%v", lease, err)
			}
			if m.Config().Worktree.Undefined != mode {
				t.Fatal("temporary permission persisted")
			}
		})
	}
}

func TestColdPolicyDoesNotLeaseExistingReadySlot(t *testing.T) {
	requireDaemonIntegration(t)
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	initGitRepo(t, repo)
	cfg := config.Defaults()
	cfg.Storage.WorktreeRoot = filepath.Join(root, "worktrees")
	cfg.Worktree.Undefined = "hot"
	store, err := state.Open(filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	m := testManager(t, cfg, store)
	defer m.Close()
	ctx := context.Background()
	discoverer := discovery.Discoverer{Git: m.git, Config: cfg}
	w, err := discoverer.Resolve(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	w = registerTestWorkspace(t, store, w)
	if err := m.ensureStandby(ctx, w); err != nil {
		t.Fatal(err)
	}
	jobs, err := store.RecoverJobs(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, job := range jobs {
		if err := m.runRecoveredJob(ctx, job); err != nil {
			t.Fatal(err)
		}
	}
	ready, ok, err := store.ReadySlot(ctx, string(w.ID))
	if err != nil || !ok {
		t.Fatalf("ready=%v err=%v", ok, err)
	}
	m.mu.Lock()
	m.cfg.Workspaces[repo] = config.Workspace{Worktree: "cold"}
	m.mu.Unlock()
	lease, err := m.leaseWithPolicy(ctx, repo, nil, "codex", os.Getpid(), false)
	if err != nil || lease.SessionID == ready.ID {
		t.Fatalf("lease=%+v ready=%s err=%v", lease, ready.ID, err)
	}
	after, ok, err := store.ReadySlot(ctx, string(w.ID))
	if err != nil || !ok || after.ID != ready.ID {
		t.Fatalf("standby changed: %+v err=%v", after, err)
	}
}
