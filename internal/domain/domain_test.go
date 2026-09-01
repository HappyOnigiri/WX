package domain

import (
	"path/filepath"
	"testing"
)

func TestIDsAndContainment(t *testing.T) {
	id, err := NewID()
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateID(id); err != nil {
		t.Fatal(err)
	}
	if err := ValidateID("not-an-id"); err == nil {
		t.Fatal("malformed ID was accepted")
	}
	if _, err := Canonicalize(""); err == nil {
		t.Fatal("empty path was canonicalized")
	}
	if _, err := Canonicalize(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("missing path was canonicalized")
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
	if !CanTransitionSlot(SlotReady, SlotFailed) || !CanTransitionSlot(SlotLeased, SlotQuarantined) {
		t.Fatal("fail-safe slot transitions were rejected")
	}
	if CanTransitionSlot(SlotArchived, SlotFailed) {
		t.Fatal("archived slot transitioned back to a failure state")
	}
}
