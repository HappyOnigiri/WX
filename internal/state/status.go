package state

import (
	"context"
	"path/filepath"
)

type Status struct{ Workspaces, Repositories, Ready, Leased, Failed, Active, Snapshots, Jobs, Quarantined int }

type (
	WorkspaceDiagnostic struct {
		ID           string `json:"id"`
		Root         string `json:"root"`
		Generation   int    `json:"generation"`
		Repositories int    `json:"repositories"`
		Ready        int    `json:"ready"`
		Leased       int    `json:"leased"`
		Failed       int    `json:"failed"`
		// LastUsedAt はその workspace で作られた session の created_at の最大値であり、一度も使っていない workspace では空になる。
		// repositories.last_leased_at を使わないのは、repository 行を共有する別 workspace の貸出でも値が入り、workspace 自身の利用実績と区別できないためである。
		LastUsedAt string `json:"last_used_at,omitempty"`
	}
	SessionDiagnostic struct {
		ID         string `json:"id"`
		Agent      string `json:"agent"`
		State      string `json:"state"`
		CreatedAt  string `json:"created_at"`
		BaseOIDs   string `json:"base_oids"`
		AgeSeconds int64  `json:"age_seconds"`
	}
	RepositoryDiagnostic struct {
		ID               string `json:"id"`
		MainPath         string `json:"main_path"`
		LastUsedAt       string `json:"last_used_at,omitempty"`
		StandbyReadyAt   string `json:"standby_ready_at,omitempty"`
		StandbyExpiresAt string `json:"standby_expires_at,omitempty"`
		Hot              bool   `json:"hot"`
	}
	JobDiagnostic struct {
		Pending int `json:"pending"`
		Running int `json:"running"`
		Failed  int `json:"failed"`
	}
	SnapshotDiagnostic struct {
		Count          int    `json:"count"`
		EarliestExpiry string `json:"earliest_expiry,omitempty"`
	}
	QuarantineDiagnostic struct {
		ID          string `json:"id"`
		Path        string `json:"path"`
		Kind        string `json:"kind,omitempty"`
		FailureCode string `json:"failure_code,omitempty"`
	}
	StandbyReplenishmentDiagnostic struct {
		WorkspaceID string `json:"workspace_id"`
		Root        string `json:"root"`
		Generation  int    `json:"generation"`
		Reason      string `json:"reason"`
		Detail      string `json:"detail,omitempty"`
		SuspendedAt string `json:"suspended_at,omitempty"`
		Action      string `json:"action,omitempty"`
	}
	StatusDiagnostics struct {
		Workspaces   []WorkspaceDiagnostic  `json:"workspaces"`
		Sessions     []SessionDiagnostic    `json:"sessions"`
		Repositories []RepositoryDiagnostic `json:"repositories"`
		Jobs         JobDiagnostic          `json:"jobs"`
		Snapshots    SnapshotDiagnostic     `json:"snapshots"`
		Quarantine   []QuarantineDiagnostic `json:"quarantine"`
	}
)

// SlotSummary は slot 1 個と、それを借りている session の情報をまとめた 1 行である。
// slot が既に無い archived session も同じ形で返し、その場合は SlotID・State・Path を空にする。
type SlotSummary struct {
	SlotID         string `json:"slot_id,omitempty"`
	State          string `json:"state,omitempty"`
	WorkspaceID    string `json:"workspace_id,omitempty"`
	Path           string `json:"path,omitempty"`
	SessionID      string `json:"session_id,omitempty"`
	SessionState   string `json:"session_state,omitempty"`
	AgentKind      string `json:"agent,omitempty"`
	AgentSessionID string `json:"agent_session_id,omitempty"`
	CreatedAt      string `json:"created_at,omitempty"`
	ReadyAt        string `json:"ready_at,omitempty"`
	LastUsedAt     string `json:"last_used_at,omitempty"`
	ArchivedAt     string `json:"archived_at,omitempty"`
	ExpiresAt      string `json:"expires_at,omitempty"`
}

// ListSlots は貸出中と待機中の slot を返す。
// all では FAILED・QUARANTINED の slot に加え、slot を手放した session も返し、`wx resume` に渡す ID をここから辿れるようにする。
func (s *Store) ListSlots(ctx context.Context, all bool) ([]SlotSummary, error) {
	q := `SELECT sl.id,sl.state,COALESCE(sl.workspace_id,''),rt.path,sl.rel_path,COALESCE(se.id,''),COALESCE(se.state,''),COALESCE(se.agent_kind,''),COALESCE(se.agent_session_id,''),sl.created_at,COALESCE(sl.ready_at,''),COALESCE(sl.last_used_at,'')
		FROM slots sl JOIN roots rt ON rt.id=sl.root_id LEFT JOIN sessions se ON se.id=sl.owner_session_id`
	if !all {
		q += ` WHERE sl.state IN ('READY','LEASED')`
	}
	q += ` ORDER BY rt.path,sl.rel_path`
	rows, err := s.db.QueryContext(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SlotSummary
	for rows.Next() {
		var x SlotSummary
		var root, relative string
		if err := rows.Scan(&x.SlotID, &x.State, &x.WorkspaceID, &root, &relative, &x.SessionID, &x.SessionState, &x.AgentKind, &x.AgentSessionID, &x.CreatedAt, &x.ReadyAt, &x.LastUsedAt); err != nil {
			return nil, err
		}
		x.Path = filepath.Join(root, relative)
		out = append(out, x)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if !all {
		return out, nil
	}
	detached, err := s.listDetachedSessions(ctx)
	if err != nil {
		return nil, err
	}
	return append(out, detached...), nil
}

// listDetachedSessions は現在どの slot も借りていない session を返す。
func (s *Store) listDetachedSessions(ctx context.Context) ([]SlotSummary, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT se.id,se.state,COALESCE(se.workspace_id,''),se.agent_kind,COALESCE(se.agent_session_id,''),se.created_at,COALESCE(se.archived_at,''),COALESCE(se.expires_at,'')
		FROM sessions se WHERE NOT EXISTS (SELECT 1 FROM slots sl WHERE sl.owner_session_id=se.id) ORDER BY se.created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SlotSummary
	for rows.Next() {
		var x SlotSummary
		if err := rows.Scan(&x.SessionID, &x.SessionState, &x.WorkspaceID, &x.AgentKind, &x.AgentSessionID, &x.CreatedAt, &x.ArchivedAt, &x.ExpiresAt); err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

// SlotUsageLocation は使用量測定のために slot 内の repository 1 個の置き場所を表す。
type SlotUsageLocation struct {
	SlotID   string
	RootPath string
	RelPath  string
	DirName  string
	MainPath string
}

// slotUsageLocationQuery はDB 管理下で回収前の slot の置き場所を引く。
const slotUsageLocationQuery = `SELECT sl.id,rt.path,sl.rel_path,COALESCE(sr.dir_name,''),COALESCE(r.main_worktree_path,'')
 FROM slots sl JOIN roots rt ON rt.id=sl.root_id LEFT JOIN slot_repositories sr ON sr.slot_id=sl.id LEFT JOIN repositories r ON r.id=sr.repository_id
 WHERE sl.state <> 'ARCHIVED'`

// SlotUsageLocations は回収前の slot と workspace snapshot を返す。
func (s *Store) SlotUsageLocations(ctx context.Context) ([]SlotUsageLocation, error) {
	return s.slotUsageLocations(ctx, slotUsageLocationQuery+`
 UNION ALL SELECT 'snapshot:'||ws.session_id,rt.path,ws.rel_path,'','' FROM workspace_snapshots ws JOIN roots rt ON rt.id=ws.root_id WHERE ws.status <> 'EXPIRED'`)
}

// SlotUsageLocationsForSlot は slot 1 個分の測定対象を返す。
// 回収済みなら空を返し、呼び出し側はその slot の測定を諦める。
func (s *Store) SlotUsageLocationsForSlot(ctx context.Context, slotID string) ([]SlotUsageLocation, error) {
	return s.slotUsageLocations(ctx, slotUsageLocationQuery+` AND sl.id=? ORDER BY sr.dir_name`, slotID)
}

func (s *Store) slotUsageLocations(ctx context.Context, query string, args ...any) ([]SlotUsageLocation, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SlotUsageLocation
	for rows.Next() {
		var x SlotUsageLocation
		if err := rows.Scan(&x.SlotID, &x.RootPath, &x.RelPath, &x.DirName, &x.MainPath); err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

func (s *Store) Status(ctx context.Context) (Status, error) {
	var x Status
	err := s.db.QueryRowContext(ctx, `SELECT
		(SELECT count(*) FROM workspaces),
		(SELECT count(*) FROM repositories),
		(SELECT count(*) FROM slots WHERE state='READY'),
		(SELECT count(*) FROM slots WHERE state='LEASED'),
		(SELECT count(*) FROM slots WHERE state='FAILED'),
		(SELECT count(*) FROM sessions WHERE state='ACTIVE'),
		(SELECT count(*) FROM snapshots WHERE status='ARCHIVED'),
		(SELECT count(*) FROM jobs WHERE state IN ('PENDING','RUNNING')),
		(SELECT count(*) FROM slots WHERE state='QUARANTINED')`).
		Scan(&x.Workspaces, &x.Repositories, &x.Ready, &x.Leased, &x.Failed, &x.Active, &x.Snapshots, &x.Jobs, &x.Quarantined)
	return x, err
}

func (s *Store) StatusDiagnostics(ctx context.Context) (StatusDiagnostics, error) {
	var out StatusDiagnostics
	workspaceRows, err := s.db.QueryContext(ctx, `SELECT w.id,w.root_path,w.generation,(SELECT count(*) FROM workspace_repositories wr WHERE wr.workspace_id=w.id),(SELECT count(*) FROM slots sl WHERE sl.workspace_id=w.id AND sl.state='READY'),(SELECT count(*) FROM slots sl WHERE sl.workspace_id=w.id AND sl.state='LEASED'),(SELECT count(*) FROM slots sl WHERE sl.workspace_id=w.id AND sl.state IN ('FAILED','QUARANTINED')),COALESCE((SELECT MAX(se.created_at) FROM sessions se WHERE se.workspace_id=w.id),'') FROM workspaces w ORDER BY w.root_path`)
	if err != nil {
		return out, err
	}
	defer workspaceRows.Close()
	for workspaceRows.Next() {
		var item WorkspaceDiagnostic
		if err := workspaceRows.Scan(&item.ID, &item.Root, &item.Generation, &item.Repositories, &item.Ready, &item.Leased, &item.Failed, &item.LastUsedAt); err != nil {
			return out, err
		}
		out.Workspaces = append(out.Workspaces, item)
	}
	if err := workspaceRows.Err(); err != nil {
		return out, err
	}
	if err := workspaceRows.Close(); err != nil {
		return out, err
	}
	sessionRows, err := s.db.QueryContext(ctx, `SELECT se.id,se.agent_kind,se.state,se.created_at,COALESCE(group_concat(sr.base_oid,','),'') FROM sessions se LEFT JOIN slot_repositories sr ON sr.slot_id=se.slot_id WHERE se.state<>'EXPIRED' GROUP BY se.id ORDER BY se.created_at DESC`)
	if err != nil {
		return out, err
	}
	defer sessionRows.Close()
	for sessionRows.Next() {
		var item SessionDiagnostic
		if err := sessionRows.Scan(&item.ID, &item.Agent, &item.State, &item.CreatedAt, &item.BaseOIDs); err != nil {
			return out, err
		}
		out.Sessions = append(out.Sessions, item)
	}
	if err := sessionRows.Err(); err != nil {
		return out, err
	}
	if err := sessionRows.Close(); err != nil {
		return out, err
	}
	repositoryRows, err := s.db.QueryContext(ctx, `SELECT r.id,r.main_worktree_path,COALESCE(r.last_leased_at,''),COALESCE(MAX(CASE WHEN sl.owner_session_id IS NULL AND sl.state='READY' AND sr.state='READY' THEN sl.ready_at END),''),CASE WHEN count(CASE WHEN sl.state IN ('READY','LEASED') AND sr.state IN ('READY','LEASED') THEN 1 END)>0 THEN 1 ELSE 0 END FROM repositories r LEFT JOIN slot_repositories sr ON sr.repository_id=r.id LEFT JOIN slots sl ON sl.id=sr.slot_id GROUP BY r.id ORDER BY r.main_worktree_path`)
	if err != nil {
		return out, err
	}
	defer repositoryRows.Close()
	for repositoryRows.Next() {
		var item RepositoryDiagnostic
		if err := repositoryRows.Scan(&item.ID, &item.MainPath, &item.LastUsedAt, &item.StandbyReadyAt, &item.Hot); err != nil {
			return out, err
		}
		out.Repositories = append(out.Repositories, item)
	}
	if err := repositoryRows.Err(); err != nil {
		return out, err
	}
	if err := repositoryRows.Close(); err != nil {
		return out, err
	}
	if err := s.db.QueryRowContext(ctx, `SELECT count(CASE WHEN state='PENDING' THEN 1 END),count(CASE WHEN state='RUNNING' THEN 1 END),count(CASE WHEN state='FAILED' THEN 1 END) FROM jobs`).Scan(&out.Jobs.Pending, &out.Jobs.Running, &out.Jobs.Failed); err != nil {
		return out, err
	}
	if err := s.db.QueryRowContext(ctx, `SELECT count(*),COALESCE(MIN(expires_at),'') FROM snapshots WHERE status='ARCHIVED'`).Scan(&out.Snapshots.Count, &out.Snapshots.EarliestExpiry); err != nil {
		return out, err
	}
	quarantineRows, err := s.db.QueryContext(ctx, `SELECT sl.id,rt.path||'/'||sl.rel_path,'slot',COALESCE(sl.failure_code,'') FROM slots sl JOIN roots rt ON rt.id=sl.root_id WHERE sl.state='QUARANTINED' UNION ALL SELECT '',path,kind,reason FROM quarantined_artifacts ORDER BY 2`)
	if err != nil {
		return out, err
	}
	defer quarantineRows.Close()
	for quarantineRows.Next() {
		var item QuarantineDiagnostic
		if err := quarantineRows.Scan(&item.ID, &item.Path, &item.Kind, &item.FailureCode); err != nil {
			return out, err
		}
		out.Quarantine = append(out.Quarantine, item)
	}
	return out, quarantineRows.Err()
}

// StandbyReplenishmentDiagnostics は待機用 worktree の補充を停止中の workspace を返す。
// 停止は `replenish_suspensions` が唯一の権威なので、隔離 slot の数は判定にも表示にも使わない。
func (s *Store) StandbyReplenishmentDiagnostics(ctx context.Context) ([]StandbyReplenishmentDiagnostic, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT w.id,w.root_path,w.generation,rs.reason,rs.detail,rs.suspended_at
		FROM replenish_suspensions rs JOIN workspaces w ON w.id=rs.workspace_id ORDER BY w.root_path`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []StandbyReplenishmentDiagnostic{}
	for rows.Next() {
		var item StandbyReplenishmentDiagnostic
		if err := rows.Scan(&item.WorkspaceID, &item.Root, &item.Generation, &item.Reason, &item.Detail, &item.SuspendedAt); err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}
