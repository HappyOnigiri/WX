package domain

import "testing"

func TestIDsAndContainment(t *testing.T) {
	id, err := NewID()
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateID(id); err != nil {
		t.Fatal(err)
	}
	if !IsWithin("/tmp/wx", "/tmp/wx/a/root") {
		t.Fatal("expected descendant")
	}
	for _, p := range []string{"/tmp/wx", "/tmp/wx-other", "/tmp/else"} {
		if IsWithin("/tmp/wx", p) {
			t.Errorf("%s must not be within root", p)
		}
	}
}

func TestSlotTransitions(t *testing.T) {
	if !CanTransitionSlot(SlotReady, SlotLeased) {
		t.Fatal("READY -> LEASED rejected")
	}
	if CanTransitionSlot(SlotArchived, SlotReady) {
		t.Fatal("ARCHIVED -> READY accepted")
	}
}
