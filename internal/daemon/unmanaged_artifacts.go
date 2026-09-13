package daemon

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"

	"github.com/HappyOnigiri/WX/internal/archive"
	"github.com/HappyOnigiri/WX/internal/state"
)

// unmanagedKind は登録外の実体の種別である。削除の手順は同じで、利用者が何を消すのか読めるようにするために持つ。
type unmanagedKind string

const (
	unmanagedSlotDirectory     unmanagedKind = "slot_directory"
	unmanagedWorkspaceSnapshot unmanagedKind = "workspace_snapshot"
)

// unmanagedArtifact は wx の予約 namespace の配下にありながら DB が説明しない実体 1 件である。
// Root は pin 対象の root 世代 path、RelPath は削除に使う root 相対 path で、Path は表示だけに使う。
type unmanagedArtifact struct {
	Root    string
	RelPath string
	Path    string
	Kind    unmanagedKind
}

// unmanagedExpectations は「DB が説明する実体」の集合である。
// slot は state で絞らず、snapshot は status で絞らない。行が残る限り wx が自分のものと主張しているためである。
type unmanagedExpectations struct {
	slots     map[string]state.SlotArtifact
	snapshots map[string]bool
}

// explainsSlot は絶対 path の slot directory が登録済みかを返す。
func (e unmanagedExpectations) explainsSlot(path string) bool {
	_, known := e.slots[filepath.Clean(path)]
	return known
}

// explainsSnapshot は root 相対 path の archive が登録済みかを返す。
// root 世代ごとに同じ相対 path があり得るので、root と組にして引く。
func (e unmanagedExpectations) explainsSnapshot(root, relative string) bool {
	return e.snapshots[unmanagedSnapshotKey(root, relative)]
}

func unmanagedSnapshotKey(root, relative string) string {
	return filepath.Clean(root) + "\x00" + filepath.Clean(relative)
}

// unmanagedExpectationSet は登録済みの slot と workspace snapshot を DB から引く。
func (m *Manager) unmanagedExpectationSet(ctx context.Context) (unmanagedExpectations, error) {
	artifacts, err := m.store.SlotArtifacts(ctx)
	if err != nil {
		return unmanagedExpectations{}, err
	}
	snapshots, err := m.store.RegisteredWorkspaceSnapshots(ctx)
	if err != nil {
		return unmanagedExpectations{}, err
	}
	expected := unmanagedExpectations{slots: expectedSlotPaths(artifacts), snapshots: map[string]bool{}}
	for _, snapshot := range snapshots {
		expected.snapshots[unmanagedSnapshotKey(snapshotRootOf(snapshot), snapshot.RelPath)] = true
	}
	return expected, nil
}

// snapshotRootOf は snapshot の登録から root 世代 path を戻す。
// ArchivePath は root path と rel_path の join なので、末尾の rel_path を取り除けば元の root に戻る。
func snapshotRootOf(snapshot state.WorkspaceSnapshot) string {
	archivePath := filepath.Clean(snapshot.ArchivePath)
	relative := filepath.Clean(filepath.FromSlash(snapshot.RelPath))
	if len(archivePath) <= len(relative) {
		return archivePath
	}
	return filepath.Clean(archivePath[:len(archivePath)-len(relative)])
}

// scanUnmanagedArtifacts は全 root 世代の登録外実体を列挙する。
// 列挙できなかった root は errs に落とし、他の root の結果は返す。全部は見えていないことを呼び出し側が扱えるようにするためである。
// reconcile の隔離記録・警告ログ・`wx prune` の対象範囲はここを経由しない。副作用の広い経路を広げない判断による。
func (m *Manager) scanUnmanagedArtifacts(ctx context.Context) (artifacts []unmanagedArtifact, errs []string) {
	expected, err := m.unmanagedExpectationSet(ctx)
	if err != nil {
		return nil, []string{fmt.Sprintf("list registered artifacts: %v", err)}
	}
	roots, rootsErr := m.rootPathsFromStore(ctx)
	if rootsErr != nil {
		errs = append(errs, fmt.Sprintf("list worktree root generations: %v", rootsErr))
	}
	artifacts = []unmanagedArtifact{}
	for _, root := range roots {
		found, err := m.unmanagedArtifactsOfRoot(root, expected)
		if err != nil {
			errs = append(errs, fmt.Sprintf("inspect root %s: %v", root, err))
			continue
		}
		artifacts = append(artifacts, found...)
	}
	sortUnmanagedArtifacts(artifacts)
	return artifacts, errs
}

// unmanagedArtifactsOfRoot は root 世代 1 つを pin して列挙する。
func (m *Manager) unmanagedArtifactsOfRoot(root string, expected unmanagedExpectations) ([]unmanagedArtifact, error) {
	var found []unmanagedArtifact
	err := m.withVerifiedRoot(root, func(owner *os.Root) error {
		var listErr error
		found, listErr = unmanagedArtifactsAt(owner, filepath.Clean(root), expected)
		return listErr
	})
	if err != nil {
		return nil, err
	}
	return found, nil
}

// unmanagedArtifactsAt は pin 済み root の上で登録外の実体を集める。
// 対象は slot を並べる namespace 直下の directory と、workspace snapshot の置き場直下の非 directory entry だけである。
// 後者を名前で絞らないのは、保存途中の一時ファイル（`<id>.tar.tmp-<id>`）も残骸として同じ扱いにするためである。
func unmanagedArtifactsAt(owner *os.Root, root string, expected unmanagedExpectations) ([]unmanagedArtifact, error) {
	slotPaths, err := ownedRootArtifactPathsAt(owner, root)
	if err != nil {
		return nil, err
	}
	found := []unmanagedArtifact{}
	for _, slotPath := range slotPaths {
		if expected.explainsSlot(slotPath) {
			continue
		}
		relative, ok := relativeWithinRoot(root, slotPath)
		if !ok {
			return nil, fmt.Errorf("%w: slot directory is outside root: %s", state.ErrOwnership, slotPath)
		}
		found = append(found, unmanagedArtifact{Root: root, RelPath: relative, Path: filepath.Clean(slotPath), Kind: unmanagedSlotDirectory})
	}
	snapshots, err := unmanagedSnapshotFilesAt(owner, root, expected)
	if err != nil {
		return nil, err
	}
	return append(found, snapshots...), nil
}

func unmanagedSnapshotFilesAt(owner *os.Root, root string, expected unmanagedExpectations) ([]unmanagedArtifact, error) {
	entries, err := fs.ReadDir(owner.FS(), archive.WorkspaceSnapshotDirectory)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("%w: inspect workspace snapshot directory: %w", state.ErrOwnership, err)
	}
	found := []unmanagedArtifact{}
	for _, entry := range entries {
		// 登録は archive ファイルしか指さないので、この階層の directory は列挙も削除もしない。使用量の走査も降りない。
		if entry.IsDir() {
			continue
		}
		relative := filepath.FromSlash(path.Join(archive.WorkspaceSnapshotDirectory, entry.Name()))
		if expected.explainsSnapshot(root, relative) {
			continue
		}
		found = append(found, unmanagedArtifact{Root: root, RelPath: relative, Path: filepath.Join(root, relative), Kind: unmanagedWorkspaceSnapshot})
	}
	return found, nil
}

func sortUnmanagedArtifacts(artifacts []unmanagedArtifact) {
	sort.Slice(artifacts, func(i, j int) bool { return artifacts[i].Path < artifacts[j].Path })
}

// unmanagedArtifactPaths は表示用に path だけを並べる。
func unmanagedArtifactPaths(artifacts []unmanagedArtifact) []string {
	paths := make([]string, 0, len(artifacts))
	for _, artifact := range artifacts {
		paths = append(paths, artifact.Path)
	}
	return paths
}
