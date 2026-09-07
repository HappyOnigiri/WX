package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/HappyOnigiri/WX/internal/archive"
	"github.com/HappyOnigiri/WX/internal/domain"
	"github.com/HappyOnigiri/WX/internal/state"
)

func (m *Manager) removeSlotJob(ctx context.Context, job state.Job) error {
	if job.SessionID != "" {
		session, err := m.store.SessionByID(ctx, job.SessionID)
		if err != nil {
			return err
		}
		if processAlive(session.AgentPID) {
			return dependencyPendingError{fmt.Errorf("agent process %d is still active", session.AgentPID)}
		}
	}
	slot, err := m.store.Slot(ctx, job.SlotID)
	if err != nil {
		return err
	}
	if slot.State == "ARCHIVED" {
		return nil
	}
	if slot.State != "REMOVING" {
		return fmt.Errorf("slot %s cannot be removed from %s", slot.ID, slot.State)
	}
	root := strings.TrimSuffix(slot.Path, string(filepath.Separator)+slot.RelPath)
	archiveManager := m.newArchiveManager(m.Config(), slot)
	if err := m.removeSlotWorktrees(ctx, archiveManager, root, slot, job.SessionID); err != nil {
		if job.SessionID == "" {
			m.quarantineOwnershipFailure(slot.ID, []string{"REMOVING"}, err)
		}
		return err
	}
	if err := m.store.FinishRemoval(ctx, slot.ID); err != nil {
		return err
	}
	m.forgetSlotUsage(slot.ID, root)
	return nil
}

func (m *Manager) removeColdRepositoryJob(ctx context.Context, job state.Job) error {
	slot, err := m.store.Slot(ctx, job.SlotID)
	if err != nil {
		return err
	}
	repositoryState, err := m.store.SlotRepository(ctx, job.SlotID, job.RepositoryID)
	if err != nil {
		return err
	}
	if repositoryState.State == "COLD" {
		return nil
	}
	if repositoryState.State != "RETIRING" || slot.State != "RETIRING" {
		return fmt.Errorf("repository %s/%s cannot retire from %s/%s", slot.ID, job.RepositoryID, slot.State, repositoryState.State)
	}
	root := strings.TrimSuffix(slot.Path, string(filepath.Separator)+slot.RelPath)
	repo, err := m.store.Repository(ctx, job.RepositoryID)
	if err != nil {
		return err
	}
	if !filepath.IsLocal(repositoryState.DirName) || strings.ContainsAny(repositoryState.DirName, `/\\`) || repositoryState.DirName == "." {
		err := fmt.Errorf("%w: invalid repository directory", state.ErrOwnership)
		m.quarantineOwnershipFailure(slot.ID, []string{"RETIRING"}, err)
		return err
	}
	owner, err := os.OpenRoot(root)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if owner != nil {
		defer func() { _ = owner.Close() }()
		if _, err := domain.PhysicalPathInfo(owner, slot.RelPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err := owner.RemoveAll(filepath.Join(slot.RelPath, repositoryState.DirName)); err != nil {
			return err
		}
	}
	if err := m.removeGitRegistration(ctx, string(repo.CommonDir), repositoryState.WorktreePath); err != nil {
		return err
	}
	// 待機枠の再利用に備え、repository の回収では slot directory を残す。
	return m.store.FinishColdRepositoryRemoval(ctx, slot.ID, job.RepositoryID)
}

func (m *Manager) quarantineOwnershipFailure(slotID string, from []string, runErr error) {
	if !errors.Is(runErr, state.ErrOwnership) || m.store == nil {
		return
	}
	if err := m.store.SetSlotState(context.Background(), slotID, from, "QUARANTINED", "WORKTREE_OWNERSHIP_UNCERTAIN"); err != nil && m.log != nil {
		m.log.Error("quarantine uncertain worktree ownership failed", "slot_id", slotID, "error", err)
	}
}

func (m *Manager) removeSlotWorktrees(ctx context.Context, archiveManager archive.Manager, root string, slot state.Slot, sessionID string) error {
	slotID, slotPath := slot.ID, slot.Path
	if !domain.IsWithin(root, slotPath) {
		return fmt.Errorf("%w: slot path is outside wx root", state.ErrOwnership)
	}
	repos, err := m.store.SlotRepositories(ctx, slotID)
	if err != nil {
		return err
	}
	expected := map[string]string{}
	if sessionID != "" {
		workspaceKind, err := m.store.SessionWorkspaceKind(ctx, sessionID)
		if err != nil {
			return removalMetadataFailure("resolve session workspace before removal", err)
		}
		if workspaceKind == "multi_repository" {
			rootSnapshot, found, err := m.store.WorkspaceSnapshot(ctx, sessionID)
			if err != nil {
				return removalMetadataFailure("read workspace root snapshot before removal", err)
			}
			if !found {
				return removalMetadataFailure("workspace root snapshot metadata is incomplete for worktree removal", errors.New("metadata row is missing"))
			}
			archiveRoot, ok := m.rootForPath(rootSnapshot.ArchivePath)
			if !ok {
				return removalMetadataFailure("workspace root snapshot is outside known wx roots", errors.New("archive path is not owned"))
			}
			archiveRootHandle, closeArchiveRoot, rootErr := m.existingRootDescriptor(archiveRoot)
			if rootErr != nil {
				return removalMetadataFailure("open workspace root snapshot owner", rootErr)
			}
			defer closeArchiveRoot()
			if err := archive.ValidateWorkspaceSnapshotAt(ctx, archiveRoot, archiveRootHandle, rootSnapshot, time.Now()); err != nil {
				return removalMetadataFailure("validate workspace root snapshot before removal", err)
			}
		}
		snapshots, err := m.store.Snapshots(ctx, sessionID)
		if err != nil {
			return removalMetadataFailure("read repository snapshots before removal", err)
		}
		for _, snapshot := range snapshots {
			expected[snapshot.RepositoryID] = snapshot.HeadOID
		}
	}
	for _, sr := range repos {
		if sessionID != "" {
			if _, ok := expected[sr.RepositoryID]; !ok {
				return removalMetadataFailure("snapshot metadata is incomplete for worktree removal", errors.New("repository snapshot row is missing"))
			}
		}
	}
	return m.removeRegisteredSlot(ctx, slot)
}

func removalMetadataFailure(message string, err error) error {
	if err == nil || errors.Is(err, state.ErrOwnership) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return fmt.Errorf("%w: %s: %w", state.ErrOwnership, message, err)
}
