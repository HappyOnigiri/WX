package state

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/HappyOnigiri/WX/internal/domain"
)

// Root は storage.worktree_root の一世代である。設定 root が変わっても既存 slot は移動せず、以前の row を active=0 で残して解決を維持する。
// 新しい slot だけを active row の下に作る。
type Root struct {
	ID, Path, Identity string
	Active             bool
}

// EnsureActiveRoot は path を active worktree root generation として登録し durable ID を返す。既登録 path は同じ ID を保ち、他の row は retired にする。
// inode identity の不一致は durable な参照がない場合だけ許可する。ユーザーが root を削除し wx が再作成しても state を取り残さない場合である。
// live slot/snapshot がある不一致は、記録済み location が wx の作成していない directory を指すため拒否する。
func (s *Store) EnsureActiveRoot(ctx context.Context, path, identity string) (string, error) {
	if path == "" || identity == "" {
		return "", errors.New("worktree root path and identity are required")
	}
	s.writer.Lock()
	defer s.writer.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer func() { _ = tx.Rollback() }()
	t := now()
	var id, storedIdentity string
	err = tx.QueryRowContext(ctx, `SELECT id,identity FROM roots WHERE path=?`, path).Scan(&id, &storedIdentity)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		id, err = newUnusedShortID(ctx, tx, "roots")
		if err != nil {
			return "", err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO roots(id,path,identity,active,created_at) VALUES(?,?,?,1,?)`, id, path, identity, t); err != nil {
			return "", err
		}
	case err != nil:
		return "", err
	case storedIdentity != identity:
		upgraded, err := upgradeLegacyIdentities(ctx, tx, id, storedIdentity, identity)
		if err != nil {
			return "", err
		}
		if !upgraded {
			var referencing int
			if err := tx.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM slots WHERE root_id=?)+(SELECT count(*) FROM workspace_snapshots WHERE root_id=?)`, id, id).Scan(&referencing); err != nil {
				return "", err
			}
			if referencing > 0 {
				return "", fmt.Errorf("%w: worktree root %s inode changed (recorded %s, found %s) while %d durable rows still reference it", ErrOwnership, path, storedIdentity, identity, referencing)
			}
		}
		if _, err := tx.ExecContext(ctx, `UPDATE roots SET identity=?,active=1,retired_at=NULL WHERE id=?`, identity, id); err != nil {
			return "", err
		}
	default:
		if _, err := tx.ExecContext(ctx, `UPDATE roots SET active=1,retired_at=NULL WHERE id=?`, id); err != nil {
			return "", err
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE roots SET active=0,retired_at=COALESCE(retired_at,?) WHERE id<>? AND active=1`, t, id); err != nil {
		return "", err
	}
	return id, tx.Commit()
}

// upgradeLegacyIdentities は device 番号を含む旧形式で記録された identity を、記録済み inode を保ったまま現行形式へ書き換える。
// macOS の device 番号は再起動で変わり、旧形式のままでは root 配下の全 row が一度に一致しなくなるため、inode が一致する間だけ形式を移行する。
// 対象は登録中の root generation とその配下に限る。他の generation は volume が同じとは限らず、開けたときの identity でしか判定できない。
func upgradeLegacyIdentities(ctx context.Context, tx *sql.Tx, rootID, stored, current string) (bool, error) {
	storedInode, storedVolume, storedOK := domain.IdentityFields(stored)
	currentInode, currentVolume, currentOK := domain.IdentityFields(current)
	if !storedOK || !currentOK || storedVolume != "" || currentVolume == "" || storedInode != currentInode {
		return false, nil
	}
	slots, err := legacyIdentityRows(ctx, tx, `SELECT id,'',dir_identity FROM slots WHERE root_id=? AND dir_identity IS NOT NULL`, rootID)
	if err != nil {
		return false, err
	}
	repositories, err := legacyIdentityRows(ctx, tx, `SELECT sr.slot_id,sr.repository_id,sr.dir_identity FROM slot_repositories sr JOIN slots sl ON sl.id=sr.slot_id WHERE sl.root_id=? AND sr.dir_identity IS NOT NULL`, rootID)
	if err != nil {
		return false, err
	}
	for _, row := range slots {
		if _, err := tx.ExecContext(ctx, `UPDATE slots SET dir_identity=? WHERE id=?`, domain.FormatIdentity(row.inode, currentVolume), row.slotID); err != nil {
			return false, err
		}
	}
	for _, row := range repositories {
		if _, err := tx.ExecContext(ctx, `UPDATE slot_repositories SET dir_identity=? WHERE slot_id=? AND repository_id=?`, domain.FormatIdentity(row.inode, currentVolume), row.slotID, row.repositoryID); err != nil {
			return false, err
		}
	}
	return true, nil
}

type legacyIdentityRow struct{ slotID, repositoryID, inode string }

// legacyIdentityRows は旧形式で記録された identity の行だけを返す。現行形式の行は書き換えずに残す。
func legacyIdentityRows(ctx context.Context, tx *sql.Tx, query, rootID string) ([]legacyIdentityRow, error) {
	rows, err := tx.QueryContext(ctx, query, rootID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []legacyIdentityRow
	for rows.Next() {
		var row legacyIdentityRow
		var identity string
		if err := rows.Scan(&row.slotID, &row.repositoryID, &identity); err != nil {
			return nil, err
		}
		inode, volume, ok := domain.IdentityFields(identity)
		if !ok || volume != "" {
			continue
		}
		row.inode = inode
		out = append(out, row)
	}
	return out, rows.Err()
}

// Roots は登録済み root generation を全て返し、daemon が durable slot の依存する descriptor を再 pin できるようにする。
func (s *Store) Roots(ctx context.Context) ([]Root, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,path,identity,active FROM roots ORDER BY created_at,id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Root
	for rows.Next() {
		var root Root
		if err := rows.Scan(&root.ID, &root.Path, &root.Identity, &root.Active); err != nil {
			return nil, err
		}
		out = append(out, root)
	}
	return out, rows.Err()
}

// PruneRoots は durable な参照がなくなった retired root generation を削除する。
// directory 自体は disk に残す。wx は configured root を削除せず、参照可能にしていた row だけを削除する。
func (s *Store) PruneRoots(ctx context.Context) error {
	s.writer.Lock()
	defer s.writer.Unlock()
	_, err := s.db.ExecContext(ctx, `DELETE FROM roots WHERE active=0 AND NOT EXISTS (SELECT 1 FROM slots WHERE slots.root_id=roots.id) AND NOT EXISTS (SELECT 1 FROM workspace_snapshots WHERE workspace_snapshots.root_id=roots.id)`)
	return err
}
