package state

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// BeginStagedPreparation は二段階準備の再実行を防ぐ durable な開始境界である。
// slot lock を取得した呼び出し元だけが、ファイルへの最初の書き込み前に記録する。
func (s *Store) BeginStagedPreparation(ctx context.Context, id string) error {
	s.writer.Lock()
	defer s.writer.Unlock()
	result, err := s.db.ExecContext(ctx, `UPDATE slots SET preparation_started_at=?,updated_at=? WHERE id=? AND state IN ('PREPARING') AND preparation_started_at IS NULL`, now(), now(), id)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return fmt.Errorf("slot %s staged preparation compare-and-swap failed", id)
	}
	return nil
}

// MarkEarlyReady は全リポジトリと workspace root の先行配置を完了した後に一度だけ記録する。
// 完全な READY 遷移や hook の解除は行わない。
func (s *Store) MarkEarlyReady(ctx context.Context, id string) error {
	s.writer.Lock()
	defer s.writer.Unlock()
	result, err := s.db.ExecContext(ctx, `UPDATE slots SET early_ready_at=?,updated_at=? WHERE id=? AND state IN ('PREPARING') AND preparation_started_at IS NOT NULL AND early_ready_at IS NULL`, now(), now(), id)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return fmt.Errorf("slot %s early readiness compare-and-swap failed", id)
	}
	return nil
}

// PrepareFailureNotice は貸出を続けている slot の準備失敗を、エージェントへ伝えるための記録である。
type PrepareFailureNotice struct {
	// Phase は準備が止まった区間名で、区間の粒度まで分からない失敗では空になる。
	Phase string
	// Code は slots.failure_code、DetailPath は失敗の詳細ログの場所である。
	Code, DetailPath string
}

// RecordEarlyReadyPrepareFailure は early ready 済みで owner session が生きている slot に、隔離せず失敗だけを記録する。
// エージェントは既にこの worktree で作業しており、隔離すると返却が snapshot へ届かず作業が失われるためである。
// slot は PREPARING のまま残し、呼び出し元が通常の完了遷移で LEASED まで進める。
// RELEASING を受けるのは、early ready 後に返却が先着すると slot が PREPARING のまま session だけ
// RELEASING になるためである。ここで弾くと隔離へ倒れ、返却済みの作業が snapshot へ届かない。
// 完了遷移はこの session 状態を見て LEASED ではなく DRAINING を選び、SNAPSHOT ジョブへ繋ぐ。
// commentlint:allow-long -- 受理する owner session 状態を広げた根拠を不変条件ごと保守時に確認できるようにする
func (s *Store) RecordEarlyReadyPrepareFailure(ctx context.Context, id, code, detailPath, phase string) error {
	s.writer.Lock()
	defer s.writer.Unlock()
	result, err := s.db.ExecContext(ctx, `UPDATE slots SET failure_code=?,failure_detail_path=?,failure_phase=?,updated_at=?
		WHERE id=? AND state='PREPARING' AND early_ready_at IS NOT NULL
		  AND EXISTS (SELECT 1 FROM sessions se WHERE se.id=slots.owner_session_id AND se.state IN ('STARTING','ACTIVE','RELEASING'))`,
		nullString(code), nullString(detailPath), nullString(phase), now(), id)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return fmt.Errorf("slot %s early ready failure compare-and-swap failed", id)
	}
	return nil
}

// ClaimPrepareFailureNotice は貸出を続けている slot の準備失敗を、session ごとに 1 回だけ返す。
// 2 度目以降と、失敗を記録していない session では ok=false を返す。
// 隔離・失敗状態の slot はこの経路で継続していないので対象にしない。
func (s *Store) ClaimPrepareFailureNotice(ctx context.Context, sessionID string) (PrepareFailureNotice, bool, error) {
	s.writer.Lock()
	defer s.writer.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return PrepareFailureNotice{}, false, err
	}
	defer tx.Rollback()
	var notice PrepareFailureNotice
	err = tx.QueryRowContext(ctx, `SELECT COALESCE(sl.failure_code,''),COALESCE(sl.failure_detail_path,''),COALESCE(sl.failure_phase,'')
		FROM sessions se JOIN slots sl ON sl.id=se.slot_id
		WHERE se.id=? AND COALESCE(sl.failure_code,'')<>'' AND sl.state NOT IN ('FAILED','QUARANTINED')`, sessionID).
		Scan(&notice.Code, &notice.DetailPath, &notice.Phase)
	if errors.Is(err, sql.ErrNoRows) {
		return PrepareFailureNotice{}, false, nil
	}
	if err != nil {
		return PrepareFailureNotice{}, false, err
	}
	result, err := tx.ExecContext(ctx, `UPDATE sessions SET prepare_notice_delivered_at=? WHERE id=? AND prepare_notice_delivered_at IS NULL`, now(), sessionID)
	if err != nil {
		return PrepareFailureNotice{}, false, err
	}
	if n, _ := result.RowsAffected(); n != 1 {
		return PrepareFailureNotice{}, false, nil
	}
	return notice, true, tx.Commit()
}
