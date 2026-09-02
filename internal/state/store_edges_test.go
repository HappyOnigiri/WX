package state

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/HappyOnigiri/WX/internal/discovery"
)

func TestRPCEnvelopeRejectsMalformedAuthenticatedPayloads(t *testing.T) {
	store := openTestStore(t)
	key := "key"
	method := "Mutate"
	paramsHash := rpcParamsHash(`{}`)
	encrypted, err := store.encryptRPCResult(key, method, paramsHash, []byte(`{"ok":true}`), "", "")
	if err != nil {
		t.Fatal(err)
	}
	if result, code, message, err := store.decryptRPCResult(key, method, paramsHash, encrypted); err != nil || string(result) != `{"ok":true}` || code != "" || message != "" {
		t.Fatalf("valid envelope result=%q code=%q message=%q err=%v", result, code, message, err)
	}

	block, err := aes.NewCipher(store.rpcKey)
	if err != nil {
		t.Fatal(err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatal(err)
	}
	nonce := make([]byte, aead.NonceSize())
	associated := []byte(key + "\x00" + method + "\x00" + paramsHash)
	malformed := append(append([]byte(nil), nonce...), aead.Seal(nil, nonce, []byte("{"), associated)...)
	if _, _, _, err := store.decryptRPCResult(key, method, paramsHash, malformed); err == nil {
		t.Fatal("authenticated malformed envelope was accepted")
	}
	store.rpcKey = []byte("invalid")
	if _, err := store.encryptRPCResult(key, method, paramsHash, nil, "", ""); err == nil {
		t.Fatal("invalid RPC key encrypted an envelope")
	}
	if _, _, _, err := store.decryptRPCResult(key, method, paramsHash, encrypted); err == nil {
		t.Fatal("invalid RPC key decrypted an envelope")
	}
}

func TestRPCKeyLoaderHandlesReadAndCreateBoundaries(t *testing.T) {
	root := t.TempDir()
	validPath := filepath.Join(root, "valid.key")
	key := []byte(strings.Repeat("k", 32))
	if err := os.WriteFile(validPath, key, 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := loadOrCreateRPCKey(validPath)
	if err != nil || string(loaded) != string(key) {
		t.Fatalf("existing RPC key=%x err=%v", loaded, err)
	}
	readDenied := filepath.Join(root, "read-denied.key")
	if err := os.WriteFile(readDenied, key, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(readDenied, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(readDenied, 0o600) })
	if _, err := loadOrCreateRPCKey(readDenied); err == nil {
		t.Fatal("unreadable RPC key was accepted")
	}
	if _, err := loadOrCreateRPCKey(filepath.Join(root, "missing", "key")); err == nil {
		t.Fatal("RPC key beneath missing parent was created")
	}
}

func TestFinishPreparationSchedulesSnapshotAfterRelease(t *testing.T) {
	store := openTestStore(t)
	seedWorkspace(t, store)
	ctx := context.Background()
	root := t.TempDir()
	session := Session{ID: "releasing", WorkspaceID: "workspace", SlotID: "releasing", State: "RELEASING", AgentKind: "codex", TokenHash: HashToken("token")}
	if _, err := store.CreateSlotSession(ctx, Slot{ID: "releasing", WorkspaceID: "workspace", Generation: 1, Path: filepath.Join(root, "slot"), State: "PREPARING"}, nil, session, ""); err != nil {
		t.Fatal(err)
	}
	job, scheduled, err := store.FinishPreparationWithRelease(ctx, session.SlotID)
	if err != nil || !scheduled || job.Kind != "SNAPSHOT" || job.WorkspaceID != "workspace" {
		t.Fatalf("finish preparation job=%+v scheduled=%v err=%v", job, scheduled, err)
	}
	slot, err := store.Slot(ctx, session.SlotID)
	if err != nil || slot.State != "DRAINING" {
		t.Fatalf("slot after release preparation=%+v err=%v", slot, err)
	}
}

func TestReleaseUnboundSessionSchedulesRemoval(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	root := t.TempDir()
	session := Session{ID: "unbound", SlotID: "unbound", State: "UNBOUND", AgentKind: "codex", TokenHash: HashToken("token")}
	if _, err := store.CreateSlotSession(ctx, Slot{ID: session.SlotID, Path: filepath.Join(root, "slot"), State: "UNBOUND"}, nil, session, ""); err != nil {
		t.Fatal(err)
	}
	job, changed, err := store.Release(ctx, session.ID, "", session.SlotID)
	if err != nil || !changed || job.Kind != "REMOVE" {
		t.Fatalf("unbound release job=%+v changed=%v err=%v", job, changed, err)
	}
	storedSession, err := store.SessionByID(ctx, session.ID)
	if err != nil || storedSession.State != "EXPIRED" {
		t.Fatalf("session after unbound release=%+v err=%v", storedSession, err)
	}
	storedSlot, err := store.Slot(ctx, session.SlotID)
	if err != nil || storedSlot.State != "REMOVING" || storedSlot.OwnerSessionID != "" {
		t.Fatalf("slot after unbound release=%+v err=%v", storedSlot, err)
	}
}

func TestQuarantineMissingRecoveryRefQuarantinesDurableMappings(t *testing.T) {
	store := openTestStore(t)
	seedWorkspace(t, store)
	ctx := context.Background()
	root := t.TempDir()
	session := Session{ID: "recover", WorkspaceID: "workspace", SlotID: "recover", State: "ACTIVE", AgentKind: "codex", TokenHash: HashToken("token")}
	if _, err := store.CreateSlotSession(ctx, Slot{ID: session.SlotID, WorkspaceID: "workspace", Generation: 1, Path: filepath.Join(root, "slot"), State: "LEASED"}, nil, session, ""); err != nil {
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

func TestRegistryReadModelsReturnCommittedLifecycleRows(t *testing.T) {
	store := openTestStore(t)
	seedWorkspace(t, store)
	ctx := context.Background()
	root := t.TempDir()
	if _, err := store.CreateStandby(ctx, Slot{ID: "ready", WorkspaceID: "workspace", Generation: 1, Path: filepath.Join(root, "ready"), State: "READY"}, []SlotRepository{{RepositoryID: "repository", WorktreePath: filepath.Join(root, "ready", "repository"), State: "READY", RequestedRef: "main", BaseOID: "head", Fingerprint: "fingerprint"}}); err != nil {
		t.Fatal(err)
	}
	if workspace, err := store.WorkspaceByRoot(ctx, "/workspace"); err != nil || workspace.ID != "workspace" || len(workspace.Repositories) != 1 {
		t.Fatalf("workspace by root=%+v err=%v", workspace, err)
	}
	if roots, err := store.WorkspaceRoots(ctx); err != nil || len(roots) != 1 || roots[0] != "/workspace" {
		t.Fatalf("workspace roots=%v err=%v", roots, err)
	}
	if ready, found, err := store.ReadySlot(ctx, "workspace"); err != nil || !found || ready.ID != "ready" {
		t.Fatalf("ready slot=%+v found=%v err=%v", ready, found, err)
	}
	if repositories, err := store.SlotRepositories(ctx, "ready"); err != nil || len(repositories) != 1 || repositories[0].RepositoryID != "repository" {
		t.Fatalf("slot repositories=%+v err=%v", repositories, err)
	}
	if artifacts, err := store.SlotArtifacts(ctx); err != nil || len(artifacts) != 1 || artifacts[0].ID != "ready" {
		t.Fatalf("slot artifacts=%+v err=%v", artifacts, err)
	}
	if repositories, err := store.Repositories(ctx); err != nil || len(repositories) != 1 || repositories[0].ID != "repository" {
		t.Fatalf("repositories=%+v err=%v", repositories, err)
	}
	if candidates, err := store.ColdRepositoryCandidates(ctx, FormatTime(time.Now().Add(time.Hour))); err != nil || len(candidates) != 1 || candidates[0].RepositoryID != "repository" {
		t.Fatalf("cold candidates=%+v err=%v", candidates, err)
	}
	if candidates, err := store.StandbyGCCandidates(ctx, now(), 0); err != nil || len(candidates) != 1 || candidates[0].SlotID != "ready" {
		t.Fatalf("standby GC candidates=%+v err=%v", candidates, err)
	}
	if value, err := TokenHex(); err != nil || len(value) != 64 {
		t.Fatalf("token hex=%q err=%v", value, err)
	}

	session := Session{ID: "archived", WorkspaceID: "workspace", SlotID: "archived", State: "ARCHIVED", AgentKind: "codex", TokenHash: HashToken("token")}
	if _, err := store.CreateSlotSession(ctx, Slot{ID: session.SlotID, WorkspaceID: "workspace", Generation: 1, Path: filepath.Join(root, "archived"), State: "ARCHIVED"}, nil, session, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE sessions SET archived_at=?,expires_at=? WHERE id=?`, FormatTime(time.Now().Add(-time.Hour)), FormatTime(time.Now().Add(-time.Minute)), session.ID); err != nil {
		t.Fatal(err)
	}
	snapshot := Snapshot{ID: "archived-snapshot", SessionID: session.ID, RepositoryID: "repository", HeadOID: "head", HeadRef: "refs/wx/recovery/head", IndexTreeOID: "index", WorktreeOID: "tree", WorktreeRef: "refs/wx/recovery/worktree", Status: "ARCHIVED", CreatedAt: now(), ExpiresAt: FormatTime(time.Now().Add(-time.Minute))}
	if err := store.SaveSnapshot(ctx, snapshot); err != nil {
		t.Fatal(err)
	}
	if snapshots, err := store.Snapshots(ctx, session.ID); err != nil || len(snapshots) != 1 || snapshots[0].ID != snapshot.ID {
		t.Fatalf("snapshots=%+v err=%v", snapshots, err)
	}
	if expired, err := store.ExpiredSnapshots(ctx, FormatTime(time.Now())); err != nil || len(expired) != 1 || expired[0].ID != snapshot.ID {
		t.Fatalf("expired snapshots=%+v err=%v", expired, err)
	}
}

func TestLeaseReadyWithColdPromotesRepositoriesAndStartsPreparation(t *testing.T) {
	store := openTestStore(t)
	seedWorkspace(t, store)
	ctx := context.Background()
	root := t.TempDir()
	if _, err := store.CreateStandby(ctx, Slot{ID: "cold-slot", WorkspaceID: "workspace", Generation: 1, Path: filepath.Join(root, "cold-slot"), State: "READY"}, []SlotRepository{{RepositoryID: "repository", WorktreePath: filepath.Join(root, "cold-slot", "repository"), State: "COLD", RequestedRef: "main", BaseOID: "head", Fingerprint: "fingerprint"}}); err != nil {
		t.Fatal(err)
	}
	session := Session{ID: "cold-session", WorkspaceID: "workspace", SlotID: "cold-slot", State: "ACTIVE", AgentKind: "codex", TokenHash: HashToken("token")}
	job, err := store.LeaseReadyWithCold(ctx, "cold-slot", session)
	if err != nil || job.Kind != "PREPARE" || job.SessionID != session.ID {
		t.Fatalf("cold lease job=%+v err=%v", job, err)
	}
	slot, err := store.Slot(ctx, "cold-slot")
	if err != nil || slot.State != "PREPARING" || slot.OwnerSessionID != session.ID {
		t.Fatalf("cold lease slot=%+v err=%v", slot, err)
	}
	repository, err := store.SlotRepository(ctx, "cold-slot", "repository")
	if err != nil || repository.State != "PREPARING" {
		t.Fatalf("cold lease repository=%+v err=%v", repository, err)
	}
	started, err := store.SessionByID(ctx, session.ID)
	if err != nil || started.State != "STARTING" {
		t.Fatalf("cold lease session=%+v err=%v", started, err)
	}
	rows, err := store.db.QueryContext(ctx, `SELECT repository_id,relative_path,ordinal FROM session_repositories WHERE session_id=?`, session.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	if !rows.Next() {
		t.Fatal("cold lease did not preserve current repository membership")
	}
	var repositoryID, relativePath string
	var ordinal int
	if err := rows.Scan(&repositoryID, &relativePath, &ordinal); err != nil {
		t.Fatal(err)
	}
	if repositoryID != "repository" || relativePath != "" || ordinal != 0 {
		t.Fatalf("cold lease membership=%q %q %d", repositoryID, relativePath, ordinal)
	}
}

func TestStatePersistenceFaultsRemainFailClosed(t *testing.T) {
	t.Run("restore membership copy", func(t *testing.T) {
		store := openTestStore(t)
		ctx := context.Background()
		parent := Session{ID: "parent", SlotID: "parent", State: "ARCHIVED", AgentKind: "codex", TokenHash: HashToken("parent")}
		if _, err := store.CreateSlotSession(ctx, Slot{ID: "parent", Path: "/wx/parent", State: "SNAPSHOTTED"}, nil, parent, ""); err != nil {
			t.Fatal(err)
		}
		if _, err := store.db.Exec(`DROP TABLE session_repositories`); err != nil {
			t.Fatal(err)
		}
		child := Session{ID: "child", SlotID: "child", ParentSessionID: parent.ID, State: "RESTORING", AgentKind: "codex", TokenHash: HashToken("child")}
		if _, err := store.CreateSlotSession(ctx, Slot{ID: "child", Path: "/wx/child", State: "RESTORING"}, nil, child, "RESTORE"); err == nil {
			t.Fatal("restore session succeeded without session repository storage")
		}
	})

	t.Run("current membership copy", func(t *testing.T) {
		store := openTestStore(t)
		seedWorkspace(t, store)
		if _, err := store.db.Exec(`DROP TABLE session_repositories`); err != nil {
			t.Fatal(err)
		}
		session := Session{ID: "current", WorkspaceID: "workspace", SlotID: "current", State: "STARTING", AgentKind: "codex", TokenHash: HashToken("current")}
		if _, err := store.CreateSlotSession(context.Background(), Slot{ID: "current", WorkspaceID: "workspace", Path: "/wx/current", State: "PREPARING"}, nil, session, ""); err == nil {
			t.Fatal("session succeeded without current membership storage")
		}
	})

	t.Run("ready lease membership", func(t *testing.T) {
		store := openTestStore(t)
		seedWorkspace(t, store)
		ctx := context.Background()
		if _, err := store.CreateStandby(ctx, Slot{ID: "ready-lease", WorkspaceID: "workspace", Generation: 1, Path: "/wx/ready-lease", State: "READY"}, []SlotRepository{{RepositoryID: "repository", WorktreePath: "/wx/ready-lease/repository", State: "READY"}}); err != nil {
			t.Fatal(err)
		}
		if _, err := store.db.Exec(`DROP TABLE session_repositories`); err != nil {
			t.Fatal(err)
		}
		session := Session{ID: "ready-session", WorkspaceID: "workspace", SlotID: "ready-lease", State: "ACTIVE", AgentKind: "codex", TokenHash: HashToken("ready")}
		if err := store.LeaseReady(ctx, "ready-lease", session); err == nil {
			t.Fatal("ready lease succeeded without membership storage")
		}
	})

	t.Run("cold lease membership", func(t *testing.T) {
		store := openTestStore(t)
		seedWorkspace(t, store)
		ctx := context.Background()
		if _, err := store.CreateStandby(ctx, Slot{ID: "cold-lease", WorkspaceID: "workspace", Generation: 1, Path: "/wx/cold-lease", State: "READY"}, []SlotRepository{{RepositoryID: "repository", WorktreePath: "/wx/cold-lease/repository", State: "COLD"}}); err != nil {
			t.Fatal(err)
		}
		if _, err := store.db.Exec(`DROP TABLE session_repositories`); err != nil {
			t.Fatal(err)
		}
		session := Session{ID: "cold-session-fault", WorkspaceID: "workspace", SlotID: "cold-lease", State: "ACTIVE", AgentKind: "codex", TokenHash: HashToken("cold")}
		if _, err := store.LeaseReadyWithCold(ctx, "cold-lease", session); err == nil {
			t.Fatal("cold lease succeeded without membership storage")
		}
	})

	t.Run("retry slot update", func(t *testing.T) {
		store := openTestStore(t)
		seedWorkspace(t, store)
		if _, err := store.CreateStandby(context.Background(), Slot{ID: "retry", WorkspaceID: "workspace", Generation: 1, Path: "/wx/retry", State: "FAILED"}, []SlotRepository{{RepositoryID: "repository", WorktreePath: "/wx/retry/repository", State: "PREPARE_RUNNING"}}); err != nil {
			t.Fatal(err)
		}
		if _, err := store.db.Exec(`CREATE TRIGGER fail_retry_slot BEFORE UPDATE OF state ON slots WHEN OLD.id='retry' BEGIN SELECT RAISE(ABORT,'fault'); END`); err != nil {
			t.Fatal(err)
		}
		if err := store.ResetPreparationForRetry(context.Background(), "retry"); err == nil {
			t.Fatal("retry succeeded despite slot transition fault")
		}
	})

	t.Run("retry repository update", func(t *testing.T) {
		store := openTestStore(t)
		seedWorkspace(t, store)
		if _, err := store.CreateStandby(context.Background(), Slot{ID: "retry", WorkspaceID: "workspace", Generation: 1, Path: "/wx/retry", State: "FAILED"}, []SlotRepository{{RepositoryID: "repository", WorktreePath: "/wx/retry/repository", State: "PREPARE_RUNNING"}}); err != nil {
			t.Fatal(err)
		}
		if _, err := store.db.Exec(`DROP TABLE slot_repositories`); err != nil {
			t.Fatal(err)
		}
		if err := store.ResetPreparationForRetry(context.Background(), "retry"); err == nil {
			t.Fatal("retry succeeded without repository metadata")
		}
	})

	t.Run("finish preparation slot transition", func(t *testing.T) {
		store := openTestStore(t)
		seedWorkspace(t, store)
		ctx := context.Background()
		session := Session{ID: "preparing", WorkspaceID: "workspace", SlotID: "preparing", State: "STARTING", AgentKind: "codex", TokenHash: HashToken("preparing")}
		if _, err := store.CreateSlotSession(ctx, Slot{ID: "preparing", WorkspaceID: "workspace", Generation: 1, Path: "/wx/preparing", State: "PREPARING"}, nil, session, ""); err != nil {
			t.Fatal(err)
		}
		if _, err := store.db.Exec(`CREATE TRIGGER fail_finish_slot BEFORE UPDATE OF state ON slots WHEN OLD.id='preparing' BEGIN SELECT RAISE(ABORT,'fault'); END`); err != nil {
			t.Fatal(err)
		}
		if _, _, err := store.FinishPreparationWithRelease(ctx, "preparing"); err == nil {
			t.Fatal("finish preparation succeeded despite slot transition fault")
		}
	})

	t.Run("finish preparation stale slot", func(t *testing.T) {
		store := openTestStore(t)
		seedWorkspace(t, store)
		ctx := context.Background()
		session := Session{ID: "stale-preparing", WorkspaceID: "workspace", SlotID: "stale-preparing", State: "STARTING", AgentKind: "codex", TokenHash: HashToken("stale")}
		if _, err := store.CreateSlotSession(ctx, Slot{ID: "stale-preparing", WorkspaceID: "workspace", Generation: 1, Path: "/wx/stale-preparing", State: "FAILED"}, nil, session, ""); err != nil {
			t.Fatal(err)
		}
		if _, _, err := store.FinishPreparationWithRelease(ctx, "stale-preparing"); err == nil {
			t.Fatal("finish preparation accepted a non-preparing slot")
		}
	})

	t.Run("finish restore pending mapping", func(t *testing.T) {
		store := openTestStore(t)
		seedWorkspace(t, store)
		ctx := context.Background()
		parent := Session{ID: "restore-parent", WorkspaceID: "workspace", SlotID: "restore-parent", State: "EXPIRED", AgentKind: "codex", AgentSessionID: "agent", TokenHash: HashToken("parent")}
		if _, err := store.CreateSlotSession(ctx, Slot{ID: parent.ID, WorkspaceID: "workspace", Generation: 1, Path: "/wx/restore-parent", State: "SNAPSHOTTED"}, nil, parent, ""); err != nil {
			t.Fatal(err)
		}
		if _, err := store.db.Exec(`UPDATE sessions SET agent_session_id='agent' WHERE id=?`, parent.ID); err != nil {
			t.Fatal(err)
		}
		child := Session{ID: "restore-child", WorkspaceID: "workspace", SlotID: "restore-child", ParentSessionID: parent.ID, State: "RESTORING", AgentKind: "codex", TokenHash: HashToken("child")}
		if _, err := store.CreateSlotSession(ctx, Slot{ID: child.ID, WorkspaceID: "workspace", Generation: 1, Path: "/wx/restore-child", State: "RESTORING"}, nil, child, ""); err != nil {
			t.Fatal(err)
		}
		if _, err := store.db.Exec(`UPDATE sessions SET pending_agent_session_id='agent' WHERE id='restore-child'`); err != nil {
			t.Fatal(err)
		}
		if _, err := store.db.Exec(`CREATE TRIGGER ignore_restore_activation BEFORE UPDATE ON sessions WHEN OLD.id='restore-child' AND NEW.pending_agent_session_id IS NULL BEGIN SELECT RAISE(IGNORE); END`); err != nil {
			t.Fatal(err)
		}
		if _, _, err := store.FinishPreparationWithRelease(ctx, child.SlotID); err == nil {
			t.Fatal("restore activation succeeded after its mapping changed")
		}
	})

	t.Run("finish release job insert", func(t *testing.T) {
		store := openTestStore(t)
		seedWorkspace(t, store)
		ctx := context.Background()
		session := Session{ID: "release-finish", WorkspaceID: "workspace", SlotID: "release-finish", State: "RELEASING", AgentKind: "codex", TokenHash: HashToken("release")}
		if _, err := store.CreateSlotSession(ctx, Slot{ID: session.SlotID, WorkspaceID: "workspace", Generation: 1, Path: "/wx/release-finish", State: "PREPARING"}, nil, session, ""); err != nil {
			t.Fatal(err)
		}
		if _, err := store.db.Exec(`CREATE TRIGGER fail_finish_job BEFORE INSERT ON jobs WHEN NEW.kind='SNAPSHOT' BEGIN SELECT RAISE(ABORT,'fault'); END`); err != nil {
			t.Fatal(err)
		}
		if _, _, err := store.FinishPreparationWithRelease(ctx, session.SlotID); err == nil {
			t.Fatal("release finish succeeded despite snapshot job fault")
		}
	})

	t.Run("release job insert", func(t *testing.T) {
		store := openTestStore(t)
		seedWorkspace(t, store)
		ctx := context.Background()
		session := Session{ID: "release-job", WorkspaceID: "workspace", SlotID: "release-job", State: "ACTIVE", AgentKind: "codex", TokenHash: HashToken("release")}
		if _, err := store.CreateSlotSession(ctx, Slot{ID: session.SlotID, WorkspaceID: "workspace", Generation: 1, Path: "/wx/release-job", State: "LEASED"}, nil, session, ""); err != nil {
			t.Fatal(err)
		}
		if _, err := store.db.Exec(`CREATE TRIGGER fail_release_job BEFORE INSERT ON jobs WHEN NEW.kind='SNAPSHOT' BEGIN SELECT RAISE(ABORT,'fault'); END`); err != nil {
			t.Fatal(err)
		}
		if _, _, err := store.Release(ctx, session.ID, session.WorkspaceID, session.SlotID); err == nil {
			t.Fatal("release succeeded despite snapshot job fault")
		}
	})

	t.Run("agent parent mapping update", func(t *testing.T) {
		store := openTestStore(t)
		ctx := context.Background()
		parent := Session{ID: "agent-parent", SlotID: "agent-parent", State: "EXPIRED", AgentKind: "codex", AgentSessionID: "agent", TokenHash: HashToken("parent")}
		if _, err := store.CreateSlotSession(ctx, Slot{ID: parent.ID, Path: "/wx/agent-parent", State: "SNAPSHOTTED"}, nil, parent, ""); err != nil {
			t.Fatal(err)
		}
		if _, err := store.db.Exec(`UPDATE sessions SET agent_session_id='agent' WHERE id=?`, parent.ID); err != nil {
			t.Fatal(err)
		}
		child := Session{ID: "agent-child", SlotID: "agent-child", ParentSessionID: parent.ID, State: "STARTING", AgentKind: "codex", TokenHash: HashToken("child")}
		if _, err := store.CreateSlotSession(ctx, Slot{ID: child.ID, Path: "/wx/agent-child", State: "UNBOUND"}, nil, child, ""); err != nil {
			t.Fatal(err)
		}
		if _, err := store.db.Exec(`CREATE TRIGGER fail_agent_parent BEFORE UPDATE ON sessions WHEN OLD.id='agent-parent' BEGIN SELECT RAISE(ABORT,'fault'); END`); err != nil {
			t.Fatal(err)
		}
		if err := store.BindAgentSession(ctx, child.ID, "agent"); err == nil {
			t.Fatal("agent binding succeeded despite parent mapping fault")
		}
	})

	t.Run("agent session update", func(t *testing.T) {
		store := openTestStore(t)
		ctx := context.Background()
		session := Session{ID: "agent-current", SlotID: "agent-current", State: "STARTING", AgentKind: "codex", TokenHash: HashToken("current")}
		if _, err := store.CreateSlotSession(ctx, Slot{ID: session.ID, Path: "/wx/agent-current", State: "UNBOUND"}, nil, session, ""); err != nil {
			t.Fatal(err)
		}
		if _, err := store.db.Exec(`CREATE TRIGGER fail_agent_current BEFORE UPDATE ON sessions WHEN OLD.id='agent-current' BEGIN SELECT RAISE(ABORT,'fault'); END`); err != nil {
			t.Fatal(err)
		}
		if err := store.BindAgentSession(ctx, session.ID, "agent"); err == nil {
			t.Fatal("agent binding succeeded despite session mapping fault")
		}
	})

	t.Run("heartbeat update", func(t *testing.T) {
		store := openTestStore(t)
		ctx := context.Background()
		session := Session{ID: "heartbeat", SlotID: "heartbeat", State: "STARTING", AgentKind: "codex", TokenHash: HashToken("token")}
		if _, err := store.CreateSlotSession(ctx, Slot{ID: session.ID, Path: "/wx/heartbeat", State: "UNBOUND"}, nil, session, ""); err != nil {
			t.Fatal(err)
		}
		if _, err := store.db.Exec(`CREATE TRIGGER fail_heartbeat BEFORE UPDATE ON sessions WHEN OLD.id='heartbeat' BEGIN SELECT RAISE(ABORT,'fault'); END`); err != nil {
			t.Fatal(err)
		}
		if err := store.Heartbeat(ctx, session.ID, "token"); err == nil {
			t.Fatal("heartbeat succeeded despite durable update fault")
		}
	})
}

func TestStateLifecycleFaultsRemainFailClosed(t *testing.T) {
	newFreshResumeState := func(t *testing.T) (*Store, context.Context) {
		t.Helper()
		store := openTestStore(t)
		seedWorkspace(t, store)
		ctx := context.Background()
		parent := Session{ID: "resume-parent", WorkspaceID: "workspace", SlotID: "resume-parent", State: "EXPIRED", AgentKind: "codex", TokenHash: HashToken("parent")}
		if _, err := store.CreateSlotSession(ctx, Slot{ID: parent.ID, WorkspaceID: "workspace", Generation: 1, Path: "/wx/resume-parent", State: "SNAPSHOTTED"}, nil, parent, ""); err != nil {
			t.Fatal(err)
		}
		if _, err := store.db.Exec(`UPDATE sessions SET agent_session_id='agent' WHERE id=?`, parent.ID); err != nil {
			t.Fatal(err)
		}
		current := Session{ID: "resume-current", SlotID: "resume-current", State: "UNBOUND", AgentKind: "codex", TokenHash: HashToken("current")}
		if _, err := store.CreateSlotSession(ctx, Slot{ID: current.ID, Path: "/wx/resume-current", State: "UNBOUND"}, nil, current, ""); err != nil {
			t.Fatal(err)
		}
		return store, ctx
	}

	t.Run("fresh resume parent update", func(t *testing.T) {
		store, ctx := newFreshResumeState(t)
		if _, err := store.db.Exec(`CREATE TRIGGER fail_fresh_parent BEFORE UPDATE ON sessions WHEN OLD.id='resume-parent' BEGIN SELECT RAISE(ABORT,'fault'); END`); err != nil {
			t.Fatal(err)
		}
		if _, err := store.BindFreshResumeSlot(ctx, "resume-current", "resume-parent", "workspace", "agent", 1, nil); err == nil {
			t.Fatal("fresh resume succeeded despite parent mapping fault")
		}
	})

	t.Run("fresh resume session update", func(t *testing.T) {
		store, ctx := newFreshResumeState(t)
		if _, err := store.db.Exec(`CREATE TRIGGER fail_fresh_session BEFORE UPDATE ON sessions WHEN OLD.id='resume-current' BEGIN SELECT RAISE(ABORT,'fault'); END`); err != nil {
			t.Fatal(err)
		}
		if _, err := store.BindFreshResumeSlot(ctx, "resume-current", "resume-parent", "workspace", "agent", 1, nil); err == nil {
			t.Fatal("fresh resume succeeded despite session mapping fault")
		}
	})

	t.Run("fresh resume slot update", func(t *testing.T) {
		store, ctx := newFreshResumeState(t)
		if _, err := store.db.Exec(`CREATE TRIGGER fail_fresh_slot BEFORE UPDATE ON slots WHEN OLD.id='resume-current' BEGIN SELECT RAISE(ABORT,'fault'); END`); err != nil {
			t.Fatal(err)
		}
		if _, err := store.BindFreshResumeSlot(ctx, "resume-current", "resume-parent", "workspace", "agent", 1, nil); err == nil {
			t.Fatal("fresh resume succeeded despite slot transition fault")
		}
	})

	t.Run("fresh resume membership", func(t *testing.T) {
		store, ctx := newFreshResumeState(t)
		if _, err := store.db.Exec(`DROP TABLE session_repositories`); err != nil {
			t.Fatal(err)
		}
		if _, err := store.BindFreshResumeSlot(ctx, "resume-current", "resume-parent", "workspace", "agent", 1, nil); err == nil {
			t.Fatal("fresh resume succeeded without membership storage")
		}
	})

	t.Run("resume membership copy", func(t *testing.T) {
		store, ctx := newFreshResumeState(t)
		if _, err := store.db.Exec(`DROP TABLE session_repositories`); err != nil {
			t.Fatal(err)
		}
		if _, err := store.BindResumeSlot(ctx, "resume-current", "resume-parent", "workspace", "agent", 1, nil); err == nil {
			t.Fatal("resume succeeded without membership storage")
		}
	})

	t.Run("restoring repository count", func(t *testing.T) {
		store := openTestStore(t)
		ctx := context.Background()
		if _, err := store.CreateSlotSession(ctx, Slot{ID: "restoring", Path: "/wx/restoring", State: "RESTORING"}, nil, Session{ID: "restoring", SlotID: "restoring", State: "RESTORING", AgentKind: "codex", TokenHash: HashToken("restoring")}, ""); err != nil {
			t.Fatal(err)
		}
		if _, err := store.db.Exec(`DROP TABLE slot_repositories`); err != nil {
			t.Fatal(err)
		}
		if err := store.AddRestoringRepositories(ctx, "restoring", []SlotRepository{{RepositoryID: "missing", WorktreePath: "/wx/restoring/missing"}}); err == nil {
			t.Fatal("restoring repository metadata succeeded without storage")
		}
	})

	t.Run("restoring repository insert", func(t *testing.T) {
		store := openTestStore(t)
		ctx := context.Background()
		if _, err := store.CreateSlotSession(ctx, Slot{ID: "restoring", Path: "/wx/restoring", State: "RESTORING"}, nil, Session{ID: "restoring", SlotID: "restoring", State: "RESTORING", AgentKind: "codex", TokenHash: HashToken("restoring")}, ""); err != nil {
			t.Fatal(err)
		}
		if _, err := store.db.Exec(`CREATE TRIGGER fail_restoring_repo BEFORE INSERT ON slot_repositories BEGIN SELECT RAISE(ABORT,'fault'); END`); err != nil {
			t.Fatal(err)
		}
		if err := store.AddRestoringRepositories(ctx, "restoring", []SlotRepository{{RepositoryID: "missing", WorktreePath: "/wx/restoring/missing"}}); err == nil {
			t.Fatal("restoring repository metadata succeeded despite insert fault")
		}
	})

	t.Run("snapshot metadata read", func(t *testing.T) {
		store := openTestStore(t)
		ctx := context.Background()
		seedWorkspace(t, store)
		session := Session{ID: "snapshot-read", WorkspaceID: "workspace", SlotID: "snapshot-read", State: "ACTIVE", AgentKind: "codex", TokenHash: HashToken("snapshot")}
		if _, err := store.CreateSlotSession(ctx, Slot{ID: session.SlotID, WorkspaceID: "workspace", Generation: 1, Path: "/wx/snapshot-read", State: "LEASED"}, nil, session, ""); err != nil {
			t.Fatal(err)
		}
		if _, err := store.db.Exec(`CREATE TRIGGER ignore_snapshot_insert BEFORE INSERT ON snapshots BEGIN SELECT RAISE(IGNORE); END`); err != nil {
			t.Fatal(err)
		}
		snapshot := Snapshot{ID: "snapshot-read", SessionID: session.ID, RepositoryID: "repository", HeadOID: "head", HeadRef: "refs/wx/head", IndexTreeOID: "index", WorktreeOID: "tree", WorktreeRef: "refs/wx/tree", Status: "ARCHIVED", CreatedAt: now(), ExpiresAt: now()}
		if err := store.SaveSnapshot(ctx, snapshot); err == nil {
			t.Fatal("snapshot save accepted an ignored durable insert")
		}
	})

	t.Run("workspace snapshot metadata read", func(t *testing.T) {
		store := openTestStore(t)
		ctx := context.Background()
		if _, err := store.CreateSlotSession(ctx, Slot{ID: "workspace-snapshot", Path: "/wx/workspace-snapshot", State: "ARCHIVED"}, nil, Session{ID: "workspace-snapshot", SlotID: "workspace-snapshot", State: "EXPIRED", AgentKind: "codex", TokenHash: HashToken("snapshot")}, ""); err != nil {
			t.Fatal(err)
		}
		if _, err := store.db.Exec(`CREATE TRIGGER ignore_workspace_snapshot_insert BEFORE INSERT ON workspace_snapshots BEGIN SELECT RAISE(IGNORE); END`); err != nil {
			t.Fatal(err)
		}
		snapshot := WorkspaceSnapshot{SessionID: "workspace-snapshot", ArchivePath: "/wx/snapshot.tar", SHA256: strings.Repeat("a", 64), Status: "ARCHIVED", CreatedAt: now(), ExpiresAt: now()}
		if err := store.SaveWorkspaceSnapshot(ctx, snapshot); err == nil {
			t.Fatal("workspace snapshot save accepted an ignored durable insert")
		}
	})
}

func TestForgetWorkspaceStopsAtEveryDurableBoundary(t *testing.T) {
	for _, table := range []string{"slots", "sessions", "snapshots", "workspace_snapshots", "jobs"} {
		t.Run("query "+table, func(t *testing.T) {
			store := openTestStore(t)
			seedWorkspace(t, store)
			if _, err := store.db.Exec(`DROP TABLE ` + table); err != nil {
				t.Fatal(err)
			}
			if err := store.ForgetWorkspace(context.Background(), "/workspace"); err == nil {
				t.Fatalf("forget succeeded without %s", table)
			}
		})
	}

	newArchivedWorkspace := func(t *testing.T) *Store {
		t.Helper()
		store := openTestStore(t)
		seedWorkspace(t, store)
		ctx := context.Background()
		session := Session{ID: "archived", WorkspaceID: "workspace", SlotID: "archived", State: "EXPIRED", AgentKind: "codex", TokenHash: HashToken("archived")}
		if _, err := store.CreateSlotSession(ctx, Slot{ID: session.SlotID, WorkspaceID: "workspace", Generation: 1, Path: "/wx/archived", State: "ARCHIVED"}, nil, session, ""); err != nil {
			t.Fatal(err)
		}
		return store
	}
	for _, test := range []struct {
		name    string
		trigger string
	}{
		{name: "slot update", trigger: `CREATE TRIGGER fail_forget_slot BEFORE UPDATE ON slots BEGIN SELECT RAISE(ABORT,'fault'); END`},
		{name: "session update", trigger: `CREATE TRIGGER fail_forget_session BEFORE UPDATE ON sessions BEGIN SELECT RAISE(ABORT,'fault'); END`},
		{name: "job update", trigger: `CREATE TRIGGER fail_forget_job BEFORE UPDATE ON jobs BEGIN SELECT RAISE(ABORT,'fault'); END`},
		{name: "repository membership delete", trigger: `CREATE TRIGGER fail_forget_membership BEFORE DELETE ON workspace_repositories BEGIN SELECT RAISE(ABORT,'fault'); END`},
		{name: "workspace delete", trigger: `CREATE TRIGGER fail_forget_workspace BEFORE DELETE ON workspaces BEGIN SELECT RAISE(ABORT,'fault'); END`},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := newArchivedWorkspace(t)
			if test.name == "job update" {
				if _, err := store.db.Exec(`INSERT INTO jobs(id,kind,state,attempt,not_before) VALUES('finished','PREPARE','SUCCEEDED',0,NULL)`); err != nil {
					t.Fatal(err)
				}
				if _, err := store.db.Exec(`UPDATE jobs SET workspace_id='workspace' WHERE id='finished'`); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := store.db.Exec(test.trigger); err != nil {
				t.Fatal(err)
			}
			if err := store.ForgetWorkspace(context.Background(), "/workspace"); err == nil {
				t.Fatal("forget succeeded despite durable cleanup fault")
			}
		})
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
	if _, err := store.CreateSlotSession(ctx, Slot{ID: session.SlotID, WorkspaceID: "workspace", Generation: 1, Path: "/wx/quarantine", State: "LEASED"}, nil, session, ""); err != nil {
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

func TestStatusDiagnosticsAndGarbageCollectionCandidatesExposeRows(t *testing.T) {
	store := openTestStore(t)
	seedWorkspace(t, store)
	ctx := context.Background()
	root := t.TempDir()
	session := Session{ID: "snapshot", WorkspaceID: "workspace", SlotID: "snapshot", State: "ARCHIVED", AgentKind: "codex", TokenHash: HashToken("token")}
	if _, err := store.CreateSlotSession(ctx, Slot{ID: "snapshot", WorkspaceID: "workspace", Generation: 1, Path: filepath.Join(root, "snapshot"), State: "SNAPSHOTTED"}, nil, session, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE sessions SET archived_at=? WHERE id=?`, FormatTime(time.Now().Add(-time.Hour)), session.ID); err != nil {
		t.Fatal(err)
	}
	job, err := store.CreateJob(ctx, "PREPARE", "workspace", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.QuarantineArtifact(ctx, "test", filepath.Join(root, "artifact"), "TEST"); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveSnapshot(ctx, Snapshot{ID: "snapshot-row", SessionID: session.ID, RepositoryID: "repository", HeadOID: "head", HeadRef: "refs/wx/recovery/head", IndexTreeOID: "index", WorktreeOID: "tree", WorktreeRef: "refs/wx/recovery/worktree", Status: "ARCHIVED", CreatedAt: now(), ExpiresAt: FormatTime(time.Now().Add(time.Hour))}); err != nil {
		t.Fatal(err)
	}
	diagnostics, err := store.StatusDiagnostics(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(diagnostics.Workspaces) != 1 || len(diagnostics.Sessions) != 1 || len(diagnostics.Repositories) != 1 || diagnostics.Jobs.Pending < 1 || diagnostics.Snapshots.Count != 1 || len(diagnostics.Quarantine) != 1 {
		t.Fatalf("diagnostics=%+v", diagnostics)
	}
	if jobs, err := store.RecoverJobs(ctx, true); err != nil || len(jobs) == 0 || jobs[0].ID != job.ID {
		t.Fatalf("reclaimed jobs=%+v err=%v", jobs, err)
	}
	candidates, err := store.GCCandidates(ctx, FormatTime(time.Now().Add(time.Hour)))
	if err != nil || len(candidates) != 1 || candidates[0].SlotID != "snapshot" || candidates[0].SessionID != session.ID {
		t.Fatalf("GC candidates=%+v err=%v", candidates, err)
	}
}

func TestStoreMutationsFailClosedWhenContextIsCanceled(t *testing.T) {
	store := openTestStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	workspace := discovery.Workspace{ID: "canceled", Root: "/canceled", Kind: "repository", Repositories: []discovery.Repository{{ID: "repository", CommonDir: "/canceled/repository.git"}}}
	session := Session{ID: "canceled", WorkspaceID: "canceled", SlotID: "canceled", State: "STARTING", AgentKind: "codex"}
	job := Job{ID: "canceled", Kind: "PREPARE", State: "PENDING"}
	operations := map[string]func() error{
		"ping": func() error { return store.Ping(ctx) },
		"begin RPC": func() error {
			_, _, _, _, err := store.BeginRPCRequest(ctx, "canceled", "method", `{}`, time.Now().Add(time.Hour))
			return err
		},
		"complete RPC": func() error {
			return store.CompleteRPCRequest(ctx, "canceled", "method", `{}`, nil, "", "", time.Now().Add(time.Hour))
		},
		"backup":               func() error { _, err := store.Backup(ctx, 1, time.Hour); return err },
		"canonical workspace":  func() error { _, err := store.CanonicalWorkspace(ctx, workspace); return err },
		"upsert workspace":     func() error { return store.UpsertWorkspace(ctx, workspace) },
		"upsert generation":    func() error { _, err := store.UpsertWorkspaceGeneration(ctx, workspace); return err },
		"create job":           func() error { _, err := store.CreateJob(ctx, job.Kind, "", "", ""); return err },
		"claim job":            func() error { _, err := store.ClaimJob(ctx, job.ID, "owner"); return err },
		"renew job":            func() error { return store.RenewJob(ctx, job.ID, "owner") },
		"finish job":           func() error { return store.FinishJob(ctx, job.ID, "owner", nil) },
		"retry job":            func() error { return store.RetryJob(ctx, job.ID, "owner", time.Second, "CANCELED") },
		"defer job":            func() error { return store.DeferJob(ctx, job.ID, "owner", time.Second, "CANCELED") },
		"recover jobs":         func() error { _, err := store.RecoverJobs(ctx, false); return err },
		"ensure recovery jobs": func() error { _, err := store.EnsureRecoveryJobs(ctx); return err },
		"ready slot":           func() error { _, _, err := store.ReadySlot(ctx, "canceled"); return err },
		"ready slots":          func() error { _, err := store.ReadySlots(ctx, "canceled"); return err },
		"create slot session": func() error {
			_, err := store.CreateSlotSession(ctx, Slot{ID: session.SlotID, Path: "/canceled/slot", State: "PREPARING"}, nil, session, "")
			return err
		},
		"create standby": func() error {
			_, err := store.CreateStandby(ctx, Slot{ID: "canceled", Path: "/canceled/slot", State: "PREPARING"}, nil)
			return err
		},
		"lease ready":           func() error { return store.LeaseReady(ctx, "canceled", session) },
		"lease ready with cold": func() error { _, err := store.LeaseReadyWithCold(ctx, "canceled", session); return err },
		"record lease":          func() error { return store.RecordLease(ctx, "canceled") },
		"set slot state":        func() error { return store.SetSlotState(ctx, "canceled", []string{"READY"}, "FAILED", "CANCELED") },
		"reset preparation":     func() error { return store.ResetPreparationForRetry(ctx, "canceled") },
		"mark ready":            func() error { return store.MarkReady(ctx, "canceled") },
		"finish preparation":    func() error { _, _, err := store.FinishPreparationWithRelease(ctx, "canceled"); return err },
		"mark session state":    func() error { return store.MarkSessionState(ctx, "canceled", []string{"STARTING"}, "ACTIVE") },
		"session":               func() error { _, err := store.Session(ctx, "canceled", "token"); return err },
		"session by ID":         func() error { _, err := store.SessionByID(ctx, "canceled"); return err },
		"register agent":        func() error { return store.RegisterAgentProcess(ctx, "canceled", "token", 1) },
		"bind agent":            func() error { return store.BindAgentSession(ctx, "canceled", "agent") },
		"bind fresh":            func() error { return store.BindFreshSession(ctx, "canceled", "", "agent") },
		"bind fresh resume": func() error {
			_, err := store.BindFreshResumeSlot(ctx, "canceled", "", "workspace", "agent", 1, nil)
			return err
		},
		"find agent": func() error { _, err := store.FindByAgentSession(ctx, "codex", "agent"); return err },
		"heartbeat":  func() error { return store.Heartbeat(ctx, "canceled", "token") },
		"orphans":    func() error { _, err := store.OrphanCandidates(ctx, now()); return err },
		"bind resume": func() error {
			_, err := store.BindResumeSlot(ctx, "canceled", "parent", "workspace", "agent", 1, nil)
			return err
		},
		"slot repositories":          func() error { _, err := store.SlotRepositories(ctx, "canceled"); return err },
		"add restoring repositories": func() error { return store.AddRestoringRepositories(ctx, "canceled", nil) },
		"slot repository":            func() error { _, err := store.SlotRepository(ctx, "canceled", "repository"); return err },
		"set slot repository": func() error {
			return store.SetSlotRepositoryState(ctx, "canceled", "repository", []string{"READY"}, "COLD")
		},
		"slot": func() error { _, err := store.Slot(ctx, "canceled"); return err },
		"save snapshot": func() error {
			return store.SaveSnapshot(ctx, Snapshot{ID: "canceled", SessionID: "canceled", RepositoryID: "repository"})
		},
		"snapshots":               func() error { _, err := store.Snapshots(ctx, "canceled"); return err },
		"save workspace snapshot": func() error { return store.SaveWorkspaceSnapshot(ctx, WorkspaceSnapshot{SessionID: "canceled"}) },
		"workspace snapshot":      func() error { _, _, err := store.WorkspaceSnapshot(ctx, "canceled"); return err },
		"repository":              func() error { _, err := store.Repository(ctx, "repository"); return err },
		"workspace":               func() error { _, err := store.Workspace(ctx, "canceled"); return err },
		"workspace by root":       func() error { _, err := store.WorkspaceByRoot(ctx, "/canceled"); return err },
		"session workspace":       func() error { _, err := store.SessionWorkspace(ctx, "canceled"); return err },
		"workspace roots":         func() error { _, err := store.WorkspaceRoots(ctx); return err },
		"list sessions":           func() error { _, err := store.ListSessions(ctx, true); return err },
		"forget workspace":        func() error { return store.ForgetWorkspace(ctx, "/canceled") },
		"status":                  func() error { _, err := store.Status(ctx); return err },
		"status diagnostics":      func() error { _, err := store.StatusDiagnostics(ctx); return err },
		"mark archived":           func() error { return store.MarkArchived(ctx, "canceled", "canceled", now()) },
		"begin snapshot":          func() error { return store.BeginSnapshot(ctx, "canceled", "canceled") },
		"release":                 func() error { _, _, err := store.Release(ctx, "canceled", "", "canceled"); return err },
		"slot artifacts":          func() error { _, err := store.SlotArtifacts(ctx); return err },
		"quarantine slot":         func() error { return store.QuarantineMissingSlot(ctx, "canceled", "CANCELED") },
		"quarantine artifact":     func() error { return store.QuarantineArtifact(ctx, "test", "/canceled", "CANCELED") },
		"quarantine recovery ref": func() error { return store.QuarantineMissingRecoveryRef(ctx, "refs/wx/canceled") },
		"repositories":            func() error { _, err := store.Repositories(ctx); return err },
		"recovery refs":           func() error { _, err := store.RecoveryRefs(ctx, "repository"); return err },
		"cold candidates":         func() error { _, err := store.ColdRepositoryCandidates(ctx, now()); return err },
		"schedule cold removal": func() error {
			_, _, err := store.ScheduleColdRepositoryRemoval(ctx, ColdRepositoryCandidate{SlotID: "canceled", WorkspaceID: "canceled", RepositoryID: "repository"})
			return err
		},
		"finish cold removal":      func() error { return store.FinishColdRepositoryRemoval(ctx, "canceled", "repository") },
		"standby GC candidates":    func() error { _, err := store.StandbyGCCandidates(ctx, now(), 1); return err },
		"mark standby archived":    func() error { return store.MarkStandbyArchived(ctx, "canceled") },
		"schedule removal":         func() error { _, _, err := store.ScheduleRemoval(ctx, "canceled", "canceled"); return err },
		"finish removal":           func() error { return store.FinishRemoval(ctx, "canceled") },
		"drain root":               func() error { return store.DrainRoot(ctx, "/canceled") },
		"expired snapshots":        func() error { _, err := store.ExpiredSnapshots(ctx, now()); return err },
		"expire session snapshots": func() error { return store.ExpireSessionSnapshots(ctx, "canceled") },
		"prune metadata":           func() error { return store.PruneMetadata(ctx, now(), now(), now()) },
		"GC candidates":            func() error { _, err := store.GCCandidates(ctx, now()); return err },
		"mark slot archived":       func() error { return store.MarkSlotArchived(ctx, "canceled") },
	}
	for name, operation := range operations {
		if err := operation(); err == nil {
			t.Errorf("%s ignored canceled context", name)
		}
	}
}

func TestPreparationRetryAndOwnerStateBranches(t *testing.T) {
	ctx := context.Background()
	t.Run("retry failed preparation", func(t *testing.T) {
		store := openTestStore(t)
		seedWorkspace(t, store)
		root := t.TempDir()
		if _, err := store.CreateStandby(ctx, Slot{ID: "retry", WorkspaceID: "workspace", Generation: 1, Path: filepath.Join(root, "retry"), State: "FAILED"}, []SlotRepository{{RepositoryID: "repository", WorktreePath: filepath.Join(root, "retry", "repository"), State: "PREPARE_RUNNING"}}); err != nil {
			t.Fatal(err)
		}
		if err := store.ResetPreparationForRetry(ctx, "retry"); err != nil {
			t.Fatal(err)
		}
		slot, err := store.Slot(ctx, "retry")
		if err != nil || slot.State != "PREPARING" {
			t.Fatalf("retried slot=%+v err=%v", slot, err)
		}
		repository, err := store.SlotRepository(ctx, "retry", "repository")
		if err != nil || repository.State != "PREPARING" {
			t.Fatalf("retried repository=%+v err=%v", repository, err)
		}
	})

	t.Run("finish unowned preparation", func(t *testing.T) {
		store := openTestStore(t)
		seedWorkspace(t, store)
		if _, err := store.CreateStandby(ctx, Slot{ID: "unowned", WorkspaceID: "workspace", Generation: 1, Path: filepath.Join(t.TempDir(), "unowned"), State: "PREPARING"}, nil); err != nil {
			t.Fatal(err)
		}
		job, scheduled, err := store.FinishPreparationWithRelease(ctx, "unowned")
		if err != nil || scheduled || job.ID != "" {
			t.Fatalf("unowned finish job=%+v scheduled=%v err=%v", job, scheduled, err)
		}
		slot, err := store.Slot(ctx, "unowned")
		if err != nil || slot.State != "READY" {
			t.Fatalf("unowned slot=%+v err=%v", slot, err)
		}
	})

	t.Run("finish starting owner", func(t *testing.T) {
		store := openTestStore(t)
		seedWorkspace(t, store)
		session := Session{ID: "starting", WorkspaceID: "workspace", SlotID: "starting", State: "STARTING", AgentKind: "codex", TokenHash: HashToken("token")}
		if _, err := store.CreateSlotSession(ctx, Slot{ID: "starting", WorkspaceID: "workspace", Generation: 1, Path: filepath.Join(t.TempDir(), "starting"), State: "PREPARING"}, nil, session, "PREPARE"); err != nil {
			t.Fatal(err)
		}
		job, scheduled, err := store.FinishPreparationWithRelease(ctx, "starting")
		if err != nil || scheduled || job.ID != "" {
			t.Fatalf("starting finish job=%+v scheduled=%v err=%v", job, scheduled, err)
		}
		slot, err := store.Slot(ctx, "starting")
		if err != nil || slot.State != "LEASED" {
			t.Fatalf("starting slot=%+v err=%v", slot, err)
		}
		stored, err := store.SessionByID(ctx, session.ID)
		if err != nil || stored.State != "ACTIVE" {
			t.Fatalf("starting session=%+v err=%v", stored, err)
		}
	})

	t.Run("finish restoring without pending mapping", func(t *testing.T) {
		store := openTestStore(t)
		seedWorkspace(t, store)
		session := Session{ID: "restoring", WorkspaceID: "workspace", SlotID: "restoring", State: "RESTORING", AgentKind: "codex", TokenHash: HashToken("token")}
		if _, err := store.CreateSlotSession(ctx, Slot{ID: "restoring", WorkspaceID: "workspace", Generation: 1, Path: filepath.Join(t.TempDir(), "restoring"), State: "RESTORING"}, nil, session, "RESTORE"); err != nil {
			t.Fatal(err)
		}
		job, scheduled, err := store.FinishPreparationWithRelease(ctx, "restoring")
		if err != nil || scheduled || job.ID != "" {
			t.Fatalf("restoring finish job=%+v scheduled=%v err=%v", job, scheduled, err)
		}
		slot, err := store.Slot(ctx, "restoring")
		if err != nil || slot.State != "LEASED" {
			t.Fatalf("restoring slot=%+v err=%v", slot, err)
		}
		stored, err := store.SessionByID(ctx, session.ID)
		if err != nil || stored.State != "ACTIVE" {
			t.Fatalf("restoring session=%+v err=%v", stored, err)
		}
	})
}

func TestColdRemovalCompletionAndAdministrativeQueries(t *testing.T) {
	store := openTestStore(t)
	seedWorkspace(t, store)
	ctx := context.Background()
	root := t.TempDir()
	repository := SlotRepository{RepositoryID: "repository", WorktreePath: filepath.Join(root, "cold", "repository"), State: "READY", RequestedRef: "main", BaseOID: "head", Fingerprint: "fingerprint"}
	job, err := store.CreateStandby(ctx, Slot{ID: "cold", WorkspaceID: "workspace", Generation: 1, Path: filepath.Join(root, "cold"), State: "READY"}, []SlotRepository{repository})
	if err != nil {
		t.Fatal(err)
	}
	if _, changed, err := store.ScheduleColdRepositoryRemoval(ctx, ColdRepositoryCandidate{SlotID: "cold", WorkspaceID: "workspace", RepositoryID: "repository", WorktreePath: repository.WorktreePath}); err != nil || !changed {
		t.Fatalf("cold removal scheduling changed=%v err=%v", changed, err)
	}
	if err := store.FinishColdRepositoryRemoval(ctx, "cold", "repository"); err != nil {
		t.Fatal(err)
	}
	finished, err := store.SlotRepository(ctx, "cold", "repository")
	if err != nil || finished.State != "COLD" {
		t.Fatalf("finished cold repository=%+v err=%v", finished, err)
	}
	slot, err := store.Slot(ctx, "cold")
	if err != nil || slot.State != "READY" {
		t.Fatalf("finished cold slot=%+v err=%v", slot, err)
	}
	if claimed, err := store.ClaimJob(ctx, job.ID, "worker"); err != nil {
		t.Fatal(err)
	} else if err := store.FinishJob(ctx, claimed.ID, "worker", nil); err != nil {
		t.Fatal(err)
	}

	removal, changed, err := store.ScheduleRemoval(ctx, "cold", "")
	if err != nil || !changed || removal.Kind != "REMOVE" || removal.WorkspaceID != "workspace" {
		t.Fatalf("schedule removal job=%+v changed=%v err=%v", removal, changed, err)
	}
	removed, err := store.Slot(ctx, "cold")
	if err != nil || removed.State != "REMOVING" || removed.OwnerSessionID != "" {
		t.Fatalf("scheduled removal slot=%+v err=%v", removed, err)
	}
	if err := store.FinishRemoval(ctx, "cold"); err != nil {
		t.Fatal(err)
	}
	archived, err := store.Slot(ctx, "cold")
	if err != nil || archived.State != "ARCHIVED" {
		t.Fatalf("finished removal slot=%+v err=%v", archived, err)
	}
}

func TestRestoringRepositoryMetadataAndSnapshotExpiryBoundaries(t *testing.T) {
	store := openTestStore(t)
	seedWorkspace(t, store)
	ctx := context.Background()
	root := t.TempDir()
	if _, err := store.CreateSlotSession(ctx, Slot{ID: "restore-meta", Path: filepath.Join(root, "restore-meta"), State: "RESTORING"}, nil, Session{ID: "restore-meta", SlotID: "restore-meta", State: "RESTORING", AgentKind: "codex", TokenHash: HashToken("token")}, ""); err != nil {
		t.Fatal(err)
	}
	repos := []SlotRepository{{RepositoryID: "repository", WorktreePath: filepath.Join(root, "restore-meta", "repository"), RequestedRef: "main", BaseOID: "head", Fingerprint: "fingerprint"}}
	if err := store.AddRestoringRepositories(ctx, "restore-meta", repos); err != nil {
		t.Fatal(err)
	}
	if err := store.AddRestoringRepositories(ctx, "restore-meta", repos); err != nil {
		t.Fatalf("idempotent restoring metadata: %v", err)
	}
	if err := store.AddRestoringRepositories(ctx, "restore-meta", nil); err == nil {
		t.Fatal("incomplete restoring metadata was accepted")
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE sessions SET state='EXPIRED' WHERE id='restore-meta'`); err != nil {
		t.Fatal(err)
	}
	if err := store.ExpireSessionSnapshots(ctx, "restore-meta"); err != nil {
		t.Fatal(err)
	}
}

func TestReadAndRestoreStateBoundaries(t *testing.T) {
	store := openTestStore(t)
	seedWorkspace(t, store)
	ctx := context.Background()
	if _, found, err := store.ReadySlot(ctx, "missing"); err != nil || found {
		t.Fatalf("missing ready slot=%+v found=%v err=%v", Slot{}, found, err)
	}
	root := t.TempDir()
	parent := Session{ID: "parent-copy", WorkspaceID: "workspace", SlotID: "parent-copy", State: "ARCHIVED", AgentKind: "codex", TokenHash: HashToken("parent")}
	parentRepo := SlotRepository{RepositoryID: "repository", WorktreePath: filepath.Join(root, "parent-copy", "repository"), State: "READY", RequestedRef: "main", BaseOID: "head", Fingerprint: "fingerprint"}
	if _, err := store.CreateSlotSession(ctx, Slot{ID: parent.SlotID, WorkspaceID: "workspace", Generation: 1, Path: filepath.Join(root, parent.SlotID), State: "SNAPSHOTTED"}, []SlotRepository{parentRepo}, parent, ""); err != nil {
		t.Fatal(err)
	}
	child := Session{ID: "child-copy", WorkspaceID: "workspace", SlotID: "child-copy", ParentSessionID: parent.ID, State: "STARTING", AgentKind: "codex", TokenHash: HashToken("child")}
	if _, err := store.CreateSlotSession(ctx, Slot{ID: child.SlotID, WorkspaceID: "workspace", Generation: 1, Path: filepath.Join(root, child.SlotID), State: "PREPARING"}, nil, child, "RESTORE"); err != nil {
		t.Fatal(err)
	}
	if repositories, err := store.SlotRepositories(ctx, child.SlotID); err != nil || len(repositories) != 0 {
		// Restore copies membership into the session table, while slot metadata
		// remains empty until the prepare phase materializes its repositories.
		t.Fatalf("child slot repositories=%v err=%v", repositories, err)
	}
	if err := store.BindAgentSession(ctx, child.ID, "agent-copy"); err != nil {
		t.Fatalf("bind child agent: %v", err)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE sessions SET state='RESTORING',parent_session_id=NULL,pending_agent_session_id='pending' WHERE id='child-copy'`); err != nil {
		t.Fatal(err)
	}
	if err := store.FinishPreparation(ctx, child.SlotID); err == nil || !strings.Contains(err.Error(), "no parent") {
		t.Fatalf("pending mapping without parent error=%v", err)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE sessions SET parent_session_id='parent-copy' WHERE id='child-copy'`); err != nil {
		t.Fatal(err)
	}
	if err := store.FinishPreparation(ctx, child.SlotID); err == nil || !strings.Contains(err.Error(), "mapping changed") {
		t.Fatalf("pending mapping with missing parent error=%v", err)
	}
}

func TestStandbyGCKeepsWarmSlotsAndReportsStaleRows(t *testing.T) {
	store := openTestStore(t)
	seedWorkspace(t, store)
	ctx := context.Background()
	root := t.TempDir()
	for _, id := range []string{"warm-a", "warm-b", "stale"} {
		if _, err := store.CreateStandby(ctx, Slot{ID: id, WorkspaceID: "workspace", Generation: 1, Path: filepath.Join(root, id), State: "READY"}, nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.SetSlotState(ctx, "stale", []string{"READY"}, "STALE", "TEST"); err != nil {
		t.Fatal(err)
	}
	candidates, err := store.StandbyGCCandidates(ctx, now(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 2 {
		t.Fatalf("GC candidates=%+v, want one warm eviction and stale slot", candidates)
	}
	foundStale := false
	for _, candidate := range candidates {
		foundStale = foundStale || candidate.SlotID == "stale"
	}
	if !foundStale {
		t.Fatalf("stale candidate missing from %+v", candidates)
	}
}

func TestReleaseAndForgetRefuseDurableStateThatChangedUnderTheCaller(t *testing.T) {
	ctx := context.Background()

	t.Run("release defers while a slot is preparing", func(t *testing.T) {
		store := openTestStore(t)
		seedWorkspace(t, store)
		session := Session{ID: "changed-release", WorkspaceID: "workspace", SlotID: "changed-release", State: "ACTIVE", AgentKind: "codex", TokenHash: HashToken("token")}
		if _, err := store.CreateSlotSession(ctx, Slot{ID: session.SlotID, WorkspaceID: "workspace", Generation: 1, Path: "/wx/changed-release", State: "PREPARING"}, nil, session, ""); err != nil {
			t.Fatal(err)
		}
		job, changed, err := store.Release(ctx, session.ID, "workspace", session.SlotID)
		if err != nil {
			t.Fatal(err)
		}
		if changed || job.ID != "" {
			t.Fatalf("release scheduled unexpectedly: job=%+v changed=%v", job, changed)
		}
		stored, err := store.SessionByID(ctx, session.ID)
		if err != nil {
			t.Fatal(err)
		}
		if stored.State != "RELEASING" {
			t.Fatalf("release state=%q", stored.State)
		}
	})

	t.Run("release rejects an unexpected slot state", func(t *testing.T) {
		store := openTestStore(t)
		seedWorkspace(t, store)
		session := Session{ID: "changed-release-ready", WorkspaceID: "workspace", SlotID: "changed-release-ready", State: "ACTIVE", AgentKind: "codex", TokenHash: HashToken("token")}
		if _, err := store.CreateSlotSession(ctx, Slot{ID: session.SlotID, WorkspaceID: "workspace", Generation: 1, Path: "/wx/changed-release-ready", State: "READY"}, nil, session, ""); err != nil {
			t.Fatal(err)
		}
		if _, _, err := store.Release(ctx, session.ID, "workspace", session.SlotID); err == nil || !strings.Contains(err.Error(), "cannot be released") {
			t.Fatalf("release accepted unexpected slot state: %v", err)
		}
	})

	t.Run("forget waits for pending jobs", func(t *testing.T) {
		store := openTestStore(t)
		seedWorkspace(t, store)
		if _, err := store.CreateJob(ctx, "REMOVE", "workspace", "", ""); err != nil {
			t.Fatal(err)
		}
		if err := store.ForgetWorkspace(ctx, "/workspace"); err == nil || !strings.Contains(err.Error(), "pending recovery jobs") {
			t.Fatalf("forget ignored pending job: %v", err)
		}
	})
}
