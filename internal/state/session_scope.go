package state

import (
	"context"
	"database/sql"
	"errors"
)

// SessionScope は picker に渡す会話 identity と状態である。
type SessionScope struct {
	ID             string `json:"id"`
	Agent          string `json:"agent"`
	AgentSessionID string `json:"agent_session_id"`
	State          string `json:"state"`
}

func (s *Store) WorkspaceSlotPaths(ctx context.Context, workspaceID string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT r.path || '/' || sl.rel_path FROM slots sl JOIN roots r ON r.id=sl.root_id WHERE sl.workspace_id=? ORDER BY r.path,sl.rel_path`, workspaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	paths := []string{}
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		paths = append(paths, p)
	}
	return paths, rows.Err()
}

func (s *Store) WorkspaceSessionScopes(ctx context.Context, workspaceID string) ([]SessionScope, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT se.id,se.agent_kind,COALESCE(se.agent_session_id,se.pending_agent_session_id,''),se.state
 FROM sessions se WHERE se.workspace_id=? ORDER BY se.created_at DESC`, workspaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []SessionScope{}
	for rows.Next() {
		var v SessionScope
		if err := rows.Scan(&v.ID, &v.Agent, &v.AgentSessionID, &v.State); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// PreviousWorktree は会話の親 lease が使用した起動先を返す。
func (s *Store) PreviousWorktree(ctx context.Context, sessionID string) (string, error) {
	var path string
	err := s.db.QueryRowContext(ctx, `SELECT r.path || '/' || sl.rel_path || CASE WHEN (SELECT count(*) FROM slot_repositories sr WHERE sr.slot_id=sl.id)=1 THEN '/' || (SELECT sr.dir_name FROM slot_repositories sr WHERE sr.slot_id=sl.id LIMIT 1) ELSE '' END
 FROM sessions child JOIN sessions parent ON parent.id=child.parent_session_id JOIN slots sl ON sl.id=parent.slot_id JOIN roots r ON r.id=sl.root_id WHERE child.id=?`, sessionID).Scan(&path)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return path, err
}
