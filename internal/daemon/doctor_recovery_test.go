package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/HappyOnigiri/WX/internal/diag"
	"github.com/HappyOnigiri/WX/internal/state"
)

// 欠損の重大さは slot の状態で決める。貸出中・保存待ちの欠損だけが利用者の対処を要する。
func TestMissingArtifactFindingsSeparateNeededSlotsFromReclaimable(t *testing.T) {
	findings := missingArtifactFindings([]missingArtifact{
		{SlotID: "ready", Path: "/root/ready", State: "READY"},
		{SlotID: "leased", Path: "/root/leased", State: "LEASED"},
		{SlotID: "snapshotted", Path: "/root/snapshotted", State: "SNAPSHOTTED"},
		{SlotID: "quarantined", Path: "/root/quarantined", State: "QUARANTINED"},
	})
	severities := map[string]diag.Severity{}
	for _, finding := range findings {
		severities[finding.Target] = finding.Severity
	}
	for path, want := range map[string]diag.Severity{
		"/root/ready":       diag.SeverityInfo,
		"/root/leased":      diag.SeverityProblem,
		"/root/snapshotted": diag.SeverityProblem,
		"/root/quarantined": diag.SeverityInfo,
	} {
		if severities[path] != want {
			t.Fatalf("finding for %s=%q, want %q", path, severities[path], want)
		}
	}
	for _, finding := range findings {
		if finding.Cause == "" || finding.Action == "" {
			t.Fatalf("finding without a cause or an action: %+v", finding)
		}
	}
}

// 期限切れの snapshot を支える ref は復元の材料ではないため、問題として扱わない。
func TestRecoveryRefFindingsExcludeExpiredSnapshots(t *testing.T) {
	past := state.FormatTime(time.Now().Add(-time.Hour))
	future := state.FormatTime(time.Now().Add(time.Hour))
	findings := recoveryRefFindings(
		[]recoveryRefIssue{{RepositoryID: "repo", Ref: "refs/wx/recovery/mismatched", ExpiresAt: future}},
		[]recoveryRefIssue{
			{RepositoryID: "repo", Ref: "refs/wx/recovery/live", ExpiresAt: future},
			{RepositoryID: "repo", Ref: "refs/wx/recovery/expired", ExpiresAt: past},
		},
	)
	severities := map[string]diag.Severity{}
	for _, finding := range findings {
		severities[finding.Target] = finding.Severity
	}
	if severities["repo:refs/wx/recovery/mismatched"] != diag.SeverityProblem {
		t.Fatalf("mismatched ref finding=%q", severities["repo:refs/wx/recovery/mismatched"])
	}
	if severities["repo:refs/wx/recovery/live"] != diag.SeverityProblem {
		t.Fatalf("missing ref finding=%q", severities["repo:refs/wx/recovery/live"])
	}
	if severities["repo:refs/wx/recovery/expired"] != diag.SeverityInfo {
		t.Fatalf("expired ref finding=%q", severities["repo:refs/wx/recovery/expired"])
	}
	// 期限を読めない ref は復元できるかもしれないため、参考へ落とさない。
	if expiredRecoverySnapshot("not a timestamp") || expiredRecoverySnapshot("") {
		t.Fatal("an unreadable expiry was treated as expired")
	}
}

func TestRecoveryFailureFindingSeparatesSaveFromRestore(t *testing.T) {
	snapshot := recoveryFailureFinding(state.RecoveryFailure{
		JobID: "job-1", Kind: "SNAPSHOT", SessionID: "session-1", SlotPath: "/root/slot",
		FailureCode: "SNAPSHOT_FAILED", FailureMessage: "write bundle: no space left on device",
		DetailPath: "/logs/job-1.log", SessionState: "SNAPSHOTTING", SlotState: "SNAPSHOTTING",
	})
	if snapshot.Severity != diag.SeverityProblem || snapshot.Target != "/root/slot" {
		t.Fatalf("snapshot failure finding=%+v", snapshot)
	}
	for _, fragment := range []string{"SNAPSHOT job job-1", "no space left on device", "/logs/job-1.log"} {
		if !strings.Contains(snapshot.Cause, fragment) {
			t.Fatalf("snapshot cause=%q, want %q", snapshot.Cause, fragment)
		}
	}
	if !strings.Contains(snapshot.Action, "end the session again") {
		t.Fatalf("snapshot action=%q", snapshot.Action)
	}
	restore := recoveryFailureFinding(state.RecoveryFailure{JobID: "job-2", Kind: "RESTORE", SessionID: "session-2"})
	if restore.Target != "session session-2" || !strings.Contains(restore.Action, "resume") {
		t.Fatalf("restore failure finding=%+v", restore)
	}
	if !strings.Contains(restore.Cause, "not recorded") {
		t.Fatalf("restore cause=%q, want it to say the reason is unknown", restore.Cause)
	}
}

func TestDoctorReportsUnresolvedSnapshotFailures(t *testing.T) {
	ctx, manager, store, workspaceRecord, _, _ := managerCoverageFixture(t)
	session := state.Session{ID: "snapshot", WorkspaceID: string(workspaceRecord.ID), SlotID: "snapshot", State: "SNAPSHOTTING", AgentKind: "codex", TokenHash: state.HashToken("snapshot")}
	slotPath := filepath.Join(manager.Config().Storage.WorktreeRoot, string(workspaceRecord.ID), "snapshot")
	if err := os.MkdirAll(slotPath, 0o700); err != nil {
		t.Fatal(err)
	}
	slot := slotAtPath(t, manager, string(workspaceRecord.ID), "snapshot", slotPath, 1, "SNAPSHOTTING")
	if _, err := store.CreateSlotSession(ctx, slot, nil, session, ""); err != nil {
		t.Fatal(err)
	}
	job, err := store.CreateJob(ctx, "SNAPSHOT", string(workspaceRecord.ID), slot.ID, session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ClaimJob(ctx, job.ID, "test"); err != nil {
		t.Fatal(err)
	}
	if err := store.FinishJobWithDetail(ctx, job.ID, "test", errWriteBundle, "SNAPSHOT_FAILED", ""); err != nil {
		t.Fatal(err)
	}
	failure := doctorProblem(t, manager.Doctor(ctx), diag.CheckRecoveryJobs)
	if !strings.Contains(failure.Cause, errWriteBundle.Error()) {
		t.Fatalf("recovery job finding=%+v", failure)
	}
}

// errWriteBundle は保存失敗の原因が診断まで届くことを確かめるための固定エラーである。
var errWriteBundle = errTestFailure("write bundle: no space left on device")

type errTestFailure string

func (e errTestFailure) Error() string { return string(e) }

// 復元に使える snapshot は、実体が無ければ問題として報告する。archive 本文は読まない。
func TestDoctorReportsWorkspaceSnapshotArchivesThatCannotRestore(t *testing.T) {
	ctx, manager, store, workspaceRecord, _, _ := managerCoverageFixture(t)
	if findings := doctorFindings(manager.Doctor(ctx), diag.CheckWorkspaceSnapshots); len(findings) != 1 || findings[0].Severity != diag.SeverityOK {
		t.Fatalf("workspace snapshot findings without any archive=%+v", findings)
	}
	session := state.Session{ID: "archived", WorkspaceID: string(workspaceRecord.ID), SlotID: "archived", State: "ARCHIVED", AgentKind: "codex", TokenHash: state.HashToken("archived")}
	slotPath := filepath.Join(manager.Config().Storage.WorktreeRoot, string(workspaceRecord.ID), "archived")
	if err := os.MkdirAll(slotPath, 0o700); err != nil {
		t.Fatal(err)
	}
	slot := slotAtPath(t, manager, string(workspaceRecord.ID), "archived", slotPath, 1, "SNAPSHOTTED")
	if _, err := store.CreateSlotSession(ctx, slot, nil, session, ""); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveWorkspaceSnapshot(ctx, state.WorkspaceSnapshot{
		SessionID: session.ID, RootID: slot.RootID, RelPath: filepath.Join("_recovery", "missing.tar"),
		SHA256: strings.Repeat("a", 64), Status: "ARCHIVED", CreatedAt: state.FormatTime(time.Now()),
		ExpiresAt: state.FormatTime(time.Now().Add(time.Hour)),
	}); err != nil {
		t.Fatal(err)
	}
	missing := doctorProblem(t, manager.Doctor(ctx), diag.CheckWorkspaceSnapshots)
	if !strings.Contains(missing.Target, "missing.tar") || !strings.Contains(missing.Cause, session.ID) {
		t.Fatalf("workspace snapshot finding=%+v", missing)
	}
}
