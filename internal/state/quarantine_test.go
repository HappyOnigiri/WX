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
