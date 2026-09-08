package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/HappyOnigiri/WX/internal/archive"
	"github.com/HappyOnigiri/WX/internal/state"
)

// GCReason は GC が対象を処理できなかった理由を対象単位で返す。
// 安全のために保留した場合も、削除できなかった事実を呼出元が区別できるようにする。
type GCReason struct {
	Target string `json:"target"`
	Status string `json:"status"`
	Reason string `json:"reason"`
}

// GCResult は GC の予約・完了・保留・失敗を分けて報告する。
// REMOVE job は予約時点では物理削除が完了していないため、Scheduled と Completed を混同しない。
type GCResult struct {
	Candidates int        `json:"candidates"`
	Scheduled  int        `json:"scheduled"`
	Completed  int        `json:"completed"`
	Pending    int        `json:"pending"`
	Failed     int        `json:"failed"`
	Reasons    []GCReason `json:"reasons"`
}

// gcProgress は GC の内部処理で結果と呼出元へ返すエラーを同時に集約する。
type gcProgress struct {
	GCResult
	errs []error
}

func newGCProgress() gcProgress {
	return gcProgress{GCResult: GCResult{Reasons: []GCReason{}}}
}

func (p *gcProgress) addIssue(target, status, reason string, cause error) {
	if cause != nil {
		if reason == "" {
			reason = cause.Error()
		} else {
			reason = fmt.Sprintf("%s: %v", reason, cause)
		}
		p.errs = append(p.errs, fmt.Errorf("%s: %w", target, cause))
	}
	p.Reasons = append(p.Reasons, GCReason{Target: target, Status: status, Reason: reason})
}

func (p *gcProgress) addPending(target, reason string, cause error) {
	p.Pending++
	p.addIssue(target, "pending", reason, cause)
}

func (p *gcProgress) addFailed(target, reason string, cause error) {
	p.Failed++
	p.addIssue(target, "failed", reason, cause)
}

func (p *gcProgress) merge(other gcProgress) {
	p.Scheduled += other.Scheduled
	p.Completed += other.Completed
	p.Pending += other.Pending
	p.Failed += other.Failed
	p.Reasons = append(p.Reasons, other.Reasons...)
	p.errs = append(p.errs, other.errs...)
}

func (p *gcProgress) err() error {
	return errors.Join(p.errs...)
}

func (m *Manager) GC(ctx context.Context, dry bool) (GCResult, error) {
	progress := newGCProgress()
	cfg := m.Config()
	nowTime := time.Now().UTC()
	failedBefore := state.FormatTime(nowTime.Add(-cfg.Retention.FailedJob.Duration))
	eventBefore := state.FormatTime(nowTime.Add(-cfg.Retention.EventLog.Duration))
	tombstoneBefore := state.FormatTime(nowTime.Add(-cfg.Retention.ExpiredSessionTombstone.Duration))
	metadataCount, err := m.store.CountMetadataCandidates(ctx, failedBefore, eventBefore, tombstoneBefore)
	if err != nil {
		progress.addFailed("metadata", "metadata candidate query failed", err)
		return progress.GCResult, progress.err()
	}
	before := state.FormatTime(nowTime.Add(-cfg.Retention.EndedWorktree.Duration))
	items, err := m.store.GCCandidates(ctx, before)
	if err != nil {
		progress.addFailed("ended worktrees", "ended worktree candidate query failed", err)
		return progress.GCResult, progress.err()
	}
	warmOverrides := cfg.WarmCountOverrides()
	standbys, err := m.store.StandbyGCCandidates(ctx, state.FormatTime(nowTime.Add(-cfg.Retention.HotStandby.Duration)), cfg.Pool.WarmPerWorkspace, warmOverrides)
	if err != nil {
		progress.addFailed("standby worktrees", "standby candidate query failed", err)
		return progress.GCResult, progress.err()
	}
	quarantined, err := m.store.QuarantinedGCCandidates(ctx, state.FormatTime(nowTime.Add(-cfg.Retention.Quarantined.Duration)))
	if err != nil {
		progress.addFailed("quarantined worktrees", "quarantined candidate query failed", err)
		return progress.GCResult, progress.err()
	}
	cold, err := m.store.ColdRepositoryCandidatesForWarm(ctx, state.FormatTime(nowTime.Add(-cfg.Retention.HotStandby.Duration)), cfg.Pool.WarmPerWorkspace, warmOverrides)
	if err != nil {
		progress.addFailed("cold repositories", "cold repository candidate query failed", err)
		return progress.GCResult, progress.err()
	}
	expired, err := m.store.ExpiredSnapshots(ctx, state.FormatTime(nowTime))
	if err != nil {
		progress.addFailed("snapshots", "expired snapshot candidate query failed", err)
		return progress.GCResult, progress.err()
	}
	expiredSessions := map[string][]state.Snapshot{}
	for _, snapshot := range expired {
		expiredSessions[snapshot.SessionID] = append(expiredSessions[snapshot.SessionID], snapshot)
	}
	expiredWorkspaces, err := m.store.ExpiredWorkspaceSnapshotSessions(ctx, state.FormatTime(nowTime))
	if err != nil {
		progress.addFailed("workspace snapshots", "expired archive query failed", err)
		return progress.GCResult, progress.err()
	}
	for _, id := range expiredWorkspaces {
		if _, ok := expiredSessions[id]; !ok {
			expiredSessions[id] = nil
		}
	}
	wholeSlotRemoval := map[string]bool{}
	for _, standby := range standbys {
		wholeSlotRemoval[standby.SlotID] = true
	}
	totalCold := 0
	for _, candidate := range cold {
		if !wholeSlotRemoval[candidate.SlotID] {
			totalCold++
		}
	}
	progress.Candidates = metadataCount + len(items) + len(standbys) + len(quarantined) + len(expiredSessions) + totalCold
	if dry {
		// dry-run は状態を変更せず、候補が処理されずに残る見込みを pending として報告する。
		progress.Pending = progress.Candidates
		return progress.GCResult, nil
	}
	if err := m.store.PruneMetadata(ctx, failedBefore, eventBefore, tombstoneBefore); err != nil {
		progress.addFailed("metadata", "metadata pruning failed", err)
	} else {
		progress.Completed += metadataCount
	}
	progress.merge(m.scheduleColdRepositoryRemovals(ctx, cold, wholeSlotRemoval))
	progress.merge(m.scheduleStandbyRemovals(ctx, standbys))
	progress.merge(m.scheduleEndedWorktreeRemovals(ctx, items))
	progress.merge(m.scheduleQuarantinedRemovals(ctx, quarantined))
	archiveManager := m.newArchiveManager(cfg, state.Slot{})
	progress.merge(m.expireWorkspaceSnapshots(ctx, expiredSessions, &archiveManager))
	// retired rootはSQLiteの参照が消えた後にrowだけをpruneし、設定済みdirectory自体は削除しない。
	if err := m.store.PruneRoots(ctx); err != nil {
		progress.addFailed("roots", "retired worktree root metadata pruning failed", err)
	}
	return progress.GCResult, progress.err()
}

func (m *Manager) quarantineCleanupFailure(slotID string, runErr error) error {
	if !errors.Is(runErr, state.ErrOwnership) {
		return runErr
	}
	if quarantineErr := m.store.QuarantineMissingSlot(context.Background(), slotID, "WORKTREE_OWNERSHIP_UNCERTAIN"); quarantineErr != nil {
		m.log.Error("quarantine cleanup ownership failure", "slot_id", slotID, "error", quarantineErr)
		return errors.Join(runErr, fmt.Errorf("quarantine slot after ownership failure: %w", quarantineErr))
	}
	return runErr
}

func (m *Manager) scheduleColdRepositoryRemovals(ctx context.Context, candidates []state.ColdRepositoryCandidate, wholeSlotRemoval map[string]bool) gcProgress {
	progress := newGCProgress()
	for _, candidate := range candidates {
		if wholeSlotRemoval[candidate.SlotID] {
			continue
		}
		target := fmt.Sprintf("cold repository %s/%s", candidate.SlotID, candidate.RepositoryID)
		job, changed, err := m.store.ScheduleColdRepositoryRemoval(ctx, candidate)
		if err != nil {
			m.log.Error("cold repository removal scheduling failed", "slot_id", candidate.SlotID, "repository_id", candidate.RepositoryID, "error", err)
			progress.addFailed(target, "cold repository removal reservation failed", err)
			continue
		}
		if !changed {
			progress.addPending(target, "candidate changed before removal reservation", nil)
			continue
		}
		m.schedule(job)
		progress.Scheduled++
	}
	return progress
}

func (m *Manager) scheduleStandbyRemovals(ctx context.Context, candidates []state.StandbyGCCandidate) gcProgress {
	progress := newGCProgress()
	for _, candidate := range candidates {
		progress.merge(m.scheduleRemovalCandidate(ctx, candidate.SlotID, candidate.Path, "", "standby removal scheduling failed"))
	}
	return progress
}

func (m *Manager) scheduleEndedWorktreeRemovals(ctx context.Context, candidates []state.GCCandidate) gcProgress {
	progress := newGCProgress()
	for _, candidate := range candidates {
		progress.merge(m.scheduleRemovalCandidate(ctx, candidate.SlotID, candidate.Path, candidate.SessionID, "ended worktree removal scheduling failed"))
	}
	return progress
}

// scheduleQuarantinedRemovals は保持期限を過ぎた隔離・失敗 slot の回収を予約する。
func (m *Manager) scheduleQuarantinedRemovals(ctx context.Context, candidates []state.QuarantinedGCCandidate) gcProgress {
	progress := newGCProgress()
	for _, candidate := range candidates {
		target := "quarantined worktree " + candidate.SlotID
		job, changed, err := m.store.ScheduleQuarantinedRemoval(ctx, candidate.SlotID)
		if err != nil {
			m.log.Error("quarantined worktree removal scheduling failed", "slot_id", candidate.SlotID, "failure_code", candidate.FailureCode, "error", err)
			progress.addFailed(target, "quarantined worktree removal reservation failed", err)
			continue
		}
		if !changed {
			progress.addPending(target, "candidate changed before removal reservation", nil)
			continue
		}
		m.schedule(job)
		progress.Scheduled++
	}
	return progress
}

func (m *Manager) scheduleRemovalCandidate(ctx context.Context, slotID, path, sessionID, logMessage string) gcProgress {
	progress := newGCProgress()
	target := "worktree " + slotID
	job, changed, err := m.store.ScheduleRemoval(ctx, slotID, sessionID)
	if err != nil {
		m.log.Error(logMessage, "slot_id", slotID, "error", err)
		progress.addFailed(target, logMessage, err)
		return progress
	}
	if !changed {
		progress.addPending(target, "candidate changed before removal reservation", nil)
		return progress
	}
	m.schedule(job)
	progress.Scheduled++
	return progress
}

func (m *Manager) expireWorkspaceSnapshots(ctx context.Context, expiredSessions map[string][]state.Snapshot, archiveManager *archive.Manager) gcProgress {
	progress := newGCProgress()
	sessionIDs := make([]string, 0, len(expiredSessions))
	for sessionID := range expiredSessions {
		sessionIDs = append(sessionIDs, sessionID)
	}
	sort.Strings(sessionIDs)
	for _, sessionID := range sessionIDs {
		snapshots := expiredSessions[sessionID]
		ok := true
		var rootSnapshotOwner string
		var rootSnapshotOwnerHandle *os.Root
		var rootSnapshotOwnerRelease func()
		var found bool
		rootSnapshot, found, snapshotErr := m.store.WorkspaceSnapshot(ctx, sessionID)
		if snapshotErr != nil {
			progress.addPending("workspace snapshot "+sessionID, "snapshot metadata could not be read", snapshotErr)
			ok = false
		}
		if found {
			rootSnapshotOwner = strings.TrimSuffix(rootSnapshot.ArchivePath, string(filepath.Separator)+rootSnapshot.RelPath)
			if !filepath.IsLocal(rootSnapshot.RelPath) || rootSnapshot.RelPath == "." {
				progress.addFailed("workspace snapshot "+sessionID, "invalid registered snapshot path", errors.New("snapshot path is outside root"))
				ok = false
			} else {
				var openErr error
				rootSnapshotOwnerHandle, openErr = os.OpenRoot(rootSnapshotOwner)
				if openErr != nil && !errors.Is(openErr, os.ErrNotExist) {
					progress.addPending("workspace snapshot "+sessionID, "snapshot root could not be opened", openErr)
					ok = false
				}
				if rootSnapshotOwnerHandle != nil {
					rootSnapshotOwnerRelease = func() { _ = rootSnapshotOwnerHandle.Close() }
				}
			}
		}

		if !ok {
			if rootSnapshotOwnerRelease != nil {
				rootSnapshotOwnerRelease()
			}
			continue
		}
		for _, snapshot := range snapshots {
			repo, err := m.store.Repository(ctx, snapshot.RepositoryID)
			if err != nil {
				progress.addFailed("snapshot "+sessionID+"/"+snapshot.RepositoryID, "snapshot repository metadata could not be read", err)
				ok = false
				break
			}
			if err := archiveManager.DeleteSnapshotRefs(ctx, repo, snapshot); err != nil {
				progress.addFailed("snapshot "+sessionID+"/"+snapshot.RepositoryID, "snapshot recovery refs could not be deleted", err)
				ok = false
				break
			}
		}
		if ok && rootSnapshot.SessionID != "" && rootSnapshotOwnerHandle != nil {
			if err := removeRegisteredSnapshot(rootSnapshotOwnerHandle, rootSnapshot.RelPath); err != nil {
				progress.addFailed("workspace snapshot "+sessionID, "workspace snapshot archive could not be deleted", err)
				ok = false
			}
		}
		if ok {
			if err := m.store.ExpireSessionSnapshots(ctx, sessionID); err != nil {
				progress.addFailed("snapshots "+sessionID, "snapshot metadata could not be expired", err)
			} else {
				progress.Completed++
			}
		}
		if rootSnapshotOwnerRelease != nil {
			rootSnapshotOwnerRelease()
		}
	}
	return progress
}
