package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/HappyOnigiri/WX/internal/config"
	"github.com/HappyOnigiri/WX/internal/diag"
	"github.com/HappyOnigiri/WX/internal/discovery"
	"github.com/HappyOnigiri/WX/internal/domain"
	"github.com/HappyOnigiri/WX/internal/launchd"
	"github.com/HappyOnigiri/WX/internal/pool"
	"github.com/HappyOnigiri/WX/internal/rpc"
	"github.com/HappyOnigiri/WX/internal/state"
	"github.com/HappyOnigiri/WX/internal/workspace"
)

func TestMutationDaemonOpsFindingBoundaries(t *testing.T) {
	t.Run("submodule refs keep equal keys in input order", func(t *testing.T) {
		findings := submoduleRefFindings([]submoduleRefIssue{
			{Kind: submoduleRefUnknown, ModuleDir: "/modules/same", Ref: "refs/wx/recovery/same", Path: "orphan"},
			{Kind: submoduleRefMissing, ModuleDir: "/modules/same", Ref: "refs/wx/recovery/same", Path: "saved", ExpiresAt: state.FormatTime(time.Now().Add(time.Hour))},
		})
		if len(findings) != 2 || findings[0].Severity != diag.SeverityInfo || findings[1].Severity != diag.SeverityProblem {
			t.Fatalf("equal-key findings=%+v, want orphan then missing", findings)
		}
	})

	t.Run("unmanaged artifact counts are signed at one", func(t *testing.T) {
		cause, messageValue := unmanagedArtifactCause([]unmanagedArtifact{{Kind: unmanagedSlotDirectory}, {Kind: unmanagedWorkspaceSnapshot}})
		if !strings.HasPrefix(cause, "1 slot directory/directories and 1 workspace snapshot archive(s)") {
			t.Fatalf("cause=%q", cause)
		}
		if messageValue.Data["Directories"] != 1 || messageValue.Data["Snapshots"] != 1 {
			t.Fatalf("message data=%v", messageValue.Data)
		}
	})

	t.Run("recovery failure details retain every state boundary", func(t *testing.T) {
		finding := recoveryFailureFinding(state.RecoveryFailure{
			JobID: "job-1", Kind: "RESTORE", SessionID: "session-1", ParentSessionID: "parent-1", SlotPath: "/slot",
			SessionState: "EXPIRED", SlotState: "QUARANTINED", FinishedAt: "2026-09-20T00:00:00Z",
			FailureCode: "RESTORE_FAILED", FailureMessage: "broken", DetailPath: "/logs/job-1.log",
		})
		for _, want := range []string{"restoring session parent-1", "session state EXPIRED", "slot state QUARANTINED", "failed at 2026-09-20T00:00:00Z"} {
			if !containsString(finding.Details, want) {
				t.Errorf("details=%v, want %q", finding.Details, want)
			}
		}
	})

	t.Run("quarantine finding includes its timestamp", func(t *testing.T) {
		finding := quarantinedSlotFinding(state.QuarantinedSlot{SlotID: "slot-1", Path: "/slot-1", UpdatedAt: "2026-09-20T00:00:00Z"})
		if !containsString(finding.Details, "quarantined at 2026-09-20T00:00:00Z") {
			t.Fatalf("quarantine details=%v", finding.Details)
		}
	})

	t.Run("backup, log, standby and query actions name their targets", func(t *testing.T) {
		if action, _ := backupDirectoryAction("/state.db.backups"); !strings.Contains(action, "/state.db.backups") {
			t.Fatalf("backup action=%q", action)
		}
		if target := os.Getenv("HOME"); target == "" {
			t.Fatal("HOME is empty")
		} else if got := daemonLogPathForDisplay(); got == "" || !strings.Contains(got, filepath.Join(target, "Library", "Logs")) {
			t.Fatalf("daemon log path=%q", got)
		}
		if lead, _ := jobFailureLead("/details/job.log"); !strings.Contains(lead, "command that failed") || !strings.Contains(lead, "/details/job.log") {
			t.Fatalf("detail lead=%q", lead)
		}
		if lead, _ := jobFailureLead(""); !strings.Contains(lead, daemonLogPathForDisplay()) {
			t.Fatalf("daemon lead=%q does not name the resolved log", lead)
		}
		finding := sqliteProblemFinding(errors.New("database is unreadable"))
		if finding.Target != statePathForDisplay() || !strings.Contains(finding.Action, finding.Target) {
			t.Fatalf("sqlite finding=%+v", finding)
		}
		if finding := standbyCheckProblem("/root", "slot-1", "", errors.New("unreadable")); finding.Target != "/root" {
			t.Fatalf("empty standby path finding=%+v", finding)
		}
		if finding := standbyCheckProblem("/root", "slot-1", "/root/slot-1", errors.New("unreadable")); finding.Target != "/root/slot-1" {
			t.Fatalf("standby path finding=%+v", finding)
		}
	})

	t.Run("state query action keeps the concrete paths", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		action, _ := stateQueryFailureAction()
		if !strings.Contains(action, filepath.Join(home, "Library", "Logs")) || !strings.Contains(action, filepath.Join(home, "Library", "Application Support", "wx", "state.db")) {
			t.Fatalf("state query action=%q", action)
		}
	})
}

func TestMutationDaemonOpsDiagnosticGatesAndArtifactRefs(t *testing.T) {
	t.Run("LFS findings distinguish a populated healthy cache from an empty check", func(t *testing.T) {
		ctx, manager, _, _, _, _ := managerCoverageFixture(t, "repository")
		noRoot := manualManagerFixture(t)
		findings := noRoot.Manager.lfsObjectFindings(ctx)
		if len(findings) != 1 || findings[0].Severity != diag.SeverityOK {
			t.Fatalf("empty registry LFS findings=%+v", findings)
		}

		// lfsObjectFindings は容量見積りを cache から読むため、source Git の内容に依存しない。
		roots, err := manager.store.WorkspaceRoots(ctx)
		if err != nil || len(roots) != 1 {
			t.Fatalf("workspace roots=%v err=%v", roots, err)
		}
		workspaceRecord, err := manager.resolveRegisteredWorkspace(ctx, roots[0], &discovery.Discoverer{Git: manager.git, Config: manager.Config()})
		if err != nil || len(workspaceRecord.Repositories) != 1 {
			t.Fatalf("registered workspace=%+v err=%v", workspaceRecord, err)
		}
		repo := workspaceRecord.Repositories[0]
		// sparse checkout は現在の選択から容量を再計算するため、cache の
		// healthy 境界を検証するこの fixture では明示的に無効化する。
		gitRun(t, string(repo.MainPath), "config", "core.sparseCheckout", "false")
		resolved, err := pool.ResolveBranches(ctx, manager.git, workspaceRecord, nil)
		if err != nil || len(resolved) != 1 {
			t.Fatalf("resolved branches=%v err=%v", resolved, err)
		}
		cfg := manager.Config()
		key := strings.Join([]string{
			string(repo.ID), resolved[0].OID,
			cfg.CopyModeForWorkspaceRepository(string(workspaceRecord.Root), repo.RelativePath, string(repo.MainPath)),
			fmt.Sprint(cfg.COWMinSizeKiBForWorkspaceRepository(string(workspaceRecord.Root), repo.RelativePath, string(repo.MainPath))),
		}, "\x00")
		manager.capacityMu.Lock()
		if manager.capacityCache == nil {
			manager.capacityCache = map[string]workspace.CapacityEstimate{}
		}
		oid := "sha256:" + strings.Repeat("a", 64)
		cacheOID := strings.TrimPrefix(oid, "sha256:")
		cachePath := filepath.Join(string(repo.CommonDir), "lfs", "objects", cacheOID[:2], cacheOID[2:4], cacheOID)
		if err := os.MkdirAll(filepath.Dir(cachePath), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(cachePath, make([]byte, 12), 0o600); err != nil {
			t.Fatal(err)
		}
		manager.capacityCache[key] = workspace.CapacityEstimate{RepositoryID: string(repo.ID), LFS: []workspace.LFSObjectInfo{{OID: oid, Size: 12, CachePath: cachePath}}, MissingLFSObjects: 0}
		manager.capacityMu.Unlock()
		findings = manager.lfsObjectFindings(ctx)
		if len(findings) != 1 || findings[0].Severity != diag.SeverityOK || !containsString(findings[0].Details, "1 LFS object(s) checked") {
			t.Fatalf("healthy LFS findings=%+v", findings)
		}
	})

	t.Run("submodule preparation honors a repository false override", func(t *testing.T) {
		disabled := false
		enabled := true
		cfg := config.Defaults()
		cfg.RepositoryDefaults.Submodules = &enabled
		cfg.Workspaces = map[string]config.Workspace{
			"/workspace": {RepositoryDefaults: config.RepositoryDefaults{Submodules: &disabled}},
		}
		w := discovery.Workspace{Root: domain.CanonicalPath("/workspace")}
		repo := discovery.Repository{MainPath: domain.CanonicalPath("/workspace"), RelativePath: "."}
		if submodulePreparationEnabled(cfg, w, repo) {
			t.Fatal("repository-level false override was ignored")
		}
	})

	t.Run("submodule artifact refs are checked when expectations exist", func(t *testing.T) {
		ctx, manager, store, workspaceRecord, resolved, _ := managerCoverageFixture(t, "repository")
		repo := resolved[0].Repository
		slot := d5SlotRow(t, manager, string(workspaceRecord.ID), "d5-submodule", "ARCHIVED")
		session := state.Session{ID: slot.ID, WorkspaceID: string(workspaceRecord.ID), SlotID: slot.ID, State: "EXPIRED", AgentKind: "codex", TokenHash: state.HashToken("token")}
		if _, err := store.CreateSlotSession(ctx, slot, nil, session, ""); err != nil {
			t.Fatal(err)
		}
		if err := store.SaveSnapshot(ctx, state.Snapshot{ID: "d5-snapshot", SessionID: slot.ID, RepositoryID: string(repo.ID), Status: "ARCHIVED", CreatedAt: state.FormatTime(time.Now()), ExpiresAt: state.FormatTime(time.Now().Add(time.Hour))}); err != nil {
			t.Fatal(err)
		}
		if err := store.ReplaceSubmoduleSnapshots(ctx, slot.ID, string(repo.ID), []state.SubmoduleSnapshot{{SessionID: slot.ID, RepositoryID: string(repo.ID), Path: "submodule", Name: "child", CapsuleOID: strings.Repeat("2", 40), CapsuleRef: "refs/wx/recovery/capsule"}}); err != nil {
			t.Fatal(err)
		}
		moduleDir := filepath.Join(string(repo.CommonDir), "modules", "child")
		if err := os.MkdirAll(moduleDir, 0o700); err != nil {
			t.Fatal(err)
		}
		if _, err := manager.git.Run(ctx, moduleDir, "init", "--bare", "."); err != nil {
			t.Fatal(err)
		}
		emptyTree, err := manager.git.RunEnvInput(ctx, moduleDir, nil, []byte{}, "mktree")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := manager.git.Run(ctx, moduleDir, "update-ref", "refs/wx/recovery/capsule", strings.TrimSpace(emptyTree.Stdout)); err != nil {
			t.Fatal(err)
		}
		report := artifactReport{}
		manager.appendSubmoduleRefIssues(ctx, &report, repo)
		if len(report.SubmoduleRefIssues) != 1 || report.SubmoduleRefIssues[0].Kind != submoduleRefMismatched {
			t.Fatalf("submodule ref report=%+v", report.SubmoduleRefIssues)
		}
	})
}

func TestMutationDaemonOpsMessageAndUsageBoundaries(t *testing.T) {
	t.Run("message accepts alternating string pairs", func(t *testing.T) {
		value := message("diag.ownership.unmanaged_cause", "first", 1, "second", 2, "third", 3)
		if len(value.Data) != 3 || value.Data["first"] != 1 || value.Data["second"] != 2 || value.Data["third"] != 3 {
			t.Fatalf("message=%+v", value)
		}
	})

	t.Run("job class names distinguish both execution pools", func(t *testing.T) {
		if jobClassInteractive.String() != "interactive" || jobClassMaintenance.String() != "maintenance" {
			t.Fatalf("class names=%q/%q", jobClassInteractive.String(), jobClassMaintenance.String())
		}
	})

	t.Run("slot usage computes exclusive bytes and sorts repositories", func(t *testing.T) {
		summary := state.SlotSummary{SlotID: "slot-1", State: "READY"}
		view := slotView(summary, map[string]slotUsageSample{
			"slot-1": {usage: workspace.SlotUsage{Files: 3, AllocatedBytes: 17, SharedBytes: 5, SharedFiles: 1, Repositories: map[string]workspace.RepositoryUsage{
				"z-repo": {Files: 1, AllocatedBytes: 11, SharedBytes: 4},
				"a-repo": {Files: 2, AllocatedBytes: 6, SharedBytes: 1},
			}}, measuredAt: time.Now()},
		})
		if view.ExclusiveBytes != 12 {
			t.Fatalf("slot exclusive bytes=%d, want 12", view.ExclusiveBytes)
		}
		if len(view.RepositoryUsage) != 2 || view.RepositoryUsage[0].Name != "a-repo" || view.RepositoryUsage[1].Name != "z-repo" {
			t.Fatalf("repository usage=%+v", view.RepositoryUsage)
		}
		if view.RepositoryUsage[0].ExclusiveBytes != 5 || view.RepositoryUsage[1].ExclusiveBytes != 7 {
			t.Fatalf("repository exclusive bytes=%+v", view.RepositoryUsage)
		}
	})
}

func TestMutationDaemonOpsDegradedRPCAndHandlerBoundaries(t *testing.T) {
	t.Run("degraded status and doctor reject malformed language payloads", func(t *testing.T) {
		h := DegradedHandler{DatabasePath: "/state.db", OpenError: errors.New("corrupt")}
		for _, method := range []string{"Status", "Doctor"} {
			if _, err := h.Handle(context.Background(), method, json.RawMessage("{")); err == nil {
				t.Fatalf("%s accepted malformed payload", method)
			}
		}
	})

	t.Run("readiness timeout zero means no timeout", func(t *testing.T) {
		ctx, manager, store, _, _, _ := managerCoverageFixture(t, "repository")
		slot := d5SlotRow(t, manager, "", "d5-ready", "READY")
		token := "d5-token"
		session := state.Session{ID: slot.ID, SlotID: slot.ID, State: "ACTIVE", AgentKind: "codex", TokenHash: state.HashToken(token)}
		if _, err := store.CreateSlotSession(ctx, slot, nil, session, ""); err != nil {
			t.Fatal(err)
		}
		result, err := (Handler{Manager: manager}).dispatch(ctx, "WaitReady", json.RawMessage(fmt.Sprintf(`{"session_id":%q,"token":%q,"timeout_ms":0}`, slot.ID, token)))
		if err != nil || result == nil {
			t.Fatalf("wait result=%v err=%v", result, err)
		}
		if ready, _ := result.(map[string]any)["ready"].(bool); !ready {
			t.Fatalf("wait result=%v", result)
		}
	})

	t.Run("liveness forwards a failed database result", func(t *testing.T) {
		ctx, manager, _, _, _, _ := managerCoverageFixture(t, "repository")
		result, handled, err := (Handler{Manager: manager}).dispatchLiveness(ctx, "Heartbeat", json.RawMessage(`{"session_id":"missing","token":"bad"}`))
		if !handled || result != nil || err == nil {
			t.Fatalf("heartbeat result=%v handled=%v err=%v", result, handled, err)
		}
	})

	t.Run("maintenance decoder rejects fields outside the method", func(t *testing.T) {
		ctx, manager, _, _, _, _ := managerCoverageFixture(t, "repository")
		_, handled, err := (Handler{Manager: manager}).dispatchWorkspaceMaintenance(ctx, "RetryStandby", json.RawMessage(`{"path":"/root","unexpected":true}`))
		if !handled || err == nil {
			t.Fatalf("maintenance decode handled=%v err=%v", handled, err)
		}
	})
}

func TestMutationDaemonOpsReadinessLoggingAndResumeRPC(t *testing.T) {
	t.Run("readiness logger handles nil manager and nil logger", func(t *testing.T) {
		mustNotPanic(t, func() { (Handler{}).logReadinessWait("session", "WaitReady") })
		mustNotPanic(t, func() { (Handler{Manager: &Manager{}}).logReadinessWait("session", "WaitReady") })
	})

	t.Run("readiness logger records early mode", func(t *testing.T) {
		var output strings.Builder
		manager := &Manager{log: slog.New(slog.NewTextHandler(&output, nil))}
		(Handler{Manager: manager}).logReadinessWait("session", "WaitEarlyReady")
		if !strings.Contains(output.String(), "wait=early") {
			t.Fatalf("readiness log=%q", output.String())
		}
	})

	t.Run("resume status accepts either complete identity form", func(t *testing.T) {
		ctx, manager, store, _, _, _ := managerCoverageFixture(t, "repository")
		slot := d5SlotRow(t, manager, "", "d5-resume", "ARCHIVED")
		session := state.Session{ID: slot.ID, SlotID: slot.ID, State: "ACTIVE", AgentKind: "codex", TokenHash: state.HashToken("token")}
		if _, err := store.CreateSlotSession(ctx, slot, nil, session, ""); err != nil {
			t.Fatal(err)
		}
		if err := store.BindAgentSession(ctx, slot.ID, "native"); err != nil {
			t.Fatal(err)
		}
		if err := store.MarkSessionState(ctx, slot.ID, []string{"ACTIVE"}, "EXPIRED"); err != nil {
			t.Fatal(err)
		}
		h := Handler{Manager: manager}
		for _, raw := range []string{`{"wx_session_id":"d5-resume"}`, `{"agent":"codex","agent_session_id":"native"}`} {
			result, err := h.resumeStatusRPC(ctx, json.RawMessage(raw))
			if err != nil {
				t.Fatalf("resume status raw=%s err=%v", raw, err)
			}
			if got, _ := result.(map[string]any)["wx_session_id"].(string); got != "d5-resume" {
				t.Fatalf("resume status raw=%s result=%v", raw, result)
			}
		}
		for _, raw := range []string{`{"wx_session_id":"d5-resume","agent":"codex"}`, `{"agent":"codex"}`, `{"agent_session_id":"native"}`} {
			if _, err := h.resumeStatusRPC(ctx, json.RawMessage(raw)); err == nil {
				t.Fatalf("invalid resume status raw=%s succeeded", raw)
			}
		}
	})
}

func TestMutationDaemonOpsRecoveredJobsAndSettlement(t *testing.T) {
	t.Run("first recovered prepare attempt does not reset a failed slot", func(t *testing.T) {
		ctx, manager, store, _, _, _ := managerCoverageFixture(t, "repository")
		slot := d5SlotRow(t, manager, "", "d5-recovered", "FAILED")
		session := state.Session{ID: slot.ID, SlotID: slot.ID, State: "EXPIRED", AgentKind: "codex", TokenHash: state.HashToken("token")}
		if _, err := store.CreateSlotSession(ctx, slot, nil, session, ""); err != nil {
			t.Fatal(err)
		}
		job := state.Job{ID: "d5-recovered-job", Kind: "PREPARE", SlotID: slot.ID, Attempt: 1}
		if err := manager.runRecoveredJob(ctx, job); err == nil {
			t.Fatal("missing workspace unexpectedly succeeded")
		}
		got, err := store.Slot(ctx, slot.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.State != "FAILED" {
			t.Fatalf("slot after first recovery attempt=%+v, want FAILED", got)
		}
	})

	t.Run("exhausted remove with a session is snapshotted, other jobs quarantine", func(t *testing.T) {
		ctx, manager, store, _, _, _ := managerCoverageFixture(t, "repository")
		removeSlot := d5SlotRow(t, manager, "", "d5-remove-exhausted", "REMOVING")
		removeSession := state.Session{ID: "d5-remove-session", SlotID: removeSlot.ID, State: "ACTIVE", AgentKind: "codex", TokenHash: state.HashToken("token")}
		if _, err := store.CreateSlotSession(ctx, removeSlot, nil, removeSession, ""); err != nil {
			t.Fatal(err)
		}
		owner := "d5-owner-remove"
		job, err := store.CreateJob(ctx, "REMOVE", "", removeSlot.ID, removeSession.ID)
		if err != nil {
			t.Fatal(err)
		}
		claimed, err := store.ClaimJob(ctx, job.ID, owner)
		if err != nil {
			t.Fatal(err)
		}
		claimed.Attempt = maxJobAttempts
		manager.settleJobAttempt(queuedJob{id: job.ID}, claimed, owner, retryableJobError{errors.New("remove failed")})
		got, err := store.Slot(ctx, removeSlot.ID)
		if err != nil || got.State != "SNAPSHOTTED" {
			t.Fatalf("remove exhausted slot=%+v err=%v", got, err)
		}

		quarantineSlot := d5SlotRow(t, manager, "", "d5-quarantine-exhausted", "PREPARING")
		quarantineSession := state.Session{ID: "d5-quarantine-session", SlotID: quarantineSlot.ID, State: "ACTIVE", AgentKind: "codex", TokenHash: state.HashToken("token")}
		if _, err := store.CreateSlotSession(ctx, quarantineSlot, nil, quarantineSession, ""); err != nil {
			t.Fatal(err)
		}
		job, err = store.CreateJob(ctx, "PREPARE", "", quarantineSlot.ID, "")
		if err != nil {
			t.Fatal(err)
		}
		owner = "d5-owner-quarantine"
		claimed, err = store.ClaimJob(ctx, job.ID, owner)
		if err != nil {
			t.Fatal(err)
		}
		claimed.Attempt = maxJobAttempts
		manager.settleJobAttempt(queuedJob{id: job.ID}, claimed, owner, retryableJobError{errors.New("prepare failed")})
		got, err = store.Slot(ctx, quarantineSlot.ID)
		if err != nil || got.State != "QUARANTINED" {
			t.Fatalf("ordinary exhausted slot=%+v err=%v", got, err)
		}
	})

	t.Run("successful DB settlement does not emit failure diagnostics", func(t *testing.T) {
		ctx, manager, store, _, _, _ := managerCoverageFixture(t, "repository")
		var output strings.Builder
		manager.log = slog.New(slog.NewTextHandler(&output, nil))
		owner := "d5-owner-success"
		job, err := store.CreateJob(ctx, "PREPARE", "", "", "")
		if err != nil {
			t.Fatal(err)
		}
		claimed, err := store.ClaimJob(ctx, job.ID, owner)
		if err != nil {
			t.Fatal(err)
		}
		manager.settleJobAttempt(queuedJob{id: job.ID}, claimed, owner, nil)
		if strings.Contains(output.String(), "finish job failed") {
			t.Fatalf("successful finish was logged as failed: %s", output.String())
		}
	})

	t.Run("dependency and retry transitions retain their delay and clean logs", func(t *testing.T) {
		ctx, manager, store, _, _, databasePath := managerCoverageFixture(t, "repository")
		var output strings.Builder
		manager.log = slog.New(slog.NewTextHandler(&output, nil))

		dependencyJob, err := store.CreateJob(ctx, "PREPARE", "", "", "")
		if err != nil {
			t.Fatal(err)
		}
		owner := "d5-owner-dependency"
		claimed, err := store.ClaimJob(ctx, dependencyJob.ID, owner)
		if err != nil {
			t.Fatal(err)
		}
		manager.settleJobAttempt(queuedJob{id: dependencyJob.ID}, claimed, owner, dependencyPendingError{errors.New("waiting")})
		if strings.Contains(output.String(), "defer dependency-bound job failed") {
			t.Fatalf("successful defer was logged as failed: %s", output.String())
		}

		retryJob, err := store.CreateJob(ctx, "PREPARE", "", "", "")
		if err != nil {
			t.Fatal(err)
		}
		owner = "d5-owner-retry"
		claimed, err = store.ClaimJob(ctx, retryJob.ID, owner)
		if err != nil {
			t.Fatal(err)
		}
		manager.settleJobAttempt(queuedJob{id: retryJob.ID}, claimed, owner, retryableJobError{errors.New("retry")})
		if strings.Contains(output.String(), "reschedule job failed") {
			t.Fatalf("successful retry was logged as failed: %s", output.String())
		}
		raw := openTestDatabase(t, databasePath)
		var notBefore string
		if err := raw.QueryRowContext(ctx, `SELECT not_before FROM jobs WHERE id=?`, retryJob.ID).Scan(&notBefore); err != nil {
			t.Fatal(err)
		}
		when, err := state.ParseTime(notBefore)
		if err != nil {
			t.Fatal(err)
		}
		if delay := time.Until(when); delay < time.Second || delay > 4*time.Second {
			t.Fatalf("retry delay=%s, want about 2s", delay)
		}
	})
}

func TestMutationDaemonOpsManagerConstructionAndReload(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	root := filepath.Join(home, "worktrees")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := config.Defaults()
	cfg.Storage.WorktreeRoot = root
	databasePath := filepath.Join(home, "state.db")
	store, err := openTestStoreAtPath(t, databasePath)
	if err != nil {
		t.Fatal(err)
	}
	var output strings.Builder
	manager := newManager(cfg, store, slog.New(slog.NewTextHandler(&output, nil)), true)
	t.Cleanup(manager.Close)
	t.Cleanup(func() { _ = store.Close() })
	if manager.git.FDHelper == "" || !manager.executableWatch {
		t.Fatalf("manager executable setup: helper=%q watch=%v", manager.git.FDHelper, manager.executableWatch)
	}
	if manager.prepareDetailDir == "" {
		t.Fatal("manager did not derive a detail log directory")
	}
	if strings.Contains(output.String(), "worktree root identity is unavailable") {
		t.Fatalf("healthy root was reported as identity-unavailable: %s", output.String())
	}

	manager.mu.Lock()
	manager.cfg.Storage.WorktreeRoot = "~/worktrees"
	manager.cfg.System.Storage.WorktreeRoot = "~/worktrees"
	manager.mu.Unlock()
	if err := config.Save(cfg); err != nil {
		t.Fatal(err)
	}
	if err := manager.reloadConfig(false); err != nil {
		t.Fatal(err)
	}
	manager.mu.RLock()
	_, literalRootRecorded := manager.roots["~/worktrees"]
	manager.mu.RUnlock()
	if literalRootRecorded {
		t.Fatal("reload recorded the unexpanded configured root")
	}
}

func TestMutationDaemonOpsLaunchdAndLockBoundaries(t *testing.T) {
	t.Setenv("XPC_SERVICE_NAME", launchd.Label)
	if launchdManagedProcess() {
		t.Fatalf("ordinary test process was treated as launchd-managed (ppid=%d)", os.Getppid())
	}
	releaseDaemonLock(nil)
}

func TestMutationDaemonOpsServeDegradedDatabase(t *testing.T) {
	home, err := os.MkdirTemp("/tmp", "wx-d5-serve-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(home) })
	t.Setenv("HOME", home)
	databaseDir := filepath.Join(home, "Library", "Application Support", "wx", "state.db")
	if err := os.MkdirAll(databaseDir, 0o700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- Serve(ctx) }()

	socket, err := config.SocketPath()
	if err != nil {
		t.Fatal(err)
	}
	client := rpc.Client{Socket: socket, Timeout: time.Second}
	var status map[string]any
	deadline := time.Now().Add(5 * time.Second)
	for {
		select {
		case serveErr := <-done:
			t.Fatalf("degraded daemon exited before answering RPC: %v", serveErr)
		default:
		}
		callErr := client.Call(context.Background(), "Status", nil, &status)
		if callErr == nil {
			break
		}
		if !rpc.IsConnectError(callErr) {
			t.Fatal(callErr)
		}
		if time.Now().After(deadline) {
			t.Fatalf("degraded daemon did not answer on %s: %v", socket, callErr)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if degraded, _ := status["degraded"].(bool); !degraded {
		t.Fatalf("degraded status=%v", status)
	}
	var ping map[string]any
	if err := client.Call(context.Background(), "Ping", nil, &ping); err != nil {
		t.Fatal(err)
	}
	if degraded, _ := ping["degraded"].(bool); !degraded {
		t.Fatalf("degraded ping=%v", ping)
	}
	cancel()
	select {
	case serveErr := <-done:
		if serveErr != nil {
			t.Fatalf("degraded daemon stop=%v", serveErr)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("degraded daemon did not stop")
	}
}

func mustNotPanic(t *testing.T, fn func()) {
	t.Helper()
	defer func() {
		if recovered := recover(); recovered != nil {
			t.Fatalf("unexpected panic: %v", recovered)
		}
	}()
	fn()
}

func d5SlotRow(t *testing.T, manager *Manager, workspaceID, slotID, slotState string) state.Slot {
	t.Helper()
	root, rootID, err := manager.activeRoot()
	if err != nil {
		t.Fatal(err)
	}
	return state.Slot{ID: slotID, WorkspaceID: workspaceID, RootID: rootID, RelPath: filepath.Join("d5", slotID), Path: filepath.Join(root, "d5", slotID), State: slotState}
}
