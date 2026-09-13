package daemon

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"time"

	"github.com/HappyOnigiri/WX/internal/archive"
	"github.com/HappyOnigiri/WX/internal/diag"
	"github.com/HappyOnigiri/WX/internal/i18n"
	"github.com/HappyOnigiri/WX/internal/state"
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
		findings = append(findings, diag.Finding{
			Check: diag.CheckArtifactOwnership, Severity: diag.SeverityInfo,
			Summary: "a worktree root holds entities the database does not explain",
			Cause:   unmanagedArtifactCause(unmanaged),
			Action:  "review them with wx clear --unmanaged --dry-run, then run wx clear --unmanaged to delete them",
			Details: unmanagedArtifactPaths(unmanaged),
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
			Details: refs,
		})
	}
	findings = append(findings, m.quarantinedRecoveryFindings(ctx)...)
	findings = append(findings, unreadableRepositoryFindings(report.UnreadableRepositories)...)
	findings = append(findings, refListFailureFindings(report.RefListFailures)...)
	findings = append(findings, missingArtifactFindings(report.Missing)...)
	findings = append(findings, recoveryRefFindings(report.MismatchedRefs, report.MissingRefs)...)
	findings = append(findings, submoduleRefFindings(report.SubmoduleRefIssues)...)
	// 同じ root の同じ失敗を照合と列挙の両方が報告するので、文言で畳んで 1 件ずつにする。
	for _, message := range mergedOwnershipErrors(report.Errors, unmanagedErrs) {
		findings = append(findings, diag.Finding{
			Check: diag.CheckArtifactOwnership, Severity: diag.SeverityUnchecked,
			Summary: "an ownership check could not be completed", Cause: message,
			Action: "fix the reported cause; wx cannot tell whether the affected artifact is still needed until then",
		})
	}
	if len(findings) == 0 {
		findings = append(findings, diag.Finding{
			Check: diag.CheckArtifactOwnership, Severity: diag.SeverityOK,
			Summary: "the registered slots and recovery refs match their artifacts",
		})
	}
	return findings
}

// unmanagedArtifactCause は登録外の実体を種別ごとの件数で説明する。
// 対処が「自分で消す」から「wx clear --unmanaged で消す」へ変わったので、何が消えるのかを種別で示す。
func unmanagedArtifactCause(artifacts []unmanagedArtifact) string {
	directories, snapshots := 0, 0
	for _, artifact := range artifacts {
		if artifact.Kind == unmanagedWorkspaceSnapshot {
			snapshots++
			continue
		}
		directories++
	}
	return fmt.Sprintf("%d slot directory/directories and %d workspace snapshot archive(s) under the wx namespaces have no database record; wx neither adopts them nor deletes them on its own",
		directories, snapshots)
}

// mergedOwnershipErrors は照合と列挙が出した失敗を、同じ文言を 1 件に畳んで順序を保ったまま返す。
func mergedOwnershipErrors(groups ...[]string) []string {
	seen := map[string]bool{}
	merged := []string{}
	for _, group := range groups {
		for _, message := range group {
			if seen[message] {
				continue
			}
			seen[message] = true
			merged = append(merged, message)
		}
	}
	return merged
}

// quarantinedRecoveryFindings は recovery ref を失って隔離された復元資産を workspace ごとに報告する。
// 隔離すると ref の照合対象から外れて他の finding が消えるため、ここで報告しないと行き止まりが黙って残る。
func (m *Manager) quarantinedRecoveryFindings(ctx context.Context) []diag.Finding {
	groups, err := m.store.QuarantinedRecoveryGroups(ctx)
	if err != nil {
		return []diag.Finding{{
			Check: diag.CheckArtifactOwnership, Severity: diag.SeverityUnchecked,
			Summary: "the quarantined recovery records could not be read", Cause: err.Error(),
			Action:   "fix the reported state database failure, then run wx doctor again",
			Messages: diag.FindingMessages{Action: i18n.Message{ID: "diag.action.fix_state_database"}},
		}}
	}
	findings := make([]diag.Finding, 0, len(groups))
	for _, group := range groups {
		findings = append(findings, diag.Finding{
			Check: diag.CheckArtifactOwnership, Severity: diag.SeverityProblem,
			Summary: "sessions of a workspace can no longer be restored because their recovery refs are gone", Target: group.Root,
			Cause:  fmt.Sprintf("%d session(s) hold %d quarantined snapshot(s) whose recovery refs are not in the source repository, which also stops wx forget", group.Sessions, group.Snapshots),
			Action: "check the sessions with wx discard-recovery " + group.Root + " --dry-run, then discard them with the same command without --dry-run; wx keeps the records until you do",
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
			Action: "make that path a readable Git repository again, or run wx forget on the workspaces that use it if you no longer need them",
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
			})
			continue
		}
		if !requiredSlotStates[item.State] {
			findings = append(findings, diag.Finding{
				Check: diag.CheckArtifactOwnership, Severity: diag.SeverityInfo,
				Summary: "a registered slot directory is missing", Target: item.Path,
				Cause:  fmt.Sprintf("slot %s is %s, and its directory does not exist", item.SlotID, item.State),
				Action: "no action is required; wx recreates or reclaims the slot on its own",
			})
			continue
		}
		findings = append(findings, diag.Finding{
			Check: diag.CheckArtifactOwnership, Severity: diag.SeverityProblem,
			Summary: "a slot directory that still holds work is missing", Target: item.Path,
			Cause:  fmt.Sprintf("slot %s is %s, so wx still needs its directory, but the directory does not exist", item.SlotID, item.State),
			Action: "restore the directory from your own backup if you need its unsaved work; wx cannot recreate it, and the slot stays quarantined until you remove it with wx clear",
		})
	}
	return findings
}

// recoveryRefFindings は復元に使う ref の不一致・欠損を報告する。
// 期限切れの snapshot を支える ref は復元の対象ではないため、参考情報に留める。
func recoveryRefFindings(mismatched, missing []recoveryRefIssue) []diag.Finding {
	findings := []diag.Finding{}
	for _, group := range []struct {
		issues  []recoveryRefIssue
		summary string
		cause   string
		action  string
	}{
		{
			mismatched, "a recovery ref does not point at the snapshot object", "the ref exists but its object ID differs from the one recorded for the snapshot",
			"keep the repository as it is and check whether another tool rewrote refs/wx/recovery; the session that owns this snapshot can no longer be resumed from it",
		},
		{
			missing, "a recovery ref recorded for a snapshot is missing", "the state database records the ref, but the source repository does not have it",
			"keep the repository as it is and check whether it was recreated or another tool rewrote refs/wx/recovery; the session that owns this snapshot can no longer be resumed from it, and wx discard-recovery <workspace-path> discards the state it left behind",
		},
	} {
		sorted := append([]recoveryRefIssue{}, group.issues...)
		sort.Slice(sorted, func(i, j int) bool { return sorted[i].key() < sorted[j].key() })
		for _, issue := range sorted {
			severity, action := diag.SeverityProblem, group.action
			if expiredRecoverySnapshot(issue.ExpiresAt) {
				severity = diag.SeverityInfo
				action = "no action is required; the snapshot behind this ref has expired and wx removes the record on its next collection"
			}
			findings = append(findings, diag.Finding{
				Check: diag.CheckArtifactOwnership, Severity: severity, Summary: group.summary, Target: issue.key(),
				Cause: group.cause, Action: action,
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
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].ModuleDir != sorted[j].ModuleDir {
			return sorted[i].ModuleDir < sorted[j].ModuleDir
		}
		return sorted[i].Ref < sorted[j].Ref
	})
	findings := make([]diag.Finding, 0, len(sorted))
	for _, issue := range sorted {
		if issue.Kind == submoduleRefUnknown {
			findings = append(findings, diag.Finding{
				Check: diag.CheckArtifactOwnership, Severity: diag.SeverityInfo,
				Summary: "an orphan submodule recovery ref remains in a local module", Target: issue.ModuleDir,
				Cause:  "ref " + issue.Ref + " has no submodule snapshot record in the state database",
				Action: "remove it yourself with git update-ref -d inside that module if you no longer need it; wx prune does not cover the module ref store",
			})
			continue
		}
		severity, action := diag.SeverityProblem, "keep the module as it is and check whether another tool rewrote refs/wx/recovery in it; the work saved for submodule "+issue.Path+" can no longer be restored"
		if expiredRecoverySnapshot(issue.ExpiresAt) {
			severity = diag.SeverityInfo
			action = "no action is required; the snapshot behind this ref has expired and wx removes the record on its next collection"
		}
		summary := "a submodule recovery ref recorded for a snapshot is missing"
		cause := "the state database records ref " + issue.Ref + " for submodule " + issue.Path + ", but its local module does not have it"
		if issue.Kind == submoduleRefMismatched {
			summary = "a submodule recovery ref does not point at the snapshot object"
			cause = "ref " + issue.Ref + " exists in the local module of submodule " + issue.Path + " but its object ID differs from the recorded one"
		}
		findings = append(findings, diag.Finding{
			Check: diag.CheckArtifactOwnership, Severity: severity, Summary: summary, Target: issue.ModuleDir,
			Cause: cause, Action: action,
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
		return []diag.Finding{{
			Check: diag.CheckRecoveryJobs, Severity: diag.SeverityProblem,
			Summary: "the recovery job history could not be read", Cause: err.Error(),
			Action:   "fix the reported state database failure, then run wx doctor again",
			Messages: diag.FindingMessages{Action: i18n.Message{ID: "diag.action.fix_state_database"}},
		}}
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
	})
}

func recoveryFailureFinding(failure state.RecoveryFailure) diag.Finding {
	target := failure.SlotPath
	if target == "" {
		target = "session " + failure.SessionID
	}
	summary := "restoring a session workspace failed and has not been retried"
	action := "fix the reported cause, then resume that conversation again; wx keeps the recovery snapshot until its retention elapses"
	if failure.Kind == "SNAPSHOT" {
		summary = "saving a session workspace failed, so its work is not snapshotted"
		action = snapshotFailureAction(failure.SlotState)
	}
	details := []string{"job " + failure.JobID}
	if failure.ParentSessionID != "" {
		// 復元先の session は失敗後に終端するため、利用者が再開し直す対象として元 session を先に示す。
		details = append(details, "restoring session "+failure.ParentSessionID)
	}
	if failure.SessionState != "" {
		details = append(details, "session state "+failure.SessionState)
	}
	if failure.SlotState != "" {
		details = append(details, "slot state "+failure.SlotState)
	}
	if failure.FinishedAt != "" {
		details = append(details, "failed at "+failure.FinishedAt)
	}
	return diag.Finding{
		Check: diag.CheckRecoveryJobs, Severity: diag.SeverityProblem, Summary: summary, Target: target,
		Cause:   jobFailureCause(failure.Kind+" job "+failure.JobID, failure.FailureCode, failure.FailureMessage, failure.DetailPath),
		Action:  action,
		Details: details,
	}
}

// snapshotFailureAction は保存失敗後の対処を slot の状態で分ける。
// 隔離済みの slot は session が終端しており、再終了しても snapshot を作り直さないため、手動退避だけを案内する。
func snapshotFailureAction(slotState string) string {
	if slotState == "QUARANTINED" {
		return "copy anything you need out of the slot directory yourself; wx cannot retry the snapshot for a quarantined slot, and the slot stays until you remove it with wx clear"
	}
	return "fix the reported cause and leave the slot alone; wx recreates the snapshot job while the slot is still returning, and you can copy anything you need out of the slot directory first"
}

// workspaceSnapshotFindings は復元に使える workspace snapshot の実体を軽量に検査する。
// archive 本文の SHA256 は読まないため、ここで正常でも内容の完全性は保証しない。
func (m *Manager) workspaceSnapshotFindings(ctx context.Context) []diag.Finding {
	at := time.Now()
	snapshots, err := m.store.ActiveWorkspaceSnapshots(ctx, state.FormatTime(at))
	if err != nil {
		return []diag.Finding{{
			Check: diag.CheckWorkspaceSnapshots, Severity: diag.SeverityProblem,
			Summary: "the workspace snapshot records could not be read", Cause: err.Error(),
			Action:   "fix the reported state database failure, then run wx doctor again",
			Messages: diag.FindingMessages{Action: i18n.Message{ID: "diag.action.fix_state_database"}},
		}}
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
		}, true
	}
	owner, release, err := m.existingRootDescriptor(root)
	if err != nil {
		return diag.Finding{
			Check: diag.CheckWorkspaceSnapshots, Severity: diag.SeverityUnchecked,
			Summary: "a workspace snapshot archive could not be inspected", Target: snapshot.ArchivePath,
			Cause:  fmt.Sprintf("open the owning root %s: %v", root, err),
			Action: "fix the reported cause on that root, then run wx doctor again",
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
		}, true
	}
}
