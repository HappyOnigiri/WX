package daemon

import (
	"archive/tar"
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/HappyOnigiri/WX/internal/archive"
	"github.com/HappyOnigiri/WX/internal/config"
	"github.com/HappyOnigiri/WX/internal/discovery"
	"github.com/HappyOnigiri/WX/internal/domain"
	"github.com/HappyOnigiri/WX/internal/state"
)

func TestReleaseIsIdempotentAfterAlreadyReleasingSession(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	store, err := state.Open(filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	if _, err := store.CreateSlotSession(ctx, storeSlotAt(t, store, root, "", "dup", filepath.Join(root, "root"), 0, "LEASED"), nil, state.Session{ID: "dup", SlotID: "dup", State: "ACTIVE", AgentKind: "codex", TokenHash: state.HashToken("token")}, ""); err != nil {
		t.Fatal(err)
	}
	manager := &Manager{store: store, jobQueue: newJobQueue(2), log: slog.New(slog.NewTextHandler(newDiagnosticLog(managerFixtureLogLimit), nil)), ctx: context.Background()}
	if err := manager.Release(ctx, "dup", "token", "client-exit"); err != nil {
		t.Fatal(err)
	}
	if err := manager.Release(ctx, "dup", "token", "client-exit"); err != nil {
		t.Fatalf("idempotent release error=%v", err)
	}
	if got, _ := manager.jobQueue.counts(jobClassInteractive); got != 1 {
		t.Fatalf("duplicate release scheduled %d jobs, want 1", got)
	}
}

func TestSnapshotSessionFailsClosedAfterRepositorySnapshotWhenWorkspaceRootIsUnsafe(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name string
		mode string
	}{
		{name: "bundle outside known roots", mode: "outside"},
		{name: "existing root snapshot artifact missing", mode: "missing-artifact"},
		{name: "unsupported root filesystem entry", mode: "unsupported-entry"},
	} {
		t.Run(test.name, func(t *testing.T) {
			// この配下にunix socketを作るため、テスト名を含んで長くなるt.TempDir()は使えない（sun_pathの上限に達する）。
			// TMPDIRはTestMainが物理パスへ差し替え済みで、OSごとの一時ディレクトリの位置にも追随する。
			root, err := os.MkdirTemp("", "wx-snap-")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.RemoveAll(root) })
			store, err := state.Open(filepath.Join(root, "state.db"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = store.Close() })
			cfg := config.Defaults()
			cfg.Storage.WorktreeRoot = filepath.Join(root, "owned")
			bundleRoot := filepath.Join(cfg.Storage.WorktreeRoot, "slot")
			if test.mode == "outside" {
				bundleRoot = filepath.Join(root, "outside", "slot")
			}
			repositoryPath := filepath.Join(bundleRoot, "repository")
			initGitRepo(t, repositoryPath)
			manager := testManager(t, cfg, store)
			t.Cleanup(manager.Close)
			ctx := context.Background()
			commonDir := gitOutput(t, repositoryPath, "rev-parse", "--path-format=absolute", "--git-common-dir")
			repository := discovery.Repository{ID: "repository", MainPath: discoveryPath(repositoryPath), CommonDir: discoveryPath(commonDir), RelativePath: "repository", DefaultBranch: "main"}
			workspaceRecord := discovery.Workspace{ID: "workspace", Root: discoveryPath(root), Kind: "multi_repository", Repositories: []discovery.Repository{repository}}
			registered, _, err := store.UpsertWorkspaceGeneration(ctx, workspaceRecord)
			if err != nil {
				t.Fatal(err)
			}
			workspaceID := string(registered.ID)
			session := state.Session{ID: "session", WorkspaceID: workspaceID, SlotID: "slot", State: "ACTIVE", AgentKind: "codex", TokenHash: state.HashToken("token")}
			slotRepository := state.SlotRepository{RepositoryID: "repository", DirName: "repository", State: "LEASED", BaseOID: gitOutput(t, repositoryPath, "rev-parse", "HEAD")}
			var bundleSlot state.Slot
			if test.mode == "outside" {
				if _, inside := relativeWithinRoot(cfg.Storage.WorktreeRoot, bundleRoot); inside {
					t.Fatalf("bundle path %s is inside root %s; the case under test no longer applies", bundleRoot, cfg.Storage.WorktreeRoot)
				}
				escaping, relErr := filepath.Rel(cfg.Storage.WorktreeRoot, bundleRoot)
				if relErr != nil {
					t.Fatal(relErr)
				}
				bundleSlot = testSlotRow(t, manager, workspaceID, "slot", 1, "LEASED")
				bundleSlot.RelPath = escaping
				bundleSlot.Path = bundleRoot
			} else {
				bundleSlot = slotAtPath(t, manager, workspaceID, "slot", bundleRoot, 1, "LEASED")
			}
			if _, err := store.CreateSlotSession(ctx, bundleSlot, []state.SlotRepository{slotRepository}, session, ""); err != nil {
				t.Fatal(err)
			}
			if test.mode == "missing-artifact" {
				snapshotOwner, _, err := domain.OpenOwnedRoot(cfg.Storage.WorktreeRoot, cfg.Storage.WorktreeRoot)
				if err != nil {
					t.Fatal(err)
				}
				rootSnapshot, err := archive.SnapshotWorkspaceAt(ctx, bundleRoot, cfg.Storage.WorktreeRoot, bundleSlot.RootID, snapshotOwner, session.ID, []string{"repository"}, time.Now().Add(time.Hour))
				if closeErr := snapshotOwner.Close(); closeErr != nil {
					t.Fatal(closeErr)
				}
				if err != nil {
					t.Fatal(err)
				}
				if err := store.SaveWorkspaceSnapshot(ctx, rootSnapshot); err != nil {
					t.Fatal(err)
				}
				if err := os.Remove(rootSnapshot.ArchivePath); err != nil {
					t.Fatal(err)
				}
			}
			var listener net.Listener
			if test.mode == "unsupported-entry" {
				listener, err = net.Listen("unix", filepath.Join(bundleRoot, "unsupported.sock"))
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = listener.Close() }()
			}
			if _, changed, err := store.Release(ctx, session.ID, session.WorkspaceID, session.SlotID); err != nil || !changed {
				t.Fatalf("release changed=%v err=%v", changed, err)
			}
			released, err := store.SessionByID(ctx, session.ID)
			if err != nil {
				t.Fatal(err)
			}
			if err := manager.snapshotSession(ctx, released); err == nil {
				t.Fatal("workspace root snapshot failure was ignored")
			}
			wantSnapshots := 1
			if test.mode == "outside" {
				wantSnapshots = 0
			}
			snapshots, err := store.Snapshots(ctx, session.ID)
			if err != nil || len(snapshots) != wantSnapshots {
				t.Fatalf("repository recovery snapshot count=%d want %d: snapshots=%+v err=%v", len(snapshots), wantSnapshots, snapshots, err)
			}
			slot, err := store.Slot(ctx, session.SlotID)
			if err != nil || slot.State != "QUARANTINED" {
				t.Fatalf("unsafe root snapshot slot=%+v err=%v", slot, err)
			}
		})
	}
}

// TestSnapshotSessionKeepsRootWorkAddedToLinkRuleWhileLeased は、貸出中に root の .worktreelink へ追記された path の扱いを固定する。
// rule を読み直して除外を決めていた頃は、slot に copy で実体配置された作業が tar から落ち、ended worktree の削除で失われた。
func TestSnapshotSessionKeepsRootWorkAddedToLinkRuleWhileLeased(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	store, err := state.Open(filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	cfg := config.Defaults()
	cfg.Storage.WorktreeRoot = filepath.Join(root, "owned")
	bundleRoot := filepath.Join(cfg.Storage.WorktreeRoot, "slot")
	repositoryPath := filepath.Join(bundleRoot, "repository")
	initGitRepo(t, repositoryPath)
	// 貸出中に slot 内へ実体として置かれた作業を再現する。
	if err := os.MkdirAll(filepath.Join(bundleRoot, "docs"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bundleRoot, "docs", "note.md"), []byte("leased work\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	manager := testManager(t, cfg, store)
	t.Cleanup(manager.Close)
	ctx := context.Background()
	commonDir := gitOutput(t, repositoryPath, "rev-parse", "--path-format=absolute", "--git-common-dir")
	repository := discovery.Repository{ID: "repository", MainPath: discoveryPath(repositoryPath), CommonDir: discoveryPath(commonDir), RelativePath: "repository", DefaultBranch: "main"}
	w := registerTestWorkspace(t, store, discovery.Workspace{ID: "workspace", Root: discoveryPath(root), Kind: "multi_repository", Repositories: []discovery.Repository{repository}})
	session := state.Session{ID: "session", WorkspaceID: string(w.ID), SlotID: "slot", State: "ACTIVE", AgentKind: "codex", TokenHash: state.HashToken("token")}
	slotRepository := state.SlotRepository{RepositoryID: "repository", DirName: "repository", State: "LEASED", BaseOID: gitOutput(t, repositoryPath, "rev-parse", "HEAD")}
	if _, err := store.CreateSlotSession(ctx, slotAtPath(t, manager, string(w.ID), "slot", bundleRoot, 1, "LEASED"), []state.SlotRepository{slotRepository}, session, ""); err != nil {
		t.Fatal(err)
	}
	// 返却の直前に、同じ path を link rule へ足す。
	if err := os.WriteFile(filepath.Join(root, ".worktreelink"), []byte("docs\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, changed, err := store.Release(ctx, session.ID, session.WorkspaceID, session.SlotID); err != nil || !changed {
		t.Fatalf("release changed=%v err=%v", changed, err)
	}
	released, err := store.SessionByID(ctx, session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.snapshotSession(ctx, released); err != nil {
		t.Fatal(err)
	}
	rootSnapshot, found, err := store.WorkspaceSnapshot(ctx, session.ID)
	if err != nil || !found {
		t.Fatalf("workspace snapshot found=%v err=%v", found, err)
	}
	names := workspaceArchiveEntryNames(t, rootSnapshot.ArchivePath)
	if !containsString(names, "docs/note.md") {
		t.Fatalf("workspace archive entries=%v dropped the materialized work", names)
	}
	if containsString(names, "repository") {
		t.Fatalf("workspace archive entries=%v include the repository worktree", names)
	}
}

func workspaceArchiveEntryNames(t *testing.T, path string) []string {
	t.Helper()
	file, err := os.Open(path) // #nosec G304 -- テストが直前に作った archive の path である。
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()
	names := []string{}
	reader := tar.NewReader(file)
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, header.Name)
	}
	return names
}

func TestSnapshotFailureQuarantinesSlotWithoutRemovingWorktreeMetadata(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	repoPath := filepath.Join(root, "repository")
	initGitRepo(t, repoPath)
	store, err := state.Open(filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	cfg := config.Defaults()
	cfg.Storage.WorktreeRoot = filepath.Join(root, "worktrees")
	m := testManager(t, cfg, store)
	t.Cleanup(m.Close)
	ctx := context.Background()
	w := discovery.Workspace{Root: discoveryPath(repoPath), Kind: "repository", Repositories: []discovery.Repository{{ID: "repository", MainPath: discoveryPath(repoPath), CommonDir: discoveryPath(filepath.Join(repoPath, ".git")), DefaultBranch: "main"}}}
	w = registerTestWorkspace(t, store, w)
	slotPath := filepath.Join(cfg.Storage.WorktreeRoot, "slot")
	session := state.Session{ID: "session", WorkspaceID: string(w.ID), SlotID: "slot", State: "ACTIVE", AgentKind: "codex", TokenHash: state.HashToken("token")}
	repository := state.SlotRepository{RepositoryID: "repository", DirName: "missing", State: "LEASED", RequestedRef: "main", BaseOID: gitOutput(t, repoPath, "rev-parse", "HEAD"), Fingerprint: "fp"}
	if _, err := store.CreateSlotSession(ctx, slotAtPath(t, m, string(w.ID), "slot", slotPath, 1, "LEASED"), []state.SlotRepository{repository}, session, ""); err != nil {
		t.Fatal(err)
	}
	if _, changed, err := store.Release(ctx, "session", string(w.ID), "slot"); err != nil || !changed {
		t.Fatalf("release changed=%v err=%v", changed, err)
	}
	released, err := store.SessionByID(ctx, "session")
	if err != nil {
		t.Fatal(err)
	}
	if err := m.snapshotSession(ctx, released); err == nil {
		t.Fatal("snapshot of missing worktree succeeded")
	}
	slot, err := store.Slot(ctx, "slot")
	if err != nil || slot.State != "QUARANTINED" {
		t.Fatalf("snapshot failure slot=%+v err=%v", slot, err)
	}
	if repositories, err := store.SlotRepositories(ctx, "slot"); err != nil || len(repositories) != 1 {
		t.Fatalf("snapshot failure discarded metadata: repositories=%+v err=%v", repositories, err)
	}
}
