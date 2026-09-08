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
	if len(report.UnknownPaths) > 0 {
		paths := append([]string{}, report.UnknownPaths...)
		sort.Strings(paths)
		findings = append(findings, diag.Finding{
			Check: diag.CheckArtifactOwnership, Severity: diag.SeverityInfo,
			Summary: "a worktree root holds paths that are not registered slots",
			Cause:   fmt.Sprintf("%d path(s) under the wx roots have no slot record; wx neither adopts nor deletes them", len(paths)),
			Action:  "inspect them yourself and remove them manually if you no longer need them",
			Details: paths,
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
	findings = append(findings, missingArtifactFindings(report.Missing)...)
	findings = append(findings, recoveryRefFindings(report.MismatchedRefs, report.MissingRefs)...)
	for _, message := range report.Errors {
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
	}{
		{mismatched, "a recovery ref does not point at the snapshot object", "the ref exists but its object ID differs from the one recorded for the snapshot"},
		{missing, "a recovery ref recorded for a snapshot is missing", "the state database records the ref, but the source repository does not have it"},
	} {
		sorted := append([]recoveryRefIssue{}, group.issues...)
		sort.Slice(sorted, func(i, j int) bool { return sorted[i].key() < sorted[j].key() })
		for _, issue := range sorted {
			severity, action := diag.SeverityProblem, "keep the repository as it is and check whether another tool rewrote refs/wx/recovery; the session that owns this snapshot can no longer be resumed from it"
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
			Action: "fix the reported state database failure, then run wx doctor again",
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
			Action: "fix the reported state database failure, then run wx doctor again",
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
