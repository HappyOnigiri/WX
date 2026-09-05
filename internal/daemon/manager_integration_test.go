package daemon

import (
	"context"
	"encoding/json"
	"fmt"
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

func JSON(v any) json.RawMessage { b, _ := json.Marshal(v); return b }

func descriptorBoundPreparerForTest(t *testing.T, runner *gitx.Runner, cfg config.Config, store *state.Store, slot state.Slot) workspace.Preparer {
	t.Helper()
	root := cfg.Storage.WorktreeRoot
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	owner, _, err := domain.OpenOwnedRoot(root, root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = owner.Close() })
	return workspace.Preparer{
		Git: runner, Config: cfg, Ownership: store, OwnedRoot: owner, RootPath: root,
		SlotPath: slot.Path, RootID: slot.RootID, SlotRelPath: slot.RelPath,
	}
}

func requireDaemonIntegration(t *testing.T) {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping daemon integration test in short mode")
	}
}

func TestCrashRecoveryConvergesAfterWorktreeAndRefsExist(t *testing.T) {
	t.Parallel()
	requireDaemonIntegration(t)
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
	w = registerTestWorkspace(t, store, w)
	resolved, err := pool.ResolveBranches(ctx, runner, w, nil)
	if err != nil {
		t.Fatal(err)
	}
	id, err := domain.NewID()
	if err != nil {
		t.Fatal(err)
	}
	slotRelative, err := slotRelPath(string(w.ID), id, false)
	if err != nil {
		t.Fatal(err)
	}
	slotRoot := filepath.Join(cfg.Storage.WorktreeRoot, slotRelative)
	if err := os.MkdirAll(slotRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	m := &Manager{cfg: cfg, store: store, git: runner, log: slog.New(slog.NewTextHandler(io.Discard, nil)), roots: map[string]bool{cfg.Storage.WorktreeRoot: true}, rootIDs: map[string]string{}}
	repos, err := m.slotRepos(slotRoot, w, resolved, 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	session := state.Session{ID: id, WorkspaceID: string(w.ID), SlotID: id, State: "STARTING", AgentKind: "codex", TokenHash: state.HashToken("token")}
	if err := os.MkdirAll(slotRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	slot := storeSlotAt(t, store, cfg.Storage.WorktreeRoot, string(w.ID), id, slotRoot, 1, "PREPARING")
	prepareJob, err := store.CreateSlotSession(ctx, slot, repos, session, "PREPARE")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ClaimJob(ctx, prepareJob.ID, "crashed-daemon"); err != nil {
		t.Fatal(err)
	}
	preparer := descriptorBoundPreparerForTest(t, runner, cfg, store, slot)
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
	prepared, err := store.Slot(ctx, id)
	if err != nil || prepared.State != "LEASED" {
		t.Fatalf("prepared slot=%+v err=%v", prepared, err)
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
	archiveManager := archive.Manager{Git: runner, Preparer: &preparer, Ownership: store}
	first, err := archiveManager.SnapshotWithPersistence(ctx, resolved[0].Repository, repos[0].WorktreePath, id, releasedAt.Add(cfg.Retention.RecoverySnapshot.Duration), nil)
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

func TestSingleRepositoryColdRemovalRecreatesReadySlotRoot(t *testing.T) {
	t.Parallel()
	requireDaemonIntegration(t)
	root := t.TempDir()
	repoPath := filepath.Join(root, "repo")
	initGitRepo(t, repoPath)
	cfg := config.Defaults()
	cfg.Storage.WorktreeRoot = filepath.Join(root, "worktrees")
	cfg.Pool.WarmPerWorkspace = 1
	store, err := state.Open(filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	m := testManager(t, cfg, store)
	m.git.SetTimeout(10 * time.Second)
	t.Cleanup(m.Close)
	ctx := context.Background()
	discoverer := discovery.Discoverer{Git: m.git, Config: cfg}
	w, err := discoverer.Resolve(ctx, repoPath)
	if err != nil {
		t.Fatal(err)
	}
	w = registerTestWorkspace(t, store, w)
	raw := openManagerCoverageDB(t, filepath.Join(root, "state.db"))
	if _, err := raw.ExecContext(ctx, `UPDATE repositories SET last_leased_at=?`, state.FormatTime(time.Now())); err != nil {
		t.Fatal(err)
	}
	if err := m.ensureStandby(ctx, w); err != nil {
		t.Fatal(err)
	}
	jobs, err := store.RecoverJobs(ctx, false)
	if err != nil || len(jobs) != 1 || jobs[0].Kind != "PREPARE" {
		t.Fatalf("standby jobs=%+v err=%v", jobs, err)
	}
	prepared, err := store.ClaimJob(ctx, jobs[0].ID, "test")
	if err != nil {
		t.Fatal(err)
	}
	if err := m.runRecoveredJob(ctx, prepared); err != nil {
		t.Fatal(err)
	}
	if err := store.FinishJob(ctx, prepared.ID, "test", nil); err != nil {
		t.Fatal(err)
	}
	ready, ok, err := store.ReadySlot(ctx, string(w.ID))
	if err != nil || !ok {
		t.Fatalf("ready slot=%+v ok=%v err=%v", ready, ok, err)
	}
	candidates, err := store.ColdRepositoryCandidates(ctx, state.FormatTime(time.Now().Add(time.Hour)))
	if err != nil || len(candidates) != 1 {
		t.Fatalf("cold candidates=%+v err=%v", candidates, err)
	}
	job, changed, err := store.ScheduleColdRepositoryRemoval(ctx, candidates[0])
	if err != nil || !changed {
		t.Fatalf("schedule cold job=%+v changed=%v err=%v", job, changed, err)
	}
	claimed, err := store.ClaimJob(ctx, job.ID, "test")
	if err != nil {
		t.Fatal(err)
	}
	if err := m.runRecoveredJob(ctx, claimed); err != nil {
		t.Fatal(err)
	}
	if err := store.FinishJob(ctx, claimed.ID, "test", nil); err != nil {
		t.Fatal(err)
	}
	slot, err := store.Slot(ctx, ready.ID)
	repository, repositoryErr := store.SlotRepository(ctx, ready.ID, candidates[0].RepositoryID)
	if err != nil || repositoryErr != nil || slot.State != "READY" || repository.State != "COLD" {
		t.Fatalf("cold state slot=%+v repository=%+v err=%v repositoryErr=%v", slot, repository, err, repositoryErr)
	}
	if _, err := os.Lstat(filepath.Join(ready.Path, repository.DirName)); !os.IsNotExist(err) {
		t.Fatalf("retired repository worktree still exists: %v", err)
	}
	entries, err := os.ReadDir(ready.Path)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != workspace.OwnershipMarkerName(candidates[0].RepositoryID) {
		names := make([]string, 0, len(entries))
		for _, entry := range entries {
			names = append(names, entry.Name())
		}
		t.Fatalf("retired slot directory contents=%v, want only the ownership marker", names)
	}
	lease, err := m.ResolveAndLease(ctx, repoPath, nil, "claude", 0)
	if err != nil {
		t.Fatal(err)
	}
	if lease.SessionID != ready.ID {
		t.Fatalf("cold lease session=%q, want the READY slot %q", lease.SessionID, ready.ID)
	}
	want := filepath.Join(ready.Path, repository.DirName)
	if lease.Path != want {
		t.Fatalf("cold lease path=%q, want the repository directory %q", lease.Path, want)
	}
	if info, err := os.Lstat(lease.Path); err != nil || !info.IsDir() {
		t.Fatalf("cold lease path is not a directory: info=%+v err=%v", info, err)
	}
}

func TestWarmSlotLeaseHandsOutTheRepositoryDirectory(t *testing.T) {
	t.Parallel()
	requireDaemonIntegration(t)
	root := t.TempDir()
	repoPath := filepath.Join(root, "repo")
	initGitRepo(t, repoPath)
	cfg := config.Defaults()
	cfg.Storage.WorktreeRoot = filepath.Join(root, "worktrees")
	cfg.Pool.WarmPerWorkspace = 1
	store, err := state.Open(filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	m := testManager(t, cfg, store)
	m.git.SetTimeout(10 * time.Second)
	t.Cleanup(m.Close)
	ctx := context.Background()
	discoverer := discovery.Discoverer{Git: m.git, Config: cfg}
	w, err := discoverer.Resolve(ctx, repoPath)
	if err != nil {
		t.Fatal(err)
	}
	w = registerTestWorkspace(t, store, w)
	raw := openManagerCoverageDB(t, filepath.Join(root, "state.db"))
	if _, err := raw.ExecContext(ctx, `UPDATE repositories SET last_leased_at=?`, state.FormatTime(time.Now())); err != nil {
		t.Fatal(err)
	}
	if err := m.ensureStandby(ctx, w); err != nil {
		t.Fatal(err)
	}
	jobs, err := store.RecoverJobs(ctx, false)
	if err != nil || len(jobs) != 1 || jobs[0].Kind != "PREPARE" {
		t.Fatalf("standby jobs=%+v err=%v", jobs, err)
	}
	prepared, err := store.ClaimJob(ctx, jobs[0].ID, "test")
	if err != nil {
		t.Fatal(err)
	}
	if err := m.runRecoveredJob(ctx, prepared); err != nil {
		t.Fatal(err)
	}
	if err := store.FinishJob(ctx, prepared.ID, "test", nil); err != nil {
		t.Fatal(err)
	}
	ready, ok, err := store.ReadySlot(ctx, string(w.ID))
	if err != nil || !ok {
		t.Fatalf("ready slot=%+v ok=%v err=%v", ready, ok, err)
	}
	repositories, err := store.SlotRepositories(ctx, ready.ID)
	if err != nil || len(repositories) != 1 {
		t.Fatalf("slot repositories=%+v err=%v", repositories, err)
	}
	lease, err := m.ResolveAndLease(ctx, repoPath, nil, "claude", 0)
	if err != nil {
		t.Fatal(err)
	}
	if lease.SessionID != ready.ID {
		t.Fatalf("lease session=%q, want the warm slot %q (a cold start would defeat the test)", lease.SessionID, ready.ID)
	}
	if !lease.Ready {
		t.Fatalf("warm lease reported not ready: %+v", lease)
	}
	want := filepath.Join(ready.Path, repositories[0].DirName)
	if lease.Path != want {
		t.Fatalf("warm lease path=%q, want the repository directory %q", lease.Path, want)
	}
	if _, err := os.Lstat(filepath.Join(lease.Path, ".git")); err != nil {
		t.Fatalf("warm lease path is not a Git worktree: %v", err)
	}
	markerName := workspace.OwnershipMarkerName(repositories[0].RepositoryID)
	if _, err := os.Lstat(filepath.Join(lease.Path, markerName)); !os.IsNotExist(err) {
		t.Fatalf("ownership marker is visible inside the leased directory: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(ready.Path, markerName)); err != nil {
		t.Fatalf("ownership marker is missing from the slot directory: %v", err)
	}
}

func TestEnsureStandbyOnlyChecksOutRecentlyUsedRepositories(t *testing.T) {
	t.Parallel()
	requireDaemonIntegration(t)
	root := t.TempDir()
	hotRepoPath := filepath.Join(root, "hot")
	coldRepoPath := filepath.Join(root, "cold")
	initGitRepo(t, hotRepoPath)
	initGitRepo(t, coldRepoPath)
	cfg := config.Defaults()
	cfg.Storage.WorktreeRoot = filepath.Join(root, "worktrees")
	cfg.Pool.WarmPerWorkspace = 1
	databasePath := filepath.Join(root, "state.db")
	store, err := state.Open(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	m := testManager(t, cfg, store)
	m.git.SetTimeout(10 * time.Second)
	t.Cleanup(m.Close)
	ctx := context.Background()
	w := discovery.Workspace{ID: "workspace", Root: discoveryPath(root), Kind: "multi_repository", Repositories: []discovery.Repository{
		{ID: "hot", MainPath: discoveryPath(hotRepoPath), CommonDir: discoveryPath(filepath.Join(hotRepoPath, ".git")), RelativePath: "hot", DefaultBranch: "main"},
		{ID: "cold", MainPath: discoveryPath(coldRepoPath), CommonDir: discoveryPath(filepath.Join(coldRepoPath, ".git")), RelativePath: "cold", DefaultBranch: "main"},
	}}
	w = registerTestWorkspace(t, store, w)
	raw := openManagerCoverageDB(t, databasePath)
	if _, err := raw.ExecContext(ctx, `UPDATE repositories SET last_leased_at=? WHERE id=?`, state.FormatTime(time.Now()), "hot"); err != nil {
		t.Fatal(err)
	}
	if err := m.ensureStandby(ctx, w); err != nil {
		t.Fatal(err)
	}
	jobs, err := store.RecoverJobs(ctx, false)
	if err != nil || len(jobs) != 1 || jobs[0].Kind != "PREPARE" {
		t.Fatalf("standby jobs=%+v err=%v", jobs, err)
	}
	prepared, err := store.ClaimJob(ctx, jobs[0].ID, "test")
	if err != nil {
		t.Fatal(err)
	}
	if err := m.runRecoveredJob(ctx, prepared); err != nil {
		t.Fatal(err)
	}
	if err := store.FinishJob(ctx, prepared.ID, "test", nil); err != nil {
		t.Fatal(err)
	}
	ready, ok, err := store.ReadySlot(ctx, string(w.ID))
	if err != nil || !ok {
		t.Fatalf("ready slot=%+v ok=%v err=%v", ready, ok, err)
	}
	hotRepository, err := store.SlotRepository(ctx, ready.ID, "hot")
	if err != nil || hotRepository.State != "READY" {
		t.Fatalf("recently-used repository was not checked out: %+v err=%v", hotRepository, err)
	}
	if _, statErr := os.Stat(hotRepository.WorktreePath); statErr != nil {
		t.Fatalf("recently-used repository worktree is missing: %v", statErr)
	}
	coldRepository, err := store.SlotRepository(ctx, ready.ID, "cold")
	if err != nil || coldRepository.State != "COLD" {
		t.Fatalf("never-leased repository was checked out early: %+v err=%v", coldRepository, err)
	}
	if _, statErr := os.Stat(coldRepository.WorktreePath); !os.IsNotExist(statErr) {
		t.Fatalf("never-leased repository worktree was materialized: err=%v", statErr)
	}
}

func TestFreshNativeResumeWithoutPriorMappingUsesSourceWorkspace(t *testing.T) {
	t.Parallel()
	requireDaemonIntegration(t)
	root := t.TempDir()
	repoPath := filepath.Join(root, "repo")
	initGitRepo(t, repoPath)
	cfg := config.Defaults()
	cfg.Storage.WorktreeRoot = filepath.Join(root, "worktrees")
	cfg.Pool.WarmPerWorkspace = 0
	store, err := state.Open(filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	m := testManager(t, cfg, store)
	t.Cleanup(m.Close)
	ctx := context.Background()
	lease, err := m.AllocateResumeSlot(ctx, "codex", os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	if err := m.PrepareFreshResume(ctx, lease.SessionID, lease.Token, "new-agent-session", repoPath, []string{"main"}); err != nil {
		t.Fatal(err)
	}
	jobs, err := store.RecoverJobs(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, job := range jobs {
		if job.Kind != "PREPARE" {
			continue
		}
		claimed, err := store.ClaimJob(ctx, job.ID, "test")
		if err != nil {
			t.Fatal(err)
		}
		if err := m.runRecoveredJob(ctx, claimed); err != nil {
			t.Fatal(err)
		}
		if err := store.FinishJob(ctx, claimed.ID, "test", nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := m.WaitReady(ctx, lease.SessionID, lease.Token); err != nil {
		t.Fatal(err)
	}
	session, err := store.FindByAgentSession(ctx, "codex", "new-agent-session")
	if err != nil || session.ID != lease.SessionID || session.WorkspaceID == "" {
		t.Fatalf("fresh source mapping session=%+v err=%v", session, err)
	}
}

func TestLeaseArchiveAndRestorePreservesGitState(t *testing.T) {
	t.Parallel()
	requireDaemonIntegration(t)
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
	if err := waitReady(ctx, m, 30*time.Second, lease.SessionID, lease.Token); err != nil {
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
	if !strings.Contains(native.Path, string(filepath.Separator)+unboundNamespace+string(filepath.Separator)) {
		t.Fatalf("native resume path is not unbound: %s", native.Path)
	}
	if entries, err := os.ReadDir(native.Path); err != nil || len(entries) != 0 {
		t.Fatalf("unbound root must start empty: entries=%v err=%v", entries, err)
	}
	if err := m.BindAndRestoreResume(ctx, native.SessionID, native.Token, "agent-session-1"); err != nil {
		t.Fatal(err)
	}
	if err := waitReady(ctx, m, 30*time.Second, native.SessionID, native.Token); err != nil {
		details, detailsErr := store.StatusDiagnostics(ctx)
		t.Fatalf("wait for native resume: %v; diagnostics=%+v diagnostics_error=%v", err, details, detailsErr)
	}
	nativeWorktree := boundWorktreePath(t, store, native.SessionID)
	if status := gitOutput(t, nativeWorktree, "status", "--porcelain"); !strings.Contains(status, "MM tracked.txt") || !strings.Contains(status, "?? untracked.txt") {
		t.Fatalf("native restored status:\n%s", status)
	}
	resumed, err := m.Resume(ctx, lease.SessionID, "codex", os.Getpid(), false)
	if err != nil {
		t.Fatal(err)
	}
	if resumed.Path == lease.Path {
		t.Fatal("resume reused old physical path")
	}
	if err := waitReady(ctx, m, 30*time.Second, resumed.SessionID, resumed.Token); err != nil {
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
	t.Parallel()
	requireDaemonIntegration(t)
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
	t.Parallel()
	requireDaemonIntegration(t)
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	initGitRepo(t, repo)
	cfg := config.Defaults()
	cfg.Storage.WorktreeRoot = filepath.Join(root, "worktrees")
	cfg.Pool.WarmPerWorkspace = 0
	cfg.Retention.EndedWorktree.Duration = 0
	cfg.Retention.RecoverySnapshot.Duration = -time.Hour
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
	if err := waitReady(ctx, m, 10*time.Second, lease.SessionID, lease.Token); err != nil {
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
	t.Parallel()
	requireDaemonIntegration(t)
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
	w = registerTestWorkspace(t, store, w)
	resolved, err := pool.ResolveBranches(ctx, runner, w, nil)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := domain.NewID()
	slotRelative, err := slotRelPath(string(w.ID), id, false)
	if err != nil {
		t.Fatal(err)
	}
	slotRoot := filepath.Join(cfg.Storage.WorktreeRoot, slotRelative)
	repos := []state.SlotRepository{{RepositoryID: string(w.Repositories[0].ID), DirName: testDirName(w.Repositories[0], cfg), State: "PREPARING", RequestedRef: resolved[0].RequestedRef, BaseOID: resolved[0].OID, Fingerprint: "test"}}
	session := state.Session{ID: id, WorkspaceID: string(w.ID), SlotID: id, State: "STARTING", AgentKind: "codex", TokenHash: state.HashToken("token")}
	if err := os.MkdirAll(slotRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	slot := storeSlotAt(t, store, cfg.Storage.WorktreeRoot, string(w.ID), id, slotRoot, 1, "PREPARING")
	job, err := store.CreateSlotSession(ctx, slot, repos, session, "PREPARE")
	if err != nil {
		t.Fatal(err)
	}
	preparer := descriptorBoundPreparerForTest(t, runner, cfg, store, slot)
	if err := preparer.Prepare(ctx, w.Repositories[0], filepath.Join(slotRoot, repos[0].DirName), resolved[0].OID, id); err != nil {
		t.Fatal(err)
	}
	claimed, err := store.ClaimJob(ctx, job.ID, "setup")
	if err != nil {
		t.Fatal(err)
	}
	m := &Manager{cfg: cfg, store: store, git: runner, log: slog.New(slog.NewTextHandler(io.Discard, nil)), roots: map[string]bool{cfg.Storage.WorktreeRoot: true}, rootIDs: map[string]string{cfg.Storage.WorktreeRoot: slot.RootID}}
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
	archiveManager := archive.Manager{Git: runner, Preparer: &preparer, Ownership: store}
	expires := time.Now().Add(time.Hour)
	snapshot, err := archiveManager.SnapshotWithPersistence(ctx, w.Repositories[0], filepath.Join(slotRoot, repos[0].DirName), id, expires, nil)
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
	removalSlot, err := store.Slot(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.removeSlotWorktrees(ctx, archiveManager, cfg.Storage.WorktreeRoot, removalSlot, id); err != nil {
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

// 2つのgoroutineが同じREADY slotを選ぶ窓を狭めるため、直列で実行する。
// 負けた側のREADY所有権検査はErrOwnershipになり、ResolveAndLeaseは再試行せず失敗する。
func TestWarmPoolMaintainsCapacityAndNeverDoubleLeases(t *testing.T) {
	requireDaemonIntegration(t)
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
	if err := waitReady(ctx, m, 10*time.Second, first.SessionID, first.Token); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, 10*time.Second, func() bool {
		status, _ := store.Status(ctx)
		return status.Ready >= cfg.Pool.WarmPerWorkspace
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
	t.Parallel()
	requireDaemonIntegration(t)
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
	if err := waitReady(ctx, m, 15*time.Second, lease.SessionID, lease.Token); err != nil {
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
	if err := waitReady(ctx, m, 15*time.Second, native.SessionID, native.Token); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(boundWorktreePath(t, store, native.SessionID), "pending.txt"))
	if err != nil || string(data) != "recover me\n" {
		t.Fatalf("restored pending file=%q err=%v", data, err)
	}
}

func TestExpiredExplicitResumeRequiresOptInAndUsesCurrentBase(t *testing.T) {
	t.Parallel()
	requireDaemonIntegration(t)
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
	if err := waitReady(ctx, m, 10*time.Second, lease.SessionID, lease.Token); err != nil {
		t.Fatal(err)
	}
	if err := m.BindAgentSession(ctx, lease.SessionID, lease.Token, "expired-agent-session"); err != nil {
		t.Fatal(err)
	}
	refusedFresh, err := m.ResolveAndLease(ctx, repo, nil, "codex", os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	if err := m.PrepareFreshResume(ctx, refusedFresh.SessionID, refusedFresh.Token, "expired-agent-session", "", nil); err == nil {
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
	if err := waitReady(ctx, m, 10*time.Second, fresh.SessionID, fresh.Token); err != nil {
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
	if err := m.PrepareFreshResume(ctx, nativeFresh.SessionID, nativeFresh.Token, "expired-agent-session", "", nil); err != nil {
		t.Fatalf("--fresh rejected expired mapping: %v", err)
	}
	nativeWait, nativeCancel := context.WithTimeout(ctx, 10*time.Second)
	defer nativeCancel()
	if err := m.WaitReady(nativeWait, nativeFresh.SessionID, nativeFresh.Token); err != nil {
		t.Fatalf("native --fresh workspace did not become ready: %v", err)
	}
	if got := gitOutput(t, boundWorktreePath(t, store, nativeFresh.SessionID), "rev-parse", "HEAD"); got != gitOutput(t, repo, "rev-parse", "refs/heads/main") {
		t.Fatalf("native fresh base=%s main=%s", got, gitOutput(t, repo, "rev-parse", "refs/heads/main"))
	}
}

func TestMultiRepositoryBundleAndRootRules(t *testing.T) {
	t.Parallel()
	requireDaemonIntegration(t)
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
	cfg.Retention.EndedWorktree.Duration = 0
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
	coldLease, err := m.ResolveAndLease(context.Background(), root, nil, "codex", os.Getpid())
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

func waitReady(ctx context.Context, m *Manager, budget time.Duration, sessionID, token string) error {
	waitCtx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	return m.WaitReady(waitCtx, sessionID, token)
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

func TestWorktreeRootChangeKeepsExistingSessionsAndPlacesNewOnesInTheNewRoot(t *testing.T) {
	requireDaemonIntegration(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	repo := filepath.Join(home, "repo")
	initGitRepo(t, repo)
	oldRoot := filepath.Join(home, "worktrees-old")
	newRoot := filepath.Join(home, "worktrees-new")
	cfg := config.Defaults()
	cfg.Storage.WorktreeRoot = oldRoot
	cfg.Pool.WarmPerWorkspace = 0
	cfg.Discovery.ReconcileInterval.Duration = time.Hour
	writeWorktreeRootConfig(t, home, oldRoot)
	store, err := state.Open(filepath.Join(home, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	m := New(cfg, store, slog.New(slog.NewTextHandler(io.Discard, nil)))
	defer m.Close()
	ctx := context.Background()

	existing, err := m.ResolveAndLease(ctx, repo, nil, "codex", os.Getpid())
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

	fresh, err := m.ResolveAndLease(ctx, repo, nil, "codex", os.Getpid())
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

func TestMultiRepositorySiblingLinkedWorktreesAcquireASession(t *testing.T) {
	requireDaemonIntegration(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	bundle := filepath.Join(home, "bundle")
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

	cfg := config.Defaults()
	cfg.Storage.WorktreeRoot = filepath.Join(home, "worktrees")
	cfg.Pool.WarmPerWorkspace = 0
	cfg.Discovery.ReconcileInterval.Duration = time.Hour
	store, err := state.Open(filepath.Join(home, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	m := New(cfg, store, slog.New(slog.NewTextHandler(io.Discard, nil)))
	defer m.Close()
	ctx := context.Background()

	lease, err := m.ResolveAndLease(ctx, bundle, nil, "codex", os.Getpid())
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
