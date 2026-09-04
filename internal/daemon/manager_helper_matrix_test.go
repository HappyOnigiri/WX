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
	"github.com/HappyOnigiri/WX/internal/config"
	"github.com/HappyOnigiri/WX/internal/discovery"
	"github.com/HappyOnigiri/WX/internal/domain"
	"github.com/HappyOnigiri/WX/internal/state"
	"github.com/HappyOnigiri/WX/internal/workspace"
)

func TestManagerRootOwnershipHelperMatrix(t *testing.T) {
	_, manager, _, workspaceRecord, _, _ := managerCoverageFixture(t)
	root := manager.Config().Storage.WorktreeRoot
	slotRoot := filepath.Join(root, string(workspaceRecord.ID), "slot")
	identity, _, err := manager.createSlotRoot(slotRoot, slotRoot)
	if err != nil {
		t.Fatalf("create slot root: %v", err)
	}
	if identity == "" {
		t.Fatal("created slot root has no identity")
	}
	if manager.rootHandleForPath(slotRoot) == nil || manager.rootHandleForRoot(root) == nil {
		t.Fatal("created root handle is unavailable")
	}
	if ok, err := manager.ownedPathExists(slotRoot); err != nil || !ok {
		t.Fatalf("owned existing path=%v err=%v", ok, err)
	}
	missing := filepath.Join(slotRoot, "missing")
	if ok, err := manager.ownedPathExists(missing); err != nil || ok {
		t.Fatalf("owned missing path=%v err=%v", ok, err)
	}
	if _, err := manager.ownedPathExists(filepath.Join(t.TempDir(), "outside")); !errors.Is(err, state.ErrOwnership) {
		t.Fatalf("outside owned path error=%v", err)
	}

	if err := os.WriteFile(filepath.Join(slotRoot, "payload"), []byte("payload"), 0o600); err != nil {
		t.Fatal(err)
	}
	bytes, allocated, err := manager.rootDirectoryUsage(root)
	if err != nil || bytes == 0 || allocated == 0 {
		t.Fatalf("root usage bytes=%d allocated=%d err=%v", bytes, allocated, err)
	}
	if _, _, err := manager.rootDirectoryUsage(filepath.Join(t.TempDir(), "unknown")); !errors.Is(err, state.ErrOwnership) {
		t.Fatalf("unknown root usage error=%v", err)
	}

	paths, err := manager.ownedRootArtifactPaths(root)
	if err != nil || len(paths) != 1 || filepath.Clean(paths[0]) != filepath.Clean(slotRoot) {
		t.Fatalf("owned artifact paths=%v err=%v", paths, err)
	}
	// A regular file at the workspace level and a symlinked namespace are
	// neither workspaces nor slots, and a regular file inside a workspace
	// namespace is not a slot directory.
	if err := os.WriteFile(filepath.Join(root, "ignored"), []byte("file"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, string(workspaceRecord.ID), "regular"), []byte("file"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(t.TempDir(), filepath.Join(root, "namespace-link")); err != nil {
		t.Fatal(err)
	}
	if paths, err := manager.ownedRootArtifactPaths(root); err != nil || len(paths) != 1 {
		t.Fatalf("unsafe artifact entries changed paths=%v err=%v", paths, err)
	}

	leaseIdentity, err := manager.leaseRootIdentity(slotRoot)
	if err != nil || leaseIdentity != identity {
		t.Fatalf("lease identity=%q want=%q err=%v", leaseIdentity, identity, err)
	}
	if _, err := manager.leaseRootIdentity(filepath.Join(t.TempDir(), "outside")); err == nil {
		t.Fatal("outside lease identity succeeded")
	}
	if _, _, err := manager.createSlotRoot(filepath.Join(t.TempDir(), "outside"), filepath.Join(t.TempDir(), "outside")); err == nil {
		t.Fatal("outside slot root creation succeeded")
	}
	if _, _, err := manager.holdVerifiedRootForPath(filepath.Join(t.TempDir(), "outside")); !errors.Is(err, state.ErrOwnership) {
		t.Fatalf("outside verified root error=%v", err)
	}

	release, err := manager.holdRootForPath(slotRoot)
	if err != nil {
		t.Fatal(err)
	}
	manager.mu.RLock()
	rootHasReferences := manager.rootHasReferencesLocked(root)
	manager.mu.RUnlock()
	if !rootHasReferences {
		t.Fatal("root reference was not retained while held")
	}
	release()

	if got := removalMetadataFailure("metadata", nil); got != nil {
		t.Fatalf("nil metadata failure=%v", got)
	}
	if got := removalMetadataFailure("metadata", state.ErrOwnership); !errors.Is(got, state.ErrOwnership) {
		t.Fatalf("ownership metadata failure=%v", got)
	}
	if got := removalMetadataFailure("metadata", context.Canceled); !errors.Is(got, context.Canceled) {
		t.Fatalf("canceled metadata failure=%v", got)
	}
	if got := removalMetadataFailure("metadata", errors.New("database unavailable")); !errors.Is(got, state.ErrOwnership) || !strings.Contains(got.Error(), "metadata") {
		t.Fatalf("ordinary metadata failure=%v", got)
	}
	if daemonVersion() == "" {
		t.Fatal("daemon version is empty")
	}

	opened, err := os.OpenRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	if adopted, releaseAdopted, err := manager.adoptRoot(root, opened, true); err != nil || adopted == nil {
		t.Fatalf("adopting existing root=%v err=%v", adopted, err)
	} else {
		releaseAdopted()
	}

	foreignPath := filepath.Join(t.TempDir(), "foreign")
	if err := os.Mkdir(foreignPath, 0o700); err != nil {
		t.Fatal(err)
	}
	foreign, err := os.OpenRoot(foreignPath)
	if err != nil {
		t.Fatal(err)
	}
	manager.mu.Lock()
	manager.ensureRootStateLocked()
	manager.rootIdentities[foreignPath] = "different-identity"
	manager.mu.Unlock()
	if _, _, err := manager.adoptRoot(foreignPath, foreign, true); err == nil {
		t.Fatal("adopting a root with a changed inode identity succeeded")
	}

	closingManager := testManager(t, config.Defaults(), nil)
	closingManager.store = nil
	closingManager.mu.Lock()
	closingManager.rootClosing = true
	closingManager.mu.Unlock()
	closingPath := filepath.Join(t.TempDir(), "closing")
	if err := os.Mkdir(closingPath, 0o700); err != nil {
		t.Fatal(err)
	}
	closingRoot, err := os.OpenRoot(closingPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := closingManager.adoptRoot(closingPath, closingRoot, true); !errors.Is(err, errManagerClosed) {
		t.Fatalf("closed manager adoption error=%v", err)
	}
	closingManager.Close()

	retiredPath := filepath.Join(t.TempDir(), "retired")
	if err := os.Mkdir(retiredPath, 0o700); err != nil {
		t.Fatal(err)
	}
	retiredRoot, err := os.OpenRoot(retiredPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, releaseRetired, err := manager.adoptRoot(retiredPath, retiredRoot, false); err != nil {
		t.Fatalf("adopt retired root: %v", err)
	} else {
		releaseRetired()
	}
}

func TestManagerExpiredSnapshotAndColdRemovalBoundaries(t *testing.T) {
	ctx, manager, store, workspaceRecord, _, _ := managerCoverageFixture(t)
	archiveManager := &archive.Manager{Git: manager.git}
	if count := manager.expireWorkspaceSnapshots(ctx, map[string][]state.Snapshot{
		"missing-session": nil,
	}, archiveManager); count != 0 {
		t.Fatalf("missing session expiry count=%d", count)
	}

	multiID := domain.StableID("expiry-matrix", "multi")
	multiSlot := testSlot(t, manager, string(workspaceRecord.ID), multiID, 1, "ARCHIVED")
	repoID := string(workspaceRecord.Repositories[0].ID)
	if _, err := store.CreateSlotSession(ctx, multiSlot,
		[]state.SlotRepository{{RepositoryID: repoID, DirName: "repository", State: "READY", BaseOID: "head"}},
		state.Session{ID: multiID, WorkspaceID: string(workspaceRecord.ID), SlotID: multiID, State: "ARCHIVED", AgentKind: "matrix", TokenHash: state.HashToken(multiID)}, ""); err != nil {
		t.Fatal(err)
	}
	if count := manager.expireWorkspaceSnapshots(ctx, map[string][]state.Snapshot{multiID: nil}, archiveManager); count != 0 {
		t.Fatalf("multi session without root snapshot count=%d", count)
	}

	validID := domain.StableID("expiry-matrix", "workspace-snapshot")
	validSlot := testSlot(t, manager, string(workspaceRecord.ID), validID, 1, "ARCHIVED")
	validPath := validSlot.Path
	if err := os.WriteFile(filepath.Join(validPath, "workspace.txt"), []byte("workspace"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateSlotSession(ctx, validSlot,
		[]state.SlotRepository{{RepositoryID: repoID, DirName: "repository", State: "READY", BaseOID: "head"}},
		state.Session{ID: validID, WorkspaceID: string(workspaceRecord.ID), SlotID: validID, State: "ARCHIVED", AgentKind: "matrix", TokenHash: state.HashToken(validID)}, ""); err != nil {
		t.Fatal(err)
	}
	owner, releaseOwner, err := manager.rootDescriptor(manager.Config().Storage.WorktreeRoot)
	if err != nil {
		t.Fatal(err)
	}
	rootSnapshot, err := archive.SnapshotWorkspaceAt(ctx, validPath, manager.Config().Storage.WorktreeRoot, validSlot.RootID, owner, validID, nil, time.Now().Add(time.Hour))
	releaseOwner()
	if err != nil {
		t.Fatalf("snapshot workspace: %v", err)
	}
	if err := store.SaveWorkspaceSnapshot(ctx, rootSnapshot); err != nil {
		t.Fatal(err)
	}
	if count := manager.expireWorkspaceSnapshots(ctx, map[string][]state.Snapshot{validID: nil}, archiveManager); count != 1 {
		t.Fatalf("valid workspace snapshot expiry count=%d", count)
	}
	// A snapshot recorded at a root-relative path that leaves the root is the
	// remaining way to express an archive wx does not own.
	if err := store.SaveWorkspaceSnapshot(ctx, state.WorkspaceSnapshot{SessionID: multiID, RootID: multiSlot.RootID, RelPath: filepath.Join("..", "outside-archive"), SHA256: "bad", Status: "ARCHIVED", CreatedAt: state.FormatTime(time.Now()), ExpiresAt: state.FormatTime(time.Now().Add(time.Hour))}); err != nil {
		t.Fatal(err)
	}
	if count := manager.expireWorkspaceSnapshots(ctx, map[string][]state.Snapshot{multiID: nil}, archiveManager); count != 0 {
		t.Fatalf("outside archive expiry count=%d", count)
	}

	// A repository-only archived session has no workspace snapshot and can
	// still expire its repository snapshots independently.
	repositorySessionID := domain.StableID("expiry-matrix", "repository")
	repositoryPath := filepath.Join(manager.Config().Storage.WorktreeRoot, "expiry", repositorySessionID, "root")
	if err := os.MkdirAll(repositoryPath, 0o700); err != nil {
		t.Fatal(err)
	}
	repositoryWorkspace := workspaceRecord
	repositoryWorkspace.ID = domain.WorkspaceID(domain.StableID("expiry-matrix", "repository-workspace"))
	repositoryWorkspace.Kind = "repository"
	repositoryWorkspace.Root = domain.CanonicalPath(filepath.Join(t.TempDir(), "repository-workspace"))
	repositoryWorkspace = registerTestWorkspace(t, store, repositoryWorkspace)
	if _, err := store.CreateSlotSession(ctx,
		slotAtPath(t, manager, string(repositoryWorkspace.ID), repositorySessionID, repositoryPath, 1, "ARCHIVED"), []state.SlotRepository{{RepositoryID: string(repositoryWorkspace.Repositories[0].ID), DirName: "repository", State: "READY", BaseOID: "head"}},
		state.Session{ID: repositorySessionID, WorkspaceID: string(repositoryWorkspace.ID), SlotID: repositorySessionID, State: "ARCHIVED", AgentKind: "matrix", TokenHash: state.HashToken(repositorySessionID)}, ""); err != nil {
		t.Fatal(err)
	}
	if count := manager.expireWorkspaceSnapshots(ctx, map[string][]state.Snapshot{repositorySessionID: nil}, archiveManager); count != 1 {
		t.Fatalf("repository-only expiry count=%d", count)
	}

	retiringID := domain.StableID("expiry-matrix", "retiring")
	retiringPath := filepath.Join(manager.Config().Storage.WorktreeRoot, "expiry", retiringID, "root")
	if err := os.MkdirAll(retiringPath, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateStandby(ctx, slotAtPath(t, manager, string(workspaceRecord.ID), retiringID, retiringPath, 1, "RETIRING"), []state.SlotRepository{{RepositoryID: string(workspaceRecord.Repositories[0].ID), DirName: escapingDirNameFor(t, manager.Config().Storage.WorktreeRoot, retiringPath), State: "RETIRING", BaseOID: "head"}}); err != nil {
		t.Fatal(err)
	}
	if err := manager.removeColdRepositoryJob(ctx, state.Job{ID: "cold-boundary", Kind: "REMOVE_REPOSITORY", SlotID: retiringID, RepositoryID: string(workspaceRecord.Repositories[0].ID)}); !errors.Is(err, state.ErrOwnership) {
		t.Fatalf("outside cold repository error=%v", err)
	}
	if slot, err := store.Slot(ctx, retiringID); err != nil || slot.State != "QUARANTINED" {
		t.Fatalf("outside cold repository slot=%+v err=%v", slot, err)
	}

	coldID := domain.StableID("expiry-matrix", "already-cold")
	coldPath := filepath.Join(manager.Config().Storage.WorktreeRoot, "expiry", coldID, "root")
	if err := os.MkdirAll(coldPath, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateStandby(ctx, slotAtPath(t, manager, string(workspaceRecord.ID), coldID, coldPath, 1, "READY"), []state.SlotRepository{{RepositoryID: string(workspaceRecord.Repositories[0].ID), DirName: escapingDirNameFor(t, manager.Config().Storage.WorktreeRoot, coldPath), State: "COLD", BaseOID: "head"}}); err != nil {
		t.Fatal(err)
	}
	if err := manager.removeColdRepositoryJob(ctx, state.Job{ID: "cold-idempotent", Kind: "REMOVE_REPOSITORY", SlotID: coldID, RepositoryID: string(workspaceRecord.Repositories[0].ID)}); err != nil {
		t.Fatalf("already cold repository removal: %v", err)
	}

	wrongID := domain.StableID("expiry-matrix", "wrong-state")
	wrongPath := filepath.Join(manager.Config().Storage.WorktreeRoot, "expiry", wrongID, "root")
	if err := os.MkdirAll(wrongPath, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateStandby(ctx, slotAtPath(t, manager, string(workspaceRecord.ID), wrongID, wrongPath, 1, "READY"), []state.SlotRepository{{RepositoryID: string(workspaceRecord.Repositories[0].ID), DirName: escapingDirNameFor(t, manager.Config().Storage.WorktreeRoot, wrongPath), State: "READY", BaseOID: "head"}}); err != nil {
		t.Fatal(err)
	}
	if err := manager.removeColdRepositoryJob(ctx, state.Job{ID: "cold-wrong-state", Kind: "REMOVE_REPOSITORY", SlotID: wrongID, RepositoryID: string(workspaceRecord.Repositories[0].ID)}); err == nil {
		t.Fatal("wrong-state cold repository removal succeeded")
	}
}

func TestManagerDiagnosticPathBoundaries(t *testing.T) {
	if got := diagnosticPath("", 0, 0); got != "path unavailable" {
		t.Fatalf("empty diagnostic path=%q", got)
	}
	if got := diagnosticPath(filepath.Join(t.TempDir(), "missing"), 0, 0); !strings.Contains(got, "no such file") {
		t.Fatalf("missing diagnostic path=%q", got)
	}
	root := t.TempDir()
	file := filepath.Join(root, "file")
	if err := os.WriteFile(file, []byte("file"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := diagnosticPath(file, 0, 0o600); got != "ok" {
		t.Fatalf("regular diagnostic path=%q", got)
	}
	if got := diagnosticPath(file, os.ModeDir, 0o700); got != "not a directory" {
		t.Fatalf("file as directory diagnostic path=%q", got)
	}
	if err := os.Chmod(file, 0o644); err != nil {
		t.Fatal(err)
	}
	if got := diagnosticPath(file, 0, 0o600); !strings.Contains(got, "unsafe permissions") {
		t.Fatalf("permission diagnostic path=%q", got)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(file, link); err != nil {
		t.Fatal(err)
	}
	if got := diagnosticPath(link, 0, 0); got != "unsafe symlink" {
		t.Fatalf("symlink diagnostic path=%q", got)
	}
}

func TestManagerColdRepositoryRemovalCompletesOwnedWorktree(t *testing.T) {
	ctx, manager, store, workspaceRecord, _, _ := managerCoverageFixture(t)
	repository := workspaceRecord.Repositories[0]
	head := gitOutput(t, string(repository.MainPath), "rev-parse", "HEAD")
	slotID := domain.StableID("cold-removal", "owned")
	ownedWorkspace := workspaceRecord
	ownedWorkspace.Root = domain.CanonicalPath(filepath.Dir(string(repository.MainPath)))
	ownedWorkspace.Repositories = append([]discovery.Repository(nil), workspaceRecord.Repositories...)
	ownedWorkspace.Repositories[0].RelativePath = filepath.Base(string(repository.MainPath))
	ownedWorkspace, _, err := store.UpsertWorkspaceGeneration(ctx, ownedWorkspace)
	if err != nil {
		t.Fatal(err)
	}
	slot := testSlot(t, manager, string(ownedWorkspace.ID), slotID, 1, "RETIRING")
	worktreePath := filepath.Join(slot.Path, "repository")
	gitRun(t, string(repository.MainPath), "worktree", "add", "--detach", worktreePath, head)
	ensureOwnershipMarkerForTest(t, manager.Config().Storage.WorktreeRoot, worktreePath, workspace.MarkerIdentity{SlotID: slotID, RootID: slot.RootID, RepositoryID: string(repository.ID)}, string(repository.CommonDir))
	gitRun(t, string(repository.MainPath), "worktree", "lock", "--reason", "wx:"+slotID+":READY", worktreePath)
	if _, err := store.CreateStandby(ctx, slot,
		[]state.SlotRepository{{RepositoryID: string(repository.ID), DirName: "repository", State: "RETIRING", BaseOID: head}}); err != nil {
		t.Fatal(err)
	}
	if err := manager.removeColdRepositoryJob(ctx, state.Job{ID: "cold-owned", SlotID: slotID, RepositoryID: string(repository.ID)}); err != nil {
		t.Fatalf("owned cold repository removal: %v", err)
	}
	if _, err := os.Lstat(worktreePath); !os.IsNotExist(err) {
		t.Fatalf("removed worktree still exists: %v", err)
	}
	finished, err := store.Slot(ctx, slotID)
	if err != nil || finished.State != "READY" {
		t.Fatalf("finished cold slot=%+v err=%v", finished, err)
	}
}
