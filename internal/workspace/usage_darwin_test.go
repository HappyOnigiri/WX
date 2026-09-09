package workspace

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// cloneInto は main worktree のファイルを slot 側へ clone し、CoW で用意した worktree を再現する。
func cloneInto(t *testing.T, mainPath, name, slotRepo string) {
	t.Helper()
	source, err := os.Open(filepath.Join(mainPath, name))
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	parent, err := os.Open(slotRepo)
	if err != nil {
		t.Fatal(err)
	}
	defer parent.Close()
	if err := cloneCOW(source, parent, name); err != nil {
		t.Fatal(err)
	}
}

func TestMeasureRootUsageDetectsClonedAndRewrittenFiles(t *testing.T) {
	root, mainPath, targets := usageRoots(t)
	slotRepo := filepath.Join(root.Name(), "workspace", "slot", "repo")
	usageWrite(t, mainPath, "shared", "shared content")
	usageWrite(t, mainPath, "diverged", "diverged content")
	cloneInto(t, mainPath, "shared", slotRepo)
	cloneInto(t, mainPath, "diverged", slotRepo)

	usage, cache, err := MeasureRootUsage(context.Background(), root, targets, nil)
	if err != nil {
		t.Fatal(err)
	}
	slot := usage.Slots["slot"]
	if slot.Compared != 2 || slot.SharedFiles != 2 || slot.SharedBytes != slot.AllocatedBytes {
		t.Fatalf("clone was not detected as shared: %+v", slot)
	}

	// 貸出後の書き換えは共有を解く。準備時の記録ではなく実測なので、次の測定でそのまま減る。
	usageWrite(t, slotRepo, "diverged", "rewritten content")
	usage, cache, err = MeasureRootUsage(context.Background(), root, targets, cache)
	if err != nil {
		t.Fatal(err)
	}
	if slot = usage.Slots["slot"]; slot.SharedFiles != 1 {
		t.Fatalf("rewritten file still counted as shared: %+v", slot)
	}
	if slot.SharedBytes == 0 || slot.SharedBytes >= slot.AllocatedBytes {
		t.Fatalf("shared bytes=%d allocated=%d", slot.SharedBytes, slot.AllocatedBytes)
	}
	// root 合計の共有量は slot ごとの合計と一致し、slot の外にあるファイルは非共有として残る。
	if usage.SharedBytes != slot.SharedBytes || usage.SharedBytes >= usage.AllocatedBytes {
		t.Fatalf("root shared=%d allocated=%d slot shared=%d", usage.SharedBytes, usage.AllocatedBytes, slot.SharedBytes)
	}

	// source 側を同じサイズで書き換えても、source identity の変化を検出して共有を解く。
	usageWrite(t, mainPath, "shared", "updatedcontent")
	usage, _, err = MeasureRootUsage(context.Background(), root, targets, cache)
	if err != nil {
		t.Fatal(err)
	}
	if slot := usage.Slots["slot"]; slot.SharedFiles != 0 || slot.SharedBytes != 0 || usage.SharedBytes != 0 {
		t.Fatalf("rewritten source still counted as shared: usage=%+v root=%d", slot, usage.SharedBytes)
	}
}

func TestMeasureRootUsageRevalidatesTheCachedVerdict(t *testing.T) {
	root, mainPath, targets := usageRoots(t)
	slotRepo := filepath.Join(root.Name(), "workspace", "slot", "repo")
	usageWrite(t, mainPath, "shared", "shared content")
	cloneInto(t, mainPath, "shared", slotRepo)
	usage, cache, err := MeasureRootUsage(context.Background(), root, targets, nil)
	if err != nil {
		t.Fatal(err)
	}
	if slot := usage.Slots["slot"]; slot.SharedFiles != 1 {
		t.Fatalf("initial clone was not detected as shared: %+v", slot)
	}
	// source の inode 置換では、内容が同じでも古い cache を再利用しない。
	usageWrite(t, mainPath, "replacement", "shared content")
	if err := os.Rename(filepath.Join(mainPath, "replacement"), filepath.Join(mainPath, "shared")); err != nil {
		t.Fatal(err)
	}
	usage, cache, err = MeasureRootUsage(context.Background(), root, targets, cache)
	if err != nil {
		t.Fatal(err)
	}
	if slot := usage.Slots["slot"]; slot.SharedFiles != 0 {
		t.Fatalf("replaced source was counted as shared: %+v", slot)
	}
	if len(cache) != 1 {
		t.Fatalf("replacement cache=%+v", cache)
	}
	// source の symlink 化では physical path を検証できないため cache を残さない。
	usageWrite(t, mainPath, "symlink-target", "shared content")
	if err := os.Remove(filepath.Join(mainPath, "shared")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("symlink-target", filepath.Join(mainPath, "shared")); err != nil {
		t.Fatal(err)
	}
	usage, cache, err = MeasureRootUsage(context.Background(), root, targets, cache)
	if err != nil {
		t.Fatal(err)
	}
	if slot := usage.Slots["slot"]; slot.SharedFiles != 0 {
		t.Fatalf("symlink source was counted as shared: %+v", slot)
	}
	if len(cache) != 0 {
		t.Fatalf("symlink source cache=%+v", cache)
	}
	// main 側を消すと共有元を検証できないため、古い判定は再利用しない。
	if err := os.Remove(filepath.Join(mainPath, "shared")); err != nil {
		t.Fatal(err)
	}
	usage, cache, err = MeasureRootUsage(context.Background(), root, targets, cache)
	if err != nil {
		t.Fatal(err)
	}
	if slot := usage.Slots["slot"]; slot.SharedFiles != 0 {
		t.Fatalf("missing source file counted as shared: %+v", slot)
	}
	if len(cache) != 0 {
		t.Fatalf("cache retained an unverifiable source: %+v", cache)
	}
	// source を再配置して clone し直した場合は、次の測定で共有を再検出する。
	if err := os.Remove(filepath.Join(slotRepo, "shared")); err != nil {
		t.Fatal(err)
	}
	usageWrite(t, mainPath, "shared", "shared content")
	cloneInto(t, mainPath, "shared", slotRepo)
	usage, cache, err = MeasureRootUsage(context.Background(), root, targets, cache)
	if err != nil {
		t.Fatal(err)
	}
	if slot := usage.Slots["slot"]; slot.SharedFiles != 1 {
		t.Fatalf("restored clone was not detected as shared: %+v", slot)
	}
	if len(cache) != 1 {
		t.Fatalf("cache=%+v", cache)
	}
}
