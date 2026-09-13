package workspace

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

func TestUsageJoinKeepsRootRelativePaths(t *testing.T) {
	t.Parallel()
	if got := usageJoin(".", "leaf"); got != "leaf" {
		t.Fatalf("root child=%q", got)
	}
	if got := usageJoin("workspace/slot", "repo"); got != "workspace/slot/repo" {
		t.Fatalf("nested child=%q", got)
	}
}

// symlink は指し先へ降りずに 1 件として数える。辿ると slot の外の実体を管理容量に混ぜてしまう。
func TestMeasureRootUsageCountsSymlinksWithoutFollowingThem(t *testing.T) {
	t.Parallel()
	root, _, targets := usageRoots(t)
	slot := filepath.Join(root.Name(), "workspace", "slot")
	outside := t.TempDir()
	usageWrite(t, outside, "hidden", "hidden content")
	if err := os.Symlink(outside, filepath.Join(slot, "link")); err != nil {
		t.Fatal(err)
	}

	usage, _, err := MeasureRootUsage(context.Background(), root, targets, testUsageNamespaces(), nil)
	if err != nil {
		t.Fatal(err)
	}
	// symlink 自身は通常ファイルではないため、件数には入るが論理サイズには足さない。
	if sample := usage.Slots["slot"]; sample.Files != 1 || sample.LogicalBytes != 0 {
		t.Fatalf("symlink was followed or skipped: %+v", sample)
	}
}

// 登録と違う slot の下に現れた repository path は共有元を持たず、共有判定の対象にもしない。
func TestMeasureRootUsageIgnoresRepositoriesOfAnotherSlot(t *testing.T) {
	t.Parallel()
	root, mainPath, _ := usageRoots(t)
	usageWrite(t, filepath.Join(root.Name(), "workspace", "slot", "repo"), "file", "content")
	usageWrite(t, mainPath, "file", "content")
	// repository は "other" の下にあると登録されているので、"slot" の下の同名 directory とは結びつかない。
	targets := []SlotUsageTarget{
		{SlotID: "slot", RelPath: "workspace/slot"},
		{SlotID: "other", RelPath: "workspace/other", Repositories: map[string]string{"repo": mainPath}},
	}

	usage, cache, err := MeasureRootUsage(context.Background(), root, targets, testUsageNamespaces(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if sample := usage.Slots["slot"]; sample.Files != 1 || sample.Compared != 0 || sample.SharedBytes != 0 {
		t.Fatalf("foreign repository was compared: %+v", sample)
	}
	if len(cache) != 0 {
		t.Fatalf("cache=%+v", cache)
	}
}

// 走査は directory ごとに並列に走るため、合計と cache が worker 間で落ちないことを検査する。
func TestMeasureRootUsageAggregatesParallelDirectories(t *testing.T) {
	t.Parallel()
	root, mainPath, targets := usageRoots(t)
	slotRepo := filepath.Join(root.Name(), "workspace", "slot", "repo")
	const directories, perDirectory = 12, 8
	for i := range directories {
		name := fmt.Sprintf("dir%02d", i)
		for _, base := range []string{slotRepo, mainPath} {
			if err := os.MkdirAll(filepath.Join(base, name), 0o755); err != nil {
				t.Fatal(err)
			}
		}
		for j := range perDirectory {
			leaf := filepath.Join(name, fmt.Sprintf("file%02d", j))
			usageWrite(t, mainPath, leaf, "shared content")
			usageWrite(t, slotRepo, leaf, "shared content")
		}
	}

	usage, cache, err := MeasureRootUsage(context.Background(), root, targets, testUsageNamespaces(), nil)
	if err != nil {
		t.Fatal(err)
	}
	sample := usage.Slots["slot"]
	if sample.Files != directories*perDirectory || usage.AllocatedBytes != sample.AllocatedBytes {
		t.Fatalf("parallel walk lost entries: %+v root=%d", sample, usage.AllocatedBytes)
	}
	if !SharingSupported() {
		return
	}
	if sample.Compared != directories*perDirectory || len(cache) != directories*perDirectory {
		t.Fatalf("parallel walk lost verdicts: %+v cache=%d", sample, len(cache))
	}
}

// 走査中に消えた entry は飛ばすだけにする。読めない entry は測定を失敗にして、部分集計を実測に見せない。
func TestMeasureRootUsageFailsOnUnreadableDirectories(t *testing.T) {
	t.Parallel()
	root, _, targets := usageRoots(t)
	blocked := filepath.Join(root.Name(), "workspace", "slot", "blocked")
	if err := os.MkdirAll(blocked, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(blocked, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(blocked, 0o700) })

	if _, _, err := MeasureRootUsage(context.Background(), root, targets, testUsageNamespaces(), nil); err == nil {
		t.Fatal("measurement succeeded despite an unreadable directory")
	}
}

// 共有元に無い directory の下は共有判定の対象に数えたまま、判定できないので cache へ残さない。
func TestMeasureRootUsageComparesDirectoriesMissingFromTheSource(t *testing.T) {
	t.Parallel()
	root, mainPath, targets := usageRoots(t)
	generated := filepath.Join(root.Name(), "workspace", "slot", "repo", "generated")
	if err := os.MkdirAll(generated, 0o755); err != nil {
		t.Fatal(err)
	}
	usageWrite(t, generated, "artifact", "generated content")
	if _, err := os.Lstat(filepath.Join(mainPath, "generated")); !os.IsNotExist(err) {
		t.Fatalf("the source unexpectedly has the directory: %v", err)
	}

	usage, cache, err := MeasureRootUsage(context.Background(), root, targets, testUsageNamespaces(), nil)
	if err != nil {
		t.Fatal(err)
	}
	sample := usage.Slots["slot"]
	if sample.Files != 1 || sample.SharedFiles != 0 || sample.SharedBytes != 0 {
		t.Fatalf("sample=%+v", sample)
	}
	if !SharingSupported() {
		return
	}
	if sample.Compared != 1 || len(cache) != 0 {
		t.Fatalf("sample=%+v cache=%+v", sample, cache)
	}
}

// ファイル 1 個で登録された対象は、directory 境界に現れなくてもその slot の使用量として数える。
// snapshot の archive はこの形の登録なので、境界だけで判定すると必ず登録外へ落ちる。
func TestMeasureRootUsageCountsFileTargetsAsRegistered(t *testing.T) {
	t.Parallel()
	root, _, targets := usageRoots(t)
	snapshots := filepath.Join(root.Name(), "_recovery", "workspace-snapshots")
	if err := os.MkdirAll(snapshots, 0o700); err != nil {
		t.Fatal(err)
	}
	usageWrite(t, snapshots, "registered.tar", "archive content")
	usageWrite(t, snapshots, "registered.tar.tmp-abc", "half written")
	targets = append(targets, SlotUsageTarget{
		SlotID: "snapshot:session", RelPath: "_recovery/workspace-snapshots/registered.tar", File: true,
	})

	usage, _, err := MeasureRootUsage(context.Background(), root, targets, testUsageNamespaces(), nil)
	if err != nil {
		t.Fatal(err)
	}
	sample, measured := usage.Slots["snapshot:session"]
	if !measured || sample.Files != 1 || sample.LogicalBytes != int64(len("archive content")) {
		t.Fatalf("registered archive sample=%+v measured=%v", sample, measured)
	}
	// 共有元を持たない archive なので CoW の比較には回さない。
	if sample.Compared != 0 || sample.SharedBytes != 0 {
		t.Fatalf("archive was compared against a source: %+v", sample)
	}
	if usage.UnmanagedBytes == 0 {
		t.Fatal("the leftover temporary archive was not reported as unmanaged")
	}
	if usage.AllocatedBytes < sample.AllocatedBytes {
		t.Fatalf("root allocated=%d does not include the archive=%d", usage.AllocatedBytes, sample.AllocatedBytes)
	}
}

// 走査は予約 namespace の配下にしか降りない。列挙も削除もできない実体を数えると、未管理量を解消できなくなる。
func TestMeasureRootUsageIgnoresEntitiesOutsideTheReservedNamespaces(t *testing.T) {
	t.Parallel()
	root, _, targets := usageRoots(t)
	usageWrite(t, root.Name(), "notes.txt", "personal notes")
	unrelated := filepath.Join(root.Name(), "my-scratch")
	if err := os.MkdirAll(unrelated, 0o755); err != nil {
		t.Fatal(err)
	}
	usageWrite(t, unrelated, "file", "scratch content")
	// _recovery の下でも、snapshot の置き場以外へは降りない。
	other := filepath.Join(root.Name(), "_recovery", "other")
	if err := os.MkdirAll(other, 0o700); err != nil {
		t.Fatal(err)
	}
	usageWrite(t, other, "file", "recovery scratch")

	usage, _, err := MeasureRootUsage(context.Background(), root, targets, testUsageNamespaces(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if usage.UnmanagedBytes != 0 || usage.AllocatedBytes != 0 {
		t.Fatalf("unrelated entities were measured: %+v", usage)
	}
}

// 登録外の実体は、slot を並べる層の directory 配下と snapshot の置き場のファイルに限って未管理として数える。
// この 2 つは `wx clear --unmanaged` が列挙して削除できる集合そのものである。
func TestMeasureRootUsageReportsOnlyDeletableEntitiesAsUnmanaged(t *testing.T) {
	t.Parallel()
	root, _, targets := usageRoots(t)
	leftover := filepath.Join(root.Name(), "workspace", "leftover-slot")
	if err := os.MkdirAll(leftover, 0o755); err != nil {
		t.Fatal(err)
	}
	usageWrite(t, leftover, "file", "leftover content")
	// namespace 直下に置かれたファイルは列挙されないので数えない。
	usageWrite(t, filepath.Join(root.Name(), "workspace"), "stray", "stray content")

	usage, _, err := MeasureRootUsage(context.Background(), root, targets, testUsageNamespaces(), nil)
	if err != nil {
		t.Fatal(err)
	}
	// 未管理量は登録外 directory の中身だけで、namespace 直下の stray は入らない。
	if want := usageAllocatedOf(t, filepath.Join(leftover, "file")); usage.UnmanagedBytes != want {
		t.Fatalf("unmanaged=%d want=%d (the stray file must not be counted)", usage.UnmanagedBytes, want)
	}
	if sample := usage.Slots["slot"]; sample.Files != 0 {
		t.Fatalf("the unregistered directory was attributed to a slot: %+v", sample)
	}
}

// usageAllocatedOf はファイル 1 個の割当量を、測定と同じ st_blocks から求める。
func usageAllocatedOf(t *testing.T, path string) int64 {
	t.Helper()
	var stat unix.Stat_t
	if err := unix.Lstat(path, &stat); err != nil {
		t.Fatal(err)
	}
	return stat.Blocks * 512
}

// ファイルの登録は directory として開けないため、slot 1 個だけの部分測定には渡せない。
func TestMeasureSlotUsageRejectsFileTargets(t *testing.T) {
	t.Parallel()
	root, _, _ := usageRoots(t)
	target := SlotUsageTarget{SlotID: "snapshot:session", RelPath: "_recovery/workspace-snapshots/a.tar", File: true}
	if _, _, err := MeasureSlotUsage(context.Background(), root, target, nil); err == nil {
		t.Fatal("a file target was accepted as a measurable slot")
	}
}
