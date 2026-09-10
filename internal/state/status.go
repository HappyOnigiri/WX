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
		// Policy は設定側の worktree 方針（hot・cold・off・ask）で、DB ではなく Manager が Config から埋める。
		// schema 14 以降は常に値がある前提で表示側が方針を判定するため、省略可能にしない。
		Policy string `json:"policy"`
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
	// JobDiagnostic は job の件数で、Failed と Discarded は DB 上どちらも state='FAILED' の行を数える。
	// 対処が必要な失敗と利用者が取り消した予定 job を読み分けられるように、error_code で分けて返す。
	JobDiagnostic struct {
		Pending   int `json:"pending"`
		Running   int `json:"running"`
		Failed    int `json:"failed"`
		Discarded int `json:"discarded"`
	}
	SnapshotDiagnostic struct {
		Count          int    `json:"count"`
		EarliestExpiry string `json:"earliest_expiry,omitempty"`
	}
	// ArchivedSessionDiagnostic は一覧から外した ARCHIVED session の集計である。
	// 復元用に retention.recovery_snapshot まで残るだけの行を 1 件ずつ出すと診断が埋まるため、件数と保持期間の両端だけを返す。
	ArchivedSessionDiagnostic struct {
		Count              int    `json:"count"`
		EarliestArchivedAt string `json:"earliest_archived_at,omitempty"`
		LatestExpiresAt    string `json:"latest_expires_at,omitempty"`
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
		// FailureCode 以降は停止の原因になった job から引き継ぐ失敗情報で、`wx clear` による停止では空になる。
		// 上位の「準備に失敗」で止めず、失敗した操作そのものを報告するために持つ。
		FailureCode    string `json:"failure_code,omitempty"`
		FailureMessage string `json:"failure_message,omitempty"`
		DetailPath     string `json:"detail_path,omitempty"`
	}
	StatusDiagnostics struct {
		Workspaces       []WorkspaceDiagnostic     `json:"workspaces"`
		Sessions         []SessionDiagnostic       `json:"sessions"`
		ArchivedSessions ArchivedSessionDiagnostic `json:"archived_sessions"`
		Repositories     []RepositoryDiagnostic    `json:"repositories"`
		Jobs             JobDiagnostic             `json:"jobs"`
		Snapshots        SnapshotDiagnostic        `json:"snapshots"`
		Quarantine       []QuarantineDiagnostic    `json:"quarantine"`
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
	// LeaseKind 以下は貸出の性質である。agent は wx claude / wx codex の従来経路で、
	// LeaseExpiresAt は lease.ttl の期限、LeaseOwnerSessionID は wx new を呼んだ親 session を指す。
	LeaseKind           string `json:"lease_kind,omitempty"`
	LeaseExpiresAt      string `json:"lease_expires_at,omitempty"`
	LeaseOwnerSessionID string `json:"lease_owner_session_id,omitempty"`
	// Repositories は行に紐づくソースリポジトリの main worktree のフルパスで、multi-repo workspace では複数入る。
	// 表示側で basename へ縮めるため、ここでは短縮しない。
	Repositories []string `json:"repositories,omitempty"`
}

// ListSlots は回収前（ARCHIVED 以外）の slot をすべて返し、SIZE 列の合計が `wx status` の Disk 行と同じ範囲を指すようにする。
// all ではさらに、slot を手放した session も返し、`wx resume` に渡す ID をここから辿れるようにする。
func (s *Store) ListSlots(ctx context.Context, all bool) ([]SlotSummary, error) {
	q := `SELECT sl.id,sl.state,COALESCE(sl.workspace_id,''),rt.path,sl.rel_path,COALESCE(se.id,''),COALESCE(se.state,''),COALESCE(se.agent_kind,''),COALESCE(se.agent_session_id,''),sl.created_at,COALESCE(sl.ready_at,''),COALESCE(sl.last_used_at,''),COALESCE(se.lease_kind,''),COALESCE(se.lease_expires_at,''),COALESCE(se.lease_owner_session_id,'')
		FROM slots sl JOIN roots rt ON rt.id=sl.root_id LEFT JOIN sessions se ON se.id=sl.owner_session_id`
	q += ` WHERE sl.state <> 'ARCHIVED' ORDER BY rt.path,sl.rel_path`
	rows, err := s.db.QueryContext(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SlotSummary
	for rows.Next() {
		var x SlotSummary
		var root, relative string
		if err := rows.Scan(&x.SlotID, &x.State, &x.WorkspaceID, &root, &relative, &x.SessionID, &x.SessionState, &x.AgentKind, &x.AgentSessionID, &x.CreatedAt, &x.ReadyAt, &x.LastUsedAt,
			&x.LeaseKind, &x.LeaseExpiresAt, &x.LeaseOwnerSessionID); err != nil {
			return nil, err
		}
		x.Path = filepath.Join(root, relative)
		out = append(out, x)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if all {
		detached, err := s.listDetachedSessions(ctx)
		if err != nil {
			return nil, err
		}
		out = append(out, detached...)
	}
	if err := s.fillSlotRepositories(ctx, out); err != nil {
		return nil, err
	}
	return out, nil
}

// fillSlotRepositories は各行に main worktree path を付ける。
// group_concat は並び順を保証しないため、slot は dir_name 順・session は ordinal 順で引いて Go 側で束ねる。
func (s *Store) fillSlotRepositories(ctx context.Context, rows []SlotSummary) error {
	bySlot, err := s.repositoryPaths(ctx, `SELECT sr.slot_id,r.main_worktree_path FROM slot_repositories sr JOIN repositories r ON r.id=sr.repository_id ORDER BY sr.slot_id,sr.dir_name`)
	if err != nil {
		return err
	}
	bySession, err := s.repositoryPaths(ctx, `SELECT sr.session_id,r.main_worktree_path FROM session_repositories sr JOIN repositories r ON r.id=sr.repository_id ORDER BY sr.session_id,sr.ordinal`)
	if err != nil {
		return err
	}
	for i := range rows {
		if paths := bySlot[rows[i].SlotID]; rows[i].SlotID != "" && len(paths) > 0 {
			rows[i].Repositories = paths
			continue
		}
		if paths := bySession[rows[i].SessionID]; rows[i].SessionID != "" && len(paths) > 0 {
			rows[i].Repositories = paths
		}
	}
	return nil
}

// repositoryPaths は (owner id, main worktree path) の2列を引き、owner ごとの一覧へまとめる。
func (s *Store) repositoryPaths(ctx context.Context, query string) (map[string][]string, error) {
	rows, err := s.db.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]string{}
	for rows.Next() {
		var owner, path string
		if err := rows.Scan(&owner, &path); err != nil {
			return nil, err
		}
		out[owner] = append(out[owner], path)
	}
	return out, rows.Err()
}

// listDetachedSessions は現在どの slot も借りていない session を返す。
func (s *Store) listDetachedSessions(ctx context.Context) ([]SlotSummary, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT se.id,se.state,COALESCE(se.workspace_id,''),se.agent_kind,COALESCE(se.agent_session_id,''),se.created_at,COALESCE(se.archived_at,''),COALESCE(se.expires_at,''),se.lease_kind,COALESCE(se.lease_expires_at,''),COALESCE(se.lease_owner_session_id,'')
		FROM sessions se WHERE NOT EXISTS (SELECT 1 FROM slots sl WHERE sl.owner_session_id=se.id) ORDER BY se.created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SlotSummary
	for rows.Next() {
		var x SlotSummary
		if err := rows.Scan(&x.SessionID, &x.SessionState, &x.WorkspaceID, &x.AgentKind, &x.AgentSessionID, &x.CreatedAt, &x.ArchivedAt, &x.ExpiresAt,
			&x.LeaseKind, &x.LeaseExpiresAt, &x.LeaseOwnerSessionID); err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

// SlotUsageLocation は使用量測定のために slot 内の repository 1 個の置き場所を表す。
// SlotState は測定した値をその slot の使用量として名乗ってよいかの判断に使う。
// snapshot の行は slot ではないので空になる。
type SlotUsageLocation struct {
	SlotID    string
	SlotState string
	RootPath  string
	RelPath   string
	DirName   string
	MainPath  string
}

// slotUsageLocationQuery はDB 管理下で回収前の slot の置き場所を引く。
const slotUsageLocationQuery = `SELECT sl.id,sl.state,rt.path,sl.rel_path,COALESCE(sr.dir_name,''),COALESCE(r.main_worktree_path,'')
 FROM slots sl JOIN roots rt ON rt.id=sl.root_id LEFT JOIN slot_repositories sr ON sr.slot_id=sl.id LEFT JOIN repositories r ON r.id=sr.repository_id
 WHERE sl.state <> 'ARCHIVED'`

// SlotUsageLocations は回収前の slot と workspace snapshot を返す。
func (s *Store) SlotUsageLocations(ctx context.Context) ([]SlotUsageLocation, error) {
	return s.slotUsageLocations(ctx, slotUsageLocationQuery+`
 UNION ALL SELECT 'snapshot:'||ws.session_id,'',rt.path,ws.rel_path,'','' FROM workspace_snapshots ws JOIN roots rt ON rt.id=ws.root_id WHERE ws.status <> 'EXPIRED'`)
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
		if err := rows.Scan(&x.SlotID, &x.SlotState, &x.RootPath, &x.RelPath, &x.DirName, &x.MainPath); err != nil {
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
	// 終端 state を除外列挙で落とし、新しい state が増えても診断から消えないようにする。
	// ARCHIVED は復元待ちで数千件まで積み上がるため一覧から外し、後段の集計だけで表す。
	sessionRows, err := s.db.QueryContext(ctx, `SELECT se.id,se.agent_kind,se.state,se.created_at,COALESCE(group_concat(sr.base_oid,','),'') FROM sessions se LEFT JOIN slot_repositories sr ON sr.slot_id=se.slot_id WHERE se.state NOT IN ('ARCHIVED','EXPIRED') GROUP BY se.id ORDER BY se.created_at DESC`)
	if err != nil {
		return out, err
	}
	defer sessionRows.Close()
	// 絞り込みで 0 件になっても payload の session_details を null にしないため、空スライスで初期化する。
	out.Sessions = []SessionDiagnostic{}
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
	// 一覧と同じ JOIN で ARCHIVED を戻すと GROUP BY 単位が崩れるため、独立したスカラー集計で引く。
	if err := s.db.QueryRowContext(ctx, `SELECT count(*),COALESCE(MIN(archived_at),''),COALESCE(MAX(expires_at),'') FROM sessions WHERE state='ARCHIVED'`).
		Scan(&out.ArchivedSessions.Count, &out.ArchivedSessions.EarliestArchivedAt, &out.ArchivedSessions.LatestExpiresAt); err != nil {
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
	// FAILED は error_code で 2 列に分ける。取り消しは記録として FAILED のまま残るので、行を消さずに集計側で除く。
	canceled := placeholders(len(canceledJobErrorCodes))
	jobArgs := make([]any, 0, len(canceledJobErrorCodes)*2)
	for range 2 {
		for _, code := range canceledJobErrorCodes {
			jobArgs = append(jobArgs, code)
		}
	}
	if err := s.db.QueryRowContext(ctx, `SELECT count(CASE WHEN state='PENDING' THEN 1 END),count(CASE WHEN state='RUNNING' THEN 1 END),
		count(CASE WHEN state='FAILED' AND COALESCE(error_code,'') NOT IN (`+canceled+`) THEN 1 END),
		count(CASE WHEN state='FAILED' AND error_code IN (`+canceled+`) THEN 1 END) FROM jobs`, jobArgs...).
		Scan(&out.Jobs.Pending, &out.Jobs.Running, &out.Jobs.Failed, &out.Jobs.Discarded); err != nil {
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
// detail は停止理由ごとに意味が違い、準備失敗では job ID なので、その job の失敗情報も FAILED に限って併せて読む。再試行待ちの error_code は失敗の確定ではない。
func (s *Store) StandbyReplenishmentDiagnostics(ctx context.Context) ([]StandbyReplenishmentDiagnostic, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT w.id,w.root_path,w.generation,rs.reason,rs.detail,rs.suspended_at,
		COALESCE(j.error_code,''),COALESCE(j.error_message,''),COALESCE(j.error_detail_path,'')
		FROM replenish_suspensions rs JOIN workspaces w ON w.id=rs.workspace_id
		LEFT JOIN jobs j ON j.id=rs.detail AND j.state='FAILED' AND rs.reason=? ORDER BY w.root_path`, SuspendReplenishReasonStandbyFailure)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []StandbyReplenishmentDiagnostic{}
	for rows.Next() {
		var item StandbyReplenishmentDiagnostic
		if err := rows.Scan(&item.WorkspaceID, &item.Root, &item.Generation, &item.Reason, &item.Detail, &item.SuspendedAt,
			&item.FailureCode, &item.FailureMessage, &item.DetailPath); err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}
