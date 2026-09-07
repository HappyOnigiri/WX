package workspace

import (
	"context"
	"path/filepath"
)

// slotLockKey は slot への書き込みを排他する key を返す。
// slot の場所と同じく root 世代と root 相対 path で表し、作成前後で変わる inode は含めない。
// どちらかを持たない Preparer（単一操作しか行わない経路）では空を返し、排他しない。
func (p *Preparer) slotLockKey() string {
	if p.RootID == "" || p.SlotRelPath == "" {
		return ""
	}
	return p.RootID + "\x00" + filepath.Clean(p.SlotRelPath)
}

// LockSlot は同じ slot へ書く操作を直列化し、解放関数と取得済みを記録した ctx を返す。
// prepare が common-directory lock を手放す区間の排他はこの lock が保つので、最上位の operation で一度だけ取得し、内側の経路には返った ctx を渡す。
// SlotLocks を持たない Preparer では何もせず、common-directory lock だけが従来どおり働く。
func (p *Preparer) LockSlot(ctx context.Context) (context.Context, func(), error) {
	key := p.slotLockKey()
	if p.SlotLocks == nil || key == "" {
		return ctx, func() {}, nil
	}
	return p.SlotLocks.Acquire(ctx, key)
}
