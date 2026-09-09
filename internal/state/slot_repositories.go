package state

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
)

// SlotRepository は slot 内の directory 名で repository worktree を位置付ける。WorktreePath は Slot.Path と同様に派生する。
type SlotRepository struct {
	RepositoryID, DirName, DirIdentity, WorktreePath, State, RequestedRef, BaseOID, Fingerprint                    string
	CompatibilityFingerprint, UpdateRequestedRef, UpdateBaseOID, UpdateFingerprint, UpdateCompatibilityFingerprint string
}

// slotRepositoryColumns は slot-repository read 全てで共有する。SlotRepository.WorktreePath は保存せず、
// 結合した root generation と slot path から組み立てるため、設定 worktree root の変更後も同じ row を解決できる。
const slotRepositoryColumns = `sr.repository_id,sr.dir_name,COALESCE(sr.dir_identity,''),rt.path,sl.rel_path,sr.state,sr.requested_ref,sr.base_oid,sr.prepare_fingerprint,COALESCE(sr.compatibility_fingerprint,''),COALESCE(sr.update_requested_ref,''),COALESCE(sr.update_base_oid,''),COALESCE(sr.update_fingerprint,''),COALESCE(sr.update_compatibility_fingerprint,'')`

const slotRepositoryFrom = ` FROM slot_repositories sr JOIN slots sl ON sl.id=sr.slot_id JOIN roots rt ON rt.id=sl.root_id`

func scanSlotRepository(row rowScanner) (SlotRepository, error) {
	var x SlotRepository
	var rootPath, slotRel string
	if err := row.Scan(&x.RepositoryID, &x.DirName, &x.DirIdentity, &rootPath, &slotRel, &x.State, &x.RequestedRef, &x.BaseOID, &x.Fingerprint, &x.CompatibilityFingerprint, &x.UpdateRequestedRef, &x.UpdateBaseOID, &x.UpdateFingerprint, &x.UpdateCompatibilityFingerprint); err != nil {
		return SlotRepository{}, err
	}
	x.WorktreePath = filepath.Join(rootPath, slotRel, x.DirName)
	return x, nil
}

func (s *Store) SlotRepositories(ctx context.Context, slotID string) ([]SlotRepository, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+slotRepositoryColumns+slotRepositoryFrom+` WHERE sr.slot_id=? ORDER BY sr.repository_id`, slotID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SlotRepository
	for rows.Next() {
		x, err := scanSlotRepository(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

func (s *Store) AddRestoringRepositories(ctx context.Context, slotID string, repos []SlotRepository) error {
	s.writer.Lock()
	defer s.writer.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var slotState string
	if err := tx.QueryRowContext(ctx, `SELECT state FROM slots WHERE id=?`, slotID).Scan(&slotState); err != nil {
		return err
	}
	if slotState != "RESTORING" {
		return errors.New("resume slot is no longer RESTORING")
	}
	var existing int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM slot_repositories WHERE slot_id=?`, slotID).Scan(&existing); err != nil {
		return err
	}
	if existing != 0 {
		if existing == len(repos) {
			return nil
		}
		return errors.New("resume repository metadata is incomplete")
	}
	for _, repo := range repos {
		if _, err := tx.ExecContext(ctx, `INSERT INTO slot_repositories(slot_id,repository_id,dir_name,state,requested_ref,base_oid,prepare_fingerprint,compatibility_fingerprint) VALUES(?,?,?,?,?,?,?,?)`, slotID, repo.RepositoryID, repo.DirName, "RESTORING", repo.RequestedRef, repo.BaseOID, repo.Fingerprint, repo.CompatibilityFingerprint); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) SlotRepository(ctx context.Context, slotID, repositoryID string) (SlotRepository, error) {
	return scanSlotRepository(s.db.QueryRowContext(ctx, `SELECT `+slotRepositoryColumns+slotRepositoryFrom+` WHERE sr.slot_id=? AND sr.repository_id=?`, slotID, repositoryID))
}

// RecordSlotRepositoryIdentity は repository worktree directory 作成直後の inode identity を保存する。
// ownership validation はこれを比較し、identity を提示できる呼び出し元で record がなければ path 比較へ戻さずフェイルクローズする。
func (s *Store) RecordSlotRepositoryIdentity(ctx context.Context, slotID, repositoryID, identity string) error {
	if identity == "" {
		return errors.New("slot repository directory identity is required")
	}
	s.writer.Lock()
	defer s.writer.Unlock()
	res, err := s.db.ExecContext(ctx, `UPDATE slot_repositories SET dir_identity=? WHERE slot_id=? AND repository_id=?`, identity, slotID, repositoryID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return fmt.Errorf("slot repository %s/%s is not registered", slotID, repositoryID)
	}
	return nil
}

func (s *Store) SetSlotRepositoryState(ctx context.Context, slotID, repositoryID string, from []string, to string) error {
	s.writer.Lock()
	defer s.writer.Unlock()
	args := append([]any{to, slotID, repositoryID}, stringsToAny(from)...)
	res, err := s.db.ExecContext(ctx, `UPDATE slot_repositories SET state=? WHERE slot_id=? AND repository_id=? AND state IN (`+placeholders(len(from))+`)`, args...)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return fmt.Errorf("slot repository %s/%s state compare-and-swap failed", slotID, repositoryID)
	}
	return nil
}
