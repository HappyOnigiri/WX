package state

import (
	"context"
	"testing"
)

// TestUpdateCheckStartsEmptyAndRecordsBothOutcomes は singleton 行の初期化と、
// 失敗した確認でも最終確認時刻が進むことを守る。進まないと保守の一巡ごとに GitHub を叩く。
func TestUpdateCheckStartsEmptyAndRecordsBothOutcomes(t *testing.T) {
	t.Parallel()
	store := openTestStore(t)
	ctx := context.Background()
	initial, err := store.UpdateCheck(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !initial.CheckedAt.IsZero() || initial.LatestVersion != "" || initial.AnnouncedVersion != "" {
		t.Fatalf("initial update check=%+v, want an empty singleton row", initial)
	}
	if err := store.RecordUpdateCheck(ctx, "v1.2.3", "https://example.test/v1.2.3", ""); err != nil {
		t.Fatal(err)
	}
	recorded, err := store.UpdateCheck(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if recorded.LatestVersion != "v1.2.3" || recorded.ReleaseURL != "https://example.test/v1.2.3" || recorded.LastError != "" {
		t.Fatalf("recorded=%+v, want the successful check", recorded)
	}
	if recorded.CheckedAt.IsZero() {
		t.Fatal("a successful check did not advance checked_at")
	}
	if err := store.RecordUpdateCheck(ctx, "", "", "dial tcp: no route to host"); err != nil {
		t.Fatal(err)
	}
	failed, err := store.UpdateCheck(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if failed.LastError == "" || failed.CheckedAt.Before(recorded.CheckedAt) {
		t.Fatalf("failed check=%+v, want the error recorded and checked_at kept moving", failed)
	}
	if failed.LatestVersion != "v1.2.3" {
		t.Fatalf("failed check dropped the last known version: %+v", failed)
	}
}

// TestClaimUpdateAnnouncementHandsTheVersionToOneCallerOnly は、案内権が版ごとに 1 回だけ渡ることを守る。
// 2 回目も true を返すと、同じ版の案内が起動のたびに出る。
func TestClaimUpdateAnnouncementHandsTheVersionToOneCallerOnly(t *testing.T) {
	t.Parallel()
	store := openTestStore(t)
	ctx := context.Background()
	first, err := store.ClaimUpdateAnnouncement(ctx, "v1.2.3")
	if err != nil {
		t.Fatal(err)
	}
	if !first {
		t.Fatal("the first claim for a version was refused")
	}
	second, err := store.ClaimUpdateAnnouncement(ctx, "v1.2.3")
	if err != nil {
		t.Fatal(err)
	}
	if second {
		t.Fatal("the same version was claimed twice")
	}
	next, err := store.ClaimUpdateAnnouncement(ctx, "v1.3.0")
	if err != nil {
		t.Fatal(err)
	}
	if !next {
		t.Fatal("a newer version could not be claimed after an earlier one")
	}
	empty, err := store.ClaimUpdateAnnouncement(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if empty {
		t.Fatal("an empty version was claimed")
	}
}
