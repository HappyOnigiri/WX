package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/HappyOnigiri/WX/internal/archive"
	"github.com/HappyOnigiri/WX/internal/config"
	"github.com/HappyOnigiri/WX/internal/discovery"
	"github.com/HappyOnigiri/WX/internal/domain"
	"github.com/HappyOnigiri/WX/internal/gitx"
	"github.com/HappyOnigiri/WX/internal/state"
)

func testManager(t *testing.T, cfg config.Config, store *state.Store) *Manager {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	root, err := config.ExpandHome(cfg.Storage.WorktreeRoot)
	if err != nil {
		t.Fatal(err)
	}
	return &Manager{
		cfg:     cfg,
		store:   store,
		git:     &gitx.Runner{Timeout: time.Second},
		log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		started: time.Now(),
		roots:   map[string]bool{filepath.Clean(root): true},
		jobs:    make(chan jobWork, 4),
		ctx:     ctx,
		cancel:  cancel,
	}
}

func TestManagerReloadForgetAndDiagnosticErrors(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	ctx := context.Background()
	store, err := state.Open(filepath.Join(home, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	cfg := config.Defaults()
	cfg.Storage.WorktreeRoot = filepath.Join(home, "old-root")
	m := testManager(t, cfg, store)
	var dynamicLevel slog.LevelVar
	m.logLevel = &dynamicLevel
	t.Cleanup(m.Close)

	repository := filepath.Join(home, "repository")
	initGitRepo(t, repository)
	lease, err := m.ResolveAndLease(ctx, repository, []string{"main"}, "codex", os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	jobs, err := store.RecoverJobs(ctx, false)
	if err != nil || len(jobs) == 0 {
		t.Fatalf("preparation jobs=%v err=%v", jobs, err)
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
	readyCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := m.WaitReady(readyCtx, lease.SessionID, lease.Token); err != nil {
		t.Fatal(err)
	}
	if err := m.Forget(ctx, repository); err == nil {
		t.Fatal("active workspace was forgotten")
	}
	if err := m.Forget(ctx, filepath.Join(home, "missing")); err == nil {
		t.Fatal("unknown workspace was forgotten")
	}

	newRoot := filepath.Join(home, "new-root")
	configPath, err := config.Path()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(configPath), 0o700); err != nil {
		t.Fatal(err)
	}
	validConfig := "version: 1\nstorage:\n  worktree_root: " + newRoot + "\npool:\n  preparation_concurrency: 3\nreadiness:\n  timeout: 1s\nlogging:\n  level: debug\n"
	if err := os.WriteFile(configPath, []byte(validConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := m.reloadConfig(false); err != nil {
		t.Fatal(err)
	}
	if got := m.Config().Storage.WorktreeRoot; got != newRoot {
		t.Fatalf("reloaded root=%q", got)
	}
	if info, err := os.Stat(newRoot); err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("new root permissions=%v err=%v", info, err)
	}
	if len(m.workerStops) != 3 || m.git.GetTimeout() != time.Second || dynamicLevel.Level() != slog.LevelDebug {
		t.Fatalf("dynamic reload workers=%d timeout=%s level=%s", len(m.workerStops), m.git.GetTimeout(), dynamicLevel.Level())
	}
	m.resizeWorkers(1)
	if len(m.workerStops) != 1 {
		t.Fatalf("worker shrink left %d workers", len(m.workerStops))
	}
	if _, ok := m.rootForPath(filepath.Join(cfg.Storage.WorktreeRoot, "retired", "slot")); !ok {
		t.Fatal("retired root was not retained for safe draining")
	}
	if _, ok := m.rootForPath(filepath.Join(home, "outside")); ok {
		t.Fatal("outside path was accepted as wx-owned")
	}
	unknownPath := filepath.Join(newRoot, "workspaces", "unknown", "slots", "orphan", "root")
	if err := os.MkdirAll(unknownPath, 0o700); err != nil {
		t.Fatal(err)
	}
	missing, err := m.AllocateResumeSlot(ctx, "codex", os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(missing.Path); err != nil {
		t.Fatal(err)
	}
	head := gitOutput(t, repository, "rev-parse", "HEAD")
	gitRun(t, repository, "update-ref", "refs/wx/recovery/unregistered", head)
	repositories, err := store.Repositories(ctx)
	if err != nil || len(repositories) != 1 {
		t.Fatalf("registered repositories=%+v err=%v", repositories, err)
	}
	if err := store.SaveSnapshot(ctx, state.Snapshot{
		ID: "missing-ref-snapshot", SessionID: lease.SessionID, RepositoryID: string(repositories[0].ID),
		HeadOID: head, HeadRef: "refs/wx/recovery/missing-head", IndexTreeOID: head,
		WorktreeOID: head, WorktreeRef: "refs/wx/recovery/missing-worktree", Status: "ARCHIVED",
		CreatedAt: time.Now().UTC().Format(time.RFC3339Nano), ExpiresAt: time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano),
	}); err != nil {
		t.Fatal(err)
	}
	artifacts := m.artifactDiagnostics(ctx)
	if len(artifacts["unknown_paths"].([]string)) != 1 || len(artifacts["missing_paths"].([]string)) == 0 || len(artifacts["unknown_refs"].([]string)) != 1 || len(artifacts["missing_refs"].([]string)) != 2 {
		t.Fatalf("artifact diagnostics=%v", artifacts)
	}
	m.reconcileArtifacts(ctx)
	missingSlot, err := store.Slot(ctx, missing.SessionID)
	if err != nil || missingSlot.State != "QUARANTINED" {
		t.Fatalf("missing owned slot was not quarantined: slot=%+v err=%v", missingSlot, err)
	}
	quarantinedSession, err := store.SessionByID(ctx, lease.SessionID)
	if err != nil || quarantinedSession.State != "QUARANTINED" {
		t.Fatalf("missing recovery refs did not quarantine session: session=%+v err=%v", quarantinedSession, err)
	}
	diagnostics, err := store.StatusDiagnostics(ctx)
	if err != nil || len(diagnostics.Quarantine) < 4 {
		t.Fatalf("reconciliation quarantine diagnostics=%+v err=%v", diagnostics.Quarantine, err)
	}
	brokenRoot := filepath.Join(home, "missing-workspace")
	brokenRepository := filepath.Join(home, "missing-repository")
	if err := store.UpsertWorkspace(ctx, discovery.Workspace{ID: "broken-workspace", Root: discoveryPath(brokenRoot), Kind: "repository", Repositories: []discovery.Repository{{ID: "broken-repository", MainPath: discoveryPath(brokenRepository), CommonDir: discoveryPath(filepath.Join(brokenRepository, ".git")), DefaultBranch: "main"}}}); err != nil {
		t.Fatal(err)
	}
	brokenArtifacts := m.artifactDiagnostics(ctx)
	if len(brokenArtifacts["errors"].([]string)) == 0 {
		t.Fatalf("missing registered repository was absent from diagnostics: %v", brokenArtifacts)
	}
	m.reconcileRegistry(ctx)
	registration := m.registrationDiagnostics(ctx)
	if invalid := registration["invalid"].([]map[string]string); len(invalid) == 0 {
		t.Fatalf("missing registered workspace was absent from diagnostics: %v", registration)
	}
	blockedRoot := filepath.Join(home, "blocked-root")
	if err := os.WriteFile(blockedRoot, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	invalidRootConfig := "version: 1\nstorage:\n  worktree_root: " + blockedRoot + "\n"
	if err := os.WriteFile(configPath, []byte(invalidRootConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := m.ReloadConfig(); err == nil {
		t.Fatal("non-directory worktree root reload succeeded")
	}
	if got := m.Config().Storage.WorktreeRoot; got != newRoot {
		t.Fatalf("failed reload replaced active root with %q", got)
	}
	if err := os.WriteFile(configPath, []byte("unknown: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := m.reloadConfig(false); err == nil {
		t.Fatal("invalid config reload succeeded")
	}
	status, err := m.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if status["config_reload_error"] == "" || status["config_last_reload"] == "" {
		t.Fatalf("reload diagnostics=%v", status)
	}
	encoded, err := json.Marshal(status["worktree_roots"])
	if err != nil || !strings.Contains(string(encoded), `"active":false`) || !strings.Contains(string(encoded), `"active":true`) || !strings.Contains(string(encoded), `"allocated_bytes":`) || !strings.Contains(string(encoded), `"measurement":"st_blocks_x_512"`) {
		t.Fatalf("root drain diagnostics=%s err=%v", encoded, err)
	}
	if status["daemon_version"] == "" || status["workspace_details"] == nil || status["session_details"] == nil || status["repository_details"] == nil || status["job_details"] == nil || status["snapshot_details"] == nil {
		t.Fatalf("status detail fields are incomplete: %v", status)
	}
	doctor := m.Doctor(ctx)
	checks := doctor["checks"].(map[string]any)
	if _, ok := checks["hooks"]; !ok {
		t.Fatalf("doctor checks=%v", checks)
	}
	if formatOptionalTime(time.Time{}) != "" || formatOptionalTime(time.Unix(1, 0)) == "" {
		t.Fatal("optional time formatting is inconsistent")
	}
	if must("", errors.New("expected")) != "" || must("value", nil) != "value" {
		t.Fatal("must helper result is inconsistent")
	}
}

func TestDiagnosticFilesystemAndHookChecks(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	regular := filepath.Join(home, "regular")
	if err := os.WriteFile(regular, []byte("1234"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := diagnosticPath(regular, 0, 0o600); got != "ok" {
		t.Fatalf("regular diagnostic=%q", got)
	}
	if got := diagnosticPath(regular, os.ModeDir, 0o700); got != "not a directory" {
		t.Fatalf("directory diagnostic=%q", got)
	}
	if err := os.Chmod(regular, 0o644); err != nil {
		t.Fatal(err)
	}
	if got := diagnosticPath(regular, 0, 0o600); !strings.Contains(got, "permissions") {
		t.Fatalf("permission diagnostic=%q", got)
	}
	link := filepath.Join(home, "link")
	if err := os.Symlink(regular, link); err != nil {
		t.Fatal(err)
	}
	if got := diagnosticPath(link, 0, 0o600); got != "unsafe symlink" {
		t.Fatalf("symlink diagnostic=%q", got)
	}
	if got := diagnosticPath(filepath.Join(home, "missing"), 0, 0o600); !strings.Contains(got, "no such file") {
		t.Fatalf("missing diagnostic=%q", got)
	}
	if got := diagnosticPath("", 0, 0o600); got != "path unavailable" {
		t.Fatalf("empty diagnostic=%q", got)
	}
	if got := diagnosticPath(regular, os.ModeSocket, 0o600); got != "not a Unix socket" {
		t.Fatalf("socket diagnostic=%q", got)
	}
	if got := diagnosticPath(home, 0, 0o700); got != "not a regular file" {
		t.Fatalf("regular-file diagnostic=%q", got)
	}
	usage, err := directoryUsage(home)
	if err != nil || usage != 4 {
		t.Fatalf("directory usage=%d err=%v", usage, err)
	}

	for _, agentKind := range []string{"claude", "codex"} {
		path := filepath.Join(home, ".claude", "settings.json")
		if agentKind == "codex" {
			path = filepath.Join(home, ".codex", "hooks.json")
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		document := `{"hooks":{"SessionStart":[{"type":"command","command":"wx hook session-start"}],"UserPromptSubmit":[{"command":"wx hook user-prompt-submit"}],"PreToolUse":[{"command":"wx hook pre-tool-use"}]}}`
		if err := os.WriteFile(path, []byte(document), 0o600); err != nil {
			t.Fatal(err)
		}
		if !diagnosticHooksAvailable(agentKind) {
			t.Fatalf("%s hooks were not detected", agentKind)
		}
	}
	if diagnosticHookTreeContainsCommand(map[string]any{"disabled": true, "command": "wx hook session-start"}, "session-start") {
		t.Fatal("disabled diagnostic hook was accepted")
	}
}

func TestManagerReadinessAndRecoveryFailurePaths(t *testing.T) {
	root := t.TempDir()
	store, err := state.Open(filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	cfg := config.Defaults()
	cfg.Storage.WorktreeRoot = filepath.Join(root, "worktrees")
	cfg.Readiness.Timeout.Duration = 20 * time.Millisecond
	m := testManager(t, cfg, store)
	t.Cleanup(m.Close)
	ctx := context.Background()

	lease, err := m.AllocateResumeSlot(ctx, "codex", os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	if err := m.WaitReady(ctx, lease.SessionID, "wrong"); err == nil {
		t.Fatal("wrong token passed readiness")
	}
	timed, cancel := context.WithTimeout(ctx, 5*time.Millisecond)
	if err := m.WaitReady(timed, lease.SessionID, lease.Token); !errors.Is(err, context.DeadlineExceeded) {
		cancel()
		t.Fatalf("unbound readiness error=%v", err)
	}
	cancel()
	if err := m.ValidateFreshResume(ctx, lease.SessionID, lease.Token, "new-agent"); err == nil {
		t.Fatal("UNBOUND slot unexpectedly accepted fresh resume")
	}
	if err := store.SetSlotState(ctx, lease.SessionID, []string{"FAILED"}, "QUARANTINED", "test"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := m.waitForSnapshot(ctx, lease.SessionID); err == nil || !strings.Contains(err.Error(), "archive failed") {
		t.Fatalf("quarantined archive wait error=%v", err)
	}
	if err := m.WaitReady(ctx, lease.SessionID, lease.Token); err == nil {
		t.Fatal("quarantined slot reported ready")
	}
	if err := m.BindAgentSession(ctx, lease.SessionID, "wrong", "agent"); err == nil {
		t.Fatal("wrong token bound agent session")
	}
	if err := m.runRecoveredJob(ctx, state.Job{Kind: "UNKNOWN"}); err == nil {
		t.Fatal("unknown persistent job kind succeeded")
	}
	if err := m.removeSlotJob(ctx, state.Job{SlotID: lease.SessionID}); err == nil {
		t.Fatal("quarantined slot was removed")
	}
	if err := m.removeColdRepositoryJob(ctx, state.Job{SlotID: lease.SessionID, RepositoryID: "missing"}); err == nil {
		t.Fatal("missing repository retirement succeeded")
	}
	if _, err := m.ResumeStatus(ctx, "missing"); err == nil {
		t.Fatal("missing resume status succeeded")
	}
	if _, err := m.Resume(ctx, "missing", "codex", os.Getpid(), false); err == nil {
		t.Fatal("missing explicit resume succeeded")
	}
	if _, _, err := m.waitForSnapshot(ctx, "missing"); err == nil {
		t.Fatal("missing snapshot wait succeeded")
	}
	w := discovery.Workspace{ID: "pending-workspace", Root: discoveryPath(root), Kind: "repository"}
	if err := store.UpsertWorkspace(ctx, w); err != nil {
		t.Fatal(err)
	}
	pending := state.Session{ID: "pending", WorkspaceID: "pending-workspace", SlotID: "pending", State: "RELEASING", AgentKind: "codex", TokenHash: state.HashToken("pending")}
	if _, err := store.CreateSlotSession(ctx, state.Slot{ID: "pending", WorkspaceID: "pending-workspace", Generation: 1, Path: filepath.Join(root, "pending"), State: "DRAINING"}, nil, pending, ""); err != nil {
		t.Fatal(err)
	}
	cancelled, cancelWaiting := context.WithCancel(ctx)
	cancelWaiting()
	if _, err := m.Resume(cancelled, "pending", "codex", os.Getpid(), false); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled pending resume error=%v", err)
	}
	expired := state.Session{ID: "expired", SlotID: "expired", State: "EXPIRED", AgentKind: "codex", TokenHash: state.HashToken("expired")}
	if _, err := store.CreateSlotSession(ctx, state.Slot{ID: "expired", Path: filepath.Join(root, "expired"), State: "SNAPSHOTTED"}, nil, expired, ""); err != nil {
		t.Fatal(err)
	}
	if _, _, err := m.waitForSnapshot(ctx, "expired"); err == nil || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("expired archive wait error=%v", err)
	}
	waiting := state.Session{ID: "waiting", SlotID: "waiting", State: "ACTIVE", AgentKind: "codex", TokenHash: state.HashToken("waiting")}
	if _, err := store.CreateSlotSession(ctx, state.Slot{ID: "waiting", Path: filepath.Join(root, "waiting"), State: "LEASED"}, nil, waiting, ""); err != nil {
		t.Fatal(err)
	}
	if _, _, err := m.waitForSnapshot(ctx, "waiting"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("archive wait timeout error=%v", err)
	}

	missing := state.Slot{ID: "missing", Path: filepath.Join(root, "does-not-exist")}
	if ok, err := m.readyMatches(ctx, missing, nil); err != nil || ok {
		t.Fatalf("missing READY root ok=%v err=%v", ok, err)
	}
	file := filepath.Join(root, "file")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if ok, err := m.readyMatches(ctx, state.Slot{ID: "file", Path: file}, nil); err != nil || ok {
		t.Fatalf("file READY root ok=%v err=%v", ok, err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(file, link); err != nil {
		t.Fatal(err)
	}
	if ok, err := m.readyMatches(ctx, state.Slot{ID: "link", Path: link}, nil); err != nil || ok {
		t.Fatalf("symlink READY root ok=%v err=%v", ok, err)
	}
}

func TestRemoveEmptySlotRejectsDescendantSymlinkSwap(t *testing.T) {
	root := t.TempDir()
	worktreeRoot := filepath.Join(root, "worktrees")
	if err := os.MkdirAll(filepath.Join(worktreeRoot, "unbound", "slot"), 0o700); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	outsideFile := filepath.Join(outside, "keep")
	if err := os.WriteFile(outsideFile, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	slotPath := filepath.Join(worktreeRoot, "unbound", "slot", "root")
	if err := os.Symlink(outside, slotPath); err != nil {
		t.Fatal(err)
	}
	store, err := state.Open(filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := store.CreateSlotSession(context.Background(), state.Slot{ID: "slot", Path: slotPath, State: "REMOVING"}, nil, state.Session{ID: "slot", SlotID: "slot", State: "EXPIRED", AgentKind: "codex", TokenHash: state.HashToken("token")}, ""); err != nil {
		t.Fatal(err)
	}
	manager := &Manager{store: store}
	if err := manager.removeSlotWorktrees(context.Background(), archive.Manager{}, worktreeRoot, "slot", "", slotPath); err == nil {
		t.Fatal("symlinked empty slot was removed")
	}
	if data, err := os.ReadFile(outsideFile); err != nil || string(data) != "keep" {
		t.Fatalf("outside file changed: data=%q err=%v", data, err)
	}
}

func TestSessionEndWaitsForForegroundClientExit(t *testing.T) {
	store, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	if _, err := store.CreateSlotSession(ctx, state.Slot{ID: "live", Path: filepath.Join(t.TempDir(), "root"), State: "LEASED"}, nil, state.Session{ID: "live", SlotID: "live", State: "ACTIVE", AgentKind: "codex", ClientPID: os.Getpid(), TokenHash: state.HashToken("token")}, ""); err != nil {
		t.Fatal(err)
	}
	manager := &Manager{store: store, jobs: make(chan jobWork, 1), ctx: context.Background()}
	if err := manager.Release(ctx, "live", "token", "session-end-hook"); err != nil {
		t.Fatal(err)
	}
	if session, err := store.SessionByID(ctx, "live"); err != nil || session.State != "ACTIVE" {
		t.Fatalf("live SessionEnd changed session: state=%s err=%v", session.State, err)
	}
	if err := manager.Release(ctx, "live", "token", "client-exit"); err != nil {
		t.Fatal(err)
	}
	if session, err := store.SessionByID(ctx, "live"); err != nil || session.State != "RELEASING" {
		t.Fatalf("client exit did not release session: state=%s err=%v", session.State, err)
	}
}

func TestManagerIdempotentJobsAndOwnershipRejections(t *testing.T) {
	root := t.TempDir()
	store, err := state.Open(filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	cfg := config.Defaults()
	cfg.Storage.WorktreeRoot = filepath.Join(root, "worktrees")
	m := testManager(t, cfg, store)
	t.Cleanup(m.Close)
	ctx := context.Background()
	w := discovery.Workspace{ID: "workspace", Root: discoveryPath(root), Kind: "repository", Repositories: []discovery.Repository{{ID: "repository", MainPath: discoveryPath(filepath.Join(root, "repository")), CommonDir: discoveryPath(filepath.Join(root, "repository", ".git")), DefaultBranch: "main"}}}
	if err := store.UpsertWorkspace(ctx, w); err != nil {
		t.Fatal(err)
	}

	readyJob, err := store.CreateStandby(ctx, state.Slot{ID: "ready", WorkspaceID: "workspace", Generation: 1, Path: filepath.Join(cfg.Storage.WorktreeRoot, "ready"), State: "PREPARING"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetSlotState(ctx, "ready", []string{"PREPARING"}, "READY", ""); err != nil {
		t.Fatal(err)
	}
	if err := m.prepareSlot(ctx, "ready", discovery.Workspace{}, nil, nil); err != nil {
		t.Fatalf("READY prepare replay: %v", err)
	}
	if err := m.restoreSlot(ctx, "ready", discovery.Workspace{}, nil, nil, nil); err != nil {
		t.Fatalf("READY restore replay: %v", err)
	}
	claimed, err := store.ClaimJob(ctx, readyJob.ID, "test")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.FinishJob(ctx, claimed.ID, "test", nil); err != nil {
		t.Fatal(err)
	}
	if err := store.SetSlotState(ctx, "ready", []string{"READY"}, "FAILED", "TEST"); err != nil {
		t.Fatal(err)
	}
	if err := m.prepareSlot(ctx, "ready", discovery.Workspace{}, nil, nil); err == nil {
		t.Fatal("FAILED slot preparation replay succeeded")
	}
	if err := m.restoreSlot(ctx, "ready", discovery.Workspace{}, nil, nil, nil); err == nil {
		t.Fatal("FAILED slot restore replay succeeded")
	}
	if _, err := m.resolvedFromStored(ctx, discovery.Workspace{}, []state.SlotRepository{{RepositoryID: "removed"}}); err == nil {
		t.Fatal("removed repository resolved from stored state")
	}

	coldRepo := state.SlotRepository{RepositoryID: "repository", WorktreePath: filepath.Join(cfg.Storage.WorktreeRoot, "cold", "repo"), State: "COLD"}
	if _, err := store.CreateStandby(ctx, state.Slot{ID: "cold", WorkspaceID: "workspace", Generation: 1, Path: filepath.Join(cfg.Storage.WorktreeRoot, "cold"), State: "PREPARING"}, []state.SlotRepository{coldRepo}); err != nil {
		t.Fatal(err)
	}
	if err := m.removeColdRepositoryJob(ctx, state.Job{SlotID: "cold", RepositoryID: "repository"}); err != nil {
		t.Fatalf("COLD removal replay: %v", err)
	}
	if err := m.removeColdRepositoryJob(ctx, state.Job{SlotID: "cold", RepositoryID: "missing"}); err == nil {
		t.Fatal("missing cold repository removal succeeded")
	}
	if err := m.removeSlotWorktrees(ctx, archive.Manager{}, filepath.Join(root, "owned"), "cold", "", filepath.Join(root, "outside")); err == nil {
		t.Fatal("outside slot worktree removal succeeded")
	}
	if snapshotsUsable([]state.Snapshot{{ExpiresAt: "invalid"}}, time.Now()) {
		t.Fatal("invalid snapshot expiry was usable")
	}
}

func TestManagerResumeAndArchiveFailureStates(t *testing.T) {
	root := t.TempDir()
	store, err := state.Open(filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	cfg := config.Defaults()
	cfg.Storage.WorktreeRoot = filepath.Join(root, "worktrees")
	m := testManager(t, cfg, store)
	t.Cleanup(m.Close)
	ctx := context.Background()
	w := discovery.Workspace{ID: "workspace", Root: discoveryPath(root), Kind: "multi_repository", Repositories: []discovery.Repository{
		{ID: "repository-1", MainPath: discoveryPath(filepath.Join(root, "repository-1")), CommonDir: discoveryPath(filepath.Join(root, "repository-1", ".git")), RelativePath: "repository-1", DefaultBranch: "main"},
		{ID: "repository-2", MainPath: discoveryPath(filepath.Join(root, "repository-2")), CommonDir: discoveryPath(filepath.Join(root, "repository-2", ".git")), RelativePath: "repository-2", DefaultBranch: "main"},
	}}
	if err := store.UpsertWorkspace(ctx, w); err != nil {
		t.Fatal(err)
	}
	createSession := func(id, sessionState, slotState, parent string) (state.Session, string) {
		t.Helper()
		token := id + "-token"
		session := state.Session{ID: id, WorkspaceID: "workspace", SlotID: id, ParentSessionID: parent, State: sessionState, AgentKind: "codex", TokenHash: state.HashToken(token)}
		if sessionState == "UNBOUND" {
			session.WorkspaceID = ""
		}
		if _, err := store.CreateSlotSession(ctx, state.Slot{ID: id, WorkspaceID: session.WorkspaceID, Generation: 1, Path: filepath.Join(cfg.Storage.WorktreeRoot, id), State: slotState}, nil, session, ""); err != nil {
			t.Fatal(err)
		}
		return session, token
	}

	missing, missingToken := createSession("missing-current", "UNBOUND", "UNBOUND", "")
	if err := m.BindAndRestoreResume(ctx, missing.ID, missingToken, "unmapped-agent"); err == nil || !strings.Contains(err.Error(), "no wx recovery mapping") {
		t.Fatalf("missing mapping error=%v", err)
	}
	if err := m.BindAndRestoreResume(ctx, missing.ID, "wrong", "unmapped-agent"); err == nil {
		t.Fatal("resume binding accepted a wrong token")
	}
	for _, priorState := range []string{"EXPIRED", "ACTIVE", "ARCHIVED"} {
		id := strings.ToLower(priorState)
		prior, _ := createSession(id+"-prior", priorState, "SNAPSHOTTED", "")
		agentID := id + "-agent"
		if err := store.BindAgentSession(ctx, prior.ID, agentID); err != nil {
			t.Fatal(err)
		}
		current, token := createSession(id+"-current", "UNBOUND", "UNBOUND", "")
		if err := m.BindAndRestoreResume(ctx, current.ID, token, agentID); err == nil {
			t.Fatalf("%s prior unexpectedly resumed without a usable snapshot", priorState)
		}
	}

	noParent, _ := createSession("no-parent", "RESTORING", "RESTORING", "")
	if err := m.resumeRestoreJob(ctx, noParent.ID); err == nil || !strings.Contains(err.Error(), "no parent") {
		t.Fatalf("parentless restore error=%v", err)
	}
	expiredParent, _ := createSession("expired-parent", "EXPIRED", "SNAPSHOTTED", "")
	expiredChild, _ := createSession("expired-child", "RESTORING", "RESTORING", expiredParent.ID)
	if err := m.resumeRestoreJob(ctx, expiredChild.ID); err == nil || !strings.Contains(err.Error(), "expired or incomplete") {
		t.Fatalf("expired snapshot restore error=%v", err)
	}
	expiredSlot, err := store.Slot(ctx, expiredChild.ID)
	if err != nil || expiredSlot.State != "QUARANTINED" {
		t.Fatalf("expired restore slot=%+v err=%v", expiredSlot, err)
	}

	incompleteParent, _ := createSession("incomplete-parent", "ARCHIVED", "SNAPSHOTTED", "")
	snapshot := state.Snapshot{ID: "incomplete-snapshot", SessionID: incompleteParent.ID, RepositoryID: "repository-1", HeadOID: "head", HeadRef: "refs/wx/recovery/head", IndexTreeOID: "index", WorktreeOID: "worktree", WorktreeRef: "refs/wx/recovery/worktree", Status: "ARCHIVED", CreatedAt: time.Now().UTC().Format(time.RFC3339Nano), ExpiresAt: time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano)}
	if err := store.SaveSnapshot(ctx, snapshot); err != nil {
		t.Fatal(err)
	}
	incompleteChild, _ := createSession("incomplete-child", "RESTORING", "RESTORING", incompleteParent.ID)
	if err := m.resumeRestoreJob(ctx, incompleteChild.ID); err == nil || !strings.Contains(err.Error(), "snapshot missing repository") {
		t.Fatalf("incomplete snapshot restore error=%v", err)
	}

	archived, _ := createSession("archived", "ARCHIVED", "SNAPSHOTTED", "")
	if err := m.snapshotSession(ctx, archived); err != nil {
		t.Fatalf("archived snapshot replay: %v", err)
	}
	active, _ := createSession("active", "ACTIVE", "LEASED", "")
	if err := m.snapshotSession(ctx, active); err == nil || !strings.Contains(err.Error(), "cannot be snapshotted") {
		t.Fatalf("invalid snapshot state error=%v", err)
	}
}

func discoveryPath(path string) domain.CanonicalPath {
	return domain.CanonicalPath(filepath.Clean(path))
}

func TestWorkerStopsRetryingAfterBoundedAttempts(t *testing.T) {
	root := t.TempDir()
	store, err := state.Open(filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	cfg := config.Defaults()
	cfg.Storage.WorktreeRoot = filepath.Join(root, "worktrees")
	m := testManager(t, cfg, store)
	t.Cleanup(m.Close)
	ctx := context.Background()
	job, err := store.CreateJob(ctx, "REMOVE", "", "missing-slot", "")
	if err != nil {
		t.Fatal(err)
	}
	for attempt := 1; attempt < maxJobAttempts; attempt++ {
		claimed, err := store.ClaimJob(ctx, job.ID, "seed")
		if err != nil {
			t.Fatal(err)
		}
		if err := store.RetryJob(ctx, claimed.ID, "seed", 0, "TEST_RETRY"); err != nil {
			t.Fatal(err)
		}
	}
	m.wg.Add(1)
	go m.runWorker(0, make(chan struct{}))
	m.schedule(job)
	deadline := time.Now().Add(2 * time.Second)
	for {
		status, err := store.Status(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if status.Jobs == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("exhausted removal job remained retryable")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestSnapshotFailureQuarantinesSlotWithoutRemovingWorktreeMetadata(t *testing.T) {
	root := t.TempDir()
	repoPath := filepath.Join(root, "repository")
	initGitRepo(t, repoPath)
	store, err := state.Open(filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	cfg := config.Defaults()
	cfg.Storage.WorktreeRoot = filepath.Join(root, "worktrees")
	m := testManager(t, cfg, store)
	t.Cleanup(m.Close)
	ctx := context.Background()
	w := discovery.Workspace{ID: "workspace", Root: discoveryPath(repoPath), Kind: "repository", Repositories: []discovery.Repository{{ID: "repository", MainPath: discoveryPath(repoPath), CommonDir: discoveryPath(filepath.Join(repoPath, ".git")), DefaultBranch: "main"}}}
	if err := store.UpsertWorkspace(ctx, w); err != nil {
		t.Fatal(err)
	}
	slotPath := filepath.Join(cfg.Storage.WorktreeRoot, "slot")
	session := state.Session{ID: "session", WorkspaceID: "workspace", SlotID: "slot", State: "ACTIVE", AgentKind: "codex", TokenHash: state.HashToken("token")}
	repository := state.SlotRepository{RepositoryID: "repository", WorktreePath: filepath.Join(slotPath, "missing"), State: "LEASED", RequestedRef: "main", BaseOID: gitOutput(t, repoPath, "rev-parse", "HEAD"), Fingerprint: "fp"}
	if _, err := store.CreateSlotSession(ctx, state.Slot{ID: "slot", WorkspaceID: "workspace", Generation: 1, Path: slotPath, State: "LEASED"}, []state.SlotRepository{repository}, session, ""); err != nil {
		t.Fatal(err)
	}
	if _, changed, err := store.Release(ctx, "session", "workspace", "slot"); err != nil || !changed {
		t.Fatalf("release changed=%v err=%v", changed, err)
	}
	released, err := store.SessionByID(ctx, "session")
	if err != nil {
		t.Fatal(err)
	}
	if err := m.snapshotSession(ctx, released); err == nil {
		t.Fatal("snapshot of missing worktree succeeded")
	}
	slot, err := store.Slot(ctx, "slot")
	if err != nil || slot.State != "QUARANTINED" {
		t.Fatalf("snapshot failure slot=%+v err=%v", slot, err)
	}
	if repositories, err := store.SlotRepositories(ctx, "slot"); err != nil || len(repositories) != 1 {
		t.Fatalf("snapshot failure discarded metadata: repositories=%+v err=%v", repositories, err)
	}
}

func TestMultiRepositoryRootMaterializationFailurePersistsFailedState(t *testing.T) {
	root := t.TempDir()
	store, err := state.Open(filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	cfg := config.Defaults()
	cfg.Storage.WorktreeRoot = filepath.Join(root, "worktrees")
	cfg.Workspaces[root] = config.Workspace{Link: []string{"missing-root-entry"}}
	m := testManager(t, cfg, store)
	t.Cleanup(m.Close)
	ctx := context.Background()
	w := discovery.Workspace{ID: "workspace", Root: discoveryPath(root), Kind: "multi_repository"}
	if err := store.UpsertWorkspace(ctx, w); err != nil {
		t.Fatal(err)
	}
	slotPath := filepath.Join(cfg.Storage.WorktreeRoot, "slot")
	if err := os.MkdirAll(slotPath, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateStandby(ctx, state.Slot{ID: "slot", WorkspaceID: "workspace", Generation: 1, Path: slotPath, State: "PREPARING"}, nil); err != nil {
		t.Fatal(err)
	}
	if err := m.prepareSlot(ctx, "slot", w, nil, nil); err == nil {
		t.Fatal("root materialization with a missing link succeeded")
	}
	slot, err := store.Slot(ctx, "slot")
	if err != nil || slot.State != "FAILED" {
		t.Fatalf("materialization failure slot=%+v err=%v", slot, err)
	}
	restorePath := filepath.Join(cfg.Storage.WorktreeRoot, "restore")
	if err := os.MkdirAll(restorePath, 0o700); err != nil {
		t.Fatal(err)
	}
	restoreSession := state.Session{ID: "restore", WorkspaceID: "workspace", SlotID: "restore", State: "RESTORING", AgentKind: "codex", TokenHash: state.HashToken("restore")}
	if _, err := store.CreateSlotSession(ctx, state.Slot{ID: "restore", WorkspaceID: "workspace", Generation: 1, Path: restorePath, State: "RESTORING"}, nil, restoreSession, ""); err != nil {
		t.Fatal(err)
	}
	if err := m.restoreSlot(ctx, "restore", w, nil, nil, nil); err == nil {
		t.Fatal("restore root materialization with a missing link succeeded")
	}
	restoredSlot, err := store.Slot(ctx, "restore")
	if err != nil || restoredSlot.State != "FAILED" {
		t.Fatalf("restore materialization failure slot=%+v err=%v", restoredSlot, err)
	}
}

func TestManagerFailsClosedWhenStateStoreBecomesUnavailable(t *testing.T) {
	root := t.TempDir()
	store, err := state.Open(filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Defaults()
	cfg.Storage.WorktreeRoot = filepath.Join(root, "worktrees")
	m := testManager(t, cfg, store)
	ctx := context.Background()
	job, err := store.CreateJob(ctx, "ENSURE_STANDBY", "missing", "", "")
	if err != nil {
		t.Fatal(err)
	}
	m.recoverJobs(false)
	select {
	case queued := <-m.jobs:
		if queued.id != job.ID {
			t.Fatalf("recovered job=%+v", queued)
		}
	default:
		t.Fatal("pending durable job was not recovered")
	}
	m.maybeBackup(ctx)
	m.maybeBackup(ctx)
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Status(ctx); err == nil {
		t.Fatal("status succeeded after state store closure")
	}
	doctor := m.Doctor(ctx)
	checks := doctor["checks"].(map[string]any)
	if checks["sqlite"] == "ok" {
		t.Fatalf("doctor did not report SQLite failure: %v", checks)
	}
	if diagnostics := m.artifactDiagnostics(ctx); len(diagnostics["errors"].([]string)) == 0 {
		t.Fatalf("artifact diagnostics did not report state failure: %v", diagnostics)
	}
	m.reconcileArtifacts(ctx)
	m.reconcileRegistry(ctx)
	m.reconcileOrphans(ctx)
	m.recoverJobs(false)
	m.mu.Lock()
	m.lastBackup = time.Time{}
	m.mu.Unlock()
	m.maybeBackup(ctx)
	if err := m.enqueue("ENSURE_STANDBY", "", "", ""); err == nil {
		t.Fatal("job enqueue succeeded after state store closure")
	}
	for _, job := range []state.Job{{Kind: "PREPARE", WorkspaceID: "missing"}, {Kind: "ENSURE_STANDBY", WorkspaceID: "missing"}, {Kind: "SNAPSHOT", SessionID: "missing"}} {
		if err := m.runRecoveredJob(ctx, job); err == nil {
			t.Fatalf("%s job succeeded after state store closure", job.Kind)
		}
	}
	if _, err := m.GC(ctx, false); err == nil {
		t.Fatal("GC succeeded after state store closure")
	}
	m.cancel()
	m.schedule(state.Job{ID: "cancelled"})
	m.scheduleDelayed(state.Job{ID: "cancelled"}, time.Millisecond)
	m.Close()
	m.resizeWorkers(1)
}

func TestScheduleLeavesOverflowForDurableRecovery(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m := &Manager{jobs: make(chan jobWork, 1), ctx: ctx, cancel: cancel}
	m.schedule(state.Job{ID: "first"})
	m.schedule(state.Job{ID: "overflow"})
	if queued := <-m.jobs; queued.id != "first" {
		t.Fatalf("queued work=%+v", queued)
	}
}
