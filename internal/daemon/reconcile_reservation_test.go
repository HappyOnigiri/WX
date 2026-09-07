package daemon

import (
	"testing"

	"github.com/HappyOnigiri/WX/internal/state"
)

// 進行中の予約はreconcileの回収対象から外す。隔離するとConfirmSlotCreationのCASが落ち、正常な起動が失敗するためである。
// 自プロセスの進行中でない予約は、従来どおりクラッシュの残骸として隔離する。
func TestReconcileArtifactsKeepsInFlightReservationAndReclaimsAbandonedOne(t *testing.T) {
	t.Parallel()
	ctx, manager, store, _, _, _ := managerCoverageFixture(t)
	inFlight := testSlotRow(t, manager, "", "in-flight", 1, "ALLOCATING")
	if err := store.ReserveSlot(ctx, state.Slot{ID: inFlight.ID, Generation: inFlight.Generation, RootID: inFlight.RootID, RelPath: inFlight.RelPath, OwnerSessionID: inFlight.ID}); err != nil {
		t.Fatal(err)
	}
	endReservation := manager.beginReservation(inFlight.ID)

	manager.reconcileArtifacts(ctx)

	if slot, err := store.Slot(ctx, inFlight.ID); err != nil || slot.State != "ALLOCATING" {
		t.Fatalf("in-flight reservation was reclaimed: slot=%+v err=%v", slot, err)
	}
	if err := store.ConfirmSlotCreation(ctx, inFlight.ID, "identity"); err != nil {
		t.Fatalf("confirm creation after reconcile: %v", err)
	}
	endReservation()

	abandoned := testSlotRow(t, manager, "", "abandoned", 1, "ALLOCATING")
	if err := store.ReserveSlot(ctx, state.Slot{ID: abandoned.ID, Generation: abandoned.Generation, RootID: abandoned.RootID, RelPath: abandoned.RelPath, OwnerSessionID: abandoned.ID}); err != nil {
		t.Fatal(err)
	}

	manager.reconcileArtifacts(ctx)

	slot, err := store.Slot(ctx, abandoned.ID)
	if err != nil || slot.State != "QUARANTINED" {
		t.Fatalf("abandoned reservation was not reclaimed: slot=%+v err=%v", slot, err)
	}
	if slot.FailureCode != "ALLOCATION_INTERRUPTED" {
		t.Fatalf("unexpected failure code: %s", slot.FailureCode)
	}
}
