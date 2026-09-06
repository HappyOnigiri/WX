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
// SharedBytes は slot ごとの SharedBytes の合計で、slot の外にあるファイルは共有判定の対象外として常に非共有に数える。
type RootUsage struct {
	LogicalBytes   int64
	AllocatedBytes int64
	SharedBytes    int64
	Slots          map[string]SlotUsage
}

// SharedFileState は 1 ファイルの共有判定と、その判定が有効な file identity である。
type SharedFileState struct {
	Dev        uint64
	Ino        uint64
	CtimeNanos int64
	Shared     bool
}

// SharedFileCache は root 相対 path をキーに前回の共有判定を保持する。
// 共有を壊す書き込みは必ず ctime を更新するため、identity が変わっていないファイルは再判定を省ける。
type SharedFileCache map[string]SharedFileState

// SharingSupported は CoW の共有判定がこの platform で行えるかを返す。
func SharingSupported() bool { return cowAvailable() }

type usageRepository struct {
	slotID   string
	mainPath string
}

// MeasureRootUsage は pin 済み root を 1 度だけ walk し、root 合計と slot ごとの使用量を返す。
// 共有判定は main worktree の同じ path を開いて物理 offset を比べるだけで、どちらのファイルも内容・metadata を変更しない。
// previous に前回の cache を渡すと identity が変わっていないファイルの判定を再利用する。返す cache は今回 walk したファイルだけを含む。
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
		if info.Mode().IsRegular() {
			usage.LogicalBytes += info.Size()
		}
		usage.AllocatedBytes += allocated
		slotID, _, inSlot := lookupUsagePrefix(name, slots)
		if !inSlot {
			return nil
		}
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

// sharedWithRepository は cache が使えるならそれを返し、使えないときだけ実際に物理 offset を比べる。
func sharedWithRepository(root *os.Root, name string, info os.FileInfo, mainPath, relative string, mainRoots map[string]*os.Root, previous, cache SharedFileCache) bool {
	identity, ok := fileIdentity(info)
	if !ok {
		return false
	}
	if before, found := previous[name]; found && before.Dev == identity.Dev && before.Ino == identity.Ino && before.CtimeNanos == identity.CtimeNanos {
		cache[name] = before
		return before.Shared
	}
	identity.Shared = sameCOWExtents(root, name, info, mainRoots, mainPath, relative)
	cache[name] = identity
	return identity.Shared
}

// sameCOWExtents は main worktree 側と slot 側を開いて物理 offset を比べる。
// 判定できない事情（open 失敗・size 不一致・platform 非対応）はすべて共有なしとして扱い、測定の失敗で準備や貸出の結果を変えない。
func sameCOWExtents(root *os.Root, name string, info os.FileInfo, mainRoots map[string]*os.Root, mainPath, relative string) bool {
	mainRoot, opened := mainRoots[mainPath]
	if !opened {
		pinned, err := openPinnedRepositoryRoot(mainPath)
		if err != nil {
			mainRoots[mainPath] = nil
			return false
		}
		mainRoots[mainPath] = pinned
		mainRoot = pinned
	}
	if mainRoot == nil {
		return false
	}
	source, sourceInfo, err := cowOpenFile(mainRoot, relative)
	if err != nil || source == nil {
		return false
	}
	defer source.Close()
	if sourceInfo.Size() != info.Size() {
		return false
	}
	target, targetInfo, err := cowOpenFile(root, name)
	if err != nil || target == nil {
		return false
	}
	defer target.Close()
	if !os.SameFile(info, targetInfo) {
		return false
	}
	return sameCOWOffsets(source, target, info.Size())
}

// sameCOWOffsets は先頭と末尾の 2 点だけを比べる sampling である。
// 途中の block だけが書き換わったファイルは共有と見えるため、SharedBytes は上限側の推定になる。
func sameCOWOffsets(source, target *os.File, size int64) bool {
	for _, offset := range []int64{0, size - 1} {
		left, err := physicalOffset(source, offset)
		if err != nil {
			return false
		}
		right, err := physicalOffset(target, offset)
		if err != nil || left != right {
			return false
		}
	}
	return true
}
