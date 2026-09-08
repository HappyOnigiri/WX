package daemon

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/HappyOnigiri/WX/internal/config"
	"github.com/HappyOnigiri/WX/internal/diag"
	"github.com/HappyOnigiri/WX/internal/discovery"
	"github.com/HappyOnigiri/WX/internal/pool"
	"github.com/HappyOnigiri/WX/internal/state"
)

// Doctor は診断結果を種別つきの finding で返す。
// 問題・参考・正常・検査不能の判定は検出側で決め、表示側が文面から読み取ることはない。
// store を読めない場合は依存する検査を未検査として並べ、同じ故障を検査ごとに繰り返さない。
func (m *Manager) Doctor(ctx context.Context) diag.Reply {
	m.mu.RLock()
	reloadError, restartPending, cfg := m.reloadError, m.restartPending, m.cfg
	rootError, backupError, lastBackup := m.rootError, m.backupError, m.lastBackup
	m.mu.RUnlock()
	findings := diag.SharedFindings(ctx, cfg, reloadError, diag.SharedOptions{RestartPending: restartPending, Git: m.git})
	findings = append(findings, diag.Finding{
		Check: diag.CheckDaemon, Severity: diag.SeverityOK, Summary: "the daemon answered this request",
		Details: []string{"pid " + strconv.Itoa(os.Getpid()), "version " + daemonVersion(), "uptime " + time.Since(m.started).Truncate(time.Second).String()},
	})
	// backup と root 登録の結果は daemon が保持しているため、store を読めなくても報告できる。
	findings = append(findings, sqliteBackupFinding(backupError, lastBackup), rootRegistrationFinding(rootError))
	if err := m.store.Ping(ctx); err != nil {
		findings = append(findings, sqliteProblemFinding(err))
		return doctorReply(append(findings, diag.UncheckedFindings(diag.CheckSQLite, storeQueryChecks()...)...))
	}
	findings = append(findings, diag.Finding{Check: diag.CheckSQLite, Severity: diag.SeverityOK, Summary: "the state database is open", Target: statePathForDisplay()})
	findings = append(findings, m.registrationFindings(ctx)...)
	findings = append(findings, m.standbyFindings(ctx)...)
	findings = append(findings, m.artifactFindings(ctx)...)
	findings = append(findings, m.recoveryFailureFindings(ctx)...)
	findings = append(findings, m.workspaceSnapshotFindings(ctx)...)
	return doctorReply(findings)
}

// storeQueryChecks は store への問い合わせだけで成り立つ検査である。
// daemon が保持している結果（backup・root 登録）は store を読めなくても報告できるため、未検査にしない。
func storeQueryChecks() []string {
	checks := make([]string, 0, len(diag.StoreDependentChecks()))
	for _, check := range diag.StoreDependentChecks() {
		if check == diag.CheckSQLiteBackup || check == diag.CheckWorktreeRootRegistration {
			continue
		}
		checks = append(checks, check)
	}
	return checks
}

func doctorReply(findings []diag.Finding) diag.Reply {
	return diag.Reply{SchemaVersion: state.JSONSchemaVersion, DBSchemaVersion: state.SchemaVersion, Findings: findings}
}

// statePathForDisplay は state database の path を表示用に返す。解決できない場合は対象を空にする。
func statePathForDisplay() string {
	path, err := config.StatePath()
	if err != nil {
		return ""
	}
	return path
}

func sqliteProblemFinding(err error) diag.Finding {
	path := statePathForDisplay()
	action := "check that the state database is readable and writable by you"
	if path != "" {
		action = fmt.Sprintf("check that %s is readable and writable by you; if it is corrupt, preserve it for investigation and restore a verified backup from %s.backups", path, path)
	}
	return diag.Finding{
		Check: diag.CheckSQLite, Severity: diag.SeverityProblem, Summary: "the state database could not be reached",
		Target: path, Cause: err.Error(), Action: action,
	}
}

// sqliteBackupFinding は保持している未解消の backup 失敗を報告する。
// 失敗は次の周期で再試行されるため、対処は原因の解消だけにする。
func sqliteBackupFinding(backupError string, lastBackup time.Time) diag.Finding {
	target := ""
	if path := statePathForDisplay(); path != "" {
		target = path + ".backups"
	}
	if backupError == "" {
		details := []string{"no backup has run yet in this daemon process"}
		if !lastBackup.IsZero() {
			details = []string{"last backup at " + state.FormatTime(lastBackup)}
		}
		return diag.Finding{
			Check: diag.CheckSQLiteBackup, Severity: diag.SeverityOK,
			Summary: "no SQLite backup failure is outstanding", Target: target, Details: details,
		}
	}
	return diag.Finding{
		Check: diag.CheckSQLiteBackup, Severity: diag.SeverityProblem, Summary: "the SQLite online backup failed",
		Target: target, Cause: backupError,
		Action: "fix the reported cause on the backups directory, such as free space or write access; wx retries the backup on its next cycle",
	}
}

// rootRegistrationFinding は root の登録結果だけを報告する。
// path 自体の検査は diag.SharedFindings が別の finding で返し、どちらも互いの結果を上書きしない。
func rootRegistrationFinding(rootError string) diag.Finding {
	if rootError == "" {
		return diag.Finding{
			Check: diag.CheckWorktreeRootRegistration, Severity: diag.SeverityOK,
			Summary: "the worktree root is registered",
		}
	}
	return diag.Finding{
		Check: diag.CheckWorktreeRootRegistration, Severity: diag.SeverityProblem,
		Summary: "the worktree root could not be registered", Cause: rootError,
		Action: "fix the reported cause on the configured root, such as its permissions or the mount it lives on; wx retries the registration on each reconcile and needs no restart",
	}
}

// registrationFindings は READY slot の登録内容を現在のリポジトリ状態と突き合わせる。
// 解決不能・DB 読み取り失敗は問題とし、cold start で作り直せる不一致は参考に留める。
func (m *Manager) registrationFindings(ctx context.Context) []diag.Finding {
	roots, err := m.store.WorkspaceRoots(ctx)
	if err != nil {
		return []diag.Finding{{
			Check: diag.CheckWorktreeRegistration, Severity: diag.SeverityProblem,
			Summary: "the registered workspaces could not be read", Cause: err.Error(),
			Action: "fix the reported state database failure, then run wx doctor again",
		}}
	}
	findings := []diag.Finding{}
	checked := 0
	discoverer := discovery.Discoverer{Git: m.git, Config: m.Config()}
	for _, root := range roots {
		workspaceRecord, resolveErr := m.resolveRegisteredWorkspace(ctx, root, &discoverer)
		if resolveErr != nil {
			findings = append(findings, registrationProblem(root, "", "", resolveErr))
			continue
		}
		resolved, resolveErr := pool.ResolveBranches(ctx, m.git, workspaceRecord, nil)
		if resolveErr != nil {
			findings = append(findings, registrationProblem(root, "", "", resolveErr))
			continue
		}
		slots, slotsErr := m.store.ReadySlots(ctx, string(workspaceRecord.ID))
		if slotsErr != nil {
			findings = append(findings, registrationProblem(root, "", "", slotsErr))
			continue
		}
		for _, slot := range slots {
			checked++
			valid, validationErr := m.readyMatches(ctx, slot, resolved)
			switch {
			case validationErr != nil:
				findings = append(findings, registrationProblem(root, slot.ID, slot.Path, validationErr))
			case !valid:
				findings = append(findings, diag.Finding{
					Check: diag.CheckWorktreeRegistration, Severity: diag.SeverityInfo,
					Summary: "a standby slot no longer matches the repository state", Target: slot.Path,
					Cause:  fmt.Sprintf("slot %s in workspace %s does not satisfy the READY invariants for the current branches", slot.ID, root),
					Action: "no action is required; wx replaces the slot with a cold start when the next session needs it",
				})
			}
		}
	}
	return append(findings, diag.Finding{
		Check: diag.CheckWorktreeRegistration, Severity: diag.SeverityOK,
		Summary: "the registered standby slots were checked", Details: []string{strconv.Itoa(checked) + " READY slot(s) checked"},
	})
}

func registrationProblem(root, slotID, path string, err error) diag.Finding {
	target := root
	if path != "" {
		target = path
	}
	cause := err.Error()
	if slotID != "" {
		cause = fmt.Sprintf("slot %s: %s", slotID, cause)
	}
	return diag.Finding{
		Check: diag.CheckWorktreeRegistration, Severity: diag.SeverityProblem,
		Summary: "a registered workspace could not be checked", Target: target, Cause: cause,
		Action: "fix the reported cause, such as the source repository, its branches, or the state database; wx cannot lease a slot from this workspace until then",
	}
}

// standbyFindings は補充停止を停止理由ごとに分ける。
// 準備失敗は失敗した job の原因まで引き継ぎ、`wx clear` による意図的な停止は参考に留める。
func (m *Manager) standbyFindings(ctx context.Context) []diag.Finding {
	items, err := m.standbyReplenishmentReport(ctx)
	if err != nil {
		return []diag.Finding{{
			Check: diag.CheckStandbyReplenishment, Severity: diag.SeverityProblem,
			Summary: "the standby replenishment state could not be read", Cause: err.Error(),
			Action: "fix the reported state database failure, then run wx doctor again",
		}}
	}
	findings := make([]diag.Finding, 0, len(items)+1)
	for _, item := range items {
		findings = append(findings, standbySuspensionFinding(item))
	}
	return append(findings, diag.Finding{
		Check: diag.CheckStandbyReplenishment, Severity: diag.SeverityOK,
		Summary: "standby replenishment is not stopped", Details: []string{strconv.Itoa(len(items)) + " suspended workspace(s)"},
	})
}

// standbySuspensionFinding は停止 1 件を理由ごとに説明する。
// 既知でない理由は原因を特定できていないことを示し、`wx clear` などの既知の経路に帰属させない。
func standbySuspensionFinding(item state.StandbyReplenishmentDiagnostic) diag.Finding {
	switch item.Reason {
	case state.SuspendReplenishReasonStandbyFailure:
		return diag.Finding{
			Check: diag.CheckStandbyReplenishment, Severity: diag.SeverityProblem,
			Summary: "standby worktree preparation failed and replenishment is stopped", Target: item.Root,
			Cause:  jobFailureCause("prepare job "+item.Detail, item.FailureCode, item.FailureMessage, item.DetailPath),
			Action: "fix the reported cause, then run " + item.Action,
		}
	case state.SuspendReplenishReasonClean:
		return diag.Finding{
			Check: diag.CheckStandbyReplenishment, Severity: diag.SeverityInfo,
			Summary: "standby replenishment is stopped on purpose", Target: item.Root,
			Cause:  fmt.Sprintf("wx clear stopped the replenishment (clean run %s)", item.Detail),
			Action: "no action is required; run " + item.Action + " to resume it",
		}
	default:
		return diag.Finding{
			Check: diag.CheckStandbyReplenishment, Severity: diag.SeverityInfo,
			Summary: "standby replenishment is stopped for an unrecognized reason", Target: item.Root,
			Cause: fmt.Sprintf("the suspension records reason %s with detail %s, and this wx binary cannot explain what stopped it",
				item.Reason, item.Detail),
			Action: "run " + item.Action + " to resume it once you know the stop was not needed",
		}
	}
}

// jobFailureCause は job の失敗を「失敗した操作・記録された理由・詳細ログの場所」の順で 1 行にまとめる。
// 理由が残っていない場合は特定できていないことを明示し、上位の処理名を原因として言い換えない。
func jobFailureCause(operation, failureCode, failureMessage, detailPath string) string {
	cause := operation
	if failureCode != "" {
		cause += " failed with " + failureCode
	} else {
		cause += " failed"
	}
	if failureMessage != "" {
		cause += ": " + failureMessage
	} else {
		cause += "; the failure reason was not recorded, so the root cause is unknown"
	}
	if detailPath != "" {
		cause += " (command output in " + detailPath + ")"
	}
	return cause
}
