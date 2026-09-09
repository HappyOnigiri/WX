package workspace

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestUsageJoinKeepsRootRelativePaths(t *testing.T) {
	if got := usageJoin(".", "leaf"); got != "leaf" {
		t.Fatalf("root child=%q", got)
	}
	if got := usageJoin("workspace/slot", "repo"); got != "workspace/slot/repo" {
		t.Fatalf("nested child=%q", got)
	}
}

// symlink は指し先へ降りずに 1 件として数える。辿ると slot の外の実体を管理容量に混ぜてしまう。
func TestMeasureRootUsageCountsSymlinksWithoutFollowingThem(t *testing.T) {
	root, _, targets := usageRoots(t)
	slot := filepath.Join(root.Name(), "workspace", "slot")
	outside := t.TempDir()
	usageWrite(t, outside, "hidden", "hidden content")
	if err := os.Symlink(outside, filepath.Join(slot, "link")); err != nil {
		t.Fatal(err)
	}

	usage, _, err := MeasureRootUsage(context.Background(), root, targets, nil)
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
	root, mainPath, _ := usageRoots(t)
	usageWrite(t, filepath.Join(root.Name(), "workspace", "slot", "repo"), "file", "content")
	usageWrite(t, mainPath, "file", "content")
	// repository は "other" の下にあると登録されているので、"slot" の下の同名 directory とは結びつかない。
	targets := []SlotUsageTarget{
		{SlotID: "slot", RelPath: "workspace/slot"},
		{SlotID: "other", RelPath: "workspace/other", Repositories: map[string]string{"repo": mainPath}},
	}

	usage, cache, err := MeasureRootUsage(context.Background(), root, targets, nil)
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

	usage, cache, err := MeasureRootUsage(context.Background(), root, targets, nil)
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
	root, _, targets := usageRoots(t)
	blocked := filepath.Join(root.Name(), "workspace", "slot", "blocked")
	if err := os.MkdirAll(blocked, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(blocked, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(blocked, 0o700) })

	if _, _, err := MeasureRootUsage(context.Background(), root, targets, nil); err == nil {
		t.Fatal("measurement succeeded despite an unreadable directory")
	}
}

// 共有元に無い directory の下は共有判定の対象に数えたまま、判定できないので cache へ残さない。
func TestMeasureRootUsageComparesDirectoriesMissingFromTheSource(t *testing.T) {
	root, mainPath, targets := usageRoots(t)
	generated := filepath.Join(root.Name(), "workspace", "slot", "repo", "generated")
	if err := os.MkdirAll(generated, 0o755); err != nil {
		t.Fatal(err)
	}
	usageWrite(t, generated, "artifact", "generated content")
	if _, err := os.Lstat(filepath.Join(mainPath, "generated")); !os.IsNotExist(err) {
		t.Fatalf("the source unexpectedly has the directory: %v", err)
	}

	usage, cache, err := MeasureRootUsage(context.Background(), root, targets, nil)
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
