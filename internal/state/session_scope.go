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

// WorkspaceRootForSlotPath は slot の path そのもの、またはその配下の path から、slot が属する workspace の root を返す。
// 会話に記録された cwd は slot を畳んだ後も残るため、実体を失った worktree からの resume を同じ workspace で作り直すのに使う。
// 一致しなければ空文字を返す。prefix 判定は substr で行い、path に LIKE のワイルドカードが含まれても誤って一致させない。
func (s *Store) WorkspaceRootForSlotPath(ctx context.Context, path string) (string, error) {
	var root string
	err := s.db.QueryRowContext(ctx, `SELECT w.root_path FROM slots sl JOIN roots r ON r.id=sl.root_id JOIN workspaces w ON w.id=sl.workspace_id
 WHERE ?=r.path || '/' || sl.rel_path OR substr(?,1,length(r.path || '/' || sl.rel_path)+1)=r.path || '/' || sl.rel_path || '/'
 ORDER BY length(r.path || '/' || sl.rel_path) DESC LIMIT 1`, path, path).Scan(&root)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return root, err
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
