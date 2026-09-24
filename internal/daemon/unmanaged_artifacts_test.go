package daemon

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/HappyOnigiri/WorktreeX/internal/archive"
	"github.com/HappyOnigiri/WorktreeX/internal/state"
)

// unmanagedFixture は登録済みと登録外の実体を並べた root を用意する。
// 登録は DB 行が先・実体が後という順序で作り、準備中の slot も期待集合に入ることを他のテストと共有する。
func unmanagedFixture(t *testing.T) (context.Context, *Manager, *state.Store, string) {
	t.Helper()
	ctx, manager, store, workspaceRecord, _, _ := managerCoverageFixture(t)
	root := filepath.Clean(manager.Config().Storage.WorktreeRoot)
	workspaceID := string(workspaceRecord.ID)
	registered := filepath.Join(root, workspaceID, "registered")
	if err := os.MkdirAll(registered, 0o700); err != nil {
		t.Fatal(err)
	}
	slot := slotAtPath(t, manager, workspaceID, "registered", registered, 1, "SNAPSHOTTED")
	session := state.Session{ID: "session", WorkspaceID: workspaceID, SlotID: slot.ID, State: "ARCHIVED", AgentKind: "codex", TokenHash: state.HashToken("token")}
	if _, err := store.CreateSlotSession(ctx, slot, nil, session, ""); err != nil {
		t.Fatal(err)
	}
	snapshots := filepath.Join(root, filepath.FromSlash(archive.WorkspaceSnapshotDirectory))
	if err := os.MkdirAll(snapshots, 0o700); err != nil {
		t.Fatal(err)
	}
	relative := filepath.Join(filepath.FromSlash(archive.WorkspaceSnapshotDirectory), "registered.tar")
	if err := store.SaveWorkspaceSnapshot(ctx, state.WorkspaceSnapshot{
		SessionID: session.ID, RootID: slot.RootID, RelPath: relative, SHA256: strings.Repeat("a", 64),
		Status: "ARCHIVED", CreatedAt: state.FormatTime(time.Now()), ExpiresAt: state.FormatTime(time.Now().Add(time.Hour)),
	}); err != nil {
		t.Fatal(err)
	}
	for _, entry := range []string{"registered.tar", "orphan.tar", "orphan.tar.tmp-9f2c1d", "not-an-archive"} {
		if err := os.WriteFile(filepath.Join(snapshots, entry), []byte("payload"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// 登録外の slot directory と、wx と無関係な top-level をそれぞれ置く。
	if err := os.MkdirAll(filepath.Join(root, workspaceID, "leftover"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "my-scratch", "inner"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "notes.txt"), []byte("personal"), 0o600); err != nil {
		t.Fatal(err)
	}
	return ctx, manager, store, root
}

func unmanagedPathSet(artifacts []unmanagedArtifact) map[string]unmanagedKind {
	found := map[string]unmanagedKind{}
	for _, artifact := range artifacts {
		found[artifact.Path] = artifact.Kind
	}
	return found
}

// archive path と相対 path が同じ長さなら root を空文字へ縮めない。
func TestSnapshotRootOfKeepsTheEqualLengthBoundary(t *testing.T) {
	t.Parallel()
	snapshot := state.WorkspaceSnapshot{ArchivePath: "archive.tar", RelPath: "archive.tar"}
	if got := snapshotRootOf(snapshot); got != "archive.tar" {
		t.Fatalf("snapshot root=%q want %q", got, "archive.tar")
	}
}

// 登録外実体は隣接する path も辞書順に並べ、表示と削除の順序を固定する。
func TestSortUnmanagedArtifactsOrdersAdjacentPaths(t *testing.T) {
	t.Parallel()
	artifacts := []unmanagedArtifact{{Path: "/root/b"}, {Path: "/root/a"}}
	sortUnmanagedArtifacts(artifacts)
	if artifacts[0].Path != "/root/a" || artifacts[1].Path != "/root/b" {
		t.Fatalf("artifacts=%+v, want ascending paths", artifacts)
	}
}

// 列挙は予約 namespace 配下で DB が説明しない実体だけを返す。
// 保存途中の一時ファイルも名前で除かず、残骸を回収する別経路を作らない。
func TestScanUnmanagedArtifactsListsOnlyWhatTheDatabaseDoesNotExplain(t *testing.T) {
	ctx, manager, _, root := unmanagedFixture(t)
	artifacts, errs := manager.scanUnmanagedArtifacts(ctx)
	if len(errs) != 0 {
		t.Fatalf("scan errors=%v", errs)
	}
	snapshots := filepath.Join(root, filepath.FromSlash(archive.WorkspaceSnapshotDirectory))
	found := unmanagedPathSet(artifacts)
	want := map[string]unmanagedKind{
		filepath.Join(snapshots, "orphan.tar"):            unmanagedWorkspaceSnapshot,
		filepath.Join(snapshots, "orphan.tar.tmp-9f2c1d"): unmanagedWorkspaceSnapshot,
		filepath.Join(snapshots, "not-an-archive"):        unmanagedWorkspaceSnapshot,
	}
	for path, kind := range want {
		if found[path] != kind {
			t.Fatalf("artifact %s kind=%q want=%q (all=%+v)", path, found[path], kind, found)
		}
	}
	leftovers := 0
	for _, artifact := range artifacts {
		if artifact.Kind == unmanagedSlotDirectory {
			leftovers++
			if filepath.Base(artifact.Path) != "leftover" {
				t.Fatalf("registered slot directory was reported as unmanaged: %+v", artifact)
			}
		}
	}
	if leftovers != 1 {
		t.Fatalf("slot directories=%d want 1 (all=%+v)", leftovers, artifacts)
	}
	for _, artifact := range artifacts {
		if strings.Contains(artifact.Path, "my-scratch") || strings.HasSuffix(artifact.Path, "notes.txt") {
			t.Fatalf("an entity outside the wx namespaces was reported: %+v", artifact)
		}
		if strings.HasSuffix(artifact.Path, "registered.tar") {
			t.Fatalf("a registered archive was reported: %+v", artifact)
		}
	}
}

// 期待集合は status・state で絞らない。回収待ちの登録を登録外と読み替えると、GC が次に触る実体を横から消す。
func TestScanUnmanagedArtifactsKeepsRecordsWaitingForCollection(t *testing.T) {
	ctx, manager, store, _ := unmanagedFixture(t)
	if err := store.SetSlotState(ctx, "registered", []string{"SNAPSHOTTED"}, "ARCHIVED", ""); err != nil {
		t.Fatal(err)
	}
	artifacts, errs := manager.scanUnmanagedArtifacts(ctx)
	if len(errs) != 0 {
		t.Fatalf("scan errors=%v", errs)
	}
	for _, artifact := range artifacts {
		if filepath.Base(artifact.Path) == "registered" {
			t.Fatalf("an archived slot record was treated as unmanaged: %+v", artifact)
		}
	}
}

// root を開けない世代は errors へ落とし、他の root の列挙は続ける。
func TestScanUnmanagedArtifactsReportsRootsItCannotInspect(t *testing.T) {
	ctx, manager, store, _ := unmanagedFixture(t)
	missing := filepath.Join(t.TempDir(), "gone")
	if err := os.MkdirAll(missing, 0o700); err != nil {
		t.Fatal(err)
	}
	identity, err := pathIdentity(missing)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.EnsureActiveRoot(ctx, missing, identity); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(missing); err != nil {
		t.Fatal(err)
	}
	artifacts, errs := manager.scanUnmanagedArtifacts(ctx)
	if len(errs) == 0 {
		t.Fatal("an unreadable root generation was reported as fully inspected")
	}
	if len(artifacts) == 0 {
		t.Fatal("the remaining roots were not inspected")
	}
}

// reconcile の隔離記録の範囲は広げない。列挙を広げた副作用が毎周期の警告と prune へ流れないことを固定する。
func TestReconcileDoesNotQuarantineTheEntitiesOnlyTheNewScanSees(t *testing.T) {
	ctx, manager, store, _ := unmanagedFixture(t)
	manager.reconcileArtifacts(ctx)
	status, err := store.StatusDiagnostics(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, record := range status.Quarantine {
		if strings.Contains(filepath.ToSlash(record.Path), archive.WorkspaceSnapshotDirectory) {
			t.Fatalf("a workspace snapshot leftover was quarantined by reconcile: %+v", record)
		}
	}
}
