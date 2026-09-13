package state

import (
	"context"
	"time"
)

// UpdateCheck は daemon が最後に行った更新確認の記録である。
// 失敗した確認も CheckedAt を進めるため、LastError が空でないときの CheckedAt は「最後に試した時刻」を表す。
type UpdateCheck struct {
	CheckedAt     time.Time
	LatestVersion string
	ReleaseURL    string
	LastError     string
}

// UpdateCheck は singleton 行を読む。migration が初期行を入れるため、行の欠落は想定しない。
func (s *Store) UpdateCheck(ctx context.Context) (UpdateCheck, error) {
	var record UpdateCheck
	var checkedAt string
	row := s.db.QueryRowContext(ctx, `SELECT checked_at,latest_version,release_url,last_error FROM update_checks WHERE id=1`)
	if err := row.Scan(&checkedAt, &record.LatestVersion, &record.ReleaseURL, &record.LastError); err != nil {
		return UpdateCheck{}, err
	}
	if checkedAt != "" {
		parsed, err := ParseTime(checkedAt)
		if err != nil {
			return UpdateCheck{}, err
		}
		record.CheckedAt = parsed
	}
	return record, nil
}

// RecordUpdateCheck は1回の確認の結果を書く。失敗（failure が非空）でも checked_at を進め、
// オフラインが続く間に保守の一巡ごとへ確認が張り付いてレート制限へ触れるのを防ぐ。
// 失敗時は直前に取得できていた版と URL を残す。
func (s *Store) RecordUpdateCheck(ctx context.Context, latest, releaseURL, failure string) error {
	s.writer.Lock()
	defer s.writer.Unlock()
	if failure != "" {
		_, err := s.db.ExecContext(ctx, `UPDATE update_checks SET checked_at=?,last_error=? WHERE id=1`, now(), failure)
		return err
	}
	_, err := s.db.ExecContext(ctx, `UPDATE update_checks SET checked_at=?,latest_version=?,release_url=?,last_error='' WHERE id=1`, now(), latest, releaseURL)
	return err
}

// ClaimUpdateAnnouncement は version についての案内権を1回だけ渡す。案内済みの版を履歴へ入れ、
// 1行を足せた呼び出しだけ true を返す。直前の版だけを覚えると、最新が A→B→A と動いたとき A が二度案内される。
// 権利を取った process が案内の前に落ちるとその版は案内されないが、重ねて出すより望ましいとして許容する。
func (s *Store) ClaimUpdateAnnouncement(ctx context.Context, version string) (bool, error) {
	if version == "" {
		return false, nil
	}
	s.writer.Lock()
	defer s.writer.Unlock()
	result, err := s.db.ExecContext(ctx, `INSERT INTO update_announcements(version,announced_at) VALUES(?,?) ON CONFLICT(version) DO NOTHING`, version, now())
	if err != nil {
		return false, err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	return changed == 1, nil
}
