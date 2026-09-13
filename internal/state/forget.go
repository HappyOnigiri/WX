package state

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// ErrWorkspaceInUse は、貸出中の slot・終了していない session・実行待ちの job が残るため forget を断ったことを示す。
var ErrWorkspaceInUse = errors.New("workspace is still in use")

// ErrWorkspaceHasRecovery は、復元資産が残るため forget を断ったことを示す。破棄を選べば解除できる。
var ErrWorkspaceHasRecovery = errors.New("workspace still has recovery state")

// ForgetBlockers は wx forget を断る理由を、貸出中のものと復元資産に分けて数える。
// 呼び出し側はこの区別で案内文を出し分ける。
type ForgetBlockers struct {
	ActiveSlots  int
	LiveSessions int
	ActiveJobs   int

	RecoverySlots      int
	RecoverySessions   int
	Snapshots          int
	WorkspaceSnapshots int
}

// Err は断る理由があればそれを表すエラーを返す。貸出中を先に返すのは、
// 復元資産の破棄では解消せず、案内する操作が違うためである。
func (b ForgetBlockers) Err() error {
	if b.ActiveSlots+b.LiveSessions+b.ActiveJobs > 0 {
		return fmt.Errorf("%w: %d slot(s), %d unfinished session(s), %d pending or running job(s); end the sessions or run wx release before forgetting it",
			ErrWorkspaceInUse, b.ActiveSlots, b.LiveSessions, b.ActiveJobs)
	}
	if b.RecoverySlots+b.RecoverySessions+b.Snapshots+b.WorkspaceSnapshots > 0 {
		return fmt.Errorf("%w: %d saved or quarantined worktree(s), %d recoverable session(s), %d repository snapshot(s), %d workspace snapshot(s); rerun wx forget with --discard-recovery to discard them",
			ErrWorkspaceHasRecovery, b.RecoverySlots, b.RecoverySessions, b.Snapshots, b.WorkspaceSnapshots)
	}
	return nil
}

// forgetBlockers は forget を断る理由を数える。
// pendingReclaim は呼び出し側がこの後 READY・STALE・FAILED の未貸出 slot を回収することを表し、
// その場合だけそれらを数えない。ForgetWorkspace 自身は回収しないので false で呼ぶ。
func forgetBlockers(ctx context.Context, tx *sql.Tx, id string, pendingReclaim bool) (ForgetBlockers, error) {
	var b ForgetBlockers
	// SNAPSHOTTED・QUARANTINED の slot は復元のために worktree を保持しているので、貸出中とは分けて数える。
	activeSlots := `SELECT count(*) FROM slots WHERE workspace_id=? AND state NOT IN ('ARCHIVED','SNAPSHOTTED','QUARANTINED')`
	if pendingReclaim {
		activeSlots += ` AND NOT (owner_session_id IS NULL AND state IN ('READY','STALE','FAILED'))`
	}
	counts := []struct {
		query string
		into  *int
	}{
		{activeSlots, &b.ActiveSlots},
		{`SELECT count(*) FROM slots WHERE workspace_id=? AND state IN ('SNAPSHOTTED','QUARANTINED')`, &b.RecoverySlots},
		{`SELECT count(*) FROM sessions WHERE workspace_id=? AND state NOT IN ('EXPIRED','ARCHIVED','QUARANTINED')`, &b.LiveSessions},
		{`SELECT count(*) FROM sessions WHERE workspace_id=? AND state IN ('ARCHIVED','QUARANTINED')`, &b.RecoverySessions},
		{`SELECT count(*) FROM snapshots sn JOIN sessions se ON se.id=sn.session_id WHERE se.workspace_id=?`, &b.Snapshots},
		{`SELECT count(*) FROM workspace_snapshots ws JOIN sessions se ON se.id=ws.session_id WHERE se.workspace_id=?`, &b.WorkspaceSnapshots},
	}
	for _, count := range counts {
		if err := tx.QueryRowContext(ctx, count.query, id).Scan(count.into); err != nil {
			return ForgetBlockers{}, err
		}
	}
	// 待ちのままの補充の再確認は workspace が消えれば意味を失うので、forget を断らせない。
	// 実行中の job は他の kind と同じく断る条件に残す。
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM jobs j WHERE j.state IN ('PENDING','RUNNING') AND NOT (j.kind='ENSURE_STANDBY' AND j.state='PENDING')
		AND (j.workspace_id=? OR EXISTS (SELECT 1 FROM sessions se WHERE se.id=j.session_id AND se.workspace_id=?))`, id, id).Scan(&b.ActiveJobs); err != nil {
		return ForgetBlockers{}, err
	}
	return b, nil
}

// WorkspaceForgetBlockers は root path の workspace について forget を断る理由を数える。
// 実体の回収を始める前の判定に使うため、この後回収する READY・STALE・FAILED の未貸出 slot は数えない。
func (s *Store) WorkspaceForgetBlockers(ctx context.Context, root string) (ForgetBlockers, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ForgetBlockers{}, err
	}
	defer tx.Rollback()
	var id string
	if err := tx.QueryRowContext(ctx, `SELECT id FROM workspaces WHERE root_path=?`, root).Scan(&id); err != nil {
		return ForgetBlockers{}, err
	}
	return forgetBlockers(ctx, tx, id, true)
}

// ForgetSlot は forget が自分で回収できる slot 1 件である。
type ForgetSlot struct{ ID, State string }

// ReclaimableSlots は workspace の slot のうち forget が回収できるものを返す。
// READY・STALE・FAILED は貸出前の待機枠で利用者の作業を含まない。
// SNAPSHOTTED・QUARANTINED は復元資産なので、破棄を選んだときだけ回収してよい。
func (s *Store) ReclaimableSlots(ctx context.Context, workspaceID string) ([]ForgetSlot, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,state FROM slots WHERE workspace_id=?
		AND ((owner_session_id IS NULL AND state IN ('READY','STALE','FAILED')) OR state IN ('SNAPSHOTTED','QUARANTINED')) ORDER BY id`, workspaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ForgetSlot
	for rows.Next() {
		var slot ForgetSlot
		if err := rows.Scan(&slot.ID, &slot.State); err != nil {
			return nil, err
		}
		out = append(out, slot)
	}
	return out, rows.Err()
}

// RecoveryStateSessions は workspace に復元資産を残している session を返す。
// 状態だけでなく snapshot 行の有無も見るのは、EXPIRED で終端したまま行が残る経路があるためである。
func (s *Store) RecoveryStateSessions(ctx context.Context, workspaceID string) ([]Session, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,state FROM sessions WHERE workspace_id=?
		AND (state IN ('ARCHIVED','QUARANTINED')
		  OR EXISTS (SELECT 1 FROM snapshots sn WHERE sn.session_id=sessions.id)
		  OR EXISTS (SELECT 1 FROM workspace_snapshots ws WHERE ws.session_id=sessions.id)) ORDER BY id`, workspaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Session
	for rows.Next() {
		var session Session
		if err := rows.Scan(&session.ID, &session.State); err != nil {
			return nil, err
		}
		out = append(out, session)
	}
	return out, rows.Err()
}

// ForgetWorkspace は workspace 登録を消す。前提の検査は forgetBlockers に集め、
// 実体の回収を終えていない slot が1つでも残っていれば断る。
// 下で workspace_id を消すと ValidateWorktreeOwnership は workspace link を必要とするため、
// 残した slot の物理 path の所有権を二度と証明できず、worktree が回収不能な leak になる。
// commentlint:allow-long -- 未回収の slot を残したまま登録を消せない理由を説明する
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
	if _, err := tx.ExecContext(ctx, `DELETE FROM jobs WHERE kind='ENSURE_STANDBY' AND workspace_id=? AND state='PENDING'`, id); err != nil {
		return err
	}
	blockers, err := forgetBlockers(ctx, tx, id, false)
	if err != nil {
		return err
	}
	if err := blockers.Err(); err != nil {
		return err
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
