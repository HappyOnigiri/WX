package daemon

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// ReleaseStatus は release が受け付けた job と、その job が属する session/slot の現在状態を返す。
// job が GC で消えた場合も session と slot の状態は返し、CLI が待機を終えられるようにする。
func (m *Manager) ReleaseStatus(ctx context.Context, sessionID, jobID string) (map[string]any, error) {
	session, err := m.store.SessionByID(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	reply := map[string]any{
		"session_id":    sessionID,
		"session_state": session.State,
	}
	if session.SlotID != "" {
		if slot, slotErr := m.store.Slot(ctx, session.SlotID); slotErr == nil {
			reply["slot_state"] = slot.State
		} else if !errors.Is(slotErr, sql.ErrNoRows) {
			return nil, slotErr
		}
	}
	if jobID == "" {
		return reply, nil
	}
	// job_id が指定された場合は、行が GC 済みでも同じ対象を返したと分かるよう
	// state を空文字で含める。CLI はこの値を完了待ちの終端として扱う。
	reply["job_id"], reply["state"] = jobID, ""
	job, err := m.store.JobByID(ctx, jobID)
	if errors.Is(err, sql.ErrNoRows) {
		// 完了後の GC や別の回収で job が消えても、空 state を終端の印として返す。
		return reply, nil
	}
	if err != nil {
		return nil, err
	}
	if job.SessionID != "" && job.SessionID != sessionID {
		return nil, fmt.Errorf("job %s does not belong to session %s", jobID, sessionID)
	}
	if job.SlotID != "" && session.SlotID != "" && job.SlotID != session.SlotID {
		return nil, fmt.Errorf("job %s does not belong to session %s", jobID, sessionID)
	}
	reply["job_kind"], reply["state"] = job.Kind, job.State
	if job.State == "FAILED" {
		if job.ErrorCode != "" {
			reply["failure_code"] = job.ErrorCode
		}
		if job.ErrorMessage != "" {
			reply["failure_message"] = job.ErrorMessage
		}
		if job.ErrorDetailPath != "" {
			reply["detail_path"] = job.ErrorDetailPath
		}
	}
	return reply, nil
}
