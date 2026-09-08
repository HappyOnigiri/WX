package state

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

// seedLease は貸出属性つきの LEASED slot と session を登録する。
func seedLease(t *testing.T, store *Store, id, kind, expiresAt, owner string, clientPID int) {
	t.Helper()
	session := Session{
		ID: id, WorkspaceID: "workspace", SlotID: id, State: "ACTIVE", AgentKind: "wx-" + kind,
		LeaseKind: kind, LeaseExpiresAt: expiresAt, LeaseOwnerSessionID: owner,
		ClientPID: clientPID, TokenHash: HashToken("token"),
	}
	slot := Slot{ID: id, WorkspaceID: "workspace", Generation: 1, RootID: testRootID, RelPath: filepath.Join("workspace", id), State: "LEASED"}
	if _, err := store.CreateSlotSession(context.Background(), slot, nil, session, ""); err != nil {
		t.Fatal(err)
	}
}

// candidateIDs は抽出結果を ID の集合へ畳む。
func candidateIDs(candidates []OrphanCandidate) map[string]bool {
	ids := map[string]bool{}
	for _, candidate := range candidates {
		ids[candidate.ID] = true
	}
	return ids
}

// 既存の agent 起動は貸出属性を書かないため、既定の 'agent' で読み戻る。
func TestSessionLeaseColumnsDefaultToAgent(t *testing.T) {
	store := openTestStore(t)
	seedWorkspace(t, store)
	ctx := context.Background()
	session := Session{ID: "agent", WorkspaceID: "workspace", SlotID: "agent", State: "ACTIVE", AgentKind: "codex", TokenHash: HashToken("token")}
	slot := Slot{ID: "agent", WorkspaceID: "workspace", Generation: 1, RootID: testRootID, RelPath: "workspace/agent", State: "LEASED"}
	if _, err := store.CreateSlotSession(ctx, slot, nil, session, ""); err != nil {
		t.Fatal(err)
	}
	stored, err := store.SessionByID(ctx, "agent")
	if err != nil {
		t.Fatal(err)
	}
	if stored.LeaseKind != LeaseKindAgent || stored.LeaseExpiresAt != "" || stored.LeaseOwnerSessionID != "" {
		t.Fatalf("agent session lease columns=%+v", stored)
	}
}

// 貸出属性は書いたとおりに読み戻り、token 認証経路でも同じ値が返る。
func TestSessionLeaseColumnsRoundTrip(t *testing.T) {
	store := openTestStore(t)
	seedWorkspace(t, store)
	ctx := context.Background()
	seedLease(t, store, "owner", LeaseKindShell, "", "", 0)
	expiry := FormatTime(time.Now().Add(time.Hour))
	seedLease(t, store, "child", LeaseKindPath, expiry, "owner", 0)
	stored, err := store.Session(ctx, "child", "token")
	if err != nil {
		t.Fatal(err)
	}
	if stored.LeaseKind != LeaseKindPath || stored.LeaseExpiresAt != expiry || stored.LeaseOwnerSessionID != "owner" {
		t.Fatalf("lease session=%+v", stored)
	}
}

// path 貸出は heartbeat を張らないため、orphan 回収の候補から外れる。
// shell / command 貸出は client を持つので従来どおり候補に残る。
func TestOrphanCandidatesExcludePathLeases(t *testing.T) {
	store := openTestStore(t)
	seedWorkspace(t, store)
	ctx := context.Background()
	seedLease(t, store, "detached", LeaseKindPath, "", "", 0)
	seedLease(t, store, "shell", LeaseKindShell, "", "", 4242)
	seedLease(t, store, "command", LeaseKindCommand, "", "", 4243)
	candidates, err := store.OrphanCandidates(ctx, FormatTime(time.Now().Add(time.Minute)))
	if err != nil {
		t.Fatal(err)
	}
	ids := candidateIDs(candidates)
	if ids["detached"] {
		t.Fatalf("path lease is an orphan candidate: %+v", candidates)
	}
	if !ids["shell"] || !ids["command"] {
		t.Fatalf("process-bound leases were excluded: %+v", candidates)
	}
}

// 期限掃引は agent 起動を拾わず、期限が来た貸出だけを返す。
func TestExpiredLeaseCandidatesSelectOnlyReachedDeadlines(t *testing.T) {
	store := openTestStore(t)
	seedWorkspace(t, store)
	ctx := context.Background()
	now := time.Now()
	seedLease(t, store, "expired", LeaseKindPath, FormatTime(now.Add(-time.Minute)), "", 0)
	seedLease(t, store, "pending", LeaseKindPath, FormatTime(now.Add(time.Hour)), "", 0)
	seedLease(t, store, "endless", LeaseKindShell, "", "", 0)
	agent := Session{ID: "agent", WorkspaceID: "workspace", SlotID: "agent", State: "ACTIVE", AgentKind: "codex", TokenHash: HashToken("token")}
	agentSlot := Slot{ID: "agent", WorkspaceID: "workspace", Generation: 1, RootID: testRootID, RelPath: "workspace/agent", State: "LEASED"}
	if _, err := store.CreateSlotSession(ctx, agentSlot, nil, agent, ""); err != nil {
		t.Fatal(err)
	}
	candidates, err := store.ExpiredLeaseCandidates(ctx, FormatTime(now))
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 1 || candidates[0].ID != "expired" || candidates[0].SlotID != "expired" {
		t.Fatalf("expired lease candidates=%+v", candidates)
	}
	// 返却済みの貸出は二度拾わない。
	if _, _, err := store.Release(ctx, "expired", "workspace", "expired"); err != nil {
		t.Fatal(err)
	}
	if candidates, err := store.ExpiredLeaseCandidates(ctx, FormatTime(now)); err != nil || len(candidates) != 0 {
		t.Fatalf("released lease still expires: %+v err=%v", candidates, err)
	}
}

// 親が使用中の間は子貸出を返却せず、親が返却された後だけ抽出する。
func TestOrphanedChildLeasesFollowTheOwnerSession(t *testing.T) {
	store := openTestStore(t)
	seedWorkspace(t, store)
	ctx := context.Background()
	seedLease(t, store, "owner", LeaseKindShell, "", "", 0)
	seedLease(t, store, "child", LeaseKindPath, "", "owner", 0)
	seedLease(t, store, "independent", LeaseKindPath, "", "", 0)
	if candidates, err := store.OrphanedChildLeases(ctx); err != nil || len(candidates) != 0 {
		t.Fatalf("child of a live owner was selected: %+v err=%v", candidates, err)
	}
	if _, _, err := store.Release(ctx, "owner", "workspace", "owner"); err != nil {
		t.Fatal(err)
	}
	candidates, err := store.OrphanedChildLeases(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 1 || candidates[0].ID != "child" {
		t.Fatalf("orphaned child leases=%+v", candidates)
	}
}
