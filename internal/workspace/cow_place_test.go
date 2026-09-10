package workspace

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/HappyOnigiri/WX/internal/config"
	"github.com/HappyOnigiri/WX/internal/discovery"
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
	if _, err := preparer.PrepareStaged(context.Background(), "slot", []Preparation{{Repository: repo, Target: target, OID: oid}}, nil, func() error { return nil }); err != nil {
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

// stagedCOWFixture は PrepareStaged が先行配置まで済ませた直後の staged repository を作る。
// cow-place と cow-verify の区間だけを取り出して検査するため、そこまでの順序は PrepareStaged と揃える。
// contents は donor へ追加してコミットする path と内容で、空なら fixture 既定の tracked だけが載る。
func stagedCOWFixture(t *testing.T, contents map[string]string) (string, discovery.Repository, *Preparer, *stagedRepository) {
	t.Helper()
	source, repo, preparer, _, target := prepareEdgesFixture(t)
	for name, content := range contents {
		if directory := filepath.Dir(name); directory != "." {
			if err := os.MkdirAll(filepath.Join(source, directory), 0o700); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.WriteFile(filepath.Join(source, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if len(contents) > 0 {
		gitCommand(t, source, "add", ".")
		gitCommand(t, source, "commit", "-m", "staged")
	}
	oid := gitOutput(t, source, "rev-parse", "HEAD")
	staged := *preparer
	staged.noCheckout = true
	ctx := context.Background()
	root, target, err := staged.prepareTarget(target)
	if err != nil {
		t.Fatal(err)
	}
	item := &stagedRepository{Preparation: Preparation{Repository: repo, Target: target, OID: oid}}
	if err := staged.Git.WithCommonDirLock(ctx, string(repo.CommonDir), func(lockCtx context.Context) error {
		var beginErr error
		item.locked, beginErr = staged.beginPrepare(lockCtx, repo, target, oid, testSlotID, preparePhaseCreate, root)
		return beginErr
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { item.locked.close() })
	if err := staged.buildEarlyPlan(ctx, item); err != nil {
		t.Fatal(err)
	}
	if err := staged.checkoutStage(ctx, item, true, nil); err != nil {
		t.Fatal(err)
	}
	return source, repo, &staged, item
}

// 下限未満の候補しか無い準備は1件も clone せず、宛先へ余計な directory も作らない。
// 事前 skip は OID の一致だけを見るため、下限の判定は donor の fstatat が担う。
func TestCOWPlacementPlacesNothingWhenEveryCandidateIsBelowTheMinimum(t *testing.T) {
	_, repo, preparer, item := stagedCOWFixture(t, map[string]string{"small/leaf.bin": "small\n"})
	placement, err := preparer.placeOwnedSharedFiles(context.Background(), repo, item, testSlotID)
	if err != nil {
		t.Fatal(err)
	}
	if len(placement.placed) != 0 {
		t.Fatalf("placed=%v", placement.placed)
	}
	if _, err := os.Stat(filepath.Join(item.Target, "small")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("destination directory err=%v", err)
	}
}

// donor 側の形状が候補と違う run は、共有できないというだけなので run ごと skip して準備を続ける。
func TestCOWPlacementSkipsRunsTheDonorCannotProvide(t *testing.T) {
	source, destination := cowRoots(t)
	cowWrite(t, source, "file", strings.Repeat("f", cowMinShareSize))
	placer := &cowPlacer{
		source: source, destination: destination, proof: func() error { return nil },
		minSize: cowMinShareSize, stats: &cowStats{}, placed: map[string]bool{},
	}
	// "missing" は donor に無く、"file" は directory ではない。どちらも run 単位で落ちる。
	chunk := splitCOWRuns([]cowIndexEntry{{name: "missing/leaf"}, {name: "file/leaf"}})
	if err := placer.placeChunk(context.Background(), chunk, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	if len(placer.placed) != 0 {
		t.Fatalf("placed=%v", placer.placed)
	}
	for _, directory := range []string{"missing", "file"} {
		if _, err := destination.Stat(directory); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("destination %s err=%v", directory, err)
		}
	}
}

// 中断された回は残りの塊を処理しない。置いた分は隔離の判断材料として集計に残す。
func TestCOWPlacementStopsOnCancellation(t *testing.T) {
	source, destination := cowRoots(t)
	cowWrite(t, source, "top", strings.Repeat("t", cowMinShareSize))
	placer := &cowPlacer{
		source: source, destination: destination, proof: func() error { return nil },
		minSize: cowMinShareSize, stats: &cowStats{}, placed: map[string]bool{},
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	chunk := splitCOWRuns([]cowIndexEntry{{name: "top"}})
	if err := placer.placeChunk(ctx, chunk, func() error { return nil }); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled err=%v", err)
	}
	if len(placer.placed) != 0 {
		t.Fatalf("placed=%v", placer.placed)
	}
}

// 共有できるのは下限を超える通常ファイルだけで、directory・symlink・消えた leaf は候補から外す。
func TestCOWShareableLeavesKeepsOnlyRegularFilesAboveTheMinimum(t *testing.T) {
	source, _ := cowRoots(t)
	cowWrite(t, source, "large", strings.Repeat("l", cowMinShareSize))
	cowWrite(t, source, "small", strings.Repeat("s", cowMinShareSize-1))
	if err := source.Mkdir("directory", 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("large", filepath.Join(source.Name(), "link")); err != nil {
		t.Fatal(err)
	}
	base, err := source.Open(".")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = base.Close() }()
	stats := &cowStats{}
	placer := &cowPlacer{minSize: cowMinShareSize, stats: stats}
	got := placer.shareableLeaves(base, []string{"large", "small", "directory", "link", "missing"})
	if len(got) != 1 || got[0] != "large" {
		t.Fatalf("shareable=%v", got)
	}
	if stats.skippedSize.Load() != 1 {
		t.Fatalf("skipped below the minimum=%d", stats.skippedSize.Load())
	}
	stats = &cowStats{}
	// 下限0の設定では size による除外が消え、small も候補として残る。
	placer = &cowPlacer{minSize: 0, stats: stats}
	if got := placer.shareableLeaves(base, []string{"large", "small"}); len(got) != 2 {
		t.Fatalf("shareable without a minimum=%v", got)
	}
	if stats.skippedSize.Load() != 0 {
		t.Fatalf("skipped without a minimum=%d", stats.skippedSize.Load())
	}
}

// 集計は塊ごとの worker から並行して呼ばれるため、同時に記録しても path を落とさない。
func TestCOWPlacementRecordsPathsFromConcurrentChunks(t *testing.T) {
	placer := &cowPlacer{stats: &cowStats{}, placed: map[string]bool{}}
	var group sync.WaitGroup
	for index := range 8 {
		group.Add(1)
		go func() {
			defer group.Done()
			placer.record([]string{joinCOWPath("dir"+strconv.Itoa(index), "leaf"), joinCOWPath(".", "top"+strconv.Itoa(index))})
		}()
	}
	group.Wait()
	if len(placer.placed) != 16 {
		t.Fatalf("placed=%v", placer.placed)
	}
	if !placer.placed["dir0/leaf"] || !placer.placed["top0"] {
		t.Fatalf("placed keys=%v", placer.placed)
	}
	// 記録すべき path が無い回は集計を触らない。
	placer.record(nil)
	if len(placer.placed) != 16 {
		t.Fatalf("placed after an empty record=%v", placer.placed)
	}
}

// clone した内容が要求 OID と違えば、その path だけ通常 checkout でやり直す。
// main が dirty だった path はこの経路で要求 OID の内容へ戻る。
func TestSettleCOWPlacementRepairsPathsThatDoNotMatchTheRequestedOID(t *testing.T) {
	ctx := context.Background()
	_, _, preparer, item := stagedCOWFixture(t, nil)
	if err := preparer.checkoutStage(ctx, item, false, nil); err != nil {
		t.Fatal(err)
	}
	tracked := filepath.Join(item.Target, "tracked")
	if err := os.WriteFile(tracked, []byte("cloned\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := preparer.settleCOWPlacement(ctx, item, map[string]bool{"tracked": true}); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(tracked)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "base\n" {
		t.Fatalf("tracked content=%q", content)
	}
	// clone していない path の差分は準備の対象外なので、この検査では直さない。
	if err := os.WriteFile(tracked, []byte("edited\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := preparer.settleCOWPlacement(ctx, item, map[string]bool{"other": true}); err != nil {
		t.Fatal(err)
	}
	content, err = os.ReadFile(tracked)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "edited\n" {
		t.Fatalf("unplaced path was repaired: %q", content)
	}
	// 1件も置かなかった回は Git を起動しない。
	if err := preparer.settleCOWPlacement(ctx, item, nil); err != nil {
		t.Fatal(err)
	}
}

// index と内容が違う tracked path だけを返す。untracked file は準備の検査対象にしない。
func TestTrackedChangedPathsReportsModifiedTrackedPathsOnly(t *testing.T) {
	ctx := context.Background()
	_, _, preparer, item := stagedCOWFixture(t, nil)
	if err := preparer.checkoutStage(ctx, item, false, nil); err != nil {
		t.Fatal(err)
	}
	paths, err := preparer.trackedChangedPaths(ctx, item)
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 0 {
		t.Fatalf("clean worktree paths=%v", paths)
	}
	if err := os.WriteFile(filepath.Join(item.Target, "tracked"), []byte("changed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(item.Target, "untracked"), []byte("new\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	paths, err = preparer.trackedChangedPaths(ctx, item)
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 1 || paths[0] != "tracked" {
		t.Fatalf("changed paths=%v", paths)
	}
}

// copy 指定では共有経路へ入らない。方式の判断は従来どおり後段の compactWorktree に委ねる。
func TestPlaceSharedFilesSkipsWhenCopyIsRequested(t *testing.T) {
	_, repo, preparer, item := stagedCOWFixture(t, map[string]string{"big/donor.bin": strings.Repeat("b", cowMinShareSize) + "\n"})
	preparer.Config.Storage.CopyMode = config.CopyModeCopy
	placement, err := preparer.placeSharedFiles(context.Background(), repo, item, testSlotID)
	if err != nil {
		t.Fatal(err)
	}
	if len(placement.placed) != 0 {
		t.Fatalf("placed=%v", placement.placed)
	}
}

// 先行配置は件数と段階別の所要時間を準備の区間内訳へ残す。`wx bench` がこの内訳を読む。
func TestCOWPlacementRecordsPhaseBreakdown(t *testing.T) {
	_, repo, preparer, item := stagedCOWFixture(t, map[string]string{"small/leaf.bin": "small\n"})
	preparer.Phases = &PhaseTimings{}
	if _, err := preparer.placeOwnedSharedFiles(context.Background(), repo, item, testSlotID); err != nil {
		t.Fatal(err)
	}
	counts := map[string]int{}
	for _, phase := range preparer.Phases.Phases() {
		counts[phase.Name] = phase.Count
	}
	for _, name := range []string{"cow-place.entries", "cow-place.candidates", "cow-place.stat", "cow-place.proof"} {
		if counts[name] == 0 {
			t.Fatalf("phase %s was not recorded: %v", name, counts)
		}
	}
	// 1件も clone しなかった回は共有件数の区間を作らない。
	if _, recorded := counts["cow-place.shared"]; recorded {
		t.Fatalf("shared count without a clone: %v", counts)
	}
}

// --all は設定のある属性だけを出す。変換に関わらない属性と、変換を外す unset は候補に残す。
func TestParseCOWConvertiblePathsKeepsOnlyConversionAttributes(t *testing.T) {
	output := strings.Join([]string{
		"binary.bin", "text", "unset",
		"binary.bin", "diff", "unset",
		"asset.bin", "filter", "lfs",
		"note.txt", "text", "set",
		"marked.bin", "merge", "ours",
	}, "\x00") + "\x00"
	got := parseCOWConvertiblePaths(output)
	want := map[string]bool{"asset.bin": true, "note.txt": true}
	if len(got) != len(want) {
		t.Fatalf("convertible=%v", got)
	}
	for path := range want {
		if !got[path] {
			t.Fatalf("convertible=%v", got)
		}
	}
}

// 変換の入る path は配置しない。配置後の tracked 検査は clean filter 越しの一致しか見ないため、
// main の未コミット内容が blob へ戻る限り検査を通り、通常 checkout と違う bytes が残る。
func TestShareableCOWPlacementsDropsConvertiblePaths(t *testing.T) {
	large := strings.Repeat("x\n", cowMinShareSize)
	_, _, preparer, item := stagedCOWFixture(t, map[string]string{
		".gitattributes": "*.dat text\n",
		"big/plain.bin":  large,
		"big/text.dat":   large,
	})
	candidates := []cowIndexEntry{{name: "big/plain.bin"}, {name: "big/text.dat"}}
	kept, excluded, err := preparer.shareableCOWPlacements(context.Background(), item, candidates)
	if err != nil {
		t.Fatal(err)
	}
	if len(kept) != 1 || kept[0].name != "big/plain.bin" {
		t.Fatalf("kept=%v", kept)
	}
	if excluded != 1 {
		t.Fatalf("excluded=%d", excluded)
	}
}

// core.autocrlf は属性を持たない path にも効くので、有効な回は1件も配置せず置換方式へ回す。
func TestShareableCOWPlacementsYieldsWhileAutocrlfConverts(t *testing.T) {
	source, _, preparer, item := stagedCOWFixture(t, map[string]string{"big/plain.bin": strings.Repeat("x\n", cowMinShareSize)})
	gitCommand(t, source, "config", "core.autocrlf", "input")
	candidates := []cowIndexEntry{{name: "big/plain.bin"}}
	kept, excluded, err := preparer.shareableCOWPlacements(context.Background(), item, candidates)
	if err != nil {
		t.Fatal(err)
	}
	if len(kept) != 0 || excluded != 1 {
		t.Fatalf("kept=%v excluded=%d", kept, excluded)
	}
}

// 置けなかった候補が残る回は、配置方式だけで共有をやり切ったとは見なさない。
func TestCOWPlacementIsCompleteOnlyWithoutPendingCandidates(t *testing.T) {
	for _, testCase := range []struct {
		name      string
		placement cowPlacement
		want      bool
	}{
		{"placed everything", cowPlacement{placed: map[string]bool{"a": true}}, true},
		{"left a candidate", cowPlacement{placed: map[string]bool{"a": true}, pending: 1}, false},
		{"placed nothing", cowPlacement{}, false},
	} {
		if got := testCase.placement.complete(); got != testCase.want {
			t.Fatalf("%s: complete=%v", testCase.name, got)
		}
	}
}

// 変換の入る tracked file は、main の作業ファイルが blob と違う bytes でも通常 checkout の内容で貸し出す。
func TestPrepareStagedKeepsCheckoutBytesForConvertedPaths(t *testing.T) {
	if !cowAvailable() {
		t.Skip("APFS is required")
	}
	source, repo, preparer, _, target := prepareEdgesFixture(t)
	committed := strings.Repeat("line\n", cowMinShareSize)
	if err := os.Mkdir(filepath.Join(source, "big"), 0o700); err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{".gitattributes": "*.dat text\n", "big/text.dat": committed} {
		if err := os.WriteFile(filepath.Join(source, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	gitCommand(t, source, "add", ".")
	gitCommand(t, source, "commit", "-m", "converted file")
	oid := gitOutput(t, source, "rev-parse", "HEAD")
	// clean filter が CRLF を LF へ戻すので、この作業ファイルは blob と違う bytes でも tracked 検査を通ってしまう。
	if err := os.WriteFile(filepath.Join(source, "big", "text.dat"), []byte(strings.ReplaceAll(committed, "\n", "\r\n")), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := preparer.PrepareStaged(context.Background(), "slot", []Preparation{{Repository: repo, Target: target, OID: oid}}, nil, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(target, "big", "text.dat"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != committed {
		t.Fatalf("the prepared worktree kept the donor bytes: %d bytes", len(got))
	}
}
