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

func insertCurrentSessionRepositories(ctx context.Context, tx *sql.Tx, sessionID, workspaceID, slotID string) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO session_repositories(session_id,repository_id,relative_path,ordinal) SELECT ?,sr.repository_id,wr.relative_path,wr.ordinal FROM slot_repositories sr JOIN workspace_repositories wr ON wr.workspace_id=? AND wr.repository_id=sr.repository_id WHERE sr.slot_id=? ORDER BY wr.ordinal`, sessionID, workspaceID, slotID)
	return err
}

func copySessionRepositories(ctx context.Context, tx *sql.Tx, sessionID, parentSessionID string) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO session_repositories(session_id,repository_id,relative_path,ordinal) SELECT ?,repository_id,relative_path,ordinal FROM session_repositories WHERE session_id=? ORDER BY ordinal`, sessionID, parentSessionID)
	return err
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

// ScopeWorkspace は会話 scope を引くための workspace identity である。
// Root は登録時点の値なので、main worktree が移動し得る呼び出し元は現在の実体から上書きする。
type ScopeWorkspace struct {
	ID   string
	Root string
	Kind string
}

// ScopeWorkspaceForSlotPath は slot の path そのもの、またはその配下の path から、slot が属する workspace を返す。
// 会話に記録された cwd は slot を畳んだ後も残るため、実体を失った worktree や退役した root 世代の記録も検索する。
// prefix 判定は substr で path component 境界を確かめ、path に LIKE のワイルドカードが含まれても誤って一致させない。
func (s *Store) ScopeWorkspaceForSlotPath(ctx context.Context, path string) (ScopeWorkspace, bool, error) {
	var scope ScopeWorkspace
	err := s.db.QueryRowContext(ctx, `SELECT w.id,w.root_path,w.kind FROM slots sl JOIN roots r ON r.id=sl.root_id JOIN workspaces w ON w.id=sl.workspace_id
 WHERE ?=r.path || '/' || sl.rel_path OR substr(?,1,length(r.path || '/' || sl.rel_path)+1)=r.path || '/' || sl.rel_path || '/'
 ORDER BY length(r.path || '/' || sl.rel_path) DESC LIMIT 1`, path, path).Scan(&scope.ID, &scope.Root, &scope.Kind)
	if errors.Is(err, sql.ErrNoRows) {
		return ScopeWorkspace{}, false, nil
	}
	if err != nil {
		return ScopeWorkspace{}, false, err
	}
	return scope, true, nil
}

// ScopeRepositoryWorkspace は Git common directory から登録済みの repository workspace を返す。
// identity が複数の workspace に跨る曖昧な状態では found=false を返し、判定を通常の探索経路へ委ねる。
func (s *Store) ScopeRepositoryWorkspace(ctx context.Context, commonDir string) (ScopeWorkspace, bool, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT w.id,w.root_path,w.kind FROM workspaces w JOIN workspace_repositories wr ON wr.workspace_id=w.id JOIN repositories r ON r.id=wr.repository_id
 WHERE w.kind='repository' AND r.common_git_dir=? ORDER BY w.id LIMIT 2`, commonDir)
	if err != nil {
		return ScopeWorkspace{}, false, err
	}
	defer rows.Close()
	var found []ScopeWorkspace
	for rows.Next() {
		var scope ScopeWorkspace
		if err := rows.Scan(&scope.ID, &scope.Root, &scope.Kind); err != nil {
			return ScopeWorkspace{}, false, err
		}
		found = append(found, scope)
	}
	if err := rows.Err(); err != nil {
		return ScopeWorkspace{}, false, err
	}
	if len(found) != 1 {
		return ScopeWorkspace{}, false, nil
	}
	return found[0], true, nil
}

// ScopeMultiWorkspaceForRoot は root path の完全一致で登録済みの multi-repository workspace を返す。
// 子 directory を prefix で結合しないため、workspace 配下の repository は従来の規則で解決される。
func (s *Store) ScopeMultiWorkspaceForRoot(ctx context.Context, root string) (ScopeWorkspace, bool, error) {
	var scope ScopeWorkspace
	err := s.db.QueryRowContext(ctx, `SELECT id,root_path,kind FROM workspaces WHERE kind='multi_repository' AND root_path=?`, root).Scan(&scope.ID, &scope.Root, &scope.Kind)
	if errors.Is(err, sql.ErrNoRows) {
		return ScopeWorkspace{}, false, nil
	}
	if err != nil {
		return ScopeWorkspace{}, false, err
	}
	return scope, true, nil
}

// WorkspaceHasCommonDir は Git common directory が workspace の現在の membership に含まれるかを返す。
// slot の記録と実体の identity が食い違うときに、記録へ強制結合してよいかの判定に使う。
func (s *Store) WorkspaceHasCommonDir(ctx context.Context, workspaceID, commonDir string) (bool, error) {
	var found int
	err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM workspace_repositories wr JOIN repositories r ON r.id=wr.repository_id WHERE wr.workspace_id=? AND r.common_git_dir=?`, workspaceID, commonDir).Scan(&found)
	return found > 0, err
}

// WorkspaceRootForSlotPath は ScopeWorkspaceForSlotPath と同じ一致規則で workspace root だけを返し、一致しなければ空文字を返す。
func (s *Store) WorkspaceRootForSlotPath(ctx context.Context, path string) (string, error) {
	scope, found, err := s.ScopeWorkspaceForSlotPath(ctx, path)
	if err != nil || !found {
		return "", err
	}
	return scope.Root, nil
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
