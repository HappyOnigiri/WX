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

	"github.com/HappyOnigiri/WorktreeX/internal/config"
	"github.com/HappyOnigiri/WorktreeX/internal/discovery"
	"github.com/HappyOnigiri/WorktreeX/internal/pool"
	"github.com/HappyOnigiri/WorktreeX/internal/state"
)

// READY 候補の診断は件数・ID・状態を区別する。どれかを取り違えると、
// standby の退役理由と設定変更の切り分けができない。
func TestDescribeReadyMismatchDistinguishesRepositorySetAndState(t *testing.T) {
	t.Parallel()
	ctx, manager, store, workspaceRecord, resolved := leaseTwoRepositoryFixture(t)

	countMismatch := testSlot(t, manager, string(workspaceRecord.ID), "mismatch-count", 1, "READY")
	countSession := state.Session{ID: countMismatch.ID, WorkspaceID: string(workspaceRecord.ID), SlotID: countMismatch.ID, State: "ACTIVE", AgentKind: "codex", TokenHash: state.HashToken(countMismatch.ID)}
	if _, err := store.CreateSlotSession(ctx, countMismatch, []state.SlotRepository{
		leaseReadyRepositoryRow(t, manager, countMismatch, resolved[0], ""),
	}, countSession, ""); err != nil {
		t.Fatal(err)
	}
	if got := manager.describeReadyMismatch(ctx, countMismatch, resolved); got.reason != "repository_set" || got.detail != "stored=1 requested=2" {
		t.Fatalf("count mismatch=%+v, want the stored/requested counts", got)
	}

	missing := testSlot(t, manager, string(workspaceRecord.ID), "mismatch-missing", 1, "READY")
	first := leaseReadyRepositoryRow(t, manager, missing, resolved[0], "")
	second := leaseReadyRepositoryRow(t, manager, missing, resolved[1], "")
	missingSession := state.Session{ID: missing.ID, WorkspaceID: string(workspaceRecord.ID), SlotID: missing.ID, State: "ACTIVE", AgentKind: "codex", TokenHash: state.HashToken(missing.ID)}
	if _, err := store.CreateSlotSession(ctx, missing, []state.SlotRepository{first, second}, missingSession, ""); err != nil {
		t.Fatal(err)
	}
	requestedWithMissing := []pool.Resolved{resolved[0], {Repository: discovery.Repository{ID: "not-requested"}}}
	got := manager.describeReadyMismatch(ctx, missing, requestedWithMissing)
	wantDetail := "repository not-requested is not registered to the slot"
	if got.reason != "repository_set" || got.detail != wantDetail {
		t.Fatalf("missing repository mismatch=%+v, want %q", got, wantDetail)
	}

	invalidState := testSlot(t, manager, string(workspaceRecord.ID), "mismatch-state", 1, "READY")
	badState := leaseReadyRepositoryRow(t, manager, invalidState, resolved[0], "")
	badState.State = "PREPARING"
	validState := leaseReadyRepositoryRow(t, manager, invalidState, resolved[1], "")
	invalidStateSession := state.Session{ID: invalidState.ID, WorkspaceID: string(workspaceRecord.ID), SlotID: invalidState.ID, State: "ACTIVE", AgentKind: "codex", TokenHash: state.HashToken(invalidState.ID)}
	if _, err := store.CreateSlotSession(ctx, invalidState, []state.SlotRepository{badState, validState}, invalidStateSession, ""); err != nil {
		t.Fatal(err)
	}
	got = manager.describeReadyMismatch(ctx, invalidState, resolved)
	if got.reason != "repository_state" || got.detail != badState.DirName+": PREPARING" {
		t.Fatalf("invalid repository state mismatch=%+v, want repository_state", got)
	}
}

// 更新予約時は OID の不一致に、同じ判定で見つけた配置差も添える。
func TestUpdateMismatchReportsOIDAndPlacementDrift(t *testing.T) {
	t.Parallel()
	requested := pool.Resolved{Repository: discovery.Repository{ID: "repository"}, OID: "new-oid"}
	previous := []state.Placement{{RelativePath: "old", Kind: "copy"}}
	planned := []state.Placement{{RelativePath: "new", Kind: "copy"}}
	got := updateMismatch(state.SlotRepository{DirName: "server", BaseOID: "old-oid", Fingerprint: "old"}, requested, "new", previous, planned)
	if got.reason != "oid" || !strings.Contains(got.detail, "old-oid -> new-oid") || !strings.Contains(got.detail, "+new -old") {
		t.Fatalf("OID mismatch=%+v, want OID and placement drift", got)
	}

	got = updateMismatch(state.SlotRepository{DirName: "server", BaseOID: requested.OID, Fingerprint: "old"}, requested, "new", previous, planned)
	if got.reason != "fingerprint" || !strings.Contains(got.detail, "+new -old") {
		t.Fatalf("fingerprint mismatch=%+v, want placement drift", got)
	}
}

// worktree root の設定を展開できない READY 検証は、空の root へ進まず不一致として返す。
func TestReadyMatchesRejectsAnUnexpandableConfiguredRoot(t *testing.T) {
	t.Parallel()
	f := manualManagerFixture(t)
	f.Manager.cfg.Storage.WorktreeRoot = "~other-user/worktrees"
	slot := state.Slot{ID: "outside-configured-root", Path: filepath.Join(f.Root, "outside")}
	matched, err := f.Manager.readyMatches(context.Background(), slot, nil)
	if err != nil || matched {
		t.Fatalf("readyMatches matched=%v err=%v, want a quiet mismatch", matched, err)
	}
}

// client が表示する mode と daemon log の mode は、repository 個別設定を反映した同じ値である。
func TestLeaseWithReadinessLogsTheEffectiveRepositoryMode(t *testing.T) {
	t.Parallel()
	f := manualManagerFixture(t)
	f.Manager.cfg.RepositoryDefaults.Readiness.Mode = "early"
	f.Manager.cfg.Repositories["/full"] = config.Repository{Readiness: config.RepositoryReadiness{Mode: "full"}}
	var logs bytes.Buffer
	f.Manager.log = slogTextLogger(&logs)
	w := discovery.Workspace{ID: "readiness-workspace", Root: "/workspace", Kind: "repository", Repositories: []discovery.Repository{{ID: "repository", MainPath: "/full", RelativePath: "."}}}
	lease := f.Manager.withReadiness(Lease{SessionID: "readiness-session", Route: RouteReady}, w)
	if lease.ReadinessMode != "full" {
		t.Fatalf("lease readiness mode=%q, want full", lease.ReadinessMode)
	}
	if !strings.Contains(logs.String(), "readiness_mode=full") {
		t.Fatalf("readiness log=%q, want the effective full mode", logs.String())
	}
}

// slog のテスト用 sink を短く作る。標準 logger ではなく内容を直接検査する。
func slogTextLogger(buf *bytes.Buffer) *slog.Logger {
	return slog.New(slog.NewTextHandler(buf, nil))
}

func TestRepositoryWorkspaceRootForLeaseWalksNestedRelativePaths(t *testing.T) {
	t.Parallel()
	repository := discovery.Repository{MainPath: "/source/group/server", RelativePath: filepath.Join("group", "server")}
	if got := repositoryWorkspaceRootForLease(repository); got != "/source" {
		t.Fatalf("workspace root=%q, want /source", got)
	}
	for _, relative := range []string{".", ""} {
		repository.RelativePath = relative
		if got := repositoryWorkspaceRootForLease(repository); got != "/source/group/server" {
			t.Fatalf("relative=%q workspace root=%q, want repository main path", relative, got)
		}
	}
}

// 期限切れ貸出の返却失敗は、候補を黙って消費せず診断へ残す。
func TestReconcileExpiredLeasesLogsReleaseFailures(t *testing.T) {
	t.Parallel()
	f := manualManagerFixture(t)
	ctx := context.Background()
	id := "expired-release-error"
	slot := testSlot(t, f.Manager, "", id, 0, "READY")
	session := state.Session{ID: id, SlotID: id, State: "ACTIVE", AgentKind: "wx-path", LeaseKind: state.LeaseKindPath, LeaseExpiresAt: state.FormatTime(time.Now().Add(-time.Minute)), TokenHash: state.HashToken(id)}
	if _, err := f.Store.CreateSlotSession(ctx, slot, nil, session, ""); err != nil {
		t.Fatal(err)
	}
	f.Manager.reconcileExpiredLeases(ctx)
	if logs := f.logs.tail(); !strings.Contains(logs, "lease release failed") {
		t.Fatalf("reconcile logs=%q, want release failure", logs)
	}
}

func TestReconcileExpiredLeasesLogsCandidateQueryFailures(t *testing.T) {
	t.Parallel()
	f := manualManagerFixture(t)
	if err := f.Store.Close(); err != nil {
		t.Fatal(err)
	}
	f.Manager.reconcileExpiredLeases(context.Background())
	if logs := f.logs.tail(); !strings.Contains(logs, "expired lease reconciliation failed") {
		t.Fatalf("reconcile logs=%q, want candidate query failure", logs)
	}
}

func TestReconcileOrphanedChildLeasesLogsReleaseFailures(t *testing.T) {
	t.Parallel()
	f := manualManagerFixture(t)
	ctx := context.Background()
	parentID, childID := "orphan-parent-error", "orphan-child-error"
	parentSlot := testSlot(t, f.Manager, "", parentID, 0, "ARCHIVED")
	parent := state.Session{ID: parentID, SlotID: parentID, State: "EXPIRED", AgentKind: "codex", TokenHash: state.HashToken(parentID)}
	if _, err := f.Store.CreateSlotSession(ctx, parentSlot, nil, parent, ""); err != nil {
		t.Fatal(err)
	}
	childSlot := testSlot(t, f.Manager, "", childID, 0, "READY")
	child := state.Session{ID: childID, SlotID: childID, State: "ACTIVE", AgentKind: "wx-path", LeaseKind: state.LeaseKindPath, LeaseOwnerSessionID: parentID, TokenHash: state.HashToken(childID)}
	if _, err := f.Store.CreateSlotSession(ctx, childSlot, nil, child, ""); err != nil {
		t.Fatal(err)
	}
	f.Manager.releaseOrphanedChildLeases(ctx)
	if logs := f.logs.tail(); !strings.Contains(logs, "lease release failed") {
		t.Fatalf("orphan reconcile logs=%q, want release failure", logs)
	}
}

// 返却済み slot へ discard を追いかけた応答には、存在しない job ID を付けない。
func TestReleaseLeaseAlreadyRemovedDoesNotAdvertiseAnEmptyJob(t *testing.T) {
	t.Parallel()
	f := manualManagerFixture(t)
	ctx := context.Background()
	id := "discard-already-removed"
	slot := testSlot(t, f.Manager, "", id, 0, "ARCHIVED")
	session := state.Session{ID: id, SlotID: id, State: "ARCHIVED", AgentKind: "wx-path", LeaseKind: state.LeaseKindPath, TokenHash: state.HashToken(id)}
	if _, err := f.Store.CreateSlotSession(ctx, slot, nil, session, ""); err != nil {
		t.Fatal(err)
	}
	reply, err := f.Manager.ReleaseLease(ctx, id, "test", true)
	if err != nil {
		t.Fatal(err)
	}
	if reply["discard_pending"] != DiscardPendingRemoved {
		t.Fatalf("reply=%+v, want discard_pending=%s", reply, DiscardPendingRemoved)
	}
	if _, ok := reply["job_id"]; ok {
		t.Fatalf("reply=%+v, must not advertise a missing job", reply)
	}
}

func TestReleaseStatusRejectsJobsOwnedByAnotherSessionOrSlot(t *testing.T) {
	t.Parallel()
	f := manualManagerFixture(t)
	ctx := context.Background()
	ownerID, otherID := "release-status-owner", "release-status-other"
	ownerSlot := testSlot(t, f.Manager, "", ownerID, 0, "LEASED")
	otherSlot := testSlot(t, f.Manager, "", otherID, 0, "LEASED")
	for _, item := range []struct {
		slot    state.Slot
		session state.Session
	}{
		{slot: ownerSlot, session: state.Session{ID: ownerID, SlotID: ownerID, State: "ACTIVE", AgentKind: "wx-path", TokenHash: state.HashToken(ownerID)}},
		{slot: otherSlot, session: state.Session{ID: otherID, SlotID: otherID, State: "ACTIVE", AgentKind: "wx-path", TokenHash: state.HashToken(otherID)}},
	} {
		if _, err := f.Store.CreateSlotSession(ctx, item.slot, nil, item.session, ""); err != nil {
			t.Fatal(err)
		}
	}
	jobFromOtherSession, err := f.Store.CreateJob(ctx, "SNAPSHOT", "", ownerID, otherID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Manager.ReleaseStatus(ctx, ownerID, jobFromOtherSession.ID); err == nil || !strings.Contains(err.Error(), "does not belong to session") {
		t.Fatalf("session ownership error=%v, want rejection", err)
	}
	jobFromOtherSlot, err := f.Store.CreateJob(ctx, "SNAPSHOT", "", otherID, ownerID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Manager.ReleaseStatus(ctx, ownerID, jobFromOtherSlot.ID); err == nil || !strings.Contains(err.Error(), "does not belong to session") {
		t.Fatalf("slot ownership error=%v, want rejection", err)
	}
}

func TestResumeStatusMarksBothSnapshottingStatesPending(t *testing.T) {
	t.Parallel()
	f := manualManagerFixture(t)
	ctx := context.Background()
	for _, sessionState := range []string{"RELEASING", "SNAPSHOTTING"} {
		id := "resume-status-" + strings.ToLower(sessionState)
		slot := testSlot(t, f.Manager, "", id, 0, "DRAINING")
		session := state.Session{ID: id, SlotID: id, State: sessionState, AgentKind: "codex", TokenHash: state.HashToken(id)}
		if _, err := f.Store.CreateSlotSession(ctx, slot, nil, session, ""); err != nil {
			t.Fatal(err)
		}
		status, err := f.Manager.ResumeStatus(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if status["pending"] != true || status["expired"] != false {
			t.Fatalf("state=%s status=%+v, want pending=true expired=false", sessionState, status)
		}
	}
}

func TestWaitForSnapshotReturnsRecoveryValidationErrors(t *testing.T) {
	t.Parallel()
	ctx, manager, store, workspaceRecord, resolved, _ := managerCoverageFixture(t)
	manager.cfg.Readiness.Timeout.Duration = 20 * time.Millisecond
	id := "wait-snapshot-validation-error"
	slot := testSlot(t, manager, string(workspaceRecord.ID), id, 1, "SNAPSHOTTED")
	repository := resolved[0].Repository
	if _, err := store.CreateSlotSession(ctx, slot, []state.SlotRepository{{RepositoryID: string(repository.ID), DirName: testDirName(repository, manager.Config()), State: "ARCHIVED", BaseOID: resolved[0].OID}}, state.Session{ID: id, WorkspaceID: string(workspaceRecord.ID), SlotID: id, State: "ARCHIVED", AgentKind: "codex", TokenHash: state.HashToken(id)}, ""); err != nil {
		t.Fatal(err)
	}
	expires := state.FormatTime(time.Now().Add(time.Hour))
	if err := store.SaveSnapshot(ctx, state.Snapshot{ID: "wait-snapshot-validation", SessionID: id, RepositoryID: string(repository.ID), HeadOID: resolved[0].OID, HeadRef: "refs/wx/recovery/head", IndexTreeOID: "index", WorktreeOID: "worktree", WorktreeRef: "refs/wx/recovery/worktree", Status: "ARCHIVED", CreatedAt: state.FormatTime(time.Now()), ExpiresAt: expires}); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveWorkspaceSnapshot(ctx, state.WorkspaceSnapshot{SessionID: id, RootID: slot.RootID, RelPath: "../outside.tar", SHA256: strings.Repeat("a", 64), Status: "ARCHIVED", CreatedAt: state.FormatTime(time.Now()), ExpiresAt: expires}); err != nil {
		t.Fatal(err)
	}
	_, _, err := manager.waitForSnapshot(ctx, id)
	if err == nil || !strings.Contains(err.Error(), "outside known wx roots") || errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("waitForSnapshot error=%v, want the recovery path validation error", err)
	}
}

func TestRestoreSlotReturnsFinishPreparationErrors(t *testing.T) {
	t.Parallel()
	f := manualManagerFixture(t)
	ctx := context.Background()
	id := "restore-finish-error"
	slot := testSlot(t, f.Manager, "", id, 0, "RESTORING")
	session := state.Session{ID: id, SlotID: id, State: "EXPIRED", AgentKind: "codex", TokenHash: state.HashToken(id)}
	if _, err := f.Store.CreateSlotSession(ctx, slot, nil, session, ""); err != nil {
		t.Fatal(err)
	}
	w := discovery.Workspace{ID: "workspace", Root: discoveryPath(f.Root), Kind: "repository"}
	if err := f.Manager.restoreSlot(ctx, id, w, nil, nil, nil); err == nil {
		t.Fatal("restoreSlot ignored the finish-preparation error")
	}
	if got, err := f.Store.Slot(ctx, id); err != nil || got.State != "RESTORING" {
		t.Fatalf("slot=%+v err=%v, want RESTORING after failed CAS", got, err)
	}
}

func TestClaimPrepareFailureNoticeUsesUnavailableForMissingDetail(t *testing.T) {
	t.Parallel()
	f := manualManagerFixture(t)
	ctx := context.Background()
	lease, err := legacyLeaseFixture(f.Manager, "codex", 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Store.SetSlotStateWithDetail(ctx, lease.SessionID, []string{"UNBOUND"}, "LEASED", "PREPARE_FAILED", ""); err != nil {
		t.Fatal(err)
	}
	notice, err := f.Manager.ClaimPrepareFailureNotice(ctx, lease.SessionID, lease.Token)
	if err != nil || !strings.Contains(notice, "detail_path=unavailable") {
		t.Fatalf("notice=%q err=%v, want unavailable detail path", notice, err)
	}
}

func TestReleaseUnreceivedPathLeaseReleasesOnlyAnActivePathLease(t *testing.T) {
	t.Parallel()
	f := manualManagerFixture(t)
	ctx := context.Background()
	pathID, pathToken := "unreceived-path", "unreceived-path-token"
	pathSlot := testSlot(t, f.Manager, "", pathID, 0, "UNBOUND")
	pathSession := state.Session{ID: pathID, SlotID: pathID, State: "UNBOUND", AgentKind: "wx-path", LeaseKind: state.LeaseKindPath, TokenHash: state.HashToken(pathToken)}
	if _, err := f.Store.CreateSlotSession(ctx, pathSlot, nil, pathSession, ""); err != nil {
		t.Fatal(err)
	}
	if err := f.Store.SetSlotState(ctx, pathID, []string{"UNBOUND"}, "LEASED", ""); err != nil {
		t.Fatal(err)
	}
	if err := f.Store.MarkSessionState(ctx, pathID, []string{"UNBOUND"}, "ACTIVE"); err != nil {
		t.Fatal(err)
	}
	f.Manager.ReleaseUnreceivedPathLease(ctx, pathID, pathToken)
	if session, err := f.Store.SessionByID(ctx, pathID); err != nil || session.State != "RELEASING" {
		t.Fatalf("path lease session=%+v err=%v, want RELEASING", session, err)
	}

	commandID, commandToken := "unreceived-command", "unreceived-command-token"
	commandSlot := testSlot(t, f.Manager, "", commandID, 0, "UNBOUND")
	commandSession := state.Session{ID: commandID, SlotID: commandID, State: "UNBOUND", AgentKind: "wx-run", LeaseKind: state.LeaseKindCommand, TokenHash: state.HashToken(commandToken)}
	if _, err := f.Store.CreateSlotSession(ctx, commandSlot, nil, commandSession, ""); err != nil {
		t.Fatal(err)
	}
	if err := f.Store.SetSlotState(ctx, commandID, []string{"UNBOUND"}, "LEASED", ""); err != nil {
		t.Fatal(err)
	}
	if err := f.Store.MarkSessionState(ctx, commandID, []string{"UNBOUND"}, "ACTIVE"); err != nil {
		t.Fatal(err)
	}
	f.Manager.ReleaseUnreceivedPathLease(ctx, commandID, commandToken)
	if session, err := f.Store.SessionByID(ctx, commandID); err != nil || session.State != "ACTIVE" {
		t.Fatalf("non-path lease session=%+v err=%v, want ACTIVE", session, err)
	}
}

func TestWaitEarlyReadyDoesNotPassBeforeTheSecondSessionCheck(t *testing.T) {
	t.Parallel()
	f := manualManagerFixture(t)
	ctx := context.Background()
	lease, err := legacyLeaseFixture(f.Manager, "codex", 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Store.SetSlotState(ctx, lease.SessionID, []string{"UNBOUND"}, "PREPARING", ""); err != nil {
		t.Fatal(err)
	}
	if err := f.Store.MarkSessionState(ctx, lease.SessionID, []string{"UNBOUND"}, "STARTING"); err != nil {
		t.Fatal(err)
	}
	waitCtx, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
	defer cancel()
	if err := f.Manager.WaitEarlyReady(waitCtx, lease.SessionID, lease.Token); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("early readiness error=%v, want deadline while PREPARING", err)
	}
}

// Scope の高速経路は、各 DB/Git の失敗を通常探索へ読み替えず返す。
func TestRegisteredScopePropagatesSlotLookupFailure(t *testing.T) {
	t.Parallel()
	f := manualManagerFixture(t)
	ctx := context.Background()
	raw := openTestDatabase(t, f.DatabasePath)
	if _, err := raw.ExecContext(ctx, `DROP TABLE slots`); err != nil {
		raw.Close()
		t.Fatal(err)
	}
	raw.Close()
	_, _, err := f.Manager.registeredScopeWorkspace(ctx, f.Root)
	if err == nil || !strings.Contains(err.Error(), "no such table: slots") {
		t.Fatalf("registered scope error=%v, want slot lookup failure", err)
	}
}

func TestRegisteredScopePropagatesGitCommonDirectoryFailure(t *testing.T) {
	f := manualManagerFixture(t)
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte("#!/bin/sh\nprintf 'git execution failed\\n' >&2\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	_, _, err := f.Manager.registeredScopeWorkspace(context.Background(), f.Root)
	if err == nil || !strings.Contains(err.Error(), "resolve Git common directory") {
		t.Fatalf("registered scope error=%v, want Git execution failure", err)
	}
}

func TestRegisteredScopeUsesSlotMembershipWhenRepositoryIdentityMatches(t *testing.T) {
	t.Parallel()
	f := manualManagerFixture(t)
	ctx := context.Background()
	root := filepath.Join(f.Root, "scope-repository")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	initGitRepo(t, root)
	common := gitOutput(t, root, "rev-parse", "--path-format=absolute", "--git-common-dir")
	w := discovery.Workspace{ID: "scope-membership", Root: discoveryPath(root), Kind: "repository", Repositories: []discovery.Repository{{ID: "scope-repo", MainPath: discoveryPath(root), CommonDir: discoveryPath(common), RelativePath: ".", DefaultBranch: "main"}}}
	w = registerTestWorkspace(t, f.Store, w)
	slot := testSlot(t, f.Manager, string(w.ID), "scope-membership-slot", 1, "ARCHIVED")
	if _, err := f.Store.CreateStandby(ctx, slot, nil); err != nil {
		t.Fatal(err)
	}
	scope, found, err := f.Manager.registeredScopeWorkspace(ctx, slot.Path)
	if err != nil || !found || scope.ID != string(w.ID) {
		t.Fatalf("registered scope=%+v found=%t err=%v, want slot membership", scope, found, err)
	}
}

func TestRegisteredScopePropagatesMainWorktreeLookupFailure(t *testing.T) {
	t.Parallel()
	f := manualManagerFixture(t)
	ctx := context.Background()
	repo := filepath.Join(f.Root, "scope-main")
	initGitRepo(t, repo)
	discoverer := discovery.Discoverer{Git: f.Manager.git, Config: f.Manager.Config()}
	w, err := discoverer.Resolve(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	w = registerTestWorkspace(t, f.Store, w)
	_, release, err := f.Manager.git.AcquireCommonDirLock(ctx, string(w.Repositories[0].CommonDir))
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	waitCtx, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
	defer cancel()
	scope, found, err := f.Manager.registeredScopeWorkspace(waitCtx, repo)
	if !errors.Is(err, context.DeadlineExceeded) || found || scope.ID != "" {
		t.Fatalf("registered scope=%+v found=%t err=%v, want main worktree lock failure", scope, found, err)
	}
}
