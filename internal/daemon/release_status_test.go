package daemon

import (
	"context"
	"errors"
	"testing"

	"github.com/HappyOnigiri/WorktreeX/internal/state"
)

func TestReleaseStatusReportsFailedJobAndDetail(t *testing.T) {
	t.Parallel()
	f := manualManagerFixture(t)
	ctx := context.Background()
	sessionID := "release-status"
	session := state.Session{ID: sessionID, SlotID: sessionID, State: "ACTIVE", AgentKind: "wx-path", TokenHash: state.HashToken("token")}
	if _, err := f.Store.CreateSlotSession(ctx, testSlotRow(t, f.Manager, "", sessionID, 0, "LEASED"), nil, session, ""); err != nil {
		t.Fatal(err)
	}
	job, err := f.Store.CreateJob(ctx, "SNAPSHOT", "", sessionID, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := f.Store.ClaimJob(ctx, job.ID, "release-status-test")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Store.FinishJobWithDetail(ctx, claimed.ID, "release-status-test", errors.New("git snapshot failed"), "SNAPSHOT_FAILED:git-failure", "/tmp/details/git-failure.log"); err != nil {
		t.Fatal(err)
	}
	reply, err := f.Manager.ReleaseStatus(ctx, sessionID, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]any{
		"job_id":          job.ID,
		"job_kind":        "SNAPSHOT",
		"state":           "FAILED",
		"slot_state":      "LEASED",
		"session_state":   "ACTIVE",
		"failure_code":    "SNAPSHOT_FAILED:git-failure",
		"failure_message": "git snapshot failed",
		"detail_path":     "/tmp/details/git-failure.log",
	} {
		if got := reply[key]; got != want {
			t.Errorf("reply[%q]=%v, want %v", key, got, want)
		}
	}
}

func TestReleaseStatusStopsWhenJobWasCollected(t *testing.T) {
	t.Parallel()
	f := manualManagerFixture(t)
	ctx := context.Background()
	sessionID := "release-status-collected"
	session := state.Session{ID: sessionID, SlotID: sessionID, State: "EXPIRED", AgentKind: "wx-path", TokenHash: state.HashToken("token")}
	if _, err := f.Store.CreateSlotSession(ctx, testSlotRow(t, f.Manager, "", sessionID, 0, "SNAPSHOTTED"), nil, session, ""); err != nil {
		t.Fatal(err)
	}
	reply, err := f.Manager.ReleaseStatus(ctx, sessionID, "collected-job")
	if err != nil {
		t.Fatal(err)
	}
	if reply["job_id"] != "collected-job" || reply["state"] != "" {
		t.Fatalf("collected job reply=%v, want job id and empty state", reply)
	}
	if reply["slot_state"] != "SNAPSHOTTED" || reply["session_state"] != "EXPIRED" {
		t.Fatalf("collected job states=%v", reply)
	}
}
