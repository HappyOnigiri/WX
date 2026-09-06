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
	// slot の外にある実体は root 合計にだけ入り、slot の内訳には数えない。
	usageWrite(t, root.Name(), "outside", "outside content")

	usage, cache, err := MeasureRootUsage(context.Background(), root, targets, nil)
	if err != nil {
		t.Fatal(err)
	}
	slot, measured := usage.Slots["slot"]
	if !measured || slot.Files != 1 || slot.LogicalBytes != int64(len("slot content")) {
		t.Fatalf("slot usage=%+v measured=%v", slot, measured)
	}
	if usage.LogicalBytes != int64(len("slot content")+len("outside content")) {
		t.Fatalf("root logical bytes=%d", usage.LogicalBytes)
	}
	if usage.AllocatedBytes < slot.AllocatedBytes || slot.AllocatedBytes == 0 {
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

func TestLookupUsagePrefixPicksTheLongestAncestor(t *testing.T) {
	prefixes := map[string]string{"a": "slot", "a/b": "repository"}
	value, relative, ok := lookupUsagePrefix("a/b/c/file", prefixes)
	if !ok || value != "repository" || relative != "c/file" {
		t.Fatalf("value=%q relative=%q ok=%v", value, relative, ok)
	}
	if value, relative, ok = lookupUsagePrefix("a/file", prefixes); !ok || value != "slot" || relative != "file" {
		t.Fatalf("value=%q relative=%q ok=%v", value, relative, ok)
	}
	if _, _, ok = lookupUsagePrefix("other/file", prefixes); ok {
		t.Fatal("unrelated path matched a prefix")
	}
	if _, _, ok = lookupUsagePrefix("a/file", map[string]string{}); ok {
		t.Fatal("empty prefix table matched")
	}
}
