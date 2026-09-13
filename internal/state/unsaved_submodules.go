package state

import (
	"context"
	"strings"
)

// UnsavedSubmodule は snapshot に入らなかった submodule 作業の記録 1 件である。
// Reasons は検出側が固定順で組み立てた理由コードを `,` で連ねた値で、DB 側は解釈しない。
type UnsavedSubmodule struct {
	RepositoryID string `json:"repository_id"`
	Path         string `json:"path"`
	Reasons      string `json:"reasons"`
	DetectedAt   string `json:"detected_at,omitempty"`
}

// ProtectedSlot は未保全の submodule 作業を持つために自動回収から外れた slot 1 件である。
type ProtectedSlot struct {
	SlotID     string
	Path       string
	Submodules []UnsavedSubmodule
}

// ReplaceUnsavedSubmodules は 1 repository 分の検出結果を丸ごと置き換える。
// EnsureRecoveryJobs による snapshot のやり直しで、解消済みの古い行が保護として残らないようにするためである。
func (s *Store) ReplaceUnsavedSubmodules(ctx context.Context, slotID, repositoryID string, entries []UnsavedSubmodule) error {
	s.writer.Lock()
	defer s.writer.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM unsaved_submodules WHERE slot_id=? AND repository_id=?`, slotID, repositoryID); err != nil {
		return err
	}
	detectedAt := now()
	for _, entry := range entries {
		if _, err := tx.ExecContext(ctx, `INSERT INTO unsaved_submodules(slot_id,repository_id,path,reasons,detected_at) VALUES(?,?,?,?,?)`,
			slotID, repositoryID, entry.Path, entry.Reasons, detectedAt); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ProtectedSlots は保護中の slot を path 順に返す。`wx doctor` が回収されない理由を説明するために使う。
func (s *Store) ProtectedSlots(ctx context.Context) ([]ProtectedSlot, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT us.slot_id,rt.path||'/'||sl.rel_path,us.repository_id,us.path,us.reasons,us.detected_at
		FROM unsaved_submodules us JOIN slots sl ON sl.id=us.slot_id JOIN roots rt ON rt.id=sl.root_id
		ORDER BY rt.path,sl.rel_path,us.repository_id,us.path`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ProtectedSlot
	index := map[string]int{}
	for rows.Next() {
		var slotID, slotPath string
		var entry UnsavedSubmodule
		if err := rows.Scan(&slotID, &slotPath, &entry.RepositoryID, &entry.Path, &entry.Reasons, &entry.DetectedAt); err != nil {
			return nil, err
		}
		position, seen := index[slotID]
		if !seen {
			position = len(out)
			index[slotID] = position
			out = append(out, ProtectedSlot{SlotID: slotID, Path: slotPath})
		}
		out[position].Submodules = append(out[position].Submodules, entry)
	}
	return out, rows.Err()
}

// unsavedSubmodulePaths は slot ごとの submodule path を返す。判定不能だけを記録した行は path を持たない。
func (s *Store) unsavedSubmodulePaths(ctx context.Context) (map[string][]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT slot_id,path FROM unsaved_submodules ORDER BY slot_id,repository_id,path`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]string{}
	for rows.Next() {
		var slotID, path string
		if err := rows.Scan(&slotID, &path); err != nil {
			return nil, err
		}
		out[slotID] = append(out[slotID], path)
	}
	return out, rows.Err()
}

// UnsavedSubmoduleReasons は記録された理由コードを分解する。表示側が `,` の連結表現に依存しないようにする。
func UnsavedSubmoduleReasons(reasons string) []string {
	if reasons == "" {
		return nil
	}
	return strings.Split(reasons, ",")
}
