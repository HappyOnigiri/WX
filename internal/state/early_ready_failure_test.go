package state

import (
	"context"
	"testing"
)

// early ready の後に準備が失敗した slot は、失敗を記録したまま LEASED に留まり、
// 返却から snapshot までの経路をそのまま通る。PREPARING や QUARANTINED のまま終えると
// 返却が DRAINING へ進まず、エージェントの作業が snapshot に届かない。
func TestEarlyReadyPrepareFailureStillReachesSnapshotted(t *testing.T) {
	t.Parallel()
	store := openTestStore(t)
	seedWorkspace(t, store)
	createSessionSlot(t, store, "kept", "STARTING", "PREPARING")
	ctx := context.Background()
	if err := store.BeginStagedPreparation(ctx, "kept"); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkEarlyReady(ctx, "kept"); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordEarlyReadyPrepareFailure(ctx, "kept", "PREPARE_FAILED:abc", "/detail/abc.log", "post-checkout"); err != nil {
		t.Fatal(err)
	}
	if _, _, _, _, err := store.FinishPreparationWithReplenishment(ctx, "kept"); err != nil {
		t.Fatal(err)
	}
	slot, err := store.Slot(ctx, "kept")
	if err != nil || slot.State != "LEASED" || slot.FailureCode != "PREPARE_FAILED:abc" || slot.FailurePhase != "post-checkout" {
		t.Fatalf("slot=%+v: %v", slot, err)
	}
	if _, _, err := store.Release(ctx, "kept", "workspace", "kept"); err != nil {
		t.Fatal(err)
	}
	if slot, err = store.Slot(ctx, "kept"); err != nil || slot.State != "DRAINING" {
		t.Fatalf("slot=%+v: %v", slot, err)
	}
	if err := store.BeginSnapshot(ctx, "kept", "kept"); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkArchived(ctx, "kept", "kept", now()); err != nil {
		t.Fatal(err)
	}
	if slot, err = store.Slot(ctx, "kept"); err != nil || slot.State != "SNAPSHOTTED" {
		t.Fatalf("slot=%+v: %v", slot, err)
	}
}

// 失敗の記録は early ready 済みで owner session が生きている slot に限る。
// owner session を持たない standby の補充・更新は、これまでどおり隔離へ倒す。
func TestEarlyReadyPrepareFailureRefusesSlotsWithoutALiveOwner(t *testing.T) {
	t.Parallel()
	store := openTestStore(t)
	seedWorkspace(t, store)
	ctx := context.Background()
	if _, err := store.CreateStandby(ctx, Slot{ID: "standby", WorkspaceID: "workspace", Generation: 1, RootID: testRootID, RelPath: "workspace/standby", State: "PREPARING"}, nil); err != nil {
		t.Fatal(err)
	}
	if err := store.BeginStagedPreparation(ctx, "standby"); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkEarlyReady(ctx, "standby"); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordEarlyReadyPrepareFailure(ctx, "standby", "PREPARE_FAILED:abc", "", ""); err == nil {
		t.Fatal("recorded a continuing failure on a slot without an owner session")
	}
	// early ready の前に落ちた貸出も、worktree がどこまで出来ているか分からないので継続させない。
	createSessionSlot(t, store, "early-pending", "STARTING", "PREPARING")
	if err := store.BeginStagedPreparation(ctx, "early-pending"); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordEarlyReadyPrepareFailure(ctx, "early-pending", "PREPARE_FAILED:abc", "", ""); err == nil {
		t.Fatal("recorded a continuing failure before early readiness")
	}
}

// 案内は session ごとに 1 回だけ渡す。daemon の再起動で再送しないよう、消費済みは session 行に残す。
func TestPrepareFailureNoticeIsClaimedOnce(t *testing.T) {
	t.Parallel()
	store := openTestStore(t)
	seedWorkspace(t, store)
	createSessionSlot(t, store, "kept", "STARTING", "PREPARING")
	ctx := context.Background()
	if _, ok, err := store.ClaimPrepareFailureNotice(ctx, "kept"); ok || err != nil {
		t.Fatalf("claimed a notice without a failure: ok=%t err=%v", ok, err)
	}
	if err := store.BeginStagedPreparation(ctx, "kept"); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkEarlyReady(ctx, "kept"); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordEarlyReadyPrepareFailure(ctx, "kept", "PREPARE_FAILED:abc", "/detail/abc.log", "post-checkout"); err != nil {
		t.Fatal(err)
	}
	notice, ok, err := store.ClaimPrepareFailureNotice(ctx, "kept")
	if err != nil || !ok || notice.Code != "PREPARE_FAILED:abc" || notice.Phase != "post-checkout" || notice.DetailPath != "/detail/abc.log" {
		t.Fatalf("notice=%+v ok=%t err=%v", notice, ok, err)
	}
	if _, ok, err := store.ClaimPrepareFailureNotice(ctx, "kept"); ok || err != nil {
		t.Fatalf("claimed the same notice twice: ok=%t err=%v", ok, err)
	}
}

// 返却が準備失敗より先に着いた early ready の slot も、隔離せず失敗を記録して DRAINING へ進む。
// owner session を STARTING/ACTIVE に限ると RELEASING で CAS が落ち、返却済みの作業が snapshot に届かない。
func TestEarlyReadyPrepareFailureSurvivesReleaseBeforeFailure(t *testing.T) {
	t.Parallel()
	store := openTestStore(t)
	seedWorkspace(t, store)
	createSessionSlot(t, store, "released", "STARTING", "PREPARING")
	ctx := context.Background()
	if err := store.BeginStagedPreparation(ctx, "released"); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkEarlyReady(ctx, "released"); err != nil {
		t.Fatal(err)
	}
	// PREPARING の slot を返却すると session だけが RELEASING へ進み、slot は PREPARING に残る。
	if _, _, err := store.Release(ctx, "released", "workspace", "released"); err != nil {
		t.Fatal(err)
	}
	session, err := store.SessionByID(ctx, "released")
	if err != nil || session.State != "RELEASING" {
		t.Fatalf("session=%+v: %v", session, err)
	}
	slot, err := store.Slot(ctx, "released")
	if err != nil || slot.State != "PREPARING" {
		t.Fatalf("slot=%+v: %v", slot, err)
	}
	if err := store.RecordEarlyReadyPrepareFailure(ctx, "released", "PREPARE_FAILED:abc", "/detail/abc.log", "post-checkout"); err != nil {
		t.Fatalf("failure preservation refused a released owner: %v", err)
	}
	job, scheduled, _, _, err := store.FinishPreparationWithReplenishment(ctx, "released")
	if err != nil || !scheduled || job.Kind != "SNAPSHOT" {
		t.Fatalf("job=%+v scheduled=%v: %v", job, scheduled, err)
	}
	if slot, err = store.Slot(ctx, "released"); err != nil || slot.State != "DRAINING" || slot.FailureCode != "PREPARE_FAILED:abc" {
		t.Fatalf("slot=%+v: %v", slot, err)
	}
}
