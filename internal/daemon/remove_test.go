package daemon

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/HappyOnigiri/WX/internal/archive"
	"github.com/HappyOnigiri/WX/internal/config"
	"github.com/HappyOnigiri/WX/internal/discovery"
	"github.com/HappyOnigiri/WX/internal/domain"
	"github.com/HappyOnigiri/WX/internal/state"
)

func TestRemoveSlotWorktreesWrapsRepositorySnapshotStorageFailure(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	databasePath := filepath.Join(root, "state.db")
	store, err := state.Open(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cfg := config.Defaults()
	cfg.Storage.WorktreeRoot = filepath.Join(root, "worktrees")
	manager := testManager(t, cfg, store)
	defer manager.Close()
	ctx := context.Background()

	w := discovery.Workspace{ID: "workspace", Root: discoveryPath(root), Kind: "repository", Repositories: []discovery.Repository{
		{ID: "repository", MainPath: discoveryPath(filepath.Join(root, "repository")), CommonDir: discoveryPath(filepath.Join(root, "repository", ".git")), DefaultBranch: "main"},
	}}
	registered, _, err := store.UpsertWorkspaceGeneration(ctx, w)
	if err != nil {
		t.Fatal(err)
	}
	workspaceID := string(registered.ID)
	slot := testSlot(t, manager, workspaceID, "slot", 1, "REMOVING")
	sessionID := "session"
	if _, err := store.CreateSlotSession(ctx, slot, nil,
		state.Session{ID: sessionID, WorkspaceID: workspaceID, SlotID: "slot", State: "ARCHIVED", AgentKind: "codex", TokenHash: state.HashToken(sessionID)}, ""); err != nil {
		t.Fatal(err)
	}

	raw := openTestDatabase(t, databasePath)
	if _, err := raw.Exec(`DROP TABLE snapshots`); err != nil {
		t.Fatal(err)
	}

	if err := manager.removeSlotWorktrees(ctx, archive.Manager{}, cfg.Storage.WorktreeRoot, slot, sessionID); err == nil {
		t.Fatal("worktree removal succeeded despite an unreadable snapshot table")
	}
}

func TestRemoveSlotWorktreesRejectsRepositoryOutsideRoot(t *testing.T) {
	t.Parallel()
	ctx, manager, store, workspaceRecord, resolved, _ := managerCoverageFixture(t)
	root := manager.Config().Storage.WorktreeRoot
	slotID := domain.StableID("remove-slot", "outside-repo")
	slot := testSlot(t, manager, string(workspaceRecord.ID), slotID, 1, "REMOVING")
	escaping := filepath.Join("..", "..", "..", "outside-repo")
	if _, err := store.CreateStandby(ctx, slot,
		[]state.SlotRepository{{RepositoryID: string(resolved[0].Repository.ID), DirName: escaping, State: "REMOVING", BaseOID: resolved[0].OID}}); err != nil {
		t.Fatal(err)
	}
	stored, err := store.SlotRepository(ctx, slotID, string(resolved[0].Repository.ID))
	if err != nil {
		t.Fatal(err)
	}
	if domain.IsWithin(root, stored.WorktreePath) {
		t.Fatalf("derived worktree path %s did not escape root %s; the case under test no longer applies", stored.WorktreePath, root)
	}
	if err := manager.removeSlotWorktrees(ctx, archive.Manager{}, root, slot, ""); !errors.Is(err, state.ErrOwnership) {
		t.Fatalf("repository outside root error=%v", err)
	}
}

func TestRemoveSlotWorktreesDeletesReplacedRegisteredDirectory(t *testing.T) {
	t.Parallel()
	ctx, manager, store, workspaceRecord, _, _ := managerCoverageFixture(t)
	root := manager.Config().Storage.WorktreeRoot
	slotID := domain.StableID("remove-slot", "replaced-directory")
	slot := testSlot(t, manager, string(workspaceRecord.ID), slotID, 1, "REMOVING")
	if _, err := store.CreateStandby(ctx, slot, nil); err != nil {
		t.Fatal(err)
	}
	// 置き換え先は元のディレクトリが在るうちに作る。
	// 消してから同じパスへ作り直すと、inodeを再利用するfilesystemでは identity が一致して置き換えを表せない。
	replacement := slot.Path + ".replacement"
	if err := os.MkdirAll(replacement, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(slot.Path); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacement, slot.Path); err != nil {
		t.Fatal(err)
	}
	if err := manager.removeSlotWorktrees(ctx, archive.Manager{}, root, slot, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(slot.Path); !os.IsNotExist(err) {
		t.Fatalf("registered directory remains: %v", err)
	}
}

func TestRemoveEmptySlotDeletesLeafSymlinkWithoutFollowingIt(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	worktreeRoot := filepath.Join(root, "worktrees")
	if err := os.MkdirAll(filepath.Join(worktreeRoot, unboundNamespace), 0o700); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	outsideFile := filepath.Join(outside, "keep")
	if err := os.WriteFile(outsideFile, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	slotPath := filepath.Join(worktreeRoot, unboundNamespace, "slt001")
	if err := os.Symlink(outside, slotPath); err != nil {
		t.Fatal(err)
	}
	store, err := state.Open(filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cfg := config.Defaults()
	cfg.Storage.WorktreeRoot = worktreeRoot
	manager := testManager(t, cfg, store)
	t.Cleanup(manager.Close)
	slot := slotAtPath(t, manager, "", "slt001", slotPath, 0, "REMOVING")
	if _, err := store.CreateSlotSession(context.Background(), slot, nil, state.Session{ID: "slt001", SlotID: "slt001", State: "EXPIRED", AgentKind: "codex", TokenHash: state.HashToken("token")}, ""); err != nil {
		t.Fatal(err)
	}
	if err := manager.removeSlotWorktrees(context.Background(), archive.Manager{}, worktreeRoot, slot, ""); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(outsideFile); err != nil || string(data) != "keep" {
		t.Fatalf("outside file changed: data=%q err=%v", data, err)
	}
}
