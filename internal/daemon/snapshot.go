package daemon

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/HappyOnigiri/WX/internal/archive"
	"github.com/HappyOnigiri/WX/internal/config"
	"github.com/HappyOnigiri/WX/internal/discovery"
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

// snapshotRepository は repository 1 個を保存し、保存できなかった submodule 作業を記録用の形へ畳んで返す。
// 理由コードは検出側が固定順で並べたものを `,` で連ね、DB へは 1 submodule 1 行として渡す。
// 子の snapshot 行は snapshots への外部キーを持つため、同じ永続化の中で SaveSnapshot の後に書く。
func (m *Manager) snapshotRepository(ctx context.Context, archiveManager *archive.Manager, repo discovery.Repository, worktree, sessionID string, expiry time.Time) ([]state.UnsavedSubmodule, error) {
	_, unsaved, err := archiveManager.SnapshotWithPersistence(ctx, repo, worktree, sessionID, expiry, func(snapshot state.Snapshot, capsules []archive.SubmoduleCapsule) error {
		if err := m.store.SaveSnapshot(ctx, snapshot); err != nil {
			return err
		}
		submodules := make([]state.SubmoduleSnapshot, 0, len(capsules))
		for _, capsule := range capsules {
			submodules = append(submodules, capsule.Snapshot(sessionID, string(repo.ID)))
		}
		return m.store.ReplaceSubmoduleSnapshots(ctx, sessionID, string(repo.ID), submodules)
	})
	if err != nil {
		return nil, err
	}
	out := make([]state.UnsavedSubmodule, 0, len(unsaved))
	for _, entry := range unsaved {
		out = append(out, state.UnsavedSubmodule{RepositoryID: string(repo.ID), Path: entry.Path, Reasons: strings.Join(entry.Reasons, ",")})
	}
	return out, nil
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
		unsaved, err := m.snapshotRepository(ctx, &archiveManager, repo, sr.WorktreePath, s.ID, expiry)
		if err != nil {
			m.log.Error("snapshot failed", "session_id", s.ID, "repository_id", repo.ID, "error", err)
			code, detail, ok := gitFailureInfo("SNAPSHOT", err, m.prepareDetailDir)
			if !ok {
				code = "SNAPSHOT_FAILED"
			}
			_ = m.store.SetSlotStateWithDetail(ctx, s.SlotID, []string{"SNAPSHOTTING"}, "QUARANTINED", code, detail)
			return err
		}
		// 記録は MarkArchived より前に置く。順序が逆だと、SNAPSHOTTED へ移ってから記録するまでの間に GC が回収し得る。
		if err := m.store.ReplaceUnsavedSubmodules(ctx, s.SlotID, string(repo.ID), unsaved); err != nil {
			return err
		}
		if len(unsaved) > 0 {
			m.log.Warn("submodule work could not be snapshotted, so the slot is kept out of automatic reclamation", "session_id", s.ID, "slot_id", s.SlotID, "repository_id", repo.ID, "submodules", len(unsaved))
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
				code, detail, ok := gitFailureInfo("SNAPSHOT", err, m.prepareDetailDir)
				if !ok {
					code = "SNAPSHOT_FAILED"
				}
				_ = m.store.SetSlotStateWithDetail(ctx, s.SlotID, []string{"SNAPSHOTTING"}, "QUARANTINED", code, detail)
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
			rootSnapshot, err = archive.SnapshotWorkspaceAt(ctx, slot.Path, ownershipRoot, ownershipRootID, ownershipRootHandle, s.ID, workspaceRecoveryExclusions(repos), expiry, rootRules.Link...)
			if err != nil {
				m.log.Error("workspace root snapshot failed", "session_id", s.ID, "error", err)
				code, detail, ok := gitFailureInfo("SNAPSHOT", err, m.prepareDetailDir)
				if !ok {
					code = "SNAPSHOT_FAILED"
				}
				_ = m.store.SetSlotStateWithDetail(ctx, s.SlotID, []string{"SNAPSHOTTING"}, "QUARANTINED", code, detail)
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
