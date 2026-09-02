package daemon

import (
	"context"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/HappyOnigiri/WX/internal/archive"
	"github.com/HappyOnigiri/WX/internal/config"
	"github.com/HappyOnigiri/WX/internal/discovery"
	"github.com/HappyOnigiri/WX/internal/domain"
	"github.com/HappyOnigiri/WX/internal/gitx"
	"github.com/HappyOnigiri/WX/internal/pool"
	"github.com/HappyOnigiri/WX/internal/state"
	"github.com/HappyOnigiri/WX/internal/workspace"
)

func TestCrashRecoveryConvergesAfterWorktreeAndRefsExist(t *testing.T) {
	root := t.TempDir()
	repoPath := filepath.Join(root, "repo")
	initGitRepo(t, repoPath)
	cfg := config.Defaults()
	cfg.Storage.WorktreeRoot = filepath.Join(root, "worktrees")
	cfg.Pool.WarmPerWorkspace = 0
	runner := &gitx.Runner{Timeout: 10 * time.Second}
	discoverer := discovery.Discoverer{Git: runner, Config: cfg}
	ctx := context.Background()
	w, err := discoverer.Resolve(ctx, repoPath)
	if err != nil {
		t.Fatal(err)
	}
	store, err := state.Open(filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
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
	slotRoot := filepath.Join(cfg.Storage.WorktreeRoot, "workspaces", string(w.ID), "slots", id, "root")
	if err := os.MkdirAll(slotRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	m := &Manager{cfg: cfg, store: store, git: runner, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	repos, err := m.slotRepos(slotRoot, w, resolved, 1)
	if err != nil {
		t.Fatal(err)
	}
	session := state.Session{ID: id, WorkspaceID: string(w.ID), SlotID: id, State: "STARTING", AgentKind: "codex", TokenHash: state.HashToken("token")}
	prepareJob, err := store.CreateSlotSession(ctx, state.Slot{ID: id, WorkspaceID: string(w.ID), Generation: 1, Path: slotRoot, State: "PREPARING"}, repos, session, "PREPARE")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ClaimJob(ctx, prepareJob.ID, "crashed-daemon"); err != nil {
		t.Fatal(err)
	}
	preparer := workspace.Preparer{Git: runner, Config: cfg}
	if err := preparer.Prepare(ctx, resolved[0].Repository, repos[0].WorktreePath, resolved[0].OID, id); err != nil {
		t.Fatal(err)
	}
	jobs, err := store.RecoverJobs(ctx, true)
	if err != nil || len(jobs) != 1 {
		t.Fatalf("recover prepare jobs=%+v err=%v", jobs, err)
	}
	claimedPrepare, err := store.ClaimJob(ctx, jobs[0].ID, "restarted-daemon")
	if err != nil {
		t.Fatal(err)
	}
	if err := m.runRecoveredJob(ctx, claimedPrepare); err != nil {
		t.Fatalf("recover after worktree creation: %v", err)
	}
	if err := store.FinishJob(ctx, claimedPrepare.ID, "restarted-daemon", nil); err != nil {
		t.Fatal(err)
	}
	slot, err := store.Slot(ctx, id)
	if err != nil || slot.State != "LEASED" {
		t.Fatalf("prepared slot=%+v err=%v", slot, err)
	}

	snapshotJob, changed, err := store.Release(ctx, id, string(w.ID), id)
	if err != nil || !changed {
		t.Fatalf("release changed=%v err=%v", changed, err)
	}
	if _, err := store.ClaimJob(ctx, snapshotJob.ID, "crashed-daemon"); err != nil {
		t.Fatal(err)
	}
	released, err := store.SessionByID(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	releasedAt, err := time.Parse(time.RFC3339Nano, released.ReleasedAt)
	if err != nil {
		t.Fatal(err)
	}
	archiveManager := archive.Manager{Git: runner, Preparer: &preparer}
	first, err := archiveManager.Snapshot(ctx, resolved[0].Repository, repos[0].WorktreePath, id, releasedAt.Add(cfg.Retention.RecoverySnapshot.Duration))
	if err != nil {
		t.Fatal(err)
	}
	jobs, err = store.RecoverJobs(ctx, true)
	if err != nil || len(jobs) != 1 {
		t.Fatalf("recover snapshot jobs=%+v err=%v", jobs, err)
	}
	for _, job := range jobs {
		if err := m.runRecoveredJob(ctx, job); err != nil {
			t.Fatalf("replay %s: %v", job.Kind, err)
		}
	}
	snapshots, err := store.Snapshots(ctx, id)
	if err != nil || len(snapshots) != 1 || snapshots[0].ID != first.ID || snapshots[0].WorktreeOID != first.WorktreeOID {
		t.Fatalf("recovered snapshots=%+v first=%+v err=%v", snapshots, first, err)
	}
}

func TestLeaseArchiveAndRestorePreservesGitState(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	initGitRepo(t, repo)
	if err := os.WriteFile(filepath.Join(repo, ".gitignore"), []byte("shared/\nlocal.cfg\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".worktreeinclude"), []byte("local.cfg\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".worktreelink"), []byte("shared\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(repo, "shared"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "shared", "data"), []byte("shared\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "local.cfg"), []byte("local\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitRun(t, repo, "add", ".gitignore", ".worktreeinclude", ".worktreelink")
	gitRun(t, repo, "commit", "-m", "worktree metadata")
	if err := os.WriteFile(filepath.Join(repo, "tracked.txt"), []byte("dirty main\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := config.Defaults()
	cfg.Storage.WorktreeRoot = filepath.Join(root, "worktrees")
	cfg.Pool.WarmPerWorkspace = 0
	cfg.Retention.EndedWorktree.Duration = 0
	cfg.Readiness.Timeout.Duration = 10 * time.Second
	store, err := state.Open(filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	m := New(cfg, store, slog.New(slog.NewTextHandler(io.Discard, nil)))
	defer m.Close()
	ctx := context.Background()
	lease, err := m.ResolveAndLease(ctx, repo, nil, "codex", os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	waitCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := m.WaitReady(waitCtx, lease.SessionID, lease.Token); err != nil {
		t.Fatal(err)
	}
	if got := gitOutput(t, lease.Path, "rev-parse", "--abbrev-ref", "HEAD"); got != "HEAD" {
		t.Fatalf("worktree branch=%q", got)
	}
	data, err := os.ReadFile(filepath.Join(lease.Path, "tracked.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "base\n" {
		t.Fatalf("main dirty content leaked: %q", data)
	}
	if info, err := os.Lstat(filepath.Join(lease.Path, "shared")); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("shared is not a symlink: %v %v", info, err)
	}
	if data, err := os.ReadFile(filepath.Join(lease.Path, "local.cfg")); err != nil || string(data) != "local\n" {
		t.Fatalf("include copy=%q err=%v", data, err)
	}
	if err := m.BindAgentSession(ctx, lease.SessionID, lease.Token, "agent-session-1"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(lease.Path, "tracked.txt"), []byte("staged\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitRun(t, lease.Path, "add", "tracked.txt")
	if err := os.WriteFile(filepath.Join(lease.Path, "tracked.txt"), []byte("working\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(lease.Path, "untracked.txt"), []byte("untracked\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := m.Release(ctx, lease.SessionID, lease.Token, "test"); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, 10*time.Second, func() bool { snaps, _ := store.Snapshots(ctx, lease.SessionID); return len(snaps) == 1 })
	native, err := m.AllocateResumeSlot(ctx, "codex", os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(native.Path, string(filepath.Separator)+"unbound"+string(filepath.Separator)) {
		t.Fatalf("native resume path is not unbound: %s", native.Path)
	}
	if entries, err := os.ReadDir(native.Path); err != nil || len(entries) != 0 {
		t.Fatalf("unbound root must start empty: entries=%v err=%v", entries, err)
	}
	if err := m.BindAndRestoreResume(ctx, native.SessionID, native.Token, "agent-session-1"); err != nil {
		t.Fatal(err)
	}
	if err := m.WaitReady(waitCtx, native.SessionID, native.Token); err != nil {
		t.Fatal(err)
	}
	if status := gitOutput(t, native.Path, "status", "--porcelain"); !strings.Contains(status, "MM tracked.txt") || !strings.Contains(status, "?? untracked.txt") {
		t.Fatalf("native restored status:\n%s", status)
	}
	resumed, err := m.Resume(ctx, lease.SessionID, "codex", os.Getpid(), false)
	if err != nil {
		t.Fatal(err)
	}
	if resumed.Path == lease.Path {
		t.Fatal("resume reused old physical path")
	}
	if err := m.WaitReady(waitCtx, resumed.SessionID, resumed.Token); err != nil {
		t.Fatal(err)
	}
	status := gitOutput(t, resumed.Path, "status", "--porcelain")
	if !strings.Contains(status, "MM tracked.txt") || !strings.Contains(status, "?? untracked.txt") {
		t.Fatalf("restored status:\n%s", status)
	}
	data, err = os.ReadFile(filepath.Join(resumed.Path, "tracked.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "working\n" {
		t.Fatalf("working content=%q", data)
	}
	data, err = os.ReadFile(filepath.Join(repo, "tracked.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "dirty main\n" {
		t.Fatalf("source main changed: %q", data)
	}
}

func TestHandlerPublicLifecycleSurface(t *testing.T) {
	root := t.TempDir()
	repository := filepath.Join(root, "repo")
	initGitRepo(t, repository)
	cfg := config.Defaults()
	cfg.Storage.WorktreeRoot = filepath.Join(root, "worktrees")
	cfg.Pool.WarmPerWorkspace = 0
	cfg.Discovery.ReconcileInterval.Duration = time.Hour
	store, err := state.Open(filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	manager := New(cfg, store, slog.New(slog.NewTextHandler(io.Discard, nil)))
	defer manager.Close()
	handler := Handler{Manager: manager}
	ctx := context.Background()
	result, err := handler.Handle(ctx, "ResolveAndLease", JSON(map[string]any{"cwd": repository, "agent": "codex", "client_pid": os.Getpid()}))
	if err != nil {
		t.Fatal(err)
	}
	lease := result.(Lease)
	if _, err := handler.Handle(ctx, "WaitReady", JSON(map[string]any{"session_id": lease.SessionID, "token": lease.Token, "timeout_ms": 10000})); err != nil {
		t.Fatal(err)
	}
	for method, params := range map[string]any{
		"BindAgentSession": map[string]any{"session_id": lease.SessionID, "token": lease.Token, "agent_session_id": "agent-session"},
		"Heartbeat":        map[string]any{"session_id": lease.SessionID, "token": lease.Token},
		"ResumeStatus":     map[string]any{"wx_session_id": lease.SessionID},
		"GC":               map[string]any{"dry_run": true},
		"Sessions":         map[string]any{"all": true},
	} {
		if _, err := handler.Handle(ctx, method, JSON(params)); err != nil {
			t.Fatalf("%s: %v", method, err)
		}
	}
	for _, method := range []string{"Status", "Doctor"} {
		if _, err := handler.Handle(ctx, method, nil); err != nil {
			t.Fatalf("%s: %v", method, err)
		}
	}
	if _, err := handler.Handle(ctx, "Release", JSON(map[string]any{"session_id": lease.SessionID, "token": lease.Token, "reason": "test"})); err != nil {
		t.Fatal(err)
	}
	if result, err := handler.Handle(ctx, "AllocateResumeSlot", JSON(map[string]any{"agent": "codex", "client_pid": os.Getpid()})); err != nil || result == nil {
		t.Fatalf("allocate resume result=%v err=%v", result, err)
	}
}

func TestGCExpiresSnapshotRefsOnlyAfterArchivingWorktree(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	initGitRepo(t, repo)
	cfg := config.Defaults()
	cfg.Storage.WorktreeRoot = filepath.Join(root, "worktrees")
	cfg.Pool.WarmPerWorkspace = 0
	cfg.Retention.EndedWorktree.Duration = 0
	cfg.Retention.RecoverySnapshot.Duration = time.Millisecond
	cfg.Discovery.ReconcileInterval.Duration = time.Hour
	store, err := state.Open(filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	m := New(cfg, store, slog.New(slog.NewTextHandler(io.Discard, nil)))
	defer m.Close()
	ctx := context.Background()
	lease, err := m.ResolveAndLease(ctx, repo, nil, "codex", os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	waitCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := m.WaitReady(waitCtx, lease.SessionID, lease.Token); err != nil {
		t.Fatal(err)
	}
	if err := m.Release(ctx, lease.SessionID, lease.Token, "test"); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, 10*time.Second, func() bool {
		session, _ := store.SessionByID(ctx, lease.SessionID)
		return session.State == "ARCHIVED"
	})
	snapshots, err := store.Snapshots(ctx, lease.SessionID)
	if err != nil || len(snapshots) != 1 {
		t.Fatalf("snapshots=%+v err=%v", snapshots, err)
	}
	time.Sleep(5 * time.Millisecond)
	if _, err := m.GC(ctx, false); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, 10*time.Second, func() bool {
		slot, _ := store.Slot(ctx, lease.SessionID)
		_, pathErr := os.Stat(lease.Path)
		return slot.State == "ARCHIVED" && os.IsNotExist(pathErr)
	})
	if _, err := m.GC(ctx, false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(lease.Path); !os.IsNotExist(err) {
		t.Fatalf("ended worktree still exists: %v", err)
	}
	session, err := store.SessionByID(ctx, lease.SessionID)
	if err != nil || session.State != "EXPIRED" {
		t.Fatalf("expired session=%+v err=%v", session, err)
	}
	if remaining, err := store.Snapshots(ctx, lease.SessionID); err != nil || len(remaining) != 0 {
		t.Fatalf("remaining snapshots=%+v err=%v", remaining, err)
	}
	for _, ref := range []string{snapshots[0].HeadRef, snapshots[0].WorktreeRef} {
		cmd := exec.Command("git", "show-ref", "--verify", ref)
		cmd.Dir = repo
		if err := cmd.Run(); err == nil {
			t.Fatalf("expired recovery ref still exists: %s", ref)
		}
	}
}

func TestRemovalJobReplaysAfterPhysicalDeletionBeforeStateCommit(t *testing.T) {
	root := t.TempDir()
	repoPath := filepath.Join(root, "repo")
	initGitRepo(t, repoPath)
	cfg := config.Defaults()
	cfg.Storage.WorktreeRoot = filepath.Join(root, "worktrees")
	cfg.Pool.WarmPerWorkspace = 0
	runner := &gitx.Runner{Timeout: 10 * time.Second}
	ctx := context.Background()
	discoverer := discovery.Discoverer{Git: runner, Config: cfg}
	w, err := discoverer.Resolve(ctx, repoPath)
	if err != nil {
		t.Fatal(err)
	}
	store, err := state.Open(filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.UpsertWorkspace(ctx, w); err != nil {
		t.Fatal(err)
	}
	resolved, err := pool.ResolveBranches(ctx, runner, w, nil)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := domain.NewID()
	slotRoot := filepath.Join(cfg.Storage.WorktreeRoot, "workspaces", string(w.ID), "slots", id, "root")
	repos := []state.SlotRepository{{RepositoryID: string(w.Repositories[0].ID), WorktreePath: slotRoot, State: "READY", RequestedRef: resolved[0].RequestedRef, BaseOID: resolved[0].OID, Fingerprint: "test"}}
	session := state.Session{ID: id, WorkspaceID: string(w.ID), SlotID: id, State: "STARTING", AgentKind: "codex", TokenHash: state.HashToken("token")}
	job, err := store.CreateSlotSession(ctx, state.Slot{ID: id, WorkspaceID: string(w.ID), Generation: 1, Path: slotRoot, State: "PREPARING"}, repos, session, "PREPARE")
	if err != nil {
		t.Fatal(err)
	}
	preparer := workspace.Preparer{Git: runner, Config: cfg}
	if err := preparer.Prepare(ctx, w.Repositories[0], slotRoot, resolved[0].OID, id); err != nil {
		t.Fatal(err)
	}
	claimed, err := store.ClaimJob(ctx, job.ID, "setup")
	if err != nil {
		t.Fatal(err)
	}
	m := &Manager{cfg: cfg, store: store, git: runner, log: slog.New(slog.NewTextHandler(io.Discard, nil)), roots: map[string]bool{cfg.Storage.WorktreeRoot: true}}
	if err := m.prepareSlot(ctx, id, w, resolved, repos); err != nil {
		t.Fatal(err)
	}
	if err := store.FinishJob(ctx, claimed.ID, "setup", nil); err != nil {
		t.Fatal(err)
	}
	snapshotJob, changed, err := store.Release(ctx, id, string(w.ID), id)
	if err != nil || !changed {
		t.Fatalf("release changed=%v err=%v", changed, err)
	}
	claimedSnapshot, err := store.ClaimJob(ctx, snapshotJob.ID, "setup")
	if err != nil {
		t.Fatal(err)
	}
	archiveManager := archive.Manager{Git: runner, Preparer: &preparer}
	expires := time.Now().Add(time.Hour)
	snapshot, err := archiveManager.Snapshot(ctx, w.Repositories[0], slotRoot, id, expires)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SaveSnapshot(ctx, snapshot); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkArchived(ctx, id, id, expires.Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	if err := store.FinishJob(ctx, claimedSnapshot.ID, "setup", nil); err != nil {
		t.Fatal(err)
	}
	removeJob, changed, err := store.ScheduleRemoval(ctx, id, id)
	if err != nil || !changed {
		t.Fatalf("schedule removal changed=%v err=%v", changed, err)
	}
	if _, err := store.ClaimJob(ctx, removeJob.ID, "crashed-daemon"); err != nil {
		t.Fatal(err)
	}
	if err := m.removeSlotWorktrees(ctx, archiveManager, cfg.Storage.WorktreeRoot, id, id, slotRoot); err != nil {
		t.Fatal(err)
	}
	if slot, _ := store.Slot(ctx, id); slot.State != "REMOVING" {
		t.Fatalf("state was committed before simulated crash: %s", slot.State)
	}
	recovered, err := store.RecoverJobs(ctx, true)
	if err != nil || len(recovered) != 1 {
		t.Fatalf("recovered removal jobs=%+v err=%v", recovered, err)
	}
	if err := m.runRecoveredJob(ctx, recovered[0]); err != nil {
		t.Fatalf("replay removal: %v", err)
	}
	if slot, _ := store.Slot(ctx, id); slot.State != "ARCHIVED" {
		t.Fatalf("replayed removal state=%s", slot.State)
	}
}

func TestWarmPoolMaintainsCapacityAndNeverDoubleLeases(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	initGitRepo(t, repo)
	cfg := config.Defaults()
	cfg.Storage.WorktreeRoot = filepath.Join(root, "worktrees")
	cfg.Pool.WarmPerWorkspace = 2
	cfg.Pool.PreparationConcurrency = 3
	cfg.Retention.HotStandby.Duration = time.Hour
	cfg.Discovery.ReconcileInterval.Duration = time.Hour
	store, err := state.Open(filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	m := New(cfg, store, slog.New(slog.NewTextHandler(io.Discard, nil)))
	defer m.Close()
	ctx := context.Background()
	first, err := m.ResolveAndLease(ctx, repo, nil, "codex", os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	waitCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := m.WaitReady(waitCtx, first.SessionID, first.Token); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, 10*time.Second, func() bool {
		status, _ := store.Status(ctx)
		return status.Ready == 2
	})

	leases := make(chan Lease, 2)
	errs := make(chan error, 2)
	for range 2 {
		go func() {
			lease, err := m.ResolveAndLease(ctx, repo, nil, "codex", os.Getpid())
			leases <- lease
			errs <- err
		}()
	}
	a, b := <-leases, <-leases
	if errA, errB := <-errs, <-errs; errA != nil || errB != nil {
		t.Fatalf("concurrent leases errors: %v, %v", errA, errB)
	}
	if a.SessionID == b.SessionID || a.Path == b.Path || a.SessionID == first.SessionID || b.SessionID == first.SessionID {
		t.Fatalf("slots were reused: first=%+v a=%+v b=%+v", first, a, b)
	}
	if !a.Ready || !b.Ready {
		t.Fatalf("warm leases were not ready: a=%+v b=%+v", a, b)
	}
	waitUntil(t, 10*time.Second, func() bool {
		status, _ := store.Status(ctx)
		return status.Ready == 2
	})
}

func TestNativeResumeWaitsForInFlightSnapshot(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	initGitRepo(t, repo)
	cfg := config.Defaults()
	cfg.Storage.WorktreeRoot = filepath.Join(root, "worktrees")
	cfg.Pool.WarmPerWorkspace = 0
	cfg.Pool.PreparationConcurrency = 2
	cfg.Discovery.ReconcileInterval.Duration = time.Hour
	store, err := state.Open(filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	m := New(cfg, store, slog.New(slog.NewTextHandler(io.Discard, nil)))
	defer m.Close()
	ctx := context.Background()
	lease, err := m.ResolveAndLease(ctx, repo, nil, "codex", os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	waitCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if err := m.WaitReady(waitCtx, lease.SessionID, lease.Token); err != nil {
		t.Fatal(err)
	}
	if err := m.BindAgentSession(ctx, lease.SessionID, lease.Token, "in-flight-agent"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(lease.Path, "pending.txt"), []byte("recover me\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := m.Release(ctx, lease.SessionID, lease.Token, "test"); err != nil {
		t.Fatal(err)
	}
	native, err := m.AllocateResumeSlot(ctx, "codex", os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	if err := m.BindAndRestoreResume(ctx, native.SessionID, native.Token, "in-flight-agent"); err != nil {
		t.Fatal(err)
	}
	if err := m.WaitReady(waitCtx, native.SessionID, native.Token); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(native.Path, "pending.txt"))
	if err != nil || string(data) != "recover me\n" {
		t.Fatalf("restored pending file=%q err=%v", data, err)
	}
}

func TestExpiredExplicitResumeRequiresOptInAndUsesCurrentBase(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	initGitRepo(t, repo)
	cfg := config.Defaults()
	cfg.Storage.WorktreeRoot = filepath.Join(root, "worktrees")
	cfg.Pool.WarmPerWorkspace = 0
	cfg.Retention.EndedWorktree.Duration = 0
	cfg.Retention.RecoverySnapshot.Duration = time.Millisecond
	cfg.Discovery.ReconcileInterval.Duration = time.Hour
	store, err := state.Open(filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	m := New(cfg, store, slog.New(slog.NewTextHandler(io.Discard, nil)))
	defer m.Close()
	ctx := context.Background()
	lease, err := m.ResolveAndLease(ctx, repo, nil, "codex", os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	waitCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := m.WaitReady(waitCtx, lease.SessionID, lease.Token); err != nil {
		t.Fatal(err)
	}
	if err := m.BindAgentSession(ctx, lease.SessionID, lease.Token, "expired-agent-session"); err != nil {
		t.Fatal(err)
	}
	refusedFresh, err := m.ResolveAndLease(ctx, repo, nil, "codex", os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	if err := m.ValidateFreshResume(ctx, refusedFresh.SessionID, refusedFresh.Token, "expired-agent-session"); err == nil {
		t.Fatal("--fresh was accepted while the mapped session was active")
	}
	if err := os.WriteFile(filepath.Join(lease.Path, "uncommitted.txt"), []byte("discarded\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := m.Release(ctx, lease.SessionID, lease.Token, "test"); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, 10*time.Second, func() bool {
		session, _ := store.SessionByID(ctx, lease.SessionID)
		return session.State == "ARCHIVED"
	})
	time.Sleep(5 * time.Millisecond)
	if _, err := m.GC(ctx, false); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, 10*time.Second, func() bool {
		slot, _ := store.Slot(ctx, lease.SessionID)
		return slot.State == "ARCHIVED"
	})
	if _, err := m.GC(ctx, false); err != nil {
		t.Fatal(err)
	}
	status, err := m.ResumeStatus(ctx, lease.SessionID)
	if err != nil || status["expired"] != true {
		t.Fatalf("resume status=%v err=%v", status, err)
	}
	if _, err := m.Resume(ctx, lease.SessionID, "codex", os.Getpid(), false); err == nil {
		t.Fatal("expired resume proceeded without confirmation")
	}
	fresh, err := m.Resume(ctx, lease.SessionID, "codex", os.Getpid(), true)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.WaitReady(waitCtx, fresh.SessionID, fresh.Token); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(fresh.Path, "uncommitted.txt")); !os.IsNotExist(err) {
		t.Fatalf("expired local state leaked into fresh workspace: %v", err)
	}
	if got := gitOutput(t, fresh.Path, "rev-parse", "HEAD"); got != gitOutput(t, repo, "rev-parse", "refs/heads/main") {
		t.Fatalf("fresh base=%s main=%s", got, gitOutput(t, repo, "rev-parse", "refs/heads/main"))
	}
	nativeFresh, err := m.AllocateResumeSlot(ctx, "codex", os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	if err := m.ValidateFreshResume(ctx, nativeFresh.SessionID, nativeFresh.Token, "expired-agent-session"); err != nil {
		t.Fatalf("--fresh rejected expired mapping: %v", err)
	}
	nativeWait, nativeCancel := context.WithTimeout(ctx, 10*time.Second)
	defer nativeCancel()
	if err := m.WaitReady(nativeWait, nativeFresh.SessionID, nativeFresh.Token); err != nil {
		t.Fatalf("native --fresh workspace did not become ready: %v", err)
	}
	if got := gitOutput(t, nativeFresh.Path, "rev-parse", "HEAD"); got != gitOutput(t, repo, "rev-parse", "refs/heads/main") {
		t.Fatalf("native fresh base=%s main=%s", got, gitOutput(t, repo, "rev-parse", "refs/heads/main"))
	}
}

func TestMultiRepositoryBundleAndRootRules(t *testing.T) {
	root := t.TempDir()
	initGitRepo(t, filepath.Join(root, "service"))
	initGitRepo(t, filepath.Join(root, "web"))
	if err := os.WriteFile(filepath.Join(root, "AGENTS.md"), []byte("root rules\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	shared := filepath.Join(root, "audit")
	if err := os.Mkdir(shared, 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := config.Defaults()
	cfg.Storage.WorktreeRoot = filepath.Join(root, "worktrees")
	cfg.Pool.WarmPerWorkspace = 1
	cfg.Discovery.ReconcileInterval.Duration = time.Hour
	cfg.Workspaces = map[string]config.Workspace{root: {Link: []string{"audit"}}}
	store, err := state.Open(filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	m := New(cfg, store, slog.New(slog.NewTextHandler(io.Discard, nil)))
	defer m.Close()
	lease, err := m.ResolveAndLease(context.Background(), root, nil, "codex", 1)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := m.WaitReady(ctx, lease.SessionID, lease.Token); err != nil {
		t.Fatal(err)
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
	discoverer := discovery.Discoverer{Git: m.git, Config: cfg}
	updated, err := discoverer.Resolve(context.Background(), root)
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
	coldLease, err := m.ResolveAndLease(context.Background(), root, nil, "codex", os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	if coldLease.SessionID != coldSlot.ID || coldLease.Ready {
		t.Fatalf("cold bundle lease=%+v slot=%+v", coldLease, coldSlot)
	}
	if err := m.WaitReady(ctx, coldLease.SessionID, coldLease.Token); err != nil {
		t.Fatal(err)
	}
	for _, repository := range updated.Repositories {
		if _, err := os.Stat(filepath.Join(coldLease.Path, repository.RelativePath, ".git")); err != nil {
			t.Fatalf("repository %s was not rematerialized: %v", repository.RelativePath, err)
		}
	}
}

func initGitRepo(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o700); err != nil {
		t.Fatal(err)
	}
	gitRun(t, path, "init", "-b", "main")
	gitRun(t, path, "config", "user.name", "test")
	gitRun(t, path, "config", "user.email", "test@example.com")
	if err := os.WriteFile(filepath.Join(path, "tracked.txt"), []byte("base\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitRun(t, path, "add", ".")
	gitRun(t, path, "commit", "-m", "initial")
}

func gitRun(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func gitOutput(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func waitUntil(t *testing.T, timeout time.Duration, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("condition was not met before timeout")
}
