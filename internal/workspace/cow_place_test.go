package workspace

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCOWDirectoryStackReusesSharedPrefixes(t *testing.T) {
	_, destination := cowRoots(t)
	stack, err := newCOWDirectoryStack(destination, true)
	if err != nil {
		t.Fatal(err)
	}
	defer stack.close()
	for _, directory := range []string{".", "a/b/c", "a/b/d", "a/e", "."} {
		if _, err := stack.at(directory); err != nil {
			t.Fatalf("at %s: %v", directory, err)
		}
	}
	for _, directory := range []string{"a/b/c", "a/b/d", "a/e"} {
		info, err := destination.Stat(directory)
		if err != nil || !info.IsDir() {
			t.Fatalf("directory %s was not created: %v", directory, err)
		}
	}
}

// 読み取り側の stack は directory を作らない。donor に無い path は run ごと skip する材料になる。
func TestCOWDirectoryStackReportsMissingWithoutCreating(t *testing.T) {
	source, _ := cowRoots(t)
	stack, err := newCOWDirectoryStack(source, false)
	if err != nil {
		t.Fatal(err)
	}
	defer stack.close()
	if _, err := stack.at("missing/child"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing directory err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(source.Name(), "missing")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("read-only stack created a directory")
	}
}

// 走査中に成分が symlink へ差し替わっても、pin した root の外は開かない。
func TestCOWDirectoryStackRejectsSymlinkComponents(t *testing.T) {
	source, _ := cowRoots(t)
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(source.Name(), "linked")); err != nil {
		t.Fatal(err)
	}
	stack, err := newCOWDirectoryStack(source, false)
	if err != nil {
		t.Fatal(err)
	}
	defer stack.close()
	if _, err := stack.at("linked"); err == nil {
		t.Fatal("symlink component was opened")
	}
}

func TestPlanCOWPlacementKeepsOnlyMatchingLateEntries(t *testing.T) {
	plan := &earlyPlan{
		tracked: []string{"early", "match", "mismatch", "symlink"},
		oids: map[string]string{
			"early":    "oid-early",
			"match":    "oid-match",
			"mismatch": "oid-target",
		},
		early: map[string]bool{"early": true},
	}
	sourceOIDs := map[string]string{
		"early":    "oid-early",
		"match":    "oid-match",
		"mismatch": "oid-main",
		"symlink":  "oid-symlink",
	}
	candidates := planCOWPlacement(plan, sourceOIDs)
	if len(candidates) != 1 || candidates[0].name != "match" {
		t.Fatalf("candidates=%v", candidates)
	}
	// main 側 index を読めなかった回は事前 skip の材料が無いので、1件も clone しない。
	if got := planCOWPlacement(plan, nil); got != nil {
		t.Fatalf("candidates without a source index=%v", got)
	}
}

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

// 所有権を証明できない回は1件も置かない。証明は塊の入口で先に呼ぶ。
func TestCOWPlacementStopsWhenOwnershipIsUnprovable(t *testing.T) {
	source, destination := cowRoots(t)
	cowWrite(t, source, "top", strings.Repeat("t", cowMinShareSize))
	failure := errors.New("proof failed")
	placer := &cowPlacer{
		source: source, destination: destination, proof: func() error { return failure },
		minSize: cowMinShareSize, stats: &cowStats{}, placed: map[string]bool{},
	}
	chunk := splitCOWRuns([]cowIndexEntry{{name: "top"}})
	err := placer.placeChunk(context.Background(), chunk, placer.verifyProof)
	if !errors.Is(err, failure) {
		t.Fatalf("ownership err=%v", err)
	}
	if len(placer.placed) != 0 {
		t.Fatalf("placed=%v", placer.placed)
	}
	if _, err := destination.Stat("top"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("destination err=%v", err)
	}
}

// 塊は path 順に連続したまま分ける。共通接頭辞を持ち越せるのは連続している間だけである。
func TestCOWChunksStayContiguous(t *testing.T) {
	runs := splitCOWRuns([]cowIndexEntry{
		{name: "a/one"}, {name: "a/two"}, {name: "b/one"}, {name: "c/one"}, {name: "d/one"},
	})
	chunks := chunkCOWRuns(runs, 1)
	var flattened []string
	for _, chunk := range chunks {
		for _, run := range chunk {
			flattened = append(flattened, run.directory)
		}
	}
	if strings.Join(flattened, ",") != "a,b,c,d" {
		t.Fatalf("chunk order=%v", flattened)
	}
}

// 新規準備は共有できる tracked file を clone で置き、main が dirty な path だけ通常 checkout へ落とす。
// clone した内容は要求 OID と一致し、main の未コミット変更は宛先へ持ち込まない。
func TestPrepareStagedPlacesCloneAndRestoresDirtySources(t *testing.T) {
	if !cowAvailable() {
		t.Skip("APFS is required")
	}
	source, repo, preparer, _, target := prepareEdgesFixture(t)
	clean := strings.Repeat("c", cowMinShareSize) + "\n"
	dirty := strings.Repeat("d", cowMinShareSize) + "\n"
	if err := os.Mkdir(filepath.Join(source, "big"), 0o700); err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{"big/clean.bin": clean, "big/dirty.bin": dirty} {
		if err := os.WriteFile(filepath.Join(source, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	gitCommand(t, source, "add", ".")
	gitCommand(t, source, "commit", "-m", "large files")
	oid := gitOutput(t, source, "rev-parse", "HEAD")
	if err := os.WriteFile(filepath.Join(source, "big", "dirty.bin"), []byte(strings.Repeat("x", cowMinShareSize)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := preparer.PrepareStaged(context.Background(), "slot", []Preparation{{Repository: repo, Target: target, OID: oid}}, nil, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{"big/clean.bin": clean, "big/dirty.bin": dirty} {
		got, err := os.ReadFile(filepath.Join(target, name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if string(got) != want {
			t.Fatalf("%s content differs from the requested OID", name)
		}
	}
	// clone は donor と別 inode になる。main を後から書き換えても宛先は変わらない。
	sourceInfo, err := os.Stat(filepath.Join(source, "big", "clean.bin"))
	if err != nil {
		t.Fatal(err)
	}
	targetInfo, err := os.Stat(filepath.Join(target, "big", "clean.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if os.SameFile(sourceInfo, targetInfo) {
		t.Fatal("the destination shares the donor inode")
	}
}
