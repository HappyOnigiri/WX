package state

import (
	"context"
	"errors"
	"strings"
	"testing"
	"unicode/utf8"
)

// createSessionSlot は session と slot を 1 組ずつ登録し、その組の job を作れる状態にする。
func createSessionSlot(t *testing.T, store *Store, id, sessionState, slotState string) {
	t.Helper()
	session := Session{ID: id, WorkspaceID: "workspace", SlotID: id, State: sessionState, AgentKind: "codex", TokenHash: HashToken(id)}
	if _, err := store.CreateSlotSession(context.Background(),
		Slot{ID: id, WorkspaceID: "workspace", Generation: 1, RootID: testRootID, RelPath: "workspace/" + id, State: slotState},
		nil, session, ""); err != nil {
		t.Fatal(err)
	}
}

// failJob は job を 1 回実行して失敗として終える。
func failJob(t *testing.T, store *Store, job Job, runErr error, failureCode, detailPath string) {
	t.Helper()
	ctx := context.Background()
	if _, err := store.ClaimJob(ctx, job.ID, "test"); err != nil {
		t.Fatal(err)
	}
	if err := store.FinishJobWithDetail(ctx, job.ID, "test", runErr, failureCode, detailPath); err != nil {
		t.Fatal(err)
	}
}

func TestUnresolvedRecoveryFailuresKeepsTheRecordedCause(t *testing.T) {
	store := openTestStore(t)
	seedWorkspace(t, store)
	ctx := context.Background()
	createSessionSlot(t, store, "snapshot", "SNAPSHOTTING", "SNAPSHOTTING")
	job, err := store.CreateJob(ctx, "SNAPSHOT", "workspace", "snapshot", "snapshot")
	if err != nil {
		t.Fatal(err)
	}
	failJob(t, store, job, errors.New("write bundle: no space left on device"), "SNAPSHOT_FAILED", "/logs/snapshot.log")
	failures, err := store.UnresolvedRecoveryFailures(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(failures) != 1 {
		t.Fatalf("unresolved failures=%+v, want the snapshot failure", failures)
	}
	failure := failures[0]
	if failure.Kind != "SNAPSHOT" || failure.FailureCode != "SNAPSHOT_FAILED" || failure.DetailPath != "/logs/snapshot.log" {
		t.Fatalf("failure=%+v", failure)
	}
	if !strings.Contains(failure.FailureMessage, "no space left on device") {
		t.Fatalf("failure message=%q, want the recorded reason", failure.FailureMessage)
	}
	if failure.SessionState != "SNAPSHOTTING" || failure.SlotState != "SNAPSHOTTING" || failure.SlotPath != testRootPath+"/workspace/snapshot" {
		t.Fatalf("failure state=%+v", failure)
	}
}

func TestUnresolvedRecoveryFailuresExcludesResolvedHistory(t *testing.T) {
	store := openTestStore(t)
	seedWorkspace(t, store)
	ctx := context.Background()
	createSessionSlot(t, store, "retried", "SNAPSHOTTING", "SNAPSHOTTING")
	first, err := store.CreateJob(ctx, "SNAPSHOT", "workspace", "retried", "retried")
	if err != nil {
		t.Fatal(err)
	}
	failJob(t, store, first, errors.New("transient failure"), "SNAPSHOT_FAILED", "")
	second, err := store.CreateJob(ctx, "SNAPSHOT", "workspace", "retried", "retried")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ClaimJob(ctx, second.ID, "test"); err != nil {
		t.Fatal(err)
	}
	if err := store.FinishJob(ctx, second.ID, "test", nil); err != nil {
		t.Fatal(err)
	}
	failures, err := store.UnresolvedRecoveryFailures(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(failures) != 0 {
		t.Fatalf("unresolved failures after a later success=%+v", failures)
	}
}

func TestUnresolvedRecoveryFailuresExcludesArchivedSessions(t *testing.T) {
	store := openTestStore(t)
	seedWorkspace(t, store)
	ctx := context.Background()
	createSessionSlot(t, store, "archived", "ARCHIVED", "SNAPSHOTTED")
	job, err := store.CreateJob(ctx, "SNAPSHOT", "workspace", "archived", "archived")
	if err != nil {
		t.Fatal(err)
	}
	failJob(t, store, job, errors.New("failed before the retry succeeded"), "SNAPSHOT_FAILED", "")
	failures, err := store.UnresolvedRecoveryFailures(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(failures) != 0 {
		t.Fatalf("unresolved failures for an archived session=%+v", failures)
	}
}

func TestTruncateFailureMessageMarksWhatItDropped(t *testing.T) {
	if got := truncateFailureMessage("short"); got != "short" {
		t.Fatalf("short message=%q", got)
	}
	long := strings.Repeat("x", maxFailureMessage+10)
	got := truncateFailureMessage(long)
	if len(got) <= maxFailureMessage || !strings.HasSuffix(got, "(truncated)") {
		t.Fatalf("long message length=%d suffix=%q", len(got), got[len(got)-20:])
	}
	// 上限の境界に多バイト文字が来ても、壊れた UTF-8 を残さない。
	for offset := 1; offset <= 3; offset++ {
		multibyte := strings.Repeat("x", maxFailureMessage-offset) + strings.Repeat("失", 10)
		truncated := truncateFailureMessage(multibyte)
		if !utf8.ValidString(truncated) {
			t.Fatalf("truncated message at offset %d is not valid UTF-8: %q", offset, truncated[len(truncated)-20:])
		}
		if !strings.HasSuffix(truncated, "(truncated)") {
			t.Fatalf("truncated message at offset %d lost its marker: %q", offset, truncated[len(truncated)-20:])
		}
	}
}
