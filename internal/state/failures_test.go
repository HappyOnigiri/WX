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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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

// createRestoreSession は復元元 session と、その復元用 session を 1 組作る。
// 元 session は ARCHIVED、復元用は resume 中に作られる子 session を表す。
func createRestoreSession(t *testing.T, store *Store, parentID, childID, childState string) {
	t.Helper()
	createSessionSlot(t, store, parentID, "ARCHIVED", "SNAPSHOTTED")
	session := Session{
		ID: childID, WorkspaceID: "workspace", SlotID: childID, ParentSessionID: parentID,
		State: childState, AgentKind: "codex", TokenHash: HashToken(childID),
	}
	if _, err := store.CreateSlotSession(context.Background(),
		Slot{ID: childID, WorkspaceID: "workspace", Generation: 1, RootID: testRootID, RelPath: "workspace/" + childID, State: "QUARANTINED"},
		nil, session, ""); err != nil {
		t.Fatal(err)
	}
}

// 復元用 session が EXPIRED でも、元 session が ARCHIVED のままなら復元は済んでいない。
func TestUnresolvedRecoveryFailuresReportsRestoreAfterTheRestoringSessionExpired(t *testing.T) {
	t.Parallel()
	store := openTestStore(t)
	seedWorkspace(t, store)
	ctx := context.Background()
	createRestoreSession(t, store, "origin", "restore", "EXPIRED")
	job, err := store.CreateJob(ctx, "RESTORE", "workspace", "restore", "restore")
	if err != nil {
		t.Fatal(err)
	}
	failJob(t, store, job, errors.New("read-tree refused a skip-worktree path"), "RESTORE_FAILED", "/logs/restore.log")
	failures, err := store.UnresolvedRecoveryFailures(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(failures) != 1 {
		t.Fatalf("unresolved failures=%+v, want the restore failure", failures)
	}
	failure := failures[0]
	if failure.Kind != "RESTORE" || failure.ParentSessionID != "origin" || failure.SlotState != "QUARANTINED" {
		t.Fatalf("failure=%+v", failure)
	}
	if !strings.Contains(failure.FailureMessage, "skip-worktree") || failure.DetailPath != "/logs/restore.log" {
		t.Fatalf("failure cause=%+v, want the recorded reason and log path", failure)
	}
}

// 同じ元 session への RESTORE が後から成功していれば、先の失敗は解消済みとして出さない。
func TestUnresolvedRecoveryFailuresExcludesRestoreRetriedOnAnotherSession(t *testing.T) {
	t.Parallel()
	store := openTestStore(t)
	seedWorkspace(t, store)
	ctx := context.Background()
	createRestoreSession(t, store, "origin", "first", "EXPIRED")
	failed, err := store.CreateJob(ctx, "RESTORE", "workspace", "first", "first")
	if err != nil {
		t.Fatal(err)
	}
	failJob(t, store, failed, errors.New("transient failure"), "RESTORE_FAILED", "")
	second := Session{
		ID: "second", WorkspaceID: "workspace", SlotID: "second", ParentSessionID: "origin",
		State: "ACTIVE", AgentKind: "codex", TokenHash: HashToken("second"),
	}
	if _, err := store.CreateSlotSession(ctx,
		Slot{ID: "second", WorkspaceID: "workspace", Generation: 1, RootID: testRootID, RelPath: "workspace/second", State: "LEASED"},
		nil, second, ""); err != nil {
		t.Fatal(err)
	}
	retry, err := store.CreateJob(ctx, "RESTORE", "workspace", "second", "second")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ClaimJob(ctx, retry.ID, "test"); err != nil {
		t.Fatal(err)
	}
	if err := store.FinishJob(ctx, retry.ID, "test", nil); err != nil {
		t.Fatal(err)
	}
	failures, err := store.UnresolvedRecoveryFailures(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(failures) != 0 {
		t.Fatalf("unresolved failures after a later restore succeeded=%+v", failures)
	}
}

// 元 session が EXPIRED まで進んでいれば復元は完了しているので、残った失敗は報告しない。
func TestUnresolvedRecoveryFailuresExcludesRestoreOfAnExpiredOrigin(t *testing.T) {
	t.Parallel()
	store := openTestStore(t)
	seedWorkspace(t, store)
	ctx := context.Background()
	createRestoreSession(t, store, "origin", "restore", "EXPIRED")
	if _, err := store.db.ExecContext(ctx, `UPDATE sessions SET state='EXPIRED' WHERE id='origin'`); err != nil {
		t.Fatal(err)
	}
	job, err := store.CreateJob(ctx, "RESTORE", "workspace", "restore", "restore")
	if err != nil {
		t.Fatal(err)
	}
	failJob(t, store, job, errors.New("failed before the retry succeeded"), "RESTORE_FAILED", "")
	failures, err := store.UnresolvedRecoveryFailures(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(failures) != 0 {
		t.Fatalf("unresolved failures for a restored origin=%+v", failures)
	}
}
