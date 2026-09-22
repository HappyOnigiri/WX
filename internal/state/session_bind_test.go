package state

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
)

func createAgentBindSession(t *testing.T, store *Store, id string) {
	t.Helper()
	session := Session{ID: id, SlotID: id, State: "ACTIVE", AgentKind: "codex", TokenHash: HashToken(id)}
	if _, err := store.CreateSlotSession(context.Background(), Slot{
		ID: id, State: "LEASED", RootID: testRootID, RelPath: filepath.Join("_unbound", id),
	}, nil, session, ""); err != nil {
		t.Fatal(err)
	}
}

func bindAgentSessionForTest(t *testing.T, store *Store, id, agentID string) {
	t.Helper()
	if err := store.BindAgentSession(context.Background(), id, agentID); err != nil {
		t.Fatal(err)
	}
}

func TestBindAgentSessionReplacesVerifiedForkMapping(t *testing.T) {
	t.Parallel()
	store := openTestStore(t)
	ctx := context.Background()
	createAgentBindSession(t, store, "fork-root")
	bindAgentSessionForTest(t, store, "fork-root", "native-parent")

	primary, err := store.BindAgentSessionFromHook(ctx, "fork-root", "native-child", "fork", "native-parent")
	if err != nil || !primary {
		t.Fatalf("replace agent session mapping: primary=%v err=%v", primary, err)
	}
	got, err := store.FindByAgentSession(ctx, "codex", "native-child")
	if err != nil || got.ID != "fork-root" {
		t.Fatalf("new mapping=%+v err=%v", got, err)
	}
	if _, err := store.FindByAgentSession(ctx, "codex", "native-parent"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("old mapping still exists: err=%v", err)
	}
}

func TestBindAgentSessionNormalBindIsIdempotent(t *testing.T) {
	t.Parallel()
	store := openTestStore(t)
	ctx := context.Background()
	createAgentBindSession(t, store, "idempotent-root")
	bindAgentSessionForTest(t, store, "idempotent-root", "native")
	if err := store.BindAgentSession(ctx, "idempotent-root", "native"); err != nil {
		t.Fatalf("same native ID bind: %v", err)
	}
}

func TestBindAgentSessionFromHookIgnoresAuxiliaryCodexStarts(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	for _, source := range []string{"startup", "fork"} {
		t.Run(source, func(t *testing.T) {
			store := openTestStore(t)
			id := "auxiliary-" + source
			createAgentBindSession(t, store, id)
			bindAgentSessionForTest(t, store, id, "native-primary")

			primary, err := store.BindAgentSessionFromHook(ctx, id, "native-btw", source)
			if err != nil || primary {
				t.Fatalf("auxiliary bind: primary=%v err=%v", primary, err)
			}
			got, err := store.SessionByID(ctx, id)
			if err != nil || got.AgentSessionID != "native-primary" || got.State != "ACTIVE" {
				t.Fatalf("auxiliary bind changed session: %+v err=%v", got, err)
			}
		})
	}
}

func TestBindAgentSessionFromHookKeepsStrictConflicts(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	for _, test := range []struct {
		name, kind, source string
	}{
		{name: "Codex resume", kind: "codex", source: "resume"},
		{name: "Codex unknown", kind: "codex", source: "future"},
		{name: "Claude startup", kind: "claude", source: "startup"},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := openTestStore(t)
			session := Session{ID: test.name, SlotID: test.name, State: "ACTIVE", AgentKind: test.kind, TokenHash: HashToken(test.name)}
			if _, err := store.CreateSlotSession(ctx, Slot{ID: test.name, State: "LEASED", RootID: testRootID, RelPath: filepath.Join("_unbound", test.name)}, nil, session, ""); err != nil {
				t.Fatal(err)
			}
			bindAgentSessionForTest(t, store, test.name, "native-primary")
			if _, err := store.BindAgentSessionFromHook(ctx, test.name, "native-other", test.source); err == nil {
				t.Fatal("conflicting bind was accepted")
			}
		})
	}
	for _, source := range []string{"startup", "fork"} {
		t.Run("child Codex "+source, func(t *testing.T) {
			store := openTestStore(t)
			parentID := "parent-" + source
			childID := "child-" + source
			createAgentBindSession(t, store, parentID)
			child := Session{ID: childID, SlotID: childID, ParentSessionID: parentID, State: "ACTIVE", AgentKind: "codex", TokenHash: HashToken(childID)}
			if _, err := store.CreateSlotSession(ctx, Slot{ID: childID, State: "LEASED", RootID: testRootID, RelPath: filepath.Join("_unbound", childID)}, nil, child, ""); err != nil {
				t.Fatal(err)
			}
			bindAgentSessionForTest(t, store, childID, "native-primary")
			if _, err := store.BindAgentSessionFromHook(ctx, childID, "native-other", source); err == nil {
				t.Fatal("child session accepted an auxiliary bind")
			}
		})
	}
}

func TestBindAgentSessionRejectsForkWhenSourceChanged(t *testing.T) {
	t.Parallel()
	store := openTestStore(t)
	ctx := context.Background()
	createAgentBindSession(t, store, "changed-root")
	bindAgentSessionForTest(t, store, "changed-root", "native-current")

	if err := store.BindAgentSession(ctx, "changed-root", "native-child", "native-old"); err == nil {
		t.Fatal("fork replacement accepted a stale source mapping")
	}
	got, err := store.SessionByID(ctx, "changed-root")
	if err != nil || got.AgentSessionID != "native-current" {
		t.Fatalf("stale replacement changed mapping: %+v err=%v", got, err)
	}
}

func TestBindAgentSessionRejectsForkWhenTargetIsOwned(t *testing.T) {
	t.Parallel()
	store := openTestStore(t)
	ctx := context.Background()
	createAgentBindSession(t, store, "target-root")
	bindAgentSessionForTest(t, store, "target-root", "native-parent")
	createAgentBindSession(t, store, "other-root")
	bindAgentSessionForTest(t, store, "other-root", "native-child")

	if err := store.BindAgentSession(ctx, "target-root", "native-child", "native-parent"); err == nil {
		t.Fatal("fork replacement stole another session's native ID")
	}
	got, err := store.SessionByID(ctx, "target-root")
	if err != nil || got.AgentSessionID != "native-parent" {
		t.Fatalf("target mapping changed after conflict: %+v err=%v", got, err)
	}
}

func TestBindAgentSessionRejectsDelayedForkAfterReplacement(t *testing.T) {
	t.Parallel()
	store := openTestStore(t)
	ctx := context.Background()
	createAgentBindSession(t, store, "retry-root")
	bindAgentSessionForTest(t, store, "retry-root", "native-parent")

	if err := store.BindAgentSession(ctx, "retry-root", "native-child", "native-parent"); err != nil {
		t.Fatalf("first replacement: %v", err)
	}
	if err := store.BindAgentSession(ctx, "retry-root", "native-child", "native-parent"); err == nil {
		t.Fatal("delayed fork replacement was accepted after the old mapping changed")
	}
}
