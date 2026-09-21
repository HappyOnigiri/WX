package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/HappyOnigiri/WX/internal/config"
	"github.com/HappyOnigiri/WX/internal/discovery"
	"github.com/HappyOnigiri/WX/internal/domain"
	"github.com/HappyOnigiri/WX/internal/launchd"
	"github.com/HappyOnigiri/WX/internal/rpc"
	"github.com/HappyOnigiri/WX/internal/state"
)

func TestMutationAllocationReportsQuarantineFailure(t *testing.T) {
	t.Parallel()
	ctx, manager, _, workspaceRecord, resolved, databasePath := managerCoverageFixture(t, "repository")
	logs := mutationLog(t, manager)
	db := openTestDatabase(t, databasePath)
	if _, err := db.ExecContext(ctx, `CREATE TRIGGER mutation_allocation_session_failure BEFORE INSERT ON sessions BEGIN SELECT RAISE(ABORT, 'injected session failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `CREATE TRIGGER mutation_allocation_quarantine_failure BEFORE UPDATE OF state ON slots WHEN NEW.state='QUARANTINED' BEGIN SELECT RAISE(ABORT, 'injected quarantine failure'); END`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = db.Exec(`DROP TRIGGER IF EXISTS mutation_allocation_session_failure`)
		_, _ = db.Exec(`DROP TRIGGER IF EXISTS mutation_allocation_quarantine_failure`)
	})
	if _, err := manager.allocate(ctx, workspaceRecord, resolved, 1, "codex", 0, leaseAttrs{}, "STARTING", ""); err == nil {
		t.Fatal("allocation succeeded despite injected registration and quarantine failures")
	}
	if !strings.Contains(logs.tail(), "quarantine failed slot reservation failed") {
		t.Fatalf("quarantine failure was not reported: %s", logs.tail())
	}
}

func TestMutationHandlerRejectsTypedPayloadFailures(t *testing.T) {
	t.Parallel()
	ctx, manager, _, _, _, _ := managerCoverageFixture(t, "repository")
	h := Handler{Manager: manager}
	for method, raw := range map[string]string{
		"BindAgentSession": `{"session_id":"missing","token":"bad","agent_session_id":"agent"}`,
		"Doctor":           `{"language":42}`,
		"Slots":            `{"all":"yes"}`,
	} {
		result, err := h.dispatch(ctx, method, json.RawMessage(raw))
		if err == nil {
			t.Fatalf("%s result=%v accepted invalid/error payload", method, result)
		}
	}
}

func TestMutationLeaseReadinessCombinesProgressAndWorkspaceRoot(t *testing.T) {
	t.Parallel()
	cfg := config.Defaults()
	globalProgress := true
	localProgress := false
	cfg.RepositoryDefaults.Readiness.Progress = &globalProgress
	cfg.Workspaces["/workspace"] = config.Workspace{
		Repositories: map[string]config.Repository{
			"repo": {Readiness: config.RepositoryReadiness{Progress: &localProgress}},
		},
	}
	repository := discovery.Repository{MainPath: domain.CanonicalPath("/source/repo"), RelativePath: "repo"}
	if _, _, progress := leaseReadinessDetails(cfg, "/workspace", []discovery.Repository{repository}); progress {
		t.Fatal("repository progress=false override was ignored")
	}
	if _, _, progress := leaseReadinessDetails(cfg, "", []discovery.Repository{repository}); !progress {
		t.Fatal("global progress was not used when no workspace root was supplied")
	}
	cfg.RepositoryDefaults.Readiness.Progress = nil
	if _, _, progress := leaseReadinessDetails(cfg, "/workspace", nil); progress {
		t.Fatal("nil global progress unexpectedly enabled progress")
	}
}

func TestMutationHandlerCeilingPreservesDefaultAndReadinessBudget(t *testing.T) {
	t.Parallel()
	margin := readinessCeilingMargin
	defaultTimeout := rpc.DefaultMaxHandlerTimeout
	for _, test := range []struct {
		name      string
		readiness time.Duration
		want      time.Duration
	}{
		{name: "below default", readiness: defaultTimeout - margin - time.Second, want: defaultTimeout},
		{name: "at default", readiness: defaultTimeout - margin, want: defaultTimeout},
		{name: "above default", readiness: defaultTimeout - margin + time.Second, want: defaultTimeout + time.Second},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := handlerCeiling(test.readiness); got != test.want {
				t.Fatalf("handlerCeiling(%s)=%s, want %s", test.readiness, got, test.want)
			}
		})
	}
}

func TestMutationSettledExhaustedJobReportsFinishFailure(t *testing.T) {
	t.Parallel()
	ctx, manager, store, _, _, databasePath := managerCoverageFixture(t, "repository")
	logs := mutationLog(t, manager)
	job, err := store.CreateJob(ctx, "PREPARE", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := store.ClaimJob(ctx, job.ID, "mutation-finish")
	if err != nil {
		t.Fatal(err)
	}
	db := openTestDatabase(t, databasePath)
	if _, err := db.ExecContext(ctx, `CREATE TRIGGER mutation_finish_exhausted_failure BEFORE UPDATE OF state ON jobs WHEN OLD.state='RUNNING' AND NEW.state='FAILED' BEGIN SELECT RAISE(ABORT, 'injected finish failure'); END`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = db.Exec(`DROP TRIGGER IF EXISTS mutation_finish_exhausted_failure`) })
	claimed.Attempt = maxJobAttempts
	manager.settleJobAttempt(queuedJob{id: job.ID}, claimed, "mutation-finish", retryableJobError{errors.New("retry exhausted")})
	if !strings.Contains(logs.tail(), "finish exhausted job failed") {
		t.Fatalf("finish failure was not reported: %s", logs.tail())
	}
}

func TestMutationRecoveredRemoveKeepsOwnershipErrorsNonRetryableOnlyForUnownedJobs(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name      string
		sessionID string
		wantRetry bool
	}{
		{name: "unowned", wantRetry: false},
		{name: "session owned", sessionID: "remove-session", wantRetry: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, manager, store, _, _, _ := managerCoverageFixture(t, "repository")
			slot := testSlotRow(t, manager, "", "remove-ownership-"+test.name, 1, "REMOVING")
			slot.RelPath = filepath.Join("..", "outside")
			slot.Path = filepath.Join(manager.Config().Storage.WorktreeRoot, slot.RelPath)
			if test.sessionID != "" {
				session := state.Session{ID: test.sessionID, SlotID: slot.ID, State: "EXPIRED", AgentKind: "codex", TokenHash: state.HashToken(test.sessionID)}
				if _, err := store.CreateSlotSession(ctx, slot, nil, session, ""); err != nil {
					t.Fatal(err)
				}
			} else {
				// session ID のない job は利用者に紐付かない所有権なしの回収経路である。
				if _, err := store.CreateSlotSession(ctx, slot, nil, state.Session{ID: slot.ID, SlotID: slot.ID, State: "EXPIRED", AgentKind: "codex", TokenHash: state.HashToken(slot.ID)}, ""); err != nil {
					t.Fatal(err)
				}
			}
			job := state.Job{ID: "remove-job-" + test.name, Kind: "REMOVE", SlotID: slot.ID, SessionID: test.sessionID}
			err := manager.runRecoveredJob(ctx, job)
			var retry retryableJobError
			if errors.As(err, &retry) != test.wantRetry {
				t.Fatalf("runRecoveredJob retryable=%v, want %v (err=%v)", errors.As(err, &retry), test.wantRetry, err)
			}
			if !test.wantRetry && !errors.Is(err, state.ErrOwnership) {
				t.Fatalf("runRecoveredJob error=%v, want ownership error", err)
			}
		})
	}
}

func TestMutationReleaseLeaseReportsScheduledDiscardRemoval(t *testing.T) {
	t.Parallel()
	ctx, manager, store, _, _, _ := managerCoverageFixture(t, "repository")
	slot := testSlot(t, manager, "", "discard-scheduled", 1, "READY")
	session := state.Session{ID: slot.ID, SlotID: slot.ID, State: "ARCHIVED", AgentKind: "wx-path", LeaseKind: state.LeaseKindPath, TokenHash: state.HashToken("discard-token")}
	if _, err := store.CreateSlotSession(ctx, slot, nil, session, ""); err != nil {
		t.Fatal(err)
	}
	reply, err := manager.ReleaseLease(ctx, session.ID, "mutation", true)
	if err != nil {
		t.Fatal(err)
	}
	jobID, ok := reply["job_id"].(string)
	if !ok || jobID == "" || reply["job_kind"] != "REMOVE" || reply["discarded"] != true {
		t.Fatalf("discard reply=%v, want scheduled REMOVE job", reply)
	}
}

func TestMutationReleaseLeaseReportsRemovingAsAlreadyRemoved(t *testing.T) {
	t.Parallel()
	ctx, manager, store, _, _, _ := managerCoverageFixture(t, "repository")
	slot := testSlot(t, manager, "", "discard-removing", 1, "REMOVING")
	session := state.Session{ID: slot.ID, SlotID: slot.ID, State: "ARCHIVED", AgentKind: "wx-path", LeaseKind: state.LeaseKindPath, TokenHash: state.HashToken("discard-token")}
	if _, err := store.CreateSlotSession(ctx, slot, nil, session, ""); err != nil {
		t.Fatal(err)
	}
	reply, err := manager.ReleaseLease(ctx, session.ID, "mutation", true)
	if err != nil {
		t.Fatal(err)
	}
	if reply["discarded"] != false || reply["discard_pending"] != DiscardPendingRemoved {
		t.Fatalf("discard reply=%v, want already-removed", reply)
	}
}

func TestMutationQuarantineOwnershipFailureLogsStorageFailureAndNilLoggerIsSafe(t *testing.T) {
	t.Parallel()
	ctx, manager, _, workspace, _, databasePath := managerCoverageFixture(t, "repository")
	slot := testSlot(t, manager, string(workspace.ID), "quarantine-log", 1, "RETIRING")
	if _, err := manager.store.CreateStandby(ctx, slot, nil); err != nil {
		t.Fatal(err)
	}
	db := openTestDatabase(t, databasePath)
	if _, err := db.ExecContext(ctx, `CREATE TRIGGER mutation_quarantine_log_failure BEFORE UPDATE OF state ON slots WHEN NEW.state='QUARANTINED' BEGIN SELECT RAISE(ABORT, 'injected quarantine write failure'); END`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = db.Exec(`DROP TRIGGER IF EXISTS mutation_quarantine_log_failure`) })
	logs := mutationLog(t, manager)
	manager.quarantineOwnershipFailure(slot.ID, []string{"RETIRING"}, fmt.Errorf("%w: replaced root", state.ErrOwnership))
	if !strings.Contains(logs.tail(), "quarantine uncertain worktree ownership failed") {
		t.Fatalf("quarantine write failure was not logged: %s", logs.tail())
	}
	manager.log = nil
	mustNotPanic(t, func() {
		manager.quarantineOwnershipFailure(slot.ID, []string{"RETIRING"}, fmt.Errorf("%w: replaced root", state.ErrOwnership))
	})
}

func TestMutationReadyMatchesUsesConfiguredRootWhenRegistryHasNoEntry(t *testing.T) {
	t.Parallel()
	f := manualManagerFixture(t)
	root := f.Config.Storage.WorktreeRoot
	slotPath := filepath.Join(root, "unregistered", "slot")
	if err := os.MkdirAll(slotPath, 0o700); err != nil {
		t.Fatal(err)
	}
	f.Manager.mu.Lock()
	f.Manager.roots = map[string]bool{}
	f.Manager.rootIdentities = map[string]string{}
	f.Manager.mu.Unlock()
	matched, err := f.Manager.readyMatches(context.Background(), state.Slot{ID: "outside-row", Path: filepath.Join(t.TempDir(), "slot")}, nil)
	if err != nil || matched {
		t.Fatalf("outside readyMatches matched=%v err=%v, want false", matched, err)
	}
}

func TestMutationRootLeaseFallbackRejectsExpansionAndOutsidePaths(t *testing.T) {
	t.Parallel()
	f := manualManagerFixture(t)
	f.Manager.mu.Lock()
	configCopy := f.Manager.cfg
	configCopy.Storage.WorktreeRoot = "~otheruser/worktrees"
	configCopy.System.Storage.WorktreeRoot = "~otheruser/worktrees"
	f.Manager.cfg = configCopy
	f.Manager.mu.Unlock()
	if err := f.Manager.retainLease("bad-home", filepath.Join(t.TempDir(), "slot")); !errors.Is(err, state.ErrOwnership) {
		t.Fatalf("invalid configured root retainLease error=%v", err)
	}
	f.Manager.mu.Lock()
	configCopy = f.Manager.cfg
	configCopy.Storage.WorktreeRoot = filepath.Join(f.Root, "worktrees")
	configCopy.System.Storage.WorktreeRoot = configCopy.Storage.WorktreeRoot
	f.Manager.cfg = configCopy
	f.Manager.mu.Unlock()
	err := f.Manager.retainLease("outside", filepath.Join(t.TempDir(), "slot"))
	if !errors.Is(err, state.ErrOwnership) || !strings.Contains(err.Error(), "outside known wx roots") {
		t.Fatalf("outside retainLease error=%v, want an explicit ownership cause", err)
	}
}

func TestMutationWorkspaceScopePropagatesMembershipStorageFailure(t *testing.T) {
	t.Parallel()
	ctx, manager, store, workspaceRecord, resolved, databasePath := managerCoverageFixture(t, "repository")
	slot := testSlot(t, manager, string(workspaceRecord.ID), "scope-membership-error", 1, "LEASED")
	if _, err := store.CreateSlotSession(ctx, slot, []state.SlotRepository{{RepositoryID: string(resolved[0].Repository.ID), DirName: "repo", State: "LEASED"}}, state.Session{ID: slot.ID, WorkspaceID: string(workspaceRecord.ID), SlotID: slot.ID, State: "ACTIVE", AgentKind: "codex", TokenHash: state.HashToken("scope")}, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.git.Run(ctx, slot.Path, "init"); err != nil {
		t.Fatal(err)
	}
	db := openTestDatabase(t, databasePath)
	if _, err := db.ExecContext(ctx, `DROP TABLE workspace_repositories`); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.WorkspaceScope(ctx, slot.Path); err == nil {
		t.Fatal("WorkspaceScope hid a membership storage failure")
	}
}

func TestMutationReleaseUnreceivedPathLeaseLogsReleaseFailure(t *testing.T) {
	t.Parallel()
	ctx, manager, store, _, _, databasePath := managerCoverageFixture(t, "repository")
	slot := testSlot(t, manager, "", "unreceived-release-failure", 1, "LEASED")
	token := "path-token"
	session := state.Session{ID: slot.ID, SlotID: slot.ID, State: "ACTIVE", AgentKind: "wx-path", LeaseKind: state.LeaseKindPath, TokenHash: state.HashToken(token)}
	if _, err := store.CreateSlotSession(ctx, slot, nil, session, ""); err != nil {
		t.Fatal(err)
	}
	db := openTestDatabase(t, databasePath)
	if _, err := db.ExecContext(ctx, `CREATE TRIGGER mutation_unreceived_release_failure BEFORE UPDATE OF state ON sessions WHEN NEW.state='RELEASING' BEGIN SELECT RAISE(ABORT, 'injected release failure'); END`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = db.Exec(`DROP TRIGGER IF EXISTS mutation_unreceived_release_failure`) })
	logs := mutationLog(t, manager)
	manager.ReleaseUnreceivedPathLease(ctx, session.ID, token)
	if !strings.Contains(logs.tail(), "lease release failed") {
		t.Fatalf("release failure was not logged: %s", logs.tail())
	}
}

func TestMutationStatusReportsRootExclusiveBytesAgeRetentionAndOrder(t *testing.T) {
	t.Parallel()
	ctx, manager, store, workspaceRecord, _, databasePath := managerCoverageFixture(t, "repository")
	statusSlot := testSlot(t, manager, string(workspaceRecord.ID), "status-age", 1, "LEASED")
	if _, err := store.CreateSlotSession(ctx, statusSlot, nil, state.Session{ID: "status-age", WorkspaceID: string(workspaceRecord.ID), SlotID: statusSlot.ID, State: "ACTIVE", AgentKind: "codex", TokenHash: state.HashToken("status-age")}, ""); err != nil {
		t.Fatal(err)
	}
	root := filepath.Clean(manager.Config().Storage.WorktreeRoot)
	second := filepath.Join(filepath.Dir(root), "second-root")
	if err := os.MkdirAll(second, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := tryRegisterTestRoot(manager, second); err != nil {
		t.Fatal(err)
	}
	if manager.rootUsage == nil {
		manager.rootUsage = map[string]rootUsageSample{}
	}
	manager.rootUsage[root] = rootUsageSample{allocated: 17, shared: 5, unmanaged: 2, measuredAt: time.Now()}
	manager.rootUsage[second] = rootUsageSample{allocated: 8, shared: 3, unmanaged: 1, measuredAt: time.Now()}
	cfg := manager.cfg
	cfg.Retention.HotStandby.Duration = 1234 * time.Millisecond
	cfg.Retention.EndedWorktree.Duration = 2345 * time.Millisecond
	cfg.System.Retention.Quarantined.Duration = 3456 * time.Millisecond
	cfg.System.Retention.RecoverySnapshot.Duration = 4567 * time.Millisecond
	cfg.System.Retention.ExpiredSessionTombstone.Duration = 5678 * time.Millisecond
	cfg.System.Retention.FailedJob.Duration = 6789 * time.Millisecond
	cfg.System.Retention.EventLog.Duration = 7890 * time.Millisecond
	cfg.System.Lease.TTL.Duration = 8901 * time.Millisecond
	manager.cfg = cfg
	raw := openTestDatabase(t, databasePath)
	if _, err := raw.ExecContext(ctx, `UPDATE sessions SET created_at='not-a-timestamp' WHERE id=?`, "status-age"); err != nil {
		t.Fatal(err)
	}
	status, err := manager.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(status["worktree_roots"])
	if err != nil {
		t.Fatal(err)
	}
	var roots []struct {
		Path           string `json:"path"`
		ExclusiveBytes int64  `json:"exclusive_bytes"`
	}
	if err := json.Unmarshal(encoded, &roots); err != nil {
		t.Fatal(err)
	}
	if len(roots) < 2 || roots[0].Path > roots[1].Path {
		t.Fatalf("worktree roots are not path sorted: %s", encoded)
	}
	for _, item := range roots {
		if item.Path == root && item.ExclusiveBytes != 12 {
			t.Fatalf("root exclusive bytes=%d, want 12: %s", item.ExclusiveBytes, encoded)
		}
	}
	retention, ok := status["retention_seconds"].(map[string]int64)
	if !ok {
		t.Fatalf("retention payload type=%T value=%v", status["retention_seconds"], status["retention_seconds"])
	}
	for key, want := range map[string]int64{
		"hot_standby": 1, "ended_worktree": 2, "quarantined": 3, "recovery_snapshot": 4,
		"expired_session_tombstone": 5, "failed_job": 6, "event_log": 7, "lease_ttl": 8,
	} {
		if retention[key] != want {
			t.Fatalf("retention[%q]=%d, want %d", key, retention[key], want)
		}
	}
}

func TestMutationGCCountsOnlyExpiredCandidatesAndMetadata(t *testing.T) {
	t.Parallel()
	ctx, manager, store, workspaceRecord, resolved, databasePath := managerCoverageFixture(t, "repository")
	root := string(workspaceRecord.Root)
	hour := config.Duration{Duration: time.Hour}
	zero := config.Duration{}
	warm := 1
	cfg := manager.Config()
	cfg.WorkspaceDefaults.Worktree = "hot"
	cfg.WorkspaceDefaults.WarmCount = &warm
	cfg.WorkspaceDefaults.Retention.HotStandby = &zero
	cfg.WorkspaceDefaults.Retention.EndedWorktree = &zero
	cfg.Pool.WarmPerWorkspace = warm
	cfg.Retention.HotStandby = zero
	cfg.Retention.EndedWorktree = zero
	cfg.Workspaces[root] = config.Workspace{Retention: config.WorkspaceRetention{HotStandby: &hour, EndedWorktree: &hour}}
	cfg.System.Retention.Quarantined = hour
	cfg.System.Retention.FailedJob = hour
	cfg.System.Retention.EventLog = hour
	cfg.System.Retention.ExpiredSessionTombstone = hour
	manager.mu.Lock()
	manager.cfg = cfg
	manager.mu.Unlock()

	now := time.Now().UTC()
	old := state.FormatTime(now.Add(-2 * time.Hour))
	near := state.FormatTime(now.Add(-30 * time.Minute))
	repositoryID := string(resolved[0].Repository.ID)
	newSession := func(name, slotState, sessionState string, archivedAt string) state.Slot {
		t.Helper()
		slot := testSlotRow(t, manager, string(workspaceRecord.ID), "gc-count-"+name, 1, slotState)
		session := state.Session{ID: slot.ID, WorkspaceID: string(workspaceRecord.ID), SlotID: slot.ID, State: sessionState, AgentKind: "mutation", TokenHash: state.HashToken(slot.ID), ArchivedAt: archivedAt}
		if _, err := store.CreateSlotSession(ctx, slot, nil, session, ""); err != nil {
			t.Fatal(err)
		}
		return slot
	}
	ended := newSession("ended", "SNAPSHOTTED", "ARCHIVED", old)
	protected := newSession("protected", "SNAPSHOTTED", "ARCHIVED", old)
	if err := store.ReplaceUnsavedSubmodules(ctx, protected.ID, repositoryID, []state.UnsavedSubmodule{{RepositoryID: repositoryID, Path: "module", Reasons: "mutation"}}); err != nil {
		t.Fatal(err)
	}
	nearEnded := newSession("near-ended", "SNAPSHOTTED", "ARCHIVED", near)
	_ = nearEnded
	quarantine := testSlotRow(t, manager, string(workspaceRecord.ID), "gc-count-quarantine", 1, "QUARANTINED")
	if _, err := store.CreateStandby(ctx, quarantine, nil); err != nil {
		t.Fatal(err)
	}
	nearQuarantine := testSlotRow(t, manager, string(workspaceRecord.ID), "gc-count-near-quarantine", 1, "QUARANTINED")
	if _, err := store.CreateStandby(ctx, nearQuarantine, nil); err != nil {
		t.Fatal(err)
	}

	// StandbyGCCandidates は STALE を常に候補にし、READY は warm_count の 1 枠を残す。
	stale := testSlotRow(t, manager, string(workspaceRecord.ID), "gc-count-stale", 1, "STALE")
	if _, err := store.CreateStandby(ctx, stale, nil); err != nil {
		t.Fatal(err)
	}
	ready := testSlotRow(t, manager, string(workspaceRecord.ID), "gc-count-ready", 1, "READY")
	if _, err := store.CreateStandby(ctx, ready, []state.SlotRepository{{RepositoryID: repositoryID, DirName: "repository", State: "READY", RequestedRef: "main", BaseOID: resolved[0].OID, Fingerprint: "fingerprint"}}); err != nil {
		t.Fatal(err)
	}

	expired := newSession("expired", "ARCHIVED", "ARCHIVED", old)
	if err := store.SaveSnapshot(ctx, state.Snapshot{ID: "gc-count-snapshot", SessionID: expired.ID, RepositoryID: repositoryID, HeadOID: resolved[0].OID, HeadRef: "refs/wx/recovery/gc-count-head", IndexTreeOID: resolved[0].OID, WorktreeOID: resolved[0].OID, WorktreeRef: "refs/wx/recovery/gc-count-worktree", Status: "ARCHIVED", CreatedAt: old, ExpiresAt: old}); err != nil {
		t.Fatal(err)
	}

	db := openTestDatabase(t, databasePath)
	// 1時間以内の候補時刻は回収件数に含めない。
	for _, item := range []struct {
		id    string
		value string
	}{
		{id: ended.ID, value: old},
		{id: protected.ID, value: old},
		{id: nearEnded.ID, value: near},
	} {
		if _, err := db.ExecContext(ctx, `UPDATE sessions SET archived_at=? WHERE id=?`, item.value, item.id); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.ExecContext(ctx, `UPDATE slots SET updated_at=? WHERE id=?`, old, quarantine.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE slots SET updated_at=? WHERE id=?`, near, nearQuarantine.ID); err != nil {
		t.Fatal(err)
	}
	// last_leased_at は repository 行で共有されるため、最近の値で workspace 固有の
	// hot cutoff を検証する。SQL 側の下限は global TTL をゼロにして許容しておく。
	if _, err := db.ExecContext(ctx, `UPDATE repositories SET last_leased_at=? WHERE id=?`, near, repositoryID); err != nil {
		t.Fatal(err)
	}
	job, err := store.CreateJob(ctx, "PREPARE", string(workspaceRecord.ID), "", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE jobs SET state='SUCCEEDED',finished_at=? WHERE id=?`, near, job.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO events(time,level,kind,message) VALUES(?,?,?,?)`, near, "info", "mutation_gc", "near-retention metadata"); err != nil {
		t.Fatal(err)
	}
	tombstone := newSession("tombstone", "ARCHIVED", "EXPIRED", "")
	if _, err := db.ExecContext(ctx, `UPDATE sessions SET expires_at=?,agent_session_id=? WHERE id=?`, near, "agent-gc-count", tombstone.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE slots SET owner_session_id=NULL WHERE id=?`, tombstone.ID); err != nil {
		t.Fatal(err)
	}

	result, err := manager.GC(ctx, true)
	if err != nil {
		t.Fatalf("GC dry-run err=%v result=%+v", err, result)
	}
	// 古い終了済み・保護済み worktree、STALE standby、隔離 slot、期限切れ snapshot
	// session が候補になる。最近のメタデータと保持期間内の行は個別の cutoff で除外する。
	if result.Candidates != 5 || result.Pending != 5 {
		t.Fatalf("GC dry-run result=%+v, want five expired candidates and pending rows", result)
	}
}

func TestMutationDescribeReadyMismatchDistinguishesColdAndInvalidReadyWorktrees(t *testing.T) {
	t.Parallel()
	ctx, manager, store, workspaceRecord, resolved := leaseTwoRepositoryFixture(t)
	slot := testSlotRow(t, manager, string(workspaceRecord.ID), "mismatch-description", 1, "READY")
	rows := []state.SlotRepository{leaseReadyRepositoryRow(t, manager, slot, resolved[0], ""), leaseReadyRepositoryRow(t, manager, slot, resolved[1], "")}
	rows[0].State = "COLD"
	rows[1].State = "COLD"
	if _, err := store.CreateStandby(ctx, slot, rows); err != nil {
		t.Fatal(err)
	}
	if mismatch := manager.describeReadyMismatch(ctx, slot, resolved); mismatch.reason != "unknown" {
		t.Fatalf("cold mismatch=%+v, want unknown because COLD has no worktree to validate", mismatch)
	}
	rows[0].State = "READY"
	if err := store.SetSlotRepositoryState(ctx, slot.ID, rows[0].RepositoryID, []string{"COLD"}, "READY"); err != nil {
		t.Fatal(err)
	}
	if mismatch := manager.describeReadyMismatch(ctx, slot, resolved); mismatch.reason != "worktree" {
		t.Fatalf("invalid ready mismatch=%+v, want worktree", mismatch)
	}
}

func TestMutationSnapshotPropagatesMarkArchivedFailure(t *testing.T) {
	t.Parallel()
	ctx, manager, store, workspaceRecord, resolved, databasePath := managerCoverageFixture(t, "repository")
	logs := mutationLog(t, manager)
	slot := testSlot(t, manager, string(workspaceRecord.ID), "snapshot-mark-archived", 1, "LEASED")
	worktreePath := filepath.Join(slot.Path, "repository")
	gitRun(t, string(workspaceRecord.Repositories[0].MainPath), "worktree", "add", "--detach", worktreePath, resolved[0].OID)
	slotRepository := state.SlotRepository{RepositoryID: string(resolved[0].Repository.ID), DirName: "repository", WorktreePath: worktreePath, State: "LEASED", BaseOID: resolved[0].OID}
	session := state.Session{ID: slot.ID, WorkspaceID: string(workspaceRecord.ID), SlotID: slot.ID, State: "ACTIVE", AgentKind: "mutation", TokenHash: state.HashToken("snapshot-mark-archived")}
	if _, err := store.CreateSlotSession(ctx, slot, []state.SlotRepository{slotRepository}, session, ""); err != nil {
		t.Fatal(err)
	}
	if _, changed, err := store.Release(ctx, session.ID, session.WorkspaceID, session.SlotID); err != nil || !changed {
		t.Fatalf("release changed=%v err=%v", changed, err)
	}
	released, err := store.SessionByID(ctx, session.ID)
	if err != nil {
		t.Fatal(err)
	}
	db := openTestDatabase(t, databasePath)
	if _, err := db.ExecContext(ctx, `CREATE TRIGGER mutation_snapshot_mark_archived_failure BEFORE UPDATE OF state ON sessions WHEN NEW.state='ARCHIVED' BEGIN SELECT RAISE(ABORT, 'injected mark archived failure'); END`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = db.Exec(`DROP TRIGGER IF EXISTS mutation_snapshot_mark_archived_failure`) })
	if err := manager.snapshotSession(ctx, released); err == nil {
		t.Fatal("snapshotSession ignored MarkArchived failure")
	}
	if strings.Contains(logs.tail(), "submodule work could not be snapshotted") {
		t.Fatalf("empty submodule snapshot was reported as unsaved work: %s", logs.tail())
	}
	if slotAfter, err := store.Slot(ctx, slot.ID); err != nil || slotAfter.State != "SNAPSHOTTING" {
		t.Fatalf("slot after MarkArchived failure=%+v err=%v, want SNAPSHOTTING", slotAfter, err)
	}
}

func TestMutationRegisteredRemovalPropagatesContextAndCleanupErrors(t *testing.T) {
	t.Parallel()
	ctx, manager, _, _, resolved, _ := managerCoverageFixture(t, "repository")
	common := string(resolved[0].Repository.CommonDir)
	worktrees := filepath.Join(common, "worktrees")
	name := "mutation-remove-error"
	entry := filepath.Join(worktrees, name)
	target := filepath.Join(t.TempDir(), "missing")
	if err := os.MkdirAll(entry, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(entry, "gitdir"), []byte(filepath.Join(target, ".git")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(worktrees, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(worktrees, 0o700) })
	if err := manager.removeGitRegistrationLocked(ctx, common, target); err == nil {
		t.Fatal("registration cleanup hid RemoveAll failure")
	}
}

func TestMutationLaunchdAndLockBoundariesRemainExplicit(t *testing.T) {
	t.Setenv("XPC_SERVICE_NAME", launchd.Label)
	if launchdManagedProcess() {
		t.Fatalf("ordinary test process was treated as launchd-managed (ppid=%d)", os.Getppid())
	}
	releaseDaemonLock(nil)
}
