package state

import (
	"context"
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
