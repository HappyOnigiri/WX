package daemon

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
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
	"github.com/HappyOnigiri/WX/internal/hookconfig"
	"github.com/HappyOnigiri/WX/internal/state"
	"github.com/HappyOnigiri/WX/internal/workspace"
)

func testManager(t *testing.T, cfg config.Config, store *state.Store) *Manager {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	root, err := config.ExpandHome(cfg.Storage.WorktreeRoot)
	if err != nil {
		t.Fatal(err)
	}
	m := &Manager{
		cfg:     cfg,
		store:   store,
		git:     &gitx.Runner{Timeout: time.Second},
		log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		started: time.Now(),
		roots:   map[string]bool{filepath.Clean(root): true},
		rootIDs: map[string]string{},
		jobs:    make(chan jobWork, 4),
		ctx:     ctx,
		cancel:  cancel,
	}
	_, _ = tryRegisterTestRoot(m, filepath.Clean(root))
	return m
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func TestScheduleDropsWorkWhenCanceledQueueIsFull(t *testing.T) {
	t.Parallel()
	store, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	m := testManager(t, config.Defaults(), store)
	defer m.Close()
	for index := 0; index < cap(m.jobs); index++ {
		m.jobs <- jobWork{id: fmt.Sprintf("queued-%d", index)}
	}
	m.cancel()
	m.schedule(state.Job{ID: "dropped"})
	if got := len(m.jobs); got != cap(m.jobs) {
		t.Fatalf("canceled schedule changed queue length=%d, want %d", got, cap(m.jobs))
	}
}

func TestNewPreparerKeepsRetiredRootForInFlightSlot(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	oldRoot := filepath.Join(base, "old-root")
	newRoot := filepath.Join(base, "new-root")
	if err := os.Mkdir(oldRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(newRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := config.Defaults()
	cfg.Storage.WorktreeRoot = newRoot
	m := &Manager{
		cfg:   cfg,
		roots: map[string]bool{oldRoot: false, newRoot: true},
	}
	t.Cleanup(m.Close)
	if _, release, err := m.existingRootDescriptor(oldRoot); err != nil {
		t.Fatal(err)
	} else {
		t.Cleanup(release)
	}

	preparer := m.newPreparer(cfg, state.Slot{RootID: "root-1", RelPath: filepath.Join("wsp001", "slt001"), Path: filepath.Join(oldRoot, "wsp001", "slt001")})
	if preparer.RootPath != oldRoot {
		t.Fatalf("in-flight slot preparer root=%q want retired root %q", preparer.RootPath, oldRoot)
	}
	if preparer.OwnedRoot == nil {
		t.Fatal("in-flight slot preparer did not retain retired root descriptor")
	}
	if _, err := preparer.OwnedRoot.Lstat("."); err != nil {
		t.Fatalf("in-flight slot preparer returned unusable root descriptor: %v", err)
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
	unknownPath := filepath.Join(newRoot, "wsp999", "orphan")
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
	if !containsString(artifacts["unknown_paths"].([]string), unknownPath) || len(artifacts["missing_paths"].([]string)) == 0 || len(artifacts["unknown_refs"].([]string)) != 1 || len(artifacts["missing_refs"].([]string)) != 2 {
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
	registerTestWorkspace(t, store, discovery.Workspace{Root: discoveryPath(brokenRoot), Kind: "repository", Repositories: []discovery.Repository{{ID: "broken-repository", MainPath: discoveryPath(brokenRepository), CommonDir: discoveryPath(filepath.Join(brokenRepository, ".git")), DefaultBranch: "main"}}})
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

func TestRootForPathChoosesMostSpecificOverlappingRoot(t *testing.T) {
	t.Parallel()
	parent := t.TempDir()
	outer := filepath.Join(parent, "outer")
	inner := filepath.Join(outer, "inner")
	path := filepath.Join(inner, "workspace", "slot")
	m := &Manager{roots: map[string]bool{outer: true, inner: true}}

	for range 100 {
		got, ok := m.rootForPath(path)
		if !ok || got != inner {
			t.Fatalf("rootForPath(%q)=%q,%v want nested root %q", path, got, ok, inner)
		}
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
	executable, err := hookconfig.CurrentExecutable()
	if err != nil {
		t.Fatal(err)
	}
	writeHooks := func(t *testing.T, path, document string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(document), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	canonical := func(event string) string {
		return fmt.Sprintf(`{"type":"command","command":%q}`, executable+" hook "+event)
	}
	valid := fmt.Sprintf(`{"hooks":{"SessionStart":[{"hooks":[%s]}],"UserPromptSubmit":[{"hooks":[%s]}],"PreToolUse":[{"hooks":[%s]}]}}`, canonical("session-start"), canonical("user-prompt-submit"), canonical("pre-tool-use"))
	claudePath := filepath.Join(home, ".claude", "settings.json")
	codexPath := filepath.Join(home, ".codex", "hooks.json")
	for _, test := range []struct {
		name     string
		document string
		want     bool
	}{
		{name: "valid canonical hooks", document: valid, want: true},
		{name: "matcher applies to one event", document: strings.Replace(valid, `[{"hooks":[`, `[{"matcher":"startup","hooks":[`, 1), want: false},
		{name: "disable all hooks", document: strings.Replace(valid, `{"hooks":`, `{"disableAllHooks":true,"hooks":`, 1), want: false},
		{name: "wrong executable", document: strings.ReplaceAll(valid, executable, filepath.Join(home, "other-wx")), want: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			other := filepath.Join(home, "other-wx")
			if test.name == "wrong executable" {
				if err := os.WriteFile(other, []byte("#!/bin/sh\n"), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			writeHooks(t, claudePath, test.document)
			if got := hookconfig.Available("claude"); got != test.want {
				t.Fatalf("claude diagnostic=%v, want %v", got, test.want)
			}
		})
	}
	writeHooks(t, claudePath, valid)
	localPath := filepath.Join(home, ".claude", "settings.local.json")
	writeHooks(t, localPath, strings.Replace(valid, `[{"hooks":[`, `[{"disabled":true,"hooks":[`, 1))
	if hookconfig.Available("claude") {
		t.Fatal("invalid local Claude hooks were ignored in favor of global hooks")
	}
	if err := os.Remove(localPath); err != nil {
		t.Fatal(err)
	}
	if !hookconfig.Available("claude") {
		t.Fatal("valid global Claude hooks were not detected")
	}

	for _, separator := range []string{"\n", "\r"} {
		document := fmt.Sprintf(`{"hooks":{"SessionStart":[{"hooks":[{"type":"command","command":%q}]}],"UserPromptSubmit":[{"hooks":[%s]}],"PreToolUse":[{"hooks":[%s]}]}}`, executable+separator+"hook session-start", canonical("user-prompt-submit"), canonical("pre-tool-use"))
		writeHooks(t, codexPath, document)
		if hookconfig.Available("codex") {
			t.Fatalf("shell separator %q was accepted", separator)
		}
	}
	wrapper := fmt.Sprintf(`{"hooks":{"SessionStart":[{"hooks":[{"type":"command","command":%q}]}],"UserPromptSubmit":[{"hooks":[%s]}],"PreToolUse":[{"hooks":[%s]}]}}`, "sh -c '"+executable+" hook session-start'", canonical("user-prompt-submit"), canonical("pre-tool-use"))
	writeHooks(t, codexPath, wrapper)
	if hookconfig.Available("codex") {
		t.Fatal("shell wrapper was accepted as a canonical readiness hook")
	}
	writeHooks(t, codexPath, valid)
	if !hookconfig.Available("codex") {
		t.Fatal("valid canonical Codex hooks were not detected")
	}
}

// Readiness.Timeoutを20msに縮めた状態で終端エラーの即時返却を確かめるため、直列で実行する。
// 並列実行の負荷では待機が先に切れ、context.DeadlineExceededに化ける。
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
	if err := m.PrepareFreshResume(ctx, lease.SessionID, lease.Token, "new-agent", "", nil); err == nil {
		t.Fatal("UNBOUND slot unexpectedly accepted fresh resume")
	}
	if err := store.SetSlotState(ctx, lease.SessionID, []string{"FAILED"}, "QUARANTINED", "test"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := m.waitForSnapshot(ctx, lease.SessionID); err == nil || !strings.Contains(err.Error(), "archive failed") {
		t.Fatalf("quarantined archive wait error=%v", err)
	}
	if err := m.WaitReady(ctx, lease.SessionID, lease.Token); err == nil ||
		!strings.Contains(err.Error(), "failure_id=test") ||
		!strings.Contains(err.Error(), "wx status") ||
		!strings.Contains(err.Error(), "wx doctor") {
		t.Fatalf("quarantined slot readiness error missing failure id or diagnostic guidance: %v", err)
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
	w := registerTestWorkspace(t, store, discovery.Workspace{Root: discoveryPath(root), Kind: "repository"})
	pending := state.Session{ID: "pending", WorkspaceID: string(w.ID), SlotID: "pending", State: "RELEASING", AgentKind: "codex", TokenHash: state.HashToken("pending")}
	if _, err := store.CreateSlotSession(ctx, testSlotRow(t, m, string(w.ID), "pending", 1, "DRAINING"), nil, pending, ""); err != nil {
		t.Fatal(err)
	}
	cancelled, cancelWaiting := context.WithCancel(ctx)
	cancelWaiting()
	if _, err := m.Resume(cancelled, "pending", "codex", os.Getpid(), false); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled pending resume error=%v", err)
	}
	expired := state.Session{ID: "expired", SlotID: "expired", State: "EXPIRED", AgentKind: "codex", TokenHash: state.HashToken("expired")}
	if _, err := store.CreateSlotSession(ctx, testSlotRow(t, m, "", "expired", 0, "SNAPSHOTTED"), nil, expired, ""); err != nil {
		t.Fatal(err)
	}
	if _, _, err := m.waitForSnapshot(ctx, "expired"); err == nil || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("expired archive wait error=%v", err)
	}
	waiting := state.Session{ID: "waiting", SlotID: "waiting", State: "ACTIVE", AgentKind: "codex", TokenHash: state.HashToken("waiting")}
	if _, err := store.CreateSlotSession(ctx, testSlotRow(t, m, "", "waiting", 0, "LEASED"), nil, waiting, ""); err != nil {
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

func TestWaitReadyIncludesPrepareDiagnosticMetadata(t *testing.T) {
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
	lease, err := m.AllocateResumeSlot(context.Background(), "codex", os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	detailPath := filepath.Join(root, "prepare-failure.log")
	detail := "failure_id: prepare-diagnostic-id\nexit_code: 17\ntimed_out: false\ncanceled: false\nstderr:\n[stderr] unique prepare cause\n"
	if err := os.WriteFile(detailPath, []byte(detail), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := store.SetSlotStateWithDetail(context.Background(), lease.SessionID, []string{"UNBOUND"}, "FAILED", "PREPARE_FAILED", detailPath); err != nil {
		t.Fatal(err)
	}
	err = m.WaitReady(context.Background(), lease.SessionID, lease.Token)
	for _, want := range []string{"failure_id=prepare-diagnostic-id", "detail_path=" + detailPath, "exit_code=17", "timed_out=false", "canceled=false"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("readiness error=%v missing %q", err, want)
		}
	}
}

func TestRemoveEmptySlotRejectsDescendantSymlinkSwap(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	worktreeRoot := filepath.Join(root, "worktrees")
	if err := os.MkdirAll(filepath.Join(worktreeRoot, unboundNamespace), 0o700); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	outsideFile := filepath.Join(outside, "keep")
	if err := os.WriteFile(outsideFile, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	slotPath := filepath.Join(worktreeRoot, unboundNamespace, "slt001")
	if err := os.Symlink(outside, slotPath); err != nil {
		t.Fatal(err)
	}
	store, err := state.Open(filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cfg := config.Defaults()
	cfg.Storage.WorktreeRoot = worktreeRoot
	manager := testManager(t, cfg, store)
	t.Cleanup(manager.Close)
	slot := slotAtPath(t, manager, "", "slt001", slotPath, 0, "REMOVING")
	if _, err := store.CreateSlotSession(context.Background(), slot, nil, state.Session{ID: "slt001", SlotID: "slt001", State: "EXPIRED", AgentKind: "codex", TokenHash: state.HashToken("token")}, ""); err != nil {
		t.Fatal(err)
	}
	if err := manager.removeSlotWorktrees(context.Background(), archive.Manager{}, worktreeRoot, slot, ""); err == nil {
		t.Fatal("symlinked empty slot was removed")
	}
	if data, err := os.ReadFile(outsideFile); err != nil || string(data) != "keep" {
		t.Fatalf("outside file changed: data=%q err=%v", data, err)
	}
}

func TestSessionEndWaitsForForegroundClientExit(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	store, err := state.Open(filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	if _, err := store.CreateSlotSession(ctx, storeSlotAt(t, store, root, "", "live", filepath.Join(root, "live"), 0, "LEASED"), nil, state.Session{ID: "live", SlotID: "live", State: "ACTIVE", AgentKind: "codex", ClientPID: os.Getpid(), TokenHash: state.HashToken("token")}, ""); err != nil {
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
	t.Parallel()
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
	w := discovery.Workspace{Root: discoveryPath(root), Kind: "repository", Repositories: []discovery.Repository{{ID: "repository", MainPath: discoveryPath(filepath.Join(root, "repository")), CommonDir: discoveryPath(filepath.Join(root, "repository", ".git")), DefaultBranch: "main"}}}
	w = registerTestWorkspace(t, store, w)

	readyJob, err := store.CreateStandby(ctx, slotAtPath(t, m, string(w.ID), "ready", filepath.Join(cfg.Storage.WorktreeRoot, "ready"), 1, "PREPARING"), nil)
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

	coldRepo := state.SlotRepository{RepositoryID: "repository", DirName: "repo", State: "COLD"}
	if _, err := store.CreateStandby(ctx, slotAtPath(t, m, string(w.ID), "cold", filepath.Join(cfg.Storage.WorktreeRoot, "cold"), 1, "PREPARING"), []state.SlotRepository{coldRepo}); err != nil {
		t.Fatal(err)
	}
	if err := m.removeColdRepositoryJob(ctx, state.Job{SlotID: "cold", RepositoryID: "repository"}); err != nil {
		t.Fatalf("COLD removal replay: %v", err)
	}
	if err := m.removeColdRepositoryJob(ctx, state.Job{SlotID: "cold", RepositoryID: "missing"}); err == nil {
		t.Fatal("missing cold repository removal succeeded")
	}
	if err := m.removeSlotWorktrees(ctx, archive.Manager{}, filepath.Join(root, "owned"), state.Slot{ID: "cold", Path: filepath.Join(root, "outside")}, ""); err == nil {
		t.Fatal("outside slot worktree removal succeeded")
	}
	if snapshotsUsable([]state.Snapshot{{ExpiresAt: "invalid"}}, time.Now()) {
		t.Fatal("invalid snapshot expiry was usable")
	}
}

func TestManagerResumeAndArchiveFailureStates(t *testing.T) {
	t.Parallel()
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
	w = registerTestWorkspace(t, store, w)
	createSession := func(id, sessionState, slotState, parent string) (state.Session, string) {
		t.Helper()
		token := id + "-token"
		session := state.Session{ID: id, WorkspaceID: string(w.ID), SlotID: id, ParentSessionID: parent, State: sessionState, AgentKind: "codex", TokenHash: state.HashToken(token)}
		if sessionState == "UNBOUND" {
			session.WorkspaceID = ""
		}
		var repositories []state.SlotRepository
		if session.WorkspaceID != "" && parent == "" {
			repositories = []state.SlotRepository{
				{RepositoryID: "repository-1", DirName: "repository-1", State: slotState},
				{RepositoryID: "repository-2", DirName: "repository-2", State: slotState},
			}
		}
		if _, err := store.CreateSlotSession(ctx, slotAtPath(t, m, session.WorkspaceID, id, filepath.Join(cfg.Storage.WorktreeRoot, id), 1, slotState), repositories, session, ""); err != nil {
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
	if err := m.resumeRestoreJob(ctx, incompleteChild.ID); err == nil || !strings.Contains(err.Error(), "expired or incomplete") {
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

func ensureOwnershipMarkerForTest(t *testing.T, root, target string, identity workspace.MarkerIdentity, commonDir string) {
	t.Helper()
	owner, _, err := domain.OpenOwnedRoot(root, root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = owner.Close() }()
	if err := workspace.EnsureOwnershipMarkerAt(owner, root, target, identity, commonDir); err != nil {
		t.Fatal(err)
	}
}

// Readiness.Timeoutを30msに縮めた終端状態の検査を含むため、直列で実行する。
// 理由はTestManagerReadinessAndRecoveryFailurePathsと同じである。
func TestWaitForSnapshotReportsTerminalAndTimeoutStates(t *testing.T) {
	newFixture := func(t *testing.T, sessionState, slotState string) (*Manager, *state.Store, state.Session) {
		t.Helper()
		root := t.TempDir()
		store, err := state.Open(filepath.Join(root, "state.db"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = store.Close() })
		cfg := config.Defaults()
		cfg.Storage.WorktreeRoot = filepath.Join(root, "worktrees")
		cfg.Readiness.Timeout.Duration = 30 * time.Millisecond
		manager := testManager(t, cfg, store)
		t.Cleanup(manager.Close)
		workspaceRecord := discovery.Workspace{ID: "workspace", Root: discoveryPath(root), Kind: "repository"}
		workspaceRecord = registerTestWorkspace(t, store, workspaceRecord)
		session := state.Session{ID: "session", WorkspaceID: string(workspaceRecord.ID), SlotID: "slot", State: sessionState, AgentKind: "codex", TokenHash: state.HashToken("token")}
		if _, err := store.CreateSlotSession(context.Background(), slotAtPath(t, manager, string(workspaceRecord.ID), "slot", filepath.Join(cfg.Storage.WorktreeRoot, "slot"), 0, slotState), nil, session, ""); err != nil {
			t.Fatal(err)
		}
		return manager, store, session
	}

	t.Run("missing session", func(t *testing.T) {
		manager, _, _ := newFixture(t, "ACTIVE", "LEASED")
		if _, _, err := manager.waitForSnapshot(context.Background(), "missing"); err == nil {
			t.Fatal("missing session wait succeeded")
		}
	})
	t.Run("expired session", func(t *testing.T) {
		manager, _, session := newFixture(t, "EXPIRED", "ARCHIVED")
		if _, _, err := manager.waitForSnapshot(context.Background(), session.ID); err == nil || !strings.Contains(err.Error(), "expired") {
			t.Fatalf("expired wait error=%v", err)
		}
	})
	t.Run("failed slot", func(t *testing.T) {
		manager, _, session := newFixture(t, "RELEASING", "FAILED")
		if _, _, err := manager.waitForSnapshot(context.Background(), session.ID); err == nil || !strings.Contains(err.Error(), "FAILED") {
			t.Fatalf("failed slot wait error=%v", err)
		}
	})
	t.Run("archived without membership", func(t *testing.T) {
		manager, _, session := newFixture(t, "ARCHIVED", "SNAPSHOTTED")
		if _, _, err := manager.waitForSnapshot(context.Background(), session.ID); err == nil || !strings.Contains(err.Error(), "no recorded repository") {
			t.Fatalf("membership wait error=%v", err)
		}
	})
	t.Run("deadline", func(t *testing.T) {
		manager, _, session := newFixture(t, "ACTIVE", "LEASED")
		if _, _, err := manager.waitForSnapshot(context.Background(), session.ID); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("deadline wait error=%v", err)
		}
	})
}

func TestGCRefusesIncompleteMultiRepositoryRecoveryMetadata(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name              string
		kind              string
		historicalRepos   bool
		workspaceSnapshot string
	}{
		{name: "missing historical membership", kind: "repository"},
		{name: "missing workspace snapshot", kind: "multi_repository", historicalRepos: true},
		{name: "workspace snapshot outside roots", kind: "multi_repository", historicalRepos: true, workspaceSnapshot: "outside"},
		{name: "missing workspace snapshot artifact", kind: "multi_repository", historicalRepos: true, workspaceSnapshot: "missing"},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			store, err := state.Open(filepath.Join(root, "state.db"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = store.Close() })
			cfg := config.Defaults()
			cfg.Storage.WorktreeRoot = filepath.Join(root, "worktrees")
			manager := testManager(t, cfg, store)
			t.Cleanup(manager.Close)
			ctx := context.Background()
			repository := discovery.Repository{ID: "repository", MainPath: discoveryPath(filepath.Join(root, "repository")), CommonDir: discoveryPath(filepath.Join(root, "repository", ".git")), DefaultBranch: "main"}
			workspaceRecord := discovery.Workspace{Root: discoveryPath(root), Kind: test.kind, Repositories: []discovery.Repository{repository}}
			workspaceRecord = registerTestWorkspace(t, store, workspaceRecord)
			var slotRepositories []state.SlotRepository
			if test.historicalRepos {
				slotRepositories = []state.SlotRepository{{RepositoryID: "repository", DirName: "repository", State: "ARCHIVED"}}
			}
			session := state.Session{ID: "session", WorkspaceID: string(workspaceRecord.ID), SlotID: "slot", State: "ARCHIVED", AgentKind: "codex", TokenHash: state.HashToken("token")}
			slot := testSlotRow(t, manager, string(workspaceRecord.ID), "slot", 0, "SNAPSHOTTED")
			if _, err := store.CreateSlotSession(ctx, slot, slotRepositories, session, ""); err != nil {
				t.Fatal(err)
			}
			expired := state.FormatTime(time.Now().Add(-time.Hour))
			if err := store.SaveSnapshot(ctx, state.Snapshot{ID: "snapshot", SessionID: session.ID, RepositoryID: "repository", HeadOID: "head", HeadRef: "refs/wx/recovery/head", IndexTreeOID: "index", WorktreeOID: "worktree", WorktreeRef: "refs/wx/recovery/worktree", Status: "ARCHIVED", CreatedAt: expired, ExpiresAt: expired}); err != nil {
				t.Fatal(err)
			}
			if err := store.SetSlotState(ctx, session.SlotID, []string{"SNAPSHOTTED"}, "ARCHIVED", ""); err != nil {
				t.Fatal(err)
			}
			if test.workspaceSnapshot != "" {
				relPath := filepath.Join("..", "outside", "snapshot.tar")
				if test.workspaceSnapshot == "missing" {
					relPath = filepath.Join("_recovery", "workspace-snapshots", "missing.tar")
				}
				if err := store.SaveWorkspaceSnapshot(ctx, state.WorkspaceSnapshot{SessionID: session.ID, RootID: slot.RootID, RelPath: relPath, SHA256: strings.Repeat("a", 64), Status: "ARCHIVED", CreatedAt: expired, ExpiresAt: expired}); err != nil {
					t.Fatal(err)
				}
			}
			result, err := manager.GC(ctx, false)
			if err == nil && result.Pending == 0 && result.Failed == 0 {
				t.Fatalf("unsafe recovery metadata was reported as complete: result=%+v", result)
			}
			if len(result.Reasons) == 0 {
				t.Fatalf("incomplete recovery metadata lost its reason: result=%+v err=%v", result, err)
			}
			if snapshots, err := store.Snapshots(ctx, session.ID); err != nil || len(snapshots) != 1 {
				t.Fatalf("recovery metadata was discarded: snapshots=%+v err=%v", snapshots, err)
			}
		})
	}
}

func TestRemoveSlotWorktreesRefusesIncompleteRecoveryMetadata(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name              string
		kind              string
		workspaceSnapshot string
	}{
		{name: "missing repository snapshot", kind: "repository"},
		{name: "missing workspace snapshot", kind: "multi_repository"},
		{name: "workspace snapshot outside roots", kind: "multi_repository", workspaceSnapshot: "outside"},
		{name: "missing workspace snapshot artifact", kind: "multi_repository", workspaceSnapshot: "missing"},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			store, err := state.Open(filepath.Join(root, "state.db"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = store.Close() })
			cfg := config.Defaults()
			cfg.Storage.WorktreeRoot = filepath.Join(root, "worktrees")
			manager := testManager(t, cfg, store)
			t.Cleanup(manager.Close)
			ctx := context.Background()
			repository := discovery.Repository{ID: "repository", MainPath: discoveryPath(filepath.Join(root, "repository")), CommonDir: discoveryPath(filepath.Join(root, "repository", ".git")), DefaultBranch: "main"}
			workspaceRecord := discovery.Workspace{ID: "workspace", Root: discoveryPath(root), Kind: test.kind, Repositories: []discovery.Repository{repository}}
			registered, _, err := store.UpsertWorkspaceGeneration(ctx, workspaceRecord)
			if err != nil {
				t.Fatal(err)
			}
			workspaceID := string(registered.ID)
			slot := testSlot(t, manager, workspaceID, "slot", 1, "SNAPSHOTTED")
			session := state.Session{ID: "session", WorkspaceID: workspaceID, SlotID: "slot", State: "ARCHIVED", AgentKind: "codex", TokenHash: state.HashToken("token")}
			slotRepositories := []state.SlotRepository{{RepositoryID: "repository", DirName: "repository", State: "ARCHIVED"}}
			if _, err := store.CreateSlotSession(ctx, slot, slotRepositories, session, ""); err != nil {
				t.Fatal(err)
			}
			if test.workspaceSnapshot != "" {
				rootID := slot.RootID
				relPath := filepath.Join("..", "outside", "snapshot.tar")
				if test.workspaceSnapshot == "missing" {
					relPath = filepath.Join("_recovery", "workspace-snapshots", "missing.tar")
				}
				if err := store.SaveWorkspaceSnapshot(ctx, state.WorkspaceSnapshot{SessionID: session.ID, RootID: rootID, RelPath: relPath, SHA256: strings.Repeat("a", 64), Status: "ARCHIVED", CreatedAt: state.FormatTime(time.Now()), ExpiresAt: state.FormatTime(time.Now().Add(time.Hour))}); err != nil {
					t.Fatal(err)
				}
			}
			err = manager.removeSlotWorktrees(ctx, archive.Manager{}, cfg.Storage.WorktreeRoot, slot, session.ID)
			if err == nil {
				t.Fatal("worktree removal with incomplete recovery metadata succeeded")
			}
		})
	}
}

func TestSnapshotSessionFailsClosedAfterRepositorySnapshotWhenWorkspaceRootIsUnsafe(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name string
		mode string
	}{
		{name: "bundle outside known roots", mode: "outside"},
		{name: "existing root snapshot artifact missing", mode: "missing-artifact"},
		{name: "unsupported root filesystem entry", mode: "unsupported-entry"},
	} {
		t.Run(test.name, func(t *testing.T) {
			root, err := os.MkdirTemp("/private/tmp", "wx-snapshot-edge-")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.RemoveAll(root) })
			store, err := state.Open(filepath.Join(root, "state.db"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = store.Close() })
			cfg := config.Defaults()
			cfg.Storage.WorktreeRoot = filepath.Join(root, "owned")
			bundleRoot := filepath.Join(cfg.Storage.WorktreeRoot, "slot")
			if test.mode == "outside" {
				bundleRoot = filepath.Join(root, "outside", "slot")
			}
			repositoryPath := filepath.Join(bundleRoot, "repository")
			initGitRepo(t, repositoryPath)
			manager := testManager(t, cfg, store)
			t.Cleanup(manager.Close)
			ctx := context.Background()
			commonDir := gitOutput(t, repositoryPath, "rev-parse", "--path-format=absolute", "--git-common-dir")
			repository := discovery.Repository{ID: "repository", MainPath: discoveryPath(repositoryPath), CommonDir: discoveryPath(commonDir), RelativePath: "repository", DefaultBranch: "main"}
			workspaceRecord := discovery.Workspace{ID: "workspace", Root: discoveryPath(root), Kind: "multi_repository", Repositories: []discovery.Repository{repository}}
			registered, _, err := store.UpsertWorkspaceGeneration(ctx, workspaceRecord)
			if err != nil {
				t.Fatal(err)
			}
			workspaceID := string(registered.ID)
			session := state.Session{ID: "session", WorkspaceID: workspaceID, SlotID: "slot", State: "ACTIVE", AgentKind: "codex", TokenHash: state.HashToken("token")}
			slotRepository := state.SlotRepository{RepositoryID: "repository", DirName: "repository", State: "LEASED", BaseOID: gitOutput(t, repositoryPath, "rev-parse", "HEAD")}
			var bundleSlot state.Slot
			if test.mode == "outside" {
				if _, inside := relativeWithinRoot(cfg.Storage.WorktreeRoot, bundleRoot); inside {
					t.Fatalf("bundle path %s is inside root %s; the case under test no longer applies", bundleRoot, cfg.Storage.WorktreeRoot)
				}
				escaping, relErr := filepath.Rel(cfg.Storage.WorktreeRoot, bundleRoot)
				if relErr != nil {
					t.Fatal(relErr)
				}
				bundleSlot = testSlotRow(t, manager, workspaceID, "slot", 1, "LEASED")
				bundleSlot.RelPath = escaping
				bundleSlot.Path = bundleRoot
			} else {
				bundleSlot = slotAtPath(t, manager, workspaceID, "slot", bundleRoot, 1, "LEASED")
			}
			if _, err := store.CreateSlotSession(ctx, bundleSlot, []state.SlotRepository{slotRepository}, session, ""); err != nil {
				t.Fatal(err)
			}
			if test.mode == "missing-artifact" {
				snapshotOwner, _, err := domain.OpenOwnedRoot(cfg.Storage.WorktreeRoot, cfg.Storage.WorktreeRoot)
				if err != nil {
					t.Fatal(err)
				}
				rootSnapshot, err := archive.SnapshotWorkspaceAt(ctx, bundleRoot, cfg.Storage.WorktreeRoot, bundleSlot.RootID, snapshotOwner, session.ID, []string{"repository"}, time.Now().Add(time.Hour))
				if closeErr := snapshotOwner.Close(); closeErr != nil {
					t.Fatal(closeErr)
				}
				if err != nil {
					t.Fatal(err)
				}
				if err := store.SaveWorkspaceSnapshot(ctx, rootSnapshot); err != nil {
					t.Fatal(err)
				}
				if err := os.Remove(rootSnapshot.ArchivePath); err != nil {
					t.Fatal(err)
				}
			}
			var listener net.Listener
			if test.mode == "unsupported-entry" {
				listener, err = net.Listen("unix", filepath.Join(bundleRoot, "unsupported.sock"))
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = listener.Close() }()
			}
			if _, changed, err := store.Release(ctx, session.ID, session.WorkspaceID, session.SlotID); err != nil || !changed {
				t.Fatalf("release changed=%v err=%v", changed, err)
			}
			released, err := store.SessionByID(ctx, session.ID)
			if err != nil {
				t.Fatal(err)
			}
			if err := manager.snapshotSession(ctx, released); err == nil {
				t.Fatal("workspace root snapshot failure was ignored")
			}
			wantSnapshots := 1
			if test.mode == "outside" {
				wantSnapshots = 0
			}
			snapshots, err := store.Snapshots(ctx, session.ID)
			if err != nil || len(snapshots) != wantSnapshots {
				t.Fatalf("repository recovery snapshot count=%d want %d: snapshots=%+v err=%v", len(snapshots), wantSnapshots, snapshots, err)
			}
			slot, err := store.Slot(ctx, session.SlotID)
			if err != nil || slot.State != "QUARANTINED" {
				t.Fatalf("unsafe root snapshot slot=%+v err=%v", slot, err)
			}
		})
	}
}

func TestWorkerStopsRetryingAfterBoundedAttempts(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	w := discovery.Workspace{Root: discoveryPath(repoPath), Kind: "repository", Repositories: []discovery.Repository{{ID: "repository", MainPath: discoveryPath(repoPath), CommonDir: discoveryPath(filepath.Join(repoPath, ".git")), DefaultBranch: "main"}}}
	w = registerTestWorkspace(t, store, w)
	slotPath := filepath.Join(cfg.Storage.WorktreeRoot, "slot")
	session := state.Session{ID: "session", WorkspaceID: string(w.ID), SlotID: "slot", State: "ACTIVE", AgentKind: "codex", TokenHash: state.HashToken("token")}
	repository := state.SlotRepository{RepositoryID: "repository", DirName: "missing", State: "LEASED", RequestedRef: "main", BaseOID: gitOutput(t, repoPath, "rev-parse", "HEAD"), Fingerprint: "fp"}
	if _, err := store.CreateSlotSession(ctx, slotAtPath(t, m, string(w.ID), "slot", slotPath, 1, "LEASED"), []state.SlotRepository{repository}, session, ""); err != nil {
		t.Fatal(err)
	}
	if _, changed, err := store.Release(ctx, "session", string(w.ID), "slot"); err != nil || !changed {
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
	t.Parallel()
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
	w := discovery.Workspace{Root: discoveryPath(root), Kind: "multi_repository"}
	w = registerTestWorkspace(t, store, w)
	slotPath := filepath.Join(cfg.Storage.WorktreeRoot, "slot")
	if err := os.MkdirAll(slotPath, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateStandby(ctx, slotAtPath(t, m, string(w.ID), "slot", slotPath, 1, "PREPARING"), nil); err != nil {
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
	restoreSession := state.Session{ID: "restore", WorkspaceID: string(w.ID), SlotID: "restore", State: "RESTORING", AgentKind: "codex", TokenHash: state.HashToken("restore")}
	if _, err := store.CreateSlotSession(ctx, slotAtPath(t, m, string(w.ID), "restore", restorePath, 1, "RESTORING"), nil, restoreSession, ""); err != nil {
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
	t.Parallel()
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

func TestOrphanReconciliationWaitsForRegisteredAgentProcess(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	databasePath := filepath.Join(root, "state.db")
	store, err := state.Open(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	cfg := config.Defaults()
	cfg.Storage.WorktreeRoot = filepath.Join(root, "worktrees")
	m := testManager(t, cfg, store)
	t.Cleanup(m.Close)
	ctx := context.Background()
	slotPath := filepath.Join(cfg.Storage.WorktreeRoot, "slot")
	if err := os.MkdirAll(slotPath, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateSlotSession(ctx, slotAtPath(t, m, "", "agent", slotPath, 0, "LEASED"), nil, state.Session{ID: "agent", SlotID: "agent", State: "ACTIVE", AgentKind: "codex", ClientPID: 99999999, TokenHash: state.HashToken("token")}, ""); err != nil {
		t.Fatal(err)
	}
	agent := exec.Command("sleep", "30")
	if err := agent.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if agent.Process != nil {
			_ = agent.Process.Kill()
		}
		_ = agent.Wait()
	})
	if err := m.RegisterAgentProcess(ctx, "agent", "token", agent.Process.Pid); err != nil {
		t.Fatal(err)
	}
	raw, err := sql.Open("sqlite", databasePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.ExecContext(ctx, `UPDATE sessions SET last_heartbeat_at=? WHERE id='agent'`, state.FormatTime(time.Now().Add(-time.Minute))); err != nil {
		_ = raw.Close()
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}
	m.reconcileOrphans(ctx)
	session, err := store.SessionByID(ctx, "agent")
	if err != nil || session.State != "ACTIVE" {
		t.Fatalf("live agent was released: session=%+v err=%v", session, err)
	}
	var pending dependencyPendingError
	if err := m.snapshotSession(ctx, session); !errors.As(err, &pending) {
		t.Fatalf("snapshot did not wait for live agent: %v", err)
	}
	if err := m.removeSlotJob(ctx, state.Job{SessionID: "agent"}); !errors.As(err, &pending) {
		t.Fatalf("removal did not wait for live agent: %v", err)
	}
	if err := m.removeSlotJob(ctx, state.Job{SessionID: "missing"}); err == nil {
		t.Fatal("removal with a missing session identity succeeded")
	}
	if err := agent.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := agent.Wait(); err == nil {
		t.Fatal("killed agent exited successfully")
	}
	agent.Process = nil
	m.reconcileOrphans(ctx)
	session, err = store.SessionByID(ctx, "agent")
	if err != nil || session.State != "RELEASING" {
		t.Fatalf("dead agent was not released: session=%+v err=%v", session, err)
	}
}

func TestWorkerDefersLiveAgentDependencyWithoutRetryConsumption(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	databasePath := filepath.Join(root, "state.db")
	store, err := state.Open(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	cfg := config.Defaults()
	cfg.Storage.WorktreeRoot = filepath.Join(root, "worktrees")
	m := testManager(t, cfg, store)
	t.Cleanup(m.Close)
	ctx := context.Background()
	job, err := store.CreateSlotSession(ctx,
		slotAtPath(t, m, "", "live-snapshot", filepath.Join(cfg.Storage.WorktreeRoot, "live-snapshot"), 0, "DRAINING"), nil,
		state.Session{ID: "live-snapshot", SlotID: "live-snapshot", State: "RELEASING", AgentKind: "codex", TokenHash: state.HashToken("token")}, "SNAPSHOT")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := sql.Open("sqlite", databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	if _, err := raw.ExecContext(ctx, `UPDATE sessions SET agent_pid=? WHERE id='live-snapshot'`, os.Getpid()); err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{})
	m.wg.Add(1)
	go m.runWorker(99, stop)
	m.schedule(job)
	waitUntil(t, 5*time.Second, func() bool {
		var count int
		return raw.QueryRowContext(ctx, `SELECT count(*) FROM events WHERE kind='job_dependency_wait' AND session_id='live-snapshot'`).Scan(&count) == nil && count == 1
	})
	jobs, err := store.RecoverJobs(ctx, false)
	if err != nil || len(jobs) != 1 || jobs[0].ID != job.ID || jobs[0].Attempt != 0 {
		t.Fatalf("dependency-bound job consumed retry budget: jobs=%+v err=%v", jobs, err)
	}
	close(stop)
}

func TestScheduleLeavesOverflowForDurableRecovery(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m := &Manager{jobs: make(chan jobWork, 1), ctx: ctx, cancel: cancel}
	m.schedule(state.Job{ID: "first"})
	m.schedule(state.Job{ID: "overflow"})
	if queued := <-m.jobs; queued.id != "first" {
		t.Fatalf("queued work=%+v", queued)
	}
}

func TestManagerHandlesUnavailableRootAndZeroLifecycleInterval(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	blockedRoot := filepath.Join(root, "blocked-root")
	if err := os.WriteFile(blockedRoot, []byte("file"), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := state.Open(filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	cfg := config.Defaults()
	cfg.Storage.WorktreeRoot = blockedRoot
	cfg.Pool.PreparationConcurrency = 0
	manager := New(cfg, store, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if len(manager.roots) != 0 {
		t.Fatalf("unavailable worktree root was registered: %v", manager.roots)
	}
	manager.Close()

	lifecycle := testManager(t, cfg, store)
	lifecycle.cfg.Discovery.ReconcileInterval.Duration = 0
	lifecycle.cancel()
	lifecycle.maintainLifecycle()
	lifecycle.Close()
}
