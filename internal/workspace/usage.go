package workspace

import (
	"context"
	"io/fs"
	"os"
	"path"
	"strings"
	"syscall"
)

// SlotUsageTarget は 1 slot 分の測定対象である。
// RelPath は root 相対の slot path で、Repositories は slot 相対の repository directory 名から main worktree path への対応である。
type SlotUsageTarget struct {
	SlotID       string
	RelPath      string
	Repositories map[string]string
}

// SlotUsage は測定 1 回分の slot 使用量である。
// SharedBytes は main worktree と block を共有していると判定したファイルの allocated 合計、Compared はその判定を試みたファイル数である。
type SlotUsage struct {
	Files          int   `json:"files"`
	LogicalBytes   int64 `json:"logical_bytes"`
	AllocatedBytes int64 `json:"allocated_bytes"`
	Compared       int   `json:"compared_files"`
	SharedFiles    int   `json:"shared_files"`
	SharedBytes    int64 `json:"shared_bytes"`
}

// RootUsage は root 1 世代分の合計と、その root 上にある slot ごとの内訳である。
// SharedBytes は slot ごとの SharedBytes の合計で、AllocatedBytes のうち main worktree と block を共有している分である。
// UnmanagedBytes は登録外の実体の割当量で、AllocatedBytes には含めず共有判定もしない。
type RootUsage struct {
	UnmanagedBytes int64
	LogicalBytes   int64
	AllocatedBytes int64
	SharedBytes    int64
	Slots          map[string]SlotUsage
}

// fileIdentity は開いた regular file の実体と変更時刻を識別する。
// slot と共有元を同じ cache entry に保存するため、判定側と共有元側を区別して保持する。
type fileIdentity struct {
	Dev        uint64
	Ino        uint64
	CtimeNanos int64
}

// SharedFileState は 1 ファイルの共有判定と、その判定が有効な slot・共有元の identity である。
type SharedFileState struct {
	Slot   fileIdentity
	Source fileIdentity
	Shared bool
}

// SharedFileCache は root 相対 path をキーに前回の共有判定を保持する。
// slot と共有元のどちらかが変わっていれば再判定し、両方が変わっていない場合だけ判定を省ける。
type SharedFileCache map[string]SharedFileState

// SharingSupported は CoW の共有判定がこの platform で行えるかを返す。
func SharingSupported() bool { return cowAvailable() }

type usageRepository struct {
	slotID   string
	mainPath string
}

// MeasureRootUsage は pin 済み root を 1 度だけ walk し、root 合計と slot ごとの使用量を返す。
// 共有判定は main worktree の同じ path を開いて物理 offset を比べるだけで、どちらのファイルも内容・metadata を変更しない。
// previous に前回の cache を渡すと slot と共有元の identity が変わっていないファイルの判定を再利用する。返す cache は今回 walk したファイルだけを含む。
func MeasureRootUsage(ctx context.Context, root *os.Root, targets []SlotUsageTarget, previous SharedFileCache) (RootUsage, SharedFileCache, error) {
	return measureUsage(ctx, root, ".", targets, previous)
}

// MeasureSlotUsage は root 配下の slot 1 個だけを walk し、その slot の使用量と共有量を返す。
// root 合計は求めないため、準備の終わった slot を root 全体の測定を待たずに反映する用途に限る。
func MeasureSlotUsage(ctx context.Context, root *os.Root, target SlotUsageTarget, previous SharedFileCache) (SlotUsage, SharedFileCache, error) {
	usage, cache, err := measureUsage(ctx, root, path.Clean(target.RelPath), []SlotUsageTarget{target}, previous)
	return usage.Slots[target.SlotID], cache, err
}

// measureUsage は start から下だけを walk する共通実装で、path はいずれも root 相対のまま扱う。
func measureUsage(ctx context.Context, root *os.Root, start string, targets []SlotUsageTarget, previous SharedFileCache) (RootUsage, SharedFileCache, error) {
	usage := RootUsage{Slots: map[string]SlotUsage{}}
	cache := SharedFileCache{}
	slots, repositories := usagePrefixes(targets, usage.Slots)
	mainRoots := map[string]*os.Root{}
	defer func() {
		for _, opened := range mainRoots {
			if opened != nil {
				_ = opened.Close()
			}
		}
	}()
	walkErr := fs.WalkDir(root.FS(), start, func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		// 高負荷時は walk が数十秒に伸びるため、停止要求を待たせないよう各 entry で中断を確認する。
		if err := ctx.Err(); err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		info, infoErr := entry.Info()
		if infoErr != nil {
			return infoErr
		}
		var allocated int64
		if stat, ok := info.Sys().(*syscall.Stat_t); ok {
			allocated = stat.Blocks * 512
		}
		slotID, _, inSlot := lookupUsagePrefix(name, slots)
		if !inSlot {
			usage.UnmanagedBytes += allocated
			return nil
		}
		if info.Mode().IsRegular() {
			usage.LogicalBytes += info.Size()
		}
		usage.AllocatedBytes += allocated
		sample := usage.Slots[slotID]
		sample.Files++
		if info.Mode().IsRegular() {
			sample.LogicalBytes += info.Size()
		}
		sample.AllocatedBytes += allocated
		repository, relative, inRepository := lookupUsagePrefix(name, repositories)
		if inRepository && repository.slotID == slotID && info.Mode().IsRegular() && info.Size() > 0 && cowAvailable() {
			sample.Compared++
			if sharedWithRepository(root, name, info, repository.mainPath, relative, mainRoots, previous, cache) {
				sample.SharedFiles++
				sample.SharedBytes += allocated
				usage.SharedBytes += allocated
			}
		}
		usage.Slots[slotID] = sample
		return nil
	})
	return usage, cache, walkErr
}

// usagePrefixes は measure 用の prefix 表を組み、slot ごとの空 sample を用意する。
func usagePrefixes(targets []SlotUsageTarget, samples map[string]SlotUsage) (map[string]string, map[string]usageRepository) {
	slots := map[string]string{}
	repositories := map[string]usageRepository{}
	for _, target := range targets {
		relative := path.Clean(target.RelPath)
		if target.SlotID == "" || relative == "." || relative == "/" || strings.HasPrefix(relative, "..") {
			continue
		}
		slots[relative] = target.SlotID
		samples[target.SlotID] = SlotUsage{}
		for directory, mainPath := range target.Repositories {
			if directory == "" || mainPath == "" {
				continue
			}
			repositories[path.Join(relative, directory)] = usageRepository{slotID: target.SlotID, mainPath: mainPath}
		}
	}
	return slots, repositories
}

// lookupUsagePrefix は path の祖先 directory を長い順に辿り、最初に一致した値とその prefix からの相対 path を返す。
func lookupUsagePrefix[T any](name string, prefixes map[string]T) (T, string, bool) {
	var zero T
	if value, ok := prefixes[name]; ok {
		return value, "", true
	}
	if len(prefixes) == 0 {
		return zero, "", false
	}
	for directory := path.Dir(name); directory != "." && directory != "/"; directory = path.Dir(directory) {
		if value, ok := prefixes[directory]; ok {
			return value, strings.TrimPrefix(name, directory+"/"), true
		}
	}
	return zero, "", false
}

// sharedWithRepository は両側の現在の identity を確認してから cache を参照し、必要なら物理 offset を比べる。
func sharedWithRepository(root *os.Root, name string, info os.FileInfo, mainPath, relative string, mainRoots map[string]*os.Root, previous, cache SharedFileCache) bool {
	source, target, sourceInfo, targetInfo, sourceIdentity, targetIdentity, opened := openCOWFiles(root, name, info, mainRoots, mainPath, relative)
	if !opened {
		return false
	}
	defer func() {
		_ = source.Close()
		_ = target.Close()
	}()
	if before, found := previous[name]; found && before.Slot == targetIdentity && before.Source == sourceIdentity {
		if !sameCOWFileIdentities(source, target, sourceIdentity, targetIdentity) {
			return false
		}
		cache[name] = before
		return before.Shared
	}
	shared := false
	if sourceInfo.Size() == targetInfo.Size() {
		var comparable bool
		shared, comparable = compareCOWOffsets(source, target, targetInfo.Size())
		if !comparable {
			return false
		}
	}
	// offset の比較中にどちらかが変更された場合は、その回の判定を cache に残さない。
	if !sameCOWFileIdentities(source, target, sourceIdentity, targetIdentity) {
		return false
	}
	state := SharedFileState{Slot: targetIdentity, Source: sourceIdentity, Shared: shared}
	cache[name] = state
	return shared
}

// openCOWFiles は main worktree 側と slot 側を読み取り専用で開き、比較に使う現在の identity を返す。
// slot は walk 時点の実体と一致することも確認し、途中で置き換わったファイルを cache に残さない。
func openCOWFiles(root *os.Root, name string, info os.FileInfo, mainRoots map[string]*os.Root, mainPath, relative string) (source, target *os.File, sourceInfo, targetInfo os.FileInfo, sourceIdentity, targetIdentity fileIdentity, opened bool) {
	mainRoot, known := mainRoots[mainPath]
	if !known {
		pinned, err := openPinnedRepositoryRoot(mainPath)
		if err != nil {
			mainRoots[mainPath] = nil
			return nil, nil, nil, nil, fileIdentity{}, fileIdentity{}, false
		}
		mainRoots[mainPath] = pinned
		mainRoot = pinned
	}
	if mainRoot == nil {
		return nil, nil, nil, nil, fileIdentity{}, fileIdentity{}, false
	}
	source, sourceInfo, err := cowOpenFile(mainRoot, relative)
	if err != nil || source == nil {
		return nil, nil, nil, nil, fileIdentity{}, fileIdentity{}, false
	}
	target, targetInfo, err = cowOpenFile(root, name)
	if err != nil || target == nil {
		_ = source.Close()
		return nil, nil, nil, nil, fileIdentity{}, fileIdentity{}, false
	}
	if !os.SameFile(info, targetInfo) {
		_ = source.Close()
		_ = target.Close()
		return nil, nil, nil, nil, fileIdentity{}, fileIdentity{}, false
	}
	sourceIdentity, sourceOK := fileIdentityOf(sourceInfo)
	targetIdentity, targetOK := fileIdentityOf(targetInfo)
	if !sourceOK || !targetOK {
		_ = source.Close()
		_ = target.Close()
		return nil, nil, nil, nil, fileIdentity{}, fileIdentity{}, false
	}
	return source, target, sourceInfo, targetInfo, sourceIdentity, targetIdentity, true
}

// sameCOWFileIdentities は比較に使った file descriptor の identity が変わっていないか確認する。
func sameCOWFileIdentities(source, target *os.File, sourceBefore, targetBefore fileIdentity) bool {
	sourceInfo, err := source.Stat()
	if err != nil {
		return false
	}
	targetInfo, err := target.Stat()
	if err != nil {
		return false
	}
	sourceAfter, sourceOK := fileIdentityOf(sourceInfo)
	targetAfter, targetOK := fileIdentityOf(targetInfo)
	return sourceOK && targetOK && sourceAfter == sourceBefore && targetAfter == targetBefore
}

// compareCOWOffsets は offset を比較し、共有判定を得られたかどうかも返す。
// offset の取得に失敗した場合は、非共有という判定を cache に固定しない。
func compareCOWOffsets(source, target *os.File, size int64) (shared, comparable bool) {
	for _, offset := range []int64{0, size - 1} {
		left, err := physicalOffset(source, offset)
		if err != nil {
			return false, false
		}
		right, err := physicalOffset(target, offset)
		if err != nil {
			return false, false
		}
		if left != right {
			return false, true
		}
	}
	return true, true
}
