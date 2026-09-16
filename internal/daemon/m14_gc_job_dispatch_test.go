package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/HappyOnigiri/WX/internal/state"
)

func TestGCProgressCountsFailedIssues(t *testing.T) {
	t.Parallel()

	progress := newGCProgress()
	progress.addFailed("target", "reason", errors.New("cause"))

	if progress.Failed != 1 {
		t.Fatalf("failed count=%d, want 1", progress.Failed)
	}
	if len(progress.Reasons) != 1 || progress.Reasons[0].Status != "failed" {
		t.Fatalf("failure reason=%+v, want one failed reason", progress.Reasons)
	}
	if !strings.Contains(progress.Reasons[0].Reason, "cause") {
		t.Fatalf("failure reason=%q, want cause", progress.Reasons[0].Reason)
	}
}

func TestGCDoesNotReportRepositoryPruningWhenNothingWasRemoved(t *testing.T) {
	t.Parallel()

	f := manualManagerFixture(t)
	var logs bytes.Buffer
	f.Manager.log = slog.New(slog.NewTextHandler(&logs, nil))

	result, err := f.Manager.GC(context.Background(), false)
	if err != nil {
		t.Fatalf("GC err=%v, result=%+v", err, result)
	}
	if result.Failed != 0 {
		t.Fatalf("GC failed=%d, result=%+v", result.Failed, result)
	}
	if strings.Contains(logs.String(), "pruned repository records") {
		t.Fatalf("GC logged repository pruning without removing a row: %q", logs.String())
	}
}

func TestHandlerLeavesZeroTimeoutReadyWaitUnbounded(t *testing.T) {
	t.Parallel()

	f := manualManagerFixture(t)
	ctx := context.Background()
	slotID := "m14-wait-ready"
	session := state.Session{
		ID:        slotID,
		SlotID:    slotID,
		State:     "ACTIVE",
		AgentKind: "codex",
		TokenHash: state.HashToken("token"),
	}
	if _, err := f.Store.CreateSlotSession(ctx, testSlotRow(t, f.Manager, "", slotID, 0, "LEASED"), nil, session, ""); err != nil {
		t.Fatal(err)
	}

	result, err := (Handler{Manager: f.Manager}).dispatch(ctx, "WaitReady", json.RawMessage(`{"session_id":"m14-wait-ready","token":"token","timeout_ms":0}`))
	if err != nil {
		t.Fatalf("zero-timeout WaitReady err=%v", err)
	}
	reply, ok := result.(map[string]bool)
	if !ok || !reply["ready"] {
		t.Fatalf("zero-timeout WaitReady reply=%T %v, want ready=true", result, result)
	}
}

func TestJobQueueClampsZeroInteractiveLimit(t *testing.T) {
	t.Parallel()

	q := newJobQueue(0)
	if got := q.limit(jobClassInteractive); got != 1 {
		t.Fatalf("zero interactive limit=%d, want 1", got)
	}
	if !q.add(queuedJob{id: "zero-limit", class: jobClassInteractive}) {
		t.Fatal("job was not accepted with a clamped interactive limit")
	}
	work, slot, ok := q.take()
	if !ok || work.id != "zero-limit" {
		t.Fatalf("take after zero limit=%+v ok=%v", work, ok)
	}
	q.finish(work, slot)
}

func TestRunRecoveredJobDoesNotResetFirstAttempt(t *testing.T) {
	t.Parallel()

	f := manualManagerFixture(t)
	ctx := context.Background()
	slotID := "m14-first-attempt"
	if _, err := f.Store.CreateSlotSession(
		ctx,
		testSlotRow(t, f.Manager, "", slotID, 0, "FAILED"),
		nil,
		state.Session{ID: slotID, SlotID: slotID, State: "RELEASING", AgentKind: "codex", TokenHash: state.HashToken("token")},
		"",
	); err != nil {
		t.Fatal(err)
	}

	err := f.Manager.runRecoveredJob(ctx, state.Job{Kind: "PREPARE", SlotID: slotID, Attempt: 1})
	if err == nil {
		t.Fatal("first-attempt PREPARE for a failed slot unexpectedly succeeded")
	}
	slot, readErr := f.Store.Slot(ctx, slotID)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if slot.State != "FAILED" {
		t.Fatalf("first-attempt recovery changed slot state to %q, want FAILED", slot.State)
	}
}
