package daemon

import (
	"strings"
	"testing"
	"time"

	"github.com/HappyOnigiri/WX/internal/diag"
	"github.com/HappyOnigiri/WX/internal/state"
)

// doctorFindings は検査名が一致する finding を返す。
func doctorFindings(reply diag.Reply, check string) []diag.Finding {
	out := []diag.Finding{}
	for _, finding := range reply.Findings {
		if finding.Check == check {
			out = append(out, finding)
		}
	}
	return out
}

// doctorProblem は指定した検査の最初の問題を返す。原因と対処が揃っていない finding は失敗として扱う。
func doctorProblem(t *testing.T, reply diag.Reply, check string) diag.Finding {
	t.Helper()
	for _, finding := range doctorFindings(reply, check) {
		if finding.Severity != diag.SeverityProblem {
			continue
		}
		if finding.Cause == "" || finding.Action == "" {
			t.Fatalf("problem for %s lacks a cause or an action: %+v", check, finding)
		}
		return finding
	}
	t.Fatalf("no problem reported for %s: %+v", check, reply.Findings)
	return diag.Finding{}
}

func TestDoctorKeepsRootPathAndRegistrationApart(t *testing.T) {
	ctx, manager, _, _, _, _ := managerCoverageFixture(t)
	manager.mu.Lock()
	manager.rootError = "register root generation: inode identity changed"
	manager.mu.Unlock()
	reply := manager.Doctor(ctx)
	registration := doctorProblem(t, reply, diag.CheckWorktreeRootRegistration)
	if !strings.Contains(registration.Cause, "inode identity") {
		t.Fatalf("registration cause=%q, want the recorded failure", registration.Cause)
	}
	path := doctorFindings(reply, diag.CheckWorktreeRoot)
	if len(path) != 1 || path[0].Severity == diag.SeverityProblem {
		t.Fatalf("worktree root path findings=%+v, want the path check to keep its own result", path)
	}
}

func TestDoctorReportsSQLiteBackupFailureWithItsCause(t *testing.T) {
	ctx, manager, _, _, _, _ := managerCoverageFixture(t)
	manager.mu.Lock()
	manager.backupError = "write backup: no space left on device"
	manager.mu.Unlock()
	backup := doctorProblem(t, manager.Doctor(ctx), diag.CheckSQLiteBackup)
	if !strings.Contains(backup.Cause, "no space left on device") {
		t.Fatalf("backup cause=%q, want the recorded failure", backup.Cause)
	}
	if !strings.HasSuffix(backup.Target, ".backups") {
		t.Fatalf("backup target=%q, want the backups directory", backup.Target)
	}
}

func TestSQLiteBackupFindingReportsTheLastSuccess(t *testing.T) {
	last := time.Unix(1700000000, 0).UTC()
	finding := sqliteBackupFinding("", last)
	if finding.Severity != diag.SeverityOK || len(finding.Details) != 1 || !strings.Contains(finding.Details[0], "1700000000") && !strings.Contains(finding.Details[0], "2023") {
		t.Fatalf("backup finding=%+v, want the last success in its details", finding)
	}
	if pending := sqliteBackupFinding("", time.Time{}); pending.Severity != diag.SeverityOK || len(pending.Details) != 1 {
		t.Fatalf("backup finding without a run=%+v", pending)
	}
}

func TestRootRegistrationFindingKeepsRetryGuidance(t *testing.T) {
	if ok := rootRegistrationFinding(""); ok.Severity != diag.SeverityOK {
		t.Fatalf("registered root finding=%+v", ok)
	}
	failed := rootRegistrationFinding("mount is read-only")
	if failed.Severity != diag.SeverityProblem || !strings.Contains(failed.Action, "reconcile") {
		t.Fatalf("failed root finding=%+v, want the automatic retry in its action", failed)
	}
}

func TestJobFailureCauseKeepsTheRecordedReason(t *testing.T) {
	cause := jobFailureCause("prepare job job-1", "PREPARE_FAILED", "copy /src/AGENTS.md to /slot/AGENTS.md: permission denied", "/logs/job-1.log")
	for _, fragment := range []string{"prepare job job-1", "PREPARE_FAILED", "permission denied", "/logs/job-1.log"} {
		if !strings.Contains(cause, fragment) {
			t.Fatalf("cause=%q, want %q", cause, fragment)
		}
	}
	unknown := jobFailureCause("SNAPSHOT job job-2", "JOB_FAILED", "", "")
	if !strings.Contains(unknown, "not recorded") || strings.Contains(unknown, "(command output") {
		t.Fatalf("cause without a recorded reason=%q, want it to say the root cause is unknown", unknown)
	}
}

// 既知でない停止理由は原因を特定できていないことを示し、`wx clear` に帰属させない。
func TestStandbySuspensionFindingSeparatesEachReason(t *testing.T) {
	failure := standbySuspensionFinding(state.StandbyReplenishmentDiagnostic{
		Root: "/root", Reason: state.SuspendReplenishReasonStandbyFailure, Detail: "job-1",
		FailureCode: "PREPARE_FAILED", Action: "wx retry-standby \"/root\"",
	})
	if failure.Severity != diag.SeverityProblem || !strings.Contains(failure.Cause, "PREPARE_FAILED") {
		t.Fatalf("preparation failure finding=%+v", failure)
	}
	clean := standbySuspensionFinding(state.StandbyReplenishmentDiagnostic{
		Root: "/root", Reason: state.SuspendReplenishReasonClean, Detail: "run-1", Action: "wx retry-standby \"/root\"",
	})
	if clean.Severity != diag.SeverityInfo || !strings.Contains(clean.Cause, "wx clear") {
		t.Fatalf("clean suspension finding=%+v", clean)
	}
	unknown := standbySuspensionFinding(state.StandbyReplenishmentDiagnostic{
		Root: "/root", Reason: "FUTURE_REASON", Detail: "detail-1", Action: "wx retry-standby \"/root\"",
	})
	if unknown.Severity != diag.SeverityInfo || strings.Contains(unknown.Cause, "wx clear") {
		t.Fatalf("unknown suspension finding=%+v", unknown)
	}
	for _, fragment := range []string{"FUTURE_REASON", "detail-1", "cannot explain"} {
		if !strings.Contains(unknown.Cause, fragment) {
			t.Fatalf("unknown suspension cause=%q, want %q", unknown.Cause, fragment)
		}
	}
}

// registrationIssues は登録検査の finding から、正常確認以外を返す。
func registrationIssues(findings []diag.Finding) []diag.Finding {
	out := []diag.Finding{}
	for _, finding := range findings {
		if finding.Severity != diag.SeverityOK {
			out = append(out, finding)
		}
	}
	return out
}
