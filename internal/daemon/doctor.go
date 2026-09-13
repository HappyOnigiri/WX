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
	"github.com/HappyOnigiri/WX/internal/i18n"
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
	pid, version := strconv.Itoa(os.Getpid()), daemonVersion()
	uptime := time.Since(m.started).Truncate(time.Second).String()
	findings = append(findings, diag.Finding{
		Check: diag.CheckDaemon, Severity: diag.SeverityOK, Summary: "the daemon answered this request",
		Details: []string{"pid " + pid, "version " + version, "uptime " + uptime},
		Messages: diag.FindingMessages{
			Summary: message("diag.daemon.answered"),
			Details: []i18n.Message{
				message("diag.detail.pid", "Value", pid),
				message("diag.detail.version", "Value", version),
				message("diag.detail.uptime", "Value", uptime),
			},
		},
	})
	// backup と root 登録の結果は daemon が保持しているため、store を読めなくても報告できる。
	findings = append(findings, sqliteBackupFinding(backupError, lastBackup), rootRegistrationFinding(rootError))
	if err := m.store.Ping(ctx); err != nil {
		findings = append(findings, sqliteProblemFinding(err))
		return doctorReply(append(findings, diag.UncheckedFindings(diag.CheckSQLite, storeQueryChecks()...)...))
	}
	findings = append(findings, diag.Finding{
		Check: diag.CheckSQLite, Severity: diag.SeverityOK, Summary: "the state database is open", Target: statePathForDisplay(),
		Messages: diag.FindingMessages{Summary: message("diag.state_db.open")},
	})
	findings = append(findings, m.registrationFindings(ctx)...)
	findings = append(findings, m.standbyFindings(ctx)...)
	findings = append(findings, m.artifactFindings(ctx)...)
	findings = append(findings, m.recoveryFailureFindings(ctx)...)
	findings = append(findings, m.workspaceSnapshotFindings(ctx)...)
	findings = append(findings, m.unsavedSubmoduleFindings(ctx)...)
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
	actionMessage := message("diag.action.check_state_db")
	if path != "" {
		action = fmt.Sprintf("check that %s is readable and writable by you; if it is corrupt, preserve it for investigation and restore a verified backup from %s.backups", path, path)
		actionMessage = message("diag.action.check_state_db_path", "Path", path)
	}
	return diag.Finding{
		Check: diag.CheckSQLite, Severity: diag.SeverityProblem, Summary: "the state database could not be reached",
		Target: path, Cause: err.Error(), Action: action,
		Messages: diag.FindingMessages{Summary: message("diag.state_db.unreachable"), Action: actionMessage},
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
		detailMessages := []i18n.Message{message("diag.detail.no_backup_yet")}
		if !lastBackup.IsZero() {
			formatted := state.FormatTime(lastBackup)
			details = []string{"last backup at " + formatted}
			detailMessages = []i18n.Message{message("diag.detail.last_backup", "Time", formatted)}
		}
		return diag.Finding{
			Check: diag.CheckSQLiteBackup, Severity: diag.SeverityOK,
			Summary: "no SQLite backup failure is outstanding", Target: target, Details: details,
			Messages: diag.FindingMessages{Summary: message("diag.sqlite_backup.none"), Details: detailMessages},
		}
	}
	return diag.Finding{
		Check: diag.CheckSQLiteBackup, Severity: diag.SeverityProblem, Summary: "the SQLite online backup failed",
		Target: target, Cause: backupError,
		Action: "fix the reported cause on the backups directory, such as free space or write access; wx retries the backup on its next cycle",
		Messages: diag.FindingMessages{
			Summary: message("diag.sqlite_backup.failed"),
			Action:  message("diag.action.fix_backup_directory"),
		},
	}
}

// rootRegistrationFinding は root の登録結果だけを報告する。
// path 自体の検査は diag.SharedFindings が別の finding で返し、どちらも互いの結果を上書きしない。
func rootRegistrationFinding(rootError string) diag.Finding {
	if rootError == "" {
		return diag.Finding{
			Check: diag.CheckWorktreeRootRegistration, Severity: diag.SeverityOK,
			Summary:  "the worktree root is registered",
			Messages: diag.FindingMessages{Summary: message("diag.worktree_root.registered")},
		}
	}
	return diag.Finding{
		Check: diag.CheckWorktreeRootRegistration, Severity: diag.SeverityProblem,
		Summary: "the worktree root could not be registered", Cause: rootError,
		Action: "fix the reported cause on the configured root, such as its permissions or the mount it lives on; wx retries the registration on each reconcile and needs no restart",
		Messages: diag.FindingMessages{
			Summary: message("diag.worktree_root.unregistered"),
			Action:  message("diag.action.fix_root_registration"),
		},
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
			Messages: diag.FindingMessages{
				Summary: message("diag.registration.workspaces_unreadable"),
				Action:  message("diag.action.fix_state_database"),
			},
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
		reuse, _ := m.Config().ReuseStandbyForWorkspace(string(workspaceRecord.Root))
		for _, slot := range slots {
			checked++
			valid, validationErr := m.standbyReadyUsable(ctx, slot, workspaceRecord, resolved, reuse)
			switch {
			case validationErr != nil:
				findings = append(findings, registrationProblem(root, slot.ID, slot.Path, validationErr))
			case !valid:
				findings = append(findings, diag.Finding{
					Check: diag.CheckWorktreeRegistration, Severity: diag.SeverityInfo,
					Summary: "a standby slot no longer matches the repository state", Target: slot.Path,
					Cause:  fmt.Sprintf("slot %s in workspace %s does not satisfy the READY invariants for the current branches", slot.ID, root),
					Action: "no action is required; wx replaces the slot with a cold start when the next session needs it",
					Messages: diag.FindingMessages{
						Summary: message("diag.registration.standby_mismatch"),
						Cause:   message("diag.registration.standby_mismatch_cause", "SlotID", slot.ID, "Root", root),
						Action:  message("diag.action.standby_mismatch"),
					},
				})
			}
		}
	}
	return append(findings, diag.Finding{
		Check: diag.CheckWorktreeRegistration, Severity: diag.SeverityOK,
		Summary: "the registered standby slots were checked", Details: []string{strconv.Itoa(checked) + " READY slot(s) checked"},
		Messages: diag.FindingMessages{
			Summary: message("diag.registration.checked"),
			Details: []i18n.Message{message("diag.detail.ready_slots_checked", "Count", checked)},
		},
	})
}

func registrationProblem(root, slotID, path string, err error) diag.Finding {
	target := root
	if path != "" {
		target = path
	}
	// slot ID が無い場合の原因は err の本文そのものなので、訳さず原文のまま残す。
	cause, causeMessage := err.Error(), i18n.Message{}
	if slotID != "" {
		cause = fmt.Sprintf("slot %s: %s", slotID, cause)
		causeMessage = message("diag.registration.slot_cause", "SlotID", slotID, "Error", err.Error())
	}
	return diag.Finding{
		Check: diag.CheckWorktreeRegistration, Severity: diag.SeverityProblem,
		Summary: "a registered workspace could not be checked", Target: target, Cause: cause,
		Action: "fix the reported cause, such as the source repository, its branches, or the state database; wx cannot lease a slot from this workspace until then",
		Messages: diag.FindingMessages{
			Summary: message("diag.registration.workspace_unchecked"),
			Cause:   causeMessage,
			Action:  message("diag.action.fix_workspace_check"),
		},
	}
}

// standbyFindings は補充が進んでいない workspace を理由ごとに分ける。
// 準備失敗と補充計画の失敗は失敗した job の原因まで引き継ぎ、`wx clear` による意図的な停止は参考に留める。
func (m *Manager) standbyFindings(ctx context.Context) []diag.Finding {
	items, err := m.standbyReplenishmentReport(ctx)
	if err != nil {
		return []diag.Finding{{
			Check: diag.CheckStandbyReplenishment, Severity: diag.SeverityProblem,
			Summary: "the standby replenishment state could not be read", Cause: err.Error(),
			Action: "fix the reported state database failure, then run wx doctor again",
			Messages: diag.FindingMessages{
				Summary: message("diag.standby.unreadable"),
				Action:  message("diag.action.fix_state_database"),
			},
		}}
	}
	findings := make([]diag.Finding, 0, len(items)+1)
	for _, item := range items {
		findings = append(findings, standbyReplenishmentFinding(item))
	}
	if len(items) > 0 {
		return findings
	}
	return append(findings, diag.Finding{
		Check: diag.CheckStandbyReplenishment, Severity: diag.SeverityOK,
		Summary: "standby replenishment is not stopped", Details: []string{"0 suspended workspace(s)", "0 failed replenishment plan(s)"},
		Messages: diag.FindingMessages{
			Summary: message("diag.standby.not_stopped"),
			Details: []i18n.Message{
				message("diag.detail.no_suspended_workspaces"),
				message("diag.detail.no_failed_plans"),
			},
		},
	})
}

// standbyReplenishmentFinding は補充が進んでいない workspace 1 件を理由ごとに説明する。
// 既知でない理由は原因を特定できていないことを示し、`wx clear` などの既知の経路に帰属させない。
func standbyReplenishmentFinding(item state.StandbyReplenishmentDiagnostic) diag.Finding {
	switch item.Reason {
	case state.StandbyReplenishReasonPlanFailure:
		// 計画の失敗は補充を止めないので、停止ではなく「枠が埋まらないまま繰り返し失敗している」ことを報告する。
		details := []string{"job " + item.Detail}
		detailMessages := []i18n.Message{message("diag.detail.job", "JobID", item.Detail)}
		if item.FailedAt != "" {
			details = append(details, "failed at "+item.FailedAt)
			detailMessages = append(detailMessages, message("diag.detail.failed_at", "Time", item.FailedAt))
		}
		cause, causeMessage := jobFailureCause("standby replenishment job "+item.Detail,
			message("diag.standby.job_replenishment", "Detail", item.Detail),
			item.FailureCode, item.FailureMessage, item.DetailPath)
		return diag.Finding{
			Check: diag.CheckStandbyReplenishment, Severity: diag.SeverityProblem,
			Summary: "planning the standby worktrees failed, so the warm slots are not replenished", Target: item.Root,
			Cause: cause,
			Action: "fix the reported cause, such as the .worktreeinclude or .worktreelink manifest; wx retries the plan on the next lease and maintenance tick, or run " +
				item.Action + " to retry it now",
			Details: details,
			Messages: diag.FindingMessages{
				Summary: message("diag.standby.plan_failed"),
				Cause:   causeMessage,
				Action:  message("diag.action.fix_standby_plan", "Action", item.Action),
				Details: detailMessages,
			},
		}
	case state.SuspendReplenishReasonStandbyFailure:
		cause, causeMessage := jobFailureCause("prepare job "+item.Detail,
			message("diag.standby.job_prepare", "Detail", item.Detail),
			item.FailureCode, item.FailureMessage, item.DetailPath)
		return diag.Finding{
			Check: diag.CheckStandbyReplenishment, Severity: diag.SeverityProblem,
			Summary: "standby worktree preparation failed and replenishment is stopped", Target: item.Root,
			Cause:  cause,
			Action: "fix the reported cause, then run " + item.Action,
			Messages: diag.FindingMessages{
				Summary: message("diag.standby.prepare_failed"),
				Cause:   causeMessage,
				Action:  message("diag.action.fix_then_run", "Action", item.Action),
			},
		}
	case state.SuspendReplenishReasonForget:
		// 解除まで進めば停止行も消えるため、残っているのは forget が途中で失敗したことを意味する。
		return diag.Finding{
			Check: diag.CheckStandbyReplenishment, Severity: diag.SeverityProblem,
			Summary: "wx forget stopped the replenishment and did not finish", Target: item.Root,
			Cause:  fmt.Sprintf("wx forget stopped the replenishment of %s and the workspace is still registered", item.Detail),
			Action: "run wx forget " + item.Root + " again, or run " + item.Action + " to keep the workspace and resume it",
			Messages: diag.FindingMessages{
				Summary: message("diag.standby.forget_unfinished"),
				Cause:   message("diag.standby.forget_cause", "Detail", item.Detail),
				Action:  message("diag.action.retry_forget", "Root", item.Root, "Action", item.Action),
			},
		}
	case state.SuspendReplenishReasonClean:
		return diag.Finding{
			Check: diag.CheckStandbyReplenishment, Severity: diag.SeverityInfo,
			Summary: "standby replenishment is stopped on purpose", Target: item.Root,
			Cause:  fmt.Sprintf("wx clear stopped the replenishment (clean run %s)", item.Detail),
			Action: "no action is required; run " + item.Action + " to resume it",
			Messages: diag.FindingMessages{
				Summary: message("diag.standby.stopped_on_purpose"),
				Cause:   message("diag.standby.clean_cause", "Detail", item.Detail),
				Action:  message("diag.action.resume_replenishment", "Action", item.Action),
			},
		}
	default:
		return diag.Finding{
			Check: diag.CheckStandbyReplenishment, Severity: diag.SeverityInfo,
			Summary: "standby replenishment is stopped for an unrecognized reason", Target: item.Root,
			Cause: fmt.Sprintf("the suspension records reason %s with detail %s, and this wx binary cannot explain what stopped it",
				item.Reason, item.Detail),
			Action: "run " + item.Action + " to resume it once you know the stop was not needed",
			Messages: diag.FindingMessages{
				Summary: message("diag.standby.stopped_unknown"),
				Cause:   message("diag.standby.unknown_cause", "Reason", item.Reason, "Detail", item.Detail),
				Action:  message("diag.action.resume_after_review", "Action", item.Action),
			},
		}
	}
}

// jobFailureCause は job の失敗を「失敗した操作・記録された理由・詳細ログの場所」の順で 1 行にまとめる。
// 理由が残っていない場合は特定できていないことを明示し、上位の処理名を原因として言い換えない。
// operation と operationMessage は同じ操作名の英語本文と message で、呼び出し側が対で渡す。

// 記録された失敗理由と詳細ログの path は値そのものなので、訳さず不透明値として message へ渡す。
func jobFailureCause(operation string, operationMessage i18n.Message, failureCode, failureMessage, detailPath string) (string, i18n.Message) {
	cause := operation
	id := "diag.job.failed"
	if failureCode != "" {
		cause += " failed with " + failureCode
		id += "_code"
	}
	if failureCode == "" {
		cause += " failed"
	}
	if failureMessage != "" {
		cause += ": " + failureMessage
		id += "_reason"
	} else {
		cause += "; the failure reason was not recorded, so the root cause is unknown"
		id += "_unknown"
	}
	result := message(id, "Operation", operationMessage, "Code", failureCode, "Reason", failureMessage)
	if detailPath != "" {
		cause += " (command output in " + detailPath + ")"
		result = message("diag.job.failed_detail_path", "Cause", result, "Path", detailPath)
	}
	return cause, result
}
