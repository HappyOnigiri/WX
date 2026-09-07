package gitx

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// countingWaiter は手放しと取り直しの回数を数える LockWaiter である。
type countingWaiter struct {
	mu        sync.Mutex
	released  int
	acquired  int
	refuse    bool
	releasedC chan struct{}
}

func newCountingWaiter() *countingWaiter {
	return &countingWaiter{releasedC: make(chan struct{}, 1)}
}

func (w *countingWaiter) Release() {
	w.mu.Lock()
	w.released++
	w.mu.Unlock()
	select {
	case w.releasedC <- struct{}{}:
	default:
	}
}

func (w *countingWaiter) Acquire(context.Context) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.acquired++
	return !w.refuse
}

func (w *countingWaiter) counts() (released, acquired int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.released, w.acquired
}

func TestKeyedLocksExcludesAndReleasesOnce(t *testing.T) {
	t.Parallel()
	var locks KeyedLocks
	ctx := context.Background()
	_, release, err := locks.Acquire(ctx, "key")
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	blocked, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	if _, _, err := locks.Acquire(blocked, "key"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("second acquire of a held key err=%v", err)
	}
	if _, _, err := locks.Acquire(ctx, "other"); err != nil {
		t.Fatalf("acquire of an independent key: %v", err)
	}
	release()
	release() // 二重解放は次の取得者の排他を壊さない。
	held, releaseAgain, err := locks.Acquire(ctx, "key")
	if err != nil {
		t.Fatalf("acquire after release: %v", err)
	}
	defer releaseAgain()
	if !holdsLock(held, "key") {
		t.Fatal("acquired context does not record the held key")
	}
	if holdsLock(ctx, "key") {
		t.Fatal("acquire recorded the held key on the caller context")
	}
}

func TestKeyedLocksTreatsAHeldKeyAsReentrant(t *testing.T) {
	t.Parallel()
	var locks KeyedLocks
	held, release, err := locks.Acquire(context.Background(), "key")
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer release()
	waiter := newCountingWaiter()
	inner := WithLockWaiter(held, waiter)
	done := make(chan error, 1)
	go func() {
		done <- locks.With(inner, "key", func(context.Context) error { return nil })
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("reentrant acquire: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("reentrant acquire waited for the lock it already holds")
	}
	if released, _ := waiter.counts(); released != 0 {
		t.Fatalf("reentrant acquire released the waiter %d times", released)
	}
}

func TestKeyedLocksReleasesTheWaiterWhileWaiting(t *testing.T) {
	t.Parallel()
	var locks KeyedLocks
	_, release, err := locks.Acquire(context.Background(), "key")
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	waiter := newCountingWaiter()
	ctx := WithLockWaiter(context.Background(), waiter)
	done := make(chan error, 1)
	go func() {
		done <- locks.With(ctx, "key", func(context.Context) error { return nil })
	}()
	select {
	case <-waiter.releasedC:
	case <-time.After(2 * time.Second):
		t.Fatal("waiting acquire kept the waiter")
	}
	release()
	if err := <-done; err != nil {
		t.Fatalf("acquire after the holder released: %v", err)
	}
	released, acquired := waiter.counts()
	if released != 1 || acquired != 1 {
		t.Fatalf("waiter released=%d acquired=%d, want 1 and 1", released, acquired)
	}
}

func TestKeyedLocksSkipsTheCallbackWhenTheRequestEnds(t *testing.T) {
	t.Parallel()
	var locks KeyedLocks
	canceledCtx, cancel := context.WithCancel(context.Background())
	cancel()
	called := false
	if err := locks.With(canceledCtx, "key", func(context.Context) error {
		called = true
		return nil
	}); !errors.Is(err, context.Canceled) {
		t.Fatalf("acquire with an ended context err=%v", err)
	}
	if called {
		t.Fatal("an ended request ran the callback")
	}
	// 待機中に終わった要求も callback を実行せず、lock を握ったままにしない。
	_, release, err := locks.Acquire(context.Background(), "key")
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	waiting, cancelWaiting := context.WithCancel(context.Background())
	waitErr := make(chan error, 1)
	go func() {
		waitErr <- locks.With(waiting, "key", func(context.Context) error {
			called = true
			return nil
		})
	}()
	time.Sleep(20 * time.Millisecond)
	cancelWaiting()
	if err := <-waitErr; !errors.Is(err, context.Canceled) {
		t.Fatalf("acquire canceled while waiting err=%v", err)
	}
	if called {
		t.Fatal("a canceled wait ran the callback")
	}
	release()
	if _, releaseAgain, err := locks.Acquire(context.Background(), "key"); err != nil {
		t.Fatalf("acquire after a canceled wait: %v", err)
	} else {
		releaseAgain()
	}
}

// TestKeyedLocksReleasesTheLockWhenTheWaiterCannotReturn は、資源を取り直せない要求が lock を持ち逃げしないことを確認する。
func TestKeyedLocksReleasesTheLockWhenTheWaiterCannotReturn(t *testing.T) {
	t.Parallel()
	var locks KeyedLocks
	_, release, err := locks.Acquire(context.Background(), "key")
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	waiter := newCountingWaiter()
	waiter.refuse = true
	ctx := WithLockWaiter(context.Background(), waiter)
	done := make(chan error, 1)
	go func() {
		_, _, acquireErr := locks.Acquire(ctx, "key")
		done <- acquireErr
	}()
	select {
	case <-waiter.releasedC:
	case <-time.After(2 * time.Second):
		t.Fatal("waiting acquire kept the waiter")
	}
	release()
	if err := <-done; err == nil {
		t.Fatal("acquire succeeded although the waiter could not return")
	}
	acquired, releaseAgain, err := locks.Acquire(context.Background(), "key")
	if err != nil {
		t.Fatalf("acquire after a refused waiter: %v", err)
	}
	defer releaseAgain()
	if !holdsLock(acquired, "key") {
		t.Fatal("acquired context does not record the held key")
	}
}

func TestWithLockWaiterIgnoresAMissingWaiter(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	if WithLockWaiter(ctx, nil) != ctx {
		t.Fatal("registering no waiter changed the context")
	}
	if lockWaiterFrom(ctx) != nil {
		t.Fatal("a context without a waiter returned one")
	}
}

func TestNormalizeLockKeyCleansPaths(t *testing.T) {
	t.Parallel()
	if got := normalizeLockKey("/repo/.git/"); got != filepath.Clean("/repo/.git") {
		t.Fatalf("normalizeLockKey=%q", got)
	}
	if normalizeLockKey("/repo/worktrees/../.git") != normalizeLockKey("/repo/.git") {
		t.Fatal("equivalent paths produced different lock keys")
	}
}

func TestRunnerCommonDirLockSharesOneKeyPerCommonDirectory(t *testing.T) {
	t.Parallel()
	runner := &Runner{}
	ctx := context.Background()
	held, release, err := runner.AcquireCommonDirLock(ctx, "/repo/.git/")
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	blocked, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	if err := runner.WithCommonDirLock(blocked, "/repo/.git", func(context.Context) error { return nil }); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("normalized key did not exclude: %v", err)
	}
	if err := runner.WithCommonDirLock(held, "/repo/.git", func(context.Context) error { return nil }); err != nil {
		t.Fatalf("reentrant common-directory lock: %v", err)
	}
	release()
	if err := runner.WithCommonDirLock(ctx, "/repo/.git", func(context.Context) error { return nil }); err != nil {
		t.Fatalf("acquire after release: %v", err)
	}
}
