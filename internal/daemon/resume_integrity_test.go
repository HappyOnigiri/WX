package daemon

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/HappyOnigiri/WX/internal/archive"
	"github.com/HappyOnigiri/WX/internal/domain"
	"github.com/HappyOnigiri/WX/internal/state"
	"github.com/HappyOnigiri/WX/internal/workspace"
)

// TestCorruptWorkspaceArchiveFailsInRestoreWorkerNotInResumeStatus は完全性の判定が復元 worker へ集約されたことを確かめる。
// 再開前の問い合わせは archive 本文を読まずに使用可能と答え、破損は worker が専用 code で隔離する。
// この失敗には recovery=unavailable を付けないため、auto_fresh でも新しい worktree へ自動で倒れない。
func TestCorruptWorkspaceArchiveFailsInRestoreWorkerNotInResumeStatus(t *testing.T) {
	t.Parallel()
	ctx, manager, store, workspaceRecord, resolved, _ := managerCoverageFixture(t)
	repository := resolved[0].Repository
	workspaceRecord.Root = domain.CanonicalPath(filepath.Dir(string(repository.MainPath)))
	workspaceRecord, _, err := store.UpsertWorkspaceGeneration(ctx, workspaceRecord)
	if err != nil {
		t.Fatal(err)
	}
	dirName := testDirName(repository, manager.Config())

	parentID := domain.StableID("restore-integrity", "parent")
	parentSlot := testSlot(t, manager, string(workspaceRecord.ID), parentID, 1, "ARCHIVED")
	if err := os.WriteFile(filepath.Join(parentSlot.Path, "workspace-state.txt"), []byte("archived workspace root\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateSlotSession(ctx, parentSlot,
		[]state.SlotRepository{{RepositoryID: string(repository.ID), DirName: dirName, State: "READY", BaseOID: resolved[0].OID}},
		state.Session{ID: parentID, WorkspaceID: string(workspaceRecord.ID), SlotID: parentID, State: "ARCHIVED", AgentKind: "codex", TokenHash: state.HashToken(parentID)}, ""); err != nil {
		t.Fatal(err)
	}
	expiry := time.Now().Add(time.Hour)
	if err := store.SaveSnapshot(ctx, state.Snapshot{ID: "snap", SessionID: parentID, RepositoryID: string(repository.ID), HeadOID: resolved[0].OID, HeadRef: "refs/wx/recovery/head", IndexTreeOID: "index", WorktreeOID: "worktree", WorktreeRef: "refs/wx/recovery/worktree", Status: "ARCHIVED", CreatedAt: state.FormatTime(time.Now()), ExpiresAt: state.FormatTime(expiry)}); err != nil {
		t.Fatal(err)
	}
	owner, releaseOwner, err := manager.rootDescriptor(manager.Config().Storage.WorktreeRoot)
	if err != nil {
		t.Fatal(err)
	}
	rootSnapshot, err := archive.SnapshotWorkspaceAt(ctx, parentSlot.Path, manager.Config().Storage.WorktreeRoot, parentSlot.RootID, owner, parentID, nil, expiry)
	releaseOwner()
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SaveWorkspaceSnapshot(ctx, rootSnapshot); err != nil {
		t.Fatal(err)
	}
	// inode と大きさを保ったまま本文だけを壊す。metadata の検査は通り、checksum だけが一致しなくなる。
	archiveFile, err := os.OpenFile(rootSnapshot.ArchivePath, os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := archiveFile.WriteAt([]byte("corrupted"), 512); err != nil {
		_ = archiveFile.Close()
		t.Fatal(err)
	}
	if err := archiveFile.Close(); err != nil {
		t.Fatal(err)
	}

	status, err := manager.ResumeStatus(ctx, parentID)
	if err != nil {
		t.Fatalf("resume status read the archive body: %v", err)
	}
	if status["expired"] != false || status["integrity"] != "not_checked" {
		t.Fatalf("resume status=%v", status)
	}

	childID := domain.StableID("restore-integrity", "child")
	childSlot := testSlot(t, manager, string(workspaceRecord.ID), childID, 1, "RESTORING")
	childWorktree := filepath.Join(childSlot.Path, dirName)
	fingerprint, err := workspace.Fingerprint(1, resolved[0].OID, repository, manager.Config())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateSlotSession(ctx, childSlot,
		[]state.SlotRepository{{RepositoryID: string(repository.ID), DirName: dirName, State: "RESTORING", RequestedRef: "main", BaseOID: resolved[0].OID, Fingerprint: fingerprint}},
		state.Session{ID: childID, WorkspaceID: string(workspaceRecord.ID), SlotID: childID, ParentSessionID: parentID, State: "RESTORING", AgentKind: "codex", TokenHash: state.HashToken(childID)}, ""); err != nil {
		t.Fatal(err)
	}

	restoreErr := manager.restoreSlot(ctx, childID, workspaceRecord, resolved, []state.SlotRepository{{RepositoryID: string(repository.ID)}}, nil)
	if !errors.Is(restoreErr, archive.ErrWorkspaceSnapshotIntegrity) {
		t.Fatalf("restore of a corrupted archive error=%v", restoreErr)
	}
	slot, err := store.Slot(ctx, childID)
	if err != nil || slot.State != "QUARANTINED" || slot.FailureCode != "SNAPSHOT_CORRUPT" {
		t.Fatalf("corrupted restore slot=%+v err=%v", slot, err)
	}
	if recoveryUnavailable(slot.FailureCode) {
		t.Fatal("archive corruption was reported as a recoverable-by-fresh failure")
	}
	// repository の復元は始まっていない。target を変える前に完全性の判定が済む契約を保つ。
	if _, err := os.Lstat(filepath.Join(childWorktree, ".git")); !os.IsNotExist(err) {
		t.Fatalf("repository restore ran before the archive was trusted: %v", err)
	}
	waitCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	readyErr := manager.WaitReady(waitCtx, childID, childID)
	if readyErr == nil || !strings.Contains(readyErr.Error(), "SNAPSHOT_CORRUPT") {
		t.Fatalf("readiness error=%v", readyErr)
	}
	if IsRecoveryUnavailable(readyErr) {
		t.Fatalf("readiness failure carried the fresh-resume marker: %v", readyErr)
	}
}
