package state

import (
	"context"
	"time"
)

// UpdateApplyMaxAttempts は 1 つの版について自動適用を始める回数の上限である。
// 適用は切り離した子 process が行い、親はその成否を観測できない。
// 上限が無いと、確実に失敗する版に対して保守の一巡ごとの起動を繰り返す。
const UpdateApplyMaxAttempts = 3

// UpdateApplyRetryInterval は前回の試行を失敗とみなして次を許すまでの間隔である。
// 成功した試行は daemon 自身が置き換わって消えるため、この窓が過ぎても残っている記録は失敗を意味する。
const UpdateApplyRetryInterval = 6 * time.Hour

// ClaimUpdateApply は version の自動適用を始める権利を 1 つの呼び出しへ渡す。初回は行を足せた呼び出しだけ、
// 2 回目以降は試行上限と再試行間隔を満たす行を更新できた呼び出しだけが権利を得る。権利は子 process の
// 起動直前に取り、起動が失敗しても試行として残す。残さないと失敗が続く版で起動を繰り返す。

// 戻り値は権利を得た呼び出しにとっての試行回数で、得られなければ 0 である。
// 呼び出し側はこれが UpdateApplyMaxAttempts と等しいときを最後の試行として扱える。
func (s *Store) ClaimUpdateApply(ctx context.Context, version string, now time.Time) (int, error) {
	if version == "" {
		return 0, nil
	}
	s.writer.Lock()
	defer s.writer.Unlock()
	retryBefore := FormatTime(now.Add(-UpdateApplyRetryInterval))
	result, err := s.db.ExecContext(ctx, `INSERT INTO update_applies(version,attempts,attempted_at) VALUES(?,1,?)
		ON CONFLICT(version) DO UPDATE SET attempts=attempts+1,attempted_at=excluded.attempted_at
		WHERE update_applies.attempts < ? AND update_applies.attempted_at <= ?`,
		version, FormatTime(now), UpdateApplyMaxAttempts, retryBefore)
	if err != nil {
		return 0, err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return 0, err
	}
	if changed != 1 {
		return 0, nil
	}
	var attempts int
	row := s.db.QueryRowContext(ctx, `SELECT attempts FROM update_applies WHERE version=?`, version)
	if err := row.Scan(&attempts); err != nil {
		return 0, err
	}
	return attempts, nil
}
