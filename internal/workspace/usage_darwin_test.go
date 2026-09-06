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
	usage, _, err = MeasureRootUsage(context.Background(), root, targets, cache)
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
}

func TestMeasureRootUsageReusesTheCachedVerdict(t *testing.T) {
	root, mainPath, targets := usageRoots(t)
	slotRepo := filepath.Join(root.Name(), "workspace", "slot", "repo")
	usageWrite(t, mainPath, "shared", "shared content")
	cloneInto(t, mainPath, "shared", slotRepo)
	_, cache, err := MeasureRootUsage(context.Background(), root, targets, nil)
	if err != nil {
		t.Fatal(err)
	}
	// identity が同じなら再判定しないため、main 側を消しても前回の判定がそのまま残る。
	if err := os.Remove(filepath.Join(mainPath, "shared")); err != nil {
		t.Fatal(err)
	}
	usage, _, err := MeasureRootUsage(context.Background(), root, targets, cache)
	if err != nil {
		t.Fatal(err)
	}
	if slot := usage.Slots["slot"]; slot.SharedFiles != 1 {
		t.Fatalf("cached verdict was not reused: %+v", slot)
	}
	usage, _, err = MeasureRootUsage(context.Background(), root, targets, nil)
	if err != nil {
		t.Fatal(err)
	}
	if slot := usage.Slots["slot"]; slot.SharedFiles != 0 {
		t.Fatalf("missing source file counted as shared: %+v", slot)
	}
}
