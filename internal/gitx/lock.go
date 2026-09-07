package gitx

import (
	"context"
	"path/filepath"
	"sync"
)

// LockWaiter はロック待ちの間だけ手放せる資源である。
// daemon の実行枠を渡すと、ロック待ちのジョブが枠を占有しなくなる。
type LockWaiter interface {
	// Release は資源を手放す。保持していない状態での呼び出しは無視する。
	Release()
	// Acquire は手放した資源を取り直す。context が終わったときだけ false を返す。
	Acquire(ctx context.Context) bool
}

type (
	lockWaiterKey struct{}
	heldLocksKey  struct{}
)

// WithLockWaiter は、この ctx を使うロック取得が待機前に手放す資源を登録する。
// 登録しない ctx では待機中も資源を保持したままになる。
func WithLockWaiter(ctx context.Context, waiter LockWaiter) context.Context {
	if waiter == nil {
		return ctx
	}
	return context.WithValue(ctx, lockWaiterKey{}, waiter)
}

func lockWaiterFrom(ctx context.Context) LockWaiter {
	waiter, _ := ctx.Value(lockWaiterKey{}).(LockWaiter)
	return waiter
}

// holdsLock は key を取得済みの経路かを返す。
func holdsLock(ctx context.Context, key string) bool {
	held, _ := ctx.Value(heldLocksKey{}).(map[string]bool)
	return held[key]
}

// withHeldLock は取得済みの key を ctx へ記録する。
// map は複製して置き換えるので、先に分岐した ctx の内容は変わらない。
func withHeldLock(ctx context.Context, key string) context.Context {
	previous, _ := ctx.Value(heldLocksKey{}).(map[string]bool)
	held := make(map[string]bool, len(previous)+1)
	for name := range previous {
		held[name] = true
	}
	held[key] = true
	return context.WithValue(ctx, heldLocksKey{}, held)
}

// normalizeLockKey は同じ対象を同じ key にするため path を正規化する。
// symlink は解決しない。解決には I/O が要り、失敗時に別 key へ落ちて排他が消えるためである。
func normalizeLockKey(path string) string { return filepath.Clean(path) }

// KeyedLocks は key ごとの排他を context 対応で行う。
// 取得済みの key は返した ctx に記録し、同じ経路からの再取得を自己 deadlock にしない。
type KeyedLocks struct{ gates sync.Map }

func (l *KeyedLocks) gate(key string) chan struct{} {
	if existing, ok := l.gates.Load(key); ok {
		return existing.(chan struct{})
	}
	value, _ := l.gates.LoadOrStore(key, make(chan struct{}, 1))
	return value.(chan struct{})
}

// Acquire は key の排他を取り、解放関数と取得済みを記録した ctx を返す。
// 空きがなければ LockWaiter を手放してから待ち、取得後に取り直す。
// context が終わっているか待機中に終わった場合は排他を持たずに error を返す。解放関数は一度だけ効く。
func (l *KeyedLocks) Acquire(ctx context.Context, key string) (context.Context, func(), error) {
	if err := ctx.Err(); err != nil {
		return ctx, nil, err
	}
	if holdsLock(ctx, key) {
		return ctx, func() {}, nil
	}
	gate := l.gate(key)
	select {
	case gate <- struct{}{}:
	default:
		waiter := lockWaiterFrom(ctx)
		if waiter != nil {
			waiter.Release()
		}
		select {
		case gate <- struct{}{}:
		case <-ctx.Done():
			return ctx, nil, ctx.Err()
		}
		if waiter != nil && !waiter.Acquire(ctx) {
			<-gate
			return ctx, nil, canceled(ctx)
		}
	}
	var once sync.Once
	return withHeldLock(ctx, key), func() { once.Do(func() { <-gate }) }, nil
}

// With は key の排他を保持したまま fn を呼ぶ。
// 取得できなかった要求では fn を呼ばない。
func (l *KeyedLocks) With(ctx context.Context, key string, fn func(context.Context) error) error {
	locked, release, err := l.Acquire(ctx, key)
	if err != nil {
		return err
	}
	defer release()
	return fn(locked)
}

func canceled(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return context.Canceled
}

// AcquireCommonDirLock は repository 共有の Git 管理情報を書き換える区間の排他を取る。
// 呼び出し側が区間を分けたい場合に使い、それ以外は WithCommonDirLock を使う。
func (r *Runner) AcquireCommonDirLock(ctx context.Context, common string) (context.Context, func(), error) {
	return r.locks.Acquire(ctx, normalizeLockKey(common))
}

// WithCommonDirLock は common directory の排他を保持したまま fn を呼ぶ。
// fn には取得済みを記録した ctx を渡すので、内側の同じ common への要求は待たずに通る。
func (r *Runner) WithCommonDirLock(ctx context.Context, common string, fn func(context.Context) error) error {
	return r.locks.With(ctx, normalizeLockKey(common), fn)
}
