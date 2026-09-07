package workspace

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/HappyOnigiri/WX/internal/gitx"
)

func TestLockSlotExcludesTheSameSlotAndAllowsOthers(t *testing.T) {
	t.Parallel()
	locks := &gitx.KeyedLocks{}
	first := &Preparer{RootID: testRootID, SlotRelPath: "workspace/slot", SlotLocks: locks}
	same := &Preparer{RootID: testRootID, SlotRelPath: "workspace/slot/", SlotLocks: locks}
	other := &Preparer{RootID: testRootID, SlotRelPath: "workspace/other", SlotLocks: locks}
	held, release, err := first.LockSlot(context.Background())
	if err != nil {
		t.Fatalf("lock slot: %v", err)
	}
	blocked, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, _, err := same.LockSlot(blocked); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("the same slot was not excluded: %v", err)
	}
	if _, releaseOther, err := other.LockSlot(context.Background()); err != nil {
		t.Fatalf("a different slot waited: %v", err)
	} else {
		releaseOther()
	}
	// 取得済みの ctx を渡す内側の経路は待たずに通る。
	if _, releaseInner, err := same.LockSlot(held); err != nil {
		t.Fatalf("reentrant lock of a held slot: %v", err)
	} else {
		releaseInner()
	}
	release()
	if _, releaseAfter, err := same.LockSlot(context.Background()); err != nil {
		t.Fatalf("lock slot after release: %v", err)
	} else {
		releaseAfter()
	}
}

// TestLockSlotWithoutASharedTableOrLocation は、slot を特定できない Preparer が排他せずに進むことを確認する。
// 単一操作しか行わない経路では common-directory lock だけが働く。
func TestLockSlotWithoutASharedTableOrLocation(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name     string
		preparer *Preparer
	}{
		{name: "no shared table", preparer: &Preparer{RootID: testRootID, SlotRelPath: "workspace/slot"}},
		{name: "no root generation", preparer: &Preparer{SlotRelPath: "workspace/slot", SlotLocks: &gitx.KeyedLocks{}}},
		{name: "no slot path", preparer: &Preparer{RootID: testRootID, SlotLocks: &gitx.KeyedLocks{}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			held, release, err := test.preparer.LockSlot(ctx)
			if err != nil {
				t.Fatalf("lock slot: %v", err)
			}
			defer release()
			if held != ctx {
				t.Fatal("an unlockable slot changed the context")
			}
			second, releaseSecond, err := test.preparer.LockSlot(ctx)
			if err != nil {
				t.Fatalf("second lock slot: %v", err)
			}
			releaseSecond()
			if second != ctx {
				t.Fatal("an unlockable slot changed the context")
			}
		})
	}
}
