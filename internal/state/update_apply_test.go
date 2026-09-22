package state

import (
	"context"
	"testing"
	"time"
)

// TestUpdateApplyIsClaimedOncePerVersion は、保守の一巡ごとに問い合わせても同じ版の適用が
// 1 回に留まること、版が変われば別に数えることを守る。
func TestUpdateApplyIsClaimedOncePerVersion(t *testing.T) {
	t.Parallel()
	store := openTestStore(t)
	ctx := context.Background()
	now := time.Now()
	attempt, err := store.ClaimUpdateApply(ctx, "v1.1.0", now)
	if err != nil {
		t.Fatal(err)
	}
	if attempt != 1 {
		t.Fatalf("first claim=%d, want 1", attempt)
	}
	again, err := store.ClaimUpdateApply(ctx, "v1.1.0", now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if again != 0 {
		t.Fatalf("a second claim inside the retry window=%d, want 0", again)
	}
	other, err := store.ClaimUpdateApply(ctx, "v1.2.0", now)
	if err != nil {
		t.Fatal(err)
	}
	if other != 1 {
		t.Fatalf("claim for another version=%d, want 1", other)
	}
}

// TestUpdateApplyRetriesAfterTheWindowAndStopsAtTheLimit は、親が子の成否を観測できない分を
// 時間と回数で切る歯止めを守る。上限が効かないと、失敗し続ける版で起動を繰り返す。
func TestUpdateApplyRetriesAfterTheWindowAndStopsAtTheLimit(t *testing.T) {
	t.Parallel()
	store := openTestStore(t)
	ctx := context.Background()
	at := time.Now()
	for want := 1; want <= UpdateApplyMaxAttempts; want++ {
		attempt, err := store.ClaimUpdateApply(ctx, "v1.1.0", at)
		if err != nil {
			t.Fatal(err)
		}
		if attempt != want {
			t.Fatalf("attempt %d of the retry sequence=%d", want, attempt)
		}
		at = at.Add(UpdateApplyRetryInterval)
	}
	exhausted, err := store.ClaimUpdateApply(ctx, "v1.1.0", at)
	if err != nil {
		t.Fatal(err)
	}
	if exhausted != 0 {
		t.Fatalf("claim past the attempt limit=%d, want 0", exhausted)
	}
}

// TestUpdateApplyIgnoresAnEmptyVersion は、未確認で版が空のまま適用を始めないことを守る。
func TestUpdateApplyIgnoresAnEmptyVersion(t *testing.T) {
	t.Parallel()
	store := openTestStore(t)
	attempt, err := store.ClaimUpdateApply(context.Background(), "", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if attempt != 0 {
		t.Fatalf("claim for an empty version=%d, want 0", attempt)
	}
}
