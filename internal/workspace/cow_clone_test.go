package workspace

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

// clone後の差し替えは、宛先のbytesとmetadataを保ったまま別inodeへ入れ替え、donorを書き換えない。
// 一時ファイルはcleanupで消え、中断時の元ファイルを指す予約名が残らない。
func TestCOWReplacementPreservesBytesAndTimes(t *testing.T) {
	if !cowAvailable() {
		t.Skip("APFS is required")
	}
	a, b := cowRoots(t)
	cowWrite(t, a, "file", "original content\n")
	cowWrite(t, b, "file", "original content\n")
	stamp := time.Unix(1700000000, 123456789)
	if err := b.Chtimes("file", stamp, stamp); err != nil {
		t.Fatal(err)
	}
	before, err := b.Stat("file")
	if err != nil {
		t.Fatal(err)
	}
	if err := compactFile(context.Background(), a, b, "file", func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	after, err := b.Stat("file")
	if err != nil {
		t.Fatal(err)
	}
	if os.SameFile(before, after) {
		t.Fatal("CoW did not replace the original inode")
	}
	if !after.ModTime().Equal(stamp) || after.Mode() != before.Mode() {
		t.Fatalf("metadata changed: %v", after)
	}
	f, err := b.OpenFile("file", os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.WriteAt([]byte("changed"), 0)
	f.Close()
	if err != nil {
		t.Fatal(err)
	}
	data, err := a.ReadFile("file")
	if err != nil || string(data) != "original content\n" {
		t.Fatalf("donor changed: %q %v", data, err)
	}
	entries, err := os.ReadDir(b.Name())
	if err != nil || len(entries) != 1 {
		t.Fatalf("temporary file remains: %v %v", entries, err)
	}
}

// 先行配置のcloneは下限を超えるleafだけを置き、下限未満の宛先は作らないまま通常checkoutへ残す。
func TestCOWPlacementClonesOnlyFilesAboveTheMinimum(t *testing.T) {
	if !cowAvailable() {
		t.Skip("APFS is required")
	}
	source, destination := cowRoots(t)
	if err := source.Mkdir("dir", 0o700); err != nil {
		t.Fatal(err)
	}
	cowWrite(t, source, "dir/large", strings.Repeat("l", cowMinShareSize))
	cowWrite(t, source, "dir/small", strings.Repeat("s", cowMinShareSize-1))
	cowWrite(t, source, "top", strings.Repeat("t", cowMinShareSize))
	stats := &cowStats{}
	placer := &cowPlacer{
		source: source, destination: destination, proof: func() error { return nil },
		minSize: cowMinShareSize, stats: stats, placed: map[string]bool{},
	}
	chunk := splitCOWRuns([]cowIndexEntry{{name: "dir/large"}, {name: "dir/small"}, {name: "top"}})
	if err := placer.placeChunk(context.Background(), chunk, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	if !placer.placed["dir/large"] || !placer.placed["top"] {
		t.Fatalf("placed=%v", placer.placed)
	}
	if placer.placed["dir/small"] {
		t.Fatal("a file below the minimum was cloned")
	}
	if _, err := destination.Stat("dir/small"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("small file destination err=%v", err)
	}
	if stats.skippedSize.Load() != 1 || stats.shared.Load() != 2 {
		t.Fatalf("skipped=%d shared=%d", stats.skippedSize.Load(), stats.shared.Load())
	}
}
