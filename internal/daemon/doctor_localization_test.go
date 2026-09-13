package daemon

import (
	"errors"
	"fmt"
	"io/fs"
	"strings"
	"testing"
	"time"

	"github.com/HappyOnigiri/WX/internal/diag"
	"github.com/HappyOnigiri/WX/internal/i18n"
	"github.com/HappyOnigiri/WX/internal/pool"
	"github.com/HappyOnigiri/WX/internal/state"
)

// localizableDoctorFindings は daemon 側の finding を、分岐ごとに 1 件ずつ作る。
// 網羅の基準は「文面が変わる分岐」であり、同じ文になる入力の違いは並べない。
func localizableDoctorFindings() []diag.Finding {
	findings := []diag.Finding{
		sqliteProblemFinding(errors.New("open state.db: permission denied")),
		sqliteBackupFinding("", time.Time{}),
		sqliteBackupFinding("", time.Unix(1700000000, 0).UTC()),
		sqliteBackupFinding("write backup: no space left on device", time.Time{}),
		rootRegistrationFinding("", ""),
		rootRegistrationFinding(rootFailureIdentity, "worktree root /root has no readable inode identity"),
		rootRegistrationFinding(rootFailureStore, "register root generation: database is locked"),
		rootRegistrationFinding(rootFailurePath, "expand ~: HOME is not set"),
		rootRegistrationFinding(rootFailureDescriptor, "open physical worktree root: not a directory"),
		rootRegistrationFinding("", "register root generation: inode identity changed"),
		workspaceResolveProblem("/root", fmt.Errorf("canonicalize %q: %w", "/root", fs.ErrNotExist)),
		workspaceResolveProblem("/root", errors.New("rediscover workspace root /root: exit status 128")),
		branchResolveProblem("/root", &pool.MissingDefaultBranchError{Branch: "main", RepositoryRelativePath: "."}),
		branchResolveProblem("/root", &pool.MissingDefaultBranchError{Branch: "main", RepositoryRelativePath: "api"}),
		branchResolveProblem("/root", errors.New("resolve branches: no such ref")),
		stateQueryProblem(diag.CheckWorktreeRegistration, "the standby slots of a registered workspace could not be read",
			message("diag.registration.slots_unreadable"), "/root", errors.New("read slots: database is locked")),
		standbyCheckProblem("/root", "slot-1", "/root/slot-1", errors.New("read slot: permission denied")),
	}
	for _, reason := range []string{
		state.StandbyReplenishReasonPlanFailure, state.SuspendReplenishReasonStandbyFailure,
		state.SuspendReplenishReasonForget, state.SuspendReplenishReasonClean, "UNRECOGNIZED",
	} {
		findings = append(findings, standbyReplenishmentFinding(state.StandbyReplenishmentDiagnostic{
			Root: "/root", Reason: reason, Detail: "job-1", FailedAt: "2024-01-01T00:00:00Z",
			Action: "wx retry-standby \"/root\"", FailureCode: "PREPARE_FAILED",
			FailureMessage: "copy /src/AGENTS.md: permission denied", DetailPath: "/logs/job-1.log",
		}))
		// 失敗情報が残っていない停止は原因の組み立てが別の分岐へ入る。
		findings = append(findings, standbyReplenishmentFinding(state.StandbyReplenishmentDiagnostic{
			Root: "/root", Reason: reason, Detail: "job-2", Action: "wx retry-standby \"/root\"",
		}))
	}
	findings = append(findings, unreadableRepositoryFindings([]unreadableRepository{
		{RepositoryID: "repo-1", Path: "/repos/one", Cause: "not a git repository"},
	})...)
	findings = append(findings, refListFailureFindings([]unreadableRepository{
		{RepositoryID: "repo-1", Path: "/repos/one", Cause: "fatal: not a git repository"},
	})...)
	findings = append(findings, missingArtifactFindings([]missingArtifact{
		{SlotID: "slot-1", Path: "/root/slot-1", State: "SNAPSHOTTED"},
		{SlotID: "slot-2", Path: "/root/slot-2", State: "READY"},
		{SlotID: "slot-3", Path: "/root/slot-3", State: "LEASED"},
	})...)
	expired, live := "2000-01-01T00:00:00Z", "2999-01-01T00:00:00Z"
	for _, expiresAt := range []string{live, expired} {
		findings = append(findings, recoveryRefFindings(
			[]recoveryRefIssue{{RepositoryID: "repo-1", Ref: "refs/wx/recovery/a", ExpiresAt: expiresAt}},
			[]recoveryRefIssue{{RepositoryID: "repo-1", Ref: "refs/wx/recovery/b", ExpiresAt: expiresAt}},
		)...)
		findings = append(findings, submoduleRefFindings([]submoduleRefIssue{
			{Kind: submoduleRefUnknown, ModuleDir: "/modules/one", Ref: "refs/wx/recovery/c"},
			{Kind: submoduleRefMismatched, ModuleDir: "/modules/one", Path: "vendor/one", Ref: "refs/wx/recovery/d", ExpiresAt: expiresAt},
			{Kind: submoduleRefMissing, ModuleDir: "/modules/one", Path: "vendor/two", Ref: "refs/wx/recovery/e", ExpiresAt: expiresAt},
		})...)
	}
	for _, slotState := range []string{"QUARANTINED", "DRAINING"} {
		findings = append(findings, recoveryFailureFinding(state.RecoveryFailure{
			JobID: "job-1", Kind: "SNAPSHOT", SessionID: "session-1", SlotPath: "/root/slot-1",
			FailureCode: "SNAPSHOT_FAILED", FailureMessage: "git push: permission denied",
			DetailPath: "/logs/job-1.log", FinishedAt: "2024-01-01T00:00:00Z",
			SessionState: "ENDED", SlotState: slotState,
		}))
	}
	findings = append(findings, recoveryFailureFinding(state.RecoveryFailure{
		JobID: "job-2", Kind: "RESTORE", SessionID: "session-2", ParentSessionID: "session-1",
	}))
	return findings
}

// 英語で解決した結果は解決前の文字列と一致しなければならない。
// `wx doctor --json` は英語で解決した本文をそのまま載せるため、ここがずれると出力の契約が変わる。
func TestDoctorFindingsKeepTheirEnglishText(t *testing.T) {
	t.Parallel()
	assertEnglishTextUnchanged(t, localizableDoctorFindings())
}

func TestManagerDoctorKeepsItsEnglishText(t *testing.T) {
	ctx, manager, _, _, _, _ := managerCoverageFixture(t)
	assertEnglishTextUnchanged(t, manager.Doctor(ctx).Findings)
}

// store を読めない検査は Doctor の正常系に現れないので、store を閉じてから各検査を直接呼ぶ。
// archive が root の外にある場合も、登録済み root からは作れない。
func TestManagerDoctorReadFailuresKeepTheirEnglishText(t *testing.T) {
	ctx, manager, store, _, _, _ := managerCoverageFixture(t)
	outside, _ := manager.workspaceSnapshotFinding(state.WorkspaceSnapshot{
		SessionID: "session-1", ArchivePath: "/outside/archive.tar",
	}, time.Now())
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	findings := []diag.Finding{outside}
	findings = append(findings, manager.registrationFindings(ctx)...)
	findings = append(findings, manager.standbyFindings(ctx)...)
	findings = append(findings, manager.quarantinedRecoveryFindings(ctx)...)
	findings = append(findings, manager.recoveryFailureFindings(ctx)...)
	findings = append(findings, manager.workspaceSnapshotFindings(ctx)...)
	findings = append(findings, manager.unsavedSubmoduleFindings(ctx)...)
	assertEnglishTextUnchanged(t, findings)
	resolved := diag.Resolve(diag.Reply{Findings: findings}, i18n.Japanese)
	for index, before := range findings {
		if before.Summary == resolved.Findings[index].Summary {
			t.Fatalf("summary of %q was not translated: %q", before.Check, before.Summary)
		}
	}
}

// 登録外の実体を持つ root では、所有権の検査が別の分岐の finding を作る。
func TestArtifactFindingsKeepTheirEnglishText(t *testing.T) {
	ctx, manager, _, _ := unmanagedFixture(t)
	findings := manager.artifactFindings(ctx)
	assertEnglishTextUnchanged(t, findings)
	resolved := diag.Resolve(diag.Reply{Findings: findings}, i18n.Japanese)
	for index, before := range findings {
		if before.Summary == resolved.Findings[index].Summary {
			t.Fatalf("summary of %q was not translated: %q", before.Check, before.Summary)
		}
	}
}

// assertEnglishTextUnchanged は英語での解決が本文を変えないことを確かめる。
func assertEnglishTextUnchanged(t *testing.T, findings []diag.Finding) {
	t.Helper()
	resolved := diag.Resolve(diag.Reply{Findings: findings}, i18n.English)
	for index, before := range findings {
		after := resolved.Findings[index]
		for _, row := range [][3]string{
			{"summary", before.Summary, after.Summary},
			{"cause", before.Cause, after.Cause},
			{"action", before.Action, after.Action},
		} {
			if row[1] != row[2] {
				t.Fatalf("%s of %q changed when resolved in English:\n before %q\n after  %q", row[0], before.Check, row[1], row[2])
			}
		}
		for detailIndex, detail := range before.Details {
			if detail != after.Details[detailIndex] {
				t.Fatalf("detail %d of %q changed when resolved in English:\n before %q\n after  %q",
					detailIndex, before.Check, detail, after.Details[detailIndex])
			}
		}
	}
}

// 日本語では本文が訳され、外部由来の値（path・ID・記録された失敗理由）は原文のまま残る。
func TestDoctorFindingsResolveIntoJapanese(t *testing.T) {
	t.Parallel()
	failure := standbyReplenishmentFinding(state.StandbyReplenishmentDiagnostic{
		Root: "/root", Reason: state.SuspendReplenishReasonStandbyFailure, Detail: "job-1",
		Action: "wx retry-standby \"/root\"", FailureCode: "PREPARE_FAILED",
		FailureMessage: "copy /src/AGENTS.md: permission denied", DetailPath: "/logs/job-1.log",
	})
	resolved := diag.Resolve(diag.Reply{Findings: []diag.Finding{failure}}, i18n.Japanese).Findings[0]
	if !strings.Contains(resolved.Summary, "standby worktree の準備に失敗") {
		t.Fatalf("summary=%q, want the Japanese text", resolved.Summary)
	}
	for _, fragment := range []string{"prepare job job-1", "PREPARE_FAILED", "copy /src/AGENTS.md: permission denied", "/logs/job-1.log"} {
		if !strings.Contains(resolved.Cause, fragment) {
			t.Fatalf("cause=%q, want it to keep %q", resolved.Cause, fragment)
		}
	}
	if !strings.Contains(resolved.Action, "wx retry-standby \"/root\"") {
		t.Fatalf("action=%q, want it to keep the command to run", resolved.Action)
	}
}

// 訳されていない finding が残っていないことを、すべての本文が日本語で変わることで確かめる。
func TestDoctorFindingsHaveNoUntranslatedText(t *testing.T) {
	t.Parallel()
	findings := localizableDoctorFindings()
	resolved := diag.Resolve(diag.Reply{Findings: findings}, i18n.Japanese)
	for index, before := range findings {
		after := resolved.Findings[index]
		// 原因が外部由来の本文そのものである finding は、訳さないことがこの規約の結論である。
		if before.Summary == after.Summary {
			t.Fatalf("summary of %q was not translated: %q", before.Check, before.Summary)
		}
		if before.Action != "" && before.Action == after.Action {
			t.Fatalf("action of %q was not translated: %q", before.Check, before.Action)
		}
	}
}
