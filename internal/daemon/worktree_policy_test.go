package daemon

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

// 隔離 slot が残る workspace でも、成功する session を待たずに standby が補充されることを確かめる。
// 上限に達したら止まるので、準備が壊れ続けても隔離 worktree は無制限には増えない。
func TestStandbyReplenishmentContinuesAfterQuarantineUpToLimit(t *testing.T) {
	t.Parallel()
	requireDaemonIntegration(t)
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	initGitRepo(t, repo)
	cfg := config.Defaults()
	cfg.Storage.WorktreeRoot = filepath.Join(root, "worktrees")
	cfg.Worktree.Undefined = "hot"
	cfg.Pool.WarmPerWorkspace = 1
	store, err := state.Open(filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	m := testManager(t, cfg, store)
	m.git.SetTimeout(10 * time.Second)
	defer m.Close()
	ctx := context.Background()
	discoverer := discovery.Discoverer{Git: m.git, Config: cfg}
	w, err := discoverer.Resolve(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	w = registerTestWorkspace(t, store, w)

	// 補充された slot をリトライ切れと同じ形で隔離する。slot は QUARANTINED、job は FAILED で残る。
	quarantineStandby := func() {
		t.Helper()
		jobs, err := store.RecoverJobs(ctx, false)
		if err != nil || len(jobs) != 1 || jobs[0].Kind != "PREPARE" {
			t.Fatalf("standby jobs=%+v err=%v", jobs, err)
		}
		if err := store.SetSlotState(ctx, jobs[0].SlotID, []string{"PREPARING"}, "QUARANTINED", "JOB_RETRY_EXHAUSTED"); err != nil {
			t.Fatal(err)
		}
		claimed, err := store.ClaimJob(ctx, jobs[0].ID, "test")
		if err != nil {
			t.Fatal(err)
		}
		if err := store.FinishJob(ctx, claimed.ID, "test", errors.New("prepare failed")); err != nil {
			t.Fatal(err)
		}
	}
	for attempt := range standbyQuarantineLimit {
		if err := m.ensureStandby(ctx, w); err != nil {
			t.Fatalf("replenishment after %d quarantined slots: %v", attempt, err)
		}
		if count := store.StandbyCount(ctx, string(w.ID)); count != 1 {
			t.Fatalf("standby count after %d quarantined slots=%d, want the new slot", attempt, count)
		}
		quarantineStandby()
		if count := store.StandbyCount(ctx, string(w.ID)); count != 0 {
			t.Fatalf("quarantined slot still occupies a standby place: %d", count)
		}
	}
	// 上限に達したことは一度だけ Warn で伝える。10 分ごとの reconcile で同じ警告を積まない。
	var logs bytes.Buffer
	m.log = slog.New(slog.NewTextHandler(&logs, nil))
	if err := m.ensureStandby(ctx, w); err != nil {
		t.Fatal(err)
	}
	jobs, err := store.RecoverJobs(ctx, false)
	if err != nil || len(jobs) != 0 {
		t.Fatalf("replenishment continued past the quarantine limit: jobs=%+v err=%v", jobs, err)
	}
	if got := strings.Count(logs.String(), "standby replenishment stopped until quarantined slots are removed"); got != 1 {
		t.Fatalf("quarantine limit warnings after the first stop=%d, want one: %s", got, logs.String())
	}
	if err := m.ensureStandby(ctx, w); err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(logs.String(), "standby replenishment stopped until quarantined slots are removed"); got != 1 {
		t.Fatalf("quarantine limit warnings after the second stop=%d, want one: %s", got, logs.String())
	}
}
