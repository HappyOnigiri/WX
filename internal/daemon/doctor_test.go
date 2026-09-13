package daemon

import (
	"errors"
	"fmt"
	"io/fs"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/HappyOnigiri/WX/internal/diag"
	"github.com/HappyOnigiri/WX/internal/pool"
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
	if ok := rootRegistrationFinding("", ""); ok.Severity != diag.SeverityOK {
		t.Fatalf("registered root finding=%+v", ok)
	}
	failed := rootRegistrationFinding(rootFailureDescriptor, "mount is read-only")
	if failed.Severity != diag.SeverityProblem || !strings.Contains(failed.Action, "reconcile") {
		t.Fatalf("failed root finding=%+v, want the automatic retry in its action", failed)
	}
}

func TestJobFailureCauseKeepsTheRecordedReason(t *testing.T) {
	cause, _ := jobFailureCause("prepare job job-1", message("diag.standby.job_prepare", "Detail", "job-1"),
		"PREPARE_FAILED", "copy /src/AGENTS.md to /slot/AGENTS.md: permission denied", "/logs/job-1.log")
	for _, fragment := range []string{"prepare job job-1", "PREPARE_FAILED", "permission denied", "/logs/job-1.log"} {
		if !strings.Contains(cause, fragment) {
			t.Fatalf("cause=%q, want %q", cause, fragment)
		}
	}
	unknown, _ := jobFailureCause("SNAPSHOT job job-2",
		message("diag.recovery.job_operation", "Kind", "SNAPSHOT", "JobID", "job-2"), "JOB_FAILED", "", "")
	if !strings.Contains(unknown, "not recorded") || strings.Contains(unknown, "(command output") {
		t.Fatalf("cause without a recorded reason=%q, want it to say the root cause is unknown", unknown)
	}
}

// 既知でない停止理由は原因を特定できていないことを示し、`wx clear` に帰属させない。
func TestStandbySuspensionFindingSeparatesEachReason(t *testing.T) {
	failure := standbyReplenishmentFinding(state.StandbyReplenishmentDiagnostic{
		Root: "/root", Reason: state.SuspendReplenishReasonStandbyFailure, Detail: "job-1",
		FailureCode: "PREPARE_FAILED", Action: "wx retry-standby \"/root\"",
	})
	if failure.Severity != diag.SeverityProblem || !strings.Contains(failure.Cause, "PREPARE_FAILED") {
		t.Fatalf("preparation failure finding=%+v", failure)
	}
	clean := standbyReplenishmentFinding(state.StandbyReplenishmentDiagnostic{
		Root: "/root", Reason: state.SuspendReplenishReasonClean, Detail: "run-1", Action: "wx retry-standby \"/root\"",
	})
	if clean.Severity != diag.SeverityInfo || !strings.Contains(clean.Cause, "wx clear") {
		t.Fatalf("clean suspension finding=%+v", clean)
	}
	unknown := standbyReplenishmentFinding(state.StandbyReplenishmentDiagnostic{
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

// 補充計画（ENSURE_STANDBY）の失敗は停止行を持たないが、待機枠が埋まらない問題として失敗理由まで報告する。
func TestStandbyReplenishmentFindingReportsPlanFailure(t *testing.T) {
	finding := standbyReplenishmentFinding(state.StandbyReplenishmentDiagnostic{
		Root: "/root", Reason: state.StandbyReplenishReasonPlanFailure, Detail: "job-9",
		FailureCode: "JOB_FAILED", FailureMessage: `unsafe .worktreeinclude pattern "../escape"`,
		DetailPath: "/logs/job-9.log", FailedAt: "2026-09-11T00:00:00Z", Action: "wx retry-standby \"/root\"",
	})
	if finding.Severity != diag.SeverityProblem || finding.Target != "/root" {
		t.Fatalf("plan failure finding=%+v", finding)
	}
	for _, fragment := range []string{"job-9", "JOB_FAILED", "unsafe .worktreeinclude pattern", "/logs/job-9.log"} {
		if !strings.Contains(finding.Cause, fragment) {
			t.Fatalf("plan failure cause=%q, want %q", finding.Cause, fragment)
		}
	}
	if !strings.Contains(finding.Action, "wx retry-standby") {
		t.Fatalf("plan failure action=%q, want the retry command", finding.Action)
	}
	if !slices.Contains(finding.Details, "failed at 2026-09-11T00:00:00Z") {
		t.Fatalf("plan failure details=%v, want the failure time", finding.Details)
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

// 登録検査の対処は原因の種別で分かれる。判定が効いていることを、種別ごとの手順の違いで固定する。
func TestRegistrationProblemsSeparateTheirActionsByCause(t *testing.T) {
	missingRoot := workspaceResolveProblem("/roots/ws", fmt.Errorf("canonicalize %q: %w", "/roots/ws", fs.ErrNotExist))
	if !strings.Contains(missingRoot.Action, "wx forget --discard-recovery /roots/ws") {
		t.Fatalf("missing root action=%q, want the unregister path", missingRoot.Action)
	}
	unreadable := workspaceResolveProblem("/roots/ws", errors.New("rediscover workspace root: exit status 128"))
	if strings.Contains(unreadable.Action, "--discard-recovery") || !strings.Contains(unreadable.Action, "git -C /roots/ws status") {
		t.Fatalf("unreadable workspace action=%q, want the repository check and no discard", unreadable.Action)
	}
	slots := stateQueryProblem(diag.CheckWorktreeRegistration, "the standby slots of a registered workspace could not be read",
		message("diag.registration.slots_unreadable"), "/roots/ws", errors.New("read slots: database is locked"))
	for _, other := range []diag.Finding{missingRoot, unreadable} {
		if slots.Action == other.Action {
			t.Fatalf("state database action=%q must differ from %q", slots.Action, other.Action)
		}
	}
	if !strings.Contains(slots.Action, "backup") {
		t.Fatalf("state database action=%q, want the preserve-and-restore path", slots.Action)
	}
}

// 既定 branch の欠落は sentinel error で判定し、workspace の構成に合う config scope を案内する。
func TestBranchResolveProblemPointsAtTheRepositoryScope(t *testing.T) {
	unresolved := branchResolveProblem("/roots/ws", &pool.UnresolvedDefaultBranchError{RepositoryRelativePath: "api"})
	if !strings.Contains(unresolved.Action, "git -C /roots/ws/api remote set-head origin --auto") ||
		!strings.Contains(unresolved.Action, "wx config --workspace /roots/ws --repository api default_branch") {
		t.Fatalf("unresolved action=%q, want remote-head and explicit config guidance", unresolved.Action)
	}
	single := branchResolveProblem("/roots/ws", fmt.Errorf("resolve branches: %w",
		&pool.MissingDefaultBranchError{Branch: "main", RepositoryRelativePath: "."}))
	if !strings.Contains(single.Action, "wx config --workspace /roots/ws --repository-defaults default_branch") {
		t.Fatalf("single repository action=%q, want the repository defaults scope", single.Action)
	}
	multi := branchResolveProblem("/roots/ws", &pool.MissingDefaultBranchError{Branch: "main", RepositoryRelativePath: "api"})
	if !strings.Contains(multi.Action, "wx config --workspace /roots/ws --repository api default_branch") {
		t.Fatalf("multi repository action=%q, want the membership scope", multi.Action)
	}
	other := branchResolveProblem("/roots/ws", errors.New("fatal: bad object HEAD"))
	if strings.Contains(other.Action, "wx config") || !strings.Contains(other.Action, "git -C /roots/ws rev-parse HEAD") {
		t.Fatalf("ref failure action=%q, want the Git check and no config command", other.Action)
	}
}

// root 登録の失敗は種別ごとに直す先が違うので、同じ対処へ畳まない。
func TestRootRegistrationActionsDifferByKind(t *testing.T) {
	seen := map[string]string{}
	for _, kind := range []string{rootFailureIdentity, rootFailureStore, rootFailurePath, rootFailureDescriptor, ""} {
		action := rootRegistrationFinding(kind, "boom").Action
		if action == "" {
			t.Fatalf("root failure %q has no action", kind)
		}
		if previous, repeated := seen[action]; repeated {
			t.Fatalf("root failure %q repeats the action of %q: %q", kind, previous, action)
		}
		seen[action] = kind
	}
}

// 所有権の照合を完了できない失敗も、読めなかった対象ごとに調べる先を変える。
func TestOwnershipFailureActionsNameTheirTarget(t *testing.T) {
	slot, _ := ownershipFailureAction(ownershipFailure{Kind: ownershipFailureSlotPath, Target: "/roots/ws/slot", Message: "boom"})
	root, _ := ownershipFailureAction(ownershipFailure{Kind: ownershipFailureRootPath, Target: "/roots", Message: "boom"})
	refs, _ := ownershipFailureAction(ownershipFailure{Kind: ownershipFailureRepositoryRef, Target: "/repos/one", Message: "boom"})
	for target, action := range map[string]string{"/roots/ws/slot": slot, "/roots": root, "/repos/one": refs} {
		if !strings.Contains(action, target) {
			t.Fatalf("action=%q, want the target %q", action, target)
		}
	}
	if !strings.Contains(refs, "for-each-ref") {
		t.Fatalf("repository ref action=%q, want the ref listing command", refs)
	}
}
