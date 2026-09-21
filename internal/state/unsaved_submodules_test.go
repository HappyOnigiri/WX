package state

import (
	"context"
	"testing"
	"time"
)

// seedSnapshottedSlot は保持期限を過ぎた終了 worktree を 1 件作る。
func seedSnapshottedSlot(t *testing.T, store *Store, slotID string) {
	t.Helper()
	ctx := context.Background()
	archivedAt := FormatTime(time.Now().Add(-30 * time.Minute))
	if _, err := store.db.ExecContext(ctx, `INSERT INTO slots(id,workspace_id,generation,root_id,rel_path,state,created_at,updated_at) VALUES(?,'workspace',1,?,?,'SNAPSHOTTED',?,?)`, slotID, testRootID, "x/"+slotID, now(), now()); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `INSERT INTO sessions(id,workspace_id,slot_id,state,agent_kind,client_pid,session_token_hash,created_at,archived_at) VALUES(?,'workspace',?,'ARCHIVED','codex',1,x'00',?,?)`, "session-"+slotID, slotID, now(), archivedAt); err != nil {
		t.Fatal(err)
	}
}

// 未保全の submodule 作業を記録した slot は自動回収の候補から外れ、保護中としてだけ数えられる。
func TestUnsavedSubmodulesKeepSlotsOutOfGCCandidates(t *testing.T) {
	t.Parallel()
	store := openTestStore(t)
	seedWorkspace(t, store)
	ctx := context.Background()
	seedSnapshottedSlot(t, store, "plain")
	seedSnapshottedSlot(t, store, "protected")
	if err := store.ReplaceUnsavedSubmodules(ctx, "protected", "repository", []UnsavedSubmodule{{RepositoryID: "repository", Path: "sub/kid", Reasons: "MODIFIED,UNTRACKED"}}); err != nil {
		t.Fatal(err)
	}
	floor := FormatTime(time.Now().Add(-time.Minute))
	candidates, err := store.GCCandidates(ctx, floor, constantBefore(floor))
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 1 || candidates[0].SlotID != "plain" {
		t.Fatalf("gc candidates=%+v, want only the unprotected slot", candidates)
	}
	protected, err := store.ProtectedGCCandidates(ctx, floor, constantBefore(floor))
	if err != nil {
		t.Fatal(err)
	}
	if len(protected) != 1 || protected[0].SlotID != "protected" {
		t.Fatalf("protected candidates=%+v", protected)
	}
	clean, err := store.CleanCandidates(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, candidate := range clean {
		want := 0
		if candidate.SlotID == "protected" {
			want = 1
		}
		if candidate.UnsavedSubmodules != want {
			t.Fatalf("clean candidate %s unsaved submodules=%d, want %d", candidate.SlotID, candidate.UnsavedSubmodules, want)
		}
	}
}

// 再検出は repository 分を置き換え、実体を消した後は記録も残らない。
func TestReplaceUnsavedSubmodulesAndRemovalClearRecords(t *testing.T) {
	t.Parallel()
	store := openTestStore(t)
	seedWorkspace(t, store)
	ctx := context.Background()
	seedSnapshottedSlot(t, store, "protected")
	if err := store.ReplaceUnsavedSubmodules(ctx, "protected", "repository", []UnsavedSubmodule{
		{RepositoryID: "repository", Path: "sub/one", Reasons: "MODIFIED"},
		{RepositoryID: "repository", Path: "sub/two", Reasons: "UNTRACKED"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.ReplaceUnsavedSubmodules(ctx, "protected", "repository", []UnsavedSubmodule{{RepositoryID: "repository", Path: "sub/two", Reasons: "UNTRACKED"}}); err != nil {
		t.Fatal(err)
	}
	slots, err := store.ProtectedSlots(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(slots) != 1 || len(slots[0].Submodules) != 1 || slots[0].Submodules[0].Path != "sub/two" {
		t.Fatalf("protected slots=%+v, want only the still-unsaved submodule", slots)
	}
	if slots[0].Path == "" || slots[0].SlotID != "protected" {
		t.Fatalf("protected slot identity=%+v", slots[0])
	}
	if count, err := store.UnsavedSubmoduleCount(ctx, "protected"); err != nil || count != 1 {
		t.Fatalf("unsaved submodule count=%d err=%v, want the remaining record", count, err)
	}
	// 自動回収と同じ予約経路は、候補選定の後に記録が付いた slot も予約させない。
	if _, changed, err := store.ScheduleRemoval(ctx, "protected", "session-protected"); err != nil || changed {
		t.Fatalf("schedule removal changed=%v err=%v, want it refused while the work is unsaved", changed, err)
	}
	if slot, err := store.Slot(ctx, "protected"); err != nil || slot.State != "SNAPSHOTTED" {
		t.Fatalf("slot after the refused reservation=%+v err=%v", slot, err)
	}
	// 利用者が明示した破棄だけが実体を消し、そこでは記録も一緒に片付く。
	if _, changed, err := store.ScheduleDiscardRemoval(ctx, "protected"); err != nil || !changed {
		t.Fatalf("schedule discard removal changed=%v err=%v", changed, err)
	}
	if _, err := store.FinishRemoval(ctx, "protected"); err != nil {
		t.Fatal(err)
	}
	remaining, err := store.ProtectedSlots(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(remaining) != 0 {
		t.Fatalf("protected slots after removal=%+v, want none", remaining)
	}
}

// `wx slots --json` の行には、その slot が回収から外れていることが path として出る。
func TestListSlotsCarriesUnsavedSubmodulePaths(t *testing.T) {
	t.Parallel()
	store := openTestStore(t)
	seedWorkspace(t, store)
	ctx := context.Background()
	seedSnapshottedSlot(t, store, "protected")
	if err := store.ReplaceUnsavedSubmodules(ctx, "protected", "repository", []UnsavedSubmodule{{RepositoryID: "repository", Path: "sub/kid", Reasons: "MODIFIED"}}); err != nil {
		t.Fatal(err)
	}
	slots, err := store.ListSlots(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(slots) != 1 || len(slots[0].UnsavedSubmodules) != 1 || slots[0].UnsavedSubmodules[0] != "sub/kid" {
		t.Fatalf("slots=%+v", slots)
	}
}

func TestUnsavedSubmoduleReasonsSplitsTheRecordedOrder(t *testing.T) {
	t.Parallel()
	if got := UnsavedSubmoduleReasons(""); got != nil {
		t.Fatalf("reasons for an empty record=%v", got)
	}
	got := UnsavedSubmoduleReasons("MODIFIED,MISSING_OBJECT")
	if len(got) != 2 || got[0] != "MODIFIED" || got[1] != "MISSING_OBJECT" {
		t.Fatalf("reasons=%v", got)
	}
}
