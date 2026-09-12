package state

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestStagedReadinessCASAndPublicSlotShape(t *testing.T) {
	t.Parallel()
	store := openTestStore(t)
	seedWorkspace(t, store)
	createSessionSlot(t, store, "early", "STARTING", "PREPARING")
	ctx := context.Background()
	if err := store.MarkEarlyReady(ctx, "early"); err == nil {
		t.Fatal("marked readiness before preparation start")
	}
	if err := store.BeginStagedPreparation(ctx, "early"); err != nil {
		t.Fatal(err)
	}
	if err := store.BeginStagedPreparation(ctx, "early"); err == nil {
		t.Fatal("restarted preparation")
	}
	if err := store.MarkEarlyReady(ctx, "early"); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkEarlyReady(ctx, "early"); err == nil {
		t.Fatal("marked early readiness twice")
	}
	slot, err := store.Slot(ctx, "early")
	if err != nil || slot.State != "PREPARING" || slot.EarlyReadyAt == "" || slot.PreparationStartedAt == "" {
		t.Fatalf("slot=%+v: %v", slot, err)
	}
	data, err := json.Marshal(slot)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "EarlyReady") || strings.Contains(string(data), "PreparationStarted") {
		t.Fatalf("public shape changed: %s", data)
	}
	if err := store.SetSlotState(ctx, "early", []string{"PREPARING"}, "QUARANTINED", "TEST"); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkEarlyReady(ctx, "early"); err == nil {
		t.Fatal("marked quarantined slot ready")
	}
}
