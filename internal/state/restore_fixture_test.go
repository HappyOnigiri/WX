package state

import "context"

// recreateRestoreFixture は旧 UNBOUND fixture を明示復元開始時の状態へ置き換える。
func recreateRestoreFixture(s *Store, ctx context.Context, id, parent, workspace, agent string, generation int, repos []SlotRepository) (Job, error) {
	se, err := s.SessionByID(ctx, id)
	if err != nil {
		return Job{}, err
	}
	sl, err := s.Slot(ctx, se.SlotID)
	if err != nil {
		return Job{}, err
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE id=?`, id); err != nil {
		return Job{}, err
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM slots WHERE id=?`, sl.ID); err != nil {
		return Job{}, err
	}
	se.State = "RESTORING"
	se.ParentSessionID = parent
	se.WorkspaceID = workspace
	se.PendingAgentSessionID = agent
	sl.State = "RESTORING"
	sl.WorkspaceID = workspace
	sl.Generation = generation
	return s.CreateSlotSession(ctx, sl, repos, se, "RESTORE")
}
