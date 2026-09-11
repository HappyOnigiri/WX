package state

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/HappyOnigiri/WX/internal/discovery"
	"github.com/HappyOnigiri/WX/internal/domain"
)

// ErrWorkspaceKindConflict は、同じ root path が別の workspace kind で登録済みであることを示す。
var ErrWorkspaceKindConflict = errors.New("workspace root is already registered with a different kind")

// ErrWorkspaceIdentityConflict は、同じ root path が別の workspace identity で登録済みであることを示す。
var ErrWorkspaceIdentityConflict = errors.New("workspace root is already registered with a different identity")

// CanonicalWorkspace は discovery の新しい proposal を、既登録 workspace の identity に置き換えて返す。single-repository は Git common directory、
// multi-repository は canonical root path で証明し、workspace ID 自体では証明しない。
// slot directory 名である ID を変えると既存 pool を取り残し、main worktree 移動後に重複 workspace を作る。
// commentlint:allow-long -- workspace identity を ID 以外で証明する理由を説明する
func (s *Store) CanonicalWorkspace(ctx context.Context, w discovery.Workspace) (discovery.Workspace, error) {
	s.writer.Lock()
	defer s.writer.Unlock()
	registered, found, err := s.registeredWorkspaceID(ctx, w)
	if err != nil {
		return w, err
	}
	if found {
		w.ID = domain.WorkspaceID(registered)
	}
	return w, nil
}

// registeredWorkspaceID は既登録 workspace の durable ID を解決し、wx が未観測なら found=false を返す。
func (s *Store) registeredWorkspaceID(ctx context.Context, w discovery.Workspace) (string, bool, error) {
	var query string
	var argument any
	switch {
	case w.Kind == "repository" && len(w.Repositories) == 1:
		query = `SELECT DISTINCT w.id FROM workspaces w JOIN workspace_repositories wr ON wr.workspace_id=w.id JOIN repositories r ON r.id=wr.repository_id WHERE w.kind='repository' AND r.common_git_dir=? ORDER BY w.id`
		argument = string(w.Repositories[0].CommonDir)
	case w.Kind == "multi_repository":
		query = `SELECT id FROM workspaces WHERE kind='multi_repository' AND root_path=? ORDER BY id`
		argument = string(w.Root)
	default:
		return "", false, nil
	}
	rows, err := s.db.QueryContext(ctx, query, argument)
	if err != nil {
		return "", false, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return "", false, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return "", false, err
	}
	if len(ids) > 1 {
		return "", false, fmt.Errorf("workspace identity %v belongs to multiple registered workspaces", argument)
	}
	var rootID, rootKind string
	err = s.db.QueryRowContext(ctx, `SELECT id,kind FROM workspaces WHERE root_path=?`, w.Root).Scan(&rootID, &rootKind)
	if errors.Is(err, sql.ErrNoRows) {
		if len(ids) == 0 {
			return "", false, nil
		}
		return ids[0], true, nil
	}
	if err != nil {
		return "", false, err
	}
	if len(ids) == 1 && rootID == ids[0] {
		return ids[0], true, nil
	}
	if rootKind != w.Kind {
		return "", false, fmt.Errorf("%w: root %q is registered as workspace %s with kind %q; archive and run wx forget %q before registering discovered kind %q", ErrWorkspaceKindConflict, w.Root, rootID, rootKind, w.Root, w.Kind)
	}
	return "", false, fmt.Errorf("%w: root %q is registered as workspace %s, but its %s identity does not match; archive and run wx forget %q before registering it again", ErrWorkspaceIdentityConflict, w.Root, rootID, w.Kind, w.Root)
}

// UpsertWorkspaceGeneration は workspace を登録し、解決済み durable ID を持つ値を返す。呼び出し元は返却値を使う。
// その ID が slot directory 名であり、既知 workspace や generated ID 再抽選時には discovery の proposal と異なる。
func (s *Store) UpsertWorkspaceGeneration(ctx context.Context, w discovery.Workspace) (discovery.Workspace, int, error) {
	s.writer.Lock()
	defer s.writer.Unlock()
	registered, found, err := s.registeredWorkspaceID(ctx, w)
	if err != nil {
		return w, 0, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return w, 0, err
	}
	defer tx.Rollback()
	if found {
		w.ID = domain.WorkspaceID(registered)
	} else {
		// discovery の proposal は random なので、無関係な workspace と衝突し得る。
		// transaction 内で引き直し、下の INSERT がその row を奪わないようにする。
		id, idErr := newUnusedShortID(ctx, tx, "workspaces")
		if idErr != nil {
			return w, 0, idErr
		}
		w.ID = domain.WorkspaceID(id)
	}
	t := now()
	generation := 1
	existing := false
	var oldKind string
	err = tx.QueryRowContext(ctx, `SELECT generation,kind FROM workspaces WHERE id=?`, w.ID).Scan(&generation, &oldKind)
	if err == nil {
		existing = true
	} else if !errors.Is(err, sql.ErrNoRows) {
		return w, 0, err
	}
	type membership struct {
		rel     string
		ordinal int
	}
	oldMembers := map[string]membership{}
	if existing {
		rows, err := tx.QueryContext(ctx, `SELECT repository_id,relative_path,ordinal FROM workspace_repositories WHERE workspace_id=?`, w.ID)
		if err != nil {
			return w, 0, err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var id string
			var member membership
			if err := rows.Scan(&id, &member.rel, &member.ordinal); err != nil {
				_ = rows.Close()
				return w, 0, err
			}
			oldMembers[id] = member
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return w, 0, err
		}
		if err := rows.Close(); err != nil {
			return w, 0, err
		}
	}
	changed := existing && oldKind != w.Kind
	if len(oldMembers) != len(w.Repositories) && existing {
		changed = true
	}
	for i, repo := range w.Repositories {
		old, ok := oldMembers[string(repo.ID)]
		if existing && (!ok || old.rel != repo.RelativePath || old.ordinal != i) {
			changed = true
		}
	}
	if changed {
		generation++
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO workspaces(id,root_path,kind,generation,discovery_state,first_seen_at,last_seen_at,last_reconciled_at) VALUES(?,?,?,?,'READY',?,?,?) ON CONFLICT(id) DO UPDATE SET root_path=excluded.root_path,kind=excluded.kind,generation=excluded.generation,last_seen_at=excluded.last_seen_at,last_reconciled_at=excluded.last_reconciled_at`, w.ID, w.Root, w.Kind, generation, t, t, t)
	if err != nil {
		return w, 0, err
	}
	if existing {
		if _, err := tx.ExecContext(ctx, `DELETE FROM workspace_repositories WHERE workspace_id=?`, w.ID); err != nil {
			return w, 0, err
		}
	}
	for i, r := range w.Repositories {
		_, err = tx.ExecContext(ctx, `INSERT INTO repositories(id,main_worktree_path,common_git_dir,default_branch,remote_name,first_seen_at,last_seen_at) VALUES(?,?,?,?,?,?,?) ON CONFLICT(id) DO UPDATE SET main_worktree_path=excluded.main_worktree_path,common_git_dir=excluded.common_git_dir,default_branch=excluded.default_branch,remote_name=CASE WHEN excluded.remote_name<>'' THEN excluded.remote_name ELSE repositories.remote_name END,last_seen_at=excluded.last_seen_at`, r.ID, r.MainPath, r.CommonDir, r.DefaultBranch, r.RemoteName, t, t)
		if err != nil {
			return w, 0, err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO workspace_repositories(workspace_id,repository_id,relative_path,ordinal) VALUES(?,?,?,?) ON CONFLICT(workspace_id,repository_id) DO UPDATE SET relative_path=excluded.relative_path,ordinal=excluded.ordinal`, w.ID, r.ID, r.RelativePath, i)
		if err != nil {
			return w, 0, err
		}
	}
	if changed {
		if _, err := tx.ExecContext(ctx, `UPDATE slots SET state='STALE',updated_at=? WHERE workspace_id=? AND generation<? AND owner_session_id IS NULL AND state IN ('PREPARING','READY')`, t, w.ID, generation); err != nil {
			return w, 0, err
		}
	}
	return w, generation, tx.Commit()
}

func (s *Store) WorkspaceGeneration(ctx context.Context, workspaceID string) (int, error) {
	var generation int
	err := s.db.QueryRowContext(ctx, `SELECT generation FROM workspaces WHERE id=?`, workspaceID).Scan(&generation)
	return generation, err
}

func (s *Store) Repository(ctx context.Context, id string) (discovery.Repository, error) {
	var r discovery.Repository
	err := s.db.QueryRowContext(ctx, `SELECT id,main_worktree_path,common_git_dir,default_branch,remote_name FROM repositories WHERE id=?`, id).Scan(&r.ID, &r.MainPath, &r.CommonDir, &r.DefaultBranch, &r.RemoteName)
	return r, err
}

func (s *Store) Workspace(ctx context.Context, id string) (discovery.Workspace, error) {
	var w discovery.Workspace
	err := s.db.QueryRowContext(ctx, `SELECT id,root_path,kind FROM workspaces WHERE id=?`, id).Scan(&w.ID, &w.Root, &w.Kind)
	if err != nil {
		return w, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT r.id,r.main_worktree_path,r.common_git_dir,wr.relative_path,r.default_branch,r.remote_name FROM workspace_repositories wr JOIN repositories r ON r.id=wr.repository_id WHERE wr.workspace_id=? ORDER BY wr.ordinal`, id)
	if err != nil {
		return w, err
	}
	defer rows.Close()
	for rows.Next() {
		var r discovery.Repository
		if err := rows.Scan(&r.ID, &r.MainPath, &r.CommonDir, &r.RelativePath, &r.DefaultBranch, &r.RemoteName); err != nil {
			return w, err
		}
		w.Repositories = append(w.Repositories, r)
	}
	return w, rows.Err()
}

func (s *Store) WorkspaceByRoot(ctx context.Context, root string) (discovery.Workspace, error) {
	var id string
	if err := s.db.QueryRowContext(ctx, `SELECT id FROM workspaces WHERE root_path=?`, root).Scan(&id); err != nil {
		return discovery.Workspace{}, err
	}
	return s.Workspace(ctx, id)
}

// SessionWorkspaceKind は session の workspace kind（"repository" または "multi_repository"）だけを解決し、
// SessionWorkspace が要求する repository membership は要求しない。kind だけで分岐する caller は repository list に触れず、
// membership が0件でも root-snapshot 処理を判定できる必要があるためである。
func (s *Store) SessionWorkspaceKind(ctx context.Context, sessionID string) (string, error) {
	var kind string
	err := s.db.QueryRowContext(ctx, `SELECT w.kind FROM sessions se JOIN workspaces w ON w.id=se.workspace_id WHERE se.id=?`, sessionID).Scan(&kind)
	return kind, err
}

// SessionWorkspace は session の workspace に少なくとも1つの repository membership row（session_repositories）も要求する。
// 0件では repository set を検証できず、w.Repositories を反復する caller がその証明に依存するためである。
// w.Kind だけが必要な caller は、この要求を持たない SessionWorkspaceKind を使う。
func (s *Store) SessionWorkspace(ctx context.Context, sessionID string) (discovery.Workspace, error) {
	var w discovery.Workspace
	err := s.db.QueryRowContext(ctx, `SELECT w.id,w.root_path,w.kind FROM sessions se JOIN workspaces w ON w.id=se.workspace_id WHERE se.id=?`, sessionID).Scan(&w.ID, &w.Root, &w.Kind)
	if err != nil {
		return w, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT r.id,r.main_worktree_path,r.common_git_dir,sr.relative_path,r.default_branch,r.remote_name FROM session_repositories sr JOIN repositories r ON r.id=sr.repository_id WHERE sr.session_id=? ORDER BY sr.ordinal`, sessionID)
	if err != nil {
		return w, err
	}
	defer rows.Close()
	for rows.Next() {
		var r discovery.Repository
		if err := rows.Scan(&r.ID, &r.MainPath, &r.CommonDir, &r.RelativePath, &r.DefaultBranch, &r.RemoteName); err != nil {
			return w, err
		}
		w.Repositories = append(w.Repositories, r)
	}
	if err := rows.Err(); err != nil {
		return w, err
	}
	if len(w.Repositories) == 0 {
		return w, errors.New("session has no recorded repository membership")
	}
	return w, nil
}

func (s *Store) WorkspaceRoots(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT root_path FROM workspaces ORDER BY root_path`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var roots []string
	for rows.Next() {
		var root string
		if err := rows.Scan(&root); err != nil {
			return nil, err
		}
		roots = append(roots, root)
	}
	return roots, rows.Err()
}

func (s *Store) ForgetWorkspace(ctx context.Context, root string) error {
	s.writer.Lock()
	defer s.writer.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var id string
	if err := tx.QueryRowContext(ctx, `SELECT id FROM workspaces WHERE root_path=?`, root).Scan(&id); err != nil {
		return err
	}
	var unsafe int
	// ここで FAILED を安全とは扱わない。下で workspace_id を消すと ValidateWorktreeOwnership は workspace link を
	// 必要とするため、その slot の物理 path の所有権を二度と証明できず、FAILED worktree が回収不能な leak になる。
	// caller（Manager.Forget）は先に ScheduleFailedSlotRemoval で FAILED slot を retired にするため、他に危険な状態がなければ詰まらない。
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM slots WHERE workspace_id=? AND state NOT IN ('ARCHIVED')`, id).Scan(&unsafe); err != nil {
		return err
	}
	if unsafe > 0 {
		return errors.New("workspace has active, ready, or unarchived slots")
	}
	var liveRecovery int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM sessions WHERE workspace_id=? AND state<>'EXPIRED'`, id).Scan(&liveRecovery); err != nil {
		return err
	}
	if liveRecovery > 0 {
		return errors.New("workspace has live session mappings; expire recovery state before forgetting it")
	}
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM snapshots sn JOIN sessions se ON se.id=sn.session_id WHERE se.workspace_id=?`, id).Scan(&liveRecovery); err != nil {
		return err
	}
	if liveRecovery > 0 {
		return errors.New("workspace has recovery snapshots; expire recovery state before forgetting it")
	}
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM workspace_snapshots ws JOIN sessions se ON se.id=ws.session_id WHERE se.workspace_id=?`, id).Scan(&liveRecovery); err != nil {
		return err
	}
	if liveRecovery > 0 {
		return errors.New("workspace has a workspace recovery snapshot; expire recovery state before forgetting it")
	}
	// 補充の再確認は workspace が消えれば意味を失うので、待ちのまま forget を断らせない。
	// 実行中の job は他の kind と同じく forget を断る条件に残す。
	if _, err := tx.ExecContext(ctx, `DELETE FROM jobs WHERE kind='ENSURE_STANDBY' AND workspace_id=? AND state='PENDING'`, id); err != nil {
		return err
	}
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM jobs j WHERE j.state IN ('PENDING','RUNNING') AND (j.workspace_id=? OR EXISTS (SELECT 1 FROM sessions se WHERE se.id=j.session_id AND se.workspace_id=?))`, id, id).Scan(&liveRecovery); err != nil {
		return err
	}
	if liveRecovery > 0 {
		return errors.New("workspace has pending recovery jobs; wait for them before forgetting it")
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM standby_replenish_exclusions WHERE workspace_id=?`, id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM standby_replenish_successes WHERE workspace_id=?`, id); err != nil {
		return err
	}
	// jobs.workspace_id には foreign key がなく、監査目的で削除後の workspace を参照できるため、手動で clear する必要がある唯一の参照である。
	// slots.workspace_id と sessions.workspace_id は NULL へ cascade し、workspace_repositories row は各列の foreign key により削除へ cascade する。
	if _, err := tx.ExecContext(ctx, `UPDATE jobs SET workspace_id=NULL WHERE workspace_id=?`, id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM workspaces WHERE id=?`, id); err != nil {
		return err
	}
	// workspace が消えると、その repository は登録のどこからも参照されなくなる。
	// 記録だけを残すと doctor が毎回その path の recovery ref を読みに行き、実体が消えていれば恒久的に失敗する。
	if _, err := pruneUnreferencedRepositories(ctx, tx); err != nil {
		return err
	}
	return tx.Commit()
}

// PruneRepositories は、どの登録からも必要とされなくなった repository 記録を削除し、削除件数を返す。
// forget の transaction 外に取り残された既存の記録を回収する保守経路で、Git リポジトリの実体には触れない。
func (s *Store) PruneRepositories(ctx context.Context) (int, error) {
	s.writer.Lock()
	defer s.writer.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	removed, err := pruneUnreferencedRepositories(ctx, tx)
	if err != nil {
		return 0, err
	}
	if removed == 0 {
		return 0, nil
	}
	return removed, tx.Commit()
}

// pruneUnreferencedRepositories は、workspace・snapshot・終了していない slot/session のどれからも参照されない repository を削除する。
// forget 後に残るのは workspace 所属を失った ARCHIVED slot と EXPIRED session の履歴だけで、
// その組み合わせでは ValidateWorktreeOwnership が workspace link を欠いて必ず失敗するため、履歴 row も同じ transaction で消す。
// commentlint:allow-long -- 履歴 row まで消してよい条件の根拠を残すため
func pruneUnreferencedRepositories(ctx context.Context, tx *sql.Tx) (int, error) {
	rows, err := tx.QueryContext(ctx, `SELECT r.id FROM repositories r
		WHERE NOT EXISTS (SELECT 1 FROM workspace_repositories wr WHERE wr.repository_id=r.id)
		  AND NOT EXISTS (SELECT 1 FROM snapshots sn WHERE sn.repository_id=r.id)
		  AND NOT EXISTS (SELECT 1 FROM slot_repositories sr JOIN slots sl ON sl.id=sr.slot_id
		                  WHERE sr.repository_id=r.id AND NOT (sl.state='ARCHIVED' AND sl.workspace_id IS NULL))
		  AND NOT EXISTS (SELECT 1 FROM session_repositories ser JOIN sessions se ON se.id=ser.session_id
		                  WHERE ser.repository_id=r.id AND NOT (se.state='EXPIRED' AND se.workspace_id IS NULL))
		ORDER BY r.id`)
	if err != nil {
		return 0, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return 0, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return 0, err
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}
	for _, id := range ids {
		// foreign key があるため、履歴の membership を先に消してから repository row を消す。
		if _, err := tx.ExecContext(ctx, `DELETE FROM session_repositories WHERE repository_id=?`, id); err != nil {
			return 0, err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM slot_repositories WHERE repository_id=?`, id); err != nil {
			return 0, err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM repositories WHERE id=?`, id); err != nil {
			return 0, err
		}
	}
	return len(ids), nil
}

// RegisteredRepositoryIDs は登録済み workspace に属する repository の id を返す。
// forget などで所属が消えた記録と、まだ使う予定のある repository を区別するために使う。
func (s *Store) RegisteredRepositoryIDs(ctx context.Context) (map[string]bool, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT repository_id FROM workspace_repositories`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	registered := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		registered[id] = true
	}
	return registered, rows.Err()
}

func (s *Store) Repositories(ctx context.Context) ([]discovery.Repository, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,main_worktree_path,common_git_dir,default_branch,remote_name FROM repositories ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var repositories []discovery.Repository
	for rows.Next() {
		var repository discovery.Repository
		if err := rows.Scan(&repository.ID, &repository.MainPath, &repository.CommonDir, &repository.DefaultBranch, &repository.RemoteName); err != nil {
			return nil, err
		}
		repositories = append(repositories, repository)
	}
	return repositories, rows.Err()
}
