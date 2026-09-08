package state

import (
	"context"
	"unicode/utf8"
)

// maxFailureMessage は job row に残す失敗理由の上限である。
// 原因の 1 行を残すためのもので、command 出力そのものは error_detail_path のログが持つ。
const maxFailureMessage = 4 << 10

// truncateFailureMessage は失敗理由を上限まで詰め、切り捨てたことを末尾で示す。
// 切る位置は rune 境界へ戻す。理由は診断の原因文と `--json` にそのまま載るため、壊れた UTF-8 を残さない。
func truncateFailureMessage(message string) string {
	if len(message) <= maxFailureMessage {
		return message
	}
	end := maxFailureMessage
	for end > 0 && !utf8.RuneStart(message[end]) {
		end--
	}
	return message[:end] + " ... (truncated)"
}

// RecoveryFailure は保存・復元の失敗のうち、後続の実行でも解消していないものを表す。
// Kind は job の種別で、SessionState・SlotState は照合に使った現在の状態である。
type RecoveryFailure struct {
	JobID          string `json:"job_id"`
	Kind           string `json:"kind"`
	SessionID      string `json:"session_id,omitempty"`
	SlotID         string `json:"slot_id,omitempty"`
	SlotPath       string `json:"slot_path,omitempty"`
	FailureCode    string `json:"failure_code,omitempty"`
	FailureMessage string `json:"failure_message,omitempty"`
	DetailPath     string `json:"detail_path,omitempty"`
	FinishedAt     string `json:"finished_at,omitempty"`
	SessionState   string `json:"session_state,omitempty"`
	SlotState      string `json:"slot_state,omitempty"`
}

// UnresolvedRecoveryFailures は失敗した SNAPSHOT・RESTORE のうち、現在の状態から見て未解消のものを返す。
// 同じ session で後から成功・実行待ちの同種 job があるもの、session が ARCHIVED・EXPIRED まで進んだものは解消済みとして除く。
// 失敗 job 自体の保持期間は retention.failed_job の GC が決めるため、ここでは期間で絞らない。
func (s *Store) UnresolvedRecoveryFailures(ctx context.Context) ([]RecoveryFailure, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT j.id,j.kind,COALESCE(j.session_id,''),COALESCE(j.slot_id,''),
		   COALESCE(rt.path||'/'||sl.rel_path,''),COALESCE(j.error_code,''),COALESCE(j.error_message,''),
		   COALESCE(j.error_detail_path,''),COALESCE(j.finished_at,''),COALESCE(se.state,''),COALESCE(sl.state,'')
		FROM jobs j
		LEFT JOIN sessions se ON se.id=j.session_id
		LEFT JOIN slots sl ON sl.id=j.slot_id
		LEFT JOIN roots rt ON rt.id=sl.root_id
		WHERE j.state='FAILED' AND j.kind IN ('SNAPSHOT','RESTORE')
		  AND (se.state IS NULL OR se.state NOT IN ('ARCHIVED','EXPIRED'))
		  AND NOT EXISTS (
			SELECT 1 FROM jobs later
			WHERE later.id<>j.id AND later.kind=j.kind AND later.session_id=j.session_id
			  AND (later.state IN ('PENDING','RUNNING','SUCCEEDED'))
		  )
		ORDER BY j.finished_at,j.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []RecoveryFailure{}
	for rows.Next() {
		var item RecoveryFailure
		if err := rows.Scan(&item.JobID, &item.Kind, &item.SessionID, &item.SlotID, &item.SlotPath, &item.FailureCode,
			&item.FailureMessage, &item.DetailPath, &item.FinishedAt, &item.SessionState, &item.SlotState); err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}
