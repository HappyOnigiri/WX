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

// 共有判定と cache 再利用の共通ロジックを、CoW を提供する platform で検証する。
func TestSharedWithRepositoryReusesUnchangedIdentities(t *testing.T) {
	root, mainPath, _ := usageRoots(t)
	usageWrite(t, filepath.Join(root.Name(), "workspace", "slot", "repo"), "nested/file", "shared content")
	usageWrite(t, mainPath, "nested/file", "shared content")
	const name, relative = "workspace/slot/repo/nested/file", "nested/file"
	info, err := root.Stat(name)
	if err != nil {
		t.Fatal(err)
	}
	mainRoots := map[string]*os.Root{}
	t.Cleanup(func() {
		for _, opened := range mainRoots {
			if opened != nil {
				opened.Close()
			}
		}
	})
	measured := SharedFileCache{}
	if !SharingSupported() {
		// 本番の walker は CoW 非対応 platform でこの関数を呼ばないが、直接呼んでも判定不能を cache に残さない。
		if sharedWithRepository(root, name, info, mainPath, relative, mainRoots, SharedFileCache{}, measured) {
			t.Fatal("unsupported platform reported files as shared")
		}
		if len(measured) != 0 {
			t.Fatalf("unsupported platform retained an unverifiable cache: %+v", measured)
		}
		source, target, _, _, sourceIdentity, targetIdentity, opened := openCOWFiles(root, name, info, mainRoots, mainPath, relative)
		if !opened {
			t.Fatal("unsupported platform could not open files for cache validation")
		}
		_ = source.Close()
		_ = target.Close()
		cached := SharedFileState{Slot: targetIdentity, Source: sourceIdentity, Shared: true}
		carried := SharedFileCache{}
		if !sharedWithRepository(root, name, info, mainPath, relative, mainRoots, SharedFileCache{name: cached}, carried) {
			t.Fatal("unchanged cache entry was not reused")
		}
		if carried[name] != cached {
			t.Fatalf("carried=%+v want=%+v", carried[name], cached)
		}
		return
	}
	// 別々に書いた実体は共有していないので、実測は cache の有無に関わらず false になる。
	if sharedWithRepository(root, name, info, mainPath, relative, mainRoots, SharedFileCache{}, measured) {
		t.Fatal("independent files reported as shared")
	}
	entry, cached := measured[name]
	if !cached {
		t.Fatalf("cache=%+v", measured)
	}
	entry.Shared = true
	carried := SharedFileCache{}
	if !sharedWithRepository(root, name, info, mainPath, relative, mainRoots, SharedFileCache{name: entry}, carried) {
		t.Fatal("cached result was recomputed")
	}
	if carried[name] != entry {
		t.Fatalf("carried=%+v want=%+v", carried[name], entry)
	}
	stale := entry
	stale.Slot.CtimeNanos++
	if sharedWithRepository(root, name, info, mainPath, relative, mainRoots, SharedFileCache{name: stale}, SharedFileCache{}) {
		t.Fatal("stale cache entry was trusted")
	}
	stale = entry
	stale.Source.CtimeNanos++
	if sharedWithRepository(root, name, info, mainPath, relative, mainRoots, SharedFileCache{name: stale}, SharedFileCache{}) {
		t.Fatal("stale source cache entry was trusted")
	}
	if err := os.Remove(filepath.Join(mainPath, "nested", "file")); err != nil {
		t.Fatal(err)
	}
	missing := SharedFileCache{}
	if sharedWithRepository(root, name, info, mainPath, relative, mainRoots, measured, missing) {
		t.Fatal("missing source file reported as shared")
	}
	if len(missing) != 0 {
		t.Fatalf("missing source file was cached: %+v", missing)
	}
}
