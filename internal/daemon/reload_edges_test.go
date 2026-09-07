package daemon

import (
	"context"
	"encoding/json"
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

func TestManagerReloadForgetAndDiagnosticErrors(t *testing.T) {
	f := manualManagerFixture(t, func(s *managerFixtureSetup) {
		t.Setenv("HOME", s.Root)
		s.Config.Storage.WorktreeRoot = filepath.Join(s.Root, "old-root")
	})
	home, cfg, store, m := f.Root, f.Config, f.Store, f.Manager
	ctx := context.Background()
	var dynamicLevel slog.LevelVar
	m.logLevel = &dynamicLevel

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
	if got := m.jobQueue.limit(jobClassInteractive); got != 3 || m.git.GetTimeout() != time.Second || dynamicLevel.Level() != slog.LevelDebug {
		t.Fatalf("dynamic reload interactive slots=%d timeout=%s level=%s", got, m.git.GetTimeout(), dynamicLevel.Level())
	}
	m.jobQueue.setInteractiveLimit(1)
	if got := m.jobQueue.limit(jobClassInteractive); got != 1 {
		t.Fatalf("interactive slot shrink left %d slots", got)
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
	missing, err := legacyLeaseFixture(m, "codex", os.Getpid())
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
	// Status は使用量を測らず lifecycle が測った値を返すため、報告内容を見る前に一度測っておく。
	m.measureRootUsage(ctx)
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
