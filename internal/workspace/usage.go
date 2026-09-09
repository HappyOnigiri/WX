package workspace

import (
	"context"
	"fmt"
	"os"
	"path"
	"strings"
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
	scan := newUsageScan(ctx, targets, previous)
	base, err := root.Open(".")
	if err != nil {
		return RootUsage{Slots: map[string]SlotUsage{}}, nil, err
	}
	scan.measure(usageDirectory{name: ".", dir: base})
	return scan.finish()
}

// MeasureSlotUsage は root 配下の slot 1 個だけを walk し、その slot の使用量と共有量を返す。
// root 合計は求めないため、準備の終わった slot を root 全体の測定を待たずに反映する用途に限る。
func MeasureSlotUsage(ctx context.Context, root *os.Root, target SlotUsageTarget, previous SharedFileCache) (SlotUsage, SharedFileCache, error) {
	relative := path.Clean(target.RelPath)
	scan := newUsageScan(ctx, []SlotUsageTarget{target}, previous)
	slotID, measurable := scan.slots[relative]
	if !measurable {
		return SlotUsage{}, nil, fmt.Errorf("slot %s has no measurable path under the root", target.SlotID)
	}
	// slot が消えていれば error にして、呼び出し側がこの回の測定を諦められるようにする。
	dir, err := openUsageDirectory(root, relative)
	if err != nil {
		return SlotUsage{}, nil, err
	}
	scan.measure(usageDirectory{name: relative, dir: dir, slotID: slotID})
	usage, cache, err := scan.finish()
	return usage.Slots[slotID], cache, err
}

// usagePrefixes は measure 用の prefix 表を組み、slot ごとの空 sample を用意する。
// 表のキーは root 相対 path なので、走査は降りた先の path を引くだけで slot と repository の境界を判別できる。
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
