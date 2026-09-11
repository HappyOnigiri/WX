package daemon

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/HappyOnigiri/WX/internal/archive"
	"github.com/HappyOnigiri/WX/internal/config"
	"github.com/HappyOnigiri/WX/internal/discovery"
	"github.com/HappyOnigiri/WX/internal/pool"
	"github.com/HappyOnigiri/WX/internal/state"
	"github.com/HappyOnigiri/WX/internal/workspace"
)

func (m *Manager) resumeRestoreJob(ctx context.Context, sessionID string) error {
	s, err := m.store.SessionByID(ctx, sessionID)
	if err != nil {
		return err
	}
	if s.ParentSessionID == "" {
		return errors.New("restore session has no parent snapshot")
	}
	parent, err := m.store.SessionByID(ctx, s.ParentSessionID)
	if err != nil {
		return err
	}
	snapshots, err := m.store.Snapshots(ctx, s.ParentSessionID)
	if err != nil {
		return err
	}
	if parent.State == "RELEASING" || parent.State == "SNAPSHOTTING" {
		parentSlot, slotErr := m.store.Slot(ctx, parent.SlotID)
		if slotErr != nil {
			return slotErr
		}
		if parentSlot.State == "FAILED" || parentSlot.State == "QUARANTINED" {
			_ = m.store.SetSlotState(ctx, s.SlotID, []string{"RESTORING"}, "QUARANTINED", "SNAPSHOT_UNAVAILABLE")
			return fmt.Errorf("parent snapshot job failed with slot state %s", parentSlot.State)
		}
		return dependencyPendingError{errors.New("parent snapshot is still being archived")}
	}
	if !snapshotsUsable(snapshots, time.Now()) {
		_ = m.store.SetSlotState(ctx, s.SlotID, []string{"RESTORING"}, "QUARANTINED", "SNAPSHOT_UNAVAILABLE")
		return errors.New("parent recovery snapshot is expired or incomplete")
	}
	w, err := m.store.SessionWorkspace(ctx, s.ParentSessionID)
	if err != nil {
		return err
	}
	usable, err := m.recoveryUsable(ctx, s.ParentSessionID, w, snapshots, time.Now())
	if err != nil || !usable {
		_ = m.store.SetSlotState(ctx, s.SlotID, []string{"RESTORING"}, "QUARANTINED", "SNAPSHOT_UNAVAILABLE")
		if err != nil {
			return fmt.Errorf("validate parent recovery snapshot: %w", err)
		}
		return errors.New("parent recovery snapshot is expired or incomplete")
	}
	repos, err := m.store.SlotRepositories(ctx, s.SlotID)
	if err != nil {
		return err
	}
	by := map[string]state.Snapshot{}
	for _, snapshot := range snapshots {
		by[snapshot.RepositoryID] = snapshot
	}
	if len(repos) == 0 {
		resolved := make([]pool.Resolved, 0, len(w.Repositories))
		for _, repo := range w.Repositories {
			snapshot, ok := by[string(repo.ID)]
			if !ok {
				_ = m.store.SetSlotState(ctx, s.SlotID, []string{"RESTORING"}, "QUARANTINED", "SNAPSHOT_INCOMPLETE")
				return fmt.Errorf("snapshot missing repository %s", repo.RelativePath)
			}
			resolved = append(resolved, pool.Resolved{Repository: repo, RequestedRef: snapshot.HeadRef, OID: snapshot.HeadOID})
		}
		slot, err := m.store.Slot(ctx, s.SlotID)
		if err != nil {
			return err
		}
		repos, err = m.slotRepos(slot.Path, w, resolved, slot.Generation, nil, config.PrepareOverride{})
		if err != nil {
			return err
		}
		for i := range repos {
			repos[i].State = "RESTORING"
		}
		if err := m.store.AddRestoringRepositories(ctx, s.SlotID, repos); err != nil {
			return err
		}
	}
	resolved, err := m.resolvedFromStored(ctx, w, repos)
	if err != nil {
		return err
	}
	return m.restoreSlot(ctx, s.SlotID, w, resolved, repos, by)
}

// verifiedWorkspaceArchive は復元 worker が持つ、検証済み workspace archive と pin した root descriptor の寿命である。
type verifiedWorkspaceArchive struct {
	snapshot *archive.VerifiedWorkspaceSnapshot
	release  func()
}

func (v *verifiedWorkspaceArchive) close() {
	if v == nil {
		return
	}
	_ = v.snapshot.Close()
	if v.release != nil {
		v.release()
	}
}

// openVerifiedWorkspaceArchive は RESTORE 予約で GC 保護を得た後に、workspace archive の完全性を 1 度だけ検証する。
// target への変更を始める前に呼ぶ契約であり、失敗した slot はここで隔離する。
func (m *Manager) openVerifiedWorkspaceArchive(ctx context.Context, id string) (*verifiedWorkspaceArchive, error) {
	session, err := m.store.SessionByID(ctx, id)
	if err != nil {
		return nil, err
	}
	rootSnapshot, found, err := m.store.WorkspaceSnapshot(ctx, session.ParentSessionID)
	if err != nil {
		return nil, err
	}
	if !found {
		_ = m.store.SetSlotState(ctx, id, []string{"RESTORING"}, "QUARANTINED", "SNAPSHOT_INCOMPLETE")
		return nil, errors.New("multi-repository recovery snapshot has no workspace root archive")
	}
	archiveRoot, ok := m.rootForPath(rootSnapshot.ArchivePath)
	if !ok {
		_ = m.store.SetSlotState(ctx, id, []string{"RESTORING"}, "QUARANTINED", "SNAPSHOT_INCOMPLETE")
		return nil, errors.New("workspace root recovery paths are outside known wx roots")
	}
	archiveRootHandle, closeArchiveRoot, archiveRootErr := m.existingRootDescriptor(archiveRoot)
	if archiveRootErr != nil {
		_ = m.store.SetSlotState(ctx, id, []string{"RESTORING"}, "QUARANTINED", "SNAPSHOT_INCOMPLETE")
		return nil, fmt.Errorf("open workspace archive root: %w", archiveRootErr)
	}
	verified, err := archive.OpenVerifiedWorkspaceSnapshotAt(ctx, archiveRoot, archiveRootHandle, rootSnapshot, time.Now())
	if err != nil {
		closeArchiveRoot()
		m.quarantineWorkspaceArchiveFailure(ctx, id, err)
		return nil, fmt.Errorf("verify workspace root snapshot: %w", err)
	}
	return &verifiedWorkspaceArchive{snapshot: verified, release: closeArchiveRoot}, nil
}

// quarantineWorkspaceArchiveFailure は workspace archive の検証・展開の失敗を隔離する。
// 破損・置換・読み取り障害は専用 code で表し、recovery=unavailable を付けないことで自動 fresh 再開へ倒さない。
func (m *Manager) quarantineWorkspaceArchiveFailure(ctx context.Context, id string, err error) {
	code := "RESTORE_FAILED"
	if errors.Is(err, archive.ErrWorkspaceSnapshotIntegrity) {
		code = "SNAPSHOT_CORRUPT"
	}
	_ = m.store.SetSlotState(ctx, id, []string{"RESTORING"}, "QUARANTINED", code)
}

func (m *Manager) restoreSlot(ctx context.Context, id string, w discovery.Workspace, resolved []pool.Resolved, repos []state.SlotRepository, snaps map[string]state.Snapshot) error {
	slotState, err := m.store.Slot(ctx, id)
	if err != nil {
		return err
	}
	if slotState.State == "READY" || slotState.State == "LEASED" {
		return nil
	}
	if slotState.State != "RESTORING" {
		return fmt.Errorf("slot %s cannot be restored from %s", id, slotState.State)
	}
	releaseRoot, err := m.holdRootForPath(slotState.Path)
	if err != nil {
		m.quarantineOwnershipFailure(id, []string{"RESTORING"}, err)
		return err
	}
	defer releaseRoot()
	archiveManager := m.newArchiveManager(m.Config(), slotState)
	// multi-repository の workspace archive は、repository の復元で target を変え始めるより前に 1 度だけ検証する。
	// 検証済み descriptor をそのまま展開へ渡すため、path からの再 open と再 hash は行わない。
	var verifiedWorkspace *verifiedWorkspaceArchive
	if w.Kind == "multi_repository" {
		verifiedWorkspace, err = m.openVerifiedWorkspaceArchive(ctx, id)
		if err != nil {
			return err
		}
		defer verifiedWorkspace.close()
	}
	if len(repos) != len(resolved) {
		return errors.New("restore repository metadata does not match resolved workspace")
	}
	for i, r := range resolved {
		stored, err := m.store.SlotRepository(ctx, id, string(r.Repository.ID))
		if err != nil {
			return err
		}
		if stored.State == "READY" {
			if err := archiveManager.Preparer.ValidateRestoringSlotWorktreeOwnership(ctx, r.Repository, stored.WorktreePath, r.OID, id); err != nil {
				m.quarantineOwnershipFailure(id, []string{"RESTORING"}, err)
				return err
			}
			continue
		}
		if stored.State == "RESTORE_RUNNING" {
			if override := m.Config().Repositories[string(r.Repository.MainPath)]; len(override.Prepare.Command) > 0 {
				err := errors.New("restore preparation command completion is ambiguous after interruption")
				_ = m.store.SetSlotState(ctx, id, []string{"RESTORING"}, "QUARANTINED", "RESTORE_AMBIGUOUS")
				return err
			}
		} else if err := m.store.SetSlotRepositoryState(ctx, id, string(r.Repository.ID), []string{"RESTORING"}, "RESTORE_RUNNING"); err != nil {
			return err
		}
		repositoryPath := repos[i].WorktreePath // #nosec G602 -- equal slice lengths are checked before the loop.
		if err := archiveManager.Restore(ctx, r.Repository, repositoryPath, id, snaps[string(r.Repository.ID)]); err != nil {
			m.log.Error("restore failed", "slot_id", id, "repository_id", r.Repository.ID, "error", err)
			var prepareErr *workspace.PrepareCommandError
			if errors.As(err, &prepareErr) {
				failureCode := "RESTORE_FAILED"
				if prepareErr.FailureID != "" {
					failureCode += ":" + prepareErr.FailureID
				}
				_ = m.store.SetSlotStateWithDetail(ctx, id, []string{"RESTORING"}, "QUARANTINED", failureCode, prepareErr.DetailPath)
			} else {
				_ = m.store.SetSlotState(ctx, id, []string{"RESTORING"}, "QUARANTINED", "RESTORE_FAILED")
			}
			return err
		}
		identity, identityErr := archiveManager.Preparer.WorktreeIdentity(repositoryPath)
		if identityErr != nil {
			m.quarantineOwnershipFailure(id, []string{"RESTORING"}, fmt.Errorf("%w: capture restored worktree identity: %w", state.ErrOwnership, identityErr))
			return identityErr
		}
		if err := m.store.RecordSlotRepositoryIdentity(ctx, id, string(r.Repository.ID), identity); err != nil {
			return err
		}
		if err := m.store.SetSlotRepositoryState(ctx, id, string(r.Repository.ID), []string{"RESTORE_RUNNING"}, "READY"); err != nil {
			return err
		}
	}
	if w.Kind == "multi_repository" {
		slot, err := m.store.Slot(ctx, id)
		if err != nil {
			return err
		}
		dirIdentity, identityErr := m.ownedDirectoryIdentity(slot.Path)
		if identityErr != nil {
			m.quarantineOwnershipFailure(id, []string{"RESTORING"}, fmt.Errorf("%w: read slot directory identity: %w", state.ErrOwnership, identityErr))
			return identityErr
		}
		if err := m.store.ValidateSlotOwnership(context.Background(), state.SlotOwnershipRequest{SlotID: id, WorkspaceID: slot.WorkspaceID, RootID: slot.RootID, RelPath: slot.RelPath, DirIdentity: dirIdentity, AllowedSlotStates: []string{"RESTORING"}}); err != nil {
			if errors.Is(err, state.ErrOwnership) {
				_ = m.store.SetSlotState(context.Background(), id, []string{"RESTORING"}, "QUARANTINED", "WORKTREE_OWNERSHIP_UNCERTAIN")
			}
			return err
		}
		rootRules, err := m.rootRules(w)
		if err != nil {
			return err
		}
		if err := m.materializeWorkspaceRoot(string(w.Root), slot.Path, rootRules); err != nil {
			if errors.Is(err, state.ErrOwnership) {
				_ = m.store.SetSlotState(context.Background(), id, []string{"RESTORING"}, "QUARANTINED", "WORKTREE_OWNERSHIP_UNCERTAIN")
			} else {
				_ = m.store.SetSlotState(ctx, id, []string{"RESTORING"}, "FAILED", "ROOT_MATERIALIZATION_FAILED")
			}
			return err
		}
		targetRoot, targetOK := m.rootForPath(slot.Path)
		if !targetOK {
			_ = m.store.SetSlotState(ctx, id, []string{"RESTORING"}, "QUARANTINED", "SNAPSHOT_INCOMPLETE")
			return errors.New("workspace root recovery paths are outside known wx roots")
		}
		targetRootHandle, closeTargetRoot, targetRootErr := m.existingRootDescriptor(targetRoot)
		if targetRootErr != nil {
			_ = m.store.SetSlotState(ctx, id, []string{"RESTORING"}, "QUARANTINED", "SNAPSHOT_INCOMPLETE")
			return fmt.Errorf("open workspace restore target root: %w", targetRootErr)
		}
		defer closeTargetRoot()
		if err := archive.RestoreVerifiedWorkspace(ctx, verifiedWorkspace.snapshot, slot.Path, targetRoot, targetRootHandle, workspaceRecoveryExclusions(repos, rootRules)); err != nil {
			m.quarantineWorkspaceArchiveFailure(ctx, id, err)
			return fmt.Errorf("restore workspace root: %w", err)
		}
	}
	if _, _, err = m.store.FinishPreparationWithRelease(ctx, id); err != nil {
		return err
	}
	m.scheduleSlotUsageMeasurement(id)
	return nil
}

func (m *Manager) ResumeStatus(ctx context.Context, oldID string) (map[string]any, error) {
	old, err := m.store.SessionByID(ctx, oldID)
	if err != nil {
		return nil, err
	}
	snaps, err := m.store.Snapshots(ctx, oldID)
	if err != nil {
		return nil, err
	}
	pending := old.State == "RELEASING" || old.State == "SNAPSHOTTING"
	expired := old.State == "EXPIRED"
	if !pending && !expired {
		w, err := m.store.SessionWorkspace(ctx, oldID)
		if err != nil {
			return nil, err
		}
		usable, err := m.recoveryUsable(ctx, oldID, w, snaps, time.Now())
		if err != nil {
			return nil, err
		}
		expired = !usable
	}
	// integrity は archive 本文を読まないことを client へ明示する。復元の完全性は RESTORE worker が判定する。
	return map[string]any{"wx_session_id": old.ID, "agent": old.AgentKind, "agent_session_id": old.AgentSessionID, "state": old.State, "expired": expired, "pending": pending, "integrity": resumeIntegrityNotChecked, "workspace_id": old.WorkspaceID}, nil
}

// resumeIntegrityNotChecked は ResumeStatus が archive 本文を検証していないことを表す。
const resumeIntegrityNotChecked = "not_checked"

func snapshotsUsable(snaps []state.Snapshot, at time.Time) bool {
	if len(snaps) == 0 {
		return false
	}
	for _, snapshot := range snaps {
		expires, err := time.Parse(time.RFC3339Nano, snapshot.ExpiresAt)
		if err != nil || !expires.After(at) {
			return false
		}
	}
	return true
}

// recoveryUsable は snapshot が復元の材料として揃っているかだけを返す軽量な確認である。
// archive 本文は読まないため、内容が壊れていないことは保証しない。完全性は復元 worker が 1 度だけ検証する。
// DB・path・権限の失敗はここで返し、成功へ握りつぶさない。
func (m *Manager) recoveryUsable(ctx context.Context, sessionID string, w discovery.Workspace, snapshots []state.Snapshot, at time.Time) (bool, error) {
	if !snapshotsUsable(snapshots, at) {
		return false, nil
	}
	if w.Kind != "multi_repository" {
		return true, nil
	}
	rootSnapshot, found, err := m.store.WorkspaceSnapshot(ctx, sessionID)
	if err != nil || !found {
		return false, err
	}
	root, ok := m.rootForPath(rootSnapshot.ArchivePath)
	if !ok {
		return false, errors.New("workspace snapshot archive is outside known wx roots")
	}
	owner, releaseOwner, err := m.existingRootDescriptor(root)
	if err != nil {
		return false, fmt.Errorf("open workspace snapshot owner: %w", err)
	}
	defer releaseOwner()
	if err := archive.ValidateWorkspaceSnapshotMetadataAt(root, owner, rootSnapshot, at); err != nil {
		if errors.Is(err, archive.ErrWorkspaceSnapshotExpired) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// workspaceRecoveryExclusions は解決済みのroot ruleを受け取る。configだけを見るとmanifest由来のlinkが除外から漏れ、
// 復元時のpruneがlinkを消してsnapshotがlink先を取り込む。
func workspaceRecoveryExclusions(repos []state.SlotRepository, rules workspace.RootRules) []string {
	// bundle root内のworktree、slot marker、.worktreelinkをsnapshot/prune対象から外す。
	// source相対pathではなくslot_repositories.dir_nameを使わないとnested repositoryを消し得る。
	links := rules.Link
	excluded := make([]string, 0, 2*len(repos)+len(links))
	for _, repository := range repos {
		if repository.DirName == "" {
			continue
		}
		excluded = append(excluded, repository.DirName, workspace.OwnershipMarkerName(repository.RepositoryID))
	}
	excluded = append(excluded, links...)
	return excluded
}

type ResumeOptions struct {
	AgentSessionID string
	Branches       []string
	// Lease は wx shell --resume / wx run --resume の貸出属性である。
	// zero 値なら従来の agent 起動としての復元になる。
	Lease leaseAttrs
}

func (m *Manager) Resume(ctx context.Context, oldID, agent string, pid int, fresh bool, options ...ResumeOptions) (Lease, error) {
	var opts ResumeOptions
	if len(options) > 0 {
		opts = options[0]
	}
	if !fresh && len(opts.Branches) > 0 {
		return Lease{}, errors.New("--branch requires --fresh when resuming")
	}
	old, err := m.store.SessionByID(ctx, oldID)
	if err != nil {
		return Lease{}, err
	}
	if agent == "" {
		agent = old.AgentKind
	}
	if !resumeAgentMatches(agent, old.AgentKind, opts.Lease.Kind, old.LeaseKind) {
		return Lease{}, errors.New("resume agent does not match the original session")
	}
	if old.State == "STARTING" || old.State == "ACTIVE" || old.State == "RESTORING" || old.State == "UNBOUND" {
		return Lease{}, errors.New("session has not been released yet")
	}
	snaps, err := m.store.Snapshots(ctx, oldID)
	if err != nil {
		return Lease{}, err
	}
	if old.State == "RELEASING" || old.State == "SNAPSHOTTING" {
		old, snaps, err = m.waitForSnapshot(ctx, oldID)
		if err != nil {
			return Lease{}, err
		}
	}
	usable := false
	var archivedWorkspace discovery.Workspace
	if !fresh && old.State != "EXPIRED" {
		archivedWorkspace, err = m.store.SessionWorkspace(ctx, oldID)
		if err != nil {
			return Lease{}, err
		}
		usable, err = m.recoveryUsable(ctx, oldID, archivedWorkspace, snaps, time.Now())
		if err != nil {
			return Lease{}, fmt.Errorf("validate recovery snapshot: %w", err)
		}
	}
	if fresh || old.State == "EXPIRED" || !usable {
		if !fresh {
			return Lease{}, errors.New("session snapshot is EXPIRED; confirmation is required before creating a workspace from the current base " + RecoveryUnavailableMarker)
		}
		w, err := m.store.Workspace(ctx, old.WorkspaceID)
		if err != nil {
			return Lease{}, err
		}
		resolved, err := pool.ResolveBranches(ctx, m.git, w, opts.Branches)
		if err != nil {
			return Lease{}, err
		}
		generation, err := m.store.WorkspaceGeneration(ctx, string(w.ID))
		if err != nil {
			return Lease{}, err
		}
		return m.allocate(ctx, w, resolved, generation, agent, pid, opts.Lease, "STARTING", oldID)
	}
	w := archivedWorkspace
	resolved := make([]pool.Resolved, 0, len(w.Repositories))
	for _, repo := range w.Repositories {
		var found *state.Snapshot
		for i := range snaps {
			if snaps[i].RepositoryID == string(repo.ID) {
				found = &snaps[i]
				break
			}
		}
		if found == nil {
			return Lease{}, errors.New("incomplete recovery snapshot")
		}
		resolved = append(resolved, pool.Resolved{Repository: repo, RequestedRef: found.HeadRef, OID: found.HeadOID})
	}
	generation, err := m.store.WorkspaceGeneration(ctx, string(w.ID))
	if err != nil {
		return Lease{}, err
	}
	lease, err := m.allocate(ctx, w, resolved, generation, agent, pid, opts.Lease, "RESTORING", oldID, opts.AgentSessionID)
	if err != nil {
		return Lease{}, err
	}
	return lease, nil
}

func (m *Manager) waitForSnapshot(ctx context.Context, sessionID string) (state.Session, []state.Snapshot, error) {
	waitCtx, cancel := context.WithTimeout(ctx, m.Config().Readiness.Timeout.Duration)
	defer cancel()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		session, err := m.store.SessionByID(waitCtx, sessionID)
		if err != nil {
			return state.Session{}, nil, err
		}
		snapshots, err := m.store.Snapshots(waitCtx, sessionID)
		if err != nil {
			return state.Session{}, nil, err
		}
		if session.State == "ARCHIVED" {
			w, workspaceErr := m.store.SessionWorkspace(waitCtx, sessionID)
			if workspaceErr != nil {
				return session, snapshots, workspaceErr
			}
			usable, usableErr := m.recoveryUsable(waitCtx, sessionID, w, snapshots, time.Now())
			if usableErr != nil {
				return session, snapshots, usableErr
			}
			if usable {
				return session, snapshots, nil
			}
		}
		if session.State == "EXPIRED" {
			return session, snapshots, errors.New("session recovery snapshot expired while waiting for archive")
		}
		slot, err := m.store.Slot(waitCtx, session.SlotID)
		if err == nil && (slot.State == "FAILED" || slot.State == "QUARANTINED") {
			return session, snapshots, fmt.Errorf("session archive failed: slot is %s", slot.State)
		}
		select {
		case <-waitCtx.Done():
			return session, snapshots, waitCtx.Err()
		case <-ticker.C:
		}
	}
}
