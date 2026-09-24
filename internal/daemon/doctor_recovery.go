package daemon

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"time"

	"github.com/HappyOnigiri/WorktreeX/internal/archive"
	"github.com/HappyOnigiri/WorktreeX/internal/diag"
	"github.com/HappyOnigiri/WorktreeX/internal/i18n"
	"github.com/HappyOnigiri/WorktreeX/internal/state"
)

// requiredSlotStates は実体が欠けていると起動中の作業か未保存の作業を失う slot の状態である。
// 待機用・準備中・回収対象の欠損は cold start か GC で解消するため、ここには含めない。
// SNAPSHOTTED も保存が済んでいて復元は新しい slot へ archive から行うため、含めない。
var requiredSlotStates = map[string]bool{
	"LEASED": true, "DRAINING": true, "SNAPSHOTTING": true,
}

// artifactFindings は所有権の照合結果を内訳ごとに分ける。
// 登録外の path と孤児 ref は存在するだけでは参考情報とし、自動採用・自動削除の案内はしない。
func (m *Manager) artifactFindings(ctx context.Context) []diag.Finding {
	report := m.artifactOwnershipReport(ctx)
	findings := []diag.Finding{}
	// 登録外の実体は reconcile が記録する狭い集合ではなく、削除できる集合そのものを材料にする。
	// 表示された対象が `wx clear --unmanaged` で必ず解消できる、という関係を doctor 側でも保つためである。
	unmanaged, unmanagedErrs := m.scanUnmanagedArtifacts(ctx)
	if len(unmanaged) > 0 {
		cause, causeMessage := unmanagedArtifactCause(unmanaged)
		findings = append(findings, diag.Finding{
			Check: diag.CheckArtifactOwnership, Severity: diag.SeverityInfo,
			Summary: "a worktree root holds entities the database does not explain",
			Cause:   cause,
			Action:  "review them with wx clear --unmanaged --dry-run, then run wx clear --unmanaged to delete them",
			// Details は登録外の実体の path そのものなので訳さない。
			Details: unmanagedArtifactPaths(unmanaged),
			Messages: diag.FindingMessages{
				Summary: message("diag.ownership.unmanaged"),
				Cause:   causeMessage,
				Action:  message("diag.action.clear_unmanaged"),
			},
		})
	}
	if len(report.UnknownRefs) > 0 {
		refs := refKeys(report.UnknownRefs)
		sort.Strings(refs)
		findings = append(findings, diag.Finding{
			Check: diag.CheckArtifactOwnership, Severity: diag.SeverityInfo,
			Summary: "orphan recovery refs remain in a source repository",
			Cause:   fmt.Sprintf("%d ref(s) under refs/wx/recovery have no snapshot record in the state database", len(refs)),
			Action:  "review them with wx prune --dry-run, then run wx prune to remove them",
			// Details は ref 名そのものなので訳さない。
			Details: refs,
			Messages: diag.FindingMessages{
				Summary: message("diag.ownership.orphan_refs"),
				Cause:   message("diag.ownership.orphan_refs_cause", "Count", len(refs)),
				Action:  message("diag.action.prune_refs"),
			},
		})
	}
	findings = append(findings, m.quarantinedRecoveryFindings(ctx)...)
	findings = append(findings, unreadableRepositoryFindings(report.UnreadableRepositories)...)
	findings = append(findings, refListFailureFindings(report.RefListFailures)...)
	findings = append(findings, missingArtifactFindings(report.Missing)...)
	findings = append(findings, recoveryRefFindings(report.MismatchedRefs, report.MissingRefs)...)
	findings = append(findings, submoduleRefFindings(report.SubmoduleRefIssues)...)
	// 同じ root の同じ失敗を照合と列挙の両方が報告するので、文言で畳んで 1 件ずつにする。
	// Cause は照合と列挙が出した失敗の本文そのものなので訳さない。
	for _, failure := range mergedOwnershipErrors(report.Errors, unmanagedErrs) {
		action, actionMessage := ownershipFailureAction(failure)
		findings = append(findings, diag.Finding{
			Check: diag.CheckArtifactOwnership, Severity: diag.SeverityUnchecked,
			Summary: "an ownership check could not be completed", Target: failure.Target, Cause: failure.Message,
			Action: action,
			Messages: diag.FindingMessages{
				Summary: message("diag.ownership.incomplete"),
				Action:  actionMessage,
			},
		})
	}
	if len(findings) == 0 {
		findings = append(findings, diag.Finding{
			Check: diag.CheckArtifactOwnership, Severity: diag.SeverityOK,
			Summary:  "the registered slots and recovery refs match their artifacts",
			Messages: diag.FindingMessages{Summary: message("diag.ownership.match")},
		})
	}
	return findings
}

// unmanagedArtifactCause は登録外の実体を種別ごとの件数で説明する。
// 対処が「自分で消す」から「wx clear --unmanaged で消す」へ変わったので、何が消えるのかを種別で示す。
func unmanagedArtifactCause(artifacts []unmanagedArtifact) (string, i18n.Message) {
	directories, snapshots := 0, 0
	for _, artifact := range artifacts {
		if artifact.Kind == unmanagedWorkspaceSnapshot {
			snapshots++
			continue
		}
		directories++
	}
	return fmt.Sprintf("%d slot directory/directories and %d workspace snapshot archive(s) under the wx namespaces have no database record; wx neither adopts them nor deletes them on its own",
			directories, snapshots),
		message("diag.ownership.unmanaged_cause", "Directories", directories, "Snapshots", snapshots)
}

// mergedOwnershipErrors は照合と列挙が出した失敗を、同じ文言を 1 件に畳んで順序を保ったまま返す。
func mergedOwnershipErrors(groups ...[]ownershipFailure) []ownershipFailure {
	seen := map[string]bool{}
	merged := []ownershipFailure{}
	for _, group := range groups {
		for _, failure := range group {
			if seen[failure.Message] {
				continue
			}
			seen[failure.Message] = true
			merged = append(merged, failure)
		}
	}
	return merged
}

// ownershipFailureAction は照合を完了できなかった原因の種別ごとに手順を返す。
// 判定できない実体は wx が採用も削除もしないため、対処は「読めるようにする」か「登録を畳む」のどちらかになる。
func ownershipFailureAction(failure ownershipFailure) (string, i18n.Message) {
	switch failure.Kind {
	case ownershipFailureStore:
		return stateQueryFailureAction()
	case ownershipFailureSlotPath:
		return fmt.Sprintf("check that %s is readable by you and that its volume is mounted, then run wx doctor again; wx neither adopts nor deletes a slot it cannot inspect", failure.Target),
			message("diag.action.check_slot_path", "Path", failure.Target)
	case ownershipFailureRootPath:
		return fmt.Sprintf("check that %s is a directory you own with 0700 access and that its volume is mounted, then run wx doctor again", failure.Target),
			message("diag.action.check_root_path", "Path", failure.Target)
	case ownershipFailureRepositoryRef:
		return fmt.Sprintf("run git -C %s for-each-ref refs/wx/recovery to see what wx reads there, then run wx doctor again", failure.Target),
			message("diag.action.check_recovery_refs", "Path", failure.Target)
	default:
		return "run wx doctor again after making the target in the cause readable; wx cannot tell whether the affected artifact is still needed until it can read it",
			message("diag.action.retry_ownership_check")
	}
}

// quarantinedRecoveryFindings は recovery ref を失って隔離された復元資産を workspace ごとに報告する。
// 隔離すると ref の照合対象から外れて他の finding が消えるため、ここで報告しないと行き止まりが黙って残る。
func (m *Manager) quarantinedRecoveryFindings(ctx context.Context) []diag.Finding {
	groups, err := m.store.QuarantinedRecoveryGroups(ctx)
	if err != nil {
		finding := stateQueryProblem(diag.CheckArtifactOwnership,
			"the quarantined recovery records could not be read", message("diag.recovery.quarantine_unreadable"), "", err)
		finding.Severity = diag.SeverityUnchecked
		return []diag.Finding{finding}
	}
	findings := make([]diag.Finding, 0, len(groups))
	for _, group := range groups {
		findings = append(findings, diag.Finding{
			Check: diag.CheckArtifactOwnership, Severity: diag.SeverityProblem,
			Summary: "sessions of a workspace can no longer be restored because their recovery refs are gone", Target: group.Root,
			Cause:  fmt.Sprintf("%d session(s) hold %d quarantined snapshot(s) whose recovery refs are not in the source repository, which also stops wx forget", group.Sessions, group.Snapshots),
			Action: "check the sessions with wx discard-recovery " + group.Root + " --dry-run, then discard them with the same command without --dry-run; wx keeps the records until you do",
			Messages: diag.FindingMessages{
				Summary: message("diag.recovery.quarantined_sessions"),
				Cause:   message("diag.recovery.quarantined_sessions_cause", "Sessions", group.Sessions, "Snapshots", group.Snapshots),
				Action:  message("diag.action.discard_recovery", "Root", group.Root),
			},
		})
	}
	return findings
}

// unreadableRepositoryFindings は照合対象を持たない読めない repository 記録を参考情報として並べる。
// 回収は GC が行い利用者の操作を要さないため対処は案内せず、doctor をこの記録で失敗させない。
func unreadableRepositoryFindings(repositories []unreadableRepository) []diag.Finding {
	sorted := append([]unreadableRepository{}, repositories...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].RepositoryID < sorted[j].RepositoryID })
	findings := make([]diag.Finding, 0, len(sorted))
	for _, repository := range sorted {
		findings = append(findings, diag.Finding{
			Check: diag.CheckArtifactOwnership, Severity: diag.SeverityInfo,
			Summary: "a repository record no longer points at a readable repository", Target: repository.Path,
			Cause:  fmt.Sprintf("repository %s cannot be read (%s), and no snapshot in the database needs its recovery refs", repository.RepositoryID, repository.Cause),
			Action: "no action is required; wx removes the record on its next collection and registers the repository again if you use that path",
			Messages: diag.FindingMessages{
				Summary: message("diag.ownership.repository_unreadable"),
				Cause:   message("diag.ownership.repository_unreadable_cause", "Repository", repository.RepositoryID, "Error", repository.Cause),
				Action:  message("diag.action.repository_record_cleanup"),
			},
		})
	}
	return findings
}

// refListFailureFindings は recovery ref を読めなかった repository を 1 件ずつ問題として並べる。
// 影響はその repository の照合だけなので、他の検査結果を巻き込む unchecked ではなく対象つきの問題として出す。
func refListFailureFindings(failures []unreadableRepository) []diag.Finding {
	sorted := append([]unreadableRepository{}, failures...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].RepositoryID < sorted[j].RepositoryID })
	findings := make([]diag.Finding, 0, len(sorted))
	for _, failure := range sorted {
		findings = append(findings, diag.Finding{
			Check: diag.CheckArtifactOwnership, Severity: diag.SeverityProblem,
			Summary: "the recovery refs of one repository could not be listed", Target: failure.Path,
			Cause:  fmt.Sprintf("repository %s is still in use, but its refs could not be read (%s); the other repositories were checked", failure.RepositoryID, failure.Cause),
			Action: fmt.Sprintf("run git -C %s rev-parse --git-dir to see why wx cannot read it, and restore it from its remote if it is gone; if you no longer need the workspaces that use it, run wx forget <workspace-path> on each and then wx forget --discard-recovery <workspace-path> for the ones it refuses while they still hold recovery state", failure.Path),
			Messages: diag.FindingMessages{
				Summary: message("diag.ownership.ref_list_failed"),
				Cause:   message("diag.ownership.ref_list_failed_cause", "Repository", failure.RepositoryID, "Error", failure.Cause),
				Action:  message("diag.action.restore_repository", "Path", failure.Path),
			},
		})
	}
	return findings
}

func missingArtifactFindings(missing []missingArtifact) []diag.Finding {
	sorted := append([]missingArtifact{}, missing...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Path < sorted[j].Path })
	findings := make([]diag.Finding, 0, len(sorted))
	for _, item := range sorted {
		if item.State == "SNAPSHOTTED" {
			findings = append(findings, diag.Finding{
				Check: diag.CheckArtifactOwnership, Severity: diag.SeverityInfo,
				Summary: "a snapshotted slot directory is missing", Target: item.Path,
				Cause:  fmt.Sprintf("slot %s is SNAPSHOTTED, so its work is already saved, and only the leftover directory is gone", item.SlotID),
				Action: "no action is required; wx quarantines the slot record and its collection removes it",
				Messages: diag.FindingMessages{
					Summary: message("diag.ownership.snapshotted_missing"),
					Cause:   message("diag.ownership.snapshotted_missing_cause", "SlotID", item.SlotID),
					Action:  message("diag.action.slot_quarantine_cleanup"),
				},
			})
			continue
		}
		if !requiredSlotStates[item.State] {
			findings = append(findings, diag.Finding{
				Check: diag.CheckArtifactOwnership, Severity: diag.SeverityInfo,
				Summary: "a registered slot directory is missing", Target: item.Path,
				Cause:  fmt.Sprintf("slot %s is %s, and its directory does not exist", item.SlotID, item.State),
				Action: "no action is required; wx recreates or reclaims the slot on its own",
				Messages: diag.FindingMessages{
					Summary: message("diag.ownership.slot_missing"),
					Cause:   message("diag.ownership.slot_missing_cause", "SlotID", item.SlotID, "State", item.State),
					Action:  message("diag.action.slot_self_heal"),
				},
			})
			continue
		}
		findings = append(findings, diag.Finding{
			Check: diag.CheckArtifactOwnership, Severity: diag.SeverityProblem,
			Summary: "a slot directory that still holds work is missing", Target: item.Path,
			Cause:  fmt.Sprintf("slot %s is %s, so wx still needs its directory, but the directory does not exist", item.SlotID, item.State),
			Action: "restore the directory from your own backup if you need its unsaved work; wx cannot recreate it, and the slot stays quarantined until you remove it with wx clear",
			Messages: diag.FindingMessages{
				Summary: message("diag.ownership.working_slot_missing"),
				Cause:   message("diag.ownership.working_slot_missing_cause", "SlotID", item.SlotID, "State", item.State),
				Action:  message("diag.action.restore_slot_directory"),
			},
		})
	}
	return findings
}

// recoveryRefFindings は復元に使う ref の不一致・欠損を報告する。
// 期限切れの snapshot を支える ref は復元の対象ではないため、参考情報に留める。
func recoveryRefFindings(mismatched, missing []recoveryRefIssue) []diag.Finding {
	findings := []diag.Finding{}
	for _, group := range []struct {
		issues    []recoveryRefIssue
		summary   string
		cause     string
		action    string
		summaryID string
		causeID   string
		actionID  string
	}{
		{
			mismatched, "a recovery ref does not point at the snapshot object", "the ref exists but its object ID differs from the one recorded for the snapshot",
			"keep the repository as it is and check whether another tool rewrote refs/wx/recovery; the session that owns this snapshot can no longer be resumed from it",
			"diag.recovery.ref_mismatched", "diag.recovery.ref_mismatched_cause", "diag.action.recovery_ref_mismatched",
		},
		{
			missing, "a recovery ref recorded for a snapshot is missing", "the state database records the ref, but the source repository does not have it",
			"keep the repository as it is and check whether it was recreated or another tool rewrote refs/wx/recovery; the session that owns this snapshot can no longer be resumed from it, and wx discard-recovery <workspace-path> discards the state it left behind",
			"diag.recovery.ref_missing", "diag.recovery.ref_missing_cause", "diag.action.recovery_ref_missing",
		},
	} {
		sorted := append([]recoveryRefIssue{}, group.issues...)
		sort.Slice(sorted, func(i, j int) bool { return sorted[i].key() < sorted[j].key() })
		for _, issue := range sorted {
			severity, action, actionID := diag.SeverityProblem, group.action, group.actionID
			if expiredRecoverySnapshot(issue.ExpiresAt) {
				severity = diag.SeverityInfo
				action = "no action is required; the snapshot behind this ref has expired and wx removes the record on its next collection"
				actionID = "diag.action.recovery_ref_expired"
			}
			findings = append(findings, diag.Finding{
				Check: diag.CheckArtifactOwnership, Severity: severity, Summary: group.summary, Target: issue.key(),
				Cause: group.cause, Action: action,
				Messages: diag.FindingMessages{
					Summary: message(group.summaryID),
					Cause:   message(group.causeID),
					Action:  message(actionID),
				},
			})
		}
	}
	return findings
}

// submoduleRefFindings は子の capsule ref の不整合を報告する。対象はローカル module の path で示す。
// ref の公開先が repository ごとの ref store ではないため、`wx prune` の `<repository_id>:<ref>` では指せず削除も案内できない。
// 欠落と不一致はその submodule の作業が復元できないことの予告なので、期限内なら問題として出す。
func submoduleRefFindings(issues []submoduleRefIssue) []diag.Finding {
	sorted := append([]submoduleRefIssue{}, issues...)
	slices.SortStableFunc(sorted, func(left, right submoduleRefIssue) int {
		if order := cmp.Compare(left.ModuleDir, right.ModuleDir); order != 0 {
			return order
		}
		return cmp.Compare(left.Ref, right.Ref)
	})
	findings := make([]diag.Finding, 0, len(sorted))
	for _, issue := range sorted {
		if issue.Kind == submoduleRefUnknown {
			findings = append(findings, diag.Finding{
				Check: diag.CheckArtifactOwnership, Severity: diag.SeverityInfo,
				Summary: "an orphan submodule recovery ref remains in a local module", Target: issue.ModuleDir,
				Cause:  "ref " + issue.Ref + " has no submodule snapshot record in the state database",
				Action: "remove it yourself with git update-ref -d inside that module if you no longer need it; wx prune does not cover the module ref store",
				Messages: diag.FindingMessages{
					Summary: message("diag.recovery.submodule_ref_orphan"),
					Cause:   message("diag.recovery.submodule_ref_orphan_cause", "Ref", issue.Ref),
					Action:  message("diag.action.submodule_ref_orphan"),
				},
			})
			continue
		}
		severity, action := diag.SeverityProblem, "keep the module as it is and check whether another tool rewrote refs/wx/recovery in it; the work saved for submodule "+issue.Path+" can no longer be restored"
		actionMessage := message("diag.action.submodule_ref_broken", "Path", issue.Path)
		if expiredRecoverySnapshot(issue.ExpiresAt) {
			severity = diag.SeverityInfo
			action = "no action is required; the snapshot behind this ref has expired and wx removes the record on its next collection"
			actionMessage = message("diag.action.recovery_ref_expired")
		}
		summary := "a submodule recovery ref recorded for a snapshot is missing"
		cause := "the state database records ref " + issue.Ref + " for submodule " + issue.Path + ", but its local module does not have it"
		summaryMessage := message("diag.recovery.submodule_ref_missing")
		causeMessage := message("diag.recovery.submodule_ref_missing_cause", "Ref", issue.Ref, "Path", issue.Path)
		if issue.Kind == submoduleRefMismatched {
			summary = "a submodule recovery ref does not point at the snapshot object"
			cause = "ref " + issue.Ref + " exists in the local module of submodule " + issue.Path + " but its object ID differs from the recorded one"
			summaryMessage = message("diag.recovery.submodule_ref_mismatched")
			causeMessage = message("diag.recovery.submodule_ref_mismatched_cause", "Ref", issue.Ref, "Path", issue.Path)
		}
		findings = append(findings, diag.Finding{
			Check: diag.CheckArtifactOwnership, Severity: severity, Summary: summary, Target: issue.ModuleDir,
			Cause: cause, Action: action,
			Messages: diag.FindingMessages{Summary: summaryMessage, Cause: causeMessage, Action: actionMessage},
		})
	}
	return findings
}

// expiredRecoverySnapshot は ref を支える snapshot が期限切れかを返す。
// 期限を読めない場合は期限内として扱い、復元できるかもしれない対象を参考情報へ落とさない。
func expiredRecoverySnapshot(expiresAt string) bool {
	if expiresAt == "" {
		return false
	}
	expires, err := time.Parse(time.RFC3339Nano, expiresAt)
	if err != nil {
		return false
	}
	return !expires.After(time.Now())
}

// recoveryFailureFindings は保存・復元の未解消の失敗を報告する。
// 後続の実行で解消した履歴は state.UnresolvedRecoveryFailures が除くため、ここでは原因と対処だけを組み立てる。
func (m *Manager) recoveryFailureFindings(ctx context.Context) []diag.Finding {
	failures, err := m.store.UnresolvedRecoveryFailures(ctx)
	if err != nil {
		return []diag.Finding{stateQueryProblem(diag.CheckRecoveryJobs,
			"the recovery job history could not be read", message("diag.recovery.history_unreadable"), "", err)}
	}
	findings := make([]diag.Finding, 0, len(failures)+1)
	for _, failure := range failures {
		findings = append(findings, recoveryFailureFinding(failure))
	}
	if len(failures) > 0 {
		return findings
	}
	return append(findings, diag.Finding{
		Check: diag.CheckRecoveryJobs, Severity: diag.SeverityOK,
		Summary: "no save or restore failure is outstanding", Details: []string{"0 unresolved failure(s)"},
		Messages: diag.FindingMessages{
			Summary: message("diag.recovery.no_failure"),
			Details: []i18n.Message{message("diag.detail.no_unresolved_failures")},
		},
	})
}

func recoveryFailureFinding(failure state.RecoveryFailure) diag.Finding {
	target := failure.SlotPath
	if target == "" {
		target = "session " + failure.SessionID
	}
	summary := "restoring a session workspace failed and has not been retried"
	summaryMessage := message("diag.recovery.restore_failed")
	lead, leadMessage := jobFailureLead(failure.DetailPath)
	action := lead + ", correct what it reports, then resume that conversation again; wx keeps the recovery snapshot until its retention elapses"
	actionMessage := message("diag.action.retry_restore", "Lead", leadMessage)
	if failure.Kind == "SNAPSHOT" {
		summary = "saving a session workspace failed, so its work is not snapshotted"
		summaryMessage = message("diag.recovery.snapshot_failed")
		action, actionMessage = snapshotFailureAction(failure.SlotState, lead, leadMessage)
	}
	details := []string{"job " + failure.JobID}
	detailMessages := []i18n.Message{message("diag.detail.job", "JobID", failure.JobID)}
	if failure.ParentSessionID != "" {
		// 復元先の session は失敗後に終端するため、利用者が再開し直す対象として元 session を先に示す。
		details = append(details, "restoring session "+failure.ParentSessionID)
		detailMessages = append(detailMessages, message("diag.detail.restoring_session", "SessionID", failure.ParentSessionID))
	}
	if failure.SessionState != "" {
		details = append(details, "session state "+failure.SessionState)
		detailMessages = append(detailMessages, message("diag.detail.session_state", "State", failure.SessionState))
	}
	if failure.SlotState != "" {
		details = append(details, "slot state "+failure.SlotState)
		detailMessages = append(detailMessages, message("diag.detail.slot_state", "State", failure.SlotState))
	}
	if failure.FinishedAt != "" {
		details = append(details, "failed at "+failure.FinishedAt)
		detailMessages = append(detailMessages, message("diag.detail.failed_at", "Time", failure.FinishedAt))
	}
	cause, causeMessage := jobFailureCause(failure.Kind+" job "+failure.JobID,
		message("diag.recovery.job_operation", "Kind", failure.Kind, "JobID", failure.JobID),
		failure.FailureCode, failure.FailureMessage, failure.DetailPath)
	return diag.Finding{
		Check: diag.CheckRecoveryJobs, Severity: diag.SeverityProblem, Summary: summary, Target: target,
		Cause:   cause,
		Action:  action,
		Details: details,
		Messages: diag.FindingMessages{
			Summary: summaryMessage, Cause: causeMessage, Action: actionMessage, Details: detailMessages,
		},
	}
}

// snapshotFailureAction は保存失敗後の対処を slot の状態で分ける。
// 隔離済みの slot は session が終端しており、再終了しても snapshot を作り直さないため、手動退避だけを案内する。
func snapshotFailureAction(slotState, lead string, leadMessage i18n.Message) (string, i18n.Message) {
	if slotState == "QUARANTINED" {
		return "copy anything you need out of the slot directory yourself; wx cannot retry the snapshot for a quarantined slot, and the slot stays until you remove it with wx clear",
			message("diag.action.snapshot_quarantined")
	}
	return lead + ", correct what it reports and leave the slot alone; wx recreates the snapshot job while the slot is still returning, and you can copy anything you need out of the slot directory first",
		message("diag.action.snapshot_retry", "Lead", leadMessage)
}

// workspaceSnapshotFindings は復元に使える workspace snapshot の実体を軽量に検査する。
// archive 本文の SHA256 は読まないため、ここで正常でも内容の完全性は保証しない。
func (m *Manager) workspaceSnapshotFindings(ctx context.Context) []diag.Finding {
	at := time.Now()
	snapshots, err := m.store.ActiveWorkspaceSnapshots(ctx, state.FormatTime(at))
	if err != nil {
		return []diag.Finding{stateQueryProblem(diag.CheckWorkspaceSnapshots,
			"the workspace snapshot records could not be read", message("diag.snapshot.records_unreadable"), "", err)}
	}
	findings := make([]diag.Finding, 0, len(snapshots)+1)
	for _, snapshot := range snapshots {
		if finding, ok := m.workspaceSnapshotFinding(snapshot, at); ok {
			findings = append(findings, finding)
		}
	}
	if len(findings) > 0 {
		return findings
	}
	return append(findings, diag.Finding{
		Check: diag.CheckWorkspaceSnapshots, Severity: diag.SeverityOK,
		Summary: "the workspace snapshot archives are in place",
		Details: []string{strconv.Itoa(len(snapshots)) + " archive(s) checked without reading their contents"},
		Messages: diag.FindingMessages{
			Summary: message("diag.snapshot.in_place"),
			Details: []i18n.Message{message("diag.detail.archives_checked", "Count", len(snapshots))},
		},
	})
}

// workspaceSnapshotFinding は snapshot 1 件の metadata と実体の種別を検査する。
// 検査と期限切れが競合した場合は問題として扱わず、finding を作らない。
func (m *Manager) workspaceSnapshotFinding(snapshot state.WorkspaceSnapshot, at time.Time) (diag.Finding, bool) {
	root, ok := m.rootForPath(snapshot.ArchivePath)
	if !ok {
		return diag.Finding{
			Check: diag.CheckWorkspaceSnapshots, Severity: diag.SeverityProblem,
			Summary: "a workspace snapshot archive is outside the known wx roots", Target: snapshot.ArchivePath,
			Cause:  fmt.Sprintf("session %s records this archive, but no registered root contains it", snapshot.SessionID),
			Action: "restore the worktree root that held the archive, or accept that this session cannot be resumed and let its snapshot expire",
			Messages: diag.FindingMessages{
				Summary: message("diag.snapshot.outside_roots"),
				Cause:   message("diag.snapshot.outside_roots_cause", "SessionID", snapshot.SessionID),
				Action:  message("diag.action.restore_snapshot_root"),
			},
		}, true
	}
	owner, release, err := m.existingRootDescriptor(root)
	if err != nil {
		return diag.Finding{
			Check: diag.CheckWorkspaceSnapshots, Severity: diag.SeverityUnchecked,
			Summary: "a workspace snapshot archive could not be inspected", Target: snapshot.ArchivePath,
			Cause:  fmt.Sprintf("open the owning root %s: %v", root, err),
			Action: fmt.Sprintf("check that %s is a directory you own with 0700 access and that its volume is mounted, then run wx doctor again", root),
			Messages: diag.FindingMessages{
				Summary: message("diag.snapshot.uninspectable"),
				Cause:   message("diag.snapshot.open_root_cause", "Root", root, "Error", err.Error()),
				Action:  message("diag.action.check_root_path", "Path", root),
			},
		}, true
	}
	defer release()
	validateErr := archive.ValidateWorkspaceSnapshotMetadataAt(root, owner, snapshot, at)
	switch {
	case validateErr == nil:
		return diag.Finding{}, false
	case errors.Is(validateErr, archive.ErrWorkspaceSnapshotExpired):
		return diag.Finding{}, false
	default:
		return diag.Finding{
			Check: diag.CheckWorkspaceSnapshots, Severity: diag.SeverityProblem,
			Summary: "a workspace snapshot cannot be used for restore", Target: snapshot.ArchivePath,
			Cause:  fmt.Sprintf("session %s: %v", snapshot.SessionID, validateErr),
			Action: "keep the archive as it is and check the path, its access, and the mount it lives on; the repositories of that session still restore from their recovery refs",
			Messages: diag.FindingMessages{
				Summary: message("diag.snapshot.unusable"),
				Cause:   message("diag.snapshot.unusable_cause", "SessionID", snapshot.SessionID, "Error", validateErr.Error()),
				Action:  message("diag.action.check_snapshot_archive"),
			},
		}, true
	}
}
