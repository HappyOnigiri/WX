package workspace

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// usageRoots は wx root と main worktree を用意し、slot 1 個分の測定対象を返す。
func usageRoots(t *testing.T) (*os.Root, string, []SlotUsageTarget) {
	t.Helper()
	rootPath, mainPath := t.TempDir(), t.TempDir()
	if err := os.MkdirAll(filepath.Join(rootPath, "workspace", "slot", "repo", "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(mainPath, "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { root.Close() })
	targets := []SlotUsageTarget{{SlotID: "slot", RelPath: "workspace/slot", Repositories: map[string]string{"repo": mainPath}}}
	return root, mainPath, targets
}

func usageWrite(t *testing.T, dir, name, data string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestMeasureRootUsageAttributesFilesToSlots(t *testing.T) {
	root, mainPath, targets := usageRoots(t)
	slotRepo := filepath.Join(root.Name(), "workspace", "slot", "repo")
	usageWrite(t, slotRepo, "nested/file", "slot content")
	usageWrite(t, mainPath, "nested/file", "main content")
	// slot の外にある実体は管理容量に含めず、登録外の容量として報告する。
	usageWrite(t, root.Name(), "outside", "outside content")

	usage, cache, err := MeasureRootUsage(context.Background(), root, targets, nil)
	if err != nil {
		t.Fatal(err)
	}
	slot, measured := usage.Slots["slot"]
	if !measured || slot.Files != 1 || slot.LogicalBytes != int64(len("slot content")) {
		t.Fatalf("slot usage=%+v measured=%v", slot, measured)
	}
	if usage.LogicalBytes != int64(len("slot content")) {
		t.Fatalf("root logical bytes=%d", usage.LogicalBytes)
	}
	if usage.AllocatedBytes != slot.AllocatedBytes || slot.AllocatedBytes == 0 || usage.UnmanagedBytes == 0 {
		t.Fatalf("allocated root=%d slot=%d", usage.AllocatedBytes, slot.AllocatedBytes)
	}
	if !SharingSupported() {
		if slot.Compared != 0 || len(cache) != 0 {
			t.Fatalf("compared without CoW support: %+v cache=%d", slot, len(cache))
		}
		return
	}
	// size が違うファイルは開くまでもなく共有対象外になる。
	if slot.Compared != 1 || slot.SharedFiles != 0 || slot.SharedBytes != 0 {
		t.Fatalf("differing files reported as shared: %+v", slot)
	}
	if len(cache) != 1 {
		t.Fatalf("cache=%+v", cache)
	}
}

func TestMeasureSlotUsageWalksOnlyThatSlot(t *testing.T) {
	root, mainPath, targets := usageRoots(t)
	slotRepo := filepath.Join(root.Name(), "workspace", "slot", "repo")
	usageWrite(t, slotRepo, "nested/file", "shared content")
	usageWrite(t, mainPath, "nested/file", "shared content")
	// slot の外を走査していれば、この実体が Files に混ざって検出できる。
	usageWrite(t, root.Name(), "outside", "outside content")

	slot, cache, err := MeasureSlotUsage(context.Background(), root, targets[0], nil)
	if err != nil {
		t.Fatal(err)
	}
	if slot.Files != 1 || slot.LogicalBytes != int64(len("shared content")) {
		t.Fatalf("slot usage=%+v", slot)
	}
	if !SharingSupported() {
		if slot.Compared != 0 || len(cache) != 0 {
			t.Fatalf("compared without CoW support: %+v cache=%d", slot, len(cache))
		}
		return
	}
	// 共有判定は root 全体を測るときと同じ結果になり、cache も同じ root 相対 path で引ける。
	if slot.Compared != 1 || len(cache) != 1 {
		t.Fatalf("slot usage=%+v cache=%+v", slot, cache)
	}
	if _, ok := cache["workspace/slot/repo/nested/file"]; !ok {
		t.Fatalf("cache=%+v", cache)
	}
}

func TestMeasureSlotUsageFailsWhenTheSlotIsGone(t *testing.T) {
	root, _, _ := usageRoots(t)
	target := SlotUsageTarget{SlotID: "slot", RelPath: "workspace/removed"}
	if _, _, err := MeasureSlotUsage(context.Background(), root, target, nil); err == nil {
		t.Fatal("measuring a missing slot succeeded")
	}
}

func TestMeasureRootUsageStopsOnCanceledContext(t *testing.T) {
	root, _, targets := usageRoots(t)
	usageWrite(t, filepath.Join(root.Name(), "workspace", "slot", "repo"), "file", "content")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := MeasureRootUsage(ctx, root, targets, nil); err == nil {
		t.Fatal("canceled measurement succeeded")
	}
}

func TestMeasureRootUsageIgnoresUnusableTargets(t *testing.T) {
	root, mainPath, _ := usageRoots(t)
	targets := []SlotUsageTarget{
		{SlotID: "", RelPath: "workspace/slot"},
		{SlotID: "root", RelPath: "."},
		{SlotID: "escaping", RelPath: ".."},
		{SlotID: "slot", RelPath: "workspace/slot", Repositories: map[string]string{"": mainPath, "repo": ""}},
	}
	usage, _, err := MeasureRootUsage(context.Background(), root, targets, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(usage.Slots) != 1 {
		t.Fatalf("slots=%+v", usage.Slots)
	}
	if _, ok := usage.Slots["slot"]; !ok {
		t.Fatalf("slots=%+v", usage.Slots)
	}
}

// prefix 表は root 相対 path をキーにするので、走査は降りた先の path を 1 度引くだけで境界を判別できる。
func TestUsagePrefixesKeysBoundariesByRootRelativePath(t *testing.T) {
	samples := map[string]SlotUsage{}
	slots, repositories := usagePrefixes([]SlotUsageTarget{
		{SlotID: "slot", RelPath: "workspace/slot/", Repositories: map[string]string{"repo": "/main"}},
	}, samples)
	if slots["workspace/slot"] != "slot" || len(slots) != 1 {
		t.Fatalf("slots=%+v", slots)
	}
	repository, ok := repositories["workspace/slot/repo"]
	if !ok || repository.slotID != "slot" || repository.mainPath != "/main" {
		t.Fatalf("repositories=%+v", repositories)
	}
	if _, seeded := samples["slot"]; !seeded {
		t.Fatalf("samples=%+v", samples)
	}
}
