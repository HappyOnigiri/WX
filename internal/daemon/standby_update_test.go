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
	"github.com/HappyOnigiri/WX/internal/gitx"
	"github.com/HappyOnigiri/WX/internal/state"
)

func TestPlacementsForSeparatesWorkspaceRoot(t *testing.T) {
	placements := []state.Placement{{RepositoryID: "repository", RelativePath: "repo"}, {RelativePath: "root"}}
	if got := placementsFor(placements, ""); len(got) != 1 || got[0].RelativePath != "root" {
		t.Fatalf("root placements=%+v", got)
	}
}

func TestStandbyUpdateReusesSlotAndSkipsHooksAndPrepare(t *testing.T) {
	requireDaemonIntegration(t)
	root := t.TempDir()
	repository := filepath.Join(root, "repo")
	initGitRepo(t, repository)
	if err := os.WriteFile(filepath.Join(repository, ".gitignore"), []byte("local.cfg\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repository, ".worktreeinclude"), []byte("local.cfg\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repository, "local.cfg"), []byte("old\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitRun(t, repository, "add", ".gitignore", ".worktreeinclude")
	gitRun(t, repository, "commit", "-m", "add include rules")
	hookLog := filepath.Join(root, "hook.log")
	hooks := filepath.Join(root, "hooks")
	if err := os.Mkdir(hooks, 0o700); err != nil {
		t.Fatal(err)
	}
	hook := "#!/bin/sh\nprintf 'hook\\n' >> " + hookLog + "\n"
	if err := os.WriteFile(filepath.Join(hooks, "post-checkout"), []byte(hook), 0o700); err != nil {
		t.Fatal(err)
	}
	gitRun(t, repository, "config", "core.hooksPath", hooks)
	prepareLog := filepath.Join(root, "prepare.log")
	store, err := state.Open(filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cfg := config.Defaults()
	cfg.Storage.WorktreeRoot = filepath.Join(root, "worktrees")
	cfg.Worktree.Undefined = "hot"
	cfg.Pool.WarmPerWorkspace = 1
	cfg.Repositories = map[string]config.Repository{repository: {Prepare: config.Prepare{Command: []string{"sh", "-c", "printf 'prepare\\n' >> " + prepareLog}}}}
	m := testManager(t, cfg, store)
	m.git = &gitx.Runner{Timeout: 10 * time.Second}
	defer m.Close()
	ctx := context.Background()
	discoverer := discovery.Discoverer{Git: m.git, Config: cfg}
	w, err := discoverer.Resolve(ctx, repository)
	if err != nil {
		t.Fatal(err)
	}
	w = registerTestWorkspace(t, store, w)
	raw := openManagerCoverageDB(t, filepath.Join(root, "state.db"))
	defer raw.Close()
	if _, err := raw.ExecContext(ctx, `UPDATE repositories SET last_leased_at=?`, state.FormatTime(time.Now())); err != nil {
		t.Fatal(err)
	}
	if err := m.ensureStandby(ctx, w); err != nil {
		t.Fatal(err)
	}
	jobs, err := store.RecoverJobs(ctx, false)
	if err != nil || len(jobs) != 1 {
		t.Fatalf("prepare jobs=%+v err=%v", jobs, err)
	}
	prepareJob, err := store.ClaimJob(ctx, jobs[0].ID, "prepare")
	if err != nil {
		t.Fatal(err)
	}
	if err := m.runRecoveredJob(ctx, prepareJob); err != nil {
		t.Fatal(err)
	}
	if err := store.FinishJob(ctx, prepareJob.ID, "prepare", nil); err != nil {
		t.Fatal(err)
	}
	ready, ok, err := store.ReadySlot(ctx, string(w.ID))
	if err != nil || !ok || !ready.PlacementHistoryComplete {
		t.Fatalf("ready=%+v ok=%t err=%v", ready, ok, err)
	}
	if err := os.WriteFile(filepath.Join(repository, "local.cfg"), []byte("new\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repository, "tracked.txt"), []byte("new head\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitRun(t, repository, "add", "tracked.txt")
	gitRun(t, repository, "commit", "-m", "advance main")
	newOID := strings.TrimSpace(gitOutput(t, repository, "rev-parse", "HEAD"))
	lease, err := m.ResolveAndLease(ctx, repository, nil, "codex", os.Getpid(), leaseAttrs{})
	if err != nil {
		t.Fatal(err)
	}
	if lease.SessionID != ready.ID || lease.Ready {
		t.Fatalf("updated lease=%+v, want same slot behind readiness gate", lease)
	}
	jobs, err = store.RecoverJobs(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	var update state.Job
	for _, job := range jobs {
		if job.Kind == "UPDATE" {
			update = job
		}
	}
	if update.ID == "" {
		t.Fatalf("jobs=%+v, want UPDATE", jobs)
	}
	claimed, err := store.ClaimJob(ctx, update.ID, "update")
	if err != nil {
		t.Fatal(err)
	}
	if err := m.runRecoveredJob(ctx, claimed); err != nil {
		t.Fatal(err)
	}
	if err := store.FinishJob(ctx, claimed.ID, "update", nil); err != nil {
		t.Fatal(err)
	}
	if err := m.WaitReady(ctx, lease.SessionID, lease.Token); err != nil {
		t.Fatal(err)
	}
	repositoryState, err := store.SlotRepository(ctx, ready.ID, string(w.Repositories[0].ID))
	if err != nil || repositoryState.BaseOID != newOID {
		t.Fatalf("repository state=%+v err=%v", repositoryState, err)
	}
	if got, err := os.ReadFile(filepath.Join(repositoryState.WorktreePath, "local.cfg")); err != nil || string(got) != "new\n" {
		t.Fatalf("updated include=%q err=%v", got, err)
	}
	for path, want := range map[string]int{hookLog: 1, prepareLog: 1} {
		data, err := os.ReadFile(path)
		if err != nil || strings.Count(string(data), "\n") != want {
			t.Fatalf("execution log %s=%q err=%v", path, data, err)
		}
	}
}
