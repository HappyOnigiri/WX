package state

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestQuarantineArtifactReportsFirstDetectionAndKeepsDetectedAt(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	inserted, err := store.QuarantineArtifact(ctx, "unknown_refs", "repo:refs/wx/recovery/a", "first")
	if err != nil || !inserted {
		t.Fatalf("first record inserted=%v err=%v", inserted, err)
	}
	var firstDetectedAt string
	if err := store.db.QueryRowContext(ctx, `SELECT detected_at FROM quarantined_artifacts WHERE path=?`, "repo:refs/wx/recovery/a").Scan(&firstDetectedAt); err != nil {
		t.Fatal(err)
	}
	// 再検出は新規と区別され、detected_at は初回検出時刻のまま残る。これが毎周の警告を抑える前提である。
	inserted, err = store.QuarantineArtifact(ctx, "unknown_refs", "repo:refs/wx/recovery/a", "second")
	if err != nil || inserted {
		t.Fatalf("repeat record inserted=%v err=%v", inserted, err)
	}
	var detectedAt, reason string
	if err := store.db.QueryRowContext(ctx, `SELECT detected_at,reason FROM quarantined_artifacts WHERE path=?`, "repo:refs/wx/recovery/a").Scan(&detectedAt, &reason); err != nil {
		t.Fatal(err)
	}
	if detectedAt != firstDetectedAt || reason != "second" {
		t.Fatalf("detected_at=%q want %q, reason=%q", detectedAt, firstDetectedAt, reason)
	}
}

func TestPruneQuarantinedArtifactsOnlyTouchesRedetectedKinds(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	for _, record := range []struct{ kind, path string }{
		{"unknown_refs", "repo:refs/wx/recovery/gone"},
		{"unknown_refs", "repo:refs/wx/recovery/present"},
		{"standby_slot", "/root/standby"},
	} {
		if _, err := store.QuarantineArtifact(ctx, record.kind, record.path, "reason"); err != nil {
			t.Fatal(err)
		}
	}
	present := map[string]bool{"repo:refs/wx/recovery/present": true}
	if err := store.PruneQuarantinedArtifacts(ctx, []string{"unknown_refs", "mismatched_refs"}, present); err != nil {
		t.Fatal(err)
	}
	diagnostics, err := store.StatusDiagnostics(ctx)
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[string]string{}
	for _, item := range diagnostics.Quarantine {
		kinds[item.Path] = item.Kind
	}
	if len(kinds) != 2 || kinds["repo:refs/wx/recovery/present"] != "unknown_refs" || kinds["/root/standby"] != "standby_slot" {
		t.Fatalf("quarantine after prune=%v", kinds)
	}
	if err := store.ForgetQuarantinedArtifact(ctx, "repo:refs/wx/recovery/present"); err != nil {
		t.Fatal(err)
	}
	if err := store.ForgetQuarantinedArtifact(ctx, "repo:refs/wx/recovery/present"); err != nil {
		t.Fatalf("forgetting an absent record failed: %v", err)
	}
	if err := store.PruneQuarantinedArtifacts(ctx, nil, nil); err != nil {
		t.Fatalf("prune without kinds failed: %v", err)
	}
}

func TestQuarantineMissingRecoveryRefQuarantinesDurableMappings(t *testing.T) {
	store := openTestStore(t)
	seedWorkspace(t, store)
	ctx := context.Background()
	session := Session{ID: "recover", WorkspaceID: "workspace", SlotID: "recover", State: "ACTIVE", AgentKind: "codex", TokenHash: HashToken("token")}
	if _, err := store.CreateSlotSession(ctx, Slot{ID: session.SlotID, WorkspaceID: "workspace", Generation: 1, RootID: testRootID, RelPath: filepath.Join("workspace", session.SlotID), State: "LEASED"}, nil, session, ""); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveSnapshot(ctx, Snapshot{ID: "snapshot", SessionID: session.ID, RepositoryID: "repository", HeadOID: "head", HeadRef: "refs/wx/recovery/missing", IndexTreeOID: "index", WorktreeOID: "tree", WorktreeRef: "refs/wx/recovery/worktree", Status: "ARCHIVED", CreatedAt: now(), ExpiresAt: FormatTime(time.Now().Add(time.Hour))}); err != nil {
		t.Fatal(err)
	}
	if err := store.QuarantineMissingRecoveryRef(ctx, "refs/wx/recovery/missing"); err != nil {
		t.Fatal(err)
	}
	var snapshotStatus, sessionState, slotState, failureCode string
	if err := store.db.QueryRowContext(ctx, `SELECT status FROM snapshots WHERE id='snapshot'`).Scan(&snapshotStatus); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRowContext(ctx, `SELECT state FROM sessions WHERE id='recover'`).Scan(&sessionState); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRowContext(ctx, `SELECT state,failure_code FROM slots WHERE id='recover'`).Scan(&slotState, &failureCode); err != nil {
		t.Fatal(err)
	}
	if snapshotStatus != "QUARANTINED" || sessionState != "QUARANTINED" || slotState != "QUARANTINED" || failureCode != "RECOVERY_REF_MISSING" {
		t.Fatalf("quarantine states snapshot=%q session=%q slot=%q code=%q", snapshotStatus, sessionState, slotState, failureCode)
	}
}

func TestQuarantineRecoveryRefStopsAtDurableBoundaries(t *testing.T) {
	t.Run("query", func(t *testing.T) {
		store := openTestStore(t)
		if _, err := store.db.Exec(`DROP TABLE snapshots`); err != nil {
			t.Fatal(err)
		}
		if err := store.QuarantineMissingRecoveryRef(context.Background(), "refs/wx/missing"); err == nil {
			t.Fatal("quarantine succeeded without snapshot storage")
		}
	})

	store := openTestStore(t)
	seedWorkspace(t, store)
	ctx := context.Background()
	session := Session{ID: "quarantine", WorkspaceID: "workspace", SlotID: "quarantine", State: "ACTIVE", AgentKind: "codex", TokenHash: HashToken("quarantine")}
	if _, err := store.CreateSlotSession(ctx, Slot{ID: session.SlotID, WorkspaceID: "workspace", Generation: 1, RootID: testRootID, RelPath: filepath.Join("workspace", session.SlotID), State: "LEASED"}, nil, session, ""); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveSnapshot(ctx, Snapshot{ID: "quarantine-snapshot", SessionID: session.ID, RepositoryID: "repository", HeadOID: "head", HeadRef: "refs/wx/missing", IndexTreeOID: "index", WorktreeOID: "tree", WorktreeRef: "refs/wx/tree", Status: "ARCHIVED", CreatedAt: now(), ExpiresAt: now()}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`CREATE TRIGGER fail_quarantine_snapshot BEFORE UPDATE ON snapshots BEGIN SELECT RAISE(ABORT,'fault'); END`); err != nil {
		t.Fatal(err)
	}
	if err := store.QuarantineMissingRecoveryRef(ctx, "refs/wx/missing"); err == nil {
		t.Fatal("quarantine succeeded despite snapshot update fault")
	}
}

func TestQuarantineMissingRecoveryRefPropagatesTransactionFaults(t *testing.T) {
	ctx := context.Background()
	newSnapshotFixture := func(t *testing.T, id string) *Store {
		t.Helper()
		store := openTestStore(t)
		seedWorkspace(t, store)
		session := Session{ID: id, WorkspaceID: "workspace", SlotID: id, State: "ACTIVE", AgentKind: "codex", TokenHash: HashToken(id)}
		if _, err := store.CreateSlotSession(ctx, Slot{ID: id, WorkspaceID: "workspace", Generation: 1, RootID: testRootID, RelPath: filepath.Join("workspace", id), State: "LEASED"}, nil, session, ""); err != nil {
			t.Fatal(err)
		}
		if err := store.SaveSnapshot(ctx, Snapshot{ID: id, SessionID: id, RepositoryID: "repository", HeadOID: "head", HeadRef: "refs/wx/recovery/" + id, IndexTreeOID: "index", WorktreeOID: "tree", WorktreeRef: "refs/wx/recovery/" + id + "-worktree", Status: "ARCHIVED", CreatedAt: now(), ExpiresAt: FormatTime(time.Now().Add(time.Hour))}); err != nil {
			t.Fatal(err)
		}
		return store
	}

	t.Run("session quarantine fault", func(t *testing.T) {
		store := newSnapshotFixture(t, "quarantine-session-fault")
		if _, err := store.db.Exec(`CREATE TRIGGER fail_quarantine_session BEFORE UPDATE OF state ON sessions WHEN OLD.id='quarantine-session-fault' BEGIN SELECT RAISE(ABORT,'fault'); END`); err != nil {
			t.Fatal(err)
		}
		if err := store.QuarantineMissingRecoveryRef(ctx, "refs/wx/recovery/quarantine-session-fault"); err == nil {
			t.Fatal("recovery ref quarantine succeeded despite a session update fault")
		}
		if session, err := store.SessionByID(ctx, "quarantine-session-fault"); err != nil || session.State != "ACTIVE" {
			t.Fatalf("rolled-back quarantine session=%+v err=%v", session, err)
		}
	})

	t.Run("slot quarantine fault", func(t *testing.T) {
		store := newSnapshotFixture(t, "quarantine-slot-fault")
		if _, err := store.db.Exec(`CREATE TRIGGER fail_quarantine_slot BEFORE UPDATE OF state ON slots WHEN OLD.id='quarantine-slot-fault' BEGIN SELECT RAISE(ABORT,'fault'); END`); err != nil {
			t.Fatal(err)
		}
		if err := store.QuarantineMissingRecoveryRef(ctx, "refs/wx/recovery/quarantine-slot-fault"); err == nil {
			t.Fatal("recovery ref quarantine succeeded despite a slot update fault")
		}
		if slot, err := store.Slot(ctx, "quarantine-slot-fault"); err != nil || slot.State != "LEASED" {
			t.Fatalf("rolled-back quarantine slot=%+v err=%v", slot, err)
		}
	})
}

// quarantinedRecoveryFixture は recovery ref を失って行き止まりになった session を 1 件作る。
func quarantinedRecoveryFixture(t *testing.T, store *Store, slotState string) context.Context {
	t.Helper()
	ctx := context.Background()
	seedWorkspace(t, store)
	session := Session{ID: "session", WorkspaceID: "workspace", SlotID: "slot", State: "ARCHIVED", AgentKind: "codex", TokenHash: HashToken("token")}
	slot := Slot{ID: "slot", WorkspaceID: "workspace", Generation: 1, RootID: testRootID, RelPath: "workspace/slot", State: slotState}
	if _, err := store.CreateSlotSession(ctx, slot, nil, session, ""); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveSnapshot(ctx, Snapshot{
		ID: "snapshot", SessionID: "session", RepositoryID: "repository", HeadOID: "head",
		HeadRef: "refs/wx/recovery/head", IndexTreeOID: "index", WorktreeOID: "worktree", WorktreeRef: "refs/wx/recovery/worktree",
		Status: "ARCHIVED", CreatedAt: now(), ExpiresAt: FormatTime(time.Now().Add(time.Hour)),
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.QuarantineMissingRecoveryRef(ctx, "refs/wx/recovery/head"); err != nil {
		t.Fatal(err)
	}
	return ctx
}

// TestDiscardQuarantinedRecoveryUnblocksForget は隔離からの唯一の出口が機能することを確認する。
// この経路が無いと sessions は EXPIRED へ進めず、ForgetWorkspace の前提を永久に満たせない。
func TestDiscardQuarantinedRecoveryUnblocksForget(t *testing.T) {
	store := openTestStore(t)
	ctx := quarantinedRecoveryFixture(t, store, "SNAPSHOTTED")
	sessions, err := store.QuarantinedRecoverySessions(ctx, "/workspace")
	if err != nil || len(sessions) != 1 {
		t.Fatalf("quarantined recovery sessions=%+v err=%v", sessions, err)
	}
	got := sessions[0]
	if got.SessionID != "session" || got.SlotID != "slot" || got.SlotState != "QUARANTINED" || got.Snapshots != 1 || got.WorkspaceSnapshots != 0 {
		t.Fatalf("quarantined recovery session=%+v", got)
	}
	if err := store.ForgetWorkspace(ctx, "/workspace"); err == nil {
		t.Fatal("forget completed while the quarantined recovery state was still recorded")
	}
	if err := store.DiscardQuarantinedRecovery(ctx, "session"); err != nil {
		t.Fatalf("discard quarantined recovery: %v", err)
	}
	var sessionState, owner string
	var snapshots int
	if err := store.db.QueryRowContext(ctx, `SELECT se.state,COALESCE(sl.owner_session_id,''),(SELECT count(*) FROM snapshots sn WHERE sn.session_id=se.id) FROM sessions se JOIN slots sl ON sl.id=se.slot_id WHERE se.id='session'`).
		Scan(&sessionState, &owner, &snapshots); err != nil {
		t.Fatal(err)
	}
	if sessionState != "EXPIRED" || owner != "" || snapshots != 0 {
		t.Fatalf("after discard: session=%q owner=%q snapshots=%d", sessionState, owner, snapshots)
	}
	// slot の worktree の回収は daemon 側の REMOVE job が行うため、ここでは回収後の状態を置いて前提の充足だけを見る。
	if _, err := store.db.ExecContext(ctx, `UPDATE slots SET state='ARCHIVED' WHERE id='slot'`); err != nil {
		t.Fatal(err)
	}
	if err := store.ForgetWorkspace(ctx, "/workspace"); err != nil {
		t.Fatalf("forget after discarding the quarantined recovery state: %v", err)
	}
	if remaining, err := store.QuarantinedRecoverySessions(ctx, "/workspace"); err != nil || len(remaining) != 0 {
		t.Fatalf("quarantined recovery sessions after forget=%+v err=%v", remaining, err)
	}
}

// TestDiscardQuarantinedRecoveryRefusesOtherStates は破棄を隔離された session に限ることを確認する。
func TestDiscardQuarantinedRecoveryRefusesOtherStates(t *testing.T) {
	t.Run("not quarantined", func(t *testing.T) {
		store := openTestStore(t)
		ctx := context.Background()
		seedWorkspace(t, store)
		session := Session{ID: "session", WorkspaceID: "workspace", SlotID: "slot", State: "ARCHIVED", AgentKind: "codex", TokenHash: HashToken("token")}
		slot := Slot{ID: "slot", WorkspaceID: "workspace", Generation: 1, RootID: testRootID, RelPath: "workspace/slot", State: "SNAPSHOTTED"}
		if _, err := store.CreateSlotSession(ctx, slot, nil, session, ""); err != nil {
			t.Fatal(err)
		}
		if err := store.DiscardQuarantinedRecovery(ctx, "session"); err == nil {
			t.Fatal("an ARCHIVED session was discarded as quarantined recovery state")
		}
		var state string
		if err := store.db.QueryRowContext(ctx, `SELECT state FROM sessions WHERE id='session'`).Scan(&state); err != nil {
			t.Fatal(err)
		}
		if state != "ARCHIVED" {
			t.Fatalf("refused discard changed the session state to %q", state)
		}
	})
	t.Run("active restore", func(t *testing.T) {
		store := openTestStore(t)
		ctx := quarantinedRecoveryFixture(t, store, "SNAPSHOTTED")
		child := Session{ID: "child", WorkspaceID: "workspace", SlotID: "child-slot", State: "RESTORING", AgentKind: "codex", TokenHash: HashToken("child"), ParentSessionID: "session"}
		childSlot := Slot{ID: "child-slot", WorkspaceID: "workspace", Generation: 1, RootID: testRootID, RelPath: "workspace/child", State: "RESTORING"}
		if _, err := store.CreateSlotSession(ctx, childSlot, nil, child, "RESTORE"); err != nil {
			t.Fatal(err)
		}
		if err := store.DiscardQuarantinedRecovery(ctx, "session"); err == nil {
			t.Fatal("recovery state with a running restore was discarded")
		}
		var snapshots int
		if err := store.db.QueryRowContext(ctx, `SELECT count(*) FROM snapshots WHERE session_id='session'`).Scan(&snapshots); err != nil {
			t.Fatal(err)
		}
		if snapshots != 1 {
			t.Fatalf("refused discard removed %d snapshot(s)", 1-snapshots)
		}
	})
}

// TestQuarantinedRecoverySessionsStayWithinTheirWorkspace は破棄対象の限定を確認する。
// 他 workspace の隔離 session を巻き添えにすると、利用者の作業が残る snapshot まで消えてしまう。
func TestQuarantinedRecoverySessionsStayWithinTheirWorkspace(t *testing.T) {
	store := openTestStore(t)
	ctx := quarantinedRecoveryFixture(t, store, "SNAPSHOTTED")
	seedWorkspaceRows(t, store, "other", "/other", "repository", "other-repository", "/other", "/other/.git", "")
	other := Session{ID: "other-session", WorkspaceID: "other", SlotID: "other-slot", State: "ARCHIVED", AgentKind: "codex", TokenHash: HashToken("other")}
	otherSlot := Slot{ID: "other-slot", WorkspaceID: "other", Generation: 1, RootID: testRootID, RelPath: "other/slot", State: "SNAPSHOTTED"}
	if _, err := store.CreateSlotSession(ctx, otherSlot, nil, other, ""); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveSnapshot(ctx, Snapshot{
		ID: "other-snapshot", SessionID: "other-session", RepositoryID: "other-repository", HeadOID: "head",
		HeadRef: "refs/wx/recovery/other-head", IndexTreeOID: "index", WorktreeOID: "worktree", WorktreeRef: "refs/wx/recovery/other-worktree",
		Status: "ARCHIVED", CreatedAt: now(), ExpiresAt: FormatTime(time.Now().Add(time.Hour)),
	}); err != nil {
		t.Fatal(err)
	}
	sessions, err := store.QuarantinedRecoverySessions(ctx, "/workspace")
	if err != nil || len(sessions) != 1 || sessions[0].SessionID != "session" {
		t.Fatalf("quarantined recovery sessions=%+v err=%v", sessions, err)
	}
	groups, err := store.QuarantinedRecoveryGroups(ctx)
	if err != nil || len(groups) != 1 || groups[0].Root != "/workspace" || groups[0].Sessions != 1 || groups[0].Snapshots != 1 {
		t.Fatalf("quarantined recovery groups=%+v err=%v", groups, err)
	}
	if err := store.DiscardQuarantinedRecovery(ctx, "session"); err != nil {
		t.Fatal(err)
	}
	var snapshots int
	if err := store.db.QueryRowContext(ctx, `SELECT count(*) FROM snapshots WHERE session_id='other-session'`).Scan(&snapshots); err != nil {
		t.Fatal(err)
	}
	if snapshots != 1 {
		t.Fatalf("snapshots of the untouched workspace=%d", snapshots)
	}
	// 破棄した workspace は診断の対象から消え、他 workspace の記録は残らない前提を満たす。
	groups, err = store.QuarantinedRecoveryGroups(ctx)
	if err != nil || len(groups) != 0 {
		t.Fatalf("quarantined recovery groups after discard=%+v err=%v", groups, err)
	}
}
