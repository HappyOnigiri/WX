package daemon

import (
	"context"
	"fmt"
	"time"

	"github.com/HappyOnigiri/WX/internal/archive"
	"github.com/HappyOnigiri/WX/internal/config"
	"github.com/HappyOnigiri/WX/internal/state"
)

func (m *Manager) newArchiveManager(cfg config.Config, slot state.Slot) archive.Manager {
	preparer := m.newPreparer(cfg, slot)
	return archive.Manager{Git: m.git, Preparer: preparer, Ownership: m.store}
}

func (m *Manager) Release(ctx context.Context, id, token, reason string) error {
	session, err := m.store.Session(ctx, id, token)
	if err != nil {
		return err
	}
	if reason == "session-end-hook" && (processAlive(session.ClientPID) || processAlive(session.AgentPID)) {
		return nil
	}
	job, changed, quarantineExpired, err := m.store.ReleaseWithOutcome(ctx, id, session.WorkspaceID, session.SlotID)
	if err != nil {
		return err
	}
	if quarantineExpired {
		// clientはReleaseの応答を読まないため、snapshotを作らずに終端したことはログだけが残す。
		m.log.Warn("session expired without a recovery snapshot: slot is quarantined", "session_id", id, "slot_id", session.SlotID, "reason", reason)
	}
	if !changed {
		m.releaseLease(id)
	} else {
		m.schedule(job)
	}
	// この session が wx new で用意した子貸出を、周期処理の 10 秒を待たずに返却する。
	// 正しさの根拠は reconcileExpiredLeases 側にあり、ここは待ち時間の最適化である。
	m.releaseOrphanedChildLeases(ctx)
	return nil
}

func (m *Manager) snapshotSession(ctx context.Context, s state.Session) error {
	if processAlive(s.AgentPID) {
		return dependencyPendingError{fmt.Errorf("agent process %d is still active", s.AgentPID)}
	}
	slot, err := m.store.Slot(ctx, s.SlotID)
	if err != nil {
		return err
	}
	if s.State == "ARCHIVED" && (slot.State == "SNAPSHOTTED" || slot.State == "ARCHIVED") {
		return nil
	}
	if slot.State == "DRAINING" || slot.State == "SNAPSHOTTING" {
		if err := m.store.BeginSnapshot(ctx, s.ID, s.SlotID); err != nil {
			return err
		}
	} else {
		return fmt.Errorf("slot %s cannot be snapshotted from %s", s.SlotID, slot.State)
	}
	repos, err := m.store.SlotRepositories(ctx, s.SlotID)
	if err != nil {
		return err
	}
	releasedAt, err := time.Parse(time.RFC3339Nano, s.ReleasedAt)
	if err != nil {
		return fmt.Errorf("session %s has invalid release time: %w", s.ID, err)
	}
	expiry := releasedAt.Add(m.Config().Retention.RecoverySnapshot.Duration)
	releaseRoot, err := m.holdRootForPath(slot.Path)
	if err != nil {
		m.quarantineOwnershipFailure(s.SlotID, []string{"SNAPSHOTTING"}, err)
		return err
	}
	defer releaseRoot()
	archiveManager := m.newArchiveManager(m.Config(), slot)
	for _, sr := range repos {
		repo, err := m.store.Repository(ctx, sr.RepositoryID)
		if err != nil {
			return err
		}
		_, err = archiveManager.SnapshotWithPersistence(ctx, repo, sr.WorktreePath, s.ID, expiry, func(snapshot state.Snapshot) error {
			return m.store.SaveSnapshot(ctx, snapshot)
		})
		if err != nil {
			m.log.Error("snapshot failed", "session_id", s.ID, "repository_id", repo.ID, "error", err)
			_ = m.store.SetSlotState(ctx, s.SlotID, []string{"SNAPSHOTTING"}, "QUARANTINED", "SNAPSHOT_FAILED")
			return err
		}
	}
	workspaceKind, err := m.store.SessionWorkspaceKind(ctx, s.ID)
	if err != nil {
		return err
	}
	if workspaceKind == "multi_repository" {
		ownershipRoot, ownershipRootID, rootIDErr := m.rootIDForPath(slot.Path)
		if rootIDErr != nil {
			return rootIDErr
		}
		ownershipRootHandle, closeOwnershipRoot, rootErr := m.existingRootDescriptor(ownershipRoot)
		if rootErr != nil {
			return fmt.Errorf("open workspace archive root: %w", rootErr)
		}
		defer closeOwnershipRoot()
		rootSnapshot, found, err := m.store.WorkspaceSnapshot(ctx, s.ID)
		if err != nil {
			return err
		}
		if found {
			if err := archive.ValidateWorkspaceSnapshotAt(ctx, ownershipRoot, ownershipRootHandle, rootSnapshot, time.Now()); err != nil {
				_ = m.store.SetSlotState(ctx, s.SlotID, []string{"SNAPSHOTTING"}, "QUARANTINED", "SNAPSHOT_FAILED")
				return fmt.Errorf("validate workspace root snapshot: %w", err)
			}
		} else {
			w, err := m.store.SessionWorkspace(ctx, s.ID)
			if err != nil {
				return err
			}
			rootRules, err := m.rootRules(w)
			if err != nil {
				return err
			}
			rootSnapshot, err = archive.SnapshotWorkspaceAt(ctx, slot.Path, ownershipRoot, ownershipRootID, ownershipRootHandle, s.ID, workspaceRecoveryExclusions(repos, rootRules), expiry)
			if err != nil {
				m.log.Error("workspace root snapshot failed", "session_id", s.ID, "error", err)
				_ = m.store.SetSlotState(ctx, s.SlotID, []string{"SNAPSHOTTING"}, "QUARANTINED", "SNAPSHOT_FAILED")
				return err
			}
			if err := m.store.SaveWorkspaceSnapshot(ctx, rootSnapshot); err != nil {
				return err
			}
		}
	}
	if err := m.store.MarkArchived(ctx, s.ID, s.SlotID, state.FormatTime(expiry)); err != nil {
		return err
	}
	// snapshot は管理対象の使用量を増やすため、周期測定を待たずに Disk へ反映する。
	m.remeasureRootUsage()
	return nil
}
